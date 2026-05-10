package identity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/richiejp/darkaifactory/internal/oops"
)

// Peer is one approved counterparty: who they are (Label/Role) and
// when we approved them. The fingerprint is the trust anchor; Label
// and Role are descriptive only and may be edited freely.
type Peer struct {
	Fingerprint Fingerprint `json:"fingerprint"`
	Label       string      `json:"label,omitempty"`
	Role        Role        `json:"role,omitempty"`
	ApprovedAt  time.Time   `json:"approved_at"`
}

// PeerStore is the on-disk TOFU set: a JSON array of approved peers,
// rewritten atomically on every mutation. Concurrency-safe via a
// single mutex; this is a config-y file accessed at handshake time,
// not a hot path.
type PeerStore struct {
	mu    sync.Mutex
	path  string
	peers map[Fingerprint]Peer
}

// OpenPeerStore reads path. A missing file is fine — the store starts
// empty and the file is created on the first mutation. A malformed
// file is fatal: refusing to start beats silently dropping the
// existing approval set.
func OpenPeerStore(path string) (*PeerStore, error) {
	if path == "" {
		return nil, errors.New("identity: empty peer-store path")
	}
	s := &PeerStore{path: path, peers: map[Fingerprint]Peer{}}
	// #nosec G304 — caller-supplied store path.
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("identity: read peer store: %w", err)
	}
	var list []Peer
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("identity: parse peer store: %w", err)
	}
	for _, p := range list {
		s.peers[p.Fingerprint] = p
	}
	return s, nil
}

// Add inserts (or replaces) a peer and rewrites the file. ApprovedAt
// is filled in if the caller left it zero.
func (s *PeerStore) Add(p Peer) error {
	if p.Fingerprint == (Fingerprint{}) {
		return errors.New("identity: cannot add peer with zero fingerprint")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ApprovedAt.IsZero() {
		p.ApprovedAt = time.Now().UTC()
	}
	s.peers[p.Fingerprint] = p
	return s.saveLocked()
}

// Remove deletes the entry if present. A missing fingerprint is not
// an error — Remove is intended to be idempotent so cleanup scripts
// don't have to LookUp first.
func (s *PeerStore) Remove(fp Fingerprint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.peers[fp]; !ok {
		return nil
	}
	delete(s.peers, fp)
	return s.saveLocked()
}

// Lookup returns the stored peer if any. Used at handshake time on
// the orchestrator and the client to authorise a peer cert.
func (s *PeerStore) Lookup(fp Fingerprint) (Peer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[fp]
	return p, ok
}

// List returns a copy of every peer, sorted oldest-approval first so
// `daft auth list` output is stable across runs.
func (s *PeerStore) List() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ApprovedAt.Equal(out[j].ApprovedAt) {
			return out[i].ApprovedAt.Before(out[j].ApprovedAt)
		}
		return out[i].Fingerprint.String() < out[j].Fingerprint.String()
	})
	return out
}

// Path returns the on-disk path of the store. Useful for `daft auth`
// CLI commands that want to surface "approvals are stored at …".
func (s *PeerStore) Path() string {
	return s.path
}

// saveLocked rewrites the JSON file atomically. The mutex is the
// caller's responsibility.
func (s *PeerStore) saveLocked() error {
	list := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		list = append(list, p)
	}
	slices.SortFunc(list, func(a, b Peer) int {
		switch {
		case a.ApprovedAt.Before(b.ApprovedAt):
			return -1
		case a.ApprovedAt.After(b.ApprovedAt):
			return 1
		}
		// Tiebreak on fingerprint so identical timestamps still produce
		// a deterministic file (matters for reproducible snapshots).
		ax, bx := a.Fingerprint.String(), b.Fingerprint.String()
		switch {
		case ax < bx:
			return -1
		case ax > bx:
			return 1
		}
		return 0
	})

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("identity: marshal peer store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), dirPerm); err != nil {
		return fmt.Errorf("identity: mkdir peer store: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, filePerm); err != nil {
		return fmt.Errorf("identity: write peer store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		oops.Skip("identity: remove partial peer store",
			os.Remove(tmp), os.ErrNotExist)
		return fmt.Errorf("identity: rename peer store: %w", err)
	}
	return nil
}
