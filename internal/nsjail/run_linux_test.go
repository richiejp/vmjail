package nsjail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/vmjail/internal/cli"

	"github.com/richiejp/vmjail/internal/agent"
	"github.com/richiejp/vmjail/internal/bundled"
	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"github.com/richiejp/vmjail/internal/sandbox"
)

func TestRunModes(t *testing.T) {
	old := runCommand
	t.Cleanup(func() { runCommand = old })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	runCommand = func(context.Context, Options, io.Reader, io.Writer, io.Writer) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return &cli.ExitError{Code: 7}
	}
	if err := Run(ctx, Options{Mode: "r"}, os.Stdin, nil, nil); err == nil || calls != 2 {
		t.Fatal(calls, err)
	}
	runCommand = func(context.Context, Options, io.Reader, io.Writer, io.Writer) error {
		return errors.New("infrastructure")
	}
	if err := Run(context.Background(), Options{Mode: "r"}, os.Stdin, nil, nil); err == nil {
		t.Fatal("reran infrastructure error")
	}
	if err := Run(context.Background(), Options{Mode: "o"}, nil, nil, nil); err == nil {
		t.Fatal("once error")
	}

	if err := Run(ctx, Options{Mode: "r"}, strings.NewReader(""), nil, nil); err == nil {
		t.Fatal("rerun accepted uncancellable input")
	}

	// An occupied loopback port is rejected, instead of binding another address.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("TCP address")
	}
	// #nosec G115 -- TCP port is 16 bits.
	if err := Run(context.Background(), Options{Mode: "l", Port: uint16(addr.Port)}, nil, nil, io.Discard); err == nil {
		t.Fatal("busy port")
	}
}

func TestRunnerLifecycle(t *testing.T) {
	fakeMounts(t)
	oldServe, oldAssets := serveVM, loadArtifacts
	t.Cleanup(func() { serveVM, loadArtifacts = oldServe, oldAssets })
	t.Setenv("TMPDIR", t.TempDir())
	loadArtifacts = func() (sandbox.Config, func(), error) { return sandbox.Config{}, func() {}, nil }
	serveVM = func(ctx context.Context, _ io.Writer, cfg sandbox.Config) error {
		conn, err := net.Dial(unixNetwork, fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, agent.DefaultCommandVsockPort))
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		if _, err := agent.ReadAttachHello(conn); err != nil {
			return err
		}
		if _, err := io.ReadAll(&agent.CommandInput{Conn: conn}); err != nil {
			return err
		}
		if err := agent.WriteJobFrame(conn, &agentv1.JobFrame{Body: &agentv1.JobFrame_Exit{Exit: &agentv1.JobExit{Code: 3}}}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runOnce(ctx, Options{Time: 5, Network: true}, strings.NewReader(""), io.Discard, io.Discard)
	var status *cli.ExitError
	if !errors.As(err, &status) || status.Code != 3 {
		t.Fatal(err)
	}
	serveVM = func(context.Context, io.Writer, sandbox.Config) error { return errors.New("VMM failed") }
	if err := runOnce(ctx, Options{}, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "console:") || !strings.Contains(err.Error(), "VMM failed") {
		t.Fatal("startup error diagnostics", err)
	}
	loadArtifacts = func() (sandbox.Config, func(), error) { return sandbox.Config{}, nil, errors.New("missing kernel") }
	if err := runOnce(ctx, Options{}, nil, nil, nil); err == nil {
		t.Fatal("artifact failure")
	}
	loadArtifacts = func() (sandbox.Config, func(), error) { return sandbox.Config{}, func() {}, nil }
	if err := runOnce(ctx, Options{Root: "/missing"}, nil, nil, nil); err == nil {
		t.Fatal("prepare failure")
	}
}

func TestArtifacts(t *testing.T) {
	for _, key := range []string{sandbox.EnvKernel, sandbox.EnvInitramfs, sandbox.EnvCloudHypervisor} {
		t.Setenv(key, "")
	}
	if _, _, err := artifacts(); err == nil {
		t.Fatal("missing artifacts")
	}
	for _, key := range []string{sandbox.EnvKernel, sandbox.EnvInitramfs, sandbox.EnvCloudHypervisor} {
		t.Setenv(key, "/fake")
	}
	_, release, err := artifacts()
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestBundledArtifacts(t *testing.T) {
	oldHas, oldExtract := hasBundle, extractBundle
	t.Cleanup(func() { hasBundle, extractBundle = oldHas, oldExtract })
	hasBundle = func() bool { return true }
	for _, key := range []string{sandbox.EnvKernel, sandbox.EnvInitramfs, sandbox.EnvCloudHypervisor, sandbox.EnvVirtiofsd} {
		t.Setenv(key, "")
	}
	extractBundle = func(string) (bundled.Paths, error) {
		return bundled.Paths{CloudHypervisor: "/vmm", Kernel: "/kernel", Initramfs: "/initramfs", Virtiofsd: "/fs"}, nil
	}
	cfg, release, err := artifacts()
	if err != nil {
		t.Fatal(err)
	}
	release()
	if cfg.Kernel != "/kernel" || cfg.Initramfs != "/initramfs" || cfg.CloudHypervisor != "/vmm" || cfg.Virtiofsd != "/fs" {
		t.Fatal(cfg)
	}
	t.Setenv(sandbox.EnvKernel, "/override")
	cfg, release, err = artifacts()
	if err != nil {
		t.Fatal(err)
	}
	release()
	if cfg.Kernel != "/override" {
		t.Fatal("environment override")
	}
	extractBundle = func(string) (bundled.Paths, error) { return bundled.Paths{}, errors.New("extract") }
	if _, _, err := artifacts(); err == nil {
		t.Fatal("extract failure")
	}
}

func TestObservationPlanning(t *testing.T) {
	fakeMounts(t)
	dir := t.TempDir()
	work := t.TempDir()
	cfg := sandbox.Config{}
	if err := configureObservation(Options{ObserveDir: dir}, &cfg, work, []mountEntry{{"/", fsExt4}}); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Shares) != 1 || cfg.IosnoopLogPath == "" || cfg.ObserverLogPath == "" {
		t.Fatalf("%+v", cfg)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := configureObservation(Options{ObserveDir: file}, &cfg, work, nil); err == nil {
		t.Fatal("file as directory")
	}
	if err := configureObservation(Options{ObserveDir: dir}, &cfg, work, nil); err == nil {
		t.Fatal("invalid source")
	}
}

func TestListenerLifecycle(t *testing.T) {
	oldListen, oldRun := listenTCP, runCommand
	t.Cleanup(func() { listenTCP, runCommand = oldListen, oldRun })
	for _, badOutput := range []bool{false, true} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listenTCP = func(string, string) (net.Listener, error) { return ln, nil }
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		runCommand = func(context.Context, Options, io.Reader, io.Writer, io.Writer) error {
			cancel()
			return &cli.ExitError{Code: 7}
		}
		output := io.Writer(io.Discard)
		if badOutput {
			output = brokenOutput{}
		}
		done := make(chan error, 1)
		go func() { done <- listen(ctx, Options{}, output) }()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if err := <-done; err == nil {
			t.Fatal("listener should report cancellation or output failure")
		}
		_ = conn.Close()
		cancel() // The request has completed.
	}
}

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, errors.New("closed output") }

func TestRunnerDisconnectAndDeadline(t *testing.T) {
	oldServe := serveVM
	t.Cleanup(func() { serveVM = oldServe })
	for _, wait := range []bool{false, true} {
		serveVM = func(ctx context.Context, _ io.Writer, cfg sandbox.Config) error {
			conn, err := net.Dial(unixNetwork, fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, agent.DefaultCommandVsockPort))
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			if _, err := agent.ReadAttachHello(conn); err != nil {
				return err
			}
			if !wait {
				_ = conn.Close()
			} // Simulate a guest disappearing after the handshake.
			<-ctx.Done()
			return ctx.Err()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := runConfigured(ctx, Options{}, sandbox.Config{}, strings.NewReader(""), io.Discard, io.Discard)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "console:") {
			t.Fatal("missing diagnostic context", err)
		}
	}
}

func TestCommandExitsBeforeReadingInput(t *testing.T) {
	oldServe := serveVM
	t.Cleanup(func() { serveVM = oldServe })
	serveVM = func(ctx context.Context, _ io.Writer, cfg sandbox.Config) error {
		conn, err := net.Dial(unixNetwork, fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, agent.DefaultCommandVsockPort))
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }() // Test guest endpoint cleanup.
		if _, err := agent.ReadAttachHello(conn); err != nil {
			return err
		}
		// Leave stdin unread and keep the socket open, as a guest with a blocked stdin pump can do.
		if err := agent.WriteJobFrame(conn, &agentv1.JobFrame{Body: &agentv1.JobFrame_Exit{Exit: &agentv1.JobExit{}}}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runConfigured(ctx, Options{}, sandbox.Config{}, strings.NewReader(strings.Repeat("x", 16<<20)), io.Discard, io.Discard); err != nil {
		t.Fatal("early exit blocked on stdin", err)
	}
	if ctx.Err() != nil {
		t.Fatal("early exit needed the timeout to unblock stdin")
	}
}
