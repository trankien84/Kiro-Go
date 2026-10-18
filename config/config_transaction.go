package config

import (
	"encoding/json"
	"fmt"
)

type configTransactionOptions struct {
	reconcileAccountLifecycle bool
	reconcileAccountStats     bool
}

// persistConfigImageLocked writes next while cfgLock is held. published is true
// once the destination path contains the new image, even if a later durability
// barrier reports an error.
func persistConfigImageLocked(next *Config) (published bool, err error) {
	if next == nil {
		return false, fmt.Errorf("configuration is nil")
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return false, err
	}
	return writeConfigBytes(cfgPath, data, 0600)
}

// publishConfigTransactionLocked stages a deep copy and publishes memory only
// after the file image is visible. Post-publication durability errors still
// publish memory so process state cannot diverge from the visible disk image.
func publishConfigTransactionLocked(options configTransactionOptions, mutate func(next *Config) error) (bool, error) {
	if cfg == nil {
		return false, fmt.Errorf("configuration is not initialized")
	}
	previous := cfg
	next := cfg.Clone()
	if err := mutate(next); err != nil {
		return false, err
	}
	published, err := persistConfigImageLocked(next)
	if !published {
		return false, err
	}
	cfg = next
	if options.reconcileAccountStats {
		reconcileAccountStatsGenerationsLocked(previous.Accounts, next.Accounts)
	}
	if options.reconcileAccountLifecycle {
		reconcileAccountLifecycleGenerationsLocked(previous.Accounts, next.Accounts)
	}
	return true, err
}

func publishConfigFieldsLocked(mutate func(next *Config) error) (bool, error) {
	return publishConfigTransactionLocked(configTransactionOptions{}, mutate)
}

func publishConfigAccountsLocked(mutate func(next *Config) error) (bool, error) {
	return publishConfigTransactionLocked(configTransactionOptions{
		reconcileAccountLifecycle: true,
		reconcileAccountStats:     true,
	}, mutate)
}

// MutateAccountIfLifecycle applies a partial mutation against the canonical
// current account. Concurrent refresh-owned fields are preserved because mutate
// sees the newest record, not an old admin snapshot.
func MutateAccountIfLifecycle(id string, generation uint64, mutate func(account *Account) error) (before Account, after Account, applied bool, err error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil || !accountLifecycleMatchesLocked(id, generation) {
		return Account{}, Account{}, false, nil
	}
	index := -1
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			index = i
			before = cloneAccountWithLifecycleLocked(cfg.Accounts[i])
			break
		}
	}
	if index < 0 {
		return Account{}, Account{}, false, nil
	}
	published, err := publishConfigAccountsLocked(func(next *Config) error {
		if index >= len(next.Accounts) || next.Accounts[index].ID != id {
			return fmt.Errorf("account %s changed while staging mutation", id)
		}
		if err := mutate(&next.Accounts[index]); err != nil {
			return err
		}
		after = next.Accounts[index].Clone()
		return nil
	})
	if published {
		if currentGeneration := accountLifecycleGenerationLocked(after.ID); currentGeneration != 0 {
			after.DispatchGeneration = currentGeneration
		}
	}
	return before, after, published, err
}

// DeleteAccountIfLifecycle removes only the exact lifecycle observed by the
// caller. A late delete for N cannot remove a recreated N+1 with the same ID.
func DeleteAccountIfLifecycle(id string, generation uint64) (deleted Account, applied bool, err error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil || !accountLifecycleMatchesLocked(id, generation) {
		return Account{}, false, nil
	}
	index := -1
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			index = i
			deleted = cloneAccountWithLifecycleLocked(cfg.Accounts[i])
			break
		}
	}
	if index < 0 {
		return Account{}, false, nil
	}
	published, err := publishConfigAccountsLocked(func(next *Config) error {
		if index >= len(next.Accounts) || next.Accounts[index].ID != id {
			return fmt.Errorf("account %s changed while staging delete", id)
		}
		next.Accounts = append(next.Accounts[:index], next.Accounts[index+1:]...)
		return nil
	})
	return deleted, published, err
}

func publishAccountByIDLocked(id string, reconcile bool, mutate func(account *Account) error) (found bool, published bool, err error) {
	if cfg == nil {
		return false, false, fmt.Errorf("configuration is not initialized")
	}
	index := -1
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return false, false, nil
	}
	apply := func(next *Config) error {
		if index >= len(next.Accounts) || next.Accounts[index].ID != id {
			return fmt.Errorf("account %s changed while staging mutation", id)
		}
		return mutate(&next.Accounts[index])
	}
	if reconcile {
		published, err = publishConfigAccountsLocked(apply)
	} else {
		published, err = publishConfigFieldsLocked(apply)
	}
	return true, published, err
}

func applyAccountInfo(account *Account, info AccountInfo) {
	if info.Email != "" {
		account.Email = info.Email
	}
	if info.UserId != "" {
		account.UserId = info.UserId
	}
	account.SubscriptionType = info.SubscriptionType
	account.SubscriptionTitle = info.SubscriptionTitle
	account.DaysRemaining = info.DaysRemaining
	account.UsageCurrent = info.UsageCurrent
	account.UsageLimit = info.UsageLimit
	account.UsagePercent = info.UsagePercent
	account.NextResetDate = info.NextResetDate
	account.LastRefresh = info.LastRefresh
	account.TrialUsageCurrent = info.TrialUsageCurrent
	account.TrialUsageLimit = info.TrialUsageLimit
	account.TrialUsagePercent = info.TrialUsagePercent
	account.TrialStatus = info.TrialStatus
	account.TrialExpiresAt = info.TrialExpiresAt
}
