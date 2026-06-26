package sandboxdcli

import (
	"net/http"
	"testing"

	"github.com/richiejp/darkaifactory/internal/ociregistry"
)

// TestResolveBuildImageRef covers the three branches of the build-image ref
// resolution: an explicit DAFT_BUILD_IMAGE_REF override, the no-transport
// case (build path off, ref empty), and the default to the orchestrator's
// auto-seeded built-in builder when a hosted transport exists.
func TestResolveBuildImageRef(t *testing.T) {
	t.Run("env override wins", func(t *testing.T) {
		t.Setenv(envBuildImageRef, "custom.example/builder:v9")
		if got := resolveBuildImageRef(nil); got != "custom.example/builder:v9" {
			t.Errorf("got %q, want the override", got)
		}
	})
	t.Run("no env, no transport -> empty", func(t *testing.T) {
		t.Setenv(envBuildImageRef, "")
		if got := resolveBuildImageRef(nil); got != "" {
			t.Errorf("got %q, want empty (build path off without a hosted transport)", got)
		}
	})
	t.Run("no env, transport present -> built-in ref", func(t *testing.T) {
		t.Setenv(envBuildImageRef, "")
		if got := resolveBuildImageRef(http.DefaultTransport); got != ociregistry.BuiltinBuildRef() {
			t.Errorf("got %q, want %q", got, ociregistry.BuiltinBuildRef())
		}
	})
}
