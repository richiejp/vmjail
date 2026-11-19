// Package ociregistry is the client-side glue for the orchestrator's
// embedded OCI registry (PLAN.md item 8). The orchestrator serves the
// registry on its mTLS endpoint; this package lets sandbox-hosts pull and
// clients push over that same mutually-authenticated channel.
//
// The wrinkle it hides: go-containerregistry forces *http* for loopback /
// RFC1918 registry names, but the orchestrator serves *https* (mTLS). So
// orchestrator-hosted image refs use a non-loopback sentinel host (Host)
// — go-containerregistry then chooses https — while Transport redirects
// the actual TCP dial to the orchestrator's real address. The TLS layer
// is the standard client mTLS config, whose verification is fingerprint
// (TOFU) based, so the sentinel SNI/hostname is irrelevant.
package ociregistry

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Host is the sentinel registry hostname for the orchestrator's embedded
// registry. It contains a dot and is neither loopback nor RFC1918, so
// go-containerregistry resolves it to https; Transport then dials the
// orchestrator's real address regardless of this name.
const Host = "daft-registry.internal"

// Ref builds an orchestrator-hosted image reference (Host/repo:tag).
func Ref(repo, tag string) string {
	return Host + "/" + repo + ":" + tag
}

// BuiltinBuildRepo / BuiltinBuildTag name the built-in devcontainer build
// image (the buildah-based image builder) in the orchestrator's embedded
// registry. The orchestrator auto-seeds this ref from its bundled copy at
// startup, and the sandbox-host defaults DAFT_BUILD_IMAGE_REF to it — so a
// build-based devcontainer works with no env vars set. The tag is a fixed
// sentinel (not the binary version) so the node and orchestrator agree on
// the ref without sharing a version string.
const (
	BuiltinBuildRepo = "daft-builtin/build"
	BuiltinBuildTag  = "bundled"
)

// BuiltinBuildRef is the orchestrator-hosted ref of the built-in build
// image. Both the orchestrator (seeder) and the sandbox-host (default
// DAFT_BUILD_IMAGE_REF) resolve to this single value.
func BuiltinBuildRef() string {
	return Ref(BuiltinBuildRepo, BuiltinBuildTag)
}

// IsHosted reports whether ref points at the orchestrator's embedded
// registry (its registry component is Host). Used by the sandbox-host
// puller to decide between the mTLS transport and the default keychain.
func IsHosted(ref string) bool {
	r, err := name.ParseReference(ref)
	if err != nil {
		return false
	}
	return r.Context().RegistryStr() == Host
}

// Transport returns an http.RoundTripper that speaks mTLS to the
// orchestrator at orchAddr ("host:port") for every request, regardless of
// the (sentinel) registry name in the ref. tlsCfg is the client mTLS
// configuration supplied by the caller.
//
// ALPN is pinned to HTTP/1.1: large image-blob uploads over HTTP/2
// deadlock on flow control with go-containerregistry, and the
// orchestrator's mTLS server offers http/1.1 alongside h2, so pinning it
// here keeps registry traffic off h2 without affecting the RPC clients.
func Transport(orchAddr string, tlsCfg *tls.Config) *http.Transport {
	c := tlsCfg.Clone()
	c.NextProtos = []string{"http/1.1"}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, orchAddr)
		},
		TLSClientConfig: c,
	}
}

// Push loads an image tarball (raw or gzipped docker/OCI tar) and pushes
// it to repo:tag in the orchestrator registry over rt, returning the
// orchestrator-hosted ref.
func Push(tarPath, repo, tag string, rt http.RoundTripper) (string, error) {
	img, err := loadImage(tarPath)
	if err != nil {
		return "", err
	}
	ref := Ref(repo, tag)
	if err := crane.Push(img, ref, crane.WithTransport(rt)); err != nil {
		return "", fmt.Errorf("ociregistry: push %s: %w", ref, err)
	}
	return ref, nil
}

// loadImage reads a docker/OCI image tarball that may be gzipped (the Nix
// image outputs are .tar.gz; go-containerregistry's tarball reader only
// reads raw tar). The raw tar is held in memory and served through an
// opener, because tarball images are lazy — the layers are read during
// the later Push, so a temp file would have to outlive this call.
func loadImage(tarPath string) (v1.Image, error) {
	in, err := os.Open(tarPath) //nolint:gosec // operator-supplied local image path.
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Close() }()
	img, err := ImageFromReader(in)
	if err != nil {
		return nil, fmt.Errorf("ociregistry: load image %s: %w", tarPath, err)
	}
	return img, nil
}

// ImageFromReader loads a docker/OCI image from r, which may be a raw or
// gzipped tar (the Nix image outputs are .tar.gz; go-containerregistry's
// tarball reader only reads raw tar). The raw tar is held in memory and
// served through an opener, because tarball images are lazy — the layers
// are read during the later Push, so a streaming reader would have to
// outlive this call. Used to load embedded/bundled image bytes without a
// temp file.
func ImageFromReader(r io.Reader) (v1.Image, error) {
	raw, err := readRawTar(r)
	if err != nil {
		return nil, err
	}
	img, err := tarball.Image(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("ociregistry: load image: %w", err)
	}
	return img, nil
}

// readRawTar returns the uncompressed tar bytes of r, transparently
// gunzipping a gzipped input. It sniffs the gzip magic (1f 8b) via a
// non-consuming Peek, so there is one linear read path either way.
func readRawTar(r io.Reader) ([]byte, error) {
	br := bufio.NewReader(r)
	magic, _ := br.Peek(2) // short read (<2 bytes) → not gzip, fall through.
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("ociregistry: decompress: %w", err)
		}
		defer func() { _ = gz.Close() }()
		return io.ReadAll(gz) //nolint:gosec // trusted local image tarball.
	}
	return io.ReadAll(br)
}
