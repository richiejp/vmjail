package nodectrlclient_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	nodectrlv1 "github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/localai-org/darkaifactory/internal/nodectrl"
	"github.com/localai-org/darkaifactory/internal/nodectrlclient"
	"github.com/localai-org/darkaifactory/internal/sandboxgateway"
)

const e2eObserverUID = "sb-observer-e2e"

// TestObserverTailEndToEnd pins the full observer-tail loop over a
// real orchestrator + sandboxd (HTTP/2 + mTLS): the orchestrator-side
// OpenObserverBridge pushes an ObserverRequest, the sandboxd's
// nodectrlclient dispatches it, resolves the uid to a real on-disk
// observer log, dials ObserverStream, and streams the log bytes back,
// which the orchestrator's reader yields to the caller.
func TestObserverTailEndToEnd(t *testing.T) {
	rig := newE2ERig(t)

	logPath := filepath.Join(t.TempDir(), "observer.jsonl")
	const logBody = "{\"cpu\":1}\n{\"cpu\":2}\n"
	if err := os.WriteFile(logPath, []byte(logBody), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	observerReg := sandboxgateway.NewObserverRegistry()
	if err := observerReg.Register(e2eObserverUID, logPath); err != nil {
		t.Fatalf("Register: %v", err)
	}

	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	client, err := nodectrlclient.New(nodectrlclient.Config{
		URL:               rig.url,
		Identity:          rig.sbID,
		Peers:             rig.sbPeer,
		Version:           testFixtureVer,
		HeartbeatInterval: 100 * time.Millisecond,
		ObserverResolver:  observerReg,
		Stderr:            io.Discard,
	})
	if err != nil {
		t.Fatalf("nodectrlclient.New: %v", err)
	}
	served := make(chan error, 1)
	go func() {
		served <- client.RegisterAndServe(clientCtx, nodectrl.Resources{CPUs: 1, MemoryBytes: 1 << 20})
	}()
	fp := rig.sbID.Fingerprint()
	waitForHeartbeat(t, rig.reg, fp)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	rc, err := rig.server.SandboxControl().OpenObserverBridge(ctx, fp, e2eObserverUID,
		nodectrlv1.ObserverSource_OBSERVER_SOURCE_LOG, false)
	if err != nil {
		t.Fatalf("OpenObserverBridge: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Contains(got, []byte("\"cpu\":1")) || !bytes.Contains(got, []byte("\"cpu\":2")) {
		t.Errorf("observer stream = %q, want both log lines", got)
	}

	clientCancel()
	select {
	case err := <-served:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("RegisterAndServe: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("RegisterAndServe did not return after cancel")
	}
}

// TestObserveSummaryEndToEnd pins the build-progress summary path over
// the same real wire: an ObserverRequest with source=SUMMARY routes
// through the node's SummaryResolver (not the observer log), and the
// one-shot read yields the summary file's bytes. This is the #6c-2
// rendezvous that backs `daft sandbox observe`.
func TestObserveSummaryEndToEnd(t *testing.T) {
	rig := newE2ERig(t)

	summaryPath := filepath.Join(t.TempDir(), "progress.json")
	const summaryBody = "{\"taken_at_unix_ms\":42,\"opensnoop\":{\"total\":7}}\n"
	if err := os.WriteFile(summaryPath, []byte(summaryBody), 0o600); err != nil {
		t.Fatalf("write summary: %v", err)
	}
	summaryReg := sandboxgateway.NewSummaryRegistry()
	if err := summaryReg.Register(e2eObserverUID, summaryPath); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// An observer log is also registered to a *different* file, so a
	// passing read proves source=SUMMARY selected the SummaryResolver
	// rather than falling through to the observer log.
	observerReg := sandboxgateway.NewObserverRegistry()
	if err := observerReg.Register(e2eObserverUID, filepath.Join(t.TempDir(), "observer.jsonl")); err != nil {
		t.Fatalf("Register observer: %v", err)
	}

	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	client, err := nodectrlclient.New(nodectrlclient.Config{
		URL:               rig.url,
		Identity:          rig.sbID,
		Peers:             rig.sbPeer,
		Version:           testFixtureVer,
		HeartbeatInterval: 100 * time.Millisecond,
		ObserverResolver:  observerReg,
		SummaryResolver:   summaryReg,
		Stderr:            io.Discard,
	})
	if err != nil {
		t.Fatalf("nodectrlclient.New: %v", err)
	}
	served := make(chan error, 1)
	go func() {
		served <- client.RegisterAndServe(clientCtx, nodectrl.Resources{CPUs: 1, MemoryBytes: 1 << 20})
	}()
	fp := rig.sbID.Fingerprint()
	waitForHeartbeat(t, rig.reg, fp)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	rc, err := rig.server.SandboxControl().OpenObserverBridge(ctx, fp, e2eObserverUID,
		nodectrlv1.ObserverSource_OBSERVER_SOURCE_SUMMARY, false)
	if err != nil {
		t.Fatalf("OpenObserverBridge: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != summaryBody {
		t.Errorf("summary stream = %q, want %q", got, summaryBody)
	}

	clientCancel()
	select {
	case err := <-served:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("RegisterAndServe: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("RegisterAndServe did not return after cancel")
	}
}

// TestObserverTailEndToEndNoRoute pins the missing-route surface over
// the real wire: with no observer registered for the uid, the node's
// handler errors after the Hello, and the orchestrator-side reader
// observes the stream ending (EOF / no bytes) rather than blocking.
func TestObserverTailEndToEndNoRoute(t *testing.T) {
	rig := newE2ERig(t)
	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	client, err := nodectrlclient.New(nodectrlclient.Config{
		URL:               rig.url,
		Identity:          rig.sbID,
		Peers:             rig.sbPeer,
		Version:           testFixtureVer,
		HeartbeatInterval: 100 * time.Millisecond,
		ObserverResolver:  sandboxgateway.NewObserverRegistry(), // empty
		Stderr:            io.Discard,
	})
	if err != nil {
		t.Fatalf("nodectrlclient.New: %v", err)
	}
	served := make(chan error, 1)
	go func() {
		served <- client.RegisterAndServe(clientCtx, nodectrl.Resources{CPUs: 1, MemoryBytes: 1 << 20})
	}()
	fp := rig.sbID.Fingerprint()
	waitForHeartbeat(t, rig.reg, fp)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	rc, err := rig.server.SandboxControl().OpenObserverBridge(ctx, fp, "unknown-uid",
		nodectrlv1.ObserverSource_OBSERVER_SOURCE_LOG, false)
	if err != nil {
		t.Fatalf("OpenObserverBridge: %v", err)
	}
	defer func() { _ = rc.Close() }()
	// The node resolves no route and tears the stream down; the reader
	// terminates (EOF/empty) rather than hanging.
	got, _ := io.ReadAll(rc)
	if len(got) != 0 {
		t.Errorf("got %q, want no bytes for an unrouted observer", got)
	}

	clientCancel()
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Error("RegisterAndServe did not return after cancel")
	}
}
