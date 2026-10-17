package proxy

import (
	"context"
	"fmt"
	"kiro-go/config"
	"strings"
	"time"
)

func canonicalAccountForStateRefresh(account *config.Account) (config.Account, error) {
	if account == nil || strings.TrimSpace(account.ID) == "" {
		return config.Account{}, fmt.Errorf("account is required for state refresh")
	}
	live, ok := config.GetAccountByID(account.ID)
	if !ok {
		return config.Account{}, errStaleAccountSelection
	}
	if account.DispatchGeneration != 0 && live.DispatchGeneration != account.DispatchGeneration {
		return config.Account{}, errStaleAccountSelection
	}
	*account = live.Clone()
	return live.Clone(), nil
}

// refreshObservedAccountState is the single manual/background/batch account-info
// state machine. Credential rotation (when actually due) is completed first;
// quota/info is then fetched without side effects and published only against the
// exact post-credential observation that initiated that network request.
func (h *Handler) refreshObservedAccountState(
	ctx context.Context,
	account *config.Account,
) (*accountRefreshOutcome, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	canonical, err := canonicalAccountForStateRefresh(account)
	if err != nil {
		return nil, err
	}

	if canonical.SupportsKiroTokenRefresh() && accountCredentialNeedsRefresh(&canonical, time.Now().Unix()) {
		if _, err := h.refreshAccountTokenContext(ctx, &canonical, false); err != nil {
			return nil, err
		}
	}

	// Re-read after the credential lane. This captures the credential/profile,
	// health fields, and AccountInfoRevision that the pure quota request must fence.
	canonical, err = canonicalAccountForStateRefresh(&canonical)
	if err != nil {
		return nil, err
	}
	*account = canonical.Clone()
	observation := canonical.Clone()

	info, fetchErr := h.refreshAccountInfoOnly(ctx, &observation)
	if fetchErr != nil {
		failure := publishReconciledAccountInfoFailure(&observation, fetchErr)
		if failure.HealthChanged && h.pool != nil {
			h.pool.Reload()
		}
		if failure.Superseded {
			return nil, errStaleAccountSelection
		}
		if failure.publishedSuspensionCleanly() {
			// Preserve the historical manual-refresh suspension response, but base
			// that control-flow decision on typed publication state rather than the
			// wording of the wrapped error.
			return &accountRefreshOutcome{Message: "Account status updated"}, nil
		}
		return nil, failure.Err()
	}

	var applied bool
	var persistErr error
	if observation.IsCustomApi() {
		// Custom relays historically publish quota fail-open without Kiro ban
		// recovery semantics. Preserve that policy while still using observed CAS.
		applied, persistErr = config.UpdateAccountInfoIfObserved(observation, *info)
	} else {
		applied, persistErr = config.UpdateAccountInfoAndRecoverIfObserved(observation, *info)
	}
	if !applied {
		if persistErr != nil {
			return nil, persistErr
		}
		return nil, errStaleAccountSelection
	}
	// Publication may be visible while a trailing durability barrier reports an
	// error. Align runtime first, then propagate that durability failure.
	if h.pool != nil {
		h.pool.Reload()
	}
	if persistErr != nil {
		return nil, persistErr
	}
	return &accountRefreshOutcome{Info: info}, nil
}
