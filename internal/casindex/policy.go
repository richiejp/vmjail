package casindex

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

var ErrInsufficientReclaim = errors.New("casindex: protected data prevents sufficient reclamation")

func BuildPlan(chunks []Chunk, usage, incoming uint64, now time.Time, policy Policy, mode PruneMode) (Plan, error) {
	if err := validatePolicy(policy); err != nil {
		return Plan{}, err
	}
	pressure := normalizedPressure(usage, policy)
	candidates := make([]Candidate, 0, len(chunks))
	for chunkIndex := range chunks {
		chunk := &chunks[chunkIndex]
		if !eligible(chunk) {
			continue
		}
		idle := now.Sub(chunk.LastUse)
		expired := idle >= policy.Expiry
		underPressure := mode == PruneHard && saturatingAdd(usage, incoming) > policy.HardBytes ||
			mode == PruneSoft && usage > policy.SoftBytes
		if !expired && !underPressure {
			continue
		}
		candidates = append(candidates, Candidate{
			Digest: chunk.Digest, Size: chunk.Size, LastUse: chunk.LastUse,
			Disposition: chunk.Disposition, Score: cleanupScore(chunk, idle, pressure, policy.Expiry),
		})
	}
	slices.SortFunc(candidates, func(a, b Candidate) int {
		if scoreOrder := cmp.Compare(b.Score, a.Score); scoreOrder != 0 {
			return scoreOrder
		}
		return cmp.Compare(a.Digest, b.Digest)
	})

	result := Plan{UsageBefore: usage, UsageAfter: usage, IncomingBytes: incoming}
	for _, candidate := range candidates {
		expired := now.Sub(candidate.LastUse) >= policy.Expiry
		needPressureDelete := mode == PruneHard && saturatingAdd(result.UsageAfter, incoming) > policy.HardBytes ||
			mode == PruneSoft && result.UsageAfter > policy.TargetBytes
		if !expired && !needPressureDelete {
			continue
		}
		result.Candidates = append(result.Candidates, candidate)
		if candidate.Size >= result.UsageAfter {
			result.UsageAfter = 0
		} else {
			result.UsageAfter -= candidate.Size
		}
	}
	if mode == PruneHard && saturatingAdd(result.UsageAfter, incoming) > policy.HardBytes {
		return result, fmt.Errorf("%w: usage=%d incoming=%d reclaimable=%d hard=%d",
			ErrInsufficientReclaim, usage, incoming, usage-result.UsageAfter, policy.HardBytes)
	}
	return result, nil
}

func validatePolicy(policy Policy) error {
	if policy.TargetBytes == 0 || policy.TargetBytes >= policy.SoftBytes || policy.SoftBytes >= policy.HardBytes || policy.Expiry <= 0 {
		return errors.New("casindex: invalid cleanup policy")
	}
	return nil
}

func eligible(chunk *Chunk) bool {
	return chunk.Recoverability == RecoverabilityRecoverable
}

func cleanupScore(chunk *Chunk, idle time.Duration, pressure float64, expiry time.Duration) float64 {
	disposition := 1.0 / 3.0
	switch chunk.Disposition {
	case DispositionRejected, DispositionDeleted:
		disposition = 1
	case DispositionSuperseded:
		disposition = 2.0 / 3.0
	}
	idleScore := math.Min(math.Max(float64(idle)/float64(expiry), 0), 1)
	return 0.50*disposition + 0.35*idleScore + 0.15*pressure
}

func normalizedPressure(usage uint64, policy Policy) float64 {
	if usage <= policy.TargetBytes {
		return 0
	}
	pressure := float64(usage-policy.TargetBytes) / float64(policy.HardBytes-policy.TargetBytes)
	return math.Min(pressure, 1)
}

func saturatingAdd(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}
