package controlstore

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dgraph-io/badger/v4"
)

func TestOpenCloseAndLocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("second Open acquired lock")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close reopened: %v", err)
	}
}

func TestOpenValidation(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open empty succeeded")
	}
	path := filepath.Join(t.TempDir(), "control")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Update(func(txn *badger.Txn) error { return txn.Set([]byte(schemaVersionKey), []byte("999")) }); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "unsupported schema") {
		t.Fatalf("Open invalid schema = %v", err)
	}
}

func TestInitialiseRejectsClosedDatabase(t *testing.T) {
	db, err := badger.Open(badger.DefaultOptions(filepath.Join(t.TempDir(), "control")).WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := initialise(db); err == nil {
		t.Fatal("initialise closed database succeeded")
	}
}
