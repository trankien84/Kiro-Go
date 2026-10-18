package config

import (
	"fmt"
	"strings"
)

// AccountRoutingObservation identifies the routing-relevant state of one exact
// account lifecycle in an ordered Admin snapshot. Priority is the raw persisted
// value, not EffectiveAccountPriority, so a concurrent same-lifecycle priority
// edit cannot be hidden by two raw values that happen to map to the same tier.
type AccountRoutingObservation struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
	Priority   int    `json:"priority"`
}

func validateAccountRoutingObservationList(items []AccountRoutingObservation) error {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.ID) == "" {
			return fmt.Errorf("account order contains a blank id")
		}
		if item.Generation == 0 {
			return fmt.Errorf("account %s has no lifecycle generation", item.ID)
		}
		if _, exists := seen[item.ID]; exists {
			return fmt.Errorf("account order contains duplicate id %s", item.ID)
		}
		seen[item.ID] = struct{}{}
	}
	return nil
}

func accountRoutingObservationMatchesLocked(item AccountRoutingObservation, account Account) bool {
	return item.ID == account.ID &&
		item.Priority == account.Priority &&
		accountLifecycleMatchesLocked(item.ID, item.Generation)
}

// ReorderAccountsIfObserved publishes desired only while Fill First is still
// the canonical routing strategy and observed still matches the exact canonical
// account order, lifecycle generations, and raw priorities. desired must be a
// permutation of those same observations.
//
// Account contents are copied from canonical cfg under cfgLock, so stale browser
// bodies never overwrite credential/quota/health progress. Fencing Priority is
// essential: moving two accounts that were observed in one tier must not commit
// after another Admin session has moved either account into a different tier.
func ReorderAccountsIfObserved(
	observedStrategy string,
	observed, desired []AccountRoutingObservation,
) (published bool, superseded bool, err error) {
	strategy, err := NormalizeAccountSelectionStrategy(observedStrategy)
	if err != nil {
		return false, false, fmt.Errorf("invalid observed account selection strategy: %w", err)
	}
	if strategy != AccountSelectionStrategyFillFirst {
		return false, false, fmt.Errorf("account order can only be changed while using %s", AccountSelectionStrategyFillFirst)
	}
	if err := validateAccountRoutingObservationList(observed); err != nil {
		return false, false, err
	}
	if err := validateAccountRoutingObservationList(desired); err != nil {
		return false, false, err
	}
	if len(observed) != len(desired) {
		return false, false, fmt.Errorf("observed and desired account orders must have the same length")
	}

	desiredByID := make(map[string]AccountRoutingObservation, len(desired))
	for _, item := range desired {
		desiredByID[item.ID] = item
	}
	for _, item := range observed {
		candidate, ok := desiredByID[item.ID]
		if !ok || candidate.Generation != item.Generation || candidate.Priority != item.Priority {
			return false, false, fmt.Errorf("desired account order must contain the same observed routing state")
		}
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return false, false, fmt.Errorf("configuration is not initialized")
	}
	if accountSelectionStrategyLocked() != strategy {
		return false, true, nil
	}
	if len(observed) != len(cfg.Accounts) {
		return false, true, nil
	}

	canonical := make(map[string]Account, len(cfg.Accounts))
	for i, account := range cfg.Accounts {
		if !accountRoutingObservationMatchesLocked(observed[i], account) {
			return false, true, nil
		}
		if _, duplicate := canonical[account.ID]; duplicate {
			return false, true, nil
		}
		canonical[account.ID] = account
	}

	sameOrder := true
	for i := range observed {
		if observed[i].ID != desired[i].ID {
			sameOrder = false
			break
		}
	}
	if sameOrder {
		return false, false, nil
	}

	published, err = publishConfigAccountsLocked(func(next *Config) error {
		ordered := make([]Account, len(desired))
		for i, item := range desired {
			account, ok := canonical[item.ID]
			if !ok {
				return fmt.Errorf("account %s changed while staging routing order", item.ID)
			}
			ordered[i] = account.Clone()
		}
		next.Accounts = ordered
		return nil
	})
	return published, false, err
}

// UpdateAccountPriorityIfObserved changes only the hard routing tier when both
// the lifecycle and exact previously-rendered raw Priority still match. The
// observed value may be negative so an old hand-edited config can be repaired;
// new values are constrained to the supported persisted domain (0 or positive).
func UpdateAccountPriorityIfObserved(id string, generation uint64, observedPriority, priority int) (after Account, published bool, superseded bool, err error) {
	if strings.TrimSpace(id) == "" {
		return Account{}, false, false, fmt.Errorf("account id is required")
	}
	if generation == 0 {
		return Account{}, false, false, fmt.Errorf("account lifecycle generation is required")
	}
	if priority < 0 {
		return Account{}, false, false, fmt.Errorf("priority must be zero or a positive integer")
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return Account{}, false, false, fmt.Errorf("configuration is not initialized")
	}
	if !accountLifecycleMatchesLocked(id, generation) {
		return Account{}, false, true, nil
	}

	index := -1
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 || cfg.Accounts[index].Priority != observedPriority {
		return Account{}, false, true, nil
	}
	if priority == observedPriority {
		after = cloneAccountWithLifecycleLocked(cfg.Accounts[index])
		return after, false, false, nil
	}

	published, err = publishConfigAccountsLocked(func(next *Config) error {
		if index >= len(next.Accounts) || next.Accounts[index].ID != id || next.Accounts[index].Priority != observedPriority {
			return fmt.Errorf("account %s changed while staging routing priority", id)
		}
		next.Accounts[index].Priority = priority
		after = next.Accounts[index].Clone()
		return nil
	})
	if published {
		after.DispatchGeneration = accountLifecycleGenerationLocked(id)
	}
	return after, published, false, err
}
