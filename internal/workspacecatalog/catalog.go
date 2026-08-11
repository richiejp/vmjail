// Package workspacecatalog is the orchestrator-side store for the
// "Workspace as a first-class object" model (PLAN.md Phase B).
// It owns the live set of Workspace records keyed by UUID, with a
// secondary (owner, name) index used to enforce per-owner alias
// uniqueness at Create time.
//
// Workspace shape mirrors Kubernetes: Metadata + Spec + Status.
//   - Metadata: identity, owner, generation, audit timestamps
//   - Spec:     declared workload state (user-writable)
//   - Status:   observed state (controller-owned), including
//     conditions[], current Sandbox ref, and the
//     human-readable Missing[] field that drives the
//     Ready condition's reason.
//
// The wire/Connect-RPC adapter that drives this package lives in
// internal/orchestrator/workspacecatalog.go. This split mirrors
// nodectrl / orchestrator.nodectrl so the catalog stays independent
// of generated proto types.
package workspacecatalog

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/richiejp/darkaifactory/internal/assert"
	"github.com/richiejp/darkaifactory/internal/identity"
)

// Condition types. Defined here as constants so handlers and the
// reconciler use the same PascalCase tokens that ARCHITECTURE.md
// pins. New types are appended; readers must ignore unknown values.
const (
	ConditionReady         = "Ready"
	ConditionDispatched    = "Dispatched"
	ConditionReconciled    = "Reconciled"
	ConditionImageResolved = "ImageResolved"
	ConditionStorageFrozen = "StorageFrozen"
)

// conditionStatusUnspecifiedStr is the rendered form for
// ConditionStatus values outside the defined enum. Hoisted because
// goconst flags the duplicate string literal.
const conditionStatusUnspecifiedStr = "Unspecified"

// MissingTarget is the missingFields token for an unset target
// (TargetSandboxFingerprint). Exported so the orchestrator's
// single-node auto-pin can recognise "target is the only thing
// standing between this workspace and Ready" without re-deriving the
// user-facing string.
const MissingTarget = "target"

// ConditionStatus mirrors daft.sandbox.v1.ConditionStatus. The
// Connect adapter does the cross-translation; the catalog stays
// independent of generated proto types.
type ConditionStatus uint8

const (
	ConditionStatusUnspecified ConditionStatus = iota
	ConditionStatusTrue
	ConditionStatusFalse
	ConditionStatusUnknown
)

// String renders ConditionStatus for diagnostics. Matches the
// PascalCase tokens used elsewhere in the wire protocol.
func (s ConditionStatus) String() string {
	switch s {
	case ConditionStatusTrue:
		return "True"
	case ConditionStatusFalse:
		return "False"
	case ConditionStatusUnknown:
		return "Unknown"
	}
	return conditionStatusUnspecifiedStr
}

// Condition is one row of WorkspaceStatus.Conditions. Mirrors the
// K8s-shaped condition catalogued in proto/sandbox/v1/sandbox.proto.
type Condition struct {
	Type               string
	Status             ConditionStatus
	Reason             string
	Message            string
	LastTransitionTime time.Time
}

// Mount is a workspace filesystem mount.
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

// PortMapping is one container port the workload binds. Protocol
// is kept as the wire enum integer (TCP=1, UDP=2) rather than its
// own Go enum so the catalog stays single-source-of-truth for
// what's on the wire; int32 matches the proto enum's underlying
// type so adapter-side casts are no-ops.
type PortMapping struct {
	ContainerPort uint32
	Protocol      int32
}

// SandboxRef is the Go-native mirror of daft.sandbox.v1.SandboxRef.
// Carried on Workspace so the catalog package stays independent of
// the wire types.
type SandboxRef struct {
	UID                 string
	NodeFingerprint     identity.Fingerprint
	AgentProfileDigest  string
	WorkspaceGeneration uint64
}

// Lock is the workspace's optional write-lock. Zero owner ⇒ unlocked.
type Lock struct {
	OwnerFingerprint identity.Fingerprint
	ExpiresAt        time.Time
}

// IsHeld reports whether the lock has an owner set. Used by handlers
// to decide whether to render the expiry / refuse a mutation.
func (l Lock) IsHeld() bool { return !l.OwnerFingerprint.IsZero() }

// IsExpired reports whether a held lock has passed its expiry.
// An unheld lock is never expired (callers check IsHeld first).
// Zero ExpiresAt is treated as "never expires" — the lock blocks
// mutations indefinitely until Release is called.
func (l Lock) IsExpired(now time.Time) bool {
	if l.ExpiresAt.IsZero() {
		return false
	}
	return !now.Before(l.ExpiresAt)
}

// HeldBy reports whether the lock is currently held by fp. A lock
// that's past its expiry is treated as unheld so a stale refresh
// from the prior holder doesn't block a new acquirer.
func (l Lock) HeldBy(fp identity.Fingerprint, now time.Time) bool {
	if !l.IsHeld() || l.IsExpired(now) {
		return false
	}
	return l.OwnerFingerprint == fp
}

// HeldByOther reports whether the lock is held by someone other
// than fp. An unheld or expired lock returns false (the caller
// can acquire / mutate).
func (l Lock) HeldByOther(fp identity.Fingerprint, now time.Time) bool {
	if !l.IsHeld() || l.IsExpired(now) {
		return false
	}
	return l.OwnerFingerprint != fp
}

// Metadata is the identity + auditing half of a Workspace.
// Immutable identity fields (UUID, OwnerFingerprint, CreatedAt) are
// stamped at Create time; mutable fields (Name, Generation,
// UpdatedAt) move with the workspace's life. Generation increments
// on every successful Spec change; the reconciler compares it
// against Status.ObservedGeneration to detect "edit pending restart".
type Metadata struct {
	UUID             string
	Name             string
	OwnerFingerprint identity.Fingerprint
	Generation       uint64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Spec is the user-declared workspace state. Editing any field bumps
// Metadata.Generation.
type Spec struct {
	ImageRef       string
	PendingBuildID string
	// DevcontainerPath is a path to a devcontainer.json WITHIN the
	// workspace content (e.g. ".devcontainer/devcontainer.json"), not a
	// host path. At Launch the orchestrator reads it from the pushed
	// content, projects it to a base spec, and overlays these explicit
	// spec fields on top (explicit wins). Empty ⇒ no devcontainer base.
	// See PLAN.md D6c.5 and internal/devcontainerspec.
	DevcontainerPath         string
	Mounts                   []Mount
	Env                      []EnvVar
	Ports                    []PortMapping
	TargetSandboxFingerprint string
	// GpuRequest / Isolation: reserved for D-phase scheduler
	// hookup. Carried on the catalog so the spec→SandboxSpec
	// projection at Launch time stays a direct copy.
	GpuCount   uint32
	GpuMinVRAM uint64
	Isolation  int32 // matches daft.sandbox.v1.Isolation enum integer
	// MemoryBytes / CPUCores: guest VM sizing. Zero ⇒ executor
	// default (256 MiB RAM, half-host-cores). Projected onto the
	// SandboxSpec at Launch, same as the GPU fields above.
	MemoryBytes uint64
	CPUCores    uint32
	// DiskBytes is the retained XFS workspace allowance. Zero selects the
	// Node's 20 GiB default.
	DiskBytes uint64
	// PreserveImageCommand opts out of the persistent Workspace keepalive.
	PreserveImageCommand bool
	// NestedVirt grants the workload nested-virtualisation access
	// (/dev/kvm bind + cgroup device-allow). False ⇒ disabled.
	// Projected onto SandboxSpec.nested_virtualization at Launch.
	NestedVirt bool
	// PrivilegedFuseMounts grants the workload FUSE-mount access
	// (/dev/fuse bind + cgroup device-allow + CAP_SYS_ADMIN). False ⇒
	// disabled. Projected onto SandboxSpec.privileged_fuse_mounts at
	// Launch.
	PrivilegedFuseMounts bool
	// PrivilegedBuild grants the workload the standard container
	// capability set so an in-sandbox buildah can build images. False ⇒
	// disabled (the zero-cap default). Projected onto
	// SandboxSpec.privileged_build at Launch.
	PrivilegedBuild bool
}

// Status is the controller-observed workspace state.
type Status struct {
	ObservedGeneration  uint64
	Conditions          []Condition
	CurrentSandbox      *SandboxRef
	Missing             []string
	Lock                Lock
	DiskUsedBytes       uint64
	DiskAllowanceBytes  uint64
	ProtectedBytes      uint64
	ManifestDigest      string
	ManifestGeneration  uint64
	LastTerminalSandbox *TerminalSandbox
}

// TerminalSandbox is the last bounded terminal observation for a Workspace.
// Unlike CurrentSandbox it is durable diagnostic history, not a live binding.
type TerminalSandbox struct {
	UID                 string
	NodeFingerprint     identity.Fingerprint
	WorkspaceGeneration uint64
	Phase               string
	ExitCode            int32
	Message             string
	ObservedAt          time.Time
}

// Condition returns the entry with matching type, or the zero
// Condition if absent. Handlers compose this with status.IsTrue
// to gate transitions.
func (s Status) Condition(typ string) Condition {
	for _, c := range s.Conditions {
		if c.Type == typ {
			return c
		}
	}
	return Condition{}
}

// IsTrue reports whether the named condition is present and set
// to True. Unknown / Unspecified / absent conditions all return
// false — handlers gate on positive proof of readiness.
func (s Status) IsTrue(typ string) bool {
	return s.Condition(typ).Status == ConditionStatusTrue
}

// Workspace is the catalog's record of a single workspace. The
// three sub-structs mirror the wire shape exactly so the
// Connect-RPC projection (workspaceToProto) is a flat copy.
type Workspace struct {
	Metadata Metadata
	Spec     Spec
	Status   Status
}

// Sentinel errors. ErrAliasInUse lets the Connect adapter map a
// duplicate (owner, name) to AlreadyExists; ErrNotFound maps to
// NotFound; ErrImmutableField lets callers detect attempts to
// mutate identity fields without wrapping every Update result.
var (
	ErrNotFound           = errors.New("workspacecatalog: workspace not found")
	ErrNotOwner           = errors.New("workspacecatalog: workspace belongs to another owner")
	ErrAliasInUse         = errors.New("workspacecatalog: name already used by this owner")
	ErrImmutableField     = errors.New("workspacecatalog: refusing to mutate immutable field")
	ErrGenerationConflict = errors.New("workspacecatalog: generation precondition failed")
	ErrGenerationOverflow = errors.New("workspacecatalog: generation exhausted")
	ErrClosed             = errors.New("workspacecatalog: store closed")
	// ErrLockHeldByOther is returned by mutators when the
	// workspace lock is held by an identity other than the caller.
	// Maps to FailedPrecondition at the Connect surface.
	ErrLockHeldByOther = errors.New("workspacecatalog: workspace locked by another caller")
)

// Store holds the live set of workspaces. Safe for concurrent use.
// All public methods take the mutex; reads return value copies so
// callers can never observe in-place mutation.
type Store struct {
	now func() time.Time

	mu      sync.Mutex
	all     map[string]Workspace
	backend workspaceBackend
	closed  bool
	// alias indexes (owner, name) → uuid; populated on Create and
	// kept consistent on every Update / Delete.
	byAlias   map[aliasKey]string
	revision  uint64
	nextWatch uint64
	watchers  map[uint64]chan uint64
}

type workspaceBackend interface {
	Save(Workspace) error
	Delete(string) error
	Close() error
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
		now:      now,
		all:      map[string]Workspace{},
		byAlias:  map[aliasKey]string{},
		watchers: map[uint64]chan uint64{},
	}
}

// Watch reports successful catalog changes. Notifications are coalesced when
// a reader is slow; consumers resnapshot instead of treating this as an event log.
func (s *Store) Watch() (changes <-chan uint64, stop func()) {
	s.mu.Lock()
	id := s.nextWatch
	s.nextWatch++
	ch := make(chan uint64, 1)
	s.watchers[id] = ch
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.watchers, id)
		s.mu.Unlock()
	}
}

func (s *Store) notifyLocked() {
	s.revision++
	for _, ch := range s.watchers {
		select {
		case ch <- s.revision:
		default:
		}
	}
}

func newStore(now func() time.Time, backend workspaceBackend) *Store {
	s := NewStore(now)
	s.backend = backend
	return s
}

// CreateInput is the new-workspace shape. UUID is supplied by the
// caller so the Connect adapter can echo the same value back to
// the client without a second RPC. Empty UUID or empty Name is
// rejected.
type CreateInput struct {
	UUID             string
	Name             string
	OwnerFingerprint identity.Fingerprint
}

// Create inserts a new workspace. Returns ErrAliasInUse if (Owner,
// Name) already exists. The new workspace starts with Generation=1,
// no conditions True, and Missing listing the still-empty required
// spec fields (image, target, at least one mount). Validate /
// mutation calls compute the Ready condition from Missing[].
func (s *Store) Create(in CreateInput) (Workspace, error) {
	assert.True(in.UUID != "",
		"workspacecatalog.Create requires a non-empty UUID; the caller mints one so the Connect adapter can echo it back without a second round-trip")
	assert.True(in.Name != "",
		"workspacecatalog.Create requires a non-empty Name; default basename(pwd) on the user-host before calling")
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Workspace{}, ErrClosed
	}
	if _, dup := s.all[in.UUID]; dup {
		return Workspace{}, fmt.Errorf("workspacecatalog: uuid %q already exists", in.UUID)
	}
	key := aliasKey{owner: in.OwnerFingerprint, name: in.Name}
	if _, dup := s.byAlias[key]; dup {
		return Workspace{}, ErrAliasInUse
	}
	ws := Workspace{
		Metadata: Metadata{
			UUID:             in.UUID,
			Name:             in.Name,
			OwnerFingerprint: in.OwnerFingerprint,
			Generation:       1,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
	}
	ws.Status.Missing = missingFields(ws.Spec)
	ws.Status.Conditions = recomputeConditions(nil, ws.Status.Missing, false, now)
	if s.backend != nil {
		if err := s.backend.Save(ws); err != nil {
			return Workspace{}, fmt.Errorf("workspacecatalog: persist create: %w", err)
		}
	}
	s.all[in.UUID] = ws
	s.byAlias[key] = in.UUID
	s.notifyLocked()
	return cloneWorkspace(ws), nil
}

// Get returns the workspace identified by uuid. The returned value
// is a shallow copy; the backing slice arrays are shared, so callers
// MUST treat the result as read-only.
func (s *Store) Get(uuid string) (Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Workspace{}, ErrClosed
	}
	ws, ok := s.all[uuid]
	if !ok {
		return Workspace{}, ErrNotFound
	}
	return cloneWorkspace(ws), nil
}

// LookupByAlias returns the workspace whose (owner, name) matches.
// Used by the CLI's "address by name" path before any mutation.
func (s *Store) LookupByAlias(owner identity.Fingerprint, name string) (Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Workspace{}, ErrClosed
	}
	uuid, ok := s.byAlias[aliasKey{owner: owner, name: name}]
	if !ok {
		return Workspace{}, ErrNotFound
	}
	return cloneWorkspace(s.all[uuid]), nil
}

// MutateKind tells Update whether a callback's edit is a spec
// change (bumps Generation, re-derives Missing + Ready), a
// status-only change (e.g. setting Dispatched after Launch), or
// metadata-only (e.g. renaming). The Store handles the derived
// state correspondingly so callers don't have to remember the
// bookkeeping.
type MutateKind uint8

const (
	// MutateSpec: fn edits Spec fields. Generation increments,
	// Missing is recomputed, Ready condition is refreshed.
	MutateSpec MutateKind = iota
	// MutateStatus: fn edits Status only (e.g. set Dispatched,
	// clear CurrentSandbox). Generation does not change.
	MutateStatus
	// MutateMetadata: fn edits Metadata only (e.g. rename).
	// Generation does not change.
	MutateMetadata
)

// Update applies fn to a copy of the workspace and stores the
// result. fn MUST NOT modify identity fields (UUID,
// OwnerFingerprint, CreatedAt) — the Store re-asserts the originals
// on return and reports ErrImmutableField if they changed.
//
// Update under a nil fn is a no-op that still bumps UpdatedAt —
// useful for touch-style refreshes (lock renewal) without a
// separate API.
func (s *Store) Update(uuid string, kind MutateKind, fn func(*Workspace) error) (Workspace, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Workspace{}, ErrClosed
	}
	prev, ok := s.all[uuid]
	if !ok {
		return Workspace{}, ErrNotFound
	}
	next := cloneWorkspace(prev)
	if fn != nil {
		if err := fn(&next); err != nil {
			return Workspace{}, err
		}
	}
	if next.Metadata.UUID != prev.Metadata.UUID ||
		next.Metadata.OwnerFingerprint != prev.Metadata.OwnerFingerprint ||
		!next.Metadata.CreatedAt.Equal(prev.Metadata.CreatedAt) {
		return Workspace{}, ErrImmutableField
	}
	if next.Metadata.Name != prev.Metadata.Name {
		newKey := aliasKey{owner: prev.Metadata.OwnerFingerprint, name: next.Metadata.Name}
		if _, dup := s.byAlias[newKey]; dup {
			return Workspace{}, ErrAliasInUse
		}
	}
	if kind == MutateSpec {
		next.Metadata.Generation++
		next.Status.Missing = missingFields(next.Spec)
		next.Status.Conditions = recomputeConditions(next.Status.Conditions, next.Status.Missing, next.Status.IsTrue(ConditionDispatched), now)
	}
	next.Metadata.UpdatedAt = now
	if s.backend != nil {
		if err := s.backend.Save(next); err != nil {
			return Workspace{}, fmt.Errorf("workspacecatalog: persist update: %w", err)
		}
	}
	if next.Metadata.Name != prev.Metadata.Name {
		oldKey := aliasKey{owner: prev.Metadata.OwnerFingerprint, name: prev.Metadata.Name}
		newKey := aliasKey{owner: prev.Metadata.OwnerFingerprint, name: next.Metadata.Name}
		delete(s.byAlias, oldKey)
		s.byAlias[newKey] = uuid
	}
	s.all[uuid] = next
	s.notifyLocked()
	return cloneWorkspace(next), nil
}

// ApplySpec atomically replaces the complete declared spec when the caller
// owns the Workspace, no foreign write lock is active, and the generation
// precondition matches. Reapplying an identical spec is a no-op even when
// expectedGeneration is stale, making a lost successful response safe to
// retry without incrementing Generation twice.
func (s *Store) ApplySpec(
	uuid string,
	owner identity.Fingerprint,
	expectedGeneration uint64,
	desired Spec,
) (Workspace, bool, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Workspace{}, false, ErrClosed
	}
	prev, ok := s.all[uuid]
	if !ok {
		return Workspace{}, false, ErrNotFound
	}
	if prev.Metadata.OwnerFingerprint != owner {
		return Workspace{}, false, ErrNotOwner
	}
	if prev.Status.Lock.HeldByOther(owner, now) {
		return Workspace{}, false, ErrLockHeldByOther
	}
	desired = materializeSpec(desired, uuid)
	if specsEqual(prev.Spec, desired) {
		return cloneWorkspace(prev), false, nil
	}
	if expectedGeneration == 0 || prev.Metadata.Generation != expectedGeneration {
		return Workspace{}, false, ErrGenerationConflict
	}
	if prev.Metadata.Generation == math.MaxUint64 {
		return Workspace{}, false, ErrGenerationOverflow
	}
	next := appliedWorkspace(prev, desired, now)
	if s.backend != nil {
		if err := s.backend.Save(next); err != nil {
			return Workspace{}, false, fmt.Errorf("workspacecatalog: persist apply: %w", err)
		}
	}
	s.all[uuid] = next
	s.notifyLocked()
	return cloneWorkspace(next), true, nil
}

// ApplySpecByName creates or updates one owner-scoped alias atomically. A zero
// expected generation means create only; retrying that create with an identical
// declaration succeeds unchanged. uuid is used only if creation wins.
func (s *Store) ApplySpecByName(
	uuid string,
	name string,
	owner identity.Fingerprint,
	expectedGeneration uint64,
	desired Spec,
) (workspace Workspace, created, changed bool, err error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Workspace{}, false, false, ErrClosed
	}
	key := aliasKey{owner: owner, name: name}
	if existingUUID, ok := s.byAlias[key]; ok {
		prev := s.all[existingUUID]
		desired = materializeSpec(desired, existingUUID)
		if prev.Status.Lock.HeldByOther(owner, now) {
			return Workspace{}, false, false, ErrLockHeldByOther
		}
		if specsEqual(prev.Spec, desired) {
			return cloneWorkspace(prev), false, false, nil
		}
		if expectedGeneration == 0 || prev.Metadata.Generation != expectedGeneration {
			return Workspace{}, false, false, ErrGenerationConflict
		}
		if prev.Metadata.Generation == math.MaxUint64 {
			return Workspace{}, false, false, ErrGenerationOverflow
		}
		next := appliedWorkspace(prev, desired, now)
		if s.backend != nil {
			if err := s.backend.Save(next); err != nil {
				return Workspace{}, false, false, fmt.Errorf("workspacecatalog: persist apply: %w", err)
			}
		}
		s.all[existingUUID] = next
		s.notifyLocked()
		return cloneWorkspace(next), false, true, nil
	}
	if expectedGeneration != 0 {
		return Workspace{}, false, false, ErrNotFound
	}
	if uuid == "" || name == "" {
		return Workspace{}, false, false, ErrNotFound
	}
	if _, exists := s.all[uuid]; exists {
		return Workspace{}, false, false, fmt.Errorf("workspacecatalog: uuid %q already exists", uuid)
	}
	desired = materializeSpec(desired, uuid)
	ws := Workspace{Metadata: Metadata{
		UUID: uuid, Name: name, OwnerFingerprint: owner, Generation: 1,
		CreatedAt: now, UpdatedAt: now,
	}, Spec: cloneSpec(desired)}
	ws.Status.Missing = missingFields(ws.Spec)
	ws.Status.Conditions = recomputeConditions(nil, ws.Status.Missing, false, now)
	if s.backend != nil {
		if err := s.backend.Save(ws); err != nil {
			return Workspace{}, false, false, fmt.Errorf("workspacecatalog: persist apply create: %w", err)
		}
	}
	s.all[uuid] = ws
	s.byAlias[key] = uuid
	s.notifyLocked()
	return cloneWorkspace(ws), true, true, nil
}

func appliedWorkspace(prev Workspace, desired Spec, now time.Time) Workspace {
	next := cloneWorkspace(prev)
	next.Spec = cloneSpec(desired)
	next.Metadata.Generation++
	next.Metadata.UpdatedAt = now
	next.Status.Missing = missingFields(next.Spec)
	next.Status.Conditions = recomputeConditions(next.Status.Conditions, next.Status.Missing, next.Status.IsTrue(ConditionDispatched), now)
	return next
}

func materializeSpec(in Spec, workspaceUUID string) Spec {
	in = cloneSpec(in)
	for i := range in.Mounts {
		if in.Mounts[i].Source == "" {
			in.Mounts[i].Source = workspaceUUID
		}
	}
	return in
}

func specsEqual(a, b Spec) bool {
	return a.ImageRef == b.ImageRef && a.PendingBuildID == b.PendingBuildID && a.DevcontainerPath == b.DevcontainerPath &&
		slices.Equal(a.Mounts, b.Mounts) && slices.Equal(a.Env, b.Env) && slices.Equal(a.Ports, b.Ports) &&
		a.TargetSandboxFingerprint == b.TargetSandboxFingerprint && a.GpuCount == b.GpuCount && a.GpuMinVRAM == b.GpuMinVRAM &&
		a.Isolation == b.Isolation && a.MemoryBytes == b.MemoryBytes && a.CPUCores == b.CPUCores && a.DiskBytes == b.DiskBytes && a.NestedVirt == b.NestedVirt &&
		a.PrivilegedFuseMounts == b.PrivilegedFuseMounts && a.PrivilegedBuild == b.PrivilegedBuild
}

func cloneSpec(in Spec) Spec {
	in.Mounts = slices.Clone(in.Mounts)
	in.Env = slices.Clone(in.Env)
	in.Ports = slices.Clone(in.Ports)
	return in
}

// Delete removes the workspace if present. Idempotent.
func (s *Store) Delete(uuid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	prev, ok := s.all[uuid]
	if !ok {
		return nil
	}
	if s.backend != nil {
		if err := s.backend.Delete(uuid); err != nil {
			return fmt.Errorf("workspacecatalog: persist delete: %w", err)
		}
	}
	delete(s.all, uuid)
	delete(s.byAlias, aliasKey{owner: prev.Metadata.OwnerFingerprint, name: prev.Metadata.Name})
	s.notifyLocked()
	return nil
}

// List returns every workspace satisfying keep (or every workspace
// if keep is nil), sorted by UUID. The mutex is released before
// sorting since the returned slice is private to the caller. Push
// the predicate into the store so we don't allocate copies of
// records that the caller is about to discard.
func (s *Store) List(keep func(*Workspace) bool) []Workspace {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	out := make([]Workspace, 0, len(s.all))
	for uuid := range s.all {
		ws := s.all[uuid]
		if keep != nil && !keep(&ws) {
			continue
		}
		out = append(out, cloneWorkspace(ws))
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b Workspace) int { return cmp.Compare(a.Metadata.UUID, b.Metadata.UUID) })
	return out
}

// Close flushes and closes a persistent Store. It is safe to call more than
// once. In-memory stores have nothing to close.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	backend := s.backend
	s.mu.Unlock()
	if backend == nil {
		return nil
	}
	if err := backend.Close(); err != nil {
		return fmt.Errorf("workspacecatalog: close: %w", err)
	}
	return nil
}

func cloneWorkspace(ws Workspace) Workspace {
	ws.Spec.Mounts = slices.Clone(ws.Spec.Mounts)
	ws.Spec.Env = slices.Clone(ws.Spec.Env)
	ws.Spec.Ports = slices.Clone(ws.Spec.Ports)
	ws.Status.Conditions = slices.Clone(ws.Status.Conditions)
	ws.Status.Missing = slices.Clone(ws.Status.Missing)
	if ws.Status.CurrentSandbox != nil {
		ref := *ws.Status.CurrentSandbox
		ws.Status.CurrentSandbox = &ref
	}
	if ws.Status.LastTerminalSandbox != nil {
		terminal := *ws.Status.LastTerminalSandbox
		ws.Status.LastTerminalSandbox = &terminal
	}
	return ws
}

// missingFields returns the human-readable list of required spec
// fields that are still unset. Drives the Ready condition's reason.
// The required set: at least one Mount, exactly one of ImageRef or
// PendingBuildID, and a target (TargetSandboxFingerprint). The CLI
// uses this list verbatim, so the strings are user-facing.
//
// A DevcontainerPath provides the image and the workspace-content mount
// itself — but only the node can read it (the content lives there), so
// the orchestrator trusts it provisionally here and lets the node
// resolve for real at dispatch. Hence a devcontainer satisfies both the
// "image" and "mount" requirements; "target" is still required (it's the
// node the content must be read on).
func missingFields(spec Spec) []string {
	var out []string
	hasDevcontainer := spec.DevcontainerPath != ""
	if len(spec.Mounts) == 0 && !hasDevcontainer {
		out = append(out, "mount")
	}
	if spec.ImageRef == "" && spec.PendingBuildID == "" && !hasDevcontainer {
		out = append(out, "image")
	}
	if spec.TargetSandboxFingerprint == "" {
		out = append(out, MissingTarget)
	}
	return out
}

// recomputeConditions refreshes the spec-derived conditions (Ready
// today, ImageResolved when E lands) without disturbing those the
// reconciler owns (Dispatched, Reconciled). Pass dispatched so the
// Ready condition can include a reason when Launch is blocked by
// an in-flight Sandbox.
//
// Callers usually pass prev (the existing slice) so transitions
// preserve LastTransitionTime when the bit didn't actually flip.
func recomputeConditions(prev []Condition, missing []string, dispatched bool, now time.Time) []Condition {
	out := make([]Condition, 0, len(prev)+1)
	for _, c := range prev {
		// Carry forward conditions the reconciler owns.
		if c.Type == ConditionDispatched || c.Type == ConditionReconciled || c.Type == ConditionImageResolved {
			out = append(out, c)
		}
	}
	ready := setReady(prev, missing, dispatched, now)
	out = append(out, ready)
	return out
}

// setReady computes the Ready condition. Preserves LastTransitionTime
// when the status bit didn't change from the prior value.
func setReady(prev []Condition, missing []string, dispatched bool, now time.Time) Condition {
	want := ConditionStatusTrue
	reason := "Ready"
	message := ""
	switch {
	case len(missing) > 0:
		want = ConditionStatusFalse
		reason = "MissingFields"
		message = fmt.Sprintf("missing: %v", missing)
	case dispatched:
		// Spec is satisfied but a Sandbox is in flight; clients
		// still see Ready=True (the workspace itself is ready),
		// only Launch is gated by Dispatched separately.
	}
	cond := Condition{Type: ConditionReady, Status: want, Reason: reason, Message: message, LastTransitionTime: now}
	for _, p := range prev {
		if p.Type == ConditionReady && p.Status == want {
			cond.LastTransitionTime = p.LastTransitionTime
			break
		}
	}
	return cond
}

// SetCondition replaces (or appends) a single condition by Type,
// preserving LastTransitionTime when Status doesn't change.
// Handlers use this to set Dispatched after Launch / clear it on
// Stop / and (B5) for the reconciler-owned Reconciled bit.
func (s *Status) SetCondition(typ string, status ConditionStatus, reason, message string, now time.Time) {
	for i, c := range s.Conditions {
		if c.Type != typ {
			continue
		}
		next := Condition{Type: typ, Status: status, Reason: reason, Message: message, LastTransitionTime: now}
		if c.Status == status {
			next.LastTransitionTime = c.LastTransitionTime
		}
		s.Conditions[i] = next
		return
	}
	s.Conditions = append(s.Conditions, Condition{
		Type:               typ,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}
