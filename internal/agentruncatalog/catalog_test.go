package agentruncatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/richiejp/darkaifactory/internal/controlstore"
	"github.com/richiejp/darkaifactory/internal/identity"
)

const (
	testProfileName   = "codex"
	testProfileDigest = "digest"
	testNodeInstance  = "boot"
	testWorkspaceUUID = "workspace"
	testRunUUID       = "run"
	testSandboxUID    = "sandbox"
	testBadUUID       = "bad"
	testJobID         = "job"
)

func testOwner(b byte) identity.Fingerprint {
	var fp identity.Fingerprint
	fp[0] = b
	return fp
}

func TestRunTransitionsPersistWithEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control")
	control, err := controlstore.Open(path)
	if err != nil {
		t.Fatalf("Open control: %v", err)
	}
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	store, err := Open(control.DB(), func() time.Time { return now })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	owner := testOwner(1)
	run, err := store.Create(CreateInput{UUID: "run-1", WorkspaceUUID: "workspace-1", ProfileName: testProfileName, ProfileDigest: "sha256:profile", Prompt: "fix it", OwnerFingerprint: owner})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	assertCreatedRun(t, run)
	now = now.Add(time.Second)
	run, err = store.Transition(owner, run.UUID, TransitionInput{From: []State{StateQueued}, To: StateStarting, Message: "sandbox assigned", SandboxUID: "sandbox-1", NodeFingerprint: testOwner(2), NodeInstanceID: "boot-1"})
	if err != nil {
		t.Fatalf("starting: %v", err)
	}
	now = now.Add(time.Second)
	run, err = store.Transition(owner, run.UUID, TransitionInput{From: []State{StateStarting}, To: StateRunning, Message: "job started", NodeJobID: "job-1"})
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	now = now.Add(time.Second)
	run, err = store.Transition(owner, run.UUID, TransitionInput{From: []State{StateRunning}, To: StateSucceeded, Message: "finished", ExitCodeSet: true, Result: "done"})
	if err != nil {
		t.Fatalf("succeeded: %v", err)
	}
	if !run.State.Terminal() || !run.ExitCodeSet || run.Result != "done" || run.CompletedAt.IsZero() {
		t.Fatalf("terminal = %#v", run)
	}
	if _, err := store.Transition(owner, run.UUID, TransitionInput{From: []State{StateSucceeded}, To: StateFailed}); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal transition = %v", err)
	}
	events, err := store.Events(owner, run.UUID, 1, 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 3 || events[0].Cursor != 2 || events[2].State != StateSucceeded {
		t.Fatalf("events = %#v", events)
	}
	if err := control.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	control, err = controlstore.Open(path)
	if err != nil {
		t.Fatalf("reopen control: %v", err)
	}
	defer func() {
		if err := control.Close(); err != nil {
			t.Errorf("Close reopened: %v", err)
		}
	}()
	reopened, err := Open(control.DB(), nil)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	got, err := reopened.Get(owner, run.UUID)
	if err != nil {
		t.Fatalf("Get reopened: %v", err)
	}
	if got.State != StateSucceeded || got.LastEventCursor != 4 || got.NodeJobID != "job-1" {
		t.Fatalf("reopened = %#v", got)
	}
}

func assertCreatedRun(t *testing.T, run Run) {
	t.Helper()
	if run.State != StateQueued || run.LastEventCursor != 1 {
		t.Fatalf("created = %#v", run)
	}
}

func TestRunOwnershipValidationAndOrdering(t *testing.T) {
	control, err := controlstore.Open(filepath.Join(t.TempDir(), "control"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := control.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	store, err := Open(control.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	owner, other := testOwner(1), testOwner(2)
	for _, uuid := range []string{"run-b", "run-a"} {
		if _, err := store.Create(CreateInput{UUID: uuid, WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: owner}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Get(other, "run-a"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Get other = %v", err)
	}
	if _, err := store.Events(other, "run-a", 0, 10); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Events other = %v", err)
	}
	if got := store.List(owner); len(got) != 2 || got[0].UUID != "run-a" {
		t.Fatalf("List = %#v", got)
	}
	bad := CreateInput{UUID: testBadUUID, WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: owner, Prompt: string(make([]byte, MaxPromptBytes+1))}
	if _, err := store.Create(bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversize prompt = %v", err)
	}
	if _, err := store.Events(owner, "run-a", 0, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero limit = %v", err)
	}
}

func TestAllLegalAndIllegalTransitions(t *testing.T) {
	legal := map[State][]State{
		StateQueued:    {StateStarting, StateCanceled, StateFailed},
		StateStarting:  {StateRunning, StateCanceled, StateFailed, StateLost},
		StateRunning:   {StateCanceling, StateSucceeded, StateFailed, StateLost},
		StateCanceling: {StateCanceled, StateFailed, StateLost},
	}
	all := []State{StateQueued, StateStarting, StateRunning, StateCanceling, StateSucceeded, StateFailed, StateCanceled, StateLost}
	for _, from := range all {
		for _, to := range all {
			if got, want := validTransition(from, to), slicesContain(legal[from], to); got != want {
				t.Errorf("validTransition(%s,%s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestEventHistoryHasStructuralFiveEventBound(t *testing.T) {
	control, err := controlstore.Open(filepath.Join(t.TempDir(), "control"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Close() }()
	store, err := Open(control.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	owner, node := testOwner(1), testOwner(2)
	run, err := store.Create(CreateInput{UUID: testRunUUID, WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: owner})
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.Transition(owner, run.UUID, TransitionInput{From: []State{StateQueued}, To: StateStarting, SandboxUID: testSandboxUID, NodeFingerprint: node, NodeInstanceID: testNodeInstance})
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.Transition(owner, run.UUID, TransitionInput{From: []State{StateStarting}, To: StateRunning, NodeJobID: testJobID})
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.Transition(owner, run.UUID, TransitionInput{From: []State{StateRunning}, To: StateCanceling})
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.Transition(owner, run.UUID, TransitionInput{From: []State{StateCanceling}, To: StateCanceled})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(owner, run.UUID, 0, MaxEventsPerRun)
	if err != nil || len(events) != MaxEventsPerRun || run.LastEventCursor != MaxEventsPerRun {
		t.Fatalf("events = %#v, run=%#v, err=%v", events, run, err)
	}
}

func TestEncodeJSON(t *testing.T) {
	if got, err := encodeJSON(Event{RunUUID: testRunUUID}); err != nil || len(got) == 0 {
		t.Fatalf("encodeJSON event = %q, %v", got, err)
	}
	if _, err := encodeJSON(make(chan int)); err == nil {
		t.Fatal("encodeJSON channel succeeded")
	}
}

func TestWritePropagatesBadgerKeyErrors(t *testing.T) {
	control, err := controlstore.Open(filepath.Join(t.TempDir(), "control"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := control.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	store, err := Open(control.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	uuid := string(make([]byte, 70<<10))
	run := Run{UUID: uuid}
	event := Event{RunUUID: uuid, Cursor: 1}
	if err := store.write(run, event, true); err == nil {
		t.Fatal("create write with oversize key succeeded")
	}
	if err := store.write(run, event, false); err == nil {
		t.Fatal("update write with oversize key succeeded")
	}
}

func slicesContain(states []State, want State) bool {
	for _, state := range states {
		if state == want {
			return true
		}
	}
	return false
}

func TestCatalogRejectsInvalidOperations(t *testing.T) {
	if _, err := Open(nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Open nil = %v", err)
	}
	control, err := controlstore.Open(filepath.Join(t.TempDir(), "control"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(control.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := testOwner(1)
	valid := CreateInput{UUID: testRunUUID, WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: owner}
	invalid := valid
	invalid.UUID = ""
	if _, err := store.Create(invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid Create = %v", err)
	}
	if _, err := store.Create(valid); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(valid); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Create = %v", err)
	}
	if _, err := store.Get(owner, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get = %v", err)
	}
	if _, err := store.Transition(owner, "missing", TransitionInput{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Transition = %v", err)
	}
	if _, err := store.Transition(testOwner(2), testRunUUID, TransitionInput{}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("foreign Transition = %v", err)
	}
	tooLarge := string(make([]byte, MaxMessageBytes+1))
	if _, err := store.Transition(owner, testRunUUID, TransitionInput{Message: tooLarge}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversize Transition = %v", err)
	}
	if _, err := store.Transition(owner, testRunUUID, TransitionInput{From: []State{StateQueued}, To: StateStarting}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbound Starting = %v", err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Events(owner, testRunUUID, 0, 1); err == nil {
		t.Fatal("Events on closed database succeeded")
	}
	if _, err := store.Transition(owner, testRunUUID, TransitionInput{From: []State{StateQueued}, To: StateFailed}); err == nil {
		t.Fatal("Transition on closed database succeeded")
	}
	if _, err := store.Create(CreateInput{UUID: "closed", WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: owner}); err == nil {
		t.Fatal("Create on closed database succeeded")
	}
}

func TestCreateRejectsOversizeID(t *testing.T) {
	control, err := controlstore.Open(filepath.Join(t.TempDir(), "control"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := control.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	store, err := Open(control.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	in := CreateInput{UUID: string(make([]byte, MaxIDBytes+1)), WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: testOwner(1)}
	if _, err := store.Create(in); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Create with oversized ID = %v", err)
	}
}

func TestCreateDetectsRunInsertedAfterLoad(t *testing.T) {
	control, err := controlstore.Open(filepath.Join(t.TempDir(), "control"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := control.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	store, err := Open(control.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := testOwner(1)
	run := Run{UUID: testRunUUID, WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: owner, State: StateQueued, LastEventCursor: 1}
	if err := control.DB().Update(seedRunAt(testRunUUID, run)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(CreateInput{UUID: testRunUUID, WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: owner}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Create externally inserted UUID = %v", err)
	}
}

func TestValidateBindings(t *testing.T) {
	bound := Run{SandboxUID: testSandboxUID, NodeFingerprint: testOwner(1), NodeInstanceID: testNodeInstance, NodeJobID: testJobID, ExitCodeSet: true}
	tests := []struct {
		name string
		run  Run
		ok   bool
	}{
		{name: "queued", run: Run{State: StateQueued}, ok: true},
		{name: "starting", run: withState(bound, StateStarting), ok: true},
		{name: "starting unbound", run: Run{State: StateStarting}},
		{name: "running", run: withState(bound, StateRunning), ok: true},
		{name: "running no job", run: withState(Run{SandboxUID: testSandboxUID, NodeFingerprint: testOwner(1), NodeInstanceID: testNodeInstance}, StateRunning)},
		{name: "succeeded", run: withState(bound, StateSucceeded), ok: true},
		{name: "succeeded no exit", run: withState(Run{SandboxUID: testSandboxUID, NodeFingerprint: testOwner(1), NodeInstanceID: testNodeInstance, NodeJobID: testJobID}, StateSucceeded)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateBindings(tc.run); (err == nil) != tc.ok {
				t.Fatalf("validateBindings = %v, ok=%v", err, tc.ok)
			}
		})
	}
}

func withState(run Run, state State) Run {
	run.State = state
	return run
}

func TestOpenRejectsCorruptRecords(t *testing.T) {
	tests := []struct {
		name string
		seed func(*badger.Txn) error
	}{
		{name: "invalid json", seed: func(txn *badger.Txn) error { return txn.Set([]byte(runPrefix+testBadUUID), []byte("{")) }},
		{name: "key mismatch", seed: seedRun(Run{UUID: "other", WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: testOwner(1), State: StateQueued, LastEventCursor: 1})},
		{name: "missing event", seed: seedRun(Run{UUID: testBadUUID, WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: testOwner(1), State: StateQueued, LastEventCursor: 1})},
		{name: "event bound exceeded", seed: seedRun(Run{UUID: testBadUUID, WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: testOwner(1), State: StateQueued, LastEventCursor: MaxEventsPerRun + 1})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			control, err := controlstore.Open(filepath.Join(t.TempDir(), "control"))
			if err != nil {
				t.Fatal(err)
			}
			if err := control.DB().Update(tc.seed); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(control.DB(), nil); err == nil {
				t.Fatal("Open corrupt store succeeded")
			}
			if err := control.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEventsRejectCorruptEvent(t *testing.T) {
	control, err := controlstore.Open(filepath.Join(t.TempDir(), "control"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := control.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	store, err := Open(control.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := testOwner(1)
	if _, err := store.Create(CreateInput{UUID: testRunUUID, WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: owner}); err != nil {
		t.Fatal(err)
	}
	key := []byte(fmt.Sprintf("%s%s/%020d", eventPrefix, testRunUUID, 1))
	if err := control.DB().Update(func(txn *badger.Txn) error { return txn.Set(key, []byte("{")) }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Events(owner, testRunUUID, 0, 1); err == nil {
		t.Fatal("Events accepted corrupt record")
	}
}

func TestEventsRejectWrongRecordVersion(t *testing.T) {
	control, err := controlstore.Open(filepath.Join(t.TempDir(), "control"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := control.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	store, err := Open(control.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := testOwner(1)
	if _, err := store.Create(CreateInput{UUID: testRunUUID, WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: owner}); err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal(persisted[Event]{Version: recordVersion + 1, Value: Event{RunUUID: testRunUUID, Cursor: 1, State: StateQueued}})
	if err != nil {
		t.Fatal(err)
	}
	key := []byte(fmt.Sprintf("%s%s/%020d", eventPrefix, testRunUUID, 1))
	if err := control.DB().Update(func(txn *badger.Txn) error { return txn.Set(key, value) }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Events(owner, testRunUUID, 0, 1); err == nil {
		t.Fatal("Events accepted unsupported record version")
	}
}

func TestOpenRejectsMismatchedLastEvent(t *testing.T) {
	control, err := controlstore.Open(filepath.Join(t.TempDir(), "control"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(control.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := testOwner(1)
	if _, err := store.Create(CreateInput{UUID: testRunUUID, WorkspaceUUID: testWorkspaceUUID, ProfileName: testProfileName, ProfileDigest: testProfileDigest, OwnerFingerprint: owner}); err != nil {
		t.Fatal(err)
	}
	event := Event{RunUUID: testRunUUID, Cursor: 1, State: StateFailed}
	value, err := json.Marshal(persisted[Event]{Version: recordVersion, Value: event})
	if err != nil {
		t.Fatal(err)
	}
	key := []byte(fmt.Sprintf("%s%s/%020d", eventPrefix, testRunUUID, 1))
	if err := control.DB().Update(func(txn *badger.Txn) error { return txn.Set(key, value) }); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(control.DB(), nil); err == nil {
		t.Fatal("Open accepted mismatched last event")
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
}

func seedRun(run Run) func(*badger.Txn) error {
	return seedRunAt(testBadUUID, run)
}

func seedRunAt(keyUUID string, run Run) func(*badger.Txn) error {
	return func(txn *badger.Txn) error {
		value, err := json.Marshal(persisted[Run]{Version: recordVersion, Value: run})
		if err != nil {
			return err
		}
		return txn.Set([]byte(fmt.Sprintf("%s%s", runPrefix, keyUUID)), value)
	}
}
