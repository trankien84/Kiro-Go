package proxy

import (
	"errors"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAccountRuntimeSnapshotInstancePreventsRestartETagCollision(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	first := (&Handler{}).accountRuntimeSnapshot(now)
	second := (&Handler{}).accountRuntimeSnapshot(now)
	if first.Instance == "" || second.Instance == "" {
		t.Fatalf("runtime instance missing: first=%q second=%q", first.Instance, second.Instance)
	}
	if first.Instance == second.Instance {
		t.Fatalf("independent handlers reused runtime instance %q", first.Instance)
	}
	if accountRuntimeETag(first.Instance, first.Revision) == accountRuntimeETag(second.Instance, second.Revision) {
		t.Fatal("different handler incarnations produced the same runtime ETag")
	}
}

func TestAccountRuntimeRevisionIgnoresServerClock(t *testing.T) {
	account := addReconcileOAuthAccount(t, time.Now().Add(-time.Minute).Unix())
	key := accountReconcileKey{id: account.ID, generation: account.DispatchGeneration}
	states := map[accountReconcileKey]*accountReconcileState{
		key: {firstSeen: time.Now().Add(-time.Minute)},
	}
	h := &Handler{}
	now := time.Now()
	h.syncAccountRuntimeFromReconciler(now, states)
	first := h.accountRuntimeSnapshot(now)
	if first.Revision == 0 || len(first.Accounts) != 1 {
		t.Fatalf("first snapshot = %#v", first)
	}

	h.syncAccountRuntimeFromReconciler(now.Add(30*time.Second), states)
	second := h.accountRuntimeSnapshot(now.Add(30 * time.Second))
	if second.Revision != first.Revision {
		t.Fatalf("server clock changed revision: first=%d second=%d", first.Revision, second.Revision)
	}
	if second.ServerTime == first.ServerTime {
		t.Fatalf("serverTime did not advance: first=%d second=%d", first.ServerTime, second.ServerTime)
	}
}

func TestAccountRuntimeRevisionChangesOnStableProjectionChange(t *testing.T) {
	account := addReconcileOAuthAccount(t, time.Now().Add(-time.Minute).Unix())
	key := accountReconcileKey{id: account.ID, generation: account.DispatchGeneration}
	state := &accountReconcileState{firstSeen: time.Now().Add(-time.Minute)}
	states := map[accountReconcileKey]*accountReconcileState{key: state}
	h := &Handler{}
	now := time.Now()
	h.syncAccountRuntimeFromReconciler(now, states)
	before := h.accountRuntimeSnapshot(now).Revision

	state.infoRefreshing = true
	h.syncAccountRuntimeFromReconciler(now.Add(time.Second), states)
	after := h.accountRuntimeSnapshot(now.Add(time.Second)).Revision
	if after != before+1 {
		t.Fatalf("revision after state change = %d, want %d", after, before+1)
	}
}

func TestAccountRuntimeEndpointUsesWeakETagAndPerformsNoRefreshWork(t *testing.T) {
	account := addReconcileOAuthAccount(t, time.Now().Add(-time.Minute).Unix())
	key := accountReconcileKey{id: account.ID, generation: account.DispatchGeneration}
	h := &Handler{}
	h.syncAccountRuntimeFromReconciler(time.Now(), map[accountReconcileKey]*accountReconcileState{
		key: {firstSeen: time.Now()},
	})

	firstReq := httptest.NewRequest(http.MethodGet, "/admin/api/accounts/runtime", nil)
	firstRec := httptest.NewRecorder()
	h.apiGetAccountsRuntime(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", firstRec.Code, firstRec.Body.String())
	}
	etag := firstRec.Header().Get("ETag")
	if !strings.HasPrefix(etag, `W/"accounts-runtime-`) {
		t.Fatalf("unexpected ETag %q", etag)
	}
	if got := firstRec.Header().Get("Cache-Control"); got != "private, no-cache" {
		t.Fatalf("Cache-Control=%q, want private, no-cache", got)
	}

	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodGet, "/admin/api/accounts/runtime", nil)
		req.Header.Set("If-None-Match", strings.TrimPrefix(etag, "W/")) // strong validator weak-matches
		rec := httptest.NewRecorder()
		h.apiGetAccountsRuntime(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Fatalf("poll %d status=%d body=%s", i, rec.Code, rec.Body.String())
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("poll %d returned a 304 body: %q", i, rec.Body.String())
		}
	}
}

func TestAccountRuntimeAdminRouteIsReadOnlyAndAuthenticated(t *testing.T) {
	account := addReconcileOAuthAccount(t, time.Now().Add(-time.Minute).Unix())
	key := accountReconcileKey{id: account.ID, generation: account.DispatchGeneration}
	h := &Handler{}
	h.syncAccountRuntimeFromReconciler(time.Now(), map[accountReconcileKey]*accountReconcileState{
		key: {firstSeen: time.Now()},
	})

	unauthorized := httptest.NewRequest(http.MethodGet, "/admin/api/accounts/runtime", nil)
	unauthorizedRec := httptest.NewRecorder()
	h.handleAdminAPI(unauthorizedRec, unauthorized)
	if unauthorizedRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorizedRec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/accounts/runtime", nil)
	req.Header.Set("X-Admin-Password", config.GetPassword())
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("runtime route status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("runtime route omitted ETag")
	}
}

func TestSafeAccountRuntimeErrorDoesNotExposeRawCredentialMaterial(t *testing.T) {
	secret := "refresh-token-secret-value"
	got := safeAccountRuntimeError("credential", errors.New("HTTP 500 upstream body token="+secret))
	if strings.Contains(got, secret) || strings.Contains(got, "HTTP 500") {
		t.Fatalf("sanitized runtime error leaked raw upstream detail: %q", got)
	}
	if got != "credential refresh failed" {
		t.Fatalf("sanitized runtime error=%q", got)
	}
}

func TestAccountRuntimeSyncDoesNotSeedReconcilerSchedulerState(t *testing.T) {
	addReconcileOAuthAccount(t, 0)
	states := make(map[accountReconcileKey]*accountReconcileState)
	h := &Handler{}
	h.syncAccountRuntimeFromReconciler(time.Now(), states)
	if len(states) != 0 {
		t.Fatalf("runtime observation mutated reconciler scheduler state: %#v", states)
	}
	snapshot := h.accountRuntimeSnapshot(time.Now())
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("runtime snapshot accounts=%d, want 1", len(snapshot.Accounts))
	}
	if snapshot.Accounts[0].AccountInfo.NextRefreshAt != 0 {
		t.Fatalf("observational startup snapshot invented info deadline: %#v", snapshot.Accounts[0].AccountInfo)
	}
}

func TestWeakETagMatchesRFCWeakComparisonShape(t *testing.T) {
	current := `W/"accounts-runtime-7"`
	for _, candidate := range []string{
		current,
		`"accounts-runtime-7"`,
		`"other", W/"accounts-runtime-7"`,
		`"opaque,with,commas", W/"accounts-runtime-7"`,
		"*",
	} {
		if !weakETagMatches(candidate, current) {
			t.Fatalf("candidate %q did not weak-match %q", candidate, current)
		}
	}
	if weakETagMatches(`W/"accounts-runtime-8"`, current) {
		t.Fatal("different revision weak-matched")
	}
}

func TestAccountRuntimeKeepsManualDisabledCredentialMaintenanceVisible(t *testing.T) {
	now := time.Now()
	account := addReconcileOAuthAccount(t, now.Add(-time.Minute).Unix())
	if err := config.SetAccountEnabled(account.ID, false); err != nil {
		t.Fatalf("manual disable: %v", err)
	}
	account, _ = config.GetAccountByID(account.ID)
	state := &accountReconcileState{firstSeen: now.Add(-time.Minute)}

	view := runtimeViewForAccount(now, account, state)
	if !view.Credential.Supported || view.Credential.NextRefreshAt == 0 {
		t.Fatalf("manual-disabled OAuth credential maintenance hidden from runtime: %#v", view.Credential)
	}
	if view.AccountInfo.Supported || view.AccountInfo.NextRefreshAt != 0 {
		t.Fatalf("manual-disabled account still exposed background account-info lane: %#v", view.AccountInfo)
	}
}

func TestAccountRuntimeHidesCredentialMaintenanceForTerminalHealth(t *testing.T) {
	now := time.Now()
	account := addReconcileOAuthAccount(t, now.Add(-time.Minute).Unix())
	if err := config.SetAccountBanStatus(account.ID, "BANNED", "revoked credential"); err != nil {
		t.Fatalf("ban account: %v", err)
	}
	account, _ = config.GetAccountByID(account.ID)
	state := &accountReconcileState{firstSeen: now.Add(-time.Minute)}

	view := runtimeViewForAccount(now, account, state)
	if view.Credential.Supported || view.Credential.NextRefreshAt != 0 {
		t.Fatalf("terminal credential remained scheduled in runtime: %#v", view.Credential)
	}
	if view.AccountInfo.Supported {
		t.Fatalf("terminal account exposed account-info maintenance: %#v", view.AccountInfo)
	}
}
