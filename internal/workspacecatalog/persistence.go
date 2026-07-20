package workspacecatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/richiejp/darkaifactory/internal/controlstore"
)

const (
	workspaceRecordVersion = 1
	workspacePrefix        = "workspace/"
)

type persistedWorkspace struct {
	Version  uint32   `json:"version"`
	Metadata Metadata `json:"metadata"`
	Spec     Spec     `json:"spec"`
}

type badgerBackend struct {
	db      *badger.DB
	closeDB bool
}

// Open opens or creates a Badger-backed Workspace store at path. Only the
// declarative Metadata and Spec are durable; Status is reconstructed and then
// reconciled from live Nodes after every orchestrator restart.
func Open(path string, now func() time.Time) (*Store, error) {
	if path == "" {
		return nil, errors.New("workspacecatalog: persistence path is empty")
	}
	control, err := controlstore.Open(path)
	if err != nil {
		return nil, fmt.Errorf("workspacecatalog: open control store: %w", err)
	}
	db := control.DB()
	backend := &badgerBackend{db: db, closeDB: true}
	s := newStore(now, backend)
	if err := s.load(db); err != nil {
		_ = control.Close()
		return nil, err
	}
	return s, nil
}

// NewPersistent builds a Workspace catalog over a shared control database.
// The caller retains ownership of db.
func NewPersistent(db *badger.DB, now func() time.Time) (*Store, error) {
	if db == nil {
		return nil, errors.New("workspacecatalog: nil control database")
	}
	s := newStore(now, &badgerBackend{db: db})
	if err := s.load(db); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load(db *badger.DB) error {
	return db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(workspacePrefix)
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			value, err := item.ValueCopy(nil)
			if err != nil {
				return fmt.Errorf("workspacecatalog: read %q: %w", item.KeyCopy(nil), err)
			}
			var record persistedWorkspace
			if err := json.Unmarshal(value, &record); err != nil {
				return fmt.Errorf("workspacecatalog: decode %q: %w", item.KeyCopy(nil), err)
			}
			if err := validatePersistedWorkspace(record); err != nil {
				return fmt.Errorf("workspacecatalog: invalid %q: %w", item.KeyCopy(nil), err)
			}
			ws := Workspace{Metadata: record.Metadata, Spec: record.Spec}
			ws.Status.Missing = missingFields(ws.Spec)
			ws.Status.Conditions = recomputeConditions(nil, ws.Status.Missing, false, s.now())
			if _, exists := s.all[ws.Metadata.UUID]; exists {
				return fmt.Errorf("workspacecatalog: duplicate uuid %q", ws.Metadata.UUID)
			}
			alias := aliasKey{owner: ws.Metadata.OwnerFingerprint, name: ws.Metadata.Name}
			if _, exists := s.byAlias[alias]; exists {
				return fmt.Errorf("workspacecatalog: duplicate owner/name alias %q", ws.Metadata.Name)
			}
			s.all[ws.Metadata.UUID] = ws
			s.byAlias[alias] = ws.Metadata.UUID
		}
		return nil
	})
}

func validatePersistedWorkspace(record persistedWorkspace) error {
	if record.Version != workspaceRecordVersion {
		return fmt.Errorf("unsupported record version %d", record.Version)
	}
	if record.Metadata.UUID == "" {
		return errors.New("empty uuid")
	}
	if record.Metadata.Name == "" {
		return errors.New("empty name")
	}
	if record.Metadata.Generation == 0 {
		return errors.New("zero generation")
	}
	if record.Metadata.CreatedAt.IsZero() || record.Metadata.UpdatedAt.IsZero() {
		return errors.New("zero audit timestamp")
	}
	return nil
}

func (b *badgerBackend) Save(ws Workspace) error {
	record := persistedWorkspace{
		Version:  workspaceRecordVersion,
		Metadata: ws.Metadata,
		Spec:     ws.Spec,
	}
	value, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode workspace: %w", err)
	}
	key := []byte(workspacePrefix + ws.Metadata.UUID)
	if err := b.db.Update(func(txn *badger.Txn) error { return txn.Set(key, value) }); err != nil {
		return fmt.Errorf("write workspace: %w", err)
	}
	return nil
}

func (b *badgerBackend) Delete(uuid string) error {
	key := []byte(workspacePrefix + uuid)
	if err := b.db.Update(func(txn *badger.Txn) error { return txn.Delete(key) }); err != nil {
		return fmt.Errorf("delete workspace: %w", err)
	}
	return nil
}

func (b *badgerBackend) Close() error {
	if !b.closeDB {
		return nil
	}
	return b.db.Close()
}
