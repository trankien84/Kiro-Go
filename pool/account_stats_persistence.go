package pool

import (
	"fmt"
	"strings"

	"kiro-go/config"
)

func persistAccountStatsBatch(snapshots []accountStatsSnapshot) error {
	writes := make([]config.AccountStatsWrite, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.skipPersist {
			continue
		}
		writes = append(writes, config.AccountStatsWrite{
			AccountID:  snapshot.accountID(),
			Generation: snapshot.key.epoch,
			Version:    snapshot.version,
			Counters: config.AccountStatsCounters{
				RequestCount: snapshot.requestCount,
				ErrorCount:   snapshot.errorCount,
				TotalTokens:  snapshot.totalTokens,
				TotalCredits: snapshot.totalCredits,
				LastUsed:     snapshot.lastUsed,
			},
		})
	}
	if len(writes) == 0 {
		return nil
	}
	if _, err := config.UpdateAccountStatsBatchIfGeneration(writes); err != nil {
		return fmt.Errorf("persist account stats batch: %w", err)
	}
	return nil
}

// poolStatsAccountID validates an account ID without normalizing it. Config owns
// exact account identity, so the pool must not collapse "a" and " a ".
func poolStatsAccountID(id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	return id
}

func applyStatsSnapshot(account *config.Account, snapshot accountStatsSnapshot) {
	account.RequestCount = snapshot.requestCount
	account.ErrorCount = snapshot.errorCount
	account.TotalTokens = snapshot.totalTokens
	account.TotalCredits = snapshot.totalCredits
	account.LastUsed = snapshot.lastUsed
}

// syncStatsEpochsLocked imports config-owned lifecycle generations. Caller MUST
// hold p.mu. The newest accepted cumulative snapshot is retained independently
// of the routable account slice so Reload cannot roll counters backward while
// asynchronous persistence is still behind.
func (p *AccountPool) syncStatsEpochsLocked(inventory []config.AccountWithStatsGeneration) map[string]uint64 {
	if p.statsEpochByID == nil {
		p.statsEpochByID = make(map[string]uint64)
	}
	if p.statsVersionByID == nil {
		p.statsVersionByID = make(map[string]uint64)
	}
	if p.statsCurrentByID == nil {
		p.statsCurrentByID = make(map[string]accountStatsSnapshot)
	}
	if p.statsLedgerByID == nil {
		p.statsLedgerByID = make(map[string]accountStatsSnapshot)
	}

	active := make(map[string]uint64, len(inventory))
	for _, item := range inventory {
		id := poolStatsAccountID(item.Account.ID)
		if id == "" || item.StatsGeneration == 0 {
			continue
		}
		durable := accountStatsSnapshot{
			key:          accountStatsKey{id: id, epoch: item.StatsGeneration},
			version:      item.StatsVersion,
			requestCount: item.Account.RequestCount,
			errorCount:   item.Account.ErrorCount,
			totalTokens:  item.Account.TotalTokens,
			totalCredits: item.Account.TotalCredits,
			lastUsed:     item.Account.LastUsed,
		}

		if p.statsEpochByID[id] != item.StatsGeneration {
			// A new stats lifecycle must start from the config-owned durable state.
			// Pending/ledger state from the previous generation is never inherited.
			p.statsVersionByID[id] = item.StatsVersion
			delete(p.statsCurrentByID, id)
			p.statsLedgerByID[id] = durable
		} else {
			if p.statsVersionByID[id] < item.StatsVersion {
				p.statsVersionByID[id] = item.StatsVersion
			}

			// statsCurrentByID retains only accepted snapshots that persistence has
			// not caught up with yet. Keep that contract separate from the ledger,
			// which survives routability changes and is the base for in-flight
			// request completions.
			if snapshot, ok := p.statsCurrentByID[id]; ok &&
				snapshot.key.epoch == item.StatsGeneration &&
				snapshot.version <= item.StatsVersion {
				delete(p.statsCurrentByID, id)
			}

			ledger, ok := p.statsLedgerByID[id]
			if !ok || ledger.key.epoch != item.StatsGeneration || ledger.version <= item.StatsVersion {
				ledger = durable
			}
			if pending, ok := p.statsCurrentByID[id]; ok &&
				pending.key.epoch == item.StatsGeneration &&
				pending.version > ledger.version {
				ledger = pending
			}
			p.statsLedgerByID[id] = ledger
		}
		p.statsEpochByID[id] = item.StatsGeneration
		active[id] = item.StatsGeneration
	}

	for id := range p.statsCurrentByID {
		if _, ok := active[id]; !ok {
			delete(p.statsCurrentByID, id)
		}
	}
	for id := range p.statsLedgerByID {
		if _, ok := active[id]; !ok {
			delete(p.statsLedgerByID, id)
		}
	}
	return active
}
func (p *AccountPool) mergeCurrentStatsLocked(item config.AccountWithStatsGeneration) config.Account {
	account := item.Account
	id := poolStatsAccountID(account.ID)
	if id == "" {
		return account
	}
	snapshot, ok := p.statsCurrentByID[id]
	if !ok || snapshot.key.epoch != item.StatsGeneration || snapshot.version <= item.StatsVersion {
		return account
	}
	applyStatsSnapshot(&account, snapshot)
	return account
}
