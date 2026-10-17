package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	"kiro-go/pool"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
)

type accountRuntimeLaneView struct {
	Supported     bool   `json:"supported"`
	Refreshing    bool   `json:"refreshing"`
	LastAttemptAt int64  `json:"lastAttemptAt,omitempty"`
	LastSuccessAt int64  `json:"lastSuccessAt,omitempty"`
	NextRefreshAt int64  `json:"nextRefreshAt,omitempty"`
	RetryAt       int64  `json:"retryAt,omitempty"`
	FailureCount  int    `json:"failureCount,omitempty"`
	Stale         bool   `json:"stale"`
	LastError     string `json:"lastError,omitempty"`
}

type accountRuntimeView struct {
	ID          string                 `json:"id"`
	Generation  uint64                 `json:"generation"`
	ExpiresAt   int64                  `json:"expiresAt,omitempty"`
	Credential  accountRuntimeLaneView `json:"credential"`
	AccountInfo accountRuntimeLaneView `json:"accountInfo"`
}

type accountRuntimeSnapshot struct {
	Instance   string               `json:"instance"`
	Revision   uint64               `json:"revision"`
	ServerTime int64                `json:"serverTime"`
	Accounts   []accountRuntimeView `json:"accounts"`
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func maxUnixTime(a time.Time, b int64) int64 {
	av := unixOrZero(a)
	if b > av {
		return b
	}
	return av
}

func accountInfoDueAt(account *config.Account, key accountReconcileKey, state *accountReconcileState) time.Time {
	if account == nil || state == nil || !accountSupportsBackgroundInfo(account) {
		return time.Time{}
	}
	if account.LastRefresh > 0 {
		return time.Unix(account.LastRefresh, 0).Add(accountInfoRefreshIntervalFor(key))
	}
	if state.firstSeen.IsZero() {
		return time.Time{}
	}
	return state.firstSeen.Add(stableAccountJitter(key, "initial-account-info", accountInfoInitialJitter))
}

func runtimeViewForAccount(now time.Time, account config.Account, state *accountReconcileState) accountRuntimeView {
	key, _ := accountReconcileKeyFor(&account)
	view := accountRuntimeView{
		ID:         account.ID,
		Generation: account.DispatchGeneration,
		ExpiresAt:  account.ExpiresAt,
	}
	if state == nil {
		return view
	}

	credentialSupported := accountSupportsCredentialMaintenance(&account)
	credentialNext := time.Time{}
	if credentialSupported {
		credentialNext = credentialRefreshAt(&account, key)
	}
	credentialExpiredOrMissing := false
	if credentialSupported {
		credentialExpiredOrMissing = strings.TrimSpace(account.AccessToken) == "" ||
			(account.ExpiresAt > 0 && now.Unix() >= account.ExpiresAt)
	}
	view.Credential = accountRuntimeLaneView{
		Supported:     credentialSupported,
		Refreshing:    state.credentialRefreshing,
		LastAttemptAt: unixOrZero(state.credentialLastAttempt),
		LastSuccessAt: unixOrZero(state.credentialLastSuccess),
		NextRefreshAt: unixOrZero(credentialNext),
		RetryAt:       unixOrZero(state.credentialRetryAt),
		FailureCount:  state.credentialFailures,
		Stale: credentialExpiredOrMissing ||
			(state.credentialFailures > 0 && !credentialNext.IsZero() && !now.Before(credentialNext)),
		LastError: state.credentialLastError,
	}

	infoNext := accountInfoDueAt(&account, key, state)
	view.AccountInfo = accountRuntimeLaneView{
		Supported:     accountSupportsBackgroundInfo(&account),
		Refreshing:    state.infoRefreshing,
		LastAttemptAt: unixOrZero(state.infoLastAttempt),
		LastSuccessAt: maxUnixTime(state.infoLastSuccess, account.LastRefresh),
		NextRefreshAt: unixOrZero(infoNext),
		RetryAt:       unixOrZero(state.infoRetryAt),
		FailureCount:  state.infoFailures,
		Stale: state.infoFailures > 0 ||
			(!infoNext.IsZero() && !now.Before(infoNext) && !state.infoRefreshing),
		LastError: state.infoLastError,
	}
	return view
}

func safeAccountRuntimeError(lane string, err error) string {
	if err == nil {
		return ""
	}
	lane = strings.TrimSpace(lane)
	if lane == "" {
		lane = "account"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return lane + " refresh timed out"
	case errors.Is(err, context.Canceled):
		return lane + " refresh canceled"
	case pool.IsSuspensionError(err):
		return "account suspended"
	case pool.IsAuthFailure(err):
		return "authentication failed"
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "profile") &&
		(strings.Contains(lower, "unavailable") || strings.Contains(lower, "not found")):
		return "profile unavailable"
	case strings.Contains(lower, "quota") || strings.Contains(lower, "usage limit"):
		return "account quota refresh failed"
	default:
		return lane + " refresh failed"
	}
}

// syncAccountRuntimeFromReconciler publishes a local-only projection of the
// reconciler lanes. Server wall-clock time is intentionally excluded from the
// revision comparison so a browser can poll frequently without manufacturing a
// new revision on every request.
func (h *Handler) syncAccountRuntimeFromReconciler(
	now time.Time,
	states map[accountReconcileKey]*accountReconcileState,
) {
	if h == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if states == nil {
		states = make(map[accountReconcileKey]*accountReconcileState)
	}
	accounts := config.GetAccounts()
	next := make([]accountRuntimeView, 0, len(accounts))
	for i := range accounts {
		account := accounts[i]
		key, ok := accountReconcileKeyFor(&account)
		if !ok {
			continue
		}
		state := states[key]
		if state == nil {
			// The runtime snapshot is observational only. Do not seed the
			// reconciler's scheduler state during the 10-second startup grace: doing
			// so would advance firstSeen and subtly move the first upstream account-
			// info probe earlier. Keep firstSeen zero in this temporary projection,
			// so the UI also avoids advertising an account-info deadline that the
			// real scheduler has not established yet.
			temporary := &accountReconcileState{}
			observeCanonicalAccountState(temporary, &account)
			state = temporary
		}
		next = append(next, runtimeViewForAccount(now, account, state))
	}

	h.accountRuntimeMu.Lock()
	if !reflect.DeepEqual(h.accountRuntimeAccounts, next) {
		h.accountRuntimeRevision++
		h.accountRuntimeAccounts = next
	}
	h.accountRuntimeMu.Unlock()
}

func (h *Handler) accountRuntimeSnapshot(now time.Time) accountRuntimeSnapshot {
	if now.IsZero() {
		now = time.Now()
	}
	h.accountRuntimeMu.Lock()
	if h.accountRuntimeInstance == "" {
		// The representation validator must change across Handler/process
		// incarnations even when both happen to be at revision 1. Otherwise a
		// browser that survives a server restart could send the old ETag and get a
		// false 304 for an unrelated new runtime snapshot.
		h.accountRuntimeInstance = uuid.NewString()
	}
	instance := h.accountRuntimeInstance
	revision := h.accountRuntimeRevision
	accounts := append([]accountRuntimeView(nil), h.accountRuntimeAccounts...)
	h.accountRuntimeMu.Unlock()
	return accountRuntimeSnapshot{
		Instance:   instance,
		Revision:   revision,
		ServerTime: now.Unix(),
		Accounts:   accounts,
	}
}

func accountRuntimeETag(instance string, revision uint64) string {
	return fmt.Sprintf(`W/"accounts-runtime-%s-%d"`, instance, revision)
}

func stripWeakETagPrefix(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "W/") {
		value = strings.TrimSpace(value[2:])
	}
	return value
}

func nextETagListToken(value string, start int) (string, int) {
	for start < len(value) && (value[start] == ' ' || value[start] == '\t' || value[start] == ',') {
		start++
	}
	if start >= len(value) {
		return "", len(value)
	}
	if value[start] == '*' {
		return "*", start + 1
	}
	tokenStart := start
	if start+2 <= len(value) && value[start:start+2] == "W/" {
		start += 2
		for start < len(value) && (value[start] == ' ' || value[start] == '\t') {
			start++
		}
	}
	if start >= len(value) || value[start] != '"' {
		for start < len(value) && value[start] != ',' {
			start++
		}
		return strings.TrimSpace(value[tokenStart:start]), start
	}
	start++
	for start < len(value) && value[start] != '"' {
		start++
	}
	if start < len(value) {
		start++
	}
	return strings.TrimSpace(value[tokenStart:start]), start
}

func weakETagMatches(headerValue, current string) bool {
	current = stripWeakETagPrefix(current)
	for pos := 0; pos < len(headerValue); {
		token, next := nextETagListToken(headerValue, pos)
		if next <= pos {
			break
		}
		pos = next
		if token == "*" {
			return true
		}
		if token != "" && stripWeakETagPrefix(token) == current {
			return true
		}
	}
	return false
}

// apiGetAccountsRuntime is a read-only local snapshot endpoint. It must never
// perform OAuth, quota, model-discovery, or other upstream work.
func (h *Handler) apiGetAccountsRuntime(w http.ResponseWriter, r *http.Request) {
	snapshot := h.accountRuntimeSnapshot(time.Now())
	etag := accountRuntimeETag(snapshot.Instance, snapshot.Revision)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", etag)
	if weakETagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(snapshot)
}
