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
)

const DefaultVsockPort uint32 = 9200

type Request struct {
	Directory string `json:"directory"`
}

func Serve(ctx context.Context, listener net.Listener, roots map[string]string) error {
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
			_ = serveOne(conn, roots)
		}()
	}
}

func serveOne(conn net.Conn, roots map[string]string) error {
	var request Request
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&request); err != nil {
		return err
	}
	root, ok := roots[request.Directory]
	if !ok {
		return fmt.Errorf("unknown workspace directory %q", request.Directory)
	}
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
