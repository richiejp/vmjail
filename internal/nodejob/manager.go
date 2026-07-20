// Package nodejob owns detached command lifecycles on a sandbox Node.
// Jobs outlive the RPC that starts them, but intentionally do not survive a
// sandboxd process restart. The manager keeps bounded, independently cursored
// stdout and stderr spools for resumable polling.
package nodejob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	DefaultMaxJobs       = 256
	DefaultSpoolBytes    = 1 << 20
	DefaultMaxReadBytes  = 64 << 10
	MaxCommandArgs       = 256
	MaxCommandArgBytes   = 16 << 10
	MaxSandboxUIDBytes   = 128
	MaxTerminalReasonLen = 16 << 10
)

var (
	ErrNotFound      = errors.New("nodejob: job not found")
	ErrCapacity      = errors.New("nodejob: job capacity reached")
	ErrInvalidCursor = errors.New("nodejob: cursor is ahead of spool")
	ErrTerminal      = errors.New("nodejob: job is already terminal")
)

type State string

const (
	StateStarting  State = "STARTING"
	StateRunning   State = "RUNNING"
	StateCanceling State = "CANCELING"
	StateSucceeded State = "SUCCEEDED"
	StateFailed    State = "FAILED"
	StateCanceled  State = "CANCELED"
)

func (s State) Terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCanceled
}

type Limits struct {
	MaxJobs      int
	SpoolBytes   int
	MaxReadBytes int
}

func (l Limits) withDefaults() Limits {
	if l.MaxJobs <= 0 {
		l.MaxJobs = DefaultMaxJobs
	}
	if l.SpoolBytes <= 0 {
		l.SpoolBytes = DefaultSpoolBytes
	}
	if l.MaxReadBytes <= 0 {
		l.MaxReadBytes = DefaultMaxReadBytes
	}
	return l
}

type StartRequest struct {
	SandboxUID string
	Command    []string
}

type Result struct {
	ExitCode int32
	Reason   string
}

// Process is a started guest command. Wait must be safe to call exactly once;
// Cancel must be safe concurrently with Wait and should be idempotent.
type Process interface {
	Wait() (Result, error)
	Cancel() error
}

// Runner starts the provider-specific non-PTY guest execution transport.
// stdout and stderr are concurrency-safe bounded writers owned by Manager.
type Runner interface {
	Start(ctx context.Context, req StartRequest, stdout, stderr io.Writer) (Process, error)
}

type Chunk struct {
	Data          []byte
	NextCursor    uint64
	CursorExpired bool
}

type Snapshot struct {
	ID           string
	SandboxUID   string
	Command      []string
	State        State
	Stdout       Chunk
	Stderr       Chunk
	ExitCode     int32
	ExitCodeSet  bool
	Reason       string
	CreatedAt    time.Time
	StartedAt    time.Time
	CompletedAt  time.Time
	CancelCalled bool
}

type Manager struct {
	ctx    context.Context
	runner Runner
	limits Limits
	now    func() time.Time

	mu   sync.RWMutex
	jobs map[string]*job
}

type job struct {
	mu sync.Mutex

	id         string
	sandboxUID string
	command    []string
	state      State
	stdout     *spool
	stderr     *spool
	process    Process
	cancel     context.CancelFunc

	exitCode     int32
	exitCodeSet  bool
	reason       string
	createdAt    time.Time
	startedAt    time.Time
	completedAt  time.Time
	cancelCalled bool
}

func New(ctx context.Context, runner Runner, limits Limits) (*Manager, error) {
	if ctx == nil {
		return nil, errors.New("nodejob: nil lifecycle context")
	}
	if runner == nil {
		return nil, errors.New("nodejob: nil runner")
	}
	m := &Manager{ctx: ctx, runner: runner, limits: limits.withDefaults(), now: time.Now, jobs: map[string]*job{}}
	go m.cancelOnShutdown()
	return m, nil
}

func (m *Manager) Start(req StartRequest) (Snapshot, error) {
	if err := validateStart(req); err != nil {
		return Snapshot{}, err
	}
	if err := m.ctx.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("nodejob: manager stopped: %w", err)
	}
	m.mu.Lock()
	if len(m.jobs) >= m.limits.MaxJobs {
		m.mu.Unlock()
		return Snapshot{}, ErrCapacity
	}
	id, err := mintJobID()
	if err != nil {
		m.mu.Unlock()
		return Snapshot{}, err
	}
	jobCtx, cancel := context.WithCancel(m.ctx)
	j := &job{
		id: id, sandboxUID: req.SandboxUID, command: append([]string(nil), req.Command...),
		state: StateStarting, stdout: newSpool(m.limits.SpoolBytes), stderr: newSpool(m.limits.SpoolBytes),
		cancel: cancel, createdAt: m.now(),
	}
	m.jobs[id] = j
	m.mu.Unlock()

	proc, err := m.runner.Start(jobCtx, req, j.stdout, j.stderr)
	if err != nil {
		j.finish(m.now(), Result{Reason: boundedReason(err.Error())}, StateFailed)
		return m.snapshot(j, 0, 0, m.limits.MaxReadBytes)
	}
	if proc == nil {
		j.finish(m.now(), Result{Reason: "runner returned a nil process"}, StateFailed)
		return m.snapshot(j, 0, 0, m.limits.MaxReadBytes)
	}
	j.mu.Lock()
	j.process = proc
	j.startedAt = m.now()
	if j.cancelCalled {
		j.state = StateCanceling
	} else {
		j.state = StateRunning
	}
	cancelNow := j.cancelCalled
	j.mu.Unlock()
	if cancelNow {
		_ = proc.Cancel()
	}
	go m.wait(j, proc)
	return m.snapshot(j, 0, 0, m.limits.MaxReadBytes)
}

func (m *Manager) Get(id string, stdoutCursor, stderrCursor uint64, maxBytes int) (Snapshot, error) {
	j, err := m.lookup(id)
	if err != nil {
		return Snapshot{}, err
	}
	if maxBytes <= 0 || maxBytes > m.limits.MaxReadBytes {
		maxBytes = m.limits.MaxReadBytes
	}
	return m.snapshot(j, stdoutCursor, stderrCursor, maxBytes)
}

func (m *Manager) Cancel(id string) (Snapshot, error) {
	j, err := m.lookup(id)
	if err != nil {
		return Snapshot{}, err
	}
	j.mu.Lock()
	if j.state.Terminal() {
		j.mu.Unlock()
		return Snapshot{}, ErrTerminal
	}
	j.cancelCalled = true
	j.state = StateCanceling
	proc := j.process
	j.cancel()
	j.mu.Unlock()
	if proc != nil {
		_ = proc.Cancel()
	}
	return m.snapshot(j, 0, 0, m.limits.MaxReadBytes)
}

func (m *Manager) wait(j *job, proc Process) {
	result, err := proc.Wait()
	if err != nil && result.Reason == "" {
		result.Reason = err.Error()
	}
	result.Reason = boundedReason(result.Reason)
	j.complete(m.now(), result, err)
}

func (j *job) complete(now time.Time, result Result, waitErr error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.state = StateFailed
	if j.cancelCalled {
		j.state = StateCanceled
	} else if waitErr == nil && result.Reason == "" && result.ExitCode == 0 {
		j.state = StateSucceeded
	}
	j.setTerminal(now, result)
}

func (j *job) finish(now time.Time, result Result, state State) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.state = state
	j.setTerminal(now, result)
}

func (j *job) setTerminal(now time.Time, result Result) {
	j.exitCode = result.ExitCode
	j.exitCodeSet = result.Reason == "" || result.ExitCode != 0
	j.reason = result.Reason
	j.completedAt = now
	j.cancel()
}

func (m *Manager) cancelOnShutdown() {
	<-m.ctx.Done()
	m.mu.RLock()
	ids := make([]string, 0, len(m.jobs))
	for id := range m.jobs {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	for _, id := range ids {
		_, _ = m.Cancel(id)
	}
}

func (m *Manager) lookup(id string) (*job, error) {
	m.mu.RLock()
	j := m.jobs[id]
	m.mu.RUnlock()
	if j == nil {
		return nil, ErrNotFound
	}
	return j, nil
}

func (m *Manager) snapshot(j *job, stdoutCursor, stderrCursor uint64, maxBytes int) (Snapshot, error) {
	out, err := j.stdout.read(stdoutCursor, maxBytes)
	if err != nil {
		return Snapshot{}, fmt.Errorf("nodejob: stdout: %w", err)
	}
	errChunk, err := j.stderr.read(stderrCursor, maxBytes)
	if err != nil {
		return Snapshot{}, fmt.Errorf("nodejob: stderr: %w", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return Snapshot{
		ID: j.id, SandboxUID: j.sandboxUID, Command: append([]string(nil), j.command...), State: j.state,
		Stdout: out, Stderr: errChunk, ExitCode: j.exitCode, ExitCodeSet: j.exitCodeSet, Reason: j.reason,
		CreatedAt: j.createdAt, StartedAt: j.startedAt, CompletedAt: j.completedAt, CancelCalled: j.cancelCalled,
	}, nil
}

func validateStart(req StartRequest) error {
	if req.SandboxUID == "" || len(req.SandboxUID) > MaxSandboxUIDBytes {
		return errors.New("nodejob: sandbox UID is empty or too long")
	}
	if len(req.Command) == 0 || len(req.Command) > MaxCommandArgs {
		return errors.New("nodejob: command must contain 1..256 arguments")
	}
	for _, arg := range req.Command {
		if arg == "" || len(arg) > MaxCommandArgBytes {
			return errors.New("nodejob: command argument is empty or too long")
		}
	}
	return nil
}

func boundedReason(reason string) string {
	if len(reason) > MaxTerminalReasonLen {
		return reason[:MaxTerminalReasonLen]
	}
	return reason
}

func mintJobID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("nodejob: mint job UUID: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}
