package agent

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
)

const (
	testWorkspaceDevice    = "/dev/vda"
	testWorkspaceDirectory = "mounts/0"
	testWorkspaceSeed      = "/seed"
)

func TestApplyWorkspaceDiskFormatsAndMaps(t *testing.T) {
	var commands [][]string
	var mounts [][2]string
	var copied [2]string
	deps := workspaceDiskDeps{
		readMagic: func(string) ([4]byte, error) { return [4]byte{}, nil },
		mkdirAll:  func(string, os.FileMode) error { return nil },
		mountXFS:  func(device, target string) error { mounts = append(mounts, [2]string{device, target}); return nil },
		bindMount: func(source, target string) error { mounts = append(mounts, [2]string{source, target}); return nil },
		copyTree:  func(source, target string) error { copied = [2]string{source, target}; return nil },
		run: func(name string, args ...string) error {
			commands = append(commands, append([]string{name}, args...))
			return nil
		},
	}
	cfg := &agentv1.GuestConfig{WorkspaceDisk: &agentv1.GuestConfig_WorkspaceDisk{
		Device: testWorkspaceDevice,
		Mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Target: overlayMergedTest, Directory: testWorkspaceDirectory, Seed: testWorkspaceSeed}},
	}}
	var output bytes.Buffer
	if err := applyWorkspaceDisk(&output, deps, cfg); err != nil {
		t.Fatal(err)
	}
	wantCommand := []string{"/bin/mkfs.xfs", "-f", "-m", "reflink=1", testWorkspaceDevice}
	if len(commands) != 1 || !reflect.DeepEqual(commands[0], wantCommand) {
		t.Fatalf("commands = %v, want %v", commands, wantCommand)
	}
	if len(mounts) != 2 || mounts[1] != [2]string{workspaceDiskRoot + "/" + testWorkspaceDirectory, overlayMergedTest} {
		t.Fatalf("mounts = %v", mounts)
	}
	if copied != [2]string{testWorkspaceSeed, workspaceDiskRoot + "/" + testWorkspaceDirectory} {
		t.Fatalf("copied = %v", copied)
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
		copyTree:  func(string, string) error { return nil },
		run: func(name string, args ...string) error {
			repaired = name == "/bin/xfs_repair" && len(args) == 1 && args[0] == testWorkspaceDevice
			return nil
		},
	}
	if err := applyWorkspaceDisk(&bytes.Buffer{}, deps, &agentv1.GuestConfig{
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
		copyTree:  func(string, string) error { return nil },
		run:       func(string, ...string) error { return nil },
	}
	for _, disk := range []*agentv1.GuestConfig_WorkspaceDisk{
		{Device: "vda"},
		{Device: testWorkspaceDevice, Mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Target: "workspace", Directory: testWorkspaceDirectory}}},
		{Device: testWorkspaceDevice, Mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Target: overlayMergedTest, Directory: "../escape"}}},
	} {
		if err := applyWorkspaceDisk(&bytes.Buffer{}, deps, &agentv1.GuestConfig{WorkspaceDisk: disk}); err == nil {
			t.Fatalf("unsafe disk accepted: %+v", disk)
		}
	}
	deps.readMagic = func(string) ([4]byte, error) { return [4]byte{}, nil }
	if err := applyWorkspaceDisk(&bytes.Buffer{}, deps, &agentv1.GuestConfig{WorkspaceDisk: &agentv1.GuestConfig_WorkspaceDisk{
		Device: testWorkspaceDevice,
		Mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Target: overlayMergedTest, Directory: testWorkspaceDirectory, Seed: "relative"}},
	}}); err == nil {
		t.Fatal("relative first-launch seed accepted")
	}
	if err := applyWorkspaceDisk(&bytes.Buffer{}, deps, nil); err != nil {
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
		{name: "seed", mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Target: overlayMergedTest, Directory: testWorkspaceDirectory, Seed: testWorkspaceSeed}}, mutate: func(d *workspaceDiskDeps) {
			d.copyTree = func(string, string) error { return errors.New("copy") }
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := workspaceDiskDeps{
				readMagic: func(string) ([4]byte, error) { return tc.magic, nil },
				mkdirAll:  func(string, os.FileMode) error { return nil },
				mountXFS:  func(string, string) error { return nil },
				bindMount: func(string, string) error { return nil },
				copyTree:  func(string, string) error { return nil },
				run:       func(string, ...string) error { return nil },
			}
			if tc.mutate != nil {
				tc.mutate(&deps)
			}
			err := applyWorkspaceDisk(&bytes.Buffer{}, deps, &agentv1.GuestConfig{
				WorkspaceDisk: &agentv1.GuestConfig_WorkspaceDisk{Device: testWorkspaceDevice, Mounts: tc.mounts},
			})
			if err == nil {
				t.Fatalf("%s failure ignored", tc.name)
			}
		})
	}
}
