package config

import (
	"fmt"
	"reflect"
	"strings"
	"time"
)

var (
	accountLifecycleGenerationByID = make(map[string]uint64)
	accountLifecyclePresentByID    = make(map[string]bool)
)

// normalizeAccountLifecycleID validates emptiness without canonicalizing the
// configured ID. Account IDs are exact identities; "a" and " a " are distinct.
func normalizeAccountLifecycleID(id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	return id
}

func ensureAccountLifecycleGenerationLocked(id string) uint64 {
	id = normalizeAccountLifecycleID(id)
	if id == "" {
		return 0
	}
	generation := accountLifecycleGenerationByID[id]
	if generation == 0 {
		generation = 1
		accountLifecycleGenerationByID[id] = generation
	}
	return generation
}

func markAccountLifecycleAddedLocked(id string) uint64 {
	id = normalizeAccountLifecycleID(id)
	generation := ensureAccountLifecycleGenerationLocked(id)
	if generation != 0 {
		accountLifecyclePresentByID[id] = true
	}
	return generation
}

func markAccountLifecycleDeletedLocked(id string) {
	id = normalizeAccountLifecycleID(id)
	if id == "" {
		return
	}
	generation := ensureAccountLifecycleGenerationLocked(id)
	if accountLifecyclePresentByID[id] {
		generation++
		accountLifecycleGenerationByID[id] = generation
	}
	accountLifecyclePresentByID[id] = false
	forgetAccountInfoRevisionLocked(id)
}

// accountDispatchIdentityChanged reports whether an UpdateAccount/Load replaced
// the logical upstream target behind the same ID. Runtime-only fields such as
// enabled/ban/quota/stat counters intentionally do not start a new lifecycle.
// Dedicated token/profile refresh mutators also keep the current lifecycle; a
// manual config replacement of credentials does not.
func accountDispatchIdentityChanged(previous, next Account) bool {
	if previous.AuthMethod != next.AuthMethod ||
		previous.Provider != next.Provider ||
		previous.BaseURL != next.BaseURL ||
		previous.KiroApiKey != next.KiroApiKey ||
		previous.AccessToken != next.AccessToken ||
		previous.RefreshToken != next.RefreshToken ||
		previous.ClientID != next.ClientID ||
		previous.ClientSecret != next.ClientSecret ||
		previous.TokenEndpoint != next.TokenEndpoint ||
		previous.IssuerURL != next.IssuerURL ||
		previous.Scopes != next.Scopes ||
		previous.StartUrl != next.StartUrl ||
		previous.Region != next.Region ||
		previous.MachineId != next.MachineId ||
		previous.ProfileArn != next.ProfileArn ||
		previous.BedrockAccessKeyID != next.BedrockAccessKeyID ||
		previous.BedrockSecretAccessKey != next.BedrockSecretAccessKey ||
		previous.BedrockSessionToken != next.BedrockSessionToken ||
		previous.BedrockAPIKey != next.BedrockAPIKey ||
		previous.BedrockUseConverse != next.BedrockUseConverse ||
		previous.ProxyURL != next.ProxyURL {
		return true
	}
	return !reflect.DeepEqual(previous.BedrockRegions, next.BedrockRegions) ||
		!reflect.DeepEqual(previous.BedrockModelMap, next.BedrockModelMap)
}

func markAccountLifecycleReplacedLocked(previous, next Account) {
	previousID := normalizeAccountLifecycleID(previous.ID)
	nextID := normalizeAccountLifecycleID(next.ID)
	if previousID != nextID {
		markAccountLifecycleDeletedLocked(previousID)
		markAccountLifecycleAddedLocked(nextID)
		return
	}
	if nextID == "" {
		return
	}
	generation := markAccountLifecycleAddedLocked(nextID)
	if accountDispatchIdentityChanged(previous, next) {
		accountLifecycleGenerationByID[nextID] = generation + 1
	}
}

// reconcileAccountLifecycleGenerationsLocked aligns lifecycle generations with
// a whole-config replacement performed by Load. Caller MUST hold cfgLock.
func reconcileAccountLifecycleGenerationsLocked(previous, next []Account) {
	previousByID := make(map[string]Account, len(previous))
	for _, account := range previous {
		if id := normalizeAccountLifecycleID(account.ID); id != "" {
			previousByID[id] = account
		}
	}
	nextByID := make(map[string]Account, len(next))
	for _, account := range next {
		if id := normalizeAccountLifecycleID(account.ID); id != "" {
			nextByID[id] = account
		}
	}

	for id := range previousByID {
		if _, ok := nextByID[id]; !ok {
			markAccountLifecycleDeletedLocked(id)
		}
	}
	for id, account := range nextByID {
		previousAccount, existed := previousByID[id]
		generation := markAccountLifecycleAddedLocked(id)
		if existed && accountDispatchIdentityChanged(previousAccount, account) {
			accountLifecycleGenerationByID[id] = generation + 1
		}
	}
}

// accountLifecycleGenerationLocked returns the current process-local generation
// without mutating lifecycle bookkeeping. Caller MUST hold cfgLock.
func accountLifecycleGenerationLocked(id string) uint64 {
	id = normalizeAccountLifecycleID(id)
	if id == "" || !accountLifecyclePresentByID[id] {
		return 0
	}
	return accountLifecycleGenerationByID[id]
}

// GetAccountLifecycleGeneration returns the current lifecycle fence for an
// existing account ID. IDs are exact; whitespace is only used to reject blank IDs.
func GetAccountLifecycleGeneration(id string) (uint64, bool) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	generation := accountLifecycleGenerationLocked(id)
	return generation, generation != 0
}

// cloneAccountWithLifecycleLocked makes a deep account snapshot and stamps the
// lifecycle fence that was current under the same config lock.
func cloneAccountWithLifecycleLocked(account Account) Account {
	out := account.Clone()
	out.DispatchGeneration = accountLifecycleGenerationLocked(account.ID)
	out.AccountInfoRevision = accountInfoRevisionLocked(account.ID)
	return out
}

func accountLifecycleMatchesLocked(id string, generation uint64) bool {
	id = normalizeAccountLifecycleID(id)
	if id == "" || generation == 0 || !accountLifecyclePresentByID[id] {
		return false
	}
	return ensureAccountLifecycleGenerationLocked(id) == generation
}

// AccountLifecycleMatches reports whether id still denotes the lifecycle that a
// previously dispatched request selected.
func AccountLifecycleMatches(id string, generation uint64) bool {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	return accountLifecycleMatchesLocked(id, generation)
}

// SetAccountBanStatusIfLifecycle prevents an old request from disabling a newly
// recreated account that happens to reuse the same textual ID.
func SetAccountBanStatusIfLifecycle(id string, generation uint64, status, reason string) (bool, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if !accountLifecycleMatchesLocked(id, generation) {
		return false, nil
	}
	found, published, err := publishAccountByIDLocked(id, false, func(account *Account) error {
		account.BanStatus = status
		account.BanReason = reason
		account.BanTime = time.Now().Unix()
		if status == "BANNED" || status == "DISABLED" || status == "BILLING_BLOCKED" {
			account.Enabled = false
		}
		return nil
	})
	return found && published, err
}

func UpdateAccountTokenIfLifecycle(id string, generation uint64, accessToken, refreshToken string, expiresAt int64) (bool, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if !accountLifecycleMatchesLocked(id, generation) {
		return false, nil
	}
	found, published, err := publishAccountByIDLocked(id, false, func(account *Account) error {
		account.AccessToken = accessToken
		if refreshToken != "" {
			account.RefreshToken = refreshToken
		}
		account.ExpiresAt = expiresAt
		return nil
	})
	return found && published, err
}

func UpdateAccountOverageStatusIfLifecycle(id string, generation uint64, status, capability string, cap, rate, current float64, checkedAt int64) (bool, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if !accountLifecycleMatchesLocked(id, generation) {
		return false, nil
	}
	found, published, err := publishAccountByIDLocked(id, false, func(account *Account) error {
		if status != "" {
			account.OverageStatus = status
		}
		if capability != "" {
			account.OverageCapability = capability
		}
		account.OverageCap = cap
		account.OverageRate = rate
		account.CurrentOverages = current
		if checkedAt > 0 {
			account.OverageCheckedAt = checkedAt
		}
		return nil
	})
	return found && published, err
}

func UpdateAccountProfileArnIfLifecycle(id string, generation uint64, profileArn string) (bool, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if !accountLifecycleMatchesLocked(id, generation) {
		return false, nil
	}
	found, published, err := publishAccountByIDLocked(id, false, func(account *Account) error { account.ProfileArn = profileArn; return nil })
	return found && published, err
}

func UpdateAccountInfoIfLifecycle(id string, generation uint64, info AccountInfo) (bool, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if !accountLifecycleMatchesLocked(id, generation) {
		return false, nil
	}
	found, published, err := publishAccountByIDLocked(id, false, func(account *Account) error { applyAccountInfo(account, info); return nil })
	applied := found && published
	if applied {
		bumpAccountInfoRevisionLocked(id)
	}
	return applied, err
}

// UpdateAccountRegionIfLifecycle persists a validated region only if the
// selected account lifecycle is still current. Region changes dispatch identity,
// so a successful update advances the lifecycle generation for future requests.
func UpdateAccountRegionIfLifecycle(id string, generation uint64, region string) (bool, error) {
	if err := validateKiroRegionHostLabel(region); err != nil {
		return false, fmt.Errorf("invalid region %q: %w", region, err)
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if !accountLifecycleMatchesLocked(id, generation) {
		return false, nil
	}
	found, published, err := publishAccountByIDLocked(id, true, func(account *Account) error {
		account.Region = strings.TrimSpace(region)
		return nil
	})
	return found && published, err
}

// UpdateAccountCredentialStateIfLifecycle is the request-safe counterpart of
// UpdateAccountCredentialState. A refresh finishing for lifecycle N cannot write
// tokens/profile data into a recreated lifecycle N+1 with the same textual ID.
func UpdateAccountCredentialStateIfLifecycle(
	id string,
	generation uint64,
	accessToken string,
	refreshToken string,
	expiresAt int64,
	profileArn string,
) (bool, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if !accountLifecycleMatchesLocked(id, generation) {
		return false, nil
	}
	found, published, err := publishAccountByIDLocked(id, false, func(account *Account) error {
		if account.RefreshTokenFingerprint == "" {
			account.RefreshTokenFingerprint = RefreshTokenFingerprint(account.RefreshToken)
		}
		account.AccessToken = accessToken
		if refreshToken != "" {
			account.RefreshToken = refreshToken
		}
		account.ExpiresAt = expiresAt
		if profileArn != "" {
			account.ProfileArn = profileArn
		}
		return nil
	})
	return found && published, err
}
