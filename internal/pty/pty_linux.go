//go:build linux

// Package pty opens Linux pseudoterminal pairs.
package pty

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Open allocates a pseudoterminal through the current devpts mount. The caller
// owns both files.
func Open() (master, slave *os.File, err error) {
	return openPair("/dev/ptmx")
}

func openPair(path string) (_, _ *os.File, resultErr error) {
	// #nosec G304 -- Open uses /dev/ptmx; only tests supply another path.
	master, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open PTY master: %w", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, master.Close())
		}
	}()

	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return nil, nil, fmt.Errorf("unlock PTY slave: %w", err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		return nil, nil, fmt.Errorf("get PTY slave number: %w", err)
	}

	slavePath := fmt.Sprintf("/dev/pts/%d", number)
	// #nosec G304 -- the kernel assigns the slave number.
	slave, err := os.OpenFile(slavePath, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open PTY slave: %w", err)
	}
	return master, slave, nil
}
