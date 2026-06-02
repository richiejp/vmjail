//go:build linux

package opensnoop

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
// opensnoop_bpf.c: pid+uid+dfd+flags (4×4) + comm[16] + filename[255],
// padded to a 4-byte boundary. A drift guard in the tests asserts this
// matches binary.Size(opensnoopEvent{}) so a change to the C struct that
// regenerates the loader can't silently desync the decoder.
const recordSize = 16 + 16 + 255 + 1

// Field offsets within a record.
const (
	offUID      = 4
	offDFD      = 8
	offFlags    = 12
	offComm     = 16
	offFilename = 32
	endFilename = offFilename + 255
)

// parseEvent decodes one ring-buffer record — the BPF `struct event`
// from opensnoop_bpf.c, little-endian — into an Event. A short record is
// rejected rather than mis-decoded. Pure: no BPF, unit-tested. Decoding
// by explicit offset (rather than binary.Read into opensnoopEvent) keeps
// the fast path allocation-free and drops an unreachable error branch.
func parseEvent(raw []byte) (Event, error) {
	if len(raw) < recordSize {
		return Event{}, fmt.Errorf("opensnoop: short record: %d < %d bytes", len(raw), recordSize)
	}
	return Event{
		PID:      binary.LittleEndian.Uint32(raw[0:offUID]),
		UID:      binary.LittleEndian.Uint32(raw[offUID:offDFD]),
		DFD:      int32(binary.LittleEndian.Uint32(raw[offDFD:offFlags])),  //nolint:gosec // G115: reinterpret the wire's 32-bit word as the signed dfd the kernel wrote; the bit pattern is intentional.
		Flags:    int32(binary.LittleEndian.Uint32(raw[offFlags:offComm])), //nolint:gosec // G115: reinterpret the wire's 32-bit word as the signed flags the kernel wrote; the bit pattern is intentional.
		Comm:     cString(raw[offComm:offFilename]),
		Filename: cString(raw[offFilename:endFilename]),
	}, nil
}

// cString trims a fixed-width, NUL-terminated C char array to its Go
// string. A full-width field with no terminator keeps every byte.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// openFlag renders the access mode + the option bits we care about for a
// readable trace line. Unknown bits are dropped — this is a diagnostic
// rendering, not a faithful reconstruction of the raw flags (which
// Event.Flags still carries verbatim). The x86 octal constants are
// inlined rather than tabled to avoid a package global (gochecknoglobals)
// and the per-event slice allocation a table lookup would cost.
func openFlag(flags int32) string {
	var parts []string
	switch flags & 0x3 { // O_ACCMODE
	case 0x0:
		parts = append(parts, "O_RDONLY")
	case 0x1:
		parts = append(parts, "O_WRONLY")
	case 0x2:
		parts = append(parts, "O_RDWR")
	}
	if flags&0x40 != 0 {
		parts = append(parts, "O_CREAT")
	}
	if flags&0x80 != 0 {
		parts = append(parts, "O_EXCL")
	}
	if flags&0x200 != 0 {
		parts = append(parts, "O_TRUNC")
	}
	if flags&0x400 != 0 {
		parts = append(parts, "O_APPEND")
	}
	if flags&0x800 != 0 {
		parts = append(parts, "O_NONBLOCK")
	}
	if flags&0x10000 != 0 {
		parts = append(parts, "O_DIRECTORY")
	}
	if flags&0x80000 != 0 {
		parts = append(parts, "O_CLOEXEC")
	}
	return strings.Join(parts, "|")
}

// String renders one trace line. The raw Flags value is kept alongside
// the symbolic rendering so an unusual bit we don't name is still
// visible in post-mortem.
func (e Event) String() string {
	return fmt.Sprintf("pid=%d uid=%d comm=%q dfd=%d flags=%s(%#x) path=%q",
		e.PID, e.UID, e.Comm, e.DFD, openFlag(e.Flags), e.Flags, e.Filename)
}

// recordReader is the read side of the BPF ring buffer. *ringbuf.Reader
// satisfies it; tests substitute a fake so the whole drain loop is
// exercised without loading BPF.
type recordReader interface {
	Read() (ringbuf.Record, error)
	Close() error
}

// opener loads + attaches the gadget and returns a reader over its ring
// buffer plus a cleanup that detaches and unloads. openSource is the
// production implementation; tests pass a fake.
type opener func() (rd recordReader, cleanup func() error, err error)

// Run loads the open-snoop gadget, attaches it to the openat tracepoint,
// and writes one line per openat(2) to w until ctx is cancelled. It is
// the entry point daft-agent calls in the guest. Cancelling ctx is the
// normal stop and returns nil.
//
// selfPidns is the caller's own PID-namespace inode number: when non-zero
// the gadget drops every openat from a task in that namespace (the agent,
// guest init, kernel threads, crun) and reports only the container
// workload, which runs in a child PID namespace. Zero traces every openat.
func Run(ctx context.Context, w io.Writer, selfPidns uint32) error {
	return run(ctx, w, nil, openSourceFor(selfPidns))
}

// RunAggregating is Run that additionally folds every decoded event into
// agg, so the agent's summary writer can snapshot the live aggregate while
// the same drain still writes the raw trace to w. A nil agg behaves like Run.
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
			return fmt.Errorf("opensnoop: read ring buffer: %w", err)
		}
		ev, perr := parseEvent(rec.RawSample)
		if perr != nil {
			continue // a truncated sample is not worth failing the whole tail
		}
		if agg != nil {
			agg.Observe(ev)
		}
		if _, err := io.WriteString(w, ev.String()+"\n"); err != nil {
			return fmt.Errorf("opensnoop: write: %w", err)
		}
	}
}

// bpfDeps are the privileged BPF primitives openSourceWith composes:
// bump memlock, load the embedded objects, attach the tracepoint, open
// the ring buffer. The real implementations need CAP_BPF on a
// BPF-capable kernel; tests inject fakes to cover the load/attach/cleanup
// wiring without loading BPF, mirroring internal/agent/tap_linux.go's
// dependency seam. attach returns a plain closer rather than a link.Link
// so the success path is fakeable (link.Link has an unexported method and
// can't be implemented outside cilium/ebpf).
type bpfDeps struct {
	memlock func() error
	// load loads + assigns the embedded objects, first writing selfPidns
	// into the program's .rodata container-workload filter (a no-op when
	// the .rodata variable is absent or selfPidns is 0).
	load      func(o *opensnoopObjects, selfPidns uint32) error
	attach    func(prog *ebpf.Program) (detach func() error, err error)
	newReader func(*ebpf.Map) (recordReader, error)
}

// defaultBPFDeps wires the real cilium/ebpf calls. Its closure bodies are
// the only part of this file that needs CAP_BPF — they run in the guest
// under the e2e gadget test, not under make check.
func defaultBPFDeps() bpfDeps {
	return bpfDeps{
		memlock: rlimit.RemoveMemlock,
		load: func(o *opensnoopObjects, selfPidns uint32) error {
			spec, err := loadOpensnoop()
			if err != nil {
				return fmt.Errorf("opensnoop: load spec: %w", err)
			}
			// Write the container-workload filter into .rodata before load.
			// Guarded against a future C rename dropping the variable; Set
			// is a width-matched marshal (u32 → __u32) that can't fail here,
			// so its error is swallowed rather than added as a branch no
			// unprivileged test could ever take. Set(0) is harmless — the C
			// default is already 0 (filter off).
			if v := spec.Variables["self_pidns"]; v != nil {
				_ = v.Set(selfPidns)
			}
			return spec.LoadAndAssign(o, nil)
		},
		attach: func(prog *ebpf.Program) (func() error, error) {
			l, err := link.Tracepoint("syscalls", "sys_enter_openat", prog, nil)
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

// openSourceWith composes the load path from injected primitives. On any
// failure it unwinds the steps already taken; on success it returns the
// ring-buffer reader plus a cleanup that closes the reader, detaches the
// tracepoint, and unloads the objects. selfPidns is the container-workload
// filter handed to the load step (see bpfDeps.load).
func openSourceWith(d bpfDeps, selfPidns uint32) (recordReader, func() error, error) {
	if err := d.memlock(); err != nil {
		return nil, nil, fmt.Errorf("opensnoop: remove memlock: %w", err)
	}
	var objs opensnoopObjects
	if err := d.load(&objs, selfPidns); err != nil {
		return nil, nil, fmt.Errorf("opensnoop: load objects: %w", err)
	}
	detach, err := d.attach(objs.HandleOpenat)
	if err != nil {
		_ = objs.Close()
		return nil, nil, fmt.Errorf("opensnoop: attach tracepoint: %w", err)
	}
	rd, err := d.newReader(objs.Events)
	if err != nil {
		_ = detach()
		_ = objs.Close()
		return nil, nil, fmt.Errorf("opensnoop: open ring buffer: %w", err)
	}
	cleanup := func() error {
		cerr := rd.Close()
		_ = detach()
		_ = objs.Close()
		return cerr
	}
	return rd, cleanup, nil
}
