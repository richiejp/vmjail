// Package workspacecatalog is the orchestrator-side store for the
// "Workspace as a first-class object" model (PLAN.md Phase B).
// It owns the live set of Workspace records keyed by UUID, with a
// secondary (owner, name) index used to enforce per-owner alias
// uniqueness at Create time.
//
// The wire/Connect-RPC adapter that drives this package will live
// in internal/orchestrator/workspacecatalog.go (Slice B2). This
// split mirrors sandboxctrl / orchestrator.sandboxctrl so the
// catalog stays independent of generated proto types.
//
// State is in-memory only; persistence (Badger or JSON-on-disk)
// lands in Slice B5.
package workspacecatalog

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/richiejp/darkaifactory/internal/assert"
	"github.com/richiejp/darkaifactory/internal/identity"
)

// Status is the workspace lifecycle state. Mirrors the proto
// WorkspaceStatus enum without depending on generated code; the
// Connect-RPC adapter does the cross-translation. Allowed
// transitions are codified in CanTransition; the full state
// machine surface is Slice B4.
type Status uint8

const (
	StatusUnspecified Status = iota
	StatusDraft
	StatusReady
	StatusRunning
	StatusFrozen
	StatusStopped
	StatusArchived
)

// statusUnspecifiedStr is the rendered form returned by
// Status.String for every value outside the defined enum.
const statusUnspecifiedStr = "UNSPECIFIED"

// String renders Status for diagnostics and error messages.
func (s Status) String() string {
	switch s {
	case StatusDraft:
		return "DRAFT"
	case StatusReady:
		return "READY"
	case StatusRunning:
		return "RUNNING"
	case StatusFrozen:
		return "FROZEN"
	case StatusStopped:
		return "STOPPED"
	case StatusArchived:
		return "ARCHIVED"
	}
	return statusUnspecifiedStr
}

// Mount is a workspace filesystem mount. Mirrors the proto Mount
// from daft.sandboxctrl.v1 without depending on generated code.
type Mount struct {
	Target   string
	Source   string
	ReadOnly bool
}

// EnvVar is one name=value pair on the workspace.
type EnvVar struct {
	Name  string
	Value string
}

// Workspace is the catalog's record of a single workspace.
//
// Identity fields (UUID, OwnerFingerprint, CreatedAt) are
// immutable after Create. The Store enforces this — callers cannot
// mutate them via Update.
type Workspace struct {
	UUID             string
	Name             string
	OwnerFingerprint identity.Fingerprint

	Mounts []Mount
	Env    []EnvVar

	ImageRef       string
	PendingBuildID string

	TargetSandboxFingerprint string

	LockOwnerFingerprint identity.Fingerprint
	LockExpiresAt        time.Time

	Status  Status
	Missing []string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Sentinel errors. ErrAliasInUse lets the Connect adapter map a
// duplicate (owner, name) to AlreadyExists; ErrNotFound maps to
// NotFound; ErrImmutableField lets callers detect attempts to
// mutate identity fields without wrapping every Update result.
var (
	ErrNotFound       = errors.New("workspacecatalog: workspace not found")
	ErrAliasInUse     = errors.New("workspacecatalog: name already used by this owner")
	ErrImmutableField = errors.New("workspacecatalog: refusing to mutate immutable field")
)

// Store holds the live set of workspaces. Safe for concurrent use.
// All public methods take the mutex; reads return value copies so
// callers can never observe in-place mutation.
type Store struct {
	now func() time.Time

	mu  sync.Mutex
	all map[string]Workspace
	// alias indexes (owner, name) → uuid; populated on Create and
	// kept consistent on every Update / Delete.
	byAlias map[aliasKey]string
}

type aliasKey struct {
	owner identity.Fingerprint
	name  string
}

// NewStore returns an empty Store. now mints every timestamp the
// Store sets; pass time.Now in production and a deterministic stub
// in tests.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		now:     now,
		all:     map[string]Workspace{},
		byAlias: map[aliasKey]string{},
	}
}

// CreateInput is the new-workspace shape. UUID is supplied by the
// caller so the Connect adapter (B2) can echo the same value back
// to the client without a second RPC. Empty UUID or empty Name is
// rejected.
type CreateInput struct {
	UUID             string
	Name             string
	OwnerFingerprint identity.Fingerprint
}

// Create inserts a new workspace. Returns ErrAliasInUse if (Owner,
// Name) already exists. The new workspace starts in StatusDraft
// with Missing listing the still-empty required fields
// (image, target, at least one mount).
func (s *Store) Create(in CreateInput) (Workspace, error) {
	assert.True(in.UUID != "",
		"workspacecatalog.Create requires a non-empty UUID; the caller mints one so the Connect adapter can echo it back without a second round-trip")
	assert.True(in.Name != "",
		"workspacecatalog.Create requires a non-empty Name; default basename(pwd) on the user-host before calling")
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.all[in.UUID]; dup {
		return Workspace{}, fmt.Errorf("workspacecatalog: uuid %q already exists", in.UUID)
	}
	key := aliasKey{owner: in.OwnerFingerprint, name: in.Name}
	if _, dup := s.byAlias[key]; dup {
		return Workspace{}, ErrAliasInUse
	}
	ws := Workspace{
		UUID:             in.UUID,
		Name:             in.Name,
		OwnerFingerprint: in.OwnerFingerprint,
		Status:           StatusDraft,
		Missing:          missingFields(Workspace{}),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	s.all[in.UUID] = ws
	s.byAlias[key] = in.UUID
	return ws, nil
}

// Get returns the workspace identified by uuid. The returned value
// is a deep-enough copy that mutating slice headers won't race the
// store; the backing arrays are shared, so callers MUST treat the
// result as read-only.
func (s *Store) Get(uuid string) (Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.all[uuid]
	if !ok {
		return Workspace{}, ErrNotFound
	}
	return ws, nil
}

// LookupByAlias returns the workspace whose (owner, name) matches.
// Used by the CLI's "address by name" path before any mutation.
func (s *Store) LookupByAlias(owner identity.Fingerprint, name string) (Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uuid, ok := s.byAlias[aliasKey{owner: owner, name: name}]
	if !ok {
		return Workspace{}, ErrNotFound
	}
	return s.all[uuid], nil
}

// Update applies fn to a copy of the workspace and stores the
// result. fn MUST NOT modify identity fields (UUID,
// OwnerFingerprint, CreatedAt) — the Store re-asserts the original
// values on return and reports ErrImmutableField if they changed.
// Missing[] and UpdatedAt are recomputed by the Store.
//
// Update under a nil fn is a no-op that still bumps UpdatedAt —
// useful for touch-style refreshes (lock renewal) without a
// separate API.
func (s *Store) Update(uuid string, fn func(*Workspace) error) (Workspace, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.all[uuid]
	if !ok {
		return Workspace{}, ErrNotFound
	}
	next := prev
	if fn != nil {
		if err := fn(&next); err != nil {
			return Workspace{}, err
		}
	}
	if next.UUID != prev.UUID ||
		next.OwnerFingerprint != prev.OwnerFingerprint ||
		!next.CreatedAt.Equal(prev.CreatedAt) {
		return Workspace{}, ErrImmutableField
	}
	if next.Name != prev.Name {
		oldKey := aliasKey{owner: prev.OwnerFingerprint, name: prev.Name}
		newKey := aliasKey{owner: prev.OwnerFingerprint, name: next.Name}
		if _, dup := s.byAlias[newKey]; dup {
			return Workspace{}, ErrAliasInUse
		}
		delete(s.byAlias, oldKey)
		s.byAlias[newKey] = uuid
	}
	next.Missing = missingFields(next)
	next.UpdatedAt = now
	s.all[uuid] = next
	return next, nil
}

// Delete removes the workspace if present. Idempotent.
func (s *Store) Delete(uuid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.all[uuid]
	if !ok {
		return
	}
	delete(s.all, uuid)
	delete(s.byAlias, aliasKey{owner: prev.OwnerFingerprint, name: prev.Name})
}

// List returns every workspace, sorted by UUID for deterministic
// test output. The mutex is released before sorting since the
// returned slice is private to the caller's goroutine.
func (s *Store) List() []Workspace {
	s.mu.Lock()
	out := make([]Workspace, 0, len(s.all))
	// Indexed-style iteration via the map keyset to avoid the
	// per-iteration value copy that ranging over the values would
	// incur (Workspace is ~300 bytes today).
	for uuid := range s.all {
		out = append(out, s.all[uuid])
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].UUID < out[j].UUID })
	return out
}

// CanTransition reports whether a status change is allowed by the
// (B4) workspace state machine. Used by the Connect adapter to
// reject invalid transitions before they hit the store. The full
// state machine — including the side effects each transition
// triggers (Launch, Stop, Freeze, etc.) — is Slice B4's surface;
// B1 reserves the allowed-edges table so the proto enum doesn't
// need to be rewired later.
func CanTransition(from, to Status) bool {
	if from == to {
		return true
	}
	switch from {
	case StatusUnspecified:
		return to == StatusDraft
	case StatusDraft:
		return to == StatusReady || to == StatusArchived
	case StatusReady:
		return to == StatusRunning || to == StatusDraft || to == StatusArchived
	case StatusRunning:
		return to == StatusFrozen || to == StatusStopped
	case StatusFrozen:
		return to == StatusRunning || to == StatusStopped
	case StatusStopped:
		return to == StatusRunning || to == StatusDraft || to == StatusArchived
	}
	return false
}

// missingFields returns the human-readable list of required
// workspace fields that are still unset. Used to populate
// Workspace.Missing on Create and after every Update.
//
// The required set: at least one Mount, exactly one of ImageRef or
// PendingBuildID, and a target (TargetSandboxFingerprint). The CLI
// uses this list verbatim, so the strings are user-facing.
func missingFields(ws Workspace) []string {
	var out []string
	if len(ws.Mounts) == 0 {
		out = append(out, "mount")
	}
	if ws.ImageRef == "" && ws.PendingBuildID == "" {
		out = append(out, "image")
	}
	if ws.TargetSandboxFingerprint == "" {
		out = append(out, "target")
	}
	return out
}
