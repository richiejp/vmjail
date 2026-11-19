package hostresources

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// procMeminfoPath is the path the resource probe reads MemTotal
// from. Overridable in tests; production callers leave it at the
// Linux default.
//
//nolint:gochecknoglobals // single test seam; no production mutation.
var procMeminfoPath = "/proc/meminfo"

// Resources describes the host CPU count and physical memory capacity.
type Resources struct {
	CPUs        uint32
	MemoryBytes uint64
}

// DetectResources reads the CPU count and MemTotal from the Linux host.
func DetectResources() (Resources, error) {
	memKB, err := readMemTotalKB(procMeminfoPath)
	if err != nil {
		return Resources{}, fmt.Errorf("hostresources: detect memory: %w", err)
	}
	cpus := max(runtime.NumCPU(), 0)
	return Resources{
		CPUs:        uint32(cpus), //nolint:gosec // NumCPU is non-negative; max(_,0) is the floor.
		MemoryBytes: memKB * 1024,
	}, nil
}

// readMemTotalKB returns the MemTotal line from a /proc/meminfo-
// shaped file in kibibytes (the unit /proc/meminfo uses). Returns
// an error if the file is unreadable or contains no MemTotal
// entry — both are signs of a sandboxd not actually running on
// Linux; return an error instead of reporting zero RAM.
func readMemTotalKB(path string) (uint64, error) {
	f, err := os.Open(path) //nolint:gosec // path is package-scoped and only overridden in tests.
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	//vmjail:lint:disable=forloopbound reason="bounded by io.EOF on a /proc/meminfo-shaped file; bufio.Scanner stops when the file does"
	for scanner.Scan() {
		line := scanner.Text()
		const prefix = "MemTotal:"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, prefix))
		if len(fields) == 0 {
			return 0, errors.New("hostresources: empty MemTotal line")
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("hostresources: parse MemTotal: %w", err)
		}
		return kb, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("hostresources: no MemTotal entry")
}
