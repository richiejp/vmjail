package sandboxgateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
)

var (
	ErrJobNoRoute     = errors.New("sandboxgateway: no job route for sandbox")
	ErrJobRouteExists = errors.New("sandboxgateway: job route already registered for sandbox")
)

// JobDialer opens one guest-vsock port on a running Sandbox. The caller owns
// the returned connection. Keeping the port as an argument lets the registry
// remain independent of the guest protocol's fixed port assignment.
type JobDialer func(ctx context.Context, port uint32) (net.Conn, error)

// JobRegistry routes detached job connections by Sandbox UID. Routes exist
// only for the lifetime of the corresponding VM.
type JobRegistry struct {
	mu      sync.RWMutex
	dialers map[string]JobDialer
}

func NewJobRegistry() *JobRegistry {
	return &JobRegistry{dialers: map[string]JobDialer{}}
}

func (r *JobRegistry) Register(sandboxUID string, dial JobDialer) error {
	if sandboxUID == "" {
		return errors.New("sandboxgateway: JobRegistry.Register: empty sandboxUID")
	}
	if dial == nil {
		return errors.New("sandboxgateway: JobRegistry.Register: nil dial")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.dialers[sandboxUID]; ok {
		return fmt.Errorf("%w: sandbox=%s", ErrJobRouteExists, sandboxUID)
	}
	r.dialers[sandboxUID] = dial
	return nil
}

func (r *JobRegistry) Unregister(sandboxUID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.dialers, sandboxUID)
}

// Dial matches sandboxjob.Dialer.
func (r *JobRegistry) Dial(ctx context.Context, sandboxUID string, port uint32) (net.Conn, error) {
	r.mu.RLock()
	dial := r.dialers[sandboxUID]
	r.mu.RUnlock()
	if dial == nil {
		return nil, fmt.Errorf("%w: sandbox=%s", ErrJobNoRoute, sandboxUID)
	}
	return dial(ctx, port)
}
