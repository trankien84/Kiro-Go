package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempCache writes content to a temp file and returns its path.
func writeTempCache(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kiro-auth-token.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp cache: %v", err)
	}
	return path
}

// TestReadIdeCacheCredentialExternalIdp verifies the Kiro IDE's camelCase
// external_idp cache maps onto a complete importCredentialRequest.
func TestReadIdeCacheCredentialExternalIdp(t *testing.T) {
	path := writeTempCache(t, `{
	  "accessToken": "at-ide",
	  "refreshToken": "rt-ide",
	  "expiresAt": "2026-06-26T09:46:28.654Z",
	  "authMethod": "external_idp",
	  "provider": "ExternalIdp",
	  "tokenEndpoint": "https://login.microsoftonline.com/tenant/oauth2/v2.0/token",
	  "issuerUrl": "https://login.microsoftonline.com/tenant/v2.0",
	  "clientId": "azure-client-123",
	  "scopes": "api://azure-client-123/codewhisperer:conversations offline_access"
	}`)

	req, err := readIdeCacheCredential(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.AuthMethod != "external_idp" {
		t.Fatalf("authMethod = %q, want external_idp", req.AuthMethod)
	}
	if req.RefreshToken != "rt-ide" {
		t.Fatalf("refreshToken = %q", req.RefreshToken)
	}
	if req.TokenEndpoint == "" || req.ClientID != "azure-client-123" {
		t.Fatalf("missing external_idp material: endpoint=%q clientId=%q", req.TokenEndpoint, req.ClientID)
	}
	if !strings.Contains(req.Scopes, "offline_access") {
		t.Fatalf("scopes = %q", req.Scopes)
	}
}

// TestReadIdeCacheCredentialMissingFile returns an actionable error, not a panic.
func TestReadIdeCacheCredentialMissingFile(t *testing.T) {
	_, err := readIdeCacheCredential(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if _, ok := err.(*importValidationError); !ok {
		t.Fatalf("expected *importValidationError, got %T", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %q, want 'not found'", err.Error())
	}
}

// TestReadIdeCacheCredentialNoRefreshToken rejects a cache lacking refresh material.
func TestReadIdeCacheCredentialNoRefreshToken(t *testing.T) {
	path := writeTempCache(t, `{"accessToken":"at","authMethod":"social"}`)
	_, err := readIdeCacheCredential(path)
	if err == nil {
		t.Fatal("expected error when refreshToken absent")
	}
	if !strings.Contains(err.Error(), "refreshToken") {
		t.Fatalf("error = %q, want 'refreshToken'", err.Error())
	}
}

// TestReadIdeCacheCredentialExternalIdpMissingEndpoint rejects an external_idp
// cache that lacks the tokenEndpoint/clientId needed to refresh.
func TestReadIdeCacheCredentialExternalIdpMissingEndpoint(t *testing.T) {
	path := writeTempCache(t, `{"accessToken":"at","refreshToken":"rt","authMethod":"external_idp"}`)
	_, err := readIdeCacheCredential(path)
	if err == nil {
		t.Fatal("expected error for external_idp without tokenEndpoint/clientId")
	}
	if !strings.Contains(err.Error(), "tokenEndpoint") {
		t.Fatalf("error = %q, want 'tokenEndpoint'", err.Error())
	}
}

// TestIdeCachePathPrecedence verifies env > default resolution for
// ideCachePath's non-request explicit argument. It still exercises an
// explicit arg winning over env/default — ideCachePath itself is a generic
// "explicit server-side arg > env > default" resolver and stays correct for
// any future non-request caller — but see
// TestApiImportIdeCacheIgnoresRequestBodyPath below for the actual security
// fix (FIX4): apiImportIdeCache, the ONLY current caller, must NEVER pass the
// HTTP request body's "path" field as that explicit arg, because the body is
// REQUEST-controlled (this route accepts a cookie with no CSRF token, so it
// is reachable via a cross-site POST) rather than operator-controlled. This
// test previously PINNED the request-controlled behavior by asserting the
// explicit arg won; that assertion is kept only for ideCachePath's generic
// contract, decoupled from any HTTP body.
func TestIdeCachePathPrecedence(t *testing.T) {
	if got := ideCachePath("/explicit/path.json"); got != "/explicit/path.json" {
		t.Fatalf("explicit arg should win, got %q", got)
	}
	t.Setenv("KIRO_IDE_CACHE", "/env/path.json")
	if got := ideCachePath(""); got != "/env/path.json" {
		t.Fatalf("env should win when no explicit arg, got %q", got)
	}
	t.Setenv("KIRO_IDE_CACHE", "")
	got := ideCachePath("")
	if !strings.HasSuffix(got, defaultIdeCacheRelPath) {
		t.Fatalf("default path should end with %q, got %q", defaultIdeCacheRelPath, got)
	}
}

// TestApiImportIdeCacheIgnoresRequestBodyPath is FIX4's regression test: the
// HTTP handler must resolve the cache file from KIRO_IDE_CACHE/the default
// ONLY, never from the request body's "path" field. Before the fix, a
// same-origin cookie-authenticated but CSRF-unprotected POST with
// {"path":"/etc/shadow"} or {"path":"/dev/zero"} could turn this route into a
// file-existence oracle or an uncapped-read DoS. This test proves it
// end-to-end: it points KIRO_IDE_CACHE at one credential and the request body
// at a DIFFERENT one, then asserts the refresh request that actually reaches
// the (fake) IdP carries the ENV file's refresh token, never the body file's.
func TestApiImportIdeCacheIgnoresRequestBodyPath(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()
	bypassExternalIdpValidator(t)

	var gotRefreshToken string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotRefreshToken = r.Form.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-env","refresh_token":"rt-env-rotated","expires_in":3600}`)
	}))
	defer idp.Close()

	// The env-configured cache is the ONLY credential that may legitimately be
	// read.
	envPath := writeTempCache(t, fmt.Sprintf(`{
		"accessToken": "at-env",
		"refreshToken": "rt-env-cache",
		"authMethod": "external_idp",
		"clientId": "azure-client",
		"tokenEndpoint": %q,
		"issuerUrl": "https://login.microsoftonline.com/tenant/v2.0",
		"scopes": "offline_access"
	}`, idp.URL))
	t.Setenv("KIRO_IDE_CACHE", envPath)

	// The request body tries to redirect the read to a different,
	// attacker-controlled file. Under the pre-fix behavior
	// (ideCachePath(body.Path)), THIS file's refresh token would be the one
	// sent upstream instead of envPath's.
	attackerPath := writeTempCache(t, `{"accessToken":"at-attacker","refreshToken":"rt-attacker-should-never-be-used"}`)

	restoreRestClient := installNoNetworkKiroRestClientForTest()
	h := &Handler{pool: accountpool.GetPool()}
	cleanupHandlerBeforeRestore(t, h, restoreRestClient)
	reqBody, err := json.Marshal(map[string]string{"path": attackerPath})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest("POST", "/auth/import-ide-cache", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.apiImportIdeCache(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if gotRefreshToken != "rt-env-cache" {
		t.Fatalf("refresh_token sent upstream = %q, want rt-env-cache — "+
			"the request body's \"path\" must be ignored, not used to redirect the read", gotRefreshToken)
	}
}
