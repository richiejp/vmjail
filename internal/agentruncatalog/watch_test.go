package agentruncatalog

import (
	"strings"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/richiejp/darkaifactory/internal/identity"
)

func TestStoreWatchCoalescesAndStops(t *testing.T) {
	db, err := badger.Open(badger.DefaultOptions(t.TempDir()).WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close badger: %v", closeErr)
		}
	})
	owner, err := identity.ParseFingerprint("sha256:" + strings.Repeat("9", 64))
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes, stop := store.Watch()
	_, err = store.Create(CreateInput{UUID: "watch-run", WorkspaceUUID: "watch-workspace", ProfileName: "watch-profile", ProfileDigest: "watch-digest", Command: []string{"watch-agent"}, OwnerFingerprint: owner})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Transition(owner, "watch-run", TransitionInput{From: []State{StateQueued}, To: StateCanceled})
	if err != nil {
		t.Fatal(err)
	}
	if revision := <-changes; revision == 0 {
		t.Fatal("zero revision")
	}
	stop()
}
