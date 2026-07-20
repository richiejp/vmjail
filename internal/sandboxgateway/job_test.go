package sandboxgateway

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestJobRegistryLifecycleAndValidation(t *testing.T) {
	r := NewJobRegistry()
	if _, err := r.Dial(t.Context(), "missing", 9100); !errors.Is(err, ErrJobNoRoute) {
		t.Fatalf("missing Dial = %v", err)
	}
	if err := r.Register("", func(context.Context, uint32) (net.Conn, error) {
		return nil, errors.New("unused")
	}); err == nil {
		t.Fatal("empty UID registered")
	}
	if err := r.Register("sb", nil); err == nil {
		t.Fatal("nil dialer registered")
	}

	host, guest := net.Pipe()
	defer func() { _ = guest.Close() }()
	var gotPort uint32
	dial := func(_ context.Context, port uint32) (net.Conn, error) {
		gotPort = port
		return host, nil
	}
	if err := r.Register("sb", dial); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("sb", dial); !errors.Is(err, ErrJobRouteExists) {
		t.Fatalf("duplicate Register = %v", err)
	}
	conn, err := r.Dial(t.Context(), "sb", 9191)
	if err != nil || conn != host || gotPort != 9191 {
		t.Fatalf("Dial = %v port=%d err=%v", conn, gotPort, err)
	}
	r.Unregister("sb")
	r.Unregister("sb")
	if _, err := r.Dial(t.Context(), "sb", 9100); !errors.Is(err, ErrJobNoRoute) {
		t.Fatalf("Dial after unregister = %v", err)
	}
}
