package version

import "testing"

// TestVersionDefault asserts the runtime invariant documented on the Version
// variable: when no ldflags override is applied (as is the case during
// `go test`), Version must be the placeholder "dev". This catches accidental
// renaming of the variable that would silently break the -ldflags injection
// path used by the Makefile and the Nix package.
func TestVersionDefault(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("Version default = %q, want %q", Version, "dev")
	}
}

// TestVersionMutable confirms Version is a package-level var (not a const),
// so -ldflags '-X ...=value' can override it at link time. If someone
// converts it to a const, this test still compiles but the build pipeline
// silently produces binaries reporting "dev" — so we explicitly assert
// mutability here.
func TestVersionMutable(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })

	Version = "test-override"
	if Version != "test-override" {
		t.Fatalf("Version after assignment = %q, want %q", Version, "test-override")
	}
}
