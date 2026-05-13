// Package sandboxctrl is the orchestrator-side state for the
// sandbox-control plane. It owns the live set of registered
// sandboxes — keyed by mTLS fingerprint — and the rules for how
// registrations expire when heartbeats stop.
//
// The Connect-RPC adapter that drives this package lives in
// internal/orchestrator/sandboxctrl.go; this split mirrors
// workspacebroker / orchestrator/broker.go so the registry stays
// independent of the wire format.
package sandboxctrl

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/richiejp/darkaifactory/internal/identity"
)

// ErrUnknownSandbox is returned by Heartbeat when the caller's
// fingerprint has no live registration. Sandboxes that have been
// forgotten (because their stream closed, or because they timed
// out via Sweep) must re-register before heartbeats are accepted
// again — otherwise the orchestrator could keep stale entries
// alive indefinitely.
var ErrUnknownSandbox = errors.New("sandboxctrl: no registration for caller")

// Resources is the host's reported capacity. Mirrors the proto
// HostResources message but stays decoupled so the registry
// doesn't depend on generated code. Conversion happens at the
// Connect-RPC boundary.
type Resources struct {
	CPUs        uint32
	MemoryBytes uint64
}

// Entry is a single live registration. Snapshot returns a slice of
// these; tests and the (Slice A2) ListSandboxes RPC read them.
type Entry struct {
	Fingerprint  identity.Fingerprint
	Version      string
	Resources    Resources
	RegisteredAt time.Time
	LastSeenAt   time.Time
}

// Registry tracks the live set of sandbox-host registrations.
// Safe for concurrent use; every public method takes the mutex.
// State is in-memory only — Slice B5 will introduce persistence.
type Registry struct {
	now func() time.Time

	mu  sync.Mutex
	all map[identity.Fingerprint]Entry
}

// NewRegistry returns an empty Registry. now is called for every
// timestamp the Registry mints; pass time.Now in production and
// a deterministic stub in tests.
func NewRegistry(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{
		now: now,
		all: map[identity.Fingerprint]Entry{},
	}
}

// Register records (or refreshes) a sandbox's registration and
// returns the accepted-at timestamp. Existing registrations for
// the same fingerprint are replaced — last-register-wins — but
// the original RegisteredAt is preserved if the caller wants to
// observe "first seen" via Snapshot.
func (r *Registry) Register(fp identity.Fingerprint, version string, res Resources) time.Time {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	registered := now
	if prev, ok := r.all[fp]; ok {
		registered = prev.RegisteredAt
	}
	r.all[fp] = Entry{
		Fingerprint:  fp,
		Version:      version,
		Resources:    res,
		RegisteredAt: registered,
		LastSeenAt:   now,
	}
	return now
}

// Heartbeat refreshes the LastSeenAt timestamp for fp's entry.
// res, if non-zero, updates the recorded capacity (the sandbox may
// report shrunk free memory as load rises). Returns the recorded
// time, or ErrUnknownSandbox if no entry exists for fp.
func (r *Registry) Heartbeat(fp identity.Fingerprint, res Resources) (time.Time, error) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.all[fp]
	if !ok {
		return time.Time{}, ErrUnknownSandbox
	}
	entry.LastSeenAt = now
	if res != (Resources{}) {
		entry.Resources = res
	}
	r.all[fp] = entry
	return now, nil
}

// Forget removes fp's registration if present. Called by the
// RegisterSandbox handler in defer so a closed stream releases
// the entry promptly. Idempotent.
func (r *Registry) Forget(fp identity.Fingerprint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.all, fp)
}

// Snapshot returns the current set of registrations, sorted by
// fingerprint string for deterministic test output.
func (r *Registry) Snapshot() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, 0, len(r.all))
	for _, e := range r.all {
		out = append(out, e)
	}
	sortByFingerprint(out)
	return out
}

// Lookup returns the entry for fp and whether it exists. The
// returned entry is a value copy; mutating it does not affect the
// registry.
func (r *Registry) Lookup(fp identity.Fingerprint) (Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.all[fp]
	return e, ok
}

func sortByFingerprint(es []Entry) {
	sort.Slice(es, func(i, j int) bool {
		return es[i].Fingerprint.String() < es[j].Fingerprint.String()
	})
}
