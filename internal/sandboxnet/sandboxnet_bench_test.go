package sandboxnet_test

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/richiejp/vmjail/internal/sandboxnet"
	"github.com/richiejp/vmjail/internal/sandboxnet/sandboxnettest"
)

// BenchmarkAcceptARPRoundTrip measures the gvisor-tap-vsock
// switch + ARP-responder path in isolation: one ARP request in,
// one ARP reply out, per iteration. The bench exists to localize
// the bun-test throughput floor (≈50 KB/s through the netstack)
// against the per-frame service time of the bottom of the stack,
// before any TCP/NAT cost. The frame pump itself was already
// benched at ~720 MB/s in internal/agent (BenchmarkPump*) so the
// stdio substrate is not under suspicion here — only the netstack
// switch + IP layer is.
//
// Wired with net.Pipe matching TestAcceptRoundTripsARP. Drainer
// goroutine pulls ARP replies so the netstack's tx side doesn't
// backpressure into the rx loop and skew the rate.
//
// b.SetBytes is the ARP frame size (42), so go test reports the
// metric on the same axis as the pump bench. ARP replies are
// matched in size, so the round-trip processes 2×42=84 bytes per
// iter; the reported MB/s is "request bytes/sec", which is the
// quantity the netstack rx loop services.
func BenchmarkAcceptARPRoundTrip(b *testing.B) {
	n, err := sandboxnet.New(sandboxnet.Config{})
	if err != nil {
		b.Fatalf("New: %v", err)
	}

	hostSide, guestSide := net.Pipe()
	b.Cleanup(func() {
		_ = hostSide.Close()
		_ = guestSide.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		_ = n.Accept(ctx, hostSide)
	}()

	// net.Pipe is synchronous: a stalled drainer would freeze the
	// switch tx loop and then the rx loop too. Drain in lockstep.
	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		hdr := make([]byte, 2)
		body := make([]byte, sandboxnettest.MaxFrameSize)
		//vmjail:lint:disable=forloopbound reason="exits on guestSide close from b.Cleanup; pump for b.N iterations is upstream"
		for {
			if _, rerr := io.ReadFull(guestSide, hdr); rerr != nil {
				return
			}
			size := binary.LittleEndian.Uint16(hdr)
			if size == 0 || int(size) > len(body) {
				return
			}
			if _, rerr := io.ReadFull(guestSide, body[:size]); rerr != nil {
				return
			}
		}
	}()

	guestMAC := []byte{0x02, 0, 0, 0, 0, 0x01}
	guestIP := []byte{192, 168, 127, 2}
	gatewayIP := net.ParseIP(sandboxnet.DefaultGatewayIP).To4()
	frame := sandboxnettest.ARPRequest(guestMAC, guestIP, gatewayIP)

	framed := make([]byte, 2+len(frame))
	binary.LittleEndian.PutUint16(framed[:2], uint16(len(frame))) //nolint:gosec // ARP frame ≤ 42 bytes.
	copy(framed[2:], frame)

	b.SetBytes(int64(len(frame)))
	for b.Loop() {
		if _, werr := guestSide.Write(framed); werr != nil {
			b.Fatalf("Write: %v", werr)
		}
	}

	cancel()
	_ = hostSide.Close()
	_ = guestSide.Close()
	<-drainerDone
	<-acceptDone
}
