package agent

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
)

// Benchmark{PumpTapToConn,PumpConnToTap} measure the agent's
// frame pump in isolation: one MTU-sized frame in, the equivalent
// out the other side. The pump's per-iteration cost (one Read,
// the length-prefix copy, one Write) is what dominates wall-
// clock time.
//
// Wired with net.Pipe rather than a real tun/vsock pair so each
// iteration is a full synchronous hand-off — exactly the property
// we want for a per-frame measurement without producer/consumer
// queueing skew. b.SetBytes(frameSize) makes go test report
// bytes/sec on the axis the sandboxnet bun-test investigation
// reasoned about; anything materially below the upstream wire
// speed surfaces here first.

func BenchmarkPumpTapToConn(b *testing.B) {
	for _, size := range []int{64, 256, 1500} {
		b.Run(fmt.Sprintf("frame=%d", size), func(b *testing.B) {
			benchmarkPump(b, size, tapToConn)
		})
	}
}

func BenchmarkPumpConnToTap(b *testing.B) {
	for _, size := range []int{64, 256, 1500} {
		b.Run(fmt.Sprintf("frame=%d", size), func(b *testing.B) {
			benchmarkPump(b, size, connToTap)
		})
	}
}

// pumpDirection picks which half of pumpFrames the benchmark
// exercises. Producer writes raw frames or StdioProtocol-framed
// frames depending on direction; drainer reads back the other
// shape. Keeping both directions in a single helper guarantees
// they stay symmetric — if one side ever grows a buffer or an
// extra syscall the other doesn't, the diff is one line.
type pumpDirection int

const (
	tapToConn pumpDirection = iota
	connToTap
)

func benchmarkPump(b *testing.B, frameSize int, dir pumpDirection) {
	b.Helper()
	connHost, connAgent := net.Pipe()
	tapHost, tapAgent := net.Pipe()
	b.Cleanup(func() {
		_ = connHost.Close()
		_ = connAgent.Close()
		_ = tapHost.Close()
		_ = tapAgent.Close()
	})

	var (
		producer io.Writer
		drainer  io.Reader
		input    []byte
		outSize  int
	)
	switch dir {
	case tapToConn:
		go pumpTapToConn(tapAgent, connAgent)
		producer = tapHost
		drainer = connHost
		input = make([]byte, frameSize)
		outSize = 2 + frameSize
	case connToTap:
		go pumpConnToTap(connAgent, tapAgent)
		producer = connHost
		drainer = tapHost
		framed := make([]byte, 2+frameSize)
		binary.LittleEndian.PutUint16(framed[:2], uint16(frameSize)) //nolint:gosec // frameSize ≤ 1500.
		input = framed
		outSize = frameSize
	}

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		buf := make([]byte, outSize)
		//vmjail:lint:disable=forloopbound reason="bounded by b.N; exits on Close-driven read error"
		for i := 0; i < b.N; i++ {
			if _, err := io.ReadFull(drainer, buf); err != nil {
				return
			}
		}
	}()

	b.SetBytes(int64(frameSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := producer.Write(input); err != nil {
			b.Fatalf("producer.Write iter %d: %v", i, err)
		}
	}
	b.StopTimer()

	// Close pipes to unblock the drainer; wait so the benchmark
	// doesn't leak the goroutine across runs (go test reuses
	// the process across sub-benchmarks).
	_ = tapHost.Close()
	_ = connHost.Close()
	<-drainDone
}
