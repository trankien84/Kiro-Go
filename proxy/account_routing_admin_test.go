package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"kiro-go/config"
	"kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func initRoutingAdminProxyTest(t *testing.T) []config.Account {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{
  "password":"changeme",
  "accounts":[
    {"id":"a","enabled":true,"authMethod":"social","accessToken":"a1","refreshToken":"ra","priority":10},
    {"id":"b","enabled":true,"authMethod":"social","accessToken":"b1","refreshToken":"rb","priority":10},
    {"id":"c","enabled":true,"authMethod":"social","accessToken":"c1","refreshToken":"rc","priority":20}
  ]
}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := config.Init(path); err != nil {
		t.Fatal(err)
	}
	return config.GetAccounts()
}

func routingAdminRequest(t *testing.T, h *Handler, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, "http://example.test/admin/api"+path, reader)
	req.Header.Set("X-Admin-Password", "changeme")
	recorder := httptest.NewRecorder()
	h.handleAdminAPI(recorder, req)
	return recorder
}

func routingAdminRawRequest(t *testing.T, h *Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://example.test/admin/api"+path, strings.NewReader(body))
	req.Header.Set("X-Admin-Password", "changeme")
	recorder := httptest.NewRecorder()
	h.handleAdminAPI(recorder, req)
	return recorder
}

func accountRoutingObservations(accounts []config.Account) []config.AccountRoutingObservation {
	out := make([]config.AccountRoutingObservation, len(accounts))
	for i, account := range accounts {
		out[i] = config.AccountRoutingObservation{
			ID:         account.ID,
			Generation: account.DispatchGeneration,
			Priority:   account.Priority,
		}
	}
	return out
}

func setRoutingAdminStrategy(t *testing.T, strategy string) {
	t.Helper()
	if err := config.UpdateAccountSelectionStrategy(strategy); err != nil {
		t.Fatal(err)
	}
}

func TestAccountRoutingAdminStrategyEndpoint(t *testing.T) {
	initRoutingAdminProxyTest(t)
	h := &Handler{}

	get := routingAdminRequest(t, h, http.MethodGet, "/account-routing", nil)
	if get.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", get.Code, get.Body.String())
	}
	var current accountRoutingConfigResponse
	if err := json.Unmarshal(get.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.Strategy != config.AccountSelectionStrategyRoundRobin || current.DefaultPriority != config.DefaultAccountPriority || len(current.SupportedStrategies) != 3 {
		t.Fatalf("unexpected routing config: %#v", current)
	}

	put := routingAdminRequest(t, h, http.MethodPut, "/account-routing", map[string]interface{}{
		"observedStrategy": current.Strategy,
		"strategy":         "priority",
	})
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", put.Code, put.Body.String())
	}
	if got := config.GetAccountSelectionStrategy(); got != config.AccountSelectionStrategyPriority {
		t.Fatalf("strategy=%q", got)
	}

	stale := routingAdminRequest(t, h, http.MethodPut, "/account-routing", map[string]interface{}{
		"observedStrategy": "round-robin",
		"strategy":         "fill-first",
	})
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale strategy status=%d body=%s", stale.Code, stale.Body.String())
	}
	if got := config.GetAccountSelectionStrategy(); got != config.AccountSelectionStrategyPriority {
		t.Fatalf("stale write changed strategy=%q", got)
	}

	bad := routingAdminRequest(t, h, http.MethodPut, "/account-routing", map[string]interface{}{
		"observedStrategy": "priority",
		"strategy":         "least-used",
	})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid strategy status=%d body=%s", bad.Code, bad.Body.String())
	}

	missing := routingAdminRequest(t, h, http.MethodPut, "/account-routing", map[string]interface{}{})
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing strategy fields status=%d body=%s", missing.Code, missing.Body.String())
	}
	unknown := routingAdminRequest(t, h, http.MethodPut, "/account-routing", map[string]interface{}{
		"observedStrategy": "priority",
		"strategy":         "fill-first",
		"surprise":         true,
	})
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown strategy field status=%d body=%s", unknown.Code, unknown.Body.String())
	}
}

func TestAccountRoutingAdminRejectsOversizedJSONWithoutMutation(t *testing.T) {
	initRoutingAdminProxyTest(t)
	h := &Handler{}

	before := config.GetAccountSelectionStrategy()
	body := strings.Repeat(" ", int(maxAccountRoutingRequestBodyBytes)+1) + `{"observedStrategy":"round-robin","strategy":"priority"}`
	recorder := routingAdminRawRequest(t, h, http.MethodPut, "/account-routing", body)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := config.GetAccountSelectionStrategy(); got != before {
		t.Fatalf("oversized request mutated strategy: before=%q after=%q", before, got)
	}
}

func TestAccountRoutingPriorityEndpointFencesLifecycleAndSameLifecycleEdits(t *testing.T) {
	accounts := initRoutingAdminProxyTest(t)
	h := &Handler{}

	updated := routingAdminRequest(t, h, http.MethodPut, "/accounts/a/routing", map[string]interface{}{
		"generation":       accounts[0].DispatchGeneration,
		"observedPriority": 10,
		"priority":         5,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("priority status=%d body=%s", updated.Code, updated.Body.String())
	}
	account, ok := config.GetAccountByID("a")
	if !ok || account.Priority != 5 || account.AccessToken != "a1" || account.RefreshToken != "ra" {
		t.Fatalf("priority mutation damaged account: %#v ok=%v", account, ok)
	}

	sameLifecycleStale := routingAdminRequest(t, h, http.MethodPut, "/accounts/a/routing", map[string]interface{}{
		"generation":       accounts[0].DispatchGeneration,
		"observedPriority": 10,
		"priority":         1,
	})
	if sameLifecycleStale.Code != http.StatusConflict {
		t.Fatalf("same-lifecycle stale priority status=%d body=%s", sameLifecycleStale.Code, sameLifecycleStale.Body.String())
	}

	staleLifecycle := routingAdminRequest(t, h, http.MethodPut, "/accounts/a/routing", map[string]interface{}{
		"generation":       accounts[0].DispatchGeneration + 1,
		"observedPriority": 5,
		"priority":         1,
	})
	if staleLifecycle.Code != http.StatusConflict {
		t.Fatalf("stale lifecycle priority status=%d body=%s", staleLifecycle.Code, staleLifecycle.Body.String())
	}
	account, _ = config.GetAccountByID("a")
	if account.Priority != 5 {
		t.Fatalf("stale request changed priority: %#v", account)
	}

	missing := routingAdminRequest(t, h, http.MethodPut, "/accounts/a/routing", map[string]interface{}{
		"generation":       accounts[0].DispatchGeneration,
		"observedPriority": 5,
	})
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing desired priority status=%d body=%s", missing.Code, missing.Body.String())
	}
	negative := routingAdminRequest(t, h, http.MethodPut, "/accounts/a/routing", map[string]interface{}{
		"generation":       accounts[0].DispatchGeneration,
		"observedPriority": 5,
		"priority":         -1,
	})
	if negative.Code != http.StatusBadRequest {
		t.Fatalf("negative desired priority status=%d body=%s", negative.Code, negative.Body.String())
	}
}

func TestAccountRoutingPriorityEndpointCanRepairLegacyNegativePriority(t *testing.T) {
	accounts := initRoutingAdminProxyTest(t)
	observed := accounts[0]
	if _, _, applied, err := config.MutateAccountIfLifecycle(observed.ID, observed.DispatchGeneration, func(account *config.Account) error {
		account.Priority = -7
		return nil
	}); err != nil || !applied {
		t.Fatalf("seed negative priority applied=%v err=%v", applied, err)
	}
	current, _ := config.GetAccountByID("a")
	h := &Handler{}
	updated := routingAdminRequest(t, h, http.MethodPut, "/accounts/a/routing", map[string]interface{}{
		"generation":       current.DispatchGeneration,
		"observedPriority": -7,
		"priority":         0,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("repair priority status=%d body=%s", updated.Code, updated.Body.String())
	}
	current, _ = config.GetAccountByID("a")
	if current.Priority != 0 {
		t.Fatalf("legacy negative priority was not repaired: %#v", current)
	}
}

func TestAccountRoutingOrderEndpointFencesOrderPriorityAndStrategy(t *testing.T) {
	t.Run("order", func(t *testing.T) {
		accounts := initRoutingAdminProxyTest(t)
		setRoutingAdminStrategy(t, config.AccountSelectionStrategyFillFirst)
		h := &Handler{}
		observed := accountRoutingObservations(accounts)
		desired := []config.AccountRoutingObservation{observed[1], observed[0], observed[2]}
		reordered := routingAdminRequest(t, h, http.MethodPut, "/accounts/order", map[string]interface{}{
			"observedStrategy": config.AccountSelectionStrategyFillFirst,
			"observed":         observed,
			"accounts":         desired,
		})
		if reordered.Code != http.StatusOK {
			t.Fatalf("reorder status=%d body=%s", reordered.Code, reordered.Body.String())
		}
		current := config.GetAccounts()
		if current[0].ID != "b" || current[1].ID != "a" || current[2].ID != "c" {
			t.Fatalf("unexpected order: %#v", current)
		}

		conflict := routingAdminRequest(t, h, http.MethodPut, "/accounts/order", map[string]interface{}{
			"observedStrategy": config.AccountSelectionStrategyFillFirst,
			"observed":         observed,
			"accounts":         []config.AccountRoutingObservation{observed[2], observed[1], observed[0]},
		})
		if conflict.Code != http.StatusConflict {
			t.Fatalf("stale reorder status=%d body=%s", conflict.Code, conflict.Body.String())
		}
	})

	t.Run("priority", func(t *testing.T) {
		accounts := initRoutingAdminProxyTest(t)
		setRoutingAdminStrategy(t, config.AccountSelectionStrategyFillFirst)
		h := &Handler{}
		observed := accountRoutingObservations(accounts)
		if _, _, applied, err := config.MutateAccountIfLifecycle(accounts[1].ID, accounts[1].DispatchGeneration, func(account *config.Account) error {
			account.Priority = 20
			return nil
		}); err != nil || !applied {
			t.Fatalf("priority change applied=%v err=%v", applied, err)
		}
		conflict := routingAdminRequest(t, h, http.MethodPut, "/accounts/order", map[string]interface{}{
			"observedStrategy": config.AccountSelectionStrategyFillFirst,
			"observed":         observed,
			"accounts":         []config.AccountRoutingObservation{observed[1], observed[0], observed[2]},
		})
		if conflict.Code != http.StatusConflict {
			t.Fatalf("stale-priority reorder status=%d body=%s", conflict.Code, conflict.Body.String())
		}
	})

	t.Run("strategy", func(t *testing.T) {
		accounts := initRoutingAdminProxyTest(t)
		setRoutingAdminStrategy(t, config.AccountSelectionStrategyFillFirst)
		h := &Handler{}
		observed := accountRoutingObservations(accounts)
		setRoutingAdminStrategy(t, config.AccountSelectionStrategyPriority)
		conflict := routingAdminRequest(t, h, http.MethodPut, "/accounts/order", map[string]interface{}{
			"observedStrategy": config.AccountSelectionStrategyFillFirst,
			"observed":         observed,
			"accounts":         []config.AccountRoutingObservation{observed[1], observed[0], observed[2]},
		})
		if conflict.Code != http.StatusConflict {
			t.Fatalf("stale-strategy reorder status=%d body=%s", conflict.Code, conflict.Body.String())
		}
	})
}

func TestAccountRoutingOrderEndpointRejectsMalformedOrWrongStrategy(t *testing.T) {
	accounts := initRoutingAdminProxyTest(t)
	setRoutingAdminStrategy(t, config.AccountSelectionStrategyFillFirst)
	h := &Handler{}
	observed := accountRoutingObservations(accounts)

	wrongStrategy := routingAdminRequest(t, h, http.MethodPut, "/accounts/order", map[string]interface{}{
		"observedStrategy": config.AccountSelectionStrategyPriority,
		"observed":         observed,
		"accounts":         observed,
	})
	if wrongStrategy.Code != http.StatusBadRequest {
		t.Fatalf("wrong strategy status=%d body=%s", wrongStrategy.Code, wrongStrategy.Body.String())
	}
	missing := routingAdminRequest(t, h, http.MethodPut, "/accounts/order", map[string]interface{}{
		"observedStrategy": config.AccountSelectionStrategyFillFirst,
	})
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing order status=%d body=%s", missing.Code, missing.Body.String())
	}
	priorityMismatch := append([]config.AccountRoutingObservation(nil), observed...)
	priorityMismatch[1].Priority++
	badPriority := routingAdminRequest(t, h, http.MethodPut, "/accounts/order", map[string]interface{}{
		"observedStrategy": config.AccountSelectionStrategyFillFirst,
		"observed":         observed,
		"accounts":         priorityMismatch,
	})
	if badPriority.Code != http.StatusBadRequest {
		t.Fatalf("priority mismatch status=%d body=%s", badPriority.Code, badPriority.Body.String())
	}
}

func TestAccountRoutingAdminControlsDriveExistingScheduler(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{
  "password":"changeme",
  "accounts":[
    {"id":"routing-admin-a","enabled":true,"authMethod":"social","accessToken":"a1","refreshToken":"ra","priority":10},
    {"id":"routing-admin-b","enabled":true,"authMethod":"social","accessToken":"b1","refreshToken":"rb","priority":10},
    {"id":"routing-admin-c","enabled":true,"authMethod":"social","accessToken":"c1","refreshToken":"rc","priority":20}
  ]
}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := config.Init(path); err != nil {
		t.Fatal(err)
	}
	p := pool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	strategy := routingAdminRequest(t, h, http.MethodPut, "/account-routing", map[string]interface{}{
		"observedStrategy": config.AccountSelectionStrategyRoundRobin,
		"strategy":         config.AccountSelectionStrategyPriority,
	})
	if strategy.Code != http.StatusOK {
		t.Fatalf("priority strategy status=%d body=%s", strategy.Code, strategy.Body.String())
	}

	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		selection := p.GetNextSelectionForModelExcluding("", nil)
		if selection == nil {
			t.Fatal("priority strategy returned no selection")
		}
		if selection.Account.ID == "routing-admin-c" {
			t.Fatalf("priority strategy selected lower tier account: %#v", selection.Account)
		}
		seen[selection.Account.ID] = true
	}
	if !seen["routing-admin-a"] || !seen["routing-admin-b"] {
		t.Fatalf("priority strategy did not round-robin within best tier: %v", seen)
	}

	accounts := config.GetAccounts()
	byID := make(map[string]config.Account, len(accounts))
	for _, account := range accounts {
		byID[account.ID] = account
	}
	priority := routingAdminRequest(t, h, http.MethodPut, "/accounts/routing-admin-a/routing", map[string]interface{}{
		"generation":       byID["routing-admin-a"].DispatchGeneration,
		"observedPriority": 10,
		"priority":         30,
	})
	if priority.Code != http.StatusOK {
		t.Fatalf("priority update status=%d body=%s", priority.Code, priority.Body.String())
	}
	selection := p.GetNextSelectionForModelExcluding("", nil)
	if selection == nil || selection.Account.ID != "routing-admin-b" {
		t.Fatalf("priority update did not reload pool/select best tier: %#v", selection)
	}

	strategy = routingAdminRequest(t, h, http.MethodPut, "/account-routing", map[string]interface{}{
		"observedStrategy": config.AccountSelectionStrategyPriority,
		"strategy":         config.AccountSelectionStrategyFillFirst,
	})
	if strategy.Code != http.StatusOK {
		t.Fatalf("fill-first strategy status=%d body=%s", strategy.Code, strategy.Body.String())
	}
	currentA, _ := config.GetAccountByID("routing-admin-a")
	priority = routingAdminRequest(t, h, http.MethodPut, "/accounts/routing-admin-a/routing", map[string]interface{}{
		"generation":       currentA.DispatchGeneration,
		"observedPriority": 30,
		"priority":         10,
	})
	if priority.Code != http.StatusOK {
		t.Fatalf("priority restore status=%d body=%s", priority.Code, priority.Body.String())
	}
	selection = p.GetNextSelectionForModelExcluding("", nil)
	if selection == nil || selection.Account.ID != "routing-admin-a" {
		t.Fatalf("fill-first did not use first account in best tier before reorder: %#v", selection)
	}

	accounts = config.GetAccounts()
	observed := accountRoutingObservations(accounts)
	desired := append([]config.AccountRoutingObservation(nil), observed...)
	if len(desired) != 3 || desired[0].ID != "routing-admin-a" || desired[1].ID != "routing-admin-b" {
		t.Fatalf("unexpected canonical order before reorder: %#v", desired)
	}
	desired[0], desired[1] = desired[1], desired[0]
	reordered := routingAdminRequest(t, h, http.MethodPut, "/accounts/order", map[string]interface{}{
		"observedStrategy": config.AccountSelectionStrategyFillFirst,
		"observed":         observed,
		"accounts":         desired,
	})
	if reordered.Code != http.StatusOK {
		t.Fatalf("reorder status=%d body=%s", reordered.Code, reordered.Body.String())
	}
	selection = p.GetNextSelectionForModelExcluding("", nil)
	if selection == nil || selection.Account.ID != "routing-admin-b" {
		t.Fatalf("fill-first reorder did not change selected account: %#v", selection)
	}
}

func TestAccountListExposesRoutingPriority(t *testing.T) {
	initRoutingAdminProxyTest(t)
	p := pool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.test/admin/api/accounts", nil)
	h.apiGetAccounts(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var accounts []map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 3 {
		t.Fatalf("accounts=%d", len(accounts))
	}
	if accounts[0]["generation"] == nil || accounts[0]["priority"] != float64(10) || accounts[0]["effectivePriority"] != float64(10) {
		t.Fatalf("routing fields missing: %#v", accounts[0])
	}
}
