package workspacesnapshot

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultVsockPort      uint32 = 9300
	controlRequestTimeout        = 10 * time.Second
	guestGrowTimeout             = 4 * time.Second
)

const (
	operationUsage = "usage"
	operationGrow  = "grow"
)

type Request struct {
	Operation string `json:"operation,omitempty"`
	Directory string `json:"directory"`
}

// Usage is the guest filesystem accounting returned to the Node. Available is
// included so callers can distinguish reserved filesystem space from data in
// use rather than assuming Used == Total-Available on every filesystem.
type Usage struct {
	Total     uint64 `json:"total_bytes"`
	Used      uint64 `json:"used_bytes"`
	Available uint64 `json:"available_bytes"`
}

// Control provides the guest-local XFS operations exposed beside snapshots.
// Nil functions make that operation unavailable without weakening snapshot
// service behavior.
type Control struct {
	Usage func() (Usage, error)
	Grow  func(context.Context) error
}

type response struct {
	Usage *Usage `json:"usage,omitempty"`
	Error string `json:"error,omitempty"`
}

func Serve(ctx context.Context, listener net.Listener, roots map[string]string, control ...Control) error {
	var ctl Control
	if len(control) > 0 {
		ctl = control[0]
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	//daft:lint:disable=forloopbound reason="bounded by listener closure when the guest context ends"
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			return err
		}
		go func() {
			defer func() { _ = conn.Close() }()
			_ = serveOne(ctx, conn, roots, ctl)
		}()
	}
}

func serveOne(ctx context.Context, conn net.Conn, roots map[string]string, control Control) error {
	var request Request
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&request); err != nil {
		return err
	}
	if request.Operation != "" {
		return serveControl(ctx, conn, request.Operation, control)
	}
	root, ok := roots[request.Directory]
	if !ok {
		return fmt.Errorf("unknown workspace directory %q", request.Directory)
	}
	return serveSnapshot(conn, root)
}

func serveControl(ctx context.Context, conn net.Conn, operation string, control Control) error {
	switch operation {
	case operationUsage:
		if control.Usage == nil {
			return json.NewEncoder(conn).Encode(response{Error: "workspace usage unavailable"})
		}
		usage, err := control.Usage()
		if err != nil {
			return json.NewEncoder(conn).Encode(response{Error: err.Error()})
		}
		return json.NewEncoder(conn).Encode(response{Usage: &usage})
	case operationGrow:
		if control.Grow == nil {
			return json.NewEncoder(conn).Encode(response{Error: "workspace grow unavailable"})
		}
		growCtx, cancel := context.WithTimeout(ctx, guestGrowTimeout)
		defer cancel()
		if err := control.Grow(growCtx); err != nil {
			return json.NewEncoder(conn).Encode(response{Error: err.Error()})
		}
		return json.NewEncoder(conn).Encode(response{})
	case "":
	default:
		return json.NewEncoder(conn).Encode(response{Error: "unknown workspace operation"})
	}
	return nil
}

func serveSnapshot(conn net.Conn, root string) error {
	tw := tar.NewWriter(conn)
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = rootHandle.Close() }()
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := rootHandle.Open(rel)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// GetUsage reads bounded filesystem accounting from the guest service.
func GetUsage(ctx context.Context, dial func(context.Context) (net.Conn, error)) (Usage, error) {
	var result Usage
	err := controlRequest(ctx, dial, Request{Operation: operationUsage}, func(resp response) error {
		if resp.Usage == nil {
			return errors.New("workspace snapshot: usage response is empty")
		}
		result = *resp.Usage
		return nil
	})
	return result, err
}

// Grow asks the guest to expand its mounted XFS filesystem to the block
// device's current size.
func Grow(ctx context.Context, dial func(context.Context) (net.Conn, error)) error {
	return controlRequest(ctx, dial, Request{Operation: operationGrow}, nil)
}

func controlRequest(ctx context.Context, dial func(context.Context) (net.Conn, error), request Request, consume func(response) error) error {
	conn, err := dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(controlRequestTimeout)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return err
	}
	var resp response
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if consume != nil {
		return consume(resp)
	}
	return nil
}

func Pull(ctx context.Context, dial func(context.Context) (net.Conn, error), directory, destination string) error {
	conn, err := dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := json.NewEncoder(conn).Encode(Request{Directory: directory}); err != nil {
		return err
	}
	return extract(tar.NewReader(bufio.NewReader(conn)), destination)
}

// ReceiveSeed extracts a host-initiated mount archive from a dedicated vsock
// connection. Unlike Pull it sends no request: the per-VM, per-mount socket is
// already bound to exactly one durable manifest.
func ReceiveSeed(ctx context.Context, dial func(context.Context) (net.Conn, error), destination string) error {
	conn, err := dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return extract(tar.NewReader(bufio.NewReader(conn)), destination)
}

func extract(tr *tar.Reader, destination string) error {
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return err
	}
	//daft:lint:disable=forloopbound reason="bounded by the tar end-of-archive marker or a connection read error"
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe snapshot path %q", header.Name)
		}
		path := filepath.Join(destination, clean)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			mode := os.FileMode(uint32(header.Mode & 0o777))
			if err := os.MkdirAll(path, mode); err != nil {
				return err
			}
		case tar.TypeReg:
			mode := os.FileMode(uint32(header.Mode & 0o777))
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) // #nosec G304,G306 -- confined destination and archived mode.
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, tr, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if err := os.Symlink(header.Linkname, path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported snapshot entry %q type %d", header.Name, header.Typeflag)
		}
	}
}
