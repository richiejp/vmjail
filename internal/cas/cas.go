// Package cas implements the content-addressed chunk store that
// backs workspace sync. Chunks are addressed by the sha256 digest
// of their *uncompressed* payload and stored compressed (zstd) on
// disk under <root>/sha256/<aa>/<bb>/<full>. The two-level fan-out
// keeps any one directory under ~10k entries even at multi-million-
// chunk scale.
//
// The store is intentionally minimal: Put, Get, Has, Delete. No
// indexing, no LRU, no quota — those are workspace-level concerns
// layered on top.
package cas

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/richiejp/darkaifactory/internal/oops"
)

// algoSubdir is the directory under root used to scope chunks by
// digest algorithm. Today only sha256 — listing it as a subdir
// leaves room for blake3 / sha512 later without invalidating
// existing chunks.
const algoSubdir = "sha256"

// digestHex is the lowercase hex string form of a sha256 digest:
// 64 chars, [0-9a-f] only.
const digestHexLen = 2 * sha256.Size

// dirPerm: chunks are private to the user — no use case for
// world-readable raw chunk files in this project. (File mode is
// inherited from os.CreateTemp, which uses 0o600 on Unix.)
const dirPerm = 0o700

// ErrCorrupt is returned by Get when the on-disk chunk's hash
// doesn't match its filename. The caller is expected to treat the
// store as compromised and either delete the bad chunk or refuse
// to proceed.
var ErrCorrupt = errors.New("cas: chunk does not match its digest")

// ErrInvalidDigest is returned when a caller passes a malformed
// digest string (wrong length, non-hex characters).
var ErrInvalidDigest = errors.New("cas: invalid digest")

// Store is a content-addressed chunk store rooted at a single
// directory. Concurrent Put / Get / Has are safe; Delete is safe
// against concurrent reads of the same digest.
type Store struct {
	root string
}

// Open returns a Store rooted at dir, creating it if necessary.
// dir must be non-empty. The algoSubdir layout is created lazily
// on first Put.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("cas: empty store dir")
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("cas: mkdir %s: %w", dir, err)
	}
	return &Store{root: dir}, nil
}

// Root returns the on-disk path of the store. Useful for cleanup
// or inspection from tests.
func (s *Store) Root() string {
	return s.root
}

// pathFor returns the absolute path for a given digest, validating
// the digest along the way.
func (s *Store) pathFor(digest string) (string, error) {
	if err := validateDigest(digest); err != nil {
		return "", err
	}
	return filepath.Join(s.root, algoSubdir, digest[:2], digest[2:4], digest), nil
}

// validateDigest checks that digest is a 64-char lowercase hex
// string. We refuse uppercase to keep on-disk paths canonical.
func validateDigest(digest string) error {
	if len(digest) != digestHexLen {
		return fmt.Errorf("%w: want %d hex chars, got %d",
			ErrInvalidDigest, digestHexLen, len(digest))
	}
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return fmt.Errorf("%w: non-hex byte at %d", ErrInvalidDigest, i)
		}
	}
	return nil
}

// Put writes the bytes from r to the store, returning the digest
// (lowercase hex sha256 of the uncompressed payload) and the
// uncompressed size. The on-disk form is zstd-compressed; callers
// see only the digest. The .tmp file is unconditionally removed on
// every exit (success or failure) — Rename moves it to its final
// home before that cleanup fires, so the post-rename Remove is a
// fast no-op via the fs.ErrNotExist allowlist.
func (s *Store) Put(r io.Reader) (digest string, size uint64, err error) {
	tmp, err := os.CreateTemp(s.root, "put-*.tmp")
	if err != nil {
		return "", 0, fmt.Errorf("cas: create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		oops.Skip("cas: remove leftover tmp",
			os.Remove(tmpPath), fs.ErrNotExist)
	}()

	enc, err := zstd.NewWriter(tmp)
	if err != nil {
		oops.Skip("cas: close after zstd-writer error",
			tmp.Close(), os.ErrClosed)
		return "", 0, fmt.Errorf("cas: zstd writer: %w", err)
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(enc, h), r)
	encErr := enc.Close()
	fileErr := tmp.Close()
	switch {
	case copyErr != nil:
		return "", 0, fmt.Errorf("cas: copy: %w", copyErr)
	case encErr != nil:
		return "", 0, fmt.Errorf("cas: zstd close: %w", encErr)
	case fileErr != nil:
		return "", 0, fmt.Errorf("cas: tmp close: %w", fileErr)
	}

	digest = hex.EncodeToString(h.Sum(nil))
	dst, err := s.pathFor(digest)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return "", 0, fmt.Errorf("cas: mkdir %s: %w", filepath.Dir(dst), err)
	}
	// os.CreateTemp opens with 0o600 on Unix — no Chmod needed.
	// Rename is the trust boundary: anything visible at dst has
	// been hash-verified.
	if err := os.Rename(tmpPath, dst); err != nil {
		return "", 0, fmt.Errorf("cas: rename: %w", err)
	}
	return digest, uint64(n), nil //nolint:gosec // G115: io.Copy returns a non-negative int64
}

// Has reports whether a chunk for digest exists. Errors other than
// "missing" surface to the caller — never silently treat permission
// or fs-corruption errors as absent.
func (s *Store) Has(digest string) (bool, error) {
	path, err := s.pathFor(digest)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cas: stat: %w", err)
	}
	return true, nil
}

// Get returns a reader over the uncompressed chunk payload. The
// returned ReadCloser must be Close()d to release the underlying
// file handle and zstd decoder. Get verifies the digest as bytes
// flow out — corruption is reported via Read returning ErrCorrupt
// at the end of the stream.
func (s *Store) Get(digest string) (io.ReadCloser, error) {
	path, err := s.pathFor(digest)
	if err != nil {
		return nil, err
	}
	// #nosec G304 — path is built from a validated digest and the
	// store's own root.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cas: open %s: %w", digest, err)
	}
	dec, err := zstd.NewReader(f)
	if err != nil {
		oops.Skip("cas: close after zstd error", f.Close(), fs.ErrClosed)
		return nil, fmt.Errorf("cas: zstd reader: %w", err)
	}
	return &verifyingReader{
		f:      f,
		dec:    dec,
		hasher: sha256.New(),
		want:   digest,
	}, nil
}

// GetCompressed returns the raw zstd-compressed payload for digest.
// Designed for forwarding chunks over the wire: the receiver does
// the decompression + sha256 verification, so the server burns no
// CPU per byte and the network sees compressed traffic.
func (s *Store) GetCompressed(digest string) ([]byte, error) {
	path, err := s.pathFor(digest)
	if err != nil {
		return nil, err
	}
	// #nosec G304 — path is built from a validated digest and the
	// store's own root.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cas: read %s: %w", digest, err)
	}
	return data, nil
}

// PutCompressed installs already-compressed bytes under the asserted
// digest. The function decompresses + sha256s the payload to verify
// the digest matches before committing, so a peer can never plant a
// chunk under a digest it doesn't actually hash to.
func (s *Store) PutCompressed(digest string, compressed []byte) error {
	if err := validateDigest(digest); err != nil {
		return err
	}
	dec, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("cas: zstd reader: %w", err)
	}
	defer dec.Close()
	h := sha256.New()
	if _, err := io.Copy(h, dec); err != nil {
		return fmt.Errorf("cas: decompress for verify: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != digest {
		return fmt.Errorf("%w: claimed %s, got %s", ErrCorrupt, digest, got)
	}

	dst, err := s.pathFor(digest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return fmt.Errorf("cas: mkdir %s: %w", filepath.Dir(dst), err)
	}
	tmp, err := os.CreateTemp(s.root, "putc-*.tmp")
	if err != nil {
		return fmt.Errorf("cas: create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		oops.Skip("cas: remove leftover tmp",
			os.Remove(tmpPath), fs.ErrNotExist)
	}()
	if _, err := tmp.Write(compressed); err != nil {
		oops.Skip("cas: close after write error", tmp.Close(), os.ErrClosed)
		return fmt.Errorf("cas: write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cas: close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("cas: rename: %w", err)
	}
	return nil
}

// Delete removes a chunk. Missing chunks are not an error —
// Delete is intended to be idempotent so eviction loops don't
// have to Has() first.
func (s *Store) Delete(digest string) error {
	path, err := s.pathFor(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cas: remove %s: %w", digest, err)
	}
	return nil
}

// verifyingReader wraps a zstd-decoding chunk reader and verifies
// the uncompressed sha256 against the requested digest. On EOF, if
// the running hash doesn't match, Read returns ErrCorrupt instead
// of io.EOF — the caller's responsibility is to treat that as a
// store-corruption signal.
type verifyingReader struct {
	f      *os.File
	dec    *zstd.Decoder
	hasher hash.Hash
	want   string
}

func (r *verifyingReader) Read(p []byte) (int, error) {
	n, err := r.dec.Read(p)
	if n > 0 {
		// hash.Hash never returns a non-nil error from Write.
		_, _ = r.hasher.Write(p[:n])
	}
	if errors.Is(err, io.EOF) {
		got := hex.EncodeToString(r.hasher.Sum(nil))
		if got != r.want {
			return n, fmt.Errorf("%w: want %s got %s", ErrCorrupt, r.want, got)
		}
		return n, io.EOF
	}
	return n, err
}

// Close releases the zstd decoder and the underlying file handle.
// dec.Close has no return; f.Close's error is propagated.
func (r *verifyingReader) Close() error {
	r.dec.Close()
	return r.f.Close()
}
