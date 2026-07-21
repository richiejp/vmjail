package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
)

type memoryCredentialStore struct {
	mu       sync.Mutex
	requests []*agentv1.SecretInstallRequest
	err      error
	cleaned  bool
}

type failingSecretWriter struct{ writes int }

func (w *failingSecretWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 2 {
		return 0, errors.New("payload write")
	}
	return len(p), nil
}

func (s *memoryCredentialStore) Install(r *agentv1.SecretInstallRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cloned, ok := proto.Clone(r).(*agentv1.SecretInstallRequest)
	if !ok {
		return errors.New("unexpected cloned request type")
	}
	s.requests = append(s.requests, cloned)
	return s.err
}
func (s *memoryCredentialStore) Cleanup() error { s.cleaned = true; return nil }

func secretRequest(id string, payload []byte) *agentv1.SecretInstallRequest {
	return &agentv1.SecretInstallRequest{DeliveryId: id, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)), Kind: agentv1.SecretKind_SECRET_KIND_CODEX_AUTH_JSON, Payload: payload}
}

func TestSecretReplayStoreValidationAndReplay(t *testing.T) {
	var installed []byte
	s := &secretReplayStore{now: time.Now, install: func(p []byte) error { installed = append([]byte(nil), p...); return nil }, cleanup: func() error { return nil }}
	req := secretRequest("one", []byte(`{"auth":"token"}`))
	if err := s.Install(req); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(installed, req.Payload) {
		t.Fatal("payload not installed")
	}
	if err := s.Install(secretRequest("one", []byte(`{"auth":"token"}`))); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if err := s.Install(secretRequest("two", []byte(`{}`))); err == nil {
		t.Fatal("different replay succeeded")
	}
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	bad := []*agentv1.SecretInstallRequest{
		secretRequest("", []byte(`{}`)), secretRequest("x", nil), secretRequest("x", []byte(`no`)),
		{DeliveryId: "x", ExpiresAt: timestamppb.New(time.Now().Add(-time.Minute)), Kind: agentv1.SecretKind_SECRET_KIND_CODEX_AUTH_JSON, Payload: []byte(`{}`)},
		{DeliveryId: "x", ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)), Payload: []byte(`{}`)},
	}
	for _, r := range bad {
		if err := s.Install(r); err == nil {
			t.Fatalf("Install(%+v) succeeded", r)
		}
	}
}

func TestSecretReplayStorePropagatesInstallAndCleanupFailures(t *testing.T) {
	wantInstall := errors.New("install")
	s := &secretReplayStore{now: time.Now, install: func([]byte) error { return wantInstall }, cleanup: func() error { return errors.New("cleanup") }}
	if err := s.Install(secretRequest("id", []byte(`{}`))); !errors.Is(err, wantInstall) {
		t.Fatalf("install error = %v", err)
	}
	if err := s.Cleanup(); err == nil || err.Error() != "cleanup" {
		t.Fatalf("cleanup error = %v", err)
	}
}

func TestSecretFramingAndHandlerZeroesPayload(t *testing.T) {
	req := secretRequest("id", []byte(`{"marker":"secret"}`))
	var buf bytes.Buffer
	if err := WriteSecretInstallRequest(&buf, req); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSecretInstallRequest(&buf)
	if err != nil || got.GetDeliveryId() != "id" {
		t.Fatalf("read=%+v err=%v", got, err)
	}
	store := &memoryCredentialStore{}
	host, guest := net.Pipe()
	done := make(chan struct{})
	go func() { handleSecret(guest, store); close(done) }()
	if err := WriteSecretInstallRequest(host, req); err != nil {
		t.Fatal(err)
	}
	resp, err := ReadSecretInstallResponse(host)
	if err != nil || !resp.GetInstalled() {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	<-done
	_ = host.Close()
	if len(store.requests) != 1 || string(store.requests[0].Payload) != `{"marker":"secret"}` {
		t.Fatalf("requests=%+v", store.requests)
	}

	store.err = errors.New(strings.Repeat("x", maxSecretErrorBytes+20))
	host, guest = net.Pipe()
	go handleSecret(guest, store)
	_ = WriteSecretInstallRequest(host, secretRequest("bad", []byte(`{}`)))
	resp, _ = ReadSecretInstallResponse(host)
	if len(resp.GetError()) != maxSecretErrorBytes {
		t.Fatalf("error len=%d", len(resp.GetError()))
	}
	_ = host.Close()
}

func TestSecretFramingRejectsMalformedAndOversize(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], maxSecretFrame+1)
	if _, err := ReadSecretInstallRequest(bytes.NewReader(hdr[:])); err == nil {
		t.Fatal("oversize frame succeeded")
	}
	if err := WriteSecretInstallRequest(&bytes.Buffer{}, secretRequest("x", make([]byte, MaxSecretPayload+1))); err == nil {
		t.Fatal("oversize payload succeeded")
	}
	raw, _ := proto.Marshal(secretRequest("x", make([]byte, MaxSecretPayload+1)))
	var framed bytes.Buffer
	var size [4]byte
	if len(raw) > maxSecretFrame {
		t.Fatalf("test frame unexpectedly exceeds transport cap: %d", len(raw))
	}
	binary.BigEndian.PutUint32(size[:], uint32(len(raw))) //nolint:gosec // test asserted the transport cap above.
	framed.Write(size[:])
	framed.Write(raw)
	clear(raw)
	if _, err := ReadSecretInstallRequest(&framed); err == nil {
		t.Fatal("embedded oversize payload succeeded")
	}
	var malformed bytes.Buffer
	binary.BigEndian.PutUint32(size[:], 1)
	malformed.Write(size[:])
	malformed.WriteByte(0xff)
	if _, err := ReadSecretInstallResponse(&malformed); err == nil {
		t.Fatal("malformed protobuf succeeded")
	}
	if _, err := ReadSecretInstallResponse(bytes.NewReader([]byte{0, 0, 0, 2, 1})); err == nil {
		t.Fatal("short frame succeeded")
	}
	w := &failingSecretWriter{}
	if err := WriteSecretInstallResponse(w, &agentv1.SecretInstallResponse{Installed: true}); err == nil {
		t.Fatal("payload write failure was lost")
	}
}

func TestStartSecretListenerLifecycleAndFailures(t *testing.T) {
	listener := newFakeVsockListener(DefaultSecretVsockPort)
	store := &memoryCredentialStore{}
	d := runDeps{cfg: &agentv1.GuestConfig{CredentialProvider: credentialProviderCodex}, secret: secretDeps{listenVsock: func(port uint32) (net.Listener, error) {
		if port != DefaultSecretVsockPort {
			t.Fatalf("port=%d", port)
		}
		return listener, nil
	}, prepare: func(containerProcess) (credentialStore, error) { return store, nil }}}
	ctx, cancel := context.WithCancel(t.Context())
	var report bytes.Buffer
	cleanup, err := startSecretListener(ctx, &report, d)
	if err != nil {
		t.Fatal(err)
	}
	host, guest := net.Pipe()
	listener.push(guest)
	if err := WriteSecretInstallRequest(host, secretRequest("live", []byte(`{}`))); err != nil {
		t.Fatal(err)
	}
	resp, err := ReadSecretInstallResponse(host)
	if err != nil || !resp.GetInstalled() {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	_ = host.Close()
	cancel()
	cleanup()
	if !store.cleaned {
		t.Fatal("store not cleaned")
	}
	if !strings.Contains(report.String(), "ready") {
		t.Fatalf("report=%q", report.String())
	}

	if cleanup, err := startSecretListener(t.Context(), io.Discard, runDeps{cfg: &agentv1.GuestConfig{}}); err != nil {
		t.Fatal(err)
	} else {
		cleanup()
	}
	for _, tc := range []runDeps{
		{cfg: &agentv1.GuestConfig{CredentialProvider: "other"}},
		{cfg: &agentv1.GuestConfig{CredentialProvider: credentialProviderCodex}, secret: secretDeps{prepare: func(containerProcess) (credentialStore, error) { return nil, errors.New("prepare") }, listenVsock: func(uint32) (net.Listener, error) { return listener, nil }}},
		{cfg: &agentv1.GuestConfig{CredentialProvider: credentialProviderCodex}, secret: secretDeps{prepare: func(containerProcess) (credentialStore, error) { return store, nil }, listenVsock: func(uint32) (net.Listener, error) { return nil, errors.New("listen") }}},
	} {
		if _, err := startSecretListener(t.Context(), io.Discard, tc); err == nil {
			t.Fatal("failure case succeeded")
		}
	}
}

func TestRunSecretListenerReportsAcceptFailure(t *testing.T) {
	done := make(chan struct{})
	var report bytes.Buffer
	store := &memoryCredentialStore{}
	runSecretListener(t.Context(), &report, errAcceptListener{}, store, done)
	<-done
	if !strings.Contains(report.String(), "accept") {
		t.Fatalf("report=%q", report.String())
	}
}
