package pool

import (
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

func newRecoveryTestPool() *AccountPool {
	return &AccountPool{
		cooldowns:            make(map[string]time.Time),
		lastDispatchSeq:      make(map[string]uint64),
		errorCounts:          make(map[string]int),
		modelLists:           make(map[string]map[string]bool),
		modelListUnavailable: make(map[string]bool),
		reprobeBackoff:       make(map[accountRecoveryKey]time.Duration),
		reprobeNext:          make(map[accountRecoveryKey]time.Time),
		circuitState:         make(map[string]*circuitBreaker),
		healthStats:          make(map[string]*accountHealth),
		apiKeyAffinity:       make(map[string]apiKeyBinding),
	}
}

func withPoolExternalIDPRefreshTransportForTest(t *testing.T) {
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

func addFailureDrivenExternalIDPAccountForRecovery(t *testing.T, id, tokenEndpoint string) config.Account {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	return addFailureDrivenExternalIDPAccountToCurrentConfig(t, id, tokenEndpoint)
}

func addFailureDrivenExternalIDPAccountToCurrentConfig(t *testing.T, id, tokenEndpoint string) config.Account {
	t.Helper()
	if err := config.AddAccount(config.Account{
		ID:            id,
		Email:         id + "@example.invalid",
		AuthMethod:    "external_idp",
		Provider:      "generic-oidc",
		AccessToken:   "locally-fresh-but-revoked-" + id,
		RefreshToken:  "refresh-" + id,
		ExpiresAt:     time.Now().Add(time.Hour).Unix(),
		ClientID:      "client-id",
		TokenEndpoint: tokenEndpoint,
		Scopes:        "openid offline_access",
		Enabled:       false,
		BanStatus:     "DISABLED",
		BanReason:     "authentication failed",
		BanTime:       123,
	}); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}
	account, ok := config.GetAccountByID(id)
	if !ok || account.DispatchGeneration == 0 {
		t.Fatalf("canonical recovery account missing lifecycle: %+v ok=%t", account, ok)
	}
	return account
}

func TestLegacyAutoRecoveryForceValidatesFreshLocalTokenAndPublishesHealthyState(t *testing.T) {
	withPoolExternalIDPRefreshTransportForTest(t)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`)
	}))
	defer server.Close()

	account := addFailureDrivenExternalIDPAccountForRecovery(t, "force-recovery", server.URL)
	p := newRecoveryTestPool()
	p.reprobeDisabled()

	if got := posts.Load(); got != 1 {
		t.Fatalf("token endpoint POSTs=%d, want 1 despite locally fresh access token", got)
	}
	current, ok := config.GetAccountByID(account.ID)
	if !ok {
		t.Fatal("recovered account disappeared")
	}
	if !current.Enabled || current.BanStatus != "ACTIVE" || current.BanReason != "" || current.BanTime != 0 {
		t.Fatalf("recovery did not publish exact healthy state: %+v", current)
	}
	if current.AccessToken != "access-2" || current.RefreshToken != "refresh-2" {
		t.Fatalf("recovery did not adopt server-validated credential: %+v", current)
	}
	key, ok := accountRecoveryKeyFor(account)
	if !ok {
		t.Fatal("recovery account key missing")
	}
	if _, exists := p.reprobeBackoff[key]; exists {
		t.Fatalf("successful recovery retained backoff for %+v", key)
	}
	if _, exists := p.reprobeNext[key]; exists {
		t.Fatalf("successful recovery retained next-attempt for %+v", key)
	}
}

func TestLegacyAutoRecoveryDoesNotTouchManualDisabledAccount(t *testing.T) {
	withPoolExternalIDPRefreshTransportForTest(t)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"unexpected","refresh_token":"unexpected","expires_in":3600}`)
	}))
	defer server.Close()

	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:            "manual-disabled",
		AuthMethod:    "external_idp",
		Provider:      "generic-oidc",
		AccessToken:   "access-1",
		RefreshToken:  "refresh-1",
		ExpiresAt:     time.Now().Add(time.Hour).Unix(),
		ClientID:      "client-id",
		TokenEndpoint: server.URL,
		Enabled:       false,
		BanStatus:     "DISABLED",
		BanReason:     "",
	}); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}

	p := newRecoveryTestPool()
	p.reprobeDisabled()
	if got := posts.Load(); got != 0 {
		t.Fatalf("manual-disabled account triggered %d legacy token POSTs", got)
	}
	current, _ := config.GetAccountByID("manual-disabled")
	if current.Enabled {
		t.Fatalf("manual-disabled account was auto-enabled: %+v", current)
	}
}

func TestRecoveryBackoffIsPrunedAcrossDeleteRecreateLifecycle(t *testing.T) {
	p := newRecoveryTestPool()
	oldKey := accountRecoveryKey{id: "acct", generation: 7}
	newKey := accountRecoveryKey{id: "acct", generation: 8}
	p.reprobeBackoff[oldKey] = 2 * time.Hour
	p.reprobeNext[oldKey] = time.Now().Add(2 * time.Hour)

	p.pruneRecoveryState(map[accountRecoveryKey]struct{}{newKey: {}})

	if _, ok := p.reprobeBackoff[oldKey]; ok {
		t.Fatal("old lifecycle backoff survived pruning")
	}
	if _, ok := p.reprobeNext[oldKey]; ok {
		t.Fatal("old lifecycle next-attempt survived pruning")
	}
	if got := p.reprobeBackoff[newKey]; got != 0 {
		t.Fatalf("new lifecycle inherited backoff %v", got)
	}
	if _, ok := p.reprobeNext[newKey]; ok {
		t.Fatal("new lifecycle inherited old next-attempt")
	}
}

func TestRecoveryBackoffProgressionRemainsBounded(t *testing.T) {
	cases := []struct {
		previous time.Duration
		want     time.Duration
	}{
		{0, time.Minute},
		{time.Minute, 5 * time.Minute},
		{5 * time.Minute, 25 * time.Minute},
		{25 * time.Minute, 2 * time.Hour},
		{2 * time.Hour, 2 * time.Hour},
	}
	for _, tc := range cases {
		if got := nextRecoveryBackoff(tc.previous); got != tc.want {
			t.Fatalf("nextRecoveryBackoff(%v)=%v, want %v", tc.previous, got, tc.want)
		}
	}
}

func TestLegacyAutoRecoveryDropsOperatorDisableThatAdvancesHealthDuringValidation(t *testing.T) {
	withPoolExternalIDPRefreshTransportForTest(t)
	var account config.Account
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := config.SetAccountBanStatus(account.ID, "DISABLED", ""); err != nil {
			t.Errorf("operator disable during recovery validation: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`)
	}))
	defer server.Close()

	account = addFailureDrivenExternalIDPAccountForRecovery(t, "operator-disable-race", server.URL)
	originalBanTime := account.BanTime
	p := newRecoveryTestPool()
	p.reprobeDisabled()

	current, _ := config.GetAccountByID(account.ID)
	if current.Enabled {
		t.Fatalf("recovery overwrote operator disable: %+v", current)
	}
	if current.BanStatus != "DISABLED" || current.BanReason != "" {
		t.Fatalf("operator disable health state was corrupted: %+v", current)
	}
	if current.BanTime == originalBanTime {
		t.Fatalf("test did not advance the disable observation: before=%d after=%d", originalBanTime, current.BanTime)
	}
	if current.AccessToken != "access-2" || current.RefreshToken != "refresh-2" {
		t.Fatalf("validated C2 should remain canonical even though recovery was dropped: %+v", current)
	}
}

func TestClearRuntimeLifecyclePrunesAllRecoveryGenerationsForID(t *testing.T) {
	p := newRecoveryTestPool()
	oldKey := accountRecoveryKey{id: "acct", generation: 7}
	newKey := accountRecoveryKey{id: "acct", generation: 8}
	otherKey := accountRecoveryKey{id: "other", generation: 3}

	p.reprobeBackoff[oldKey] = time.Minute
	p.reprobeBackoff[newKey] = 5 * time.Minute
	p.reprobeBackoff[otherKey] = 25 * time.Minute
	p.reprobeNext[oldKey] = time.Now().Add(time.Minute)
	p.reprobeNext[newKey] = time.Now().Add(5 * time.Minute)
	p.reprobeNext[otherKey] = time.Now().Add(25 * time.Minute)

	p.mu.Lock()
	p.clearRuntimeLifecycleLocked("acct")
	p.mu.Unlock()

	for _, key := range []accountRecoveryKey{oldKey, newKey} {
		if _, ok := p.reprobeBackoff[key]; ok {
			t.Fatalf("recovery backoff survived runtime lifecycle clear for %+v", key)
		}
		if _, ok := p.reprobeNext[key]; ok {
			t.Fatalf("recovery schedule survived runtime lifecycle clear for %+v", key)
		}
	}
	if got := p.reprobeBackoff[otherKey]; got != 25*time.Minute {
		t.Fatalf("unrelated recovery backoff changed: got %v", got)
	}
	if _, ok := p.reprobeNext[otherKey]; !ok {
		t.Fatal("unrelated recovery schedule was removed")
	}
}

func TestScheduleRecoveryRetryUsesCompletionTimeAndStoredProgression(t *testing.T) {
	p := newRecoveryTestPool()
	key := accountRecoveryKey{id: "acct", generation: 7}
	firstCompleted := time.Now()
	p.scheduleRecoveryRetry(key, firstCompleted)

	p.mu.RLock()
	firstBackoff := p.reprobeBackoff[key]
	firstNext := p.reprobeNext[key]
	p.mu.RUnlock()
	if firstBackoff != time.Minute {
		t.Fatalf("first recovery backoff=%v, want 1m", firstBackoff)
	}
	if want := firstCompleted.Add(time.Minute); !firstNext.Equal(want) {
		t.Fatalf("first retry deadline=%v, want completion-based %v", firstNext, want)
	}

	secondCompleted := firstCompleted.Add(30 * time.Second)
	p.scheduleRecoveryRetry(key, secondCompleted)
	p.mu.RLock()
	secondBackoff := p.reprobeBackoff[key]
	secondNext := p.reprobeNext[key]
	p.mu.RUnlock()
	if secondBackoff != 5*time.Minute {
		t.Fatalf("second recovery backoff=%v, want 5m", secondBackoff)
	}
	if want := secondCompleted.Add(5 * time.Minute); !secondNext.Equal(want) {
		t.Fatalf("second retry deadline=%v, want completion-based %v", secondNext, want)
	}
}

func TestLegacyAutoRecoverySchedulesFailureBackoffFromAttemptCompletion(t *testing.T) {
	withPoolExternalIDPRefreshTransportForTest(t)
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	responseAt := make(chan time.Time, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-releaseResponse
		completedBoundary := time.Now()
		responseAt <- completedBoundary
		http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	account := addFailureDrivenExternalIDPAccountForRecovery(t, "completion-backoff", server.URL)
	p := newRecoveryTestPool()
	done := make(chan struct{})
	go func() {
		p.reprobeDisabled()
		close(done)
	}()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery request did not reach test IdP")
	}
	close(releaseResponse)
	var responseBoundary time.Time
	select {
	case responseBoundary = <-responseAt:
	case <-time.After(2 * time.Second):
		t.Fatal("test IdP did not produce failure response")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery scan did not finish after IdP failure")
	}

	key, ok := accountRecoveryKeyFor(account)
	if !ok {
		t.Fatal("recovery account key missing")
	}
	p.mu.RLock()
	backoff := p.reprobeBackoff[key]
	next := p.reprobeNext[key]
	p.mu.RUnlock()
	if backoff != time.Minute {
		t.Fatalf("failure backoff=%v, want 1m", backoff)
	}
	minimumDeadline := responseBoundary.Add(time.Minute)
	if next.Before(minimumDeadline) {
		t.Fatalf("retry deadline=%v starts before attempt completion boundary %v + 1m", next, responseBoundary)
	}
}

func TestLegacyAutoRecoveryReevaluatesDueTimePerCandidate(t *testing.T) {
	withPoolExternalIDPRefreshTransportForTest(t)
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	var p *AccountPool
	var secondKey accountRecoveryKey
	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.reprobeNext[secondKey] = time.Now()
		p.mu.Unlock()
		http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer firstServer.Close()
	var secondPosts atomic.Int32
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondPosts.Add(1)
		http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer secondServer.Close()

	addFailureDrivenExternalIDPAccountToCurrentConfig(t, "first", firstServer.URL)
	second := addFailureDrivenExternalIDPAccountToCurrentConfig(t, "second", secondServer.URL)
	var ok bool
	secondKey, ok = accountRecoveryKeyFor(second)
	if !ok {
		t.Fatal("second recovery account key missing")
	}
	p = newRecoveryTestPool()
	p.reprobeDisabled()

	if got := secondPosts.Load(); got != 1 {
		t.Fatalf("second account token POSTs=%d, want 1 after becoming due during the same scan", got)
	}
}

func TestLegacyAutoRecoveryRevalidatesCandidateBeforeNetwork(t *testing.T) {
	withPoolExternalIDPRefreshTransportForTest(t)
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	var secondPosts atomic.Int32
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondPosts.Add(1)
		http.Error(w, `{"error":"unexpected"}`, http.StatusServiceUnavailable)
	}))
	defer secondServer.Close()

	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := config.SetAccountBanStatus("second-preflight", "DISABLED", ""); err != nil {
			t.Errorf("manual-disable second recovery candidate: %v", err)
		}
		http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer firstServer.Close()

	addFailureDrivenExternalIDPAccountToCurrentConfig(t, "first-preflight", firstServer.URL)
	addFailureDrivenExternalIDPAccountToCurrentConfig(t, "second-preflight", secondServer.URL)

	p := newRecoveryTestPool()
	p.reprobeDisabled()
	if got := secondPosts.Load(); got != 0 {
		t.Fatalf("manual-disabled candidate triggered %d token POSTs after becoming ineligible before its turn", got)
	}
	current, _ := config.GetAccountByID("second-preflight")
	if current.Enabled || current.BanStatus != "DISABLED" || current.BanReason != "" {
		t.Fatalf("manual-disabled second candidate changed unexpectedly: %+v", current)
	}
}

func TestLegacyAutoRecoveryInvalidGrantBansAndStopsNetworkRetry(t *testing.T) {
	withPoolExternalIDPRefreshTransportForTest(t)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"refresh token revoked"}`)
	}))
	defer server.Close()

	account := addFailureDrivenExternalIDPAccountForRecovery(t, "terminal-invalid-grant", server.URL)
	p := newRecoveryTestPool()
	p.reprobeDisabled()

	if got := posts.Load(); got != 1 {
		t.Fatalf("token endpoint POSTs=%d, want 1", got)
	}
	current, ok := config.GetAccountByID(account.ID)
	if !ok {
		t.Fatal("terminal account disappeared")
	}
	if current.Enabled || current.BanStatus != "BANNED" {
		t.Fatalf("invalid_grant did not converge to BANNED: %+v", current)
	}

	p.reprobeDisabled()
	if got := posts.Load(); got != 1 {
		t.Fatalf("terminal credential was retried after BANNED: POSTs=%d", got)
	}
}

func TestLegacyAutoRecoverySlowDownRemainsRetryableEvenWithHTTP400(t *testing.T) {
	withPoolExternalIDPRefreshTransportForTest(t)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"slow_down","error_description":"too frequent"}`)
	}))
	defer server.Close()

	account := addFailureDrivenExternalIDPAccountForRecovery(t, "retry-slow-down", server.URL)
	p := newRecoveryTestPool()
	p.reprobeDisabled()

	current, _ := config.GetAccountByID(account.ID)
	if current.Enabled || current.BanStatus != "DISABLED" {
		t.Fatalf("slow_down was treated as terminal: %+v", current)
	}
	key, _ := accountRecoveryKeyFor(account)
	if p.reprobeBackoff[key] != time.Minute || p.reprobeNext[key].IsZero() {
		t.Fatalf("slow_down did not schedule retry: backoff=%v next=%v", p.reprobeBackoff[key], p.reprobeNext[key])
	}
	p.reprobeDisabled()
	if got := posts.Load(); got != 1 {
		t.Fatalf("backoff did not suppress immediate retry: POSTs=%d", got)
	}
}

func TestLegacyAutoRecoveryTerminalFailureCannotBanCredentialAdvancedDuringRequest(t *testing.T) {
	withPoolExternalIDPRefreshTransportForTest(t)
	var posts atomic.Int32
	var observed config.Account
	var advanceFailure atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		applied, err := config.UpdateAccountCredentialStateIfLifecycle(
			observed.ID,
			observed.DispatchGeneration,
			"access-new",
			"refresh-new",
			time.Now().Add(2*time.Hour).Unix(),
			"",
		)
		if err != nil || !applied {
			advanceFailure.Store(fmt.Sprintf("applied=%t err=%v", applied, err))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	}))
	defer server.Close()

	observed = addFailureDrivenExternalIDPAccountForRecovery(t, "terminal-stale", server.URL)
	p := newRecoveryTestPool()
	p.reprobeDisabled()

	if failure := advanceFailure.Load(); failure != nil {
		t.Fatalf("advance credential during request failed: %v", failure)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("token endpoint POSTs=%d, want 1", got)
	}
	current, _ := config.GetAccountByID(observed.ID)
	if current.AccessToken != "access-new" || current.RefreshToken != "refresh-new" || current.BanStatus == "BANNED" {
		t.Fatalf("stale terminal result contaminated newer credential: %+v", current)
	}
}

func TestLegacyAutoRecoveryPendingRecoveryPublicationRetriesLocallyWithoutIdP(t *testing.T) {
	withPoolExternalIDPRefreshTransportForTest(t)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	account := addFailureDrivenExternalIDPAccountForRecovery(t, "pending-local-recover", server.URL)
	key, _ := accountRecoveryKeyFor(account)
	p := newRecoveryTestPool()
	p.storePendingRecoveryPublication(key, pendingAccountRecoveryPublication{
		kind:     accountRecoveryPublicationRecover,
		observed: account.Clone(),
	})
	p.reprobeDisabled()

	if got := posts.Load(); got != 0 {
		t.Fatalf("pending local recovery replayed IdP: POSTs=%d", got)
	}
	current, _ := config.GetAccountByID(account.ID)
	if !current.Enabled || current.BanStatus != "ACTIVE" || current.BanReason != "" || current.BanTime != 0 {
		t.Fatalf("pending local recovery did not publish healthy state: %+v", current)
	}
}

func TestLegacyAutoRecoveryPendingTerminalBanRetriesLocallyWithoutIdP(t *testing.T) {
	withPoolExternalIDPRefreshTransportForTest(t)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	account := addFailureDrivenExternalIDPAccountForRecovery(t, "pending-local-ban", server.URL)
	key, _ := accountRecoveryKeyFor(account)
	p := newRecoveryTestPool()
	p.storePendingRecoveryPublication(key, pendingAccountRecoveryPublication{
		kind:     accountRecoveryPublicationBan,
		observed: account.Clone(),
		status:   "BANNED",
		reason:   "Authentication failed - token invalid, expired, or revoked",
	})
	p.reprobeDisabled()

	if got := posts.Load(); got != 0 {
		t.Fatalf("pending local terminal publication replayed IdP: POSTs=%d", got)
	}
	current, _ := config.GetAccountByID(account.ID)
	if current.Enabled || current.BanStatus != "BANNED" {
		t.Fatalf("pending terminal publication did not ban account: %+v", current)
	}
}
