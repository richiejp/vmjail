package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
)

const (
	testWorkspaceDevice    = "/dev/vda"
	testWorkspaceDirectory = "mounts/0"
	testWorkspaceSeedPort  = uint32(9400)
)

func applyWorkspaceDiskForTest(t *testing.T, w io.Writer, deps workspaceDiskDeps, cfg *agentv1.GuestConfig) error {
	t.Helper()
	return applyWorkspaceDisk(t.Context(), w, deps, cfg, func(context.Context, uint32) (net.Conn, error) {
		return nil, errors.New("unexpected seed dial")
	})
}

func TestApplyWorkspaceDiskFormatsAndMaps(t *testing.T) {
	var commands [][]string
	var mounts [][2]string
	var seeded string
	var seedPort uint32
	deps := workspaceDiskDeps{
		readMagic: func(string) ([4]byte, error) { return [4]byte{}, nil },
		mkdirAll:  func(string, os.FileMode) error { return nil },
		mountXFS:  func(device, target string) error { mounts = append(mounts, [2]string{device, target}); return nil },
		bindMount: func(source, target string) error { mounts = append(mounts, [2]string{source, target}); return nil },
		receiveSeed: func(ctx context.Context, dial func(context.Context) (net.Conn, error), target string) error {
			seeded = target
			_, _ = dial(ctx)
			return nil
		},
		run: func(name string, args ...string) error {
			commands = append(commands, append([]string{name}, args...))
			return nil
		},
	}
	cfg := &agentv1.GuestConfig{WorkspaceDisk: &agentv1.GuestConfig_WorkspaceDisk{
		Device: testWorkspaceDevice,
		Mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Target: overlayMergedTest, Directory: testWorkspaceDirectory, SeedPort: testWorkspaceSeedPort}},
	}}
	var output bytes.Buffer
	if err := applyWorkspaceDisk(t.Context(), &output, deps, cfg, func(_ context.Context, port uint32) (net.Conn, error) {
		seedPort = port
		return nil, errors.New("test receiver does not consume the connection")
	}); err != nil {
		t.Fatal(err)
	}
	wantCommand := []string{"/bin/mkfs.xfs", "-f", "-m", "reflink=1", testWorkspaceDevice}
	if len(commands) != 1 || !reflect.DeepEqual(commands[0], wantCommand) {
		t.Fatalf("commands = %v, want %v", commands, wantCommand)
	}
	if len(mounts) != 2 || mounts[1] != [2]string{workspaceDiskRoot + "/" + testWorkspaceDirectory, overlayMergedTest} {
		t.Fatalf("mounts = %v", mounts)
	}
	if seeded != workspaceDiskRoot+"/"+testWorkspaceDirectory || seedPort != testWorkspaceSeedPort {
		t.Fatalf("seeded = %q on %d", seeded, seedPort)
	}
	if !strings.Contains(output.String(), "1 mappings") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestApplyWorkspaceDiskRepairsExistingXFS(t *testing.T) {
	mountCalls := 0
	repaired := false
	deps := workspaceDiskDeps{
		readMagic: func(string) ([4]byte, error) { return [4]byte{'X', 'F', 'S', 'B'}, nil },
		mkdirAll:  func(string, os.FileMode) error { return nil },
		mountXFS: func(string, string) error {
			mountCalls++
			if mountCalls == 1 {
				return errors.New("dirty log")
			}
			return nil
		},
		bindMount: func(string, string) error { return nil },
		run: func(name string, args ...string) error {
			repaired = name == "/bin/xfs_repair" && len(args) == 1 && args[0] == testWorkspaceDevice
			return nil
		},
	}
	if err := applyWorkspaceDiskForTest(t, &bytes.Buffer{}, deps, &agentv1.GuestConfig{
		WorkspaceDisk: &agentv1.GuestConfig_WorkspaceDisk{Device: testWorkspaceDevice},
	}); err != nil {
		t.Fatal(err)
	}
	if !repaired || mountCalls != 2 {
		t.Fatalf("repaired=%v mountCalls=%d", repaired, mountCalls)
	}
}

func TestApplyWorkspaceDiskRejectsUnsafeConfig(t *testing.T) {
	deps := workspaceDiskDeps{
		readMagic: func(string) ([4]byte, error) { return [4]byte{'X', 'F', 'S', 'B'}, nil },
		mkdirAll:  func(string, os.FileMode) error { return nil },
		mountXFS:  func(string, string) error { return nil },
		bindMount: func(string, string) error { return nil },
		run:       func(string, ...string) error { return nil },
	}
	for _, disk := range []*agentv1.GuestConfig_WorkspaceDisk{
		{Device: "vda"},
		{Device: testWorkspaceDevice, Mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Target: "workspace", Directory: testWorkspaceDirectory}}},
		{Device: testWorkspaceDevice, Mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Target: overlayMergedTest, Directory: "../escape"}}},
	} {
		if err := applyWorkspaceDiskForTest(t, &bytes.Buffer{}, deps, &agentv1.GuestConfig{WorkspaceDisk: disk}); err == nil {
			t.Fatalf("unsafe disk accepted: %+v", disk)
		}
	}
	deps.readMagic = func(string) ([4]byte, error) { return [4]byte{}, nil }
	if err := applyWorkspaceDisk(t.Context(), &bytes.Buffer{}, deps, &agentv1.GuestConfig{WorkspaceDisk: &agentv1.GuestConfig_WorkspaceDisk{
		Device: testWorkspaceDevice,
		Mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Target: overlayMergedTest, Directory: testWorkspaceDirectory, SeedPort: testWorkspaceSeedPort}},
	}}, nil); err == nil {
		t.Fatal("first-launch seed without transport accepted")
	}
	if err := applyWorkspaceDiskForTest(t, &bytes.Buffer{}, deps, nil); err != nil {
		t.Fatalf("nil config: %v", err)
	}
}

func TestApplyWorkspaceDiskFailurePaths(t *testing.T) {
	xfsMagic := [4]byte{'X', 'F', 'S', 'B'}
	tests := []struct {
		name   string
		magic  [4]byte
		mounts []*agentv1.GuestConfig_WorkspaceDisk_Mount
		mutate func(*workspaceDiskDeps)
	}{
		{name: "read magic", mutate: func(d *workspaceDiskDeps) {
			d.readMagic = func(string) ([4]byte, error) { return [4]byte{}, errors.New("read") }
		}},
		{name: "unknown magic", magic: [4]byte{'E', 'X', 'T', '4'}},
		{name: "format", mutate: func(d *workspaceDiskDeps) {
			d.run = func(string, ...string) error { return errors.New("format") }
		}},
		{name: "root mkdir", magic: xfsMagic, mutate: func(d *workspaceDiskDeps) {
			d.mkdirAll = func(string, os.FileMode) error { return errors.New("mkdir") }
		}},
		{name: "repair", magic: xfsMagic, mutate: func(d *workspaceDiskDeps) {
			d.mountXFS = func(string, string) error { return errors.New("mount") }
			d.run = func(string, ...string) error { return errors.New("repair") }
		}},
		{name: "remount", magic: xfsMagic, mutate: func(d *workspaceDiskDeps) {
			d.mountXFS = func(string, string) error { return errors.New("mount") }
		}},
		{name: "new mount", mutate: func(d *workspaceDiskDeps) {
			d.mountXFS = func(string, string) error { return errors.New("mount") }
		}},
		{name: "source mkdir", magic: xfsMagic, mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Target: overlayMergedTest, Directory: testWorkspaceDirectory}}, mutate: func(d *workspaceDiskDeps) {
			calls := 0
			d.mkdirAll = func(string, os.FileMode) error {
				calls++
				if calls == 2 {
					return errors.New("source")
				}
				return nil
			}
		}},
		{name: "target mkdir", magic: xfsMagic, mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Target: overlayMergedTest, Directory: testWorkspaceDirectory}}, mutate: func(d *workspaceDiskDeps) {
			calls := 0
			d.mkdirAll = func(string, os.FileMode) error {
				calls++
				if calls == 3 {
					return errors.New("target")
				}
				return nil
			}
		}},
		{name: "bind", magic: xfsMagic, mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Target: overlayMergedTest, Directory: testWorkspaceDirectory}}, mutate: func(d *workspaceDiskDeps) {
			d.bindMount = func(string, string) error { return errors.New("bind") }
		}},
		{name: "seed", mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Target: overlayMergedTest, Directory: testWorkspaceDirectory, SeedPort: testWorkspaceSeedPort}}, mutate: func(d *workspaceDiskDeps) {
			d.receiveSeed = func(context.Context, func(context.Context) (net.Conn, error), string) error {
				return errors.New("receive")
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := workspaceDiskDeps{
				readMagic:   func(string) ([4]byte, error) { return tc.magic, nil },
				mkdirAll:    func(string, os.FileMode) error { return nil },
				mountXFS:    func(string, string) error { return nil },
				bindMount:   func(string, string) error { return nil },
				receiveSeed: func(context.Context, func(context.Context) (net.Conn, error), string) error { return nil },
				run:         func(string, ...string) error { return nil },
			}
			if tc.mutate != nil {
				tc.mutate(&deps)
			}
			err := applyWorkspaceDiskForTest(t, &bytes.Buffer{}, deps, &agentv1.GuestConfig{
				WorkspaceDisk: &agentv1.GuestConfig_WorkspaceDisk{Device: testWorkspaceDevice, Mounts: tc.mounts},
			})
			if err == nil {
				t.Fatalf("%s failure ignored", tc.name)
			}
		})
	}
}
