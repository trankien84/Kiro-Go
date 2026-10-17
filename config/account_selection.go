package config

import (
	"fmt"
	"strings"
)

const (
	AccountSelectionStrategyRoundRobin = "round-robin"
	AccountSelectionStrategyPriority   = "priority"
	AccountSelectionStrategyFillFirst  = "fill-first"

	// DefaultAccountPriority is the effective hard-priority tier for legacy
	// accounts whose Priority field is zero/unset (or otherwise non-positive).
	DefaultAccountPriority = 100
)

// NormalizeAccountSelectionStrategy validates and canonicalizes the configured
// global account-routing policy. An empty value preserves the historical
// round-robin behavior for existing config files.
func NormalizeAccountSelectionStrategy(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", AccountSelectionStrategyRoundRobin:
		return AccountSelectionStrategyRoundRobin, nil
	case AccountSelectionStrategyPriority:
		return AccountSelectionStrategyPriority, nil
	case AccountSelectionStrategyFillFirst:
		return AccountSelectionStrategyFillFirst, nil
	default:
		return "", fmt.Errorf("invalid account selection strategy %q", value)
	}
}

func accountSelectionStrategyLocked() string {
	if cfg == nil {
		return AccountSelectionStrategyRoundRobin
	}
	strategy, err := NormalizeAccountSelectionStrategy(cfg.AccountSelectionStrategy)
	if err != nil {
		return AccountSelectionStrategyRoundRobin
	}
	return strategy
}

// GetAccountSelectionStrategy returns the effective global routing policy.
// Invalid hand-edited values fail closed to the historical round-robin behavior.
func GetAccountSelectionStrategy() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return accountSelectionStrategyLocked()
}

// UpdateAccountSelectionStrategyPublished validates and persists the global
// routing policy while preserving the config transaction's publication bit.
// published=true with err!=nil means the new strategy is already canonical but
// a later durability barrier failed; callers must not treat that as a rollback.
func UpdateAccountSelectionStrategyPublished(value string) (published bool, err error) {
	strategy, err := NormalizeAccountSelectionStrategy(value)
	if err != nil {
		return false, err
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	return publishConfigFieldsLocked(func(next *Config) error {
		next.AccountSelectionStrategy = strategy
		return nil
	})
}

// UpdateAccountSelectionStrategyIfObserved adds optimistic concurrency for the
// Admin control plane. The comparison uses the effective normalized strategy so
// legacy empty config and invalid hand-edited state both retain fail-closed
// round-robin semantics.
func UpdateAccountSelectionStrategyIfObserved(observed, value string) (published bool, superseded bool, err error) {
	observedStrategy, err := NormalizeAccountSelectionStrategy(observed)
	if err != nil {
		return false, false, fmt.Errorf("invalid observed account selection strategy: %w", err)
	}
	strategy, err := NormalizeAccountSelectionStrategy(value)
	if err != nil {
		return false, false, err
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return false, false, fmt.Errorf("configuration is not initialized")
	}
	current := accountSelectionStrategyLocked()
	if current != observedStrategy {
		return false, true, nil
	}
	if strategy == current {
		return false, false, nil
	}
	published, err = publishConfigFieldsLocked(func(next *Config) error {
		next.AccountSelectionStrategy = strategy
		return nil
	})
	return published, false, err
}

// UpdateAccountSelectionStrategy preserves the historical error-only API for
// existing callers. New control-plane code should use a publication-aware
// variant so post-publication durability failures remain distinguishable.
func UpdateAccountSelectionStrategy(value string) error {
	_, err := UpdateAccountSelectionStrategyPublished(value)
	return err
}

// EffectiveAccountPriority converts the persisted value into a hard routing
// tier. Lower positive numbers have higher priority; zero/unset keeps legacy
// accounts in the neutral default tier.
func EffectiveAccountPriority(priority int) int {
	if priority <= 0 {
		return DefaultAccountPriority
	}
	return priority
}
