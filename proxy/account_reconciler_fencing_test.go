package proxy

import (
	"context"
	"errors"
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func withAccountInfoFetchForTest(t *testing.T, fn func(context.Context, *config.Account) (*config.AccountInfo, error)) {
	t.Helper()
	previous := fetchAccountInfoForReconcile
	fetchAccountInfoForReconcile = fn
	t.Cleanup(func() { fetchAccountInfoForReconcile = previous })
}

func addReconcileOAuthAccount(t *testing.T, lastRefresh int64) config.Account {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "acct",
		Email:        "a@example.invalid",
		AuthMethod:   "social",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		LastRefresh:  lastRefresh,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}
	account, ok := config.GetAccountByID("acct")
	if !ok || account.DispatchGeneration == 0 {
		t.Fatalf("canonical account missing lifecycle: %+v ok=%t", account, ok)
	}
	return account
}

func TestAccountReconcilerDropsOldToken401AfterCredentialRefresh(t *testing.T) {
	account := addReconcileOAuthAccount(t, time.Now().Add(-time.Hour).Unix())
	withAccountInfoFetchForTest(t, func(context.Context, *config.Account) (*config.AccountInfo, error) {
		applied, err := config.UpdateAccountCredentialStateIfLifecycle(
			account.ID,
			account.DispatchGeneration,
			"access-2",
			"refresh-2",
			time.Now().Add(2*time.Hour).Unix(),
			"",
		)
		if err != nil || !applied {
			t.Fatalf("publish concurrent credential refresh: applied=%t err=%v", applied, err)
		}
		return nil, fmt.Errorf("HTTP 401: token invalid or expired")
	})

	h := &Handler{}
	result := h.runAccountReconcileJob(context.Background(), accountReconcileJob{
		key:     accountReconcileKey{id: account.ID, generation: account.DispatchGeneration},
		account: account,
		infoDue: true,
	})
	if !result.infoAttempted || !result.infoSuperseded || result.infoErr != nil || result.healthChanged {
		t.Fatalf("stale 401 result = %#v", result)
	}
	current, _ := config.GetAccountByID(account.ID)
	if !current.Enabled || current.AccessToken != "access-2" || current.BanStatus == "BANNED" {
		t.Fatalf("old-token 401 contaminated refreshed account: %+v", current)
	}
}

func TestAccountReconcilerDropsOlderInfoAfterNewerPublication(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	withAccountInfoFetchForTest(t, func(context.Context, *config.Account) (*config.AccountInfo, error) {
		applied, err := config.UpdateAccountInfoIfLifecycle(account.ID, account.DispatchGeneration, config.AccountInfo{
			UsageCurrent: 80,
			UsageLimit:   100,
			UsagePercent: 0.8,
			LastRefresh:  20,
		})
		if err != nil || !applied {
			t.Fatalf("publish newer info: applied=%t err=%v", applied, err)
		}
		return &config.AccountInfo{UsageCurrent: 20, UsageLimit: 100, UsagePercent: 0.2, LastRefresh: 30}, nil
	})

	result := (&Handler{}).runAccountReconcileJob(context.Background(), accountReconcileJob{
		key:     accountReconcileKey{id: account.ID, generation: account.DispatchGeneration},
		account: account,
		infoDue: true,
	})
	if !result.infoSuperseded || result.infoChanged || result.infoErr != nil {
		t.Fatalf("older info result = %#v", result)
	}
	current, _ := config.GetAccountByID(account.ID)
	if current.UsageCurrent != 80 || current.LastRefresh != 20 {
		t.Fatalf("newer account-info was overwritten: %+v", current)
	}
}

func TestAccountReconcilerCurrentAuthFailureChangesHealth(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	withAccountInfoFetchForTest(t, func(context.Context, *config.Account) (*config.AccountInfo, error) {
		return nil, fmt.Errorf("HTTP 401: unauthorized")
	})

	result := (&Handler{}).runAccountReconcileJob(context.Background(), accountReconcileJob{
		key:     accountReconcileKey{id: account.ID, generation: account.DispatchGeneration},
		account: account,
		infoDue: true,
	})
	if result.infoSuperseded || !result.healthChanged || result.infoErr == nil {
		t.Fatalf("current auth failure result = %#v", result)
	}
	current, _ := config.GetAccountByID(account.ID)
	if current.Enabled || current.BanStatus != "BANNED" {
		t.Fatalf("current auth failure did not disable account: %+v", current)
	}
}

func TestAccountReconcilerBackoffStartsAtCompletion(t *testing.T) {
	started := time.Unix(2_000_000_000, 0)
	completed := started.Add(90 * time.Second)
	key := accountReconcileKey{id: "acct", generation: 1}
	state := &accountReconcileState{firstSeen: started}
	result := accountReconcileResult{
		job:           accountReconcileJob{key: key, infoDue: true},
		infoAttempted: true,
		infoErr:       context.DeadlineExceeded,
	}
	updateAccountReconcileState(completed, state, result)
	if !state.infoRetryAt.After(completed) {
		t.Fatalf("retry deadline %v was not based on completion %v", state.infoRetryAt, completed)
	}
}

func TestAccountReconcilerDoesNotScheduleInfoWhileExpiredCredentialBacksOff(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	account := config.Account{
		ID:                 "acct",
		AuthMethod:         "social",
		AccessToken:        "expired",
		RefreshToken:       "refresh",
		ExpiresAt:          now.Add(-time.Minute).Unix(),
		LastRefresh:        now.Add(-time.Hour).Unix(),
		Enabled:            true,
		DispatchGeneration: 2,
	}
	state := &accountReconcileState{
		firstSeen:          now.Add(-time.Hour),
		credentialRetryAt:  now.Add(time.Minute),
		credentialFailures: 1,
	}
	job, due := planAccountReconcile(now, account, state)
	if due || job.infoDue || job.credentialDue {
		t.Fatalf("expired credential in backoff scheduled churn: due=%t job=%#v", due, job)
	}
}

func TestAccountReconcilerSupersededInfoDoesNotChangeBackoff(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	state := &accountReconcileState{
		firstSeen:    now.Add(-time.Hour),
		infoFailures: 2,
		infoRetryAt:  now.Add(time.Minute),
	}
	beforeRetry := state.infoRetryAt
	updateAccountReconcileState(now, state, accountReconcileResult{
		job:            accountReconcileJob{key: accountReconcileKey{id: "acct", generation: 1}, infoDue: true},
		infoAttempted:  true,
		infoSuperseded: true,
		infoErr:        errors.New("stale response"),
	})
	if state.infoFailures != 2 || !state.infoRetryAt.Equal(beforeRetry) {
		t.Fatalf("superseded result changed backoff: %#v", state)
	}
}

func TestAccountReconcilerEnqueueDoesNotBlockWhenQueueIsFull(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "queued",
		AuthMethod:   "social",
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		LastRefresh:  time.Now().Add(-time.Hour).Unix(),
		Enabled:      true,
	}); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}

	states := make(map[accountReconcileKey]*accountReconcileState)
	inFlight := make(map[accountReconcileKey]struct{})
	jobCh := make(chan accountReconcileJob, 1)
	jobCh <- accountReconcileJob{} // deliberately saturate the bounded queue

	done := make(chan struct{})
	go func() {
		enqueueDueAccountReconcileJobs(time.Now(), states, inFlight, jobCh)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler blocked behind a saturated worker queue")
	}
	if len(inFlight) != 0 {
		t.Fatalf("unsent job was incorrectly marked in-flight: %d", len(inFlight))
	}

	<-jobCh
	enqueueDueAccountReconcileJobs(time.Now(), states, inFlight, jobCh)
	if len(inFlight) != 1 || len(jobCh) != 1 {
		t.Fatalf("due account was not queued once capacity returned: inFlight=%d queued=%d", len(inFlight), len(jobCh))
	}
	// A second scan must not duplicate a lifecycle already in-flight.
	enqueueDueAccountReconcileJobs(time.Now(), states, inFlight, jobCh)
	if len(jobCh) != 1 {
		t.Fatalf("in-flight lifecycle was enqueued twice: queued=%d", len(jobCh))
	}
}

func TestAccountReconcilerDropsQueuedInfoSupersededBeforeNetwork(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	withAccountInfoFetchForTest(t, func(context.Context, *config.Account) (*config.AccountInfo, error) {
		t.Fatal("superseded queued info job must not contact upstream")
		return nil, nil
	})
	if applied, err := config.UpdateAccountInfoIfLifecycle(account.ID, account.DispatchGeneration, config.AccountInfo{
		UsageCurrent: 50,
		UsageLimit:   100,
		UsagePercent: 0.5,
		LastRefresh:  20,
	}); err != nil || !applied {
		t.Fatalf("publish newer info before worker start: applied=%t err=%v", applied, err)
	}

	result := (&Handler{}).runAccountReconcileJob(context.Background(), accountReconcileJob{
		key:     accountReconcileKey{id: account.ID, generation: account.DispatchGeneration},
		account: account,
		infoDue: true,
	})
	if !result.infoSuperseded || result.infoAttempted {
		t.Fatalf("queued stale job result = %#v", result)
	}
}

func TestAccountReconcilerCredentialReplacementClearsBackoffEvenWhenExpiryUnchanged(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	state := &accountReconcileState{
		credentialFailures:  2,
		credentialRetryAt:   now.Add(10 * time.Minute),
		observedExpiresAt:   now.Add(time.Hour).Unix(),
		observedHasAccess:   true,
		observedAccessToken: "access-1",
	}
	account := config.Account{
		AccessToken: "access-2",
		ExpiresAt:   state.observedExpiresAt,
	}
	observeCanonicalAccountState(state, &account)
	if state.credentialFailures != 0 || !state.credentialRetryAt.IsZero() {
		t.Fatalf("credential replacement did not clear stale backoff: %#v", state)
	}
}

func TestAccountReconcilerDropsCredentialFailureAfterNewerCredential(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	applied, err := config.UpdateAccountCredentialStateIfLifecycle(
		account.ID,
		account.DispatchGeneration,
		"access-2",
		"refresh-2",
		account.ExpiresAt+3600,
		"",
	)
	if err != nil || !applied {
		t.Fatalf("publish newer credential: applied=%t err=%v", applied, err)
	}

	healthChanged, superseded, publishErr := publishReconciledCredentialFailure(
		&account,
		fmt.Errorf("token refresh failed: HTTP 401 invalid_grant"),
	)
	if publishErr != nil || healthChanged || !superseded {
		t.Fatalf("stale credential failure result: healthChanged=%t superseded=%t err=%v", healthChanged, superseded, publishErr)
	}
	current, _ := config.GetAccountByID(account.ID)
	if !current.Enabled || current.AccessToken != "access-2" || current.BanStatus == "BANNED" {
		t.Fatalf("stale credential failure contaminated refreshed account: %+v", current)
	}
}

func TestAccountReconcilerCurrentCredentialAuthFailureChangesHealth(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	healthChanged, superseded, publishErr := publishReconciledCredentialFailure(
		&account,
		fmt.Errorf("token refresh failed: HTTP 401 invalid_grant"),
	)
	if publishErr != nil || !healthChanged || superseded {
		t.Fatalf("current credential failure result: healthChanged=%t superseded=%t err=%v", healthChanged, superseded, publishErr)
	}
	current, _ := config.GetAccountByID(account.ID)
	if current.Enabled || current.BanStatus != "BANNED" {
		t.Fatalf("current credential auth failure did not disable account: %+v", current)
	}
}

func TestAccountReconcilerSupersededCredentialFailureDoesNotAdvanceBackoff(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	state := &accountReconcileState{
		firstSeen:          now.Add(-time.Hour),
		credentialFailures: 2,
		credentialRetryAt:  now.Add(time.Minute),
	}
	beforeRetry := state.credentialRetryAt
	updateAccountReconcileState(now, state, accountReconcileResult{
		job:                  accountReconcileJob{key: accountReconcileKey{id: "acct", generation: 1}, credentialDue: true},
		credentialSuperseded: true,
		credentialErr:        errors.New("stale refresh failure"),
	})
	if state.credentialFailures != 2 || !state.credentialRetryAt.Equal(beforeRetry) {
		t.Fatalf("superseded credential failure changed backoff: %#v", state)
	}
}

func TestAccountReconcilerDropsQueuedInfoAfterIdenticalNewerPublication(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	withAccountInfoFetchForTest(t, func(context.Context, *config.Account) (*config.AccountInfo, error) {
		t.Fatal("identical-but-newer queued info publication must fence upstream work")
		return nil, nil
	})

	// The account was added directly with LastRefresh=10 and otherwise-zero quota
	// fields. Publishing the exact same projection must still advance the runtime
	// revision and make the queued observation stale.
	if applied, err := config.UpdateAccountInfoIfLifecycle(account.ID, account.DispatchGeneration, config.AccountInfo{
		Email:       account.Email,
		LastRefresh: account.LastRefresh,
	}); err != nil || !applied {
		t.Fatalf("publish identical info: applied=%t err=%v", applied, err)
	}

	result := (&Handler{}).runAccountReconcileJob(context.Background(), accountReconcileJob{
		key:     accountReconcileKey{id: account.ID, generation: account.DispatchGeneration},
		account: account,
		infoDue: true,
	})
	if !result.infoSuperseded || result.infoAttempted {
		t.Fatalf("identical newer publication did not fence queued job: %#v", result)
	}
}

func TestAccountReconcilerInfoRevisionClearsBackoffWithoutTimestampAdvance(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	state := &accountReconcileState{
		infoFailures:         2,
		infoRetryAt:          now.Add(10 * time.Minute),
		observedLastRefresh:  20,
		observedInfoRevision: 7,
	}
	account := config.Account{
		LastRefresh:         20,
		AccountInfoRevision: 8,
	}
	observeCanonicalAccountState(state, &account)
	if state.infoFailures != 0 || !state.infoRetryAt.IsZero() {
		t.Fatalf("new account-info revision did not clear stale backoff: %#v", state)
	}
	if state.observedInfoRevision != 8 {
		t.Fatalf("observed revision=%d, want 8", state.observedInfoRevision)
	}
}

func TestAccountReconcilerCanonicalProgressClearsRuntimeError(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	state := &accountReconcileState{
		credentialFailures:  2,
		credentialRetryAt:   now.Add(time.Minute),
		credentialLastError: "credential refresh failed",
		observedExpiresAt:   now.Add(time.Hour).Unix(),
		observedHasAccess:   true,
		observedAccessToken: "access-1",
		infoFailures:        2,
		infoRetryAt:         now.Add(time.Minute),
		infoLastError:       "account-info refresh failed",
		observedLastRefresh: 10,
	}
	account := config.Account{
		AccessToken: "access-2",
		ExpiresAt:   state.observedExpiresAt,
		LastRefresh: 20,
	}
	observeCanonicalAccountState(state, &account)
	if state.credentialFailures != 0 || !state.credentialRetryAt.IsZero() || state.credentialLastError != "" {
		t.Fatalf("canonical credential progress left stale runtime error: %#v", state)
	}
	if state.infoFailures != 0 || !state.infoRetryAt.IsZero() || state.infoLastError != "" {
		t.Fatalf("canonical account-info progress left stale runtime error: %#v", state)
	}
}

func TestAccountReconcilerManualDisableKeepsCredentialLaneButDropsQueuedInfo(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	withAccountInfoFetchForTest(t, func(context.Context, *config.Account) (*config.AccountInfo, error) {
		t.Fatal("manual-disabled account must not run queued account-info work")
		return nil, nil
	})
	if err := config.SetAccountEnabled(account.ID, false); err != nil {
		t.Fatalf("manual disable: %v", err)
	}

	result := (&Handler{}).runAccountReconcileJob(context.Background(), accountReconcileJob{
		key:                   accountReconcileKey{id: account.ID, generation: account.DispatchGeneration},
		account:               account,
		credentialDue:         true,
		credentialMinValidity: 0, // fresh token: exercises lane admission without an IdP call
		infoDue:               true,
	})
	if result.credentialSuperseded || !result.credentialAttempted || result.credentialErr != nil {
		t.Fatalf("manual disable incorrectly stopped credential maintenance: %#v", result)
	}
	if !result.infoSuperseded || result.infoAttempted {
		t.Fatalf("manual disable did not drop queued account-info lane: %#v", result)
	}
	current, _ := config.GetAccountByID(account.ID)
	if current.Enabled || current.BanStatus != "DISABLED" {
		t.Fatalf("credential maintenance re-enabled manual-disabled account: %+v", current)
	}
}

func TestAccountReconcilerTerminalHealthSupersedesQueuedCredentialBeforeNetwork(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	if err := config.SetAccountBanStatus(account.ID, "BANNED", "revoked credential"); err != nil {
		t.Fatalf("ban account: %v", err)
	}

	result := (&Handler{}).runAccountReconcileJob(context.Background(), accountReconcileJob{
		key:           accountReconcileKey{id: account.ID, generation: account.DispatchGeneration},
		account:       account,
		credentialDue: true,
	})
	if !result.credentialSuperseded || result.credentialAttempted || result.credentialErr != nil {
		t.Fatalf("terminal health did not supersede queued credential work: %#v", result)
	}
}

func addReconcileExternalIDPAccount(t *testing.T, tokenEndpoint string) config.Account {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:            "external-acct",
		Email:         "external@example.invalid",
		AuthMethod:    "external_idp",
		Provider:      "generic-oidc",
		AccessToken:   "access-1",
		RefreshToken:  "refresh-1",
		ExpiresAt:     time.Now().Add(time.Minute).Unix(),
		ClientID:      "client-id",
		TokenEndpoint: tokenEndpoint,
		Scopes:        "openid offline_access",
		LastRefresh:   time.Now().Add(-time.Hour).Unix(),
		Enabled:       true,
	}); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}
	account, ok := config.GetAccountByID("external-acct")
	if !ok || account.DispatchGeneration == 0 {
		t.Fatalf("canonical external IdP account missing lifecycle: %+v ok=%t", account, ok)
	}
	return account
}

func withExternalIDPRefreshTransportForTest(t *testing.T) {
	t.Helper()
	previousClient := auth.SetGlobalAuthClientForTest(&http.Client{
		Transport: &http.Transport{Proxy: nil},
	})
	previousValidator := auth.SetExternalIdpValidatorForTest(func(string) error { return nil })
	t.Cleanup(func() {
		auth.SetGlobalAuthClientForTest(previousClient)
		auth.SetExternalIdpValidatorForTest(previousValidator)
	})
}

func TestAccountReconcilerSecondInfoPreflightDropsDisableDuringCredentialIO(t *testing.T) {
	withExternalIDPRefreshTransportForTest(t)
	var posts atomic.Int32
	var account config.Account
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		if err := config.SetAccountEnabled(account.ID, false); err != nil {
			t.Errorf("disable during credential refresh: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`)
	}))
	defer server.Close()
	account = addReconcileExternalIDPAccount(t, server.URL)

	withAccountInfoFetchForTest(t, func(context.Context, *config.Account) (*config.AccountInfo, error) {
		t.Fatal("disable during credential I/O must suppress the subsequent account-info request")
		return nil, nil
	})

	result := (&Handler{}).runAccountReconcileJob(context.Background(), accountReconcileJob{
		key:                   accountReconcileKey{id: account.ID, generation: account.DispatchGeneration},
		account:               account,
		credentialDue:         true,
		credentialMinValidity: 5 * time.Minute,
		infoDue:               true,
	})
	if got := posts.Load(); got != 1 {
		t.Fatalf("token endpoint POSTs=%d, want 1", got)
	}
	if !result.credentialAttempted || result.credentialErr != nil || !result.credentialChanged {
		t.Fatalf("credential result = %#v", result)
	}
	if !result.infoSuperseded || result.infoAttempted {
		t.Fatalf("second info preflight did not drop disabled account: %#v", result)
	}
	current, _ := config.GetAccountByID(account.ID)
	if current.Enabled || current.BanStatus != "DISABLED" || current.AccessToken != "access-2" {
		t.Fatalf("credential/disable publication state = %+v", current)
	}
}

func TestAccountReconcilerSecondInfoPreflightDropsBanDuringCredentialIO(t *testing.T) {
	withExternalIDPRefreshTransportForTest(t)
	var account config.Account
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := config.SetAccountBanStatus(account.ID, "BANNED", "revoked while refreshing"); err != nil {
			t.Errorf("ban during credential refresh: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`)
	}))
	defer server.Close()
	account = addReconcileExternalIDPAccount(t, server.URL)

	withAccountInfoFetchForTest(t, func(context.Context, *config.Account) (*config.AccountInfo, error) {
		t.Fatal("BANNED during credential I/O must suppress the subsequent account-info request")
		return nil, nil
	})

	result := (&Handler{}).runAccountReconcileJob(context.Background(), accountReconcileJob{
		key:                   accountReconcileKey{id: account.ID, generation: account.DispatchGeneration},
		account:               account,
		credentialDue:         true,
		credentialMinValidity: 5 * time.Minute,
		infoDue:               true,
	})
	if !result.infoSuperseded || result.infoAttempted {
		t.Fatalf("second info preflight did not drop terminal health: %#v", result)
	}
	current, _ := config.GetAccountByID(account.ID)
	if current.Enabled || current.BanStatus != "BANNED" || current.AccessToken != "access-2" {
		t.Fatalf("credential/ban publication state = %+v", current)
	}
}

func TestAccountReconcilerSameJobCredentialAdvanceFeedsInfoLaneFreshCredential(t *testing.T) {
	withExternalIDPRefreshTransportForTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`)
	}))
	defer server.Close()
	account := addReconcileExternalIDPAccount(t, server.URL)

	var fetchedAccess string
	withAccountInfoFetchForTest(t, func(_ context.Context, current *config.Account) (*config.AccountInfo, error) {
		fetchedAccess = current.AccessToken
		return &config.AccountInfo{
			Email:        current.Email,
			UsageCurrent: 25,
			UsageLimit:   100,
			UsagePercent: 0.25,
			LastRefresh:  time.Now().Unix(),
		}, nil
	})

	result := (&Handler{}).runAccountReconcileJob(context.Background(), accountReconcileJob{
		key:                   accountReconcileKey{id: account.ID, generation: account.DispatchGeneration},
		account:               account,
		credentialDue:         true,
		credentialMinValidity: 5 * time.Minute,
		infoDue:               true,
	})
	if result.credentialErr != nil || !result.credentialChanged || !result.infoAttempted || result.infoSuperseded || result.infoErr != nil {
		t.Fatalf("same-job credential->info result = %#v", result)
	}
	if fetchedAccess != "access-2" {
		t.Fatalf("account-info used access token %q, want freshly published T2", fetchedAccess)
	}
	current, _ := config.GetAccountByID(account.ID)
	if current.AccessToken != "access-2" || current.UsageCurrent != 25 {
		t.Fatalf("same-job canonical state = %+v", current)
	}
}
