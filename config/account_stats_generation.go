package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// accountStatsVersionKey identifies a durable cumulative-stats version within
// one logical lifetime of an account ID. The generation is process-local because
// pending asynchronous persistence work is process-local too.
type accountStatsVersionKey struct {
	id         string
	generation uint64
}

var (
	accountStatsGenerationByID = make(map[string]uint64)
	accountStatsPresentByID    = make(map[string]bool)
	accountStatsAppliedVersion = make(map[accountStatsVersionKey]uint64)
)

type AccountWithStatsGeneration struct {
	Account             Account
	LifecycleGeneration uint64
	StatsGeneration     uint64
	StatsVersion        uint64
}

type AccountStatsCounters struct {
	RequestCount int
	ErrorCount   int
	TotalTokens  int
	TotalCredits float64
	LastUsed     int64
}

type AccountStatsWrite struct {
	AccountID  string
	Generation uint64
	Version    uint64
	Counters   AccountStatsCounters
}

type AccountStatsWriteResult struct {
	AccountID  string
	Generation uint64
	Version    uint64
	Superseded bool
}

// normalizeAccountStatsID validates emptiness without changing identity. Account
// IDs are exact keys throughout config/pool; leading/trailing spaces therefore
// remain significant if an existing configuration contains them.
func normalizeAccountStatsID(id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	return id
}

func accountStatsCountersRegressed(previous, next Account) bool {
	return next.RequestCount < previous.RequestCount ||
		next.ErrorCount < previous.ErrorCount ||
		next.TotalTokens < previous.TotalTokens ||
		next.TotalCredits < previous.TotalCredits ||
		next.LastUsed < previous.LastUsed
}

func ensureAccountStatsGenerationLocked(id string) uint64 {
	id = normalizeAccountStatsID(id)
	if id == "" {
		return 0
	}
	generation := accountStatsGenerationByID[id]
	if generation == 0 {
		generation = 1
		accountStatsGenerationByID[id] = generation
	}
	return generation
}

func markAccountStatsAddedLocked(id string) uint64 {
	id = normalizeAccountStatsID(id)
	generation := ensureAccountStatsGenerationLocked(id)
	if generation != 0 {
		accountStatsPresentByID[id] = true
	}
	return generation
}

func markAccountStatsDeletedLocked(id string) {
	id = normalizeAccountStatsID(id)
	if id == "" {
		return
	}
	generation := ensureAccountStatsGenerationLocked(id)
	if accountStatsPresentByID[id] {
		generation++
		accountStatsGenerationByID[id] = generation
	}
	accountStatsPresentByID[id] = false
}

func markAccountStatsReplacedLocked(previous, next Account) {
	previousID := normalizeAccountStatsID(previous.ID)
	nextID := normalizeAccountStatsID(next.ID)
	if previousID != nextID {
		markAccountStatsDeletedLocked(previousID)
		markAccountStatsAddedLocked(nextID)
		return
	}
	if nextID == "" {
		return
	}
	generation := markAccountStatsAddedLocked(nextID)
	if accountStatsCountersRegressed(previous, next) {
		accountStatsGenerationByID[nextID] = generation + 1
	}
}

// reconcileAccountStatsGenerationsLocked aligns process-local lifecycle state
// with a configuration replacement performed by Load. Caller MUST hold cfgLock.
func reconcileAccountStatsGenerationsLocked(previous, next []Account) {
	previousByID := make(map[string]Account, len(previous))
	for _, account := range previous {
		if id := normalizeAccountStatsID(account.ID); id != "" {
			previousByID[id] = account
		}
	}
	nextByID := make(map[string]Account, len(next))
	for _, account := range next {
		if id := normalizeAccountStatsID(account.ID); id != "" {
			nextByID[id] = account
		}
	}

	for id := range previousByID {
		if _, ok := nextByID[id]; !ok {
			markAccountStatsDeletedLocked(id)
		}
	}
	for id, account := range nextByID {
		previousAccount, existed := previousByID[id]
		generation := markAccountStatsAddedLocked(id)
		if existed && accountStatsCountersRegressed(previousAccount, account) {
			accountStatsGenerationByID[id] = generation + 1
		}
	}
}

// GetAccountsWithStatsGeneration captures account records, lifecycle generation,
// and durable stats version under one config lock so Pool.Reload cannot combine
// observations from different config states.
func GetAccountsWithStatsGeneration() []AccountWithStatsGeneration {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return nil
	}
	result := make([]AccountWithStatsGeneration, 0, len(cfg.Accounts))
	for _, account := range cfg.Accounts {
		id := normalizeAccountStatsID(account.ID)
		generation := markAccountStatsAddedLocked(id)
		lifecycleGeneration := markAccountLifecycleAddedLocked(account.ID)
		accountSnapshot := account.Clone()
		accountSnapshot.DispatchGeneration = lifecycleGeneration
		result = append(result, AccountWithStatsGeneration{
			Account:             accountSnapshot,
			LifecycleGeneration: lifecycleGeneration,
			StatsGeneration:     generation,
			StatsVersion: accountStatsAppliedVersion[accountStatsVersionKey{
				id: id, generation: generation,
			}],
		})
	}
	return result
}

// UpdateAccountStatsBatchIfGeneration validates lifecycle and version at the
// persistence boundary. Stale generations/versions are resolved as superseded;
// future generations or a missing account in the current generation are errors.
// The in-memory config and applied-version map are published only after the full
// config file has been written successfully.
func UpdateAccountStatsBatchIfGeneration(writes []AccountStatsWrite) ([]AccountStatsWriteResult, error) {
	if len(writes) == 0 {
		return nil, nil
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return nil, fmt.Errorf("configuration is not initialized")
	}

	results := make([]AccountStatsWriteResult, len(writes))
	accountIndex := make(map[string]int, len(cfg.Accounts))
	for i, account := range cfg.Accounts {
		id := normalizeAccountStatsID(account.ID)
		if id == "" {
			continue
		}
		if _, duplicate := accountIndex[id]; duplicate {
			return nil, fmt.Errorf("duplicate account id in configuration: %s", id)
		}
		accountIndex[id] = i
		markAccountStatsAddedLocked(id)
	}

	next := cfg.Clone()
	stagedVersions := make(map[accountStatsVersionKey]uint64, len(accountStatsAppliedVersion))
	for key, version := range accountStatsAppliedVersion {
		stagedVersions[key] = version
	}

	applied := false
	for i, write := range writes {
		id := normalizeAccountStatsID(write.AccountID)
		results[i] = AccountStatsWriteResult{
			AccountID:  id,
			Generation: write.Generation,
			Version:    write.Version,
		}
		if id == "" || write.Generation == 0 || write.Version == 0 {
			return results, fmt.Errorf("invalid account stats write at index %d", i)
		}

		currentGeneration := ensureAccountStatsGenerationLocked(id)
		index, present := accountIndex[id]
		knownPresent := accountStatsPresentByID[id] && present
		switch {
		case write.Generation < currentGeneration:
			results[i].Superseded = true
			continue
		case write.Generation > currentGeneration:
			return results, fmt.Errorf(
				"account %s stats generation %d is ahead of current generation %d",
				id, write.Generation, currentGeneration,
			)
		case !knownPresent:
			return results, fmt.Errorf(
				"account %s is missing at stats generation %d",
				id, write.Generation,
			)
		}

		versionKey := accountStatsVersionKey{id: id, generation: write.Generation}
		if write.Version <= stagedVersions[versionKey] {
			results[i].Superseded = true
			continue
		}
		stagedVersions[versionKey] = write.Version
		next.Accounts[index].RequestCount = write.Counters.RequestCount
		next.Accounts[index].ErrorCount = write.Counters.ErrorCount
		next.Accounts[index].TotalTokens = write.Counters.TotalTokens
		next.Accounts[index].TotalCredits = write.Counters.TotalCredits
		next.Accounts[index].LastUsed = write.Counters.LastUsed
		applied = true
	}

	if !applied {
		return results, nil
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return results, err
	}
	published, err := writeConfigBytes(cfgPath, data, 0600)
	if !published {
		return results, err
	}
	// The new image is visible at cfgPath. Keep memory aligned even if a later
	// durability barrier failed, but only advance the durable version after every
	// barrier succeeds so the persistence worker retries the same snapshot.
	cfg = next
	if err != nil {
		return results, err
	}
	accountStatsAppliedVersion = stagedVersions
	return results, nil
}
