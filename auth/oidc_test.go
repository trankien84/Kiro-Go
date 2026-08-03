package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
)

// TestRefreshLockCollapsesConcurrentRefreshes verifies that N goroutines
// refreshing the same expired-token account collapse into a single IdP POST:
// the first caller refreshes+persists under the per-account lock, and every
// follower's double-check reads the now-fresh token from config and skips the
// POST. Before the fix each goroutine POSTed independently (refresh-storm +
// lost-update on refresh-token rotation).
func TestRefreshLockCollapsesConcurrentRefreshes(t *testing.T) {
	// The per-account refresh lock is auth-method agnostic, so this drives the
	// *social* refresh path: it is the one refresh grant whose endpoint is a
	// package-level var, which lets the test point it at an httptest server
	// without having to defeat the external-IdP tenant/allow-list validation
	// (that validation is covered by microsoft_sso_test.go, not here).
	origSocialURL := socialTokenURL
	defer func() { socialTokenURL = origSocialURL }()

	var postCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&postCount, 1)
		// Hold the response so the other goroutines are definitely waiting on
		// the lock while the leader POSTs (makes the test robust to scheduling).
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  "at-fresh",
			"refreshToken": "rt-rotated",
			"expiresIn":    3600,
		})
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "acct",
		AuthMethod:   "social",
		RefreshToken: "rt-old",
		ExpiresAt:    1, // expired → every caller wants a refresh
		Enabled:      true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	tokens := make([]string, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			acc := config.Account{
				ID: "acct", AuthMethod: "social",
				RefreshToken: "rt-old", ExpiresAt: 1,
			}
			<-start // release all goroutines at once to maximise overlap
			at, _, _, _, err := RefreshToken(&acc)
			tokens[idx] = at
			errs[idx] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d refresh failed: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&postCount); got != 1 {
		t.Fatalf("expected exactly 1 IdP POST (collapse), got %d", got)
	}
	for i, tk := range tokens {
		if tk != "at-fresh" {
			t.Fatalf("goroutine %d got token %q, want the shared fresh token", i, tk)
		}
	}
}

// stubProxyClient registers proxyURL in authProxyClientCache with a transport
// that intercepts every request (no real DNS/network), and returns the URL
// so callers can stamp it onto config.Account.ProxyURL. refreshTokenDirect
// resolves GetAuthClientForProxy(account.ProxyURL) internally, so this is the
// seam these tests use instead of mutating the shared global auth client.
func stubProxyClient(t *testing.T, proxyURL string, fn microsoftRoundTripFunc) {
	t.Helper()
	authProxyClientCache.Store(proxyURL, &http.Client{Transport: fn})
	t.Cleanup(func() { authProxyClientCache.Delete(proxyURL) })
}

// microsoftAccountForTest builds an id-less external_idp account classified
// Microsoft (Provider="AzureAD") — matching what the interactive enterprise-SSO
// login (kiro_sso.go's exchange()) always persists, genuinely Microsoft or not.
func microsoftAccountForTest(proxyURL, scopes string) *config.Account {
	tokenEndpoint := issuerBase(testMicrosoftIssuer()) + "/" + testMicrosoftTenantID + "/oauth2/v2.0/token"
	return &config.Account{
		AuthMethod:    MicrosoftSSOAuthMethod,
		Provider:      "AzureAD",
		ClientID:      testMicrosoftClientID,
		TokenEndpoint: tokenEndpoint,
		IssuerURL:     testMicrosoftIssuer(),
		Scopes:        scopes,
		RefreshToken:  "refresh-old",
		ProxyURL:      proxyURL,
	}
}

// TestRefreshTokenDirectMicrosoftStrictPathNormalizesScopes is the "strict path
// still runs for a genuinely Microsoft account" half of F1's three-way test
// requirement. A duplicate "openid" in the persisted scopes is only removed by
// validateMicrosoftScopes' normalization (auth/microsoft_sso.go) — the generic
// path would send the raw, unnormalized string — so the exact scope string the
// server receives proves the strict Microsoft path ran, not a silent fallback.
func TestRefreshTokenDirectMicrosoftStrictPathNormalizesScopes(t *testing.T) {
	const proxyURL = "test-proxy-strict-normalizes"
	rawScopes := strings.Join([]string{
		"openid", "openid", "profile", "email", "offline_access",
		"api://" + testMicrosoftClientID + "/codewhisperer:conversations",
	}, " ")
	wantNormalized := strings.Join([]string{
		"openid", "profile", "email", "offline_access",
		"api://" + testMicrosoftClientID + "/codewhisperer:conversations",
	}, " ")

	var requests int32
	var gotScope string
	stubProxyClient(t, proxyURL, func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&requests, 1)
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		gotScope = r.PostForm.Get("scope")
		return microsoftJSONResponse(r, http.StatusOK, map[string]any{
			"access_token":  "access-strict",
			"refresh_token": "refresh-strict",
			"expires_in":    3600,
		}), nil
	})

	account := microsoftAccountForTest(proxyURL, rawScopes)
	access, refresh, _, _, err := refreshTokenDirect(account)
	if err != nil {
		t.Fatalf("refreshTokenDirect: %v", err)
	}
	if access != "access-strict" || refresh != "refresh-strict" {
		t.Fatalf("got access=%q refresh=%q, want access-strict/refresh-strict", access, refresh)
	}
	if atomic.LoadInt32(&requests) != 1 {
		t.Fatalf("requests = %d, want exactly 1", requests)
	}
	if gotScope != wantNormalized {
		t.Fatalf("server received scope=%q, want the de-duplicated %q (proves the strict path's normalization ran)", gotScope, wantNormalized)
	}
}

// TestRefreshTokenDirectFallsBackToGenericOnMicrosoftScopeMismatch is F1's core
// fix: an account misclassified Microsoft (Provider="AzureAD" — what every
// interactive external_idp login persists, per kiro_sso.go's exchange(),
// regardless of the actual IdP) but carrying a scope the strict Microsoft
// validator rejects (e.g. a Graph scope from a broader portal descriptor) must
// fall back to the generic refresh instead of dying forever on
// "Microsoft scope ... is not expected for Kiro". The strict attempt fails
// LOCALLY (scope validation runs before any network call), so exactly one HTTP
// request — the generic fallback's — must reach the server.
func TestRefreshTokenDirectFallsBackToGenericOnMicrosoftScopeMismatch(t *testing.T) {
	const proxyURL = "test-proxy-falls-back-scope"
	// "User.Read" is a real Microsoft Graph scope a broader portal descriptor
	// could plausibly include, but it is not in validateMicrosoftScopes' allow
	// list (standard OIDC scopes + api://<clientID>/codewhisperer:*).
	rawScopes := "openid profile email offline_access User.Read"

	var requests int32
	var gotScope, gotGrantType, gotClientID string
	stubProxyClient(t, proxyURL, func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&requests, 1)
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		gotScope = r.PostForm.Get("scope")
		gotGrantType = r.PostForm.Get("grant_type")
		gotClientID = r.PostForm.Get("client_id")
		return microsoftJSONResponse(r, http.StatusOK, map[string]any{
			"access_token":  "access-generic",
			"refresh_token": "refresh-generic",
			"expires_in":    1800,
		}), nil
	})

	account := microsoftAccountForTest(proxyURL, rawScopes)
	access, refresh, _, _, err := refreshTokenDirect(account)
	if err != nil {
		t.Fatalf("refreshTokenDirect: %v, want a successful generic-path fallback", err)
	}
	if access != "access-generic" || refresh != "refresh-generic" {
		t.Fatalf("got access=%q refresh=%q, want access-generic/refresh-generic", access, refresh)
	}
	if atomic.LoadInt32(&requests) != 1 {
		t.Fatalf("requests = %d, want exactly 1 (the strict attempt must fail locally, before any network call)", requests)
	}
	if gotGrantType != "refresh_token" || gotClientID != testMicrosoftClientID {
		t.Fatalf("generic POST form = grant_type=%q client_id=%q, want refresh_token/%s", gotGrantType, gotClientID, testMicrosoftClientID)
	}
	// The generic path imposes no Microsoft-specific shape: it must send the
	// RAW scopes (including the rejected "User.Read"), not a normalized string
	// (which would mean validateMicrosoftScopes' output leaked through, i.e.
	// the strict path silently ran again instead of the plain generic grant).
	if gotScope != rawScopes {
		t.Fatalf("server received scope=%q, want the raw unnormalized %q", gotScope, rawScopes)
	}
}

// TestRefreshTokenDirectDoesNotFallBackOnRealIdpRejection is F1's safety rail:
// a genuinely Microsoft-shaped account (valid scopes/UUID/tenant — the strict
// local validation PASSES) whose refresh token was actually revoked must
// surface that IdP rejection as an error, not silently retry via the generic
// path. Falling back here would mask a real auth failure as an infra hiccup
// and could send the (still-invalid) refresh token to a second endpoint for no
// benefit — the discriminator is validation-SHAPE failure, not upstream
// failure of any kind.
func TestRefreshTokenDirectDoesNotFallBackOnRealIdpRejection(t *testing.T) {
	const proxyURL = "test-proxy-no-fallback-real-rejection"

	var requests int32
	stubProxyClient(t, proxyURL, func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&requests, 1)
		return microsoftJSONResponse(r, http.StatusBadRequest, map[string]any{
			"error":             "invalid_grant",
			"error_description": "refresh token has been revoked",
		}), nil
	})

	account := microsoftAccountForTest(proxyURL, testMicrosoftScopes())
	_, _, _, _, err := refreshTokenDirect(account)
	if err == nil {
		t.Fatal("expected an error: the IdP rejected the refresh token")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("error = %v, want it to surface the IdP's invalid_grant rejection", err)
	}
	if atomic.LoadInt32(&requests) != 1 {
		t.Fatalf("requests = %d, want exactly 1 (a real IdP rejection must NOT trigger a generic-path retry)", requests)
	}
}
