package proxy

import (
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	"strings"
	"time"
)

func copyRefreshCredentialState(dst *config.Account, src config.Account) {
	if dst == nil {
		return
	}
	dst.AccessToken = src.AccessToken
	dst.RefreshToken = src.RefreshToken
	dst.ExpiresAt = src.ExpiresAt
	dst.ProfileArn = src.ProfileArn
	dst.DispatchGeneration = src.DispatchGeneration
}

// refreshAccountToken serializes the complete Kiro OAuth rotation lifecycle.
// It always refreshes from the authoritative persisted credential, commits the
// access token + rotated refresh token + expiry + profile ARN as one state
// transition, and only then rebuilds the runtime pool. Generation-aware callers
// are fenced so an in-flight refresh can never publish into a delete/recreated
// account that reused the same textual ID.
func (h *Handler) refreshAccountToken(account *config.Account, force bool) (bool, error) {
	if account == nil || strings.TrimSpace(account.ID) == "" {
		return false, fmt.Errorf("account is required for token refresh")
	}
	if !account.HasKnownKind() {
		return false, &unsupportedAccountKindError{AuthMethod: account.AuthMethod}
	}
	if !account.SupportsKiroTokenRefresh() {
		return false, nil
	}

	h.tokenRefreshMu.Lock()
	defer h.tokenRefreshMu.Unlock()

	latest, ok := config.GetAccountByID(account.ID)
	if !ok {
		return false, errStaleAccountSelection
	}
	generation := account.DispatchGeneration
	if generation != 0 && latest.DispatchGeneration != generation {
		return false, errStaleAccountSelection
	}

	working := latest.Clone()
	if !working.HasKnownKind() {
		return false, &unsupportedAccountKindError{AuthMethod: working.AuthMethod}
	}
	if !working.SupportsKiroTokenRefresh() {
		copyRefreshCredentialState(account, working)
		return false, nil
	}

	// A concurrent request may already have refreshed this same lifecycle while
	// we waited for tokenRefreshMu. Adopt that complete persisted state and avoid
	// another IdP request unless the caller explicitly requested a force refresh.
	if !force && (working.ExpiresAt == 0 || time.Now().Unix() < working.ExpiresAt-tokenRefreshSkewSeconds) {
		copyRefreshCredentialState(account, working)
		return false, nil
	}
	if strings.TrimSpace(working.RefreshToken) == "" {
		return false, fmt.Errorf("account %s has no refresh token", working.ID)
	}

	refreshFn := auth.RefreshToken
	if force {
		refreshFn = auth.RefreshTokenForce
	}
	accessToken, refreshToken, expiresAt, profileArn, err := refreshFn(&working)
	if err != nil {
		return false, &accountTokenRefreshError{err: err}
	}
	if strings.TrimSpace(refreshToken) == "" {
		refreshToken = working.RefreshToken
	}
	if strings.TrimSpace(profileArn) == "" {
		profileArn = working.ProfileArn
	}

	if generation != 0 {
		applied, persistErr := config.UpdateAccountCredentialStateIfLifecycle(
			working.ID,
			generation,
			accessToken,
			refreshToken,
			expiresAt,
			profileArn,
		)
		if persistErr != nil {
			return false, fmt.Errorf("persist refreshed token for account %s: %w", working.ID, persistErr)
		}
		if !applied {
			return false, errStaleAccountSelection
		}
	} else if err := config.UpdateAccountCredentialState(
		working.ID,
		accessToken,
		refreshToken,
		expiresAt,
		profileArn,
	); err != nil {
		return false, fmt.Errorf("persist refreshed token for account %s: %w", working.ID, err)
	}

	// Publication order is deliberate: the config transaction above is the
	// authoritative commit. Runtime snapshots are rebuilt only after that commit
	// succeeds, so no request can observe a new access token paired with an old
	// rotated refresh token/profile ARN.
	if h.pool != nil {
		h.pool.Reload()
	}
	persisted, ok := config.GetAccountByID(working.ID)
	if !ok {
		return false, errStaleAccountSelection
	}
	if generation != 0 && persisted.DispatchGeneration != generation {
		return false, errStaleAccountSelection
	}
	copyRefreshCredentialState(account, persisted)
	return true, nil
}
