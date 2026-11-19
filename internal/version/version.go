// Package version exposes the build-time version string of the vmjail binary.
//
// The Version variable is set via -ldflags at build time:
//
//	go build -ldflags '-X github.com/richiejp/vmjail/internal/version.Version=1.2.3' ./cmd/vmjail
//
// When the binary is run from `go run` or built without ldflags, Version
// retains its default value of "dev". This makes it easy to tell a release
// build from an ad-hoc one.
package version

// Version is the semantic version of the vmjail binary. Overridden at build
// time via -ldflags. The default "dev" indicates an unversioned local build.
//
//nolint:gochecknoglobals // ldflags-set build identity, conventional Go pattern.
var Version = "dev"
