package config

import (
	"strings"
	"time"
)

// accountInfoProjectionMatches reports whether the durable account-info fields
// are still exactly the snapshot that started an asynchronous info operation.
// AccountInfoRevision below is the authoritative operation-order fence; retaining
// the full projection here adds defense in depth against accidental mutation paths
// that fail to advance the revision.
func accountInfoProjectionMatches(current *Account, observed Account) bool {
	if current == nil {
		return false
	}
	return current.Email == observed.Email &&
		current.UserId == observed.UserId &&
		current.SubscriptionType == observed.SubscriptionType &&
		current.SubscriptionTitle == observed.SubscriptionTitle &&
		current.DaysRemaining == observed.DaysRemaining &&
		current.UsageCurrent == observed.UsageCurrent &&
		current.UsageLimit == observed.UsageLimit &&
		current.UsagePercent == observed.UsagePercent &&
		current.NextResetDate == observed.NextResetDate &&
		current.LastRefresh == observed.LastRefresh &&
		current.TrialUsageCurrent == observed.TrialUsageCurrent &&
		current.TrialUsageLimit == observed.TrialUsageLimit &&
		current.TrialUsagePercent == observed.TrialUsagePercent &&
		current.TrialStatus == observed.TrialStatus &&
		current.TrialExpiresAt == observed.TrialExpiresAt
}

// accountInfoObservationMatchesLocked verifies that an asynchronous account-info
// result still describes both the credential and account-info snapshot that
// initiated it. DispatchGeneration protects delete/recreate and identity
// replacement; access-token/expiry/profile additionally fence normal same-
// lifecycle credential/profile progress. AccountInfoRevision distinguishes every
// successful account-info publication, even if it writes byte-for-byte identical
// values inside the same Unix second. Caller MUST
// hold cfgLock for writing because accountLifecycleMatchesLocked may initialize
// lifecycle bookkeeping.
func accountInfoObservationMatchesLocked(observed Account) bool {
	if cfg == nil || observed.DispatchGeneration == 0 ||
		!accountLifecycleMatchesLocked(observed.ID, observed.DispatchGeneration) {
		return false
	}
	for i := range cfg.Accounts {
		account := &cfg.Accounts[i]
		if account.ID != observed.ID {
			continue
		}
		return account.AccessToken == observed.AccessToken &&
			account.RefreshToken == observed.RefreshToken &&
			account.ExpiresAt == observed.ExpiresAt &&
			account.ProfileArn == observed.ProfileArn &&
			account.Enabled == observed.Enabled &&
			account.BanStatus == observed.BanStatus &&
			account.BanReason == observed.BanReason &&
			account.BanTime == observed.BanTime &&
			accountInfoRevisionLocked(observed.ID) == observed.AccountInfoRevision &&
			accountInfoProjectionMatches(account, observed)
	}
	return false
}

// AccountInfoObservationMatches is the read/check form used before recording a
// transient background error. Publication helpers below repeat the same check
// atomically under the config transaction lock before mutating durable state.
func AccountInfoObservationMatches(observed Account) bool {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	return accountInfoObservationMatchesLocked(observed)
}

// UpdateAccountInfoIfObserved publishes quota/subscription data only when
// neither the account lifecycle, OAuth/profile observation, nor a newer account-
// info publication revision has superseded the network operation.
func UpdateAccountInfoIfObserved(observed Account, info AccountInfo) (bool, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if !accountInfoObservationMatchesLocked(observed) {
		return false, nil
	}
	found, published, err := publishAccountByIDLocked(observed.ID, false, func(account *Account) error {
		applyAccountInfo(account, info)
		return nil
	})
	applied := found && published
	if applied {
		bumpAccountInfoRevisionLocked(observed.ID)
	}
	return applied, err
}

// SetAccountBanStatusIfObserved is the health-state counterpart of
// UpdateAccountInfoIfObserved. In particular, a 401 returned for access token T1
// cannot disable the same account after a concurrent OAuth refresh has already
// published T2 without changing DispatchGeneration.
func SetAccountBanStatusIfObserved(observed Account, status, reason string) (bool, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if !accountInfoObservationMatchesLocked(observed) {
		return false, nil
	}
	found, published, err := publishAccountByIDLocked(observed.ID, false, func(account *Account) error {
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

// accountCredentialObservationMatchesLocked verifies that the exact OAuth
// credential snapshot which started a refresh operation is still canonical.
// A normal token rotation intentionally does not advance DispatchGeneration, so
// lifecycle fencing alone cannot distinguish a late failure produced by T1 from
// the successfully published T2 credential in the same account lifecycle.
func accountCredentialObservationMatchesLocked(observed Account) bool {
	if cfg == nil || observed.DispatchGeneration == 0 ||
		!accountLifecycleMatchesLocked(observed.ID, observed.DispatchGeneration) {
		return false
	}
	for i := range cfg.Accounts {
		account := &cfg.Accounts[i]
		if account.ID != observed.ID {
			continue
		}
		return account.AccessToken == observed.AccessToken &&
			account.RefreshToken == observed.RefreshToken &&
			account.ExpiresAt == observed.ExpiresAt &&
			account.Enabled == observed.Enabled &&
			account.BanStatus == observed.BanStatus &&
			account.BanReason == observed.BanReason &&
			account.BanTime == observed.BanTime
	}
	return false
}

// AccountCredentialObservationMatches is the read/check form for non-mutating
// refresh-failure handling. Terminal health publication must use the atomic
// conditional helper below instead of check-then-write.
func AccountCredentialObservationMatches(observed Account) bool {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	return accountCredentialObservationMatchesLocked(observed)
}

// SetAccountBanStatusIfCredentialObserved applies a terminal OAuth refresh
// failure only while the credential that produced that failure is still the
// canonical credential for the same account lifecycle.
func SetAccountBanStatusIfCredentialObserved(observed Account, status, reason string) (bool, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if !accountCredentialObservationMatchesLocked(observed) {
		return false, nil
	}
	found, published, err := publishAccountByIDLocked(observed.ID, false, func(account *Account) error {
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

// RecoverDisabledAccountIfCredentialObserved atomically publishes the terminal
// transition of legacy failure-driven auto-recovery. The recovery is allowed
// only for the exact disabled OAuth credential/lifecycle that was validated by
// the token endpoint. Any operator edit, credential rotation, health transition,
// or delete/recreate supersedes the observation and makes this a no-op.
//
// published=true remains authoritative even when the config durability barrier
// returns an error: the visible file image and in-memory state already contain
// the recovered account in that case.
func RecoverDisabledAccountIfCredentialObserved(observed Account) (bool, error) {
	if observed.Enabled ||
		!observed.SupportsKiroTokenRefresh() ||
		strings.TrimSpace(observed.RefreshToken) == "" ||
		!strings.EqualFold(strings.TrimSpace(observed.BanStatus), "DISABLED") ||
		strings.TrimSpace(observed.BanReason) == "" {
		return false, nil
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	if !accountCredentialObservationMatchesLocked(observed) {
		return false, nil
	}
	found, published, err := publishAccountByIDLocked(observed.ID, false, func(account *Account) error {
		account.Enabled = true
		account.BanStatus = "ACTIVE"
		account.BanReason = ""
		account.BanTime = 0
		return nil
	})
	return found && published, err
}

// accountCredentialPublicationObservationMatchesLocked is the commit fence for a
// successful OAuth exchange. Unlike accountCredentialObservationMatchesLocked,
// it intentionally ignores health/quota state: those fields may legitimately
// advance while an IdP request is in flight and must not discard a valid rotated
// credential. The source credential itself and lifecycle must still be exact.
func accountCredentialPublicationObservationMatchesLocked(observed Account) bool {
	if cfg == nil || observed.DispatchGeneration == 0 ||
		!accountLifecycleMatchesLocked(observed.ID, observed.DispatchGeneration) {
		return false
	}
	for i := range cfg.Accounts {
		account := &cfg.Accounts[i]
		if account.ID != observed.ID {
			continue
		}
		return account.AccessToken == observed.AccessToken &&
			account.RefreshToken == observed.RefreshToken &&
			account.ExpiresAt == observed.ExpiresAt
	}
	return false
}

// AccountCredentialPublicationObservationMatches reports whether a pending
// rotated credential can still be published into the lifecycle that produced
// it. It is primarily used to retire stale in-memory recovery candidates after a
// delete/recreate or a different credential advance.
func AccountCredentialPublicationObservationMatches(observed Account) bool {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	return accountCredentialPublicationObservationMatchesLocked(observed)
}

// UpdateAccountCredentialStateIfObserved atomically publishes the complete
// credential returned by an IdP only while the credential that was sent to that
// IdP remains canonical. Health/quota changes do not invalidate the result.
//
// ProfileArn is merged conservatively: if another operation advanced the profile
// after observation, keep that newer canonical profile rather than overwriting
// it with a stale value bundled with the OAuth response.
func UpdateAccountCredentialStateIfObserved(
	observed Account,
	accessToken string,
	refreshToken string,
	expiresAt int64,
	profileArn string,
) (bool, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if !accountCredentialPublicationObservationMatchesLocked(observed) {
		return false, nil
	}
	found, published, err := publishAccountByIDLocked(observed.ID, false, func(account *Account) error {
		if account.RefreshTokenFingerprint == "" {
			account.RefreshTokenFingerprint = RefreshTokenFingerprint(account.RefreshToken)
		}
		account.AccessToken = accessToken
		if refreshToken != "" {
			account.RefreshToken = refreshToken
		}
		account.ExpiresAt = expiresAt
		if profileArn != "" && account.ProfileArn == observed.ProfileArn {
			account.ProfileArn = profileArn
		}
		return nil
	})
	return found && published, err
}

// UpdateAccountInfoAndRecoverIfObserved is the manual/batch success publication
// primitive. It preserves the legacy recovery semantics (clear an existing
// non-ACTIVE ban marker, but never implicitly re-enable the account) while
// making quota/info and health recovery one observed transaction.
func UpdateAccountInfoAndRecoverIfObserved(observed Account, info AccountInfo) (bool, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if !accountInfoObservationMatchesLocked(observed) {
		return false, nil
	}
	found, published, err := publishAccountByIDLocked(observed.ID, false, func(account *Account) error {
		applyAccountInfo(account, info)
		if account.BanStatus != "" && account.BanStatus != "ACTIVE" {
			account.BanStatus = "ACTIVE"
			account.BanReason = ""
			account.BanTime = 0
		}
		return nil
	})
	applied := found && published
	if applied {
		bumpAccountInfoRevisionLocked(observed.ID)
	}
	return applied, err
}
