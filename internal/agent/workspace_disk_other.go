//go:build !linux

package agent

import (
	"context"
	"errors"
	"net"
	"os"

	"github.com/richiejp/darkaifactory/internal/workspacesnapshot"
)

func defaultWorkspaceDiskDeps() workspaceDiskDeps {
	err := func() error { return errors.New("agent: workspace disks unsupported off Linux") }
	return workspaceDiskDeps{
		readMagic:   func(string) ([4]byte, error) { return [4]byte{}, err() },
		mkdirAll:    func(string, os.FileMode) error { return err() },
		mountXFS:    func(string, string) error { return err() },
		bindMount:   func(string, string) error { return err() },
		receiveSeed: func(context.Context, func(context.Context) (net.Conn, error), string) error { return err() },
		run:         func(context.Context, string, ...string) error { return err() },
		usage:       func(string) (workspacesnapshot.Usage, error) { return workspacesnapshot.Usage{}, err() },
	}
}
