// Package controlstore owns the orchestrator's single durable Badger database.
package controlstore

import (
	"errors"
	"fmt"
	"sync"

	"github.com/dgraph-io/badger/v4"
)

const schemaVersion = "1"

const schemaVersionKey = "meta/schema_version"

// Store owns a Badger database shared by the durable control-plane catalogs.
type Store struct {
	db   *badger.DB
	once sync.Once
	err  error
}

// Open opens the control-plane database and verifies its schema.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("controlstore: persistence path is empty")
	}
	db, err := badger.Open(badger.DefaultOptions(path).WithLogger(nil))
	if err != nil {
		return nil, fmt.Errorf("controlstore: open badger: %w", err)
	}
	if err := initialise(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func initialise(db *badger.DB) error {
	return db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(schemaVersionKey))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return txn.Set([]byte(schemaVersionKey), []byte(schemaVersion))
		}
		if err != nil {
			return fmt.Errorf("controlstore: read schema version: %w", err)
		}
		got, err := item.ValueCopy(nil)
		if err != nil {
			return fmt.Errorf("controlstore: copy schema version: %w", err)
		}
		if string(got) != schemaVersion {
			return fmt.Errorf("controlstore: unsupported schema version %q (want %q)", got, schemaVersion)
		}
		return nil
	})
}

// DB returns the shared database. Callers must not close it.
func (s *Store) DB() *badger.DB { return s.db }

// Close closes the database. It is idempotent.
func (s *Store) Close() error {
	s.once.Do(func() { s.err = s.db.Close() })
	return s.err
}
