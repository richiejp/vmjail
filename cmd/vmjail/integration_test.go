//go:build linux && integration

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/richiejp/vmjail/internal/pty"
)

func builtExecutable(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "bin", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("make dev must build %s: %v", name, err)
	}
	return p
}

func localProcess(t *testing.T, ctx context.Context, args ...string) *exec.Cmd {
	t.Helper()
	// #nosec G204 -- path selects the built vmjail test binary.
	return exec.CommandContext(ctx, builtExecutable(t, "vmjail"), args...)
}

func localCommand(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := localProcess(t, ctx, args...)
	var output, diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &diagnostics
	cmd.Stdin = strings.NewReader("input line\n")
	err = cmd.Run()
	return output.String(), diagnostics.String(), err
}

func TestLocalHostShell(t *testing.T) {
	script := `read line
printf '%s' "$line"
echo error >&2
test -c /dev/urandom || exit 90
read host < /proc/sys/kernel/hostname
test "$host" = VMJAIL || exit 91
exit 7`
	out, stderr, err := localCommand(t, "-Mo", "-c", "/", "-u", "99999", "-g", "99999", "--", "/bin/sh", "-c", script)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 || out != "input line" || !strings.Contains(stderr, "error") {
		t.Fatalf("stdout=%q stderr=%q err=%v", out, stderr, err)
	}
}

func TestLocalMounts(t *testing.T) {
	dir := t.TempDir()
	script := `echo "$VALUE" > /data/result
echo scratch > local
test -f local
test "$(ulimit -n)" = 128`
	out, stderr, err := localCommand(
		t,
		"-c", "/", "-u", "99999", "-g", "99999", "-B", dir+":/data",
		"-T", "/scratch", "--rlimit_nofile", "128", "-E", "VALUE=works",
		"--cwd", "/scratch", "--", "/bin/sh", "-c", script,
	)
	if err != nil {
		t.Fatalf("stdout=%q stderr=%q err=%v", out, stderr, err)
	}
	// #nosec G304 -- file is in the test-owned temporary directory.
	data, err := os.ReadFile(filepath.Join(dir, "result"))
	if err != nil || string(data) != "works\n" {
		t.Fatalf("host write=%q %v", data, err)
	}
	_, stderr, err = localCommand(t, "-c", "/", "-R", dir+":/data", "--", "/bin/sh", "-c", "echo broken >/data/result")
	if err == nil {
		t.Fatalf("read-only mount accepted write: %s", stderr)
	}
	// #nosec G304 -- file is in the test-owned temporary directory.
	data, err = os.ReadFile(filepath.Join(dir, "result"))
	if err != nil || string(data) != "works\n" {
		t.Fatal("read-only content changed", err)
	}
	_, stderr, err = localCommand(t, "-c", "/", "-R", "/proc:/data", "--", "/bin/sh", "-c", "exit 0")
	if err == nil || !strings.Contains(stderr, "pseudo-filesystem") {
		t.Fatal("host proc accepted", err, stderr)
	}
}

func TestLocalMinimalRoot(t *testing.T) {
	hello := builtExecutable(t, "vmjail-hello-container")
	out, stderr, err := localCommand(t, "-u", "99999", "-g", "99999", "-R", hello+":/hello", "--", "/hello")
	if err != nil || !strings.Contains(out, "hello world") {
		t.Fatalf("stdout=%q stderr=%q err=%v", out, stderr, err)
	}
}

func TestLocalDeadline(t *testing.T) {
	_, stderr, err := localCommand(t, "-c", "/", "-t", "1", "--", "/bin/sh", "-c", "while :; do :; done")
	if err == nil || !strings.Contains(stderr, "deadline") {
		t.Fatalf("err=%v stderr=%s", err, stderr)
	}
}

func TestLocalRerun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := localProcess(t, ctx, "-Mr", "-c", "/", "--", "/bin/sh", "-c", "echo iteration")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scan := bufio.NewScanner(pipe)
	count := 0
	for scan.Scan() {
		if scan.Text() == "iteration" {
			count++
		}
		if count == 3 {
			cancel()
			break
		}
	}
	_ = cmd.Wait() // Cancellation is the expected stop for rerun mode.
	if count != 3 {
		t.Fatalf("iterations=%d stderr=%s", count, stderr.String())
	}
}

func TestLocalListener(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := localProcess(
		t, ctx,
		"-Ml", "--port", port, "-c", "/", "--", "/bin/sh", "-c", "read line; echo reply:$line",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	for request := 0; request < 2; request++ {
		var conn net.Conn
		for attempt := 0; attempt < 100; attempt++ {
			conn, err = net.DialTimeout("tcp", address, 100*time.Millisecond)
			if err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(conn, "hello"); err != nil {
			t.Fatal(err)
		}
		reply, err := io.ReadAll(conn)
		_ = conn.Close() // Completed request connection.
		if err != nil || string(reply) != "reply:hello\n" {
			t.Fatalf("reply=%q err=%v", reply, err)
		}
	}
}

func TestLocalNetworkAndObservation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "network-ok") }))
	defer server.Close()
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal(err)
	}
	target := strings.Replace(server.URL, "127.0.0.1", "192.168.127.254", 1)
	dir := t.TempDir()
	out, stderr, err := localCommand(
		t,
		"-c", "/", "-u", "1000", "-g", "1000", "--use_pasta", "--observe-dir", dir,
		"--", "/bin/sh", "-c", `read resolver < /etc/resolv.conf || exit 99; exec "$@"`,
		"sh", curl, "--silent", "--show-error", "--fail", "--max-time", "10", target,
	)
	if err != nil || out != "network-ok" {
		t.Fatalf("out=%q stderr=%q err=%v", out, stderr, err)
	}
	runs, err := filepath.Glob(filepath.Join(dir, "run-*"))
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	for _, name := range []string{"resources.jsonl", "opensnoop.jsonl", "execsnoop.jsonl", "syscalls.txt"} {
		info, err := os.Stat(filepath.Join(runs[0], name))
		if err != nil || info.Size() == 0 {
			t.Errorf("missing/empty observation %s: %v", name, err)
		}
	}
}

func TestLocalTTY(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })

	// Give the CLI a controlling terminal and capture what the guest writes.
	cmd := localProcess(t, ctx, "-c", "/", "--", "/bin/sh", "-c", "test -t 0 && test -t 1 && echo tty-ok")
	cmd.Stdin = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	data, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(data), "tty-ok") {
		t.Fatalf("TTY: %s %v", data, err)
	}
}

func TestLocalMountNamespace(t *testing.T) {
	const probeEnv = "VMJAIL_TEST_MOUNT_NAMESPACE"
	if fields := strings.Fields(os.Getenv(probeEnv)); len(fields) == 4 {
		for i, ns := range []string{"user", "mnt"} {
			if got := namespaceLink(t, ns); got == fields[i] {
				t.Errorf("child uses parent %s namespace %s", ns, got)
			}
		}
		for i, name := range []string{"uid_map", "gid_map"} {
			got := strings.Join(strings.Fields(readSelfProc(t, name)), " ")
			if want := "0 " + fields[i+2] + " 1"; got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		if os.Geteuid() != 0 || os.Getegid() != 0 {
			t.Errorf("child effective IDs = (%d, %d), want (0, 0)", os.Geteuid(), os.Getegid())
		}
		for line := range strings.SplitSeq(readSelfProc(t, "mountinfo"), "\n") {
			before, _, ok := strings.Cut(line, " - ")
			if !ok {
				continue
			}
			fields := strings.Fields(before)
			for _, propagation := range fields[6:] {
				if strings.HasPrefix(propagation, "shared:") || strings.HasPrefix(propagation, "master:") {
					t.Fatalf("mount remains shared or slave: %s", line)
				}
			}
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// No external binaries are available on PATH.
	t.Setenv("PATH", t.TempDir())
	cmd := namespaceCommand(ctx, []string{"-test.run=^TestLocalMountNamespace$"}, "")
	expected := fmt.Sprintf("%s %s %d %d", namespaceLink(t, "user"), namespaceLink(t, "mnt"), os.Geteuid(), os.Getegid())
	cmd.Env = append(cmd.Env, probeEnv+"="+expected)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("namespace probe failed: %v\n%s", err, out)
	}
}

func namespaceLink(t *testing.T, name string) string {
	t.Helper()
	link, err := os.Readlink("/proc/self/ns/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return link
}

func readSelfProc(t *testing.T, name string) string {
	t.Helper()
	// #nosec G304 -- callers use fixed proc file names.
	data, err := os.ReadFile("/proc/self/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
