package nsjail

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	echoCommand  = "echo hi"
	shellPath    = "/bin/sh"
	argCPUs      = "--max_cpus"
	relativePath = "relative"
)

func TestArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		bad  bool
	}{
		{[]string{"-Mo", "-c", "/", "--cwd", "/tmp", "-u", "123", "-g", "456", "-R", "/a:/b", "-B", "/c:/d", "-T", "/scratch", "-E", "KEY=value", "--rlimit_as", "512", "--rlimit_cpu", "2", "--rlimit_nofile", "64", argCPUs, "2", "--", shellPath, "-c", echoCommand}, false},
		{[]string{"-Ml", "--port", "1234", "--", "sh"}, false},
		{[]string{"-Mr", "--", "sh"}, false},
		{[]string{"-Me", "--", "sh"}, true},
		{[]string{}, true},
		{[]string{"--cwd", relativePath, "--", "sh"}, true},
		{[]string{"-c", relativePath, "--", "sh"}, true},
		{[]string{argCPUs, "0", "--", "sh"}, true},
		{[]string{argCPUs, "257", "--", "sh"}, true},
		{[]string{"--time_limit", "999999999999", "--", "sh"}, true},
		{[]string{"--rlimit_as", "999999999999999", "--", "sh"}, true},
		{[]string{"-Ml", "--", "sh"}, true},
		{[]string{"--port", "1234", "--", "sh"}, true},
		{[]string{"-E", "INVALID", "--", "sh"}, true},
		{[]string{"-E", "=value", "--", "sh"}, true},
		{[]string{"--seccomp_string", "anything", "--", "sh"}, true},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			called := false
			cmd := NewCommand(strings.NewReader(""), io.Discard, io.Discard, func(_ context.Context, o Options, _ io.Reader, _, _ io.Writer) error {
				called = true
				if o.Command[len(o.Command)-1] == echoCommand {
					if !reflect.DeepEqual(o.Command, []string{shellPath, "-c", echoCommand}) || o.User != 123 || o.Group != 456 || o.AddressSpace != 512 || o.Files != 64 || o.CPUTime != 2 || o.CPUs != 2 || len(o.ReadOnly) != 1 || len(o.Writable) != 1 || len(o.Tmpfs) != 1 {
						t.Fatalf("options: %+v", o)
					}
				}
				return nil
			})
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if (err != nil) != tc.bad || called == tc.bad {
				t.Fatalf("called=%v err=%v", called, err)
			}
		})
	}
	var out bytes.Buffer
	cmd := NewCommand(nil, &out, io.Discard, nil)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil || out.Len() == 0 {
		t.Fatalf("version: %s %v", out.String(), err)
	}

	cmd = NewCommand(nil, io.Discard, io.Discard, nil)
	cmd.SetArgs([]string{"version", "extra"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("version accepted an unexpected argument")
	}
}

func TestSpecAndBinds(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	if err := writeSpec(file, Options{Command: []string{shellPath}, Cwd: "/", User: 42, AddressSpace: 64, Files: 128, CPUTime: 3, Env: []string{"A=B"}}, guestMounts()); err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- file belongs to t.TempDir.
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err = json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	p, ok := spec["process"].(map[string]any)
	if !ok {
		t.Fatal("process missing")
	}
	limits, ok := p["rlimits"].([]any)
	if !ok {
		t.Fatal("rlimits missing")
	}
	root, ok := spec["root"].(map[string]any)
	if !ok {
		t.Fatal("root missing")
	}
	if p["noNewPrivileges"] != true || len(limits) != 3 || root["readonly"] != true {
		t.Fatalf("spec %s", raw)
	}
	for _, v := range []string{dir, dir + ":/data"} {
		b, err := parseBind(v, true)
		if err != nil || b.source != dir || !b.readonly {
			t.Fatalf("%+v %v", b, err)
		}
	}
	for _, v := range []string{relativePath, dir + ":relative", dir + ":/dev/../proc/x", dir + ":/sys", dir + ":/", dir + ":/dev/null", "/does-not-exist:/data"} {
		if _, err := parseBind(v, false); err == nil {
			t.Fatalf("accepted %s", v)
		}
	}
	if err := writeSpec(dir, Options{}, nil); err == nil {
		t.Fatal("write directory")
	}
	t.Setenv("VMJAIL_ENV_TEST", "present")
	if !strings.Contains(strings.Join(environment(Options{KeepEnv: true}), "\n"), "VMJAIL_ENV_TEST=present") {
		t.Fatal("keep env")
	}
}
