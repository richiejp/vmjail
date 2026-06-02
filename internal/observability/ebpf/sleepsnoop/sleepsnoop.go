//go:build linux

package sleepsnoop

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// recordSize is the wire size of the BPF `struct event` from sleepsnoop_bpf.c:
// req_ns (8) + pid (4) + comm[16] = 28, padded to 32 by the struct's 8-byte
// alignment. A drift guard in the tests asserts this matches
// binary.Size(sleepsnoopEvent{}) so a C-struct change that regenerates the
// loader can't silently desync the decoder.
const recordSize = 32

// Field offsets within a record. req_ns leads (offset 0) so its __u64 is
// aligned; pid and comm follow.
const (
	offPID  = 8
	offComm = 12
	commLen = 16
)

// parseEvent decodes one ring-buffer record — the BPF `struct event` from
// sleepsnoop_bpf.c, little-endian — into an Event. A short record is rejected
// rather than mis-decoded. Pure: no BPF, unit-tested.
func parseEvent(raw []byte) (Event, error) {
	if len(raw) < recordSize {
		return Event{}, fmt.Errorf("sleepsnoop: short record: %d < %d bytes", len(raw), recordSize)
	}
	return Event{
		ReqNs: binary.LittleEndian.Uint64(raw[0:offPID]),
		PID:   binary.LittleEndian.Uint32(raw[offPID:offComm]),
		Comm:  cString(raw[offComm : offComm+commLen]),
	}, nil
}

// cString trims a fixed-width, NUL-terminated C char array to its Go string.
// A full-width field with no terminator keeps every byte.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// String renders one trace line. The duration renders human-readably ("100ms",
// "2s"); a 0 (an absolute-deadline clock_nanosleep, whose duration the gadget
// does not record) shows as "0s".
func (e Event) String() string {
	return fmt.Sprintf("pid=%d comm=%q dur=%s", e.PID, e.Comm, durString(e.ReqNs))
}

// durString renders a requested-sleep nanosecond count as a Go duration. An
// absurd value beyond time.Duration's int64 range (never a real sleep) falls
// back to a raw ns count rather than overflowing to a negative duration.
func durString(ns uint64) string {
	if ns > uint64(^uint64(0)>>1) {
		return fmt.Sprintf("%dns", ns)
	}
	return time.Duration(ns).String() //nolint:gosec // G115: guarded above against the int64 range.
}

// recordReader is the read side of the BPF ring buffer. *ringbuf.Reader
// satisfies it; tests substitute a fake so the whole drain loop is exercised
// without loading BPF.
type recordReader interface {
	Read() (ringbuf.Record, error)
	Close() error
}

// opener loads + attaches the gadget and returns a reader over its ring buffer
// plus a cleanup that detaches and unloads. openSourceFor binds the production
// implementation; tests pass a fake.
type opener func() (rd recordReader, cleanup func() error, err error)

// Run loads the sleep-snoop gadget, attaches it to the nanosleep +
// clock_nanosleep enter tracepoints, and writes one line per sleep to w until
// ctx is cancelled. Cancelling ctx is the normal stop and returns nil.
//
// selfPidns is the caller's own PID-namespace inode number: when non-zero the
// gadget drops every sleep from a task in that namespace (the agent, guest
// init, kernel threads, crun) and reports only the container workload. Zero
// traces every sleep.
func Run(ctx context.Context, w io.Writer, selfPidns uint32) error {
	return run(ctx, w, nil, openSourceFor(selfPidns))
}

// RunAggregating is Run that additionally folds every decoded event into agg,
// so the agent's summary writer can snapshot the live aggregate while the same
// drain still writes the raw trace to w. A nil agg behaves like Run.
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
	// cancel so Run returns promptly. done guards against leaking the watcher
	// goroutine once the loop exits on its own.
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
			return fmt.Errorf("sleepsnoop: read ring buffer: %w", err)
		}
		ev, perr := parseEvent(rec.RawSample)
		if perr != nil {
			continue // a truncated sample is not worth failing the whole tail
		}
		if agg != nil {
			agg.Observe(ev)
		}
		if _, err := io.WriteString(w, ev.String()+"\n"); err != nil {
			return fmt.Errorf("sleepsnoop: write: %w", err)
		}
	}
}

// bpfDeps are the privileged BPF primitives openSourceWith composes: bump
// memlock, load the embedded objects, attach the two enter tracepoints, open
// the ring buffer. The real implementations need CAP_BPF; tests inject fakes
// to cover the wiring without loading BPF, mirroring the other gadgets' seam.
type bpfDeps struct {
	memlock func() error
	// load loads + assigns the embedded objects, first writing selfPidns into
	// the program's .rodata container-workload filter (a no-op when the
	// .rodata variable is absent or selfPidns is 0).
	load      func(o *sleepsnoopObjects, selfPidns uint32) error
	attach    func(name string, prog *ebpf.Program) (detach func() error, err error)
	newReader func(*ebpf.Map) (recordReader, error)
}

// defaultBPFDeps wires the real cilium/ebpf calls. Its closure bodies are the
// only part of this file that needs CAP_BPF — they run in the guest under the
// e2e gadget test, not under make check.
func defaultBPFDeps() bpfDeps {
	return bpfDeps{
		memlock: rlimit.RemoveMemlock,
		load: func(o *sleepsnoopObjects, selfPidns uint32) error {
			spec, err := loadSleepsnoop()
			if err != nil {
				return fmt.Errorf("sleepsnoop: load spec: %w", err)
			}
			// Write the container-workload filter into .rodata before load.
			// Guarded against a future C rename dropping the variable; Set is a
			// width-matched marshal (u32 → __u32) that can't fail here, so its
			// error is swallowed rather than added as an untestable branch.
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
// attaches both sleep syscalls' enter tracepoints (nanosleep and
// clock_nanosleep) and unwinds in reverse on any later failure.
func openSourceWith(d bpfDeps, selfPidns uint32) (recordReader, func() error, error) {
	if err := d.memlock(); err != nil {
		return nil, nil, fmt.Errorf("sleepsnoop: remove memlock: %w", err)
	}
	var objs sleepsnoopObjects
	if err := d.load(&objs, selfPidns); err != nil {
		return nil, nil, fmt.Errorf("sleepsnoop: load objects: %w", err)
	}
	detachNano, err := d.attach("sys_enter_nanosleep", objs.HandleNanosleepEnter)
	if err != nil {
		_ = objs.Close()
		return nil, nil, fmt.Errorf("sleepsnoop: attach nanosleep tracepoint: %w", err)
	}
	detachClock, err := d.attach("sys_enter_clock_nanosleep", objs.HandleClockNanosleepEnter)
	if err != nil {
		_ = detachNano()
		_ = objs.Close()
		return nil, nil, fmt.Errorf("sleepsnoop: attach clock_nanosleep tracepoint: %w", err)
	}
	rd, err := d.newReader(objs.Events)
	if err != nil {
		_ = detachClock()
		_ = detachNano()
		_ = objs.Close()
		return nil, nil, fmt.Errorf("sleepsnoop: open ring buffer: %w", err)
	}
	cleanup := func() error {
		cerr := rd.Close()
		_ = detachClock()
		_ = detachNano()
		_ = objs.Close()
		return cerr
	}
	return rd, cleanup, nil
}
