package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// installCleanAuthClient replaces the global auth HTTP client with one whose
// Transport does not consult http.ProxyFromEnvironment — that function caches
// env vars on first call and would otherwise poison TestBuildKiroTransport*
// when tests run in the default order. Returns a cleanup that restores the
// previous client.
func installCleanAuthClient(t *testing.T) func() {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
	prev := auth.SetGlobalAuthClientForTest(c)
	return func() { auth.SetGlobalAuthClientForTest(prev) }
}

// TestApiImportCredentialsRejectsWhenRefreshFails verifies the regression:
// previously, when auth.RefreshToken failed and the user supplied an accessToken,
// the handler stored that accessToken with ExpiresAt = now+300, producing an
// account that the pool would skip (Pick uses now > ExpiresAt-120 → ~3 min) and
// that the on-demand refresh path could never repair (Pick filters it out before
// ensureValidToken runs). The fix is to reject the import outright; the caller
// must provide a refreshToken that actually works.
func TestApiImportCredentialsRejectsWhenRefreshFails(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	// Stand up a fake OIDC endpoint that always 400s, simulating an unreachable
	// or invalid refresh.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer fake.Close()

	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return fake.URL })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	h := &Handler{pool: accountpool.GetPool()}

	body := `{"refreshToken":"rt-broken","accessToken":"at-still-valid-elsewhere","clientId":"c","clientSecret":"s","authMethod":"idc","region":"us-east-1"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when refresh fails, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "Token refresh failed") {
		t.Fatalf("expected refresh-failed error, got %q", resp["error"])
	}

	// Crucial: no account should have been created. The previous bug stored a
	// half-broken account with ExpiresAt ~now+300 that would die in 3 minutes.
	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("expected no accounts to be persisted on failed import, got %+v", accs)
	}
}

// TestApiImportCredentialsUsesUpstreamExpiresAt verifies the happy path: when
// refresh succeeds, the persisted ExpiresAt reflects the upstream expiresIn,
// not a hard-coded 300s.
func TestApiImportCredentialsUsesUpstreamExpiresAt(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	const upstreamExpiresIn = 3600
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"accessToken":"at-new","refreshToken":"rt-rotated","expiresIn":%d,"profileArn":"arn:aws:codewhisperer:profile/test"}`, upstreamExpiresIn)
	}))
	defer fake.Close()

	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return fake.URL })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	h := &Handler{pool: accountpool.GetPool()}

	before := time.Now().Unix()
	body := `{"refreshToken":"rt-good","clientId":"c","clientSecret":"s","authMethod":"idc","region":"us-east-1"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)
	after := time.Now().Unix()

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on successful refresh, got %d body=%s", rec.Code, rec.Body.String())
	}

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected exactly one account persisted, got %d", len(accs))
	}
	got := accs[0]
	if got.AccessToken != "at-new" {
		t.Fatalf("expected upstream-issued accessToken, got %q", got.AccessToken)
	}
	if got.RefreshToken != "rt-rotated" {
		t.Fatalf("expected rotated refreshToken to be persisted, got %q", got.RefreshToken)
	}
	// Allow ±5s of drift but require the value to clearly come from upstream's
	// expiresIn rather than the old 300s fallback.
	expectMin := before + upstreamExpiresIn - 5
	expectMax := after + upstreamExpiresIn + 5
	if got.ExpiresAt < expectMin || got.ExpiresAt > expectMax {
		t.Fatalf("expected ExpiresAt ≈ now+%d ([%d..%d]), got %d", upstreamExpiresIn, expectMin, expectMax, got.ExpiresAt)
	}
	if got.ExpiresAt-time.Now().Unix() < 1500 {
		t.Fatalf("ExpiresAt too short — looks like the 300s fallback is still in play: %d (delta %d)", got.ExpiresAt, got.ExpiresAt-time.Now().Unix())
	}
}

// authOidcURL captures the current oidc URL builder so the test can restore it.
func authOidcURL() func(string) string { return auth.GetOIDCTokenURLForTest() }

// TestNormalizeImportAuthMethod pins the auth-method normalization for import,
// including the key regression: external_idp accounts carry clientId but NO
// clientSecret, so the old default branch misclassified them as "social".
func TestNormalizeImportAuthMethod(t *testing.T) {
	cases := []struct {
		name          string
		authMethod    string
		clientID      string
		clientSecret  string
		tokenEndpoint string
		want          string
	}{
		{"explicit external_idp", "external_idp", "c", "", "https://login.microsoftonline.com/t/oauth2/v2.0/token", "external_idp"},
		{"azure alias", "AzureAD", "c", "", "https://login.microsoftonline.com/t/oauth2/v2.0/token", "external_idp"},
		{"microsoft alias", "microsoft", "c", "", "https://login.microsoftonline.com/t/oauth2/v2.0/token", "external_idp"},
		{"inferred from tokenEndpoint", "", "c", "", "https://login.microsoftonline.com/t/oauth2/v2.0/token", "external_idp"},
		{"external_idp even with clientSecret", "external_idp", "c", "s", "https://login.microsoftonline.com/t/oauth2/v2.0/token", "external_idp"},
		{"enterprise stays idc", "enterprise", "c", "s", "", "idc"},
		{"idc with clientid+secret", "idc", "c", "s", "", "idc"},
		{"empty + clientid (no secret) -> idc", "", "c", "", "", "idc"},
		{"empty no clientid -> social", "", "", "", "", "social"},
		{"social explicit", "social", "", "", "", "social"},
		{"google alias", "google", "", "", "", "social"},
		{"unrecognized with clientid+secret -> idc", "weird", "c", "s", "", "idc"},
		{"unrecognized without secret -> social", "weird", "c", "", "", "social"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeImportAuthMethod(tc.authMethod, tc.clientID, tc.clientSecret, tc.tokenEndpoint); got != tc.want {
				t.Fatalf("normalizeImportAuthMethod(%q,%q,%q,%q) = %q, want %q",
					tc.authMethod, tc.clientID, tc.clientSecret, tc.tokenEndpoint, got, tc.want)
			}
		})
	}
}

// TestApiImportCredentialsExternalIdpHappyPath verifies an external_idp credential
// imports successfully: authMethod normalizes to external_idp, refresh hits the
// (fake) IdP token endpoint, and the account is persisted with all refresh material.
func TestApiImportCredentialsExternalIdpHappyPath(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	const upstreamExpiresIn = 3600
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if got := r.PostForm.Get("grant_type"); got != "refresh_token" {
			t.Errorf("expected grant_type=refresh_token, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		// external IdP token responses are snake_case.
		fmt.Fprintf(w, `{"access_token":"at-ext","refresh_token":"rt-rotated","expires_in":%d}`, upstreamExpiresIn)
	}))
	defer fake.Close()

	// fake.URL is http + 127.0.0.1; bypass the allow-list validator for this test.
	restore := auth.SetExternalIdpValidatorForTest(func(string) error { return nil })
	defer auth.SetExternalIdpValidatorForTest(restore)

	h := &Handler{pool: accountpool.GetPool()}

	body := fmt.Sprintf(`{"authMethod":"external_idp","refreshToken":"rt-ext","clientId":"ext-client","tokenEndpoint":%q,"issuerUrl":"https://login.microsoftonline.com/t/v2.0","scopes":"api://x/codewhisperer:conversations offline_access","region":"eu-central-1"}`, fake.URL)
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	before := time.Now().Unix()
	h.apiImportCredentials(rec, req)
	after := time.Now().Unix()

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected 1 account persisted, got %d", len(accs))
	}
	got := accs[0]
	if got.AuthMethod != "external_idp" {
		t.Fatalf("AuthMethod: want external_idp, got %q", got.AuthMethod)
	}
	if got.AccessToken != "at-ext" {
		t.Fatalf("AccessToken: want at-ext, got %q", got.AccessToken)
	}
	if got.RefreshToken != "rt-rotated" {
		t.Fatalf("RefreshToken: want rt-rotated (rotated), got %q", got.RefreshToken)
	}
	if got.TokenEndpoint != fake.URL {
		t.Fatalf("TokenEndpoint not persisted: got %q", got.TokenEndpoint)
	}
	if got.ClientID != "ext-client" {
		t.Fatalf("ClientID not persisted: got %q", got.ClientID)
	}
	if got.Scopes == "" {
		t.Fatalf("Scopes not persisted: got %q", got.Scopes)
	}
	if got.Provider != "AzureAD" {
		t.Fatalf("Provider default: want AzureAD, got %q", got.Provider)
	}
	if got.Region != "eu-central-1" {
		t.Fatalf("Region: want eu-central-1, got %q", got.Region)
	}
	if got.ExpiresAt < before+upstreamExpiresIn-5 || got.ExpiresAt > after+upstreamExpiresIn+5 {
		t.Fatalf("ExpiresAt not from upstream expiresIn: got %d (want ~now+%d)", got.ExpiresAt, upstreamExpiresIn)
	}
}

// TestApiImportCredentialsExternalIdpRejectsNonAllowListedEndpoint verifies the SSRF
// guard: a tokenEndpoint outside the IdP allow-list is rejected with 400 before any
// refresh POST, and nothing is persisted. (Validator is NOT bypassed here.)
func TestApiImportCredentialsExternalIdpRejectsNonAllowListedEndpoint(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	h := &Handler{pool: accountpool.GetPool()}

	body := `{"authMethod":"external_idp","refreshToken":"rt","clientId":"c","tokenEndpoint":"https://evil.example.com/oauth/token","region":"us-east-1"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "endpoint rejected") {
		t.Fatalf("expected endpoint-rejected error, got %q", resp["error"])
	}
	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("expected no account persisted, got %d", len(accs))
	}
}

// TestApiImportCredentialsExternalIdpRejectsWhenRefreshFails verifies the refresh
// gate holds for external_idp: a refresh that 400s (invalid_grant) must reject the
// import and persist nothing.
func TestApiImportCredentialsExternalIdpRejectsWhenRefreshFails(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer fake.Close()

	restore := auth.SetExternalIdpValidatorForTest(func(string) error { return nil })
	defer auth.SetExternalIdpValidatorForTest(restore)

	h := &Handler{pool: accountpool.GetPool()}

	body := fmt.Sprintf(`{"authMethod":"external_idp","refreshToken":"rt-broken","clientId":"c","tokenEndpoint":%q,"region":"us-east-1"}`, fake.URL)
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "Token refresh failed") {
		t.Fatalf("expected refresh-failed error, got %q", resp["error"])
	}
	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("expected no account persisted, got %d", len(accs))
	}
}

// TestApiImportCredentialsExternalIdpPreservesFullRecordIdentity verifies that when
// a full account record (with id/email/profileArn) is pasted, those are preserved
// rather than regenerated, so re-importing a backup does not duplicate accounts.
func TestApiImportCredentialsExternalIdpPreservesFullRecordIdentity(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-ext","refresh_token":"rt-rotated","expires_in":3600}`)
	}))
	defer fake.Close()

	restore := auth.SetExternalIdpValidatorForTest(func(string) error { return nil })
	defer auth.SetExternalIdpValidatorForTest(restore)

	h := &Handler{pool: accountpool.GetPool()}

	const providedID = "11111111-2222-3333-4444-555555555555"
	// Account-ID segment must be a real 12-digit AWS account id (kiroAccountPattern);
	// "1" was a shorthand placeholder that no longer parses as a valid profile ARN.
	body := fmt.Sprintf(`{"id":%q,"email":"ada@example.com","profileArn":"arn:aws:codewhisperer:eu-central-1:123456789012:profile/PRESERVED","authMethod":"external_idp","refreshToken":"rt","clientId":"c","tokenEndpoint":%q,"region":"eu-central-1"}`, providedID, fake.URL)
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	got := config.GetAccounts()[0]
	if got.ID != providedID {
		t.Fatalf("ID: want reused %q, got %q", providedID, got.ID)
	}
	if got.Email != "ada@example.com" {
		t.Fatalf("Email: want ada@example.com (GetUserInfo empty in test → fallback), got %q", got.Email)
	}
	if got.ProfileArn != "arn:aws:codewhisperer:eu-central-1:123456789012:profile/PRESERVED" {
		t.Fatalf("ProfileArn: want preserved, got %q", got.ProfileArn)
	}
}

// TestApiImportCredentialsExternalIdpDerivesEndpointsFromUserId verifies the
// Kiro Account Manager export shape: a credential carrying only refreshToken +
// clientId + userId (NO tokenEndpoint/issuerUrl/scopes) is accepted, with the
// endpoints+scopes derived from userId's embedded Azure tenant.
func TestApiImportCredentialsExternalIdpDerivesEndpointsFromUserId(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-derived","refresh_token":"rt-d2","expires_in":3600}`)
	}))
	defer fake.Close()

	restore := auth.SetExternalIdpValidatorForTest(func(string) error { return nil })
	defer auth.SetExternalIdpValidatorForTest(restore)

	h := &Handler{pool: accountpool.GetPool()}

	// userId points at the fake so the derived tokenEndpoint hits it.
	userID := fake.URL + "/5fbc183e-3d09-4043-b36f-0c49d3665977/v2.0.8db0e2eb-d491-4a1a-98f1-cbdc12bb60a0"
	body := fmt.Sprintf(`{"authMethod":"external_idp","refreshToken":"rt","clientId":"fa6d79bf-cdaa-495e-8359-78aab7c7cd9b","userId":%q,"region":"eu-central-1"}`, userID)
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	got := config.GetAccounts()[0]
	wantTE := fake.URL + "/5fbc183e-3d09-4043-b36f-0c49d3665977/oauth2/v2.0/token"
	if got.TokenEndpoint != wantTE {
		t.Fatalf("derived TokenEndpoint: want %q, got %q", wantTE, got.TokenEndpoint)
	}
	if got.IssuerURL != fake.URL+"/5fbc183e-3d09-4043-b36f-0c49d3665977/v2.0" {
		t.Fatalf("derived IssuerURL: got %q", got.IssuerURL)
	}
	if !strings.Contains(got.Scopes, "codewhisperer:conversations") || !strings.Contains(got.Scopes, "offline_access") {
		t.Fatalf("derived Scopes: got %q", got.Scopes)
	}
	if got.AccessToken != "at-derived" {
		t.Fatalf("AccessToken: want at-derived, got %q", got.AccessToken)
	}
}

// TestApiImportCredentialsExternalIdpDerivesFromAccessTokenJWT verifies a bare
// credential blob (clientId + accessToken + refreshToken, NO authMethod/userId/
// tokenEndpoint) imports via TRUST-ON-IMPORT: the accessToken's JWT issuer
// classifies it as external_idp + yields the tenant to derive the endpoint from,
// and — because the token is an Azure AD JWT with a real exp — the credential is
// persisted verbatim WITHOUT a live refresh round-trip (so the same JSON can be
// re-imported without the refresh token getting consumed/rotated).
func TestApiImportCredentialsExternalIdpDerivesFromAccessTokenJWT(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	// Sentinel: if trust-on-import were broken, a refresh would hit this server and
	// the persisted AccessToken would be "at-jwt" instead of the pasted JWT.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-jwt","refresh_token":"rt-j2","expires_in":3600}`)
	}))
	defer fake.Close()

	restore := auth.SetExternalIdpValidatorForTest(func(string) error { return nil })
	defer auth.SetExternalIdpValidatorForTest(restore)

	// Bare blob: only clientId + accessToken + refreshToken. The access token is an
	// Azure AD JWT (iss + future exp) → external_idp + derived tenant + trust-on-import
	// (NO live refresh; pasted tokens persist verbatim, ExpiresAt from JWT exp).
	tenant := "5fbc183e-3d09-4043-b36f-0c49d3665977"
	const exp int64 = 2000000000
	jwt := "eyJhbGciOiJub25lIn0." +
		base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"iss":%q,"exp":%d}`, fake.URL+"/"+tenant+"/v2.0", exp))) + "."

	h := &Handler{pool: accountpool.GetPool()}

	body := fmt.Sprintf(`{"clientId":"fa6d79bf-cdaa-495e-8359-78aab7c7cd9b","accessToken":%q,"refreshToken":"rt","region":"eu-central-1"}`, jwt)
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (trust-on-import), got %d body=%s", rec.Code, rec.Body.String())
	}
	got := config.GetAccounts()[0]
	if got.AuthMethod != "external_idp" {
		t.Fatalf("AuthMethod: want external_idp (derived from accessToken JWT), got %q", got.AuthMethod)
	}
	wantTE := fake.URL + "/" + tenant + "/oauth2/v2.0/token"
	if got.TokenEndpoint != wantTE {
		t.Fatalf("derived TokenEndpoint: want %q, got %q", wantTE, got.TokenEndpoint)
	}
	if got.AccessToken != jwt {
		t.Fatalf("AccessToken: want the pasted JWT persisted verbatim (trust-on-import), got %q", got.AccessToken)
	}
	if got.ExpiresAt != exp {
		t.Fatalf("ExpiresAt: want %d (from JWT exp, trust-on-import), got %d", exp, got.ExpiresAt)
	}
	if got.RefreshToken != "rt" {
		t.Fatalf("RefreshToken: want the pasted token (not rotated), got %q", got.RefreshToken)
	}
}

// TestApiImportCredentialsAPIKeySuccess verifies importing a Kiro API-key
// credential: it persists AuthMethod "api_key" with the key mirrored into
// AccessToken (so pool selection logic that only looks at AccessToken still
// works), region parsed from the "key|region" suffix, and no OAuth fields set.
func TestApiImportCredentialsAPIKeySuccess(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// The import handler spawns a background, un-awaited model-list refresh
	// for enabled accounts. Route it through a stub transport with no Proxy
	// configured (rather than the real http.ProxyFromEnvironment-backed
	// client) and restore to an equally inert stub afterward — never back to
	// the real client — since the goroutine may still be in flight after this
	// test returns and would otherwise poison TestBuildKiroTransport*, which
	// relies on http.ProxyFromEnvironment reading env vars for the first time.
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("network disabled in test")
		}),
	})
	t.Cleanup(func() {
		kiroRestHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	})

	h := &Handler{pool: accountpool.GetPool()}
	body := `{"kiroApiKey":"ksk_test_import|eu-central-1","authMethod":"api_key","nickname":"cli-key"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accs))
	}
	got := accs[0]
	if got.AuthMethod != "api_key" || got.KiroApiKey != "ksk_test_import" {
		t.Fatalf("unexpected account: %+v", got)
	}
	if got.AccessToken != "ksk_test_import" {
		t.Fatalf("accessToken should mirror api key, got %q", got.AccessToken)
	}
	if got.Region != "eu-central-1" {
		t.Fatalf("region = %q", got.Region)
	}
	if got.RefreshToken != "" || got.ExpiresAt != 0 || got.ProfileArn != "" {
		t.Fatalf("oauth fields should be empty: %+v", got)
	}
	if got.MachineId != config.MachineIdFromAPIKey("ksk_test_import") {
		t.Fatalf("machineId = %q", got.MachineId)
	}
}

// TestApiImportCredentialsAPIKeyDuplicateRejected verifies re-importing the
// same Kiro API key is rejected with 409 rather than creating a duplicate
// account entry.
func TestApiImportCredentialsAPIKeyDuplicateRejected(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// See TestApiImportCredentialsAPIKeySuccess: avoid ever restoring the real
	// http.ProxyFromEnvironment-backed client, since the background model
	// refresh goroutine may still be in flight after this test returns.
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("network disabled in test")
		}),
	})
	t.Cleanup(func() {
		kiroRestHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	})

	h := &Handler{pool: accountpool.GetPool()}
	body := `{"kiroApiKey":"ksk_dup_import","authMethod":"api_key"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first import expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	req2 := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec2 := httptest.NewRecorder()
	h.apiImportCredentials(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("duplicate expected 409, got %d body=%s", rec2.Code, rec2.Body.String())
	}
}

// TestApiImportCredentialsAPIKeyDropsUnverifiedIdentityOnProbeFailure is the
// regression test for H1: when resolveApiKeyRegion's live probe fails (network
// down, key mistyped, transient upstream outage — the import must still
// succeed per the CLI export/restore trust-on-import contract, matching
// TestApiImportCredentialsAPIKeySuccess), the caller-supplied email/userId
// were previously persisted verbatim with NO live confirmation they actually
// belong to this key. findAccountForKiroIdentity (admin_bot_api.go) later
// matches on exactly those fields to recognize "the same Kiro account" — so a
// bad-key import carrying a real account's identity could plant a slot that
// makes a later legitimate supply-side add either get discarded as a false
// duplicate or overwrite its live credential into the planted slot. The fix
// drops (blanks) email/userId whenever the probe could not verify them; the
// key and region still import unchanged (availability preserved).
func TestApiImportCredentialsAPIKeyDropsUnverifiedIdentityOnProbeFailure(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// Network disabled → resolveApiKeyRegion's probe fails for every candidate
	// region, exactly like TestApiImportCredentialsAPIKeySuccess.
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("network disabled in test")
		}),
	})
	t.Cleanup(func() {
		kiroRestHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	})

	h := &Handler{pool: accountpool.GetPool()}
	body := `{"kiroApiKey":"ksk_unverified|eu-central-1","authMethod":"api_key","email":"real-owner@example.com","userId":"real-owner-uid"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (trust-on-import must still succeed), got %d body=%s", rec.Code, rec.Body.String())
	}
	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accs))
	}
	got := accs[0]
	if got.KiroApiKey != "ksk_unverified" || got.Region != "eu-central-1" {
		t.Fatalf("key/region should still import from the unverifiable request: %+v", got)
	}
	// UserId has no synthetic fallback (config.NormalizeAPIKeyAccount only
	// backfills Email), so it staying blank directly proves the caller-supplied
	// value was dropped. Email gets a synthetic "api-key-<fingerprint>" display
	// placeholder deterministically derived from the KEY itself when blank
	// (NormalizeAPIKeyAccount) — harmless for identity matching (an attacker
	// cannot steer it to collide with a real account's email) — so the
	// assertion is "not the attacker-supplied value", not "empty".
	if got.UserId != "" {
		t.Fatalf("userId=%q, want blank — a failed probe must not let unverified caller-supplied identity into the account", got.UserId)
	}
	if got.Email == "real-owner@example.com" {
		t.Fatalf("email=%q, want the caller-supplied (unverified) identity dropped, not persisted verbatim", got.Email)
	}
}
