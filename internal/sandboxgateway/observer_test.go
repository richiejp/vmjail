package sandboxgateway

import (
	"errors"
	"testing"
)

func TestObserverRegistryRegisterResolve(t *testing.T) {
	r := NewObserverRegistry()
	if err := r.Register("sb-1", "/state/sb-1/observer.jsonl"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := r.Resolve("sb-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "/state/sb-1/observer.jsonl" {
		t.Errorf("path = %q, want the registered path", got)
	}
}

func TestObserverRegistryRegisterRejectsEmpty(t *testing.T) {
	r := NewObserverRegistry()
	if err := r.Register("", "/p"); err == nil {
		t.Error("empty uid should be rejected")
	}
	if err := r.Register("sb", ""); err == nil {
		t.Error("empty path should be rejected")
	}
}

func TestObserverRegistryRegisterDuplicate(t *testing.T) {
	r := NewObserverRegistry()
	if err := r.Register("sb", "/p"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := r.Register("sb", "/q"); !errors.Is(err, ErrObserverRouteExists) {
		t.Errorf("dup Register = %v, want ErrObserverRouteExists", err)
	}
}

func TestObserverRegistryResolveMiss(t *testing.T) {
	r := NewObserverRegistry()
	if _, err := r.Resolve("ghost"); !errors.Is(err, ErrObserverNoRoute) {
		t.Errorf("Resolve miss = %v, want ErrObserverNoRoute", err)
	}
}

func TestObserverRegistryUnregisterIdempotent(t *testing.T) {
	r := NewObserverRegistry()
	if err := r.Register("sb", "/p"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r.Unregister("sb")
	r.Unregister("sb") // second is a no-op
	if _, err := r.Resolve("sb"); !errors.Is(err, ErrObserverNoRoute) {
		t.Errorf("after Unregister, Resolve = %v, want ErrObserverNoRoute", err)
	}
	// Re-register must succeed once the route is gone.
	if err := r.Register("sb", "/p2"); err != nil {
		t.Errorf("re-Register after Unregister: %v", err)
	}
}
