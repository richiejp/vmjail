package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
)

func TestCommandStreams(t *testing.T) {
	for _, tty := range []bool{false, true} {
		var out, errout bytes.Buffer
		cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "printf output; printf error >&2; exit 7")
		code, err := runCommandStreams(cmd, &agentv1.AgentAttachHello{Tty: tty, Cols: 80, Rows: 24}, strings.NewReader(""), &out, &errout)
		if err != nil || code != 7 || !strings.Contains(out.String(), "output") {
			t.Fatalf("tty=%v code=%d error=%v out=%q err=%q", tty, code, err, out.String(), errout.String())
		}
		if !tty && errout.String() != "error" {
			t.Fatal("stderr mixed")
		}
	}
	var out bytes.Buffer
	code, err := runCommandStreams(exec.Command("/bin/sh", "-c", "cat"), &agentv1.AgentAttachHello{}, strings.NewReader("input\x00binary"), &out, io.Discard)
	if err != nil || code != 0 || out.String() != "input\x00binary" {
		t.Fatalf("stdin EOF: %d %v %q", code, err, out.String())
	}
	for _, tty := range []bool{false, true} {
		code, err = runCommandStreams(exec.Command("/missing-command"), &agentv1.AgentAttachHello{Tty: tty}, strings.NewReader(""), io.Discard, io.Discard)
		if err == nil || code != 125 {
			t.Fatal("missing command")
		}
	}
}

func TestCommandFrames(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCommandInput(&buf, []byte("hello\x00")); err != nil {
		t.Fatal(err)
	}
	if err := EndCommandInput(&buf); err != nil {
		t.Fatal(err)
	}
	r := &CommandInput{Conn: &buf}
	if n, err := r.Read(nil); n != 0 || err != nil {
		t.Fatal("empty read")
	}
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "hello\x00" {
		t.Fatalf("%q %v", data, err)
	}
	if _, err = io.ReadAll(&CommandInput{Conn: strings.NewReader("")}); err != nil {
		t.Fatal(err)
	}
	if err := WriteJobFrame(&buf, &agentv1.JobFrame{Body: &agentv1.JobFrame_Stderr{Stderr: []byte("bad")}}); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(&CommandInput{Conn: &buf}); err == nil {
		t.Fatal("wrong frame")
	}
	for _, stderr := range []bool{false, true} {
		var buf bytes.Buffer
		w := jobOutputWriter{output: &jobOutput{conn: &buf}, stderr: stderr}
		data := bytes.Repeat([]byte("a"), 70000)
		if n, err := w.Write(data); err != nil || n != len(data) {
			t.Fatal(n, err)
		}
		var got []byte
		for buf.Len() > 0 {
			f, err := ReadJobFrame(&buf)
			if err != nil {
				t.Fatal(err)
			}
			if stderr {
				got = append(got, f.GetStderr()...)
			} else {
				got = append(got, f.GetStdout()...)
			}
		}
		if !bytes.Equal(got, data) {
			t.Fatal("chunked output differs")
		}
	}
}

func TestHostCommandSession(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	withTestRuntime(t, exe)
	t.Setenv(fakeCrunEnv, "session output")
	for _, broken := range []bool{false, true} {
		client, server := net.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- runHostCommand(context.Background(), "/bundle", func(context.Context, uint32) (net.Conn, error) { return server, nil })
		}()
		if broken {
			_ = client.Close()
			if err := <-done; err == nil {
				t.Fatal("missing hello")
			}
			continue
		}
		if err := WriteAttachHello(client, &agentv1.AgentAttachHello{}); err != nil {
			t.Fatal(err)
		}
		var output string
		//vmjail:lint:disable=forloopbound reason="the command session terminates with an exit frame or read error"
		for {
			frame, err := ReadJobFrame(client)
			if err != nil {
				t.Fatal(err)
			}
			if frame.GetExit() != nil {
				if frame.GetExit().GetCode() != 0 {
					t.Fatal(frame)
				}
				break
			}
			output += string(frame.GetStdout())
		}
		if !strings.Contains(output, "session output") {
			t.Fatal(output)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		_ = client.Close()
	}
	if err := runHostCommand(context.Background(), "", func(context.Context, uint32) (net.Conn, error) { return nil, errors.New("dial") }); err == nil {
		t.Fatal("dial failure")
	}
}

func TestHostCommandBootFailure(t *testing.T) {
	for _, network := range []bool{false, true} {
		ctrl := &fakeControl{cfg: &agentv1.GuestConfig{HostCommand: true, NetEnabled: network}}
		err := runWith(context.Background(), io.Discard, runDeps{
			openControl: func(context.Context) (controlChannel, error) { return ctrl, nil },
			dialVsock:   func(context.Context, uint32) (net.Conn, error) { return nil, errors.New("vsock unavailable") },
		})
		if err == nil {
			t.Fatal("failed required channel accepted")
		}
	}
}

type commandOutputFailure struct{ err error }

func (w commandOutputFailure) Write([]byte) (int, error) { return 0, w.err }

func TestCommandTTYOutputFailureStopsProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "printf ready; exec sleep 30")
	want := errors.New("terminal output failed")
	_, err := runCommandStreams(cmd, &agentv1.AgentAttachHello{Tty: true}, strings.NewReader(""), commandOutputFailure{want}, io.Discard)
	if !errors.Is(err, want) || ctx.Err() != nil {
		t.Fatalf("output failure = %v, context = %v", err, ctx.Err())
	}
}
