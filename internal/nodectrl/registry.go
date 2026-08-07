// Package nodectrl is the orchestrator-side state for the
// node-control plane. It owns the live set of registered Nodes —
// keyed by mTLS fingerprint — and the rules for how registrations
// expire when heartbeats stop.
//
// The Connect-RPC adapter that drives this package lives in
// internal/orchestrator/nodectrl.go; this split mirrors
// workspacebroker / orchestrator/broker.go so the registry stays
// independent of the wire format.
package nodectrl

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/richiejp/darkaifactory/internal/identity"
)

// ErrUnknownNode is returned by Heartbeat when the caller's
// fingerprint has no live registration. Sandboxes that have been
// forgotten (because their stream closed, or because they timed
// out via Sweep) must re-register before heartbeats are accepted
// again — otherwise the orchestrator could keep stale entries
// alive indefinitely.
var ErrUnknownNode = errors.New("nodectrl: no registration for caller")

// Resources is the host's reported capacity. Mirrors the proto
// HostResources message but stays decoupled so the registry
// doesn't depend on generated code. Conversion happens at the
// Connect-RPC boundary.
type Resources struct {
	CPUs        uint32
	MemoryBytes uint64
}

// Entry is a single live registration. Snapshot returns a slice of
// these; tests and the (Slice A2) ListNodes RPC read them.
type Entry struct {
	Fingerprint identity.Fingerprint
	// InstanceID changes on every sandboxd process start. Durable controllers
	// use it to detect that process-local resources such as detached jobs were
	// lost even when the same certificate reconnects.
	InstanceID string
	// Hostname is the sandbox-host's OS hostname, reported at
	// registration. A human-friendly alias for selection/display; may
	// be empty and may collide across hosts. The fingerprint is the
	// canonical id.
	Hostname     string
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

	mu        sync.Mutex
	all       map[identity.Fingerprint]Entry
	revision  uint64
	nextWatch uint64
	watchers  map[uint64]chan uint64
}

// NewRegistry returns an empty Registry. now is called for every
// timestamp the Registry mints; pass time.Now in production and
// a deterministic stub in tests.
func NewRegistry(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{
		now:      now,
		all:      map[identity.Fingerprint]Entry{},
		watchers: map[uint64]chan uint64{},
	}
}

// Watch reports successful registry changes. Notifications are coalesced when
// a reader is slow; the revision tells callers only that they must resnapshot.
func (r *Registry) Watch() (changes <-chan uint64, stop func()) {
	r.mu.Lock()
	id := r.nextWatch
	r.nextWatch++
	ch := make(chan uint64, 1)
	r.watchers[id] = ch
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.watchers, id)
		r.mu.Unlock()
	}
}

func (r *Registry) notifyLocked() {
	r.revision++
	for _, ch := range r.watchers {
		select {
		case ch <- r.revision:
		default:
		}
	}
}

// Register records (or refreshes) a sandbox's registration and
// returns the accepted-at timestamp. Existing registrations for
// the same fingerprint are replaced — last-register-wins — but
// the original RegisteredAt is preserved if the caller wants to
// observe "first seen" via Snapshot.
func (r *Registry) Register(fp identity.Fingerprint, hostname, version string, res Resources) time.Time {
	return r.RegisterInstance(fp, "", hostname, version, res)
}

// RegisterInstance is Register with the process-lifetime Node identity carried
// by current protocol peers. Register remains as a compatibility helper for
// in-process callers that do not model a live daemon.
func (r *Registry) RegisterInstance(fp identity.Fingerprint, instanceID, hostname, version string, res Resources) time.Time {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	registered := now
	if prev, ok := r.all[fp]; ok {
		registered = prev.RegisteredAt
	}
	r.all[fp] = Entry{
		Fingerprint:  fp,
		InstanceID:   instanceID,
		Hostname:     hostname,
		Version:      version,
		Resources:    res,
		RegisteredAt: registered,
		LastSeenAt:   now,
	}
	r.notifyLocked()
	return now
}

// Heartbeat refreshes the LastSeenAt timestamp for fp's entry.
// res, if non-zero, updates the recorded capacity (the sandbox may
// report shrunk free memory as load rises). Returns the recorded
// time, or ErrUnknownNode if no entry exists for fp.
func (r *Registry) Heartbeat(fp identity.Fingerprint, res Resources) (time.Time, error) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.all[fp]
	if !ok {
		return time.Time{}, ErrUnknownNode
	}
	entry.LastSeenAt = now
	resourcesChanged := res != (Resources{}) && res != entry.Resources
	if res != (Resources{}) {
		entry.Resources = res
	}
	r.all[fp] = entry
	if resourcesChanged {
		r.notifyLocked()
	}
	return now, nil
}

// Forget removes fp's registration if present. Called by the
// RegisterNode handler in defer so a closed stream releases
// the entry promptly. Idempotent.
func (r *Registry) Forget(fp identity.Fingerprint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.all[fp]; ok {
		delete(r.all, fp)
		r.notifyLocked()
	}
}

// Snapshot returns the current set of registrations, sorted by
// fingerprint string for deterministic test output. The mutex is
// released before the sort — out is private to the caller's
// goroutine, so the O(n log n) work doesn't need to block
// concurrent Register / Heartbeat / Forget.
func (r *Registry) Snapshot() []Entry {
	r.mu.Lock()
	out := make([]Entry, 0, len(r.all))
	for fp := range r.all {
		out = append(out, r.all[fp])
	}
	r.mu.Unlock()
	sortByFingerprint(out)
	return out
}

// RegisteredFingerprints returns the fingerprints of every live
// registration, sorted for determinism. A thin projection of
// Snapshot the orchestrator uses to auto-pin the sole node as a
// workspace's target on a single-node cluster.
func (r *Registry) RegisteredFingerprints() []identity.Fingerprint {
	entries := r.Snapshot()
	out := make([]identity.Fingerprint, len(entries))
	for i := range entries {
		out[i] = entries[i].Fingerprint
	}
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
