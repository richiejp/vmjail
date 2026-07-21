// Package agentruncatalog persists overseer-started agent runs and their
// state-transition events in the orchestrator control store.
package agentruncatalog

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/richiejp/darkaifactory/internal/identity"
)

const (
	recordVersion   = 1
	runPrefix       = "agent-run/"
	eventPrefix     = "agent-run-event/"
	MaxPromptBytes  = 256 << 10
	MaxMessageBytes = 16 << 10
	MaxResultBytes  = 1 << 20
	MaxIDBytes      = 128
	MaxCommandArgs  = 64
	MaxCommandBytes = 16 << 10
	// MaxEventsPerRun is a structural bound: the acyclic state machine permits
	// creation plus at most four transitions before reaching a terminal state.
	MaxEventsPerRun = 5
)

type State string

const (
	StateQueued State = "QUEUED"
	// StateAwaitingCredential has a Sandbox binding but grants no permission to
	// start the provider process. Only an acknowledged one-shot delivery may
	// advance it to StateStarting.
	StateAwaitingCredential State = "AWAITING_CREDENTIAL" //nolint:gosec // state label, not credential material.
	StateStarting           State = "STARTING"
	StateRunning            State = "RUNNING"
	StateCanceling          State = "CANCELING"
	StateSucceeded          State = "SUCCEEDED"
	StateFailed             State = "FAILED"
	StateCanceled           State = "CANCELED"
	StateLost               State = "LOST"
)

const CredentialProviderCodex = "codex"

func (s State) Terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCanceled || s == StateLost
}

type Run struct {
	UUID                 string               `json:"uuid"`
	OwnerFingerprint     identity.Fingerprint `json:"owner_fingerprint"`
	WorkspaceUUID        string               `json:"workspace_uuid"`
	ProfileName          string               `json:"profile_name"`
	ProfileDigest        string               `json:"profile_digest"`
	CredentialRequired   bool                 `json:"credential_required,omitempty"`
	CredentialProvider   string               `json:"credential_provider,omitempty"`
	CredentialDeliveryID string               `json:"credential_delivery_id,omitempty"`
	Prompt               string               `json:"prompt"`
	Command              []string             `json:"command"`
	State                State                `json:"state"`
	SandboxUID           string               `json:"sandbox_uid,omitempty"`
	NodeFingerprint      identity.Fingerprint `json:"node_fingerprint,omitempty"`
	NodeInstanceID       string               `json:"node_instance_id,omitempty"`
	NodeJobID            string               `json:"node_job_id,omitempty"`
	CreatedAt            time.Time            `json:"created_at"`
	UpdatedAt            time.Time            `json:"updated_at"`
	StartedAt            time.Time            `json:"started_at,omitempty"`
	CompletedAt          time.Time            `json:"completed_at,omitempty"`
	ExitCode             int32                `json:"exit_code,omitempty"`
	ExitCodeSet          bool                 `json:"exit_code_set,omitempty"`
	Reason               string               `json:"reason,omitempty"`
	Result               string               `json:"result,omitempty"`
	TeardownPending      bool                 `json:"teardown_pending,omitempty"`
	LastEventCursor      uint64               `json:"last_event_cursor"`
}

type Event struct {
	RunUUID   string    `json:"run_uuid"`
	Cursor    uint64    `json:"cursor"`
	Timestamp time.Time `json:"timestamp"`
	State     State     `json:"state"`
	Message   string    `json:"message,omitempty"`
}

type CreateInput struct {
	UUID, WorkspaceUUID, ProfileName, ProfileDigest, Prompt, CredentialProvider string
	Command                                                                     []string
	OwnerFingerprint                                                            identity.Fingerprint
	CredentialRequired                                                          bool
}

type TransitionInput struct {
	From                                                                                 []State
	To                                                                                   State
	Message, SandboxUID, NodeInstanceID, NodeJobID, CredentialDeliveryID, Reason, Result string
	NodeFingerprint                                                                      identity.Fingerprint
	ExitCode                                                                             int32
	ExitCodeSet                                                                          bool
}

var (
	ErrNotFound = errors.New("agentruncatalog: run not found")
	ErrNotOwner = errors.New("agentruncatalog: run belongs to another owner")
	ErrConflict = errors.New("agentruncatalog: state transition conflict")
	ErrInvalid  = errors.New("agentruncatalog: invalid run")
)

type persisted[T any] struct {
	Version uint32 `json:"version"`
	Value   T      `json:"value"`
}

type Store struct {
	db   *badger.DB
	now  func() time.Time
	mu   sync.Mutex
	runs map[string]Run
}

func Open(db *badger.DB, now func() time.Time) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: nil control database", ErrInvalid)
	}
	if now == nil {
		now = time.Now
	}
	s := &Store{db: db, now: now, runs: map[string]Run{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Create(in CreateInput) (Run, error) {
	if in.UUID == "" || in.WorkspaceUUID == "" || in.ProfileName == "" || in.ProfileDigest == "" || in.OwnerFingerprint.IsZero() || len(in.UUID) > MaxIDBytes || len(in.WorkspaceUUID) > MaxIDBytes {
		return Run{}, fmt.Errorf("%w: required create field is empty", ErrInvalid)
	}
	if len(in.Prompt) > MaxPromptBytes {
		return Run{}, fmt.Errorf("%w: prompt exceeds %d bytes", ErrInvalid, MaxPromptBytes)
	}
	if err := validateCommand(in.Command); err != nil {
		return Run{}, err
	}
	now := s.now().UTC()
	if in.CredentialRequired != (in.CredentialProvider != "") || (in.CredentialProvider != "" && in.CredentialProvider != CredentialProviderCodex) {
		return Run{}, fmt.Errorf("%w: credential provider must be codex exactly when credentials are required", ErrInvalid)
	}
	r := Run{UUID: in.UUID, OwnerFingerprint: in.OwnerFingerprint, WorkspaceUUID: in.WorkspaceUUID, ProfileName: in.ProfileName, ProfileDigest: in.ProfileDigest, CredentialRequired: in.CredentialRequired, CredentialProvider: in.CredentialProvider, Prompt: in.Prompt, Command: slices.Clone(in.Command), State: StateQueued, CreatedAt: now, UpdatedAt: now, LastEventCursor: 1}
	e := Event{RunUUID: r.UUID, Cursor: 1, Timestamp: now, State: r.State, Message: "run queued"}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[r.UUID]; ok {
		return Run{}, fmt.Errorf("%w: uuid already exists", ErrConflict)
	}
	if err := s.write(r, e, true); err != nil {
		return Run{}, err
	}
	s.runs[r.UUID] = r
	return cloneRun(r), nil
}

func (s *Store) Get(owner identity.Fingerprint, uuid string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[uuid]
	if !ok {
		return Run{}, ErrNotFound
	}
	if r.OwnerFingerprint != owner {
		return Run{}, ErrNotOwner
	}
	return cloneRun(r), nil
}

func (s *Store) List(owner identity.Fingerprint) []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Run, 0)
	for uuid := range s.runs {
		r := s.runs[uuid]
		if r.OwnerFingerprint == owner {
			out = append(out, cloneRun(r))
		}
	}
	slices.SortFunc(out, func(a, b Run) int { return cmp.Compare(a.UUID, b.UUID) })
	return out
}

// Active returns non-terminal runs plus terminal runs whose Sandbox teardown
// is still durably pending. It is intentionally not owner-scoped: only the
// internal reconciliation controller uses it.
func (s *Store) Active() []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Run, 0)
	for uuid := range s.runs {
		run := s.runs[uuid]
		if !run.State.Terminal() || run.TeardownPending {
			out = append(out, cloneRun(run))
		}
	}
	slices.SortFunc(out, func(a, b Run) int { return cmp.Compare(a.UUID, b.UUID) })
	return out
}

func (s *Store) Events(owner identity.Fingerprint, uuid string, after uint64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 200 {
		return nil, fmt.Errorf("%w: event limit must be 1..200", ErrInvalid)
	}
	if _, err := s.Get(owner, uuid); err != nil {
		return nil, err
	}
	out := make([]Event, 0, limit)
	err := s.db.View(func(txn *badger.Txn) error {
		prefix := []byte(eventPrefix + uuid + "/")
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()
		seek := []byte(fmt.Sprintf("%s%020d", prefix, after+1))
		for it.Seek(seek); it.ValidForPrefix(prefix) && len(out) < limit; it.Next() {
			var p persisted[Event]
			if err := decodeItem(it.Item(), &p); err != nil {
				return err
			}
			if p.Version != recordVersion || p.Value.RunUUID != uuid {
				return fmt.Errorf("agentruncatalog: corrupt event record")
			}
			out = append(out, p.Value)
		}
		return nil
	})
	return out, err
}

func (s *Store) Transition(owner identity.Fingerprint, uuid string, in TransitionInput) (Run, error) {
	if len(in.Message) > MaxMessageBytes || len(in.Result) > MaxResultBytes {
		return Run{}, fmt.Errorf("%w: transition body exceeds limit", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.runs[uuid]
	if !ok {
		return Run{}, ErrNotFound
	}
	if prev.OwnerFingerprint != owner {
		return Run{}, ErrNotOwner
	}
	if prev.State.Terminal() || !slices.Contains(in.From, prev.State) || !validTransition(prev.State, in.To) {
		return Run{}, ErrConflict
	}
	next := prev
	now := s.now().UTC()
	next.State = in.To
	next.UpdatedAt = now
	next.LastEventCursor++
	if next.LastEventCursor > MaxEventsPerRun {
		return Run{}, fmt.Errorf("%w: event retention invariant exceeded", ErrInvalid)
	}
	if in.SandboxUID != "" {
		next.SandboxUID = in.SandboxUID
	}
	if !in.NodeFingerprint.IsZero() {
		next.NodeFingerprint = in.NodeFingerprint
	}
	if in.NodeInstanceID != "" {
		next.NodeInstanceID = in.NodeInstanceID
	}
	if in.NodeJobID != "" {
		next.NodeJobID = in.NodeJobID
	}
	if in.CredentialDeliveryID != "" {
		if next.CredentialDeliveryID != "" && next.CredentialDeliveryID != in.CredentialDeliveryID {
			return Run{}, fmt.Errorf("%w: credential delivery ID is immutable", ErrInvalid)
		}
		next.CredentialDeliveryID = in.CredentialDeliveryID
	}
	if next.State == StateRunning && next.StartedAt.IsZero() {
		next.StartedAt = now
	}
	if next.State.Terminal() {
		next.CompletedAt = now
		next.ExitCode = in.ExitCode
		next.ExitCodeSet = in.ExitCodeSet
		next.Reason = in.Reason
		next.Result = in.Result
		next.TeardownPending = next.SandboxUID != ""
	}
	if err := validateBindings(next); err != nil {
		return Run{}, err
	}
	e := Event{RunUUID: uuid, Cursor: next.LastEventCursor, Timestamp: now, State: next.State, Message: in.Message}
	if err := s.write(next, e, false); err != nil {
		return Run{}, err
	}
	s.runs[uuid] = next
	return cloneRun(next), nil
}

// CompleteTeardown clears the durable retry marker without appending a state
// event. The externally visible AgentRun state/result are already terminal and
// immutable; this transaction records completion of its internal cleanup.
func (s *Store) CompleteTeardown(owner identity.Fingerprint, uuid string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[uuid]
	if !ok {
		return Run{}, ErrNotFound
	}
	if run.OwnerFingerprint != owner {
		return Run{}, ErrNotOwner
	}
	if !run.State.Terminal() || !run.TeardownPending {
		return Run{}, ErrConflict
	}
	run.TeardownPending = false
	run.UpdatedAt = s.now().UTC()
	encoded, err := encodeJSON(persisted[Run]{Version: recordVersion, Value: run})
	if err != nil {
		return Run{}, err
	}
	if err := s.db.Update(func(txn *badger.Txn) error { return txn.Set([]byte(runPrefix+run.UUID), encoded) }); err != nil {
		return Run{}, err
	}
	s.runs[uuid] = run
	return cloneRun(run), nil
}

func cloneRun(run Run) Run {
	run.Command = slices.Clone(run.Command)
	return run
}

func validateCommand(command []string) error {
	if len(command) == 0 || len(command) > MaxCommandArgs {
		return fmt.Errorf("%w: command must contain 1..%d arguments", ErrInvalid, MaxCommandArgs)
	}
	for _, arg := range command {
		if arg == "" || len(arg) > MaxCommandBytes {
			return fmt.Errorf("%w: command argument is empty or exceeds %d bytes", ErrInvalid, MaxCommandBytes)
		}
	}
	return nil
}

func validTransition(from, to State) bool {
	switch from {
	case StateQueued:
		return to == StateAwaitingCredential || to == StateStarting || to == StateCanceled || to == StateFailed
	case StateAwaitingCredential:
		return to == StateStarting || to == StateCanceled || to == StateFailed || to == StateLost
	case StateStarting:
		return to == StateRunning || to == StateCanceled || to == StateFailed || to == StateLost
	case StateRunning:
		return to == StateCanceling || to == StateSucceeded || to == StateFailed || to == StateLost
	case StateCanceling:
		return to == StateCanceled || to == StateFailed || to == StateLost
	default:
		return false
	}
}

func (s *Store) write(r Run, e Event, create bool) error {
	rv, err := encodeJSON(persisted[Run]{Version: recordVersion, Value: r})
	if err != nil {
		return err
	}
	ev, err := encodeJSON(persisted[Event]{Version: recordVersion, Value: e})
	if err != nil {
		return err
	}
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(runPrefix + r.UUID)
		_, getErr := txn.Get(key)
		if create && getErr == nil {
			return ErrConflict
		}
		if create && !errors.Is(getErr, badger.ErrKeyNotFound) {
			return getErr
		}
		if err := txn.Set(key, rv); err != nil {
			return err
		}
		return txn.Set([]byte(fmt.Sprintf("%s%s/%020d", eventPrefix, r.UUID, e.Cursor)), ev)
	})
}

func encodeJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("agentruncatalog: encode record: %w", err)
	}
	return encoded, nil
}

func (s *Store) load() error {
	return s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(runPrefix)
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			var p persisted[Run]
			if err := decodeItem(it.Item(), &p); err != nil {
				return err
			}
			r := p.Value
			if p.Version != recordVersion || r.UUID == "" || r.WorkspaceUUID == "" || r.ProfileName == "" || r.ProfileDigest == "" || string(it.Item().Key()) != runPrefix+r.UUID || r.OwnerFingerprint.IsZero() || !validState(r.State) || r.LastEventCursor == 0 || r.LastEventCursor > MaxEventsPerRun || len(r.Prompt) > MaxPromptBytes || len(r.Result) > MaxResultBytes {
				return fmt.Errorf("agentruncatalog: invalid run record %q", it.Item().KeyCopy(nil))
			}
			if err := validateBindings(r); err != nil {
				return fmt.Errorf("agentruncatalog: invalid run record %q: %w", it.Item().KeyCopy(nil), err)
			}
			lastKey := []byte(fmt.Sprintf("%s%s/%020d", eventPrefix, r.UUID, r.LastEventCursor))
			last, err := txn.Get(lastKey)
			if err != nil {
				return fmt.Errorf("agentruncatalog: run %q missing last event: %w", r.UUID, err)
			}
			var event persisted[Event]
			if err := decodeItem(last, &event); err != nil || event.Version != recordVersion || event.Value.RunUUID != r.UUID || event.Value.Cursor != r.LastEventCursor || event.Value.State != r.State {
				return fmt.Errorf("agentruncatalog: run %q has invalid last event", r.UUID)
			}
			if _, exists := s.runs[r.UUID]; exists {
				return fmt.Errorf("agentruncatalog: duplicate run %q", r.UUID)
			}
			s.runs[r.UUID] = r
		}
		return nil
	})
}

func validState(s State) bool {
	return s == StateQueued || s == StateAwaitingCredential || s == StateStarting || s == StateRunning || s == StateCanceling || s.Terminal()
}

func validateBindings(r Run) error {
	if r.TeardownPending && (!r.State.Terminal() || r.SandboxUID == "") {
		return fmt.Errorf("%w: teardown marker requires a terminal bound run", ErrInvalid)
	}
	switch r.State {
	case StateAwaitingCredential, StateStarting, StateRunning, StateCanceling, StateSucceeded:
		if r.SandboxUID == "" || r.NodeFingerprint.IsZero() || r.NodeInstanceID == "" {
			return fmt.Errorf("%w: state %s requires sandbox and node binding", ErrInvalid, r.State)
		}
	}
	if r.CredentialRequired != (r.CredentialProvider != "") || (r.CredentialProvider != "" && r.CredentialProvider != CredentialProviderCodex) {
		return fmt.Errorf("%w: invalid credential requirement", ErrInvalid)
	}
	if r.State == StateAwaitingCredential && !r.CredentialRequired {
		return fmt.Errorf("%w: awaiting-credential state requires credentials", ErrInvalid)
	}
	if r.CredentialDeliveryID != "" && (!r.CredentialRequired || r.State == StateQueued || r.State == StateAwaitingCredential) {
		return fmt.Errorf("%w: credential delivery binding is inconsistent", ErrInvalid)
	}
	switch r.State {
	case StateRunning, StateCanceling, StateSucceeded:
		if r.NodeJobID == "" {
			return fmt.Errorf("%w: state %s requires node job id", ErrInvalid, r.State)
		}
	}
	if r.State == StateSucceeded && !r.ExitCodeSet {
		return fmt.Errorf("%w: succeeded run requires exit code", ErrInvalid)
	}
	return nil
}

func decodeItem(item *badger.Item, dst any) error {
	v, err := item.ValueCopy(nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(v, dst); err != nil {
		return fmt.Errorf("agentruncatalog: decode %q: %w", item.KeyCopy(nil), err)
	}
	return nil
}
