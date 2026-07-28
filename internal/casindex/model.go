// Package casindex provides durable provenance and cleanup metadata for one
// content-addressed store.
package casindex

import "time"

type Origin string

const (
	OriginLegacy        Origin = "legacy"
	OriginClientUpload  Origin = "client-upload"
	OriginClientAdopted Origin = "client-adopted"
	OriginSandbox       Origin = "sandbox"
)

type Recoverability string

const (
	RecoverabilityProtected   Recoverability = "protected"
	RecoverabilityRecoverable Recoverability = "recoverable"
)

type Disposition string

const (
	DispositionActive     Disposition = "active"
	DispositionSuperseded Disposition = "superseded"
	DispositionRejected   Disposition = "rejected"
	DispositionDeleted    Disposition = "deleted"
)

// Reference identifies an exact manifest generation currently requiring a
// chunk. Superseded/rejected/deleted manifests do not retain references.
type Reference struct {
	SyncKey    string `json:"sync_key"`
	Digest     string `json:"digest"`
	Generation uint64 `json:"generation"`
}

type Chunk struct {
	Digest         string         `json:"digest"`
	Size           uint64         `json:"size"`
	LastUse        time.Time      `json:"last_use"`
	References     []Reference    `json:"references,omitempty"`
	Origin         Origin         `json:"origin"`
	Recoverability Recoverability `json:"recoverability"`
	Disposition    Disposition    `json:"disposition"`
	QueuedAt       time.Time      `json:"queued_at,omitempty"`
	QueueScore     float64        `json:"queue_score,omitempty"`
}

type Manifest struct {
	SyncKey        string         `json:"sync_key"`
	Digest         string         `json:"digest"`
	Generation     uint64         `json:"generation"`
	Chunks         []ChunkRef     `json:"chunks"`
	Origin         Origin         `json:"origin"`
	Recoverability Recoverability `json:"recoverability"`
	Disposition    Disposition    `json:"disposition"`
	Current        bool           `json:"current"`
	NeedsRepush    bool           `json:"needs_repush,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
}

type ChunkRef struct {
	Digest string `json:"digest"`
	Size   uint64 `json:"size"`
}

type CommitRequest struct {
	SyncKey        string
	Digest         string
	Generation     uint64
	Chunks         []ChunkRef
	Origin         Origin
	Recoverability Recoverability
}

type Policy struct {
	HardBytes   uint64
	SoftBytes   uint64
	TargetBytes uint64
	Expiry      time.Duration
}

func DefaultPolicy() Policy {
	return Policy{
		HardBytes:   100 << 30,
		SoftBytes:   85 << 30,
		TargetBytes: 75 << 30,
		Expiry:      180 * 24 * time.Hour,
	}
}

type PruneMode uint8

const (
	PruneSoft PruneMode = iota
	PruneHard
)

type Candidate struct {
	Digest      string
	Size        uint64
	Score       float64
	Disposition Disposition
	LastUse     time.Time
}

type Plan struct {
	Candidates    []Candidate
	UsageBefore   uint64
	UsageAfter    uint64
	IncomingBytes uint64
}
