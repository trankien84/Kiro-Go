package proxy

import (
	"context"
	"errors"
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

func refreshCredentialStateChanged(before, after config.Account) bool {
	return before.AccessToken != after.AccessToken ||
		before.RefreshToken != after.RefreshToken ||
		before.ExpiresAt != after.ExpiresAt ||
		before.ProfileArn != after.ProfileArn
}

func (h *Handler) refreshAccountToken(account *config.Account, force bool) (bool, error) {
	return h.refreshAccountTokenContext(context.Background(), account, force)
}

func (h *Handler) refreshAccountTokenContext(ctx context.Context, account *config.Account, force bool) (bool, error) {
	return h.refreshAccountTokenWithPolicyContext(
		ctx,
		account,
		force,
		time.Duration(tokenRefreshSkewSeconds)*time.Second,
		true,
	)
}

func (h *Handler) refreshAccountTokenIfExpiringWithin(account *config.Account, minValidity time.Duration, reloadPool bool) (bool, error) {
	return h.refreshAccountTokenIfExpiringWithinContext(context.Background(), account, minValidity, reloadPool)
}

func (h *Handler) refreshAccountTokenIfExpiringWithinContext(ctx context.Context, account *config.Account, minValidity time.Duration, reloadPool bool) (bool, error) {
	return h.refreshAccountTokenWithPolicyContext(ctx, account, false, minValidity, reloadPool)
}

// refreshAccountTokenWithPolicy delegates the authoritative OAuth rotation
// transaction to auth.RefreshToken. The auth package owns per-lifecycle
// serialization and the lifecycle-CAS persistence boundary; this layer only
// validates account kind, maps stale-lifecycle errors into request semantics,
// and optionally republishes the committed config image into the runtime pool.
// Background reconciliation batches pool publication by passing reloadPool=false.
func (h *Handler) refreshAccountTokenWithPolicy(account *config.Account, force bool, minValidity time.Duration, reloadPool bool) (bool, error) {
	return h.refreshAccountTokenWithPolicyContext(context.Background(), account, force, minValidity, reloadPool)
}

func (h *Handler) refreshAccountTokenWithPolicyContext(ctx context.Context, account *config.Account, force bool, minValidity time.Duration, reloadPool bool) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if account == nil || strings.TrimSpace(account.ID) == "" {
		return false, fmt.Errorf("account is required for token refresh")
	}
	if !account.HasKnownKind() {
		return false, &unsupportedAccountKindError{AuthMethod: account.AuthMethod}
	}
	if !account.SupportsKiroTokenRefresh() {
		return false, nil
	}

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

	// Adopt a concurrent winner before entering auth. The auth layer repeats this
	// check under its lifecycle lock, so a race between here and there still
	// collapses to one IdP exchange.
	if minValidity < 0 {
		minValidity = 0
	}
	if !force && strings.TrimSpace(working.AccessToken) != "" &&
		(working.ExpiresAt == 0 || time.Now().Before(time.Unix(working.ExpiresAt, 0).Add(-minValidity))) {
		copyRefreshCredentialState(account, working)
		return false, nil
	}
	if strings.TrimSpace(working.RefreshToken) == "" {
		return false, fmt.Errorf("account %s has no refresh token", working.ID)
	}

	before := working.Clone()
	var err error
	if force {
		_, _, _, _, err = auth.RefreshTokenForceContext(ctx, &working)
	} else {
		_, _, _, _, err = auth.RefreshTokenIfExpiringWithinContext(ctx, &working, minValidity)
	}
	if err != nil {
		if errors.Is(err, auth.ErrAccountLifecycleChanged) || errors.Is(err, auth.ErrAccountCredentialChanged) {
			return false, errStaleAccountSelection
		}
		var persistenceErr *auth.AccountCredentialPersistenceError
		if errors.As(err, &persistenceErr) {
			// A post-publication durability error is not a rollback: config already
			// exposes C2. Re-read/adopt C2 and align the pool before propagating the
			// durability error. A pre-publication failure keeps C1 canonical and the
			// auth lifecycle entry retains C2 for a local-only retry.
			if persistenceErr.Published {
				persisted, ok := config.GetAccountByID(working.ID)
				if !ok {
					return false, errStaleAccountSelection
				}
				effectiveGeneration := generation
				if effectiveGeneration == 0 {
					effectiveGeneration = working.DispatchGeneration
				}
				if effectiveGeneration == 0 || persisted.DispatchGeneration != effectiveGeneration {
					return false, errStaleAccountSelection
				}
				changed := refreshCredentialStateChanged(before, persisted)
				copyRefreshCredentialState(account, persisted)
				if reloadPool && changed && h.pool != nil {
					h.pool.Reload()
				}
				return changed, fmt.Errorf("persist refreshed token for account %s: %w", working.ID, persistenceErr)
			}
			return false, fmt.Errorf("persist refreshed token for account %s: %w", working.ID, persistenceErr)
		}
		return false, &accountTokenRefreshError{err: err}
	}

	persisted, ok := config.GetAccountByID(working.ID)
	if !ok {
		return false, errStaleAccountSelection
	}
	effectiveGeneration := generation
	if effectiveGeneration == 0 {
		effectiveGeneration = working.DispatchGeneration
	}
	if effectiveGeneration == 0 || persisted.DispatchGeneration != effectiveGeneration {
		return false, errStaleAccountSelection
	}

	changed := refreshCredentialStateChanged(before, persisted)
	copyRefreshCredentialState(account, persisted)
	if reloadPool && changed && h.pool != nil {
		h.pool.Reload()
	}
	return changed, nil
}
