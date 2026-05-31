// Package observability captures per-sandbox host-side resource
// telemetry into a JSON-lines log that survives the VM's life-time.
//
// It exists to answer the question "what was the sandbox VM doing
// when this test stalled / failed", without requiring any code or
// kernel feature inside the guest. Tier 1 of the observability
// stack (see docs/observability.md): the cheap, always-on layer
// that flags idle-vs-busy from cgroup / process counters. Tier 2
// (guest-side eBPF programs under future internal/observability/
// ebpf/) answers "what specifically".
//
// One Observer per sandbox VM. The Observer samples a target PID
// (the cloud-hypervisor process) at SampleEvery, reads its /proc/
// counters, and appends one JSON object per line to LogPath. The
// goroutine is owned by Start/Stop on the parent (sandbox.Run);
// the file is rotated once at MaxBytes/2 so the on-disk footprint
// stays bounded at ~MaxBytes per sandbox.
//
// /proc is the v1 source. Cgroup v2 (cpu.stat / memory.peak /
// io.stat) is a follow-up: it gives identical-or-better numbers,
// but requires cgroup delegation that isn't universal across host
// environments. We start with /proc so the feature lands on every
// Linux host today, then upgrade callers to cgroup as the better
// source.
package observability

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Default sampling cadence and on-disk cap. Chosen to be cheap
// enough that every test can leave the observer on by default
// (~500 B per sample × 1 Hz × 10 min = 300 KB / test) while still
// useful for hours-long manual sandbox sessions before rotation
// kicks in.
const (
	DefaultSampleEvery = time.Second
	DefaultMaxBytes    = int64(50 * 1024 * 1024) // 50 MB total, ~25 MB current + ~25 MB .1
	// DefaultLogFilename is the conventional basename for a per-sandbox
	// observer trace living at <state-dir>/<sandbox-id>/<here>. The
	// daft-sandboxd launch paths default Config.ObserverLogPath to it
	// and `daft sandbox observer-tail` (roadmap) finds the log from a
	// sandbox id alone — one source of truth so the two never drift.
	DefaultLogFilename = "observer.jsonl"
)

// DefaultPath returns the per-sandbox observer-log path
// <parent>/<id>/observer.jsonl, creating the <parent>/<id> directory
// (which persists past VM exit for post-mortem inspection). It is the
// single place that joins the DefaultLogFilename convention onto a
// state dir + sandbox id, shared by every daft-sandboxd launch path
// (CLI hello/run and orchestrator dispatch) so the layout can't drift.
func DefaultPath(parent, id string) (string, error) {
	dir := filepath.Join(parent, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("observability: mkdir log dir %s: %w", dir, err)
	}
	return filepath.Join(dir, DefaultLogFilename), nil
}

// Sample is the per-tick record written to the log. JSON tags are
// the public schema; renames require a doc update and a version
// bump to docs/observability.md.
type Sample struct {
	// TS is the wall-clock time the sample was taken (RFC3339Nano).
	TS time.Time `json:"ts"`
	// PID is the target process the sample is about.
	PID int `json:"pid"`
	// Proc is the /proc/<pid> reading. Absent when reading failed
	// — e.g. the target exited between two samples — in which
	// case Err carries the reason.
	Proc *ProcSample `json:"proc,omitempty"`
	// Cgroup is the per-VM cgroup v2 reading. Absent when the
	// observer was configured without a CgroupDir, or when the
	// cgroup files couldn't be read this tick. The cgroup view
	// aggregates every PID in the workload's cgroup tree (CH +
	// future virtiofsd/helpers), so it's the more meaningful
	// signal once the cgroup wrap is in place.
	Cgroup *CgroupSample `json:"cgroup,omitempty"`
	// Net is the per-VM userspace network reading. Absent when the
	// observer was configured without a CollectNet callback (no
	// sandboxnet wired in), or when the callback returns nil. The
	// counters are wire-bytes through the StdioProtocol vsock conn
	// between sandboxd and the guest agent — they include the
	// 2-byte size prefix per frame. Cumulative from VM start;
	// deltas reconstructed in post.
	Net *NetSample `json:"net,omitempty"`
	// Err is the read error this tick, if any. Present alongside
	// no Proc; the observer keeps running so the next successful
	// sample lands as soon as the target reappears (or the
	// observer is stopped).
	Err string `json:"err,omitempty"`
}

// ProcSample is the per-tick projection of /proc/<pid>/{stat,
// status,io}. Field choice biased to the questions "is the
// process making forward progress" (utime/stime jiffies, syscw/
// syscr counts) and "what is its footprint" (RSS, threads, IO
// byte totals).
type ProcSample struct {
	Comm         string `json:"comm"`
	State        string `json:"state"`
	UtimeJiffies uint64 `json:"utime_jiffies"`
	StimeJiffies uint64 `json:"stime_jiffies"`
	NumThreads   int    `json:"num_threads"`
	VMRSSKB      uint64 `json:"vm_rss_kb"`
	VMSizeKB     uint64 `json:"vm_size_kb"`
	// IO byte/syscall counters from /proc/<pid>/io. These are
	// cumulative from process start; deltas reconstructed in post.
	Rchar      uint64 `json:"rchar"`
	Wchar      uint64 `json:"wchar"`
	Syscr      uint64 `json:"syscr"`
	Syscw      uint64 `json:"syscw"`
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
}

// NetSample is the per-tick projection of the per-VM userspace
// network stack's wire counters. RxBytes is bytes flowing from the
// guest into sandboxd; TxBytes is bytes flowing from sandboxd to
// the guest. Both include StdioProtocol 2-byte size headers.
type NetSample struct {
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}

// Options tunes an Observer at construction time.
type Options struct {
	// SampleEvery is the cadence between /proc reads. Zero means
	// DefaultSampleEvery.
	SampleEvery time.Duration
	// MaxBytes caps the total on-disk footprint across the live
	// file and one rotated backup. Zero means DefaultMaxBytes.
	// Negative means "no rotation" — for tests that want the
	// whole log in one file.
	MaxBytes int64
	// CgroupDir, when non-empty, instructs the observer to read
	// the workload's cgroup v2 files (cpu.stat / memory.* /
	// io.stat / pids.*) once per tick in addition to /proc/<pid>.
	// Path must be the absolute filesystem path to the cgroup
	// directory (e.g. /sys/fs/cgroup/.../daft-<id>). Empty
	// disables cgroup sampling; the /proc sampler still runs.
	CgroupDir string
	// CollectNet, when non-nil, is called once per tick to capture
	// the current per-VM network byte counters. nil disables the
	// network block; the rest of the sample still ticks. Returning
	// nil from the callback also omits the block for this tick.
	// Implementation must be safe for concurrent use — it shares
	// state with the netstack's accept loop.
	CollectNet func() *NetSample
	// Now is the clock source for the TS field. nil means
	// time.Now. Injectable so tests can pin timestamps.
	Now func() time.Time
}

// Observer samples a PID and writes JSON-lines to a log file
// until Stop is called. Construct with New; Start does not happen
// implicitly — the caller controls when sampling begins so the
// first sample falls inside the workload window, not before it.
type Observer struct {
	pid         int
	logPath     string
	cgroupDir   string
	sampleEvery time.Duration
	maxBytes    int64
	now         func() time.Time
	collect     func(pid int) (*ProcSample, error)
	collectCG   func(dir string) (*CgroupSample, error)
	collectNet  func() *NetSample

	mu      sync.Mutex
	file    *os.File
	written int64

	stop chan struct{}
	done chan struct{}
	err  error
}

// New constructs an Observer for pid, writing to logPath. The log
// directory must already exist. The Observer's goroutine doesn't
// start until Start; closing without Start is safe.
func New(pid int, logPath string, opts Options) (*Observer, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("observability: pid must be > 0, got %d", pid)
	}
	if logPath == "" {
		return nil, errors.New("observability: logPath required")
	}
	if !filepath.IsAbs(logPath) {
		return nil, fmt.Errorf("observability: logPath must be absolute, got %q", logPath)
	}
	every := opts.SampleEvery
	if every == 0 {
		every = DefaultSampleEvery
	}
	if every < 10*time.Millisecond {
		return nil, fmt.Errorf("observability: SampleEvery must be >= 10ms, got %s", every)
	}
	maxBytes := opts.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.CgroupDir != "" && !filepath.IsAbs(opts.CgroupDir) {
		return nil, fmt.Errorf("observability: CgroupDir must be absolute, got %q", opts.CgroupDir)
	}
	return &Observer{
		pid:         pid,
		logPath:     logPath,
		cgroupDir:   opts.CgroupDir,
		sampleEvery: every,
		maxBytes:    maxBytes,
		now:         now,
		collect:     collectProcSample,
		collectCG:   collectCgroupSample,
		collectNet:  opts.CollectNet,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}, nil
}

// Start begins sampling in a goroutine and returns immediately. It
// is safe to call Start exactly once per Observer; subsequent
// calls return an error.
func (o *Observer) Start() error {
	o.mu.Lock()
	if o.file != nil {
		o.mu.Unlock()
		return errors.New("observability: observer already started")
	}
	// #nosec G304 — logPath is caller-controlled, validated absolute, and
	// only ever written by this process.
	f, err := os.OpenFile(o.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		o.mu.Unlock()
		return fmt.Errorf("observability: open log %s: %w", o.logPath, err)
	}
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		o.mu.Unlock()
		return fmt.Errorf("observability: stat log %s: %w", o.logPath, err)
	}
	o.file = f
	o.written = stat.Size()
	o.mu.Unlock()

	go o.run()
	return nil
}

// Stop signals the goroutine to exit, waits for it to finish, and
// returns the goroutine's terminal error (e.g. a final write
// failure). Safe to call multiple times; subsequent calls return
// the same recorded error.
func (o *Observer) Stop() error {
	o.mu.Lock()
	if o.file == nil {
		o.mu.Unlock()
		return errors.New("observability: observer not started")
	}
	select {
	case <-o.stop:
		// already stopping
	default:
		close(o.stop)
	}
	o.mu.Unlock()
	<-o.done
	return o.err
}

// LogPath returns the absolute path the observer writes to. Useful
// for tests and CLI surfaces that want to point a user at the log.
func (o *Observer) LogPath() string { return o.logPath }

func (o *Observer) run() {
	defer close(o.done)
	t := time.NewTicker(o.sampleEvery)
	defer t.Stop()

	// First sample fires immediately so a quickly-failing
	// workload still leaves at least one record on disk.
	if err := o.tick(); err != nil {
		o.finish(err)
		return
	}
	//daft:lint:disable=forloopbound reason="exits when o.stop is closed by Stop, or when tick reports a fatal write/marshal error"
	for {
		select {
		case <-o.stop:
			o.finish(nil)
			return
		case <-t.C:
			if err := o.tick(); err != nil {
				o.finish(err)
				return
			}
		}
	}
}

func (o *Observer) tick() error {
	sample := Sample{TS: o.now(), PID: o.pid}
	proc, err := o.collect(o.pid)
	switch {
	case err == nil:
		sample.Proc = proc
	case errors.Is(err, os.ErrNotExist):
		// Target exited. Record the gap and keep ticking — the
		// next call may succeed if the caller restarts the
		// target, and Stop will catch up either way.
		sample.Err = "pid gone"
	default:
		sample.Err = err.Error()
	}
	if o.collectNet != nil {
		sample.Net = o.collectNet()
	}
	if o.cgroupDir != "" {
		cg, cgErr := o.collectCG(o.cgroupDir)
		switch {
		case cgErr == nil:
			sample.Cgroup = cg
		case errors.Is(cgErr, os.ErrNotExist):
			// Cgroup directory gone — the workload's been
			// cleaned up. Don't overwrite Err if /proc already
			// set it; just leave Cgroup nil.
			if sample.Err == "" {
				sample.Err = "cgroup gone"
			}
		default:
			if sample.Err == "" {
				sample.Err = cgErr.Error()
			}
		}
	}
	return o.write(sample)
}

func (o *Observer) write(s Sample) error {
	line, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("observability: marshal sample: %w", err)
	}
	line = append(line, '\n')

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file == nil {
		return errors.New("observability: file closed")
	}
	if o.maxBytes > 0 && o.written+int64(len(line)) > o.maxBytes/2 {
		if err := o.rotateLocked(); err != nil {
			return err
		}
	}
	n, err := o.file.Write(line)
	o.written += int64(n)
	if err != nil {
		return fmt.Errorf("observability: write log: %w", err)
	}
	return nil
}

// rotateLocked renames the current log to logPath+".1" and reopens
// a fresh logPath. Caller must hold o.mu. Total on-disk usage is
// bounded at MaxBytes (one current file up to MaxBytes/2, one
// rotated backup up to MaxBytes/2 written before the previous
// rotation).
func (o *Observer) rotateLocked() error {
	if err := o.file.Close(); err != nil {
		return fmt.Errorf("observability: close before rotate: %w", err)
	}
	o.file = nil
	o.written = 0
	if err := os.Rename(o.logPath, o.logPath+".1"); err != nil {
		return fmt.Errorf("observability: rotate: %w", err)
	}
	// #nosec G304 — same path as Start; validated absolute.
	f, err := os.OpenFile(o.logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("observability: reopen after rotate: %w", err)
	}
	o.file = f
	return nil
}

func (o *Observer) finish(runErr error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file != nil {
		if closeErr := o.file.Close(); closeErr != nil && runErr == nil {
			runErr = fmt.Errorf("observability: close log: %w", closeErr)
		}
		o.file = nil
	}
	o.err = runErr
}
