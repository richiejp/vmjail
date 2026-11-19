package sandboxdcli

import (
	"net/http"
	"testing"
)

func TestOpenSandboxbootStore(t *testing.T) {
	t.Setenv(EnvStoreDir, t.TempDir())

	// nil transport: no hosted registry wired.
	if _, err := openSandboxbootStore(nil); err != nil {
		t.Fatalf("openSandboxbootStore(nil): %v", err)
	}

	// non-nil transport exercises the UseHostedRegistry branch.
	rt := http.DefaultTransport
	if _, err := openSandboxbootStore(rt); err != nil {
		t.Fatalf("openSandboxbootStore(rt): %v", err)
	}
}
