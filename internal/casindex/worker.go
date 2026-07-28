package casindex

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const DefaultCleanupInterval = 5 * time.Minute

type StorageStatus struct {
	UsageBytes       uint64
	ProtectedBytes   uint64
	RecoverableBytes uint64
	QueuedBytes      uint64
	ChunkCount       int
	QueuedCount      int
	Policy           Policy
}

type WorkerConfig struct {
	Index    *Index
	Policy   Policy
	Interval time.Duration
	Now      func() time.Time
	// Ticks makes cleanup deterministic in tests. Production leaves it nil and
	// Run owns a time.Ticker at Interval.
	Ticks <-chan time.Time
}

type Worker struct {
	index    *Index
	policy   Policy
	interval time.Duration
	now      func() time.Time
	ticks    <-chan time.Time
}

func NewWorker(config WorkerConfig) (*Worker, error) {
	if config.Index == nil {
		return nil, errors.New("casindex: cleanup worker requires an index")
	}
	if config.Policy == (Policy{}) {
		config.Policy = DefaultPolicy()
	}
	if err := validatePolicy(config.Policy); err != nil {
		return nil, err
	}
	if config.Interval <= 0 {
		config.Interval = DefaultCleanupInterval
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Worker{
		index: config.Index, policy: config.Policy, interval: config.Interval,
		now: config.Now, ticks: config.Ticks,
	}, nil
}

// Run evaluates durable cleanup every five minutes by default. A failed pass
// is returned so the Node supervisor can surface storage health rather than
// silently running without its hard-limit guard.
func (w *Worker) Run(ctx context.Context) error {
	if w.ticks != nil {
		return w.runTicks(ctx, w.ticks)
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	return w.runTicks(ctx, ticker.C)
}

func (w *Worker) runTicks(ctx context.Context, ticks <-chan time.Time) error {
	//daft:lint:disable=forloopbound reason="bounded by the worker context or closure of the injected tick channel"
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, open := <-ticks:
			if !open {
				return nil
			}
			if _, err := w.Prune(ctx, PruneSoft, 0, false); err != nil {
				return err
			}
		}
	}
}

// AdmitWrite performs synchronous hard-pressure cleanup before incoming bytes
// are accepted. If protected data prevents enough reclamation the write is
// rejected without weakening protection.
func (w *Worker) AdmitWrite(ctx context.Context, incoming uint64) (Plan, error) {
	return w.Prune(ctx, PruneHard, incoming, false)
}

// Prune builds a deterministic plan and optionally executes it. Dry runs do
// not mutate queue state or unlink chunks.
func (w *Worker) Prune(ctx context.Context, mode PruneMode, incoming uint64, dryRun bool) (Plan, error) {
	chunks, err := w.index.Chunks()
	if err != nil {
		return Plan{}, err
	}
	usage := chunkUsage(chunks)
	plan, err := BuildPlan(chunks, usage, incoming, w.now(), w.policy, mode)
	if err != nil {
		return plan, err
	}
	if dryRun || len(plan.Candidates) == 0 {
		return plan, nil
	}
	if err := w.index.QueuePlan(plan); err != nil {
		return plan, err
	}
	for _, candidate := range plan.Candidates {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		if err := w.index.DeleteQueued(ctx, candidate.Digest); err != nil {
			if errors.Is(err, ErrChunkReferenced) {
				continue
			}
			return plan, fmt.Errorf("casindex: delete queued chunk %s: %w", candidate.Digest, err)
		}
	}
	return plan, nil
}

func (w *Worker) Status() (StorageStatus, error) {
	chunks, err := w.index.Chunks()
	if err != nil {
		return StorageStatus{}, err
	}
	status := StorageStatus{Policy: w.policy, ChunkCount: len(chunks)}
	for index := range chunks {
		chunk := &chunks[index]
		status.UsageBytes += chunk.Size
		if chunk.Recoverability == RecoverabilityProtected {
			status.ProtectedBytes += chunk.Size
		} else {
			status.RecoverableBytes += chunk.Size
		}
		if !chunk.QueuedAt.IsZero() {
			status.QueuedCount++
			status.QueuedBytes += chunk.Size
		}
	}
	return status, nil
}

func chunkUsage(chunks []Chunk) uint64 {
	var usage uint64
	for index := range chunks {
		usage = saturatingAdd(usage, chunks[index].Size)
	}
	return usage
}
