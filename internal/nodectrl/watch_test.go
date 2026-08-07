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
	if revision := <-changes; revision == 0 {
		t.Fatal("zero revision")
	}
	if _, err := registry.Heartbeat(fingerprint, Resources{}); err != nil {
		t.Fatal(err)
	}
	select {
	case revision := <-changes:
		t.Fatalf("unchanged heartbeat emitted revision %d", revision)
	default:
	}
	if _, err := registry.Heartbeat(fingerprint, Resources{CPUs: 2}); err != nil {
		t.Fatal(err)
	}
	if revision := <-changes; revision == 0 {
		t.Fatal("zero resource-change revision")
	}
	registry.Register(fingerprint, "node", "version", Resources{CPUs: 2})
	registry.Register(fingerprint, "node", "version", Resources{CPUs: 2})
	<-changes // Repeated registrations coalesce while the watcher is slow.
	stop()
	registry.Forget(fingerprint)
	registry.Forget(fingerprint)
}
