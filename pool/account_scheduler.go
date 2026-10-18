package pool

import "kiro-go/config"

// accountSelectionCandidate is the selector's already-eligible account plus the
// existing round-robin/LRU and health tie-breaker signals.
type accountSelectionCandidate struct {
	acc     *config.Account
	lastSeq uint64
	score   float64
}

// applyAccountSelectionStrategy may only narrow an already-eligible candidate
// set. It must never reintroduce an account rejected by model compatibility,
// retry exclusion, cooldown, circuit breaker, credential eligibility, or quota gates.
//
// Round-robin preserves the historical candidate set exactly. Priority keeps
// only the best currently eligible hard-priority tier and then lets the existing
// LRU selector distribute within that tier. Fill-first keeps only the first
// account (pool/config order) in the best eligible tier.
func applyAccountSelectionStrategy(candidates []accountSelectionCandidate, strategy string) []accountSelectionCandidate {
	if len(candidates) <= 1 {
		return candidates
	}

	switch strategy {
	case config.AccountSelectionStrategyPriority, config.AccountSelectionStrategyFillFirst:
		bestPriority := config.EffectiveAccountPriority(candidates[0].acc.Priority)
		for _, candidate := range candidates[1:] {
			priority := config.EffectiveAccountPriority(candidate.acc.Priority)
			if priority < bestPriority {
				bestPriority = priority
			}
		}

		if strategy == config.AccountSelectionStrategyFillFirst {
			for i := range candidates {
				if config.EffectiveAccountPriority(candidates[i].acc.Priority) == bestPriority {
					return candidates[i : i+1]
				}
			}
			return candidates
		}

		filtered := make([]accountSelectionCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			if config.EffectiveAccountPriority(candidate.acc.Priority) == bestPriority {
				filtered = append(filtered, candidate)
			}
		}
		return filtered

	case config.AccountSelectionStrategyRoundRobin:
		fallthrough
	default:
		// GetAccountSelectionStrategy already normalizes invalid persisted values
		// to round-robin. Keep the helper fail-safe when called in isolation too.
		return candidates
	}
}
