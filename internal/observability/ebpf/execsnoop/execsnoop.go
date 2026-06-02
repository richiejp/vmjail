//go:build linux

package execsnoop

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// recordSize is the wire size of the BPF `struct event` from
// execsnoop_bpf.c: pid+ppid+uid+ret+args_count+args_size (6×4) + comm[16] +
// args[2560]. A drift guard in the tests asserts this matches
// binary.Size(execsnoopEvent{}) so a C-struct change that regenerates the
// loader can't silently desync the decoder.
const recordSize = 24 + 16 + 2560

// Field offsets within a record.
const (
	offPPID      = 4
	offUID       = 8
	offRet       = 12
	offArgsCount = 16
	offArgsSize  = 20
	offComm      = 24
	offArgs      = 40
	argsCap      = 2560
	// maxArgs mirrors MAX_ARGS in execsnoop_bpf.c: an args_count at the cap
	// means the argv may have been clipped.
	maxArgs = 20
)

// parseEvent decodes one ring-buffer record — the BPF `struct event` from
// execsnoop_bpf.c, little-endian — into an Event. A short record is rejected
// rather than mis-decoded. Pure: no BPF, unit-tested.
func parseEvent(raw []byte) (Event, error) {
	if len(raw) < recordSize {
		return Event{}, fmt.Errorf("execsnoop: short record: %d < %d bytes", len(raw), recordSize)
	}
	argsSize := min(binary.LittleEndian.Uint32(raw[offArgsSize:offComm]), argsCap)
	argsCount := int32(binary.LittleEndian.Uint32(raw[offArgsCount:offArgsSize])) //nolint:gosec // G115: reinterpret the wire's 32-bit word as the signed count the kernel wrote.
	return Event{
		PID:       binary.LittleEndian.Uint32(raw[0:offPPID]),
		PPID:      binary.LittleEndian.Uint32(raw[offPPID:offUID]),
		UID:       binary.LittleEndian.Uint32(raw[offUID:offRet]),
		Ret:       int32(binary.LittleEndian.Uint32(raw[offRet:offArgsCount])), //nolint:gosec // G115: reinterpret the wire's 32-bit word as the signed return value (0 or -errno) the kernel wrote.
		Comm:      cString(raw[offComm:offArgs]),
		Argv:      splitArgs(raw[offArgs : offArgs+int(argsSize)]),
		Truncated: argsCount >= maxArgs,
	}, nil
}

// splitArgs unpacks the NUL-separated argv buffer the BPF program built with
// bpf_probe_read_user_str (each string written with its terminator). Empty
// segments — including the trailing one after the final NUL — are dropped;
// a genuinely empty argv entry (rare) is lost, which is acceptable for a
// diagnostic trace.
func splitArgs(b []byte) []string {
	parts := bytes.Split(b, []byte{0})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}

// cString trims a fixed-width, NUL-terminated C char array to its Go string.
// A full-width field with no terminator keeps every byte.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// String renders one trace line. ret renders as "ok" on success or an errno
// name on failure, so a command-not-found exec reads at a glance.
func (e Event) String() string {
	trunc := ""
	if e.Truncated {
		trunc = " (truncated)"
	}
	return fmt.Sprintf("pid=%d ppid=%d uid=%d comm=%q ret=%s argv=[%s]%s",
		e.PID, e.PPID, e.UID, e.Comm, retString(e.Ret), strings.Join(e.Argv, " "), trunc)
}

// retString renders the execve(2) result: "ok" (ret 0, the image was
// replaced) or the failure errno by name, falling back to the raw number.
func retString(ret int32) string {
	if ret == 0 {
		return "ok"
	}
	if name := errnoName(-ret); name != "" {
		return name
	}
	return fmt.Sprintf("errno %d", -ret)
}

// errnoName maps the errnos execve(2) actually returns to their symbolic
// names. A switch (not a package-global table) keeps it allocation-free and
// avoids gochecknoglobals. Unnamed errnos return "" so retString falls back
// to the number.
func errnoName(errno int32) string {
	switch errno {
	case 1:
		return "EPERM"
	case 2:
		return "ENOENT"
	case 7:
		return "E2BIG"
	case 8:
		return "ENOEXEC"
	case 13:
		return "EACCES"
	case 20:
		return "ENOTDIR"
	case 26:
		return "ETXTBSY"
	case 36:
		return "ENAMETOOLONG"
	case 40:
		return "ELOOP"
	default:
		return ""
	}
}

// recordReader is the read side of the BPF ring buffer. *ringbuf.Reader
// satisfies it; tests substitute a fake so the whole drain loop is exercised
// without loading BPF.
type recordReader interface {
	Read() (ringbuf.Record, error)
	Close() error
}

// opener loads + attaches the gadget and returns a reader over its ring
// buffer plus a cleanup that detaches and unloads. openSourceFor binds the
// production implementation; tests pass a fake.
type opener func() (rd recordReader, cleanup func() error, err error)

// Run loads the exec-snoop gadget, attaches it to the execve enter+exit
// tracepoints, and writes one line per execve(2) to w until ctx is
// cancelled. Cancelling ctx is the normal stop and returns nil.
//
// selfPidns is the caller's own PID-namespace inode number: when non-zero
// the gadget drops every exec from a task in that namespace (the agent,
// guest init, kernel threads, crun) and reports only the container
// workload. Zero traces every exec.
func Run(ctx context.Context, w io.Writer, selfPidns uint32) error {
	return run(ctx, w, nil, openSourceFor(selfPidns))
}

// RunAggregating is Run that additionally folds every decoded event into agg,
// so the agent's summary writer can snapshot the live aggregate while the
// same drain still writes the raw trace to w. A nil agg behaves like Run.
func RunAggregating(ctx context.Context, w io.Writer, agg *Aggregator, selfPidns uint32) error {
	return run(ctx, w, agg, openSourceFor(selfPidns))
}

func run(ctx context.Context, w io.Writer, agg *Aggregator, open opener) error {
	rd, cleanup, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = cleanup() }()

	// ringbuf Read blocks; closing the reader unblocks it. Close on ctx
	// cancel so Run returns promptly. done guards against leaking the
	// watcher goroutine once the loop exits on its own.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = rd.Close()
		case <-done:
		}
	}()

	//daft:lint:disable=forloopbound reason="drains the ring buffer until the read fails — ctx cancel closes the reader (ringbuf.ErrClosed) or the writer errors; an open-ended tail of a live ring buffer is the intended behaviour"
	for {
		rec, err := rd.Read()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ringbuf.ErrClosed) {
				return nil
			}
			return fmt.Errorf("execsnoop: read ring buffer: %w", err)
		}
		ev, perr := parseEvent(rec.RawSample)
		if perr != nil {
			continue // a truncated sample is not worth failing the whole tail
		}
		if agg != nil {
			agg.Observe(ev)
		}
		if _, err := io.WriteString(w, ev.String()+"\n"); err != nil {
			return fmt.Errorf("execsnoop: write: %w", err)
		}
	}
}

// bpfDeps are the privileged BPF primitives openSourceWith composes: bump
// memlock, load the embedded objects, attach the enter+exit tracepoints,
// open the ring buffer. The real implementations need CAP_BPF; tests inject
// fakes to cover the wiring without loading BPF, mirroring opensnoop's seam.
type bpfDeps struct {
	memlock func() error
	// load loads + assigns the embedded objects, first writing selfPidns
	// into the program's .rodata container-workload filter (a no-op when
	// the .rodata variable is absent or selfPidns is 0).
	load      func(o *execsnoopObjects, selfPidns uint32) error
	attach    func(name string, prog *ebpf.Program) (detach func() error, err error)
	newReader func(*ebpf.Map) (recordReader, error)
}

// defaultBPFDeps wires the real cilium/ebpf calls. Its closure bodies are
// the only part of this file that needs CAP_BPF — they run in the guest
// under the e2e gadget test, not under make check.
func defaultBPFDeps() bpfDeps {
	return bpfDeps{
		memlock: rlimit.RemoveMemlock,
		load: func(o *execsnoopObjects, selfPidns uint32) error {
			spec, err := loadExecsnoop()
			if err != nil {
				return fmt.Errorf("execsnoop: load spec: %w", err)
			}
			// Write the container-workload filter into .rodata before load.
			// Guarded against a future C rename dropping the variable; Set is
			// a width-matched marshal (u32 → __u32) that can't fail here, so
			// its error is swallowed rather than added as an untestable
			// branch. Set(0) is harmless (the C default is already 0).
			if v := spec.Variables["self_pidns"]; v != nil {
				_ = v.Set(selfPidns)
			}
			return spec.LoadAndAssign(o, nil)
		},
		attach: func(name string, prog *ebpf.Program) (func() error, error) {
			l, err := link.Tracepoint("syscalls", name, prog, nil)
			if err != nil {
				return nil, err
			}
			return l.Close, nil
		},
		newReader: func(m *ebpf.Map) (recordReader, error) {
			r, err := ringbuf.NewReader(m)
			return r, err
		},
	}
}

// openSourceFor binds selfPidns into an opener so run's drain loop stays
// unaware of the filter (and the test fakes keep the no-arg opener shape).
func openSourceFor(selfPidns uint32) opener {
	return func() (recordReader, func() error, error) {
		return openSourceWith(defaultBPFDeps(), selfPidns)
	}
}

// openSourceWith composes the load path from injected primitives. The gadget
// needs both halves — enter stashes the argv, exit completes the record with
// the return value — so it attaches two tracepoints and unwinds in reverse
// on any later failure.
func openSourceWith(d bpfDeps, selfPidns uint32) (recordReader, func() error, error) {
	if err := d.memlock(); err != nil {
		return nil, nil, fmt.Errorf("execsnoop: remove memlock: %w", err)
	}
	var objs execsnoopObjects
	if err := d.load(&objs, selfPidns); err != nil {
		return nil, nil, fmt.Errorf("execsnoop: load objects: %w", err)
	}
	detachEnter, err := d.attach("sys_enter_execve", objs.HandleExecveEnter)
	if err != nil {
		_ = objs.Close()
		return nil, nil, fmt.Errorf("execsnoop: attach enter tracepoint: %w", err)
	}
	detachExit, err := d.attach("sys_exit_execve", objs.HandleExecveExit)
	if err != nil {
		_ = detachEnter()
		_ = objs.Close()
		return nil, nil, fmt.Errorf("execsnoop: attach exit tracepoint: %w", err)
	}
	rd, err := d.newReader(objs.Events)
	if err != nil {
		_ = detachExit()
		_ = detachEnter()
		_ = objs.Close()
		return nil, nil, fmt.Errorf("execsnoop: open ring buffer: %w", err)
	}
	cleanup := func() error {
		cerr := rd.Close()
		_ = detachExit()
		_ = detachEnter()
		_ = objs.Close()
		return cerr
	}
	return rd, cleanup, nil
}
