//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// virtiofsBenchProg is an in-guest microbench (run via the baked Go) that
// measures, for a directory of many small files, the per-open+read latency
// in three modes, so we can attribute the dogfood self-build's cold-compile
// stall to a specific virtiofs property:
//
//   - serial-cold : first read of each file → the raw per-request round trip.
//   - serial-warm : immediate re-read of the same files → reveals whether the
//     guest is allowed to page-cache content (cache=auto/always) or every read
//     round-trips (cache=never / no guest cache).
//   - par8-cold   : 8 goroutines over a *distinct* (never-read) half → whether
//     concurrent reads scale (virtiofsd thread pool) or serialize (pool=0).
//
// Run against a virtiofs dir and a tmpfs copy of the same tree so the tmpfs
// numbers are the round-trip-free baseline.
const virtiofsBenchProg = `package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func collect(dir string, max int) []string {
	var fs []string
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, e error) error {
		if e != nil {
			return nil
		}
		if fi.Mode().IsRegular() && fi.Size() > 0 {
			fs = append(fs, p)
		}
		if len(fs) >= max {
			return filepath.SkipAll
		}
		return nil
	})
	sort.Strings(fs)
	return fs
}

func readSerial(files []string) {
	buf := make([]byte, 1<<16)
	for _, f := range files {
		fd, err := os.Open(f)
		if err != nil {
			continue
		}
		for {
			if _, e := fd.Read(buf); e != nil {
				break
			}
		}
		fd.Close()
	}
}

func readPar(files []string, workers int) {
	var idx int64 = -1
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 1<<16)
			for {
				i := atomic.AddInt64(&idx, 1)
				if int(i) >= len(files) {
					return
				}
				fd, err := os.Open(files[i])
				if err != nil {
					continue
				}
				for {
					if _, e := fd.Read(buf); e != nil {
						break
					}
				}
				fd.Close()
			}
		}()
	}
	wg.Wait()
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func main() {
	dir := os.Args[1]
	files := collect(dir, 8000)
	n := len(files)
	if n < 4 {
		fmt.Printf("DIR %s: too few files (%d)\n", dir, n)
		return
	}
	a := files[:n/2]
	b := files[n/2:]

	t := time.Now()
	readSerial(a)
	cold := time.Since(t)
	t = time.Now()
	readSerial(a)
	warm := time.Since(t)
	t = time.Now()
	readPar(b, 8)
	par := time.Since(t)

	fmt.Printf("DIR %s  files=%d (a=%d b=%d)\n", dir, n, len(a), len(b))
	fmt.Printf("  serial-cold : %9.1f ms  %8.4f ms/file\n", ms(cold), ms(cold)/float64(len(a)))
	fmt.Printf("  serial-warm : %9.1f ms  %8.4f ms/file  (cold/warm %.1fx)\n", ms(warm), ms(warm)/float64(len(a)), float64(cold)/float64(warm))
	fmt.Printf("  par8-cold   : %9.1f ms  %8.4f ms/file  (vs serial-cold/file %.1fx)\n", ms(par), ms(par)/float64(len(b)), (ms(cold)/float64(len(a)))/(ms(par)/float64(len(b))))
}
`

// TestClusterVirtiofsReadLatency measures the per-op virtiofs read latency
// inside the sandbox to pin down the dogfood self-build's cold-compile stall.
// Host-disk I/O was already falsified (pgpgin ~0 during compile), so this
// isolates the virtiofs transport: cold round-trip, guest-cache behaviour, and
// concurrency scaling, vs a tmpfs baseline. Gated — it's a diagnostic, not a
// gate.
func TestClusterVirtiofsReadLatency(t *testing.T) {
	if os.Getenv("DAFT_RUN_VFSBENCH") != "1" {
		t.Skip("set DAFT_RUN_VFSBENCH=1 to run the virtiofs read-latency microbench")
	}
	imgPath := requireEnvPath(t, "DAFT_CHECK_IMAGE",
		"nix build .#daft-check-image -o bin/daft-check-image")
	ref := pushDockerTarballToRegistry(t, imgPath, "test/vfsbench:v1")

	// Mount a tree of many small files (the vendored Go deps) — the same
	// kind of read pattern a compile hits — and drop the bench program in.
	srcDir := t.TempDir()
	exportVendorTo(t, srcDir)
	if err := os.WriteFile(filepath.Join(srcDir, "bench.go"), []byte(virtiofsBenchProg), 0o644); err != nil {
		t.Fatalf("write bench.go: %v", err)
	}

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)
	wsName := "vfsbench"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "VFSB", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf("%s workspace create --name %s %s", prefix, wsName, orch)},
		{Label: "image", Cmd: fmt.Sprintf("%s workspace image %s --preserve-command --name %s %s", prefix, ref, wsName, orch)},
		{Label: "mount", Cmd: fmt.Sprintf("%s workspace mount add /workspace --source %s --name %s %s", prefix, srcDir, wsName, orch)},
		{Label: "resources", Cmd: fmt.Sprintf("%s workspace resources --memory 8G --cpus 4 --name %s %s", prefix, wsName, orch)},
		{Label: "target", Cmd: fmt.Sprintf("%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch)},
		{Label: "launch", Cmd: fmt.Sprintf("%s workspace launch --name %s %s", prefix, wsName, orch)},
	}, 30*time.Second)

	c.WaitForSandboxdOutput(t, "daft-check ready", 60*time.Second)

	logf := filepath.Join(t.TempDir(), "vfsbench.log")
	// Bench /workspace (virtiofs) FIRST so its files are cold, then copy the
	// tree to /tmp (tmpfs, RAM) and bench that as the round-trip-free baseline.
	execShell := "exec env HOME=/tmp PATH=/bin GOTOOLCHAIN=local GO111MODULE=off GOPROXY=off " +
		"GOCACHE=/tmp/gc GOPATH=/tmp/gp /bin/sh -c '" +
		"mkdir -p /tmp/gc /tmp/gp && cp /workspace/bench.go /tmp/bench.go && " +
		"echo VFS-BENCH-START && " +
		"go run /tmp/bench.go /workspace/vendor && " +
		"cp -r /workspace/vendor /tmp/vcopy && " +
		"go run /tmp/bench.go /tmp/vcopy && " +
		"echo VFS-BENCH-DONE'"

	proc := startDaftSubprocess(t, logf,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName, "--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/sh", "-c", execShell)
	waitForFileContains(t, proc, logf, "VFS-BENCH-DONE", 5*time.Minute,
		func() string { return s.Capture() })

	out, _ := os.ReadFile(logf) //nolint:gosec // logf is under t.TempDir; test-only.
	t.Logf("virtiofs read-latency bench:\n%s", out)
}
