//go:build !linux

package agent

import (
	"errors"
	"os"
)

func defaultWorkspaceDiskDeps() workspaceDiskDeps {
	err := func() error { return errors.New("agent: workspace disks unsupported off Linux") }
	return workspaceDiskDeps{
		readMagic: func(string) ([4]byte, error) { return [4]byte{}, err() },
		mkdirAll:  func(string, os.FileMode) error { return err() },
		mountXFS:  func(string, string) error { return err() },
		bindMount: func(string, string) error { return err() },
		copyTree:  func(string, string) error { return err() },
		run:       func(string, ...string) error { return err() },
	}
}
