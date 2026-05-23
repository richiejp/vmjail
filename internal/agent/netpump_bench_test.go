package agent

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
)

// BenchmarkPumpTapToConn measures the tap→conn half of the agent's
// frame pump in isolation: one MTU-sized raw Ethernet frame goes in
// the tap side, the StdioProtocol-framed equivalent comes out the
// conn side. The pump's per-iteration cost (one Read syscall, the
// length-prefix copy, one Write syscall) dominates wall-clock time.
//
// Wired with `net.Pipe()` rather than a real tun/vsock pair: both
// pipe ends are synchronous, so each iteration is a full
// hand-off — exactly the property we want for a per-frame
// measurement without producer / consumer queueing skew.
//
// `b.SetBytes(framePayloadBenchSize)` makes `go test -bench`
// report bytes/sec on the same axis the bun-test investigation
// reasoned about (≈50 KB/s observed end-to-end through 30k+
// round-trips). Anything materially below the upstream Cloud
// Hypervisor + virtio-net wire speed surfaces here first.
func BenchmarkPumpTapToConn(b *testing.B) {
	for _, size := range []int{64, 256, 1500} {
		b.Run(formatFrameSize(size), func(b *testing.B) {
			benchmarkPumpTapToConn(b, size)
		})
	}
}

// BenchmarkPumpConnToTap covers the opposite direction:
// StdioProtocol-framed bytes in, raw Ethernet frame out.
// Symmetric numbers with PumpTapToConn say the pump itself is
// balanced; an asymmetry would point at one direction's framing
// code as the suspect.
func BenchmarkPumpConnToTap(b *testing.B) {
	for _, size := range []int{64, 256, 1500} {
		b.Run(formatFrameSize(size), func(b *testing.B) {
			benchmarkPumpConnToTap(b, size)
		})
	}
}

func benchmarkPumpTapToConn(b *testing.B, frameSize int) {
	b.Helper()
	connHost, connAgent := net.Pipe()
	tapHost, tapAgent := net.Pipe()
	b.Cleanup(func() {
		_ = connHost.Close()
		_ = connAgent.Close()
		_ = tapHost.Close()
		_ = tapAgent.Close()
	})

	go pumpTapToConn(tapAgent, connAgent)

	// Drain side: read N framed packets off conn. Errors here
	// after b.N iterations are expected (we close pipes in
	// b.Cleanup); we exit on any read failure rather than failing
	// the benchmark.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		hdr := make([]byte, 2)
		buf := make([]byte, frameSize)
		//daft:lint:disable=forloopbound reason="bounded by b.N; exits on Close-driven read error"
		for i := 0; i < b.N; i++ {
			if _, err := io.ReadFull(connHost, hdr); err != nil {
				return
			}
			size := binary.LittleEndian.Uint16(hdr)
			if int(size) != frameSize {
				return
			}
			if _, err := io.ReadFull(connHost, buf[:size]); err != nil {
				return
			}
		}
	}()

	frame := make([]byte, frameSize)
	b.SetBytes(int64(frameSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tapHost.Write(frame); err != nil {
			b.Fatalf("tapHost.Write iter %d: %v", i, err)
		}
	}
	b.StopTimer()

	// Close to unblock the drainer; wait so the benchmark doesn't
	// leak the goroutine across runs (go test reuses process across
	// benches in the same package).
	_ = tapHost.Close()
	_ = connHost.Close()
	<-drainDone
}

func benchmarkPumpConnToTap(b *testing.B, frameSize int) {
	b.Helper()
	connHost, connAgent := net.Pipe()
	tapHost, tapAgent := net.Pipe()
	b.Cleanup(func() {
		_ = connHost.Close()
		_ = connAgent.Close()
		_ = tapHost.Close()
		_ = tapAgent.Close()
	})

	go pumpConnToTap(connAgent, tapAgent)

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		buf := make([]byte, frameSize)
		//daft:lint:disable=forloopbound reason="bounded by b.N; exits on Close-driven read error"
		for i := 0; i < b.N; i++ {
			if _, err := io.ReadFull(tapHost, buf); err != nil {
				return
			}
		}
	}()

	frame := make([]byte, 2+frameSize)
	binary.LittleEndian.PutUint16(frame[:2], uint16(frameSize)) //nolint:gosec // frameSize ≤ 1500, fits u16.
	b.SetBytes(int64(frameSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := connHost.Write(frame); err != nil {
			b.Fatalf("connHost.Write iter %d: %v", i, err)
		}
	}
	b.StopTimer()

	_ = tapHost.Close()
	_ = connHost.Close()
	<-drainDone
}

// formatFrameSize renders the benchmark subtest name so the
// b.Run output is greppable: BenchmarkPumpTapToConn/frame=1500-N.
func formatFrameSize(n int) string {
	switch n {
	case 64:
		return "frame=64"
	case 256:
		return "frame=256"
	case 1500:
		return "frame=1500"
	}
	return "frame=other"
}
