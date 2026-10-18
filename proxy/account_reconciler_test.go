package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

func TestAccountReconcilerSchedulesAccountInfoBetweenTwoAndFiveMinutes(t *testing.T) {
	key := accountReconcileKey{id: "acct", generation: 7}
	interval := accountInfoRefreshIntervalFor(key)
	if interval < 2*time.Minute || interval >= 5*time.Minute {
		t.Fatalf("account-info interval = %v, want [2m,5m)", interval)
	}
	if again := accountInfoRefreshIntervalFor(key); again != interval {
		t.Fatalf("deterministic jitter changed: first=%v second=%v", interval, again)
	}
}

func TestAccountReconcilerCredentialDeadlineIsThreeToFiveMinutesBeforeExpiry(t *testing.T) {
	key := accountReconcileKey{id: "acct", generation: 3}
	expiry := time.Unix(2_000_000_000, 0)
	account := config.Account{
		ID:                 key.id,
		AuthMethod:         "social",
		AccessToken:        "access",
		RefreshToken:       "refresh",
		ExpiresAt:          expiry.Unix(),
		Enabled:            true,
		DispatchGeneration: key.generation,
	}
	refreshAt := credentialRefreshAt(&account, key)
	lead := expiry.Sub(refreshAt)
	if lead < 3*time.Minute || lead >= 5*time.Minute {
		t.Fatalf("credential lead = %v, want [3m,5m)", lead)
	}
	state := &accountReconcileState{firstSeen: refreshAt.Add(-time.Minute)}
	job, due := planAccountReconcile(refreshAt, account, state)
	if !due || !job.credentialDue {
		t.Fatalf("credential was not due at proactive deadline: due=%t job=%#v", due, job)
	}
	if job.credentialMinValidity != lead {
		t.Fatalf("job min validity = %v, want deadline lead %v", job.credentialMinValidity, lead)
	}
}

func TestAccountReconcilerBackoffLanesAreIndependent(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	key := accountReconcileKey{id: "acct", generation: 9}
	state := &accountReconcileState{firstSeen: now.Add(-time.Hour)}

	updateAccountReconcileState(now, state, accountReconcileResult{
		job:           accountReconcileJob{key: key, credentialDue: true, infoDue: true},
		credentialErr: context.DeadlineExceeded,
		infoErr:       nil,
	})
	if state.credentialFailures != 1 || state.credentialRetryAt.IsZero() {
		t.Fatalf("credential backoff not recorded: %#v", state)
	}
	if state.infoFailures != 0 || !state.infoRetryAt.IsZero() {
		t.Fatalf("credential failure contaminated info lane: %#v", state)
	}

	credentialRetryAt := state.credentialRetryAt
	updateAccountReconcileState(now.Add(time.Second), state, accountReconcileResult{
		job:           accountReconcileJob{key: key, infoDue: true},
		infoAttempted: true,
		infoErr:       context.DeadlineExceeded,
	})
	if state.infoFailures != 1 || state.infoRetryAt.IsZero() {
		t.Fatalf("info backoff not recorded: %#v", state)
	}
	if !state.credentialRetryAt.Equal(credentialRetryAt) || state.credentialFailures != 1 {
		t.Fatalf("info failure contaminated credential lane: %#v", state)
	}
}

func TestAccountReconcilerMissingAccessTokenIsImmediatelyCredentialDue(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	account := config.Account{
		ID:                 "acct",
		AuthMethod:         "social",
		RefreshToken:       "refresh",
		Enabled:            true,
		DispatchGeneration: 4,
	}
	state := &accountReconcileState{firstSeen: now}
	job, due := planAccountReconcile(now, account, state)
	if !due || !job.credentialDue {
		t.Fatalf("missing access token was not scheduled for refresh: due=%t job=%#v", due, job)
	}
}

func TestAccountReconcilerCanonicalProgressClearsStaleBackoff(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	state := &accountReconcileState{
		credentialFailures:  3,
		credentialRetryAt:   now.Add(10 * time.Minute),
		observedExpiresAt:   now.Add(time.Minute).Unix(),
		observedHasAccess:   true,
		infoFailures:        2,
		infoRetryAt:         now.Add(10 * time.Minute),
		observedLastRefresh: now.Add(-10 * time.Minute).Unix(),
	}
	account := config.Account{
		AccessToken: "request-refreshed-access",
		ExpiresAt:   now.Add(time.Hour).Unix(),
		LastRefresh: now.Unix(),
	}
	observeCanonicalAccountState(state, &account)
	if state.credentialFailures != 0 || !state.credentialRetryAt.IsZero() {
		t.Fatalf("canonical credential progress did not clear stale backoff: %#v", state)
	}
	if state.infoFailures != 0 || !state.infoRetryAt.IsZero() {
		t.Fatalf("canonical info progress did not clear stale backoff: %#v", state)
	}
}

func TestAccountReconcilerSkippedInfoProbeDoesNotAdvanceInfoBackoff(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	state := &accountReconcileState{firstSeen: now}
	result := accountReconcileResult{
		job: accountReconcileJob{
			key:     accountReconcileKey{id: "acct", generation: 1},
			infoDue: true,
		},
		infoAttempted: false,
	}
	updateAccountReconcileState(now, state, result)
	if state.infoFailures != 0 || !state.infoRetryAt.IsZero() {
		t.Fatalf("skipped info probe advanced backoff: %#v", state)
	}
}

func TestBackgroundRefreshStopsBeforeStartupWork(t *testing.T) {
	// backgroundRefresh starts the account reconciler immediately, and the
	// reconciler publishes a local-only runtime snapshot before its network
	// startup delay. Production always initializes config before NewHandler, so
	// this direct orchestrator test must establish the same prerequisite instead
	// of depending on another test having initialized the package-global config.
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	h := &Handler{stopRefresh: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		h.backgroundRefresh()
		close(done)
	}()
	close(h.stopRefresh)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background refresh orchestrator did not stop promptly")
	}
}

func TestAccountReconcilerCredentialSuccessSurvivesInfoFailure(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	key := accountReconcileKey{id: "acct", generation: 11}
	state := &accountReconcileState{
		firstSeen:          now.Add(-time.Hour),
		credentialFailures: 2,
		credentialRetryAt:  now.Add(-time.Second),
	}
	updateAccountReconcileState(now, state, accountReconcileResult{
		job:               accountReconcileJob{key: key, credentialDue: true, infoDue: true},
		credentialChanged: true,
		credentialErr:     nil,
		infoAttempted:     true,
		infoErr:           context.DeadlineExceeded,
	})
	if state.credentialFailures != 0 || !state.credentialRetryAt.IsZero() {
		t.Fatalf("successful credential lane retained failure state: %#v", state)
	}
	if state.infoFailures != 1 || state.infoRetryAt.IsZero() {
		t.Fatalf("info failure was not isolated to info lane: %#v", state)
	}
}

func TestAccountReconcilerRefreshTokenOnlyAdvanceClearsCredentialBackoff(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	state := &accountReconcileState{
		credentialFailures:              3,
		credentialRetryAt:               now.Add(10 * time.Minute),
		credentialLastError:             "old refresh failure",
		observedExpiresAt:               now.Add(time.Hour).Unix(),
		observedHasAccess:               true,
		observedAccessToken:             "same-access",
		observedHasRefresh:              true,
		observedRefreshTokenFingerprint: sha256.Sum256([]byte("refresh-1")),
	}
	account := &config.Account{
		AccessToken:  "same-access",
		RefreshToken: "refresh-2",
		ExpiresAt:    state.observedExpiresAt,
	}
	observeCanonicalAccountState(state, account)
	if state.credentialFailures != 0 || !state.credentialRetryAt.IsZero() || state.credentialLastError != "" {
		t.Fatalf("refresh-token-only progress retained stale backoff: %#v", state)
	}
}

func TestAccountInfoFailurePublicationKeepsSuspensionAndPersistenceSeparate(t *testing.T) {
	observed := &config.Account{
		ID:                 "acct",
		DispatchGeneration: 7,
		AuthMethod:         "social",
		AccessToken:        "access",
		RefreshToken:       "refresh",
	}
	upstreamErr := errors.New("TEMPORARILY_SUSPENDED")
	durabilityErr := errors.New("directory sync failed")

	result := publishReconciledAccountInfoFailureWithBanPublisher(
		observed,
		upstreamErr,
		func(got config.Account, status, reason string) (bool, error) {
			if got.ID != observed.ID || got.DispatchGeneration != observed.DispatchGeneration {
				t.Fatalf("publisher observation=%+v, want lifecycle %+v", got, observed)
			}
			if status != "BANNED" || !strings.Contains(reason, "suspended") {
				t.Fatalf("publisher health transition status=%q reason=%q", status, reason)
			}
			return true, durabilityErr
		},
	)
	if result.Kind != accountInfoFailureSuspension || !result.HealthChanged || result.Superseded {
		t.Fatalf("publication result=%+v, want visible suspension", result)
	}
	if !errors.Is(result.PersistenceErr, durabilityErr) || !errors.Is(result.Err(), durabilityErr) {
		t.Fatalf("durability error lost: result=%+v err=%v", result, result.Err())
	}
	if result.publishedSuspensionCleanly() {
		t.Fatal("durability failure was mistaken for clean suspension publication")
	}
}

func TestAccountInfoFailurePublicationDistinguishesPrePublicationAndSuperseded(t *testing.T) {
	observed := &config.Account{
		ID:                 "acct",
		DispatchGeneration: 7,
		AuthMethod:         "social",
		AccessToken:        "access",
		RefreshToken:       "refresh",
	}
	upstreamErr := errors.New("TEMPORARILY_SUSPENDED")
	persistErr := errors.New("replace failed")

	prePublish := publishReconciledAccountInfoFailureWithBanPublisher(
		observed,
		upstreamErr,
		func(config.Account, string, string) (bool, error) { return false, persistErr },
	)
	if prePublish.HealthChanged || prePublish.Superseded || prePublish.publishedSuspensionCleanly() {
		t.Fatalf("pre-publication failure misclassified: %+v", prePublish)
	}
	if !errors.Is(prePublish.Err(), persistErr) {
		t.Fatalf("pre-publication persistence error lost: %v", prePublish.Err())
	}

	superseded := publishReconciledAccountInfoFailureWithBanPublisher(
		observed,
		upstreamErr,
		func(config.Account, string, string) (bool, error) { return false, nil },
	)
	if !superseded.Superseded || superseded.HealthChanged || superseded.Err() != nil {
		t.Fatalf("superseded failure result=%+v err=%v", superseded, superseded.Err())
	}
}

func TestCleanSuspensionDecisionDoesNotDependOnErrorWording(t *testing.T) {
	result := accountInfoFailurePublication{
		Kind:          accountInfoFailureSuspension,
		HealthChanged: true,
		FailureErr:    errors.New("future wording that does not match the old wrapper"),
	}
	if !result.publishedSuspensionCleanly() {
		t.Fatalf("semantic suspension result depended on error wording: %+v", result)
	}
}

func TestAccountCredentialMaintenanceSeparatesOperatorDisableFromTerminalHealth(t *testing.T) {
	base := config.Account{
		ID:           "acct",
		AuthMethod:   "social",
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}

	tests := []struct {
		name   string
		mutate func(*config.Account)
		want   bool
	}{
		{name: "enabled healthy", mutate: func(a *config.Account) { a.Enabled = true }, want: true},
		{name: "manual disabled empty health", mutate: func(a *config.Account) { a.Enabled = false }, want: true},
		{name: "manual disabled active", mutate: func(a *config.Account) { a.Enabled = false; a.BanStatus = "ACTIVE" }, want: true},
		{name: "manual disabled legacy marker", mutate: func(a *config.Account) { a.Enabled = false; a.BanStatus = "DISABLED" }, want: true},
		{name: "enabled legacy manual marker", mutate: func(a *config.Account) { a.Enabled = true; a.BanStatus = "DISABLED" }, want: true},
		{name: "failure driven disabled", mutate: func(a *config.Account) {
			a.Enabled = false
			a.BanStatus = "DISABLED"
			a.BanReason = "automatic auth failure"
		}, want: false},
		{name: "banned", mutate: func(a *config.Account) { a.Enabled = false; a.BanStatus = "BANNED" }, want: false},
		{name: "billing blocked", mutate: func(a *config.Account) { a.Enabled = false; a.BanStatus = "BILLING_BLOCKED" }, want: false},
		{name: "suspended", mutate: func(a *config.Account) { a.Enabled = false; a.BanStatus = "SUSPENDED" }, want: false},
		{name: "missing refresh token", mutate: func(a *config.Account) { a.RefreshToken = "" }, want: false},
		{name: "static credential", mutate: func(a *config.Account) { a.AuthMethod = "anthropic_api" }, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := base.Clone()
			tt.mutate(&account)
			if got := accountSupportsCredentialMaintenance(&account); got != tt.want {
				t.Fatalf("accountSupportsCredentialMaintenance(%+v)=%t, want %t", account, got, tt.want)
			}
		})
	}
}

func TestAccountReconcilerManualDisabledSchedulesCredentialButNotInfo(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	account := config.Account{
		ID:                 "acct",
		AuthMethod:         "social",
		AccessToken:        "expired-access",
		RefreshToken:       "refresh",
		ExpiresAt:          now.Add(-time.Minute).Unix(),
		LastRefresh:        now.Add(-time.Hour).Unix(),
		Enabled:            false,
		BanStatus:          "DISABLED",
		DispatchGeneration: 2,
	}
	state := &accountReconcileState{firstSeen: now.Add(-time.Hour)}
	job, due := planAccountReconcile(now, account, state)
	if !due || !job.credentialDue {
		t.Fatalf("manual-disabled credential was not scheduled: due=%t job=%#v", due, job)
	}
	if job.infoDue {
		t.Fatalf("manual-disabled account scheduled quota/account-info work: %#v", job)
	}
}

func TestAccountReconcilerTerminalDisabledDoesNotScheduleCredentialMaintenance(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	for _, tc := range []struct {
		name   string
		status string
		reason string
	}{
		{name: "failure driven disabled", status: "DISABLED", reason: "automatic auth failure"},
		{name: "banned", status: "BANNED"},
		{name: "billing blocked", status: "BILLING_BLOCKED"},
		{name: "suspended", status: "SUSPENDED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := config.Account{
				ID:                 "acct",
				AuthMethod:         "social",
				AccessToken:        "expired-access",
				RefreshToken:       "refresh",
				ExpiresAt:          now.Add(-time.Minute).Unix(),
				LastRefresh:        now.Add(-time.Hour).Unix(),
				Enabled:            false,
				BanStatus:          tc.status,
				BanReason:          tc.reason,
				DispatchGeneration: 2,
			}
			job, due := planAccountReconcile(now, account, &accountReconcileState{firstSeen: now.Add(-time.Hour)})
			if due || job.credentialDue || job.infoDue {
				t.Fatalf("terminal account scheduled maintenance: due=%t job=%#v", due, job)
			}
		})
	}
}
