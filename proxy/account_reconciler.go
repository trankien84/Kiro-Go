package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"strings"
	"sync"
	"time"
)

const (
	accountReconcileStartupDelay  = 10 * time.Second
	accountReconcileScanInterval  = 15 * time.Second
	accountReconcileMaxConcurrent = 4
	accountReconcileTimeout       = 2 * time.Minute

	accountInfoRefreshMinInterval = 2 * time.Minute
	accountInfoRefreshJitter      = 3 * time.Minute
	accountInfoInitialJitter      = 30 * time.Second

	credentialRefreshLeadMin = 3 * time.Minute
	credentialRefreshJitter  = 2 * time.Minute
	reconcileBackoffBase     = 30 * time.Second
	reconcileBackoffMax      = 15 * time.Minute

	modelRefreshStartupDelay = 10 * time.Second
	modelRefreshInterval     = 30 * time.Minute
)

type accountReconcileKey struct {
	id         string
	generation uint64
}

type accountReconcileState struct {
	firstSeen time.Time

	credentialFailures              int
	credentialRetryAt               time.Time
	credentialRefreshing            bool
	credentialLastAttempt           time.Time
	credentialLastSuccess           time.Time
	credentialLastError             string
	observedExpiresAt               int64
	observedHasAccess               bool
	observedAccessToken             string
	observedHasRefresh              bool
	observedRefreshTokenFingerprint [32]byte

	infoFailures         int
	infoRetryAt          time.Time
	infoRefreshing       bool
	infoLastAttempt      time.Time
	infoLastSuccess      time.Time
	infoLastError        string
	observedLastRefresh  int64
	observedInfoRevision uint64
}

type accountReconcileJob struct {
	key                   accountReconcileKey
	account               config.Account
	credentialDue         bool
	credentialMinValidity time.Duration
	infoDue               bool
}

type accountReconcileResult struct {
	job                   accountReconcileJob
	startedAt             time.Time
	credentialAttempted   bool
	credentialObservation config.Account
	credentialChanged     bool
	credentialErr         error
	infoAttempted         bool
	infoChanged           bool
	healthChanged         bool
	credentialSuperseded  bool
	infoSuperseded        bool
	infoErr               error
	completedAt           time.Time
}

func accountReconcileKeyFor(account *config.Account) (accountReconcileKey, bool) {
	if account == nil || strings.TrimSpace(account.ID) == "" || account.DispatchGeneration == 0 {
		return accountReconcileKey{}, false
	}
	return accountReconcileKey{id: account.ID, generation: account.DispatchGeneration}, true
}

func stableAccountJitter(key accountReconcileKey, lane string, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d:%s:%d:%s", len(key.id), key.id, key.generation, lane)
	return time.Duration(h.Sum64() % uint64(window))
}

func accountInfoRefreshIntervalFor(key accountReconcileKey) time.Duration {
	return accountInfoRefreshMinInterval + stableAccountJitter(key, "account-info", accountInfoRefreshJitter)
}

func credentialRefreshLeadFor(key accountReconcileKey) time.Duration {
	return credentialRefreshLeadMin + stableAccountJitter(key, "credential", credentialRefreshJitter)
}

func credentialRefreshAt(account *config.Account, key accountReconcileKey) time.Time {
	if account == nil || account.ExpiresAt <= 0 || !account.SupportsKiroTokenRefresh() || strings.TrimSpace(account.RefreshToken) == "" {
		return time.Time{}
	}
	return time.Unix(account.ExpiresAt, 0).Add(-credentialRefreshLeadFor(key))
}

// accountSupportsCredentialMaintenance separates routing participation from OAuth
// credential upkeep. An operator-disabled account is intentionally excluded from
// request selection, but keeping its refresh token active avoids an unnecessary
// cold-start (and possible refresh-token inactivity expiry) when it is enabled
// again. Terminal/provider-driven health states remain fail-closed.
//
// Historically SetAccountEnabled(false) persisted BanStatus=DISABLED without a
// reason. Failure-driven pool.DisableAccount also used DISABLED, but always with
// a reason and its own legacy auto-recovery loop. Preserve that distinction so
// the two background systems never race or silently re-enable an operator pause.
func accountSupportsCredentialMaintenance(account *config.Account) bool {
	if account == nil || !account.HasKnownKind() || !account.SupportsKiroTokenRefresh() ||
		strings.TrimSpace(account.RefreshToken) == "" {
		return false
	}

	switch strings.ToUpper(strings.TrimSpace(account.BanStatus)) {
	case "", "ACTIVE":
		return true
	case "DISABLED":
		return strings.TrimSpace(account.BanReason) == ""
	default:
		return false
	}
}

func reconcileBackoffFor(key accountReconcileKey, lane string, failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	shift := failures - 1
	if shift > 5 {
		shift = 5
	}
	base := reconcileBackoffBase * time.Duration(1<<shift)
	if base > reconcileBackoffMax {
		base = reconcileBackoffMax
	}
	jitterWindow := base / 5
	backoff := base + stableAccountJitter(key, lane+"-backoff", jitterWindow)
	if backoff > reconcileBackoffMax {
		return reconcileBackoffMax
	}
	return backoff
}

func accountSupportsBackgroundInfo(account *config.Account) bool {
	if account == nil || !account.Enabled || !account.HasKnownKind() {
		return false
	}
	switch account.Kind() {
	case config.AccountKindCustomRelay:
		return strings.TrimSpace(account.BaseURL) != "" && strings.TrimSpace(account.KiroApiKey) != ""
	case config.AccountKindKiroOAuth:
		return strings.TrimSpace(account.AccessToken) != "" || strings.TrimSpace(account.RefreshToken) != ""
	case config.AccountKindKiroAPIKey:
		return strings.TrimSpace(account.KiroApiKey) != "" || strings.TrimSpace(account.AccessToken) != ""
	default:
		return false
	}
}

func observeCanonicalAccountState(state *accountReconcileState, account *config.Account) {
	if state == nil || account == nil {
		return
	}
	hasAccess := strings.TrimSpace(account.AccessToken) != ""
	hasRefresh := strings.TrimSpace(account.RefreshToken) != ""
	refreshFingerprint := [32]byte{}
	if hasRefresh {
		refreshFingerprint = sha256.Sum256([]byte(account.RefreshToken))
	}
	accessChanged := state.observedAccessToken != "" && account.AccessToken != state.observedAccessToken
	refreshChanged := state.observedHasRefresh && hasRefresh &&
		state.observedRefreshTokenFingerprint != refreshFingerprint
	if (state.observedExpiresAt != 0 && account.ExpiresAt > state.observedExpiresAt) ||
		(!state.observedHasAccess && hasAccess) || accessChanged ||
		(!state.observedHasRefresh && hasRefresh) || refreshChanged {
		// A request/manual path refreshed this lifecycle while the background lane
		// was backing off. Canonical progress supersedes the old failure history.
		state.credentialFailures = 0
		state.credentialRetryAt = time.Time{}
		state.credentialLastError = ""
	}
	infoRevisionChanged := account.AccountInfoRevision != state.observedInfoRevision &&
		(account.AccountInfoRevision != 0 || state.observedInfoRevision != 0)
	if (account.LastRefresh > 0 && account.LastRefresh > state.observedLastRefresh) || infoRevisionChanged {
		// Likewise, any successful manual/info publication supersedes an old info
		// retry. AccountInfoRevision catches identical publications in the same Unix
		// second, where LastRefresh/projection comparison alone has no ordering signal.
		state.infoFailures = 0
		state.infoRetryAt = time.Time{}
		state.infoLastError = ""
	}
	state.observedExpiresAt = account.ExpiresAt
	state.observedHasAccess = hasAccess
	state.observedAccessToken = account.AccessToken
	state.observedHasRefresh = hasRefresh
	state.observedRefreshTokenFingerprint = refreshFingerprint
	state.observedLastRefresh = account.LastRefresh
	state.observedInfoRevision = account.AccountInfoRevision
}

func planAccountReconcile(
	now time.Time,
	account config.Account,
	state *accountReconcileState,
) (accountReconcileJob, bool) {
	key, ok := accountReconcileKeyFor(&account)
	if !ok || !account.HasKnownKind() {
		return accountReconcileJob{}, false
	}
	if state == nil {
		state = &accountReconcileState{firstSeen: now}
	}
	if state.firstSeen.IsZero() {
		state.firstSeen = now
	}

	job := accountReconcileJob{key: key, account: account.Clone()}
	credentialRetryReady := state.credentialRetryAt.IsZero() || !now.Before(state.credentialRetryAt)
	if accountSupportsCredentialMaintenance(&account) && credentialRetryReady {
		job.credentialMinValidity = credentialRefreshLeadFor(key)
		if strings.TrimSpace(account.AccessToken) == "" {
			job.credentialDue = true
		} else if refreshAt := credentialRefreshAt(&account, key); !refreshAt.IsZero() && !now.Before(refreshAt) {
			job.credentialDue = true
		}
	}

	// Do not repeatedly enqueue an account-info probe that cannot possibly
	// authenticate while the credential lane is still backing off. If credential
	// refresh is due in this same job, keep infoDue eligible: a successful refresh
	// will make the subsequent info fetch usable immediately.
	credentialUnusable := account.SupportsKiroTokenRefresh() &&
		(strings.TrimSpace(account.AccessToken) == "" ||
			(account.ExpiresAt > 0 && now.Unix() >= account.ExpiresAt))
	infoCredentialReady := !credentialUnusable || job.credentialDue
	if infoCredentialReady && accountSupportsBackgroundInfo(&account) &&
		(state.infoRetryAt.IsZero() || !now.Before(state.infoRetryAt)) {
		var infoDueAt time.Time
		if account.LastRefresh > 0 {
			infoDueAt = time.Unix(account.LastRefresh, 0).Add(accountInfoRefreshIntervalFor(key))
		} else {
			infoDueAt = state.firstSeen.Add(stableAccountJitter(key, "initial-account-info", accountInfoInitialJitter))
		}
		if !now.Before(infoDueAt) {
			job.infoDue = true
		}
	}

	return job, job.credentialDue || job.infoDue
}

func updateAccountReconcileState(completedAt time.Time, state *accountReconcileState, result accountReconcileResult) {
	if state == nil {
		return
	}
	if completedAt.IsZero() {
		completedAt = time.Now()
	}
	if result.job.credentialDue {
		state.credentialRefreshing = false
		if !result.startedAt.IsZero() {
			state.credentialLastAttempt = result.startedAt
		}
	}
	if result.job.credentialDue && !result.credentialSuperseded {
		if result.credentialErr == nil {
			state.credentialFailures = 0
			state.credentialRetryAt = time.Time{}
			state.credentialLastSuccess = completedAt
			state.credentialLastError = ""
		} else {
			state.credentialFailures++
			state.credentialRetryAt = completedAt.Add(reconcileBackoffFor(result.job.key, "credential", state.credentialFailures))
			state.credentialLastError = safeAccountRuntimeError("credential", result.credentialErr)
		}
	}
	if result.job.infoDue {
		state.infoRefreshing = false
		if result.infoAttempted && !result.startedAt.IsZero() {
			state.infoLastAttempt = result.startedAt
		}
	}
	if result.job.infoDue && result.infoAttempted && !result.infoSuperseded {
		if result.infoErr == nil {
			state.infoFailures = 0
			state.infoRetryAt = time.Time{}
			state.infoLastSuccess = completedAt
			state.infoLastError = ""
		} else {
			state.infoFailures++
			state.infoRetryAt = completedAt.Add(reconcileBackoffFor(result.job.key, "account-info", state.infoFailures))
			state.infoLastError = safeAccountRuntimeError("account-info", result.infoErr)
		}
	}
}

var fetchAccountInfoForReconcile = fetchAccountInfoForReconcileContext

// refreshAccountInfoOnly is deliberately a fetch-only operation. It neither
// rotates OAuth credentials nor publishes account health/quota itself. The job
// coordinator validates the credential/profile + account-info revision observation
// after the network response returns and only then publishes a still-current result. The
// config-side AccountInfoRevision is the final operation-order fence.
func (h *Handler) refreshAccountInfoOnly(ctx context.Context, account *config.Account) (*config.AccountInfo, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if account.IsCustomApi() {
		quota, err := probeCustomApiQuotaWithContext(ctx, account.BaseURL, account.KiroApiKey)
		if err != nil {
			return nil, err
		}
		info := quota.toAccountInfo(time.Now().Unix())
		return &info, nil
	}
	if !account.SupportsKiroAccountInfo() {
		return nil, fmt.Errorf("account kind %q does not support background account-info refresh", account.AuthMethod)
	}

	info, err := fetchAccountInfoForReconcile(ctx, account)
	if err != nil {
		return nil, err
	}
	return info, nil
}

func accountInfoResultStillCurrent(account *config.Account) bool {
	if account == nil || account.DispatchGeneration == 0 {
		return false
	}
	return config.AccountInfoObservationMatches(account.Clone())
}

func publishReconciledAccountInfo(account *config.Account, info config.AccountInfo) (bool, bool, error) {
	if account == nil || account.DispatchGeneration == 0 {
		return false, true, nil
	}
	applied, err := config.UpdateAccountInfoIfObserved(account.Clone(), info)
	if !applied {
		if err != nil {
			return false, false, err
		}
		return false, true, nil
	}
	// Config transactions can publish the new in-memory/disk-visible image and
	// still return a post-publication durability error. Preserve both facts:
	// callers must reload the runtime pool even while the lane backs off/reports
	// the durability failure.
	return true, false, err
}

type accountInfoFailureKind uint8

const (
	accountInfoFailureOther accountInfoFailureKind = iota
	accountInfoFailureCustomRelay
	accountInfoFailureProfileUnavailable
	accountInfoFailureAuth
	accountInfoFailureSuspension
)

// accountInfoFailurePublication keeps failure classification separate from the
// publication outcome. In particular, callers must not infer whether a health
// transition was published (or whether its trailing durability barrier failed)
// by comparing human-readable error strings.
type accountInfoFailurePublication struct {
	Kind           accountInfoFailureKind
	HealthChanged  bool
	Superseded     bool
	FailureErr     error
	PersistenceErr error
}

func (result accountInfoFailurePublication) Err() error {
	if result.FailureErr == nil {
		return result.PersistenceErr
	}
	if result.PersistenceErr == nil {
		return result.FailureErr
	}
	return errors.Join(result.FailureErr, result.PersistenceErr)
}

func (result accountInfoFailurePublication) publishedSuspensionCleanly() bool {
	return result.Kind == accountInfoFailureSuspension &&
		result.HealthChanged && !result.Superseded && result.PersistenceErr == nil
}

type observedAccountBanPublisher func(config.Account, string, string) (bool, error)

func publishReconciledAccountInfoFailure(account *config.Account, err error) accountInfoFailurePublication {
	return publishReconciledAccountInfoFailureWithBanPublisher(account, err, config.SetAccountBanStatusIfObserved)
}

func publishReconciledAccountInfoFailureWithBanPublisher(
	account *config.Account,
	err error,
	publishBan observedAccountBanPublisher,
) accountInfoFailurePublication {
	if err == nil {
		return accountInfoFailurePublication{}
	}
	if account == nil || account.DispatchGeneration == 0 {
		return accountInfoFailurePublication{Superseded: true}
	}
	// API-key/custom relay account-info errors were already fail-open before B.1;
	// they have no OAuth self-heal path and must never be disabled by a transient
	// quota endpoint failure.
	if account.IsCustomApi() {
		if !accountInfoResultStillCurrent(account) {
			return accountInfoFailurePublication{Kind: accountInfoFailureCustomRelay, Superseded: true}
		}
		return accountInfoFailurePublication{Kind: accountInfoFailureCustomRelay, FailureErr: err}
	}
	if account.IsApiKeyCredential() || isProfileUnavailableErrorMessage(err.Error()) {
		kind := accountInfoFailureOther
		if isProfileUnavailableErrorMessage(err.Error()) {
			kind = accountInfoFailureProfileUnavailable
		}
		if !accountInfoResultStillCurrent(account) {
			return accountInfoFailurePublication{Kind: kind, Superseded: true}
		}
		return accountInfoFailurePublication{Kind: kind, FailureErr: fmt.Errorf("GetUsageLimits: %w", err)}
	}

	kind := accountInfoFailureOther
	status := ""
	reason := ""
	wrapped := fmt.Errorf("GetUsageLimits: %w", err)
	switch {
	case pool.IsSuspensionError(err):
		kind = accountInfoFailureSuspension
		status = "BANNED"
		reason = "AWS temporarily suspended - unusual user activity detected"
		wrapped = fmt.Errorf("Account suspended: %w", err)
	case pool.IsAuthFailure(err):
		kind = accountInfoFailureAuth
		status = "BANNED"
		reason = "Authentication failed - token invalid or expired"
	}
	if status == "" {
		if !accountInfoResultStillCurrent(account) {
			return accountInfoFailurePublication{Kind: kind, Superseded: true}
		}
		return accountInfoFailurePublication{Kind: kind, FailureErr: wrapped}
	}

	applied, persistErr := publishBan(account.Clone(), status, reason)
	if !applied {
		if persistErr != nil {
			return accountInfoFailurePublication{Kind: kind, FailureErr: wrapped, PersistenceErr: persistErr}
		}
		// Most importantly, a 401 produced with token T1 is ignored after the
		// canonical lifecycle has already advanced to token T2, even though the
		// account's DispatchGeneration itself intentionally stays unchanged.
		return accountInfoFailurePublication{Kind: kind, Superseded: true}
	}
	return accountInfoFailurePublication{
		Kind:           kind,
		HealthChanged:  true,
		FailureErr:     wrapped,
		PersistenceErr: persistErr,
	}
}

func publishReconciledCredentialFailure(account *config.Account, err error) (bool, bool, error) {
	if err == nil {
		return false, false, nil
	}
	if account == nil || account.DispatchGeneration == 0 {
		return false, true, nil
	}

	disposition := auth.ClassifyRefreshFailure(err)
	status := ""
	reason := ""
	switch disposition {
	case auth.RefreshFailureCredentialTerminal:
		status = "BANNED"
		reason = "Authentication failed - token invalid, expired, or revoked"
	case auth.RefreshFailureConfigurationTerminal:
		status = "BANNED"
		reason = "OAuth refresh rejected - re-authentication or configuration update required"
	}

	if status == "" {
		// Proactive OAuth refresh failures already have their own bounded retry
		// lane. A transport/5xx/429 failure is not evidence that the account is
		// unhealthy, and feeding it into generic account failover would couple
		// credential refresh backoff to request routing again. Still distinguish
		// a result superseded by a newer credential so stale failure history does
		// not remain attached to T2.
		if !config.AccountCredentialObservationMatches(account.Clone()) {
			return false, true, nil
		}
		return false, false, nil
	}

	applied, persistErr := config.SetAccountBanStatusIfCredentialObserved(account.Clone(), status, reason)
	if !applied {
		if persistErr != nil {
			return false, false, persistErr
		}
		return false, true, nil
	}
	return true, false, persistErr
}

func (h *Handler) runAccountReconcileJob(ctx context.Context, job accountReconcileJob) accountReconcileResult {
	result := accountReconcileResult{job: job}

	// Jobs may spend time in the bounded queue. Revalidate lifecycle and each
	// maintenance lane independently immediately before network work. A manual
	// disable must cancel quota/account-info probing but is not a reason to let an
	// otherwise healthy OAuth credential expire. Terminal health, delete/recreate,
	// or an automatic failure-driven disable still supersede credential upkeep.
	latest, ok := config.GetAccountByID(job.key.id)
	if !ok || latest.DispatchGeneration != job.key.generation {
		result.credentialSuperseded = job.credentialDue
		result.infoSuperseded = job.infoDue
		result.completedAt = time.Now()
		return result
	}
	if !latest.HasKnownKind() {
		result.credentialSuperseded = job.credentialDue
		result.infoSuperseded = job.infoDue
		result.completedAt = time.Now()
		return result
	}

	if job.credentialDue && !accountSupportsCredentialMaintenance(&latest) {
		result.credentialSuperseded = true
	}
	if job.infoDue && !accountSupportsBackgroundInfo(&latest) {
		result.infoSuperseded = true
	}
	if (!job.credentialDue || result.credentialSuperseded) &&
		(!job.infoDue || result.infoSuperseded) {
		result.completedAt = time.Now()
		return result
	}

	// Fence queued info work against any publication that happened before this
	// worker started. Credential progress performed by THIS job is handled by a
	// second preflight below, so keep only the account-info revision as the
	// operation-order fence across the credential lane.
	var infoRevisionAtStart uint64
	if job.infoDue && !result.infoSuperseded {
		if !config.AccountInfoObservationMatches(job.account.Clone()) {
			result.infoSuperseded = true
		} else {
			infoRevisionAtStart = latest.AccountInfoRevision
		}
	}
	account := latest.Clone()
	result.startedAt = time.Now()

	if job.credentialDue && !result.credentialSuperseded {
		result.credentialAttempted = true
		changed, err := h.refreshAccountTokenIfExpiringWithinContext(ctx, &account, job.credentialMinValidity, false)
		// refreshAccountTokenWithPolicy copies back the canonical credential that
		// actually entered the lifecycle-serialized IdP attempt, including on a
		// failed exchange. Fence any terminal failure against that observation, not
		// against the potentially older snapshot captured when this job was queued.
		result.credentialObservation = account.Clone()
		result.credentialChanged = changed
		result.credentialErr = err
	}

	if job.infoDue && !result.infoSuperseded {
		// Credential work above can take a network round-trip. Re-read canonical
		// state at the account-info network boundary instead of carrying the first
		// preflight through that I/O. This drops a disable/BANNED/delete-recreate
		// that happened while refreshing credentials, while deliberately adopting
		// T2 when this same job validly advanced T1 -> T2. AccountInfoRevision is
		// the separate-operation fence: same-job credential progress does not bump
		// it, but another account-info publication does.
		infoAccount, ok := config.GetAccountByID(job.key.id)
		if !ok ||
			infoAccount.DispatchGeneration != job.key.generation ||
			!infoAccount.HasKnownKind() ||
			!accountSupportsBackgroundInfo(&infoAccount) ||
			infoAccount.AccountInfoRevision != infoRevisionAtStart {
			result.infoSuperseded = true
			result.completedAt = time.Now()
			return result
		}
		account = infoAccount.Clone()

		// A failed proactive refresh must not prevent quota/account-info from
		// updating while the old access token is still actually valid. Once an
		// OAuth access token is missing or actually expired, however, skip the
		// guaranteed-unauthorized probe. A skipped probe is not an info failure.
		if account.SupportsKiroTokenRefresh() &&
			(strings.TrimSpace(account.AccessToken) == "" ||
				(account.ExpiresAt > 0 && time.Now().Unix() >= account.ExpiresAt)) {
			result.completedAt = time.Now()
			return result
		}

		result.infoAttempted = true
		infoCtx, cancel := context.WithTimeout(ctx, accountReconcileTimeout)
		info, fetchErr := h.refreshAccountInfoOnly(infoCtx, &account)
		cancel()

		if fetchErr == nil && info != nil {
			changed, superseded, persistErr := publishReconciledAccountInfo(&account, *info)
			result.infoChanged = changed
			result.infoSuperseded = superseded
			result.infoErr = persistErr
		} else if fetchErr != nil {
			failure := publishReconciledAccountInfoFailure(&account, fetchErr)
			result.healthChanged = failure.HealthChanged
			result.infoSuperseded = failure.Superseded
			result.infoErr = failure.Err()
		}
	}
	result.completedAt = time.Now()
	return result
}

func enqueueDueAccountReconcileJobs(
	now time.Time,
	states map[accountReconcileKey]*accountReconcileState,
	inFlight map[accountReconcileKey]struct{},
	jobCh chan<- accountReconcileJob,
) {
	accounts := config.GetAccounts()
	seen := make(map[accountReconcileKey]struct{}, len(accounts))
	for i := range accounts {
		account := accounts[i]
		key, ok := accountReconcileKeyFor(&account)
		if !ok {
			continue
		}
		seen[key] = struct{}{}
		state := states[key]
		if state == nil {
			state = &accountReconcileState{firstSeen: now}
			states[key] = state
		}
		observeCanonicalAccountState(state, &account)
		if _, busy := inFlight[key]; busy {
			continue
		}
		job, due := planAccountReconcile(now, account, state)
		if !due {
			continue
		}
		select {
		case jobCh <- job:
			inFlight[key] = struct{}{}
			if job.credentialDue {
				state.credentialRefreshing = true
			}
			if job.infoDue {
				state.infoRefreshing = true
			}
		default:
			// The bounded queue is deliberately non-blocking. A saturated worker
			// pool must not stall the 15s scheduler; this account remains due and is
			// reconsidered on the next scan/result wake-up.
		}
	}
	for key := range states {
		if _, present := seen[key]; present {
			continue
		}
		if _, busy := inFlight[key]; !busy {
			delete(states, key)
		}
	}
}

func (h *Handler) handleAccountReconcileResult(
	states map[accountReconcileKey]*accountReconcileState,
	inFlight map[accountReconcileKey]struct{},
	result accountReconcileResult,
) bool {
	delete(inFlight, result.job.key)
	if result.credentialErr != nil {
		logger.Warnf("[AccountReconciler] credential refresh failed account=%s: %v", result.job.account.ID, result.credentialErr)
		var tokenErr *accountTokenRefreshError
		if result.credentialAttempted &&
			!errors.Is(result.credentialErr, errStaleAccountSelection) &&
			!errors.Is(result.credentialErr, context.Canceled) &&
			errors.As(result.credentialErr, &tokenErr) {
			healthChanged, superseded, publishErr := publishReconciledCredentialFailure(
				&result.credentialObservation,
				result.credentialErr,
			)
			result.healthChanged = result.healthChanged || healthChanged
			result.credentialSuperseded = superseded
			if publishErr != nil {
				logger.Warnf("[AccountReconciler] credential failure publication failed account=%s: %v", result.job.account.ID, publishErr)
			}
		}
	}
	state := states[result.job.key]
	updateAccountReconcileState(result.completedAt, state, result)
	if result.infoErr != nil {
		logger.Warnf("[AccountReconciler] account-info refresh failed account=%s: %v", result.job.account.ID, result.infoErr)
	}
	return result.credentialChanged || result.infoChanged || result.healthChanged
}

func (h *Handler) backgroundAccountReconciler(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	states := make(map[accountReconcileKey]*accountReconcileState)
	inFlight := make(map[accountReconcileKey]struct{})
	jobCh := make(chan accountReconcileJob, accountReconcileMaxConcurrent*8)
	resultCh := make(chan accountReconcileResult, accountReconcileMaxConcurrent*8)

	var workers sync.WaitGroup
	workers.Add(accountReconcileMaxConcurrent)
	for i := 0; i < accountReconcileMaxConcurrent; i++ {
		go func() {
			defer workers.Done()
			for job := range jobCh {
				if ctx.Err() != nil {
					return
				}
				result := h.runAccountReconcileJob(ctx, job)
				select {
				case resultCh <- result:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	defer func() {
		close(jobCh)
		workers.Wait()
	}()

	// Publish a zero-network runtime snapshot immediately. This gives the admin
	// UI stable account IDs/schedules during the historical 10-second startup
	// grace without pulling quota or rotating credentials early.
	h.syncAccountRuntimeFromReconciler(time.Now(), states)

	startup := time.NewTimer(accountReconcileStartupDelay)
	defer startup.Stop()
	select {
	case <-startup.C:
		now := time.Now()
		enqueueDueAccountReconcileJobs(now, states, inFlight, jobCh)
		h.syncAccountRuntimeFromReconciler(now, states)
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(accountReconcileScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			enqueueDueAccountReconcileJobs(now, states, inFlight, jobCh)
			h.syncAccountRuntimeFromReconciler(now, states)
		case result := <-resultCh:
			poolDirty := h.handleAccountReconcileResult(states, inFlight, result)
			// Coalesce a burst of completed workers into one pool reload.
		drain:
			for {
				select {
				case more := <-resultCh:
					poolDirty = h.handleAccountReconcileResult(states, inFlight, more) || poolDirty
				default:
					break drain
				}
			}
			if poolDirty && h.pool != nil {
				h.pool.Reload()
			}
			// A completion frees queue/worker capacity; reconsider due accounts now
			// rather than making them wait for the next 15-second tick.
			now := time.Now()
			enqueueDueAccountReconcileJobs(now, states, inFlight, jobCh)
			h.syncAccountRuntimeFromReconciler(now, states)
		case <-ctx.Done():
			return
		}
	}
}

func (h *Handler) backgroundModelRefresh(ctx context.Context) {
	refresh := func() {
		opCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		h.refreshModelsCacheContext(opCtx)
	}

	startup := time.NewTimer(modelRefreshStartupDelay)
	defer startup.Stop()
	select {
	case <-startup.C:
		refresh()
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(modelRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			refresh()
		case <-ctx.Done():
			return
		}
	}
}
