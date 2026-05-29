//go:build !linux

package observability

import "errors"

// collectProcSample is Linux-only; the observability stack reads
// /proc/<pid>/{stat,status,io} which exists only there. Other
// platforms return ErrUnsupported and the Observer records the
// error in the Sample's Err field for each tick. We keep the
// package buildable cross-platform because the surrounding
// daft-sandboxd command needs to build on developer laptops
// running macOS for unit-test work, even though the runtime
// target is Linux.
func collectProcSample(_ int) (*ProcSample, error) {
	return nil, errors.New("observability: /proc sampling requires linux")
}
