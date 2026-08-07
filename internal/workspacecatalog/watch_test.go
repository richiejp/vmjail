package workspacecatalog

import (
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/identity"
)

func TestStoreWatchCoalescesAndStops(t *testing.T) {
	owner, err := identity.ParseFingerprint("sha256:" + strings.Repeat("8", 64))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(nil)
	changes, stop := store.Watch()
	_, err = store.Create(CreateInput{UUID: "watch-workspace", Name: "watch", OwnerFingerprint: owner})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("watch-workspace", MutateMetadata, nil)
	if err != nil {
		t.Fatal(err)
	}
	if revision := <-changes; revision == 0 {
		t.Fatal("zero revision")
	}
	stop()
	if err := store.Delete("watch-workspace"); err != nil {
		t.Fatal(err)
	}
}
