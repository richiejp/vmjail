package imagestore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Test-package constants reused across files so goconst doesn't fire
// on repeated string literals; bundled here because each file uses
// only one or two and pulling them into one place keeps the test
// fixtures in one navigable spot.
const (
	testEntrypoint = "/hello"
	testTraversed  = "/var/tmp/bundle/etc/passwd"
)

// tarGzWriter is a tiny helper that produces a tar.gz blob with
// arbitrary file entries. Used to assemble synthetic image layers
// where we want to assert specific files are unpacked.
type tarGzWriter struct {
	buf *bytes.Buffer
	gz  *gzip.Writer
	tw  *tar.Writer
}

func newTarGzWriter(buf *bytes.Buffer) *tarGzWriter {
	gz := gzip.NewWriter(buf)
	return &tarGzWriter{
		buf: buf,
		gz:  gz,
		tw:  tar.NewWriter(gz),
	}
}

func (w *tarGzWriter) addFile(name, body string, mode int64) {
	hdr := &tar.Header{
		Name:    name,
		Mode:    mode,
		Size:    int64(len(body)),
		ModTime: time.Unix(0, 0),
	}
	_ = w.tw.WriteHeader(hdr)
	_, _ = w.tw.Write([]byte(body))
}

func (w *tarGzWriter) close() {
	_ = w.tw.Close()
	_ = w.gz.Close()
}

// tarballLayerFromBytes wraps a tar.gz byte slice as a v1.Layer that
// crane.Push will accept and the registry will store as-is.
func tarballLayerFromBytes(b []byte) (v1.Layer, error) {
	return tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	})
}
