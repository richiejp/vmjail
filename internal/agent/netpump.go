package agent

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"

	"github.com/richiejp/vmjail/internal/oops"
)

// maxNetFrameSize caps a single Ethernet frame on the wire. 1600
// covers MTU 1500 plus header slack; anything bigger must be a
// framing corruption and we abort the pump rather than allocate.
// Mirrors sandboxnettest.MaxFrameSize on the host side.
const maxNetFrameSize = 1600

// pumpFrames bridges conn (StdioProtocol-framed Ethernet) and tap
// (raw Ethernet packets, one per Read/Write) until ctx ends or
// either side errors. Returns after both pump goroutines have
// exited; both conn and tap are closed before return so the
// caller can rely on it as the teardown boundary.
func pumpFrames(ctx context.Context, conn, tap io.ReadWriteCloser) {
	var closeOnce sync.Once
	stop := func() {
		closeOnce.Do(func() {
			oops.Skip("agent: close net conn", conn.Close(), net.ErrClosed, io.ErrClosedPipe)
			oops.Skip("agent: close tap", tap.Close(), net.ErrClosed, io.ErrClosedPipe)
		})
	}
	defer stop()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		<-ctx.Done()
		stop()
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer cancel()
		pumpConnToTap(conn, tap)
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		pumpTapToConn(tap, conn)
	}()
	wg.Wait()
}

// pumpConnToTap reads u16-le-framed Ethernet from conn and writes
// each raw frame to tap. Returns on any read/write error — the
// caller closes both ends.
func pumpConnToTap(conn io.Reader, tap io.Writer) {
	hdr := make([]byte, 2)
	buf := make([]byte, maxNetFrameSize)
	//vmjail:lint:disable=forloopbound reason="bounded by conn.Close in pumpFrames"
	for {
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		size := binary.LittleEndian.Uint16(hdr)
		if size == 0 || size > maxNetFrameSize {
			return
		}
		if _, err := io.ReadFull(conn, buf[:size]); err != nil {
			return
		}
		if _, err := tap.Write(buf[:size]); err != nil {
			return
		}
	}
}

// pumpTapToConn reads one packet at a time from tap and writes a
// u16-le size prefix + frame to conn.
func pumpTapToConn(tap io.Reader, conn io.Writer) {
	buf := make([]byte, 2+maxNetFrameSize)
	//vmjail:lint:disable=forloopbound reason="bounded by tap.Close in pumpFrames"
	for {
		n, err := tap.Read(buf[2:])
		if err != nil {
			return
		}
		if n <= 0 || n > maxNetFrameSize {
			return
		}
		binary.LittleEndian.PutUint16(buf[:2], uint16(n)) //nolint:gosec // n is bounded above by maxNetFrameSize.
		if _, err := conn.Write(buf[:2+n]); err != nil {
			return
		}
	}
}
