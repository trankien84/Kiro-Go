package proxy

import (
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"strings"
)

const maxAccountRetryAttempts = 3

func isQuotaErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	// Match the 429 status code only as a digit-boundary token (parity with
	// pool.HasStatusToken used by the auth classifier) so a stray "429" inside
	// an upstream body token/ID can't false-trigger RecordError(true). "quota"
	// remains a word marker.
	return pool.HasStatusToken(lower, "429") || strings.Contains(lower, "quota")
}

func isOverageErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	// 402 must be a digit-boundary token, not an arbitrary substring (parity
	// with pool.HasStatusToken), ANDed with the "overage" word marker.
	return pool.HasStatusToken(lower, "402") && strings.Contains(lower, "overage")
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

// isAuthErrorMessage reports whether upstream prose looks like an authentication
// failure. Native web-search paths use it for request/account classification;
// structured request paths still prefer pool.IsAuthFailure.
func isAuthErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "token invalid") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access token expired") ||
		strings.Contains(msg, "refresh token expired")
}

// shouldRetryAccountRefreshOnError reports whether a RefreshAccountInfo error
// looks like a stale/invalid token worth one token-refresh + retry in the admin
// "refresh account" endpoint (handler.go). This is a RETRY trigger, NOT a ban
// classifier — a false positive only costs a redundant refresh + retry. Status
// codes 401/403 are matched by digit boundary (pool.HasStatusToken) for parity
// with the ban classifiers, so a stray digit in a request ID/token can't fire a
// spurious refresh+retry; "invalid"/"expired" remain word markers.
func shouldRetryAccountRefreshOnError(msg string) bool {
	lower := strings.ToLower(msg)
	return pool.HasStatusToken(lower, "401") || pool.HasStatusToken(lower, "403") ||
		strings.Contains(lower, "invalid") || strings.Contains(lower, "expired")
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}
	if !account.Enabled && account.BanStatus == banStatus && account.BanReason == banReason {
		return
	}

	var (
		applied bool
		err     error
	)
	if account.DispatchGeneration != 0 {
		applied, err = config.SetAccountBanStatusIfLifecycle(
			account.ID,
			account.DispatchGeneration,
			banStatus,
			banReason,
		)
	} else {
		err = config.SetAccountBanStatus(account.ID, banStatus, banReason)
		applied = err == nil
	}
	if err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}
	if !applied {
		logger.Warnf("[AccountFailover] Ignored stale disable for account=%s generation=%d", account.ID, account.DispatchGeneration)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}
	if isStreamIntegrityError(err) || isKiroTransientTransportFailure(err) {
		logger.Warnf("[AccountFailover] Transport-scoped Kiro failure not applied to account health account=%s: %v", account.ID, err)
		return
	}
	if account.DispatchGeneration != 0 {
		h.handleAccountFailureForSelection(pool.AccountSelection{
			Account:             account.Clone(),
			LifecycleGeneration: account.DispatchGeneration,
		}, err)
		return
	}

	errMsg := err.Error()
	switch {
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, false)
	case isQuotaErrorMessage(errMsg):
		h.pool.RecordError(account.ID, true)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false)
	case pool.IsAuthFailure(err):
		h.disableAccount(account, "BANNED", "Authentication failed - token invalid or expired")
	default:
		h.pool.RecordError(account.ID, false)
	}
}

// disableAccountForSelection persists a request-derived terminal state only
// while the selected lifecycle is still current. A late response from lifecycle
// N must not disable a recreated account N+1 that reuses the same textual ID.
func (h *Handler) disableAccountForSelection(selection pool.AccountSelection, banStatus, banReason string) bool {
	applied, err := config.SetAccountBanStatusIfLifecycle(
		selection.Account.ID,
		selection.LifecycleGeneration,
		banStatus,
		banReason,
	)
	if err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", selection.Account.Email, err)
		return false
	}
	if !applied {
		logger.Warnf("[AccountFailover] Ignored stale disable for account=%s generation=%d", selection.Account.ID, selection.LifecycleGeneration)
		return false
	}
	logger.Warnf("[AccountFailover] Disabled %s: %s", selection.Account.Email, banReason)
	h.pool.Reload()
	return true
}

// disableAccountOverageForSelection refreshes and publishes overage state only
// for the lifecycle that produced the upstream 402.
func (h *Handler) disableAccountOverageForSelection(selection pool.AccountSelection) bool {
	account := &selection.Account
	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return false
	}
	applied, persistErr := config.UpdateAccountOverageStatusIfLifecycle(
		account.ID,
		selection.LifecycleGeneration,
		snap.Status,
		snap.Capability,
		snap.OverageCap,
		snap.OverageRate,
		snap.CurrentOverages,
		snap.CheckedAt,
	)
	if persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return false
	}
	if !applied {
		logger.Warnf("[AccountFailover] Ignored stale overage snapshot for account=%s generation=%d", account.ID, selection.LifecycleGeneration)
		return false
	}
	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
	return true
}

// handleAccountFailureForSelection fences every request-derived health/ban
// mutation with the immutable dispatch lease. The legacy ID-only helper remains
// for admin/background paths that intentionally target the current account.
func (h *Handler) handleAccountFailureForSelection(selection pool.AccountSelection, err error) {
	if err == nil {
		return
	}
	if !h.pool.IsSelectionCurrent(selection) {
		logger.Warnf("[AccountFailover] Ignored stale failure account=%s generation=%d: %v", selection.Account.ID, selection.LifecycleGeneration, err)
		return
	}
	if isStreamIntegrityError(err) || isKiroTransientTransportFailure(err) {
		// A transient Kiro socket/stream failure is endpoint/transport health, not
		// credential health. The Kiro transport has already exhausted its bounded
		// same-account retry budget before this error reaches account failover.
		logger.Warnf("[AccountFailover] Transport-scoped Kiro failure not applied to account health account=%s generation=%d: %v", selection.Account.ID, selection.LifecycleGeneration, err)
		return
	}

	errMsg := err.Error()
	switch {
	case isOverageErrorMessage(errMsg):
		if h.disableAccountOverageForSelection(selection) {
			// The lifecycle can remain current after an overage refresh; record the
			// request error only when the same lease still owns runtime state.
			h.pool.RecordErrorForSelection(selection, false)
		}
	case isQuotaErrorMessage(errMsg):
		h.pool.RecordErrorForSelection(selection, true)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccountForSelection(selection, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		h.pool.RecordErrorForSelection(selection, false)
	case pool.IsAuthFailure(err):
		h.disableAccountForSelection(selection, "BANNED", "Authentication failed - token invalid or expired")
	default:
		h.pool.RecordErrorForSelection(selection, false)
	}
}
