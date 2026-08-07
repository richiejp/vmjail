package nodectrl

import (
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/identity"
)

func TestRegistryWatchCoalescesAndStops(t *testing.T) {
	fingerprint, err := identity.ParseFingerprint("sha256:" + strings.Repeat("7", 64))
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(nil)
	changes, stop := registry.Watch()
	registry.Register(fingerprint, "node", "version", Resources{})
	registry.Register(fingerprint, "node", "version", Resources{})
	if revision := <-changes; revision == 0 {
		t.Fatal("zero revision")
	}
	stop()
	registry.Forget(fingerprint)
	registry.Forget(fingerprint)
}
