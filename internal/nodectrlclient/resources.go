package nodectrlclient

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/richiejp/darkaifactory/internal/nodectrl"
)

// procMeminfoPath is the path the resource probe reads MemTotal
// from. Overridable in tests; production callers leave it at the
// Linux default.
//
//nolint:gochecknoglobals // single test seam; no production mutation.
var procMeminfoPath = "/proc/meminfo"

// DetectResources reports the host's capacity for advertisement
// at register-time. Linux-only — sandboxd does not build on other
// platforms (`//go:build linux` is enforced upstream via the
// daft-agent tag catalog), so the /proc/meminfo dependency is
// fine.
//
// CPU count comes from runtime.NumCPU() so it honours
// GOMAXPROCS / taskset / cgroup CPU sets.
func DetectResources() (nodectrl.Resources, error) {
	memKB, err := readMemTotalKB(procMeminfoPath)
	if err != nil {
		return nodectrl.Resources{}, fmt.Errorf("nodectrlclient: detect memory: %w", err)
	}
	cpus := max(runtime.NumCPU(), 0)
	return nodectrl.Resources{
		CPUs:        uint32(cpus), //nolint:gosec // NumCPU is non-negative; max(_,0) is the floor.
		MemoryBytes: memKB * 1024,
	}, nil
}

// readMemTotalKB returns the MemTotal line from a /proc/meminfo-
// shaped file in kibibytes (the unit /proc/meminfo uses). Returns
// an error if the file is unreadable or contains no MemTotal
// entry — both are signs of a sandboxd not actually running on
// Linux and we'd rather fail-fast at register-time than advertise
// 0 RAM.
func readMemTotalKB(path string) (uint64, error) {
	f, err := os.Open(path) //nolint:gosec // path is package-scoped and only overridden in tests.
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	//daft:lint:disable=forloopbound reason="bounded by io.EOF on a /proc/meminfo-shaped file; bufio.Scanner stops when the file does"
	for scanner.Scan() {
		line := scanner.Text()
		const prefix = "MemTotal:"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, prefix))
		if len(fields) == 0 {
			return 0, errors.New("nodectrlclient: empty MemTotal line")
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("nodectrlclient: parse MemTotal: %w", err)
		}
		return kb, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("nodectrlclient: no MemTotal entry")
}
