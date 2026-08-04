package proxy

import (
	"os"
	"strings"
	"testing"
)

// TestNormalizeRawCredentialSnakeCaseExternalIdp verifies the helper's native
// snake_case external_idp JSON (CLIProxyAPI_*.json) maps onto the canonical
// request with every external-IdP field carried through. Provider is left RAW
// (empty here, since the fixture supplies none) rather than defaulted to
// "AzureAD" at decode time — see normalizeRawCredential's doc comment: a
// decode-time Provider default would contaminate h.importCredential's
// Microsoft-vs-generic classifier (its explicitMicrosoft check reads
// req.Provider), misclassifying every generic external_idp import as
// Microsoft. h.importCredential itself applies the same "AzureAD" default,
// but only AFTER classification has already run.
func TestNormalizeRawCredentialSnakeCaseExternalIdp(t *testing.T) {
	body := []byte(`{
		"access_token": "at-1",
		"auth_method": "external_idp",
		"client_id": "azure-client",
		"refresh_token": "rt-1",
		"region": "us-east-1",
		"profile_arn": "arn:aws:codewhisperer:eu-central-1:123:profile/X",
		"issuer_url": "https://login.microsoftonline.com/tid/v2.0",
		"token_endpoint": "https://login.microsoftonline.com/tid/oauth2/v2.0/token",
		"scopes": "api://azure-client/codewhisperer:conversations offline_access",
		"type": "kiro"
	}`)
	req, err := decodeImportRequest(body)
	if err != nil {
		t.Fatalf("decodeImportRequest: %v", err)
	}
	if req.AuthMethod != "external_idp" {
		t.Fatalf("AuthMethod = %q, want external_idp", req.AuthMethod)
	}
	if req.ClientID != "azure-client" {
		t.Fatalf("ClientID = %q", req.ClientID)
	}
	if req.TokenEndpoint == "" || req.IssuerURL == "" || req.Scopes == "" {
		t.Fatalf("external_idp fields dropped: tokenEndpoint=%q issuerUrl=%q scopes=%q",
			req.TokenEndpoint, req.IssuerURL, req.Scopes)
	}
	if req.ProfileARN != "arn:aws:codewhisperer:eu-central-1:123:profile/X" {
		t.Fatalf("ProfileARN = %q", req.ProfileARN)
	}
	if req.Provider != "" {
		t.Fatalf("Provider = %q, want empty (defaulting happens post-classification in h.importCredential)", req.Provider)
	}
	if req.AccessToken != "at-1" || req.RefreshToken != "rt-1" {
		t.Fatalf("tokens not mapped: access=%q refresh=%q", req.AccessToken, req.RefreshToken)
	}
}

// TestNormalizeRawCredentialCamelCaseStillWorks guards the existing API/UI
// camelCase shape so the upgrade is backward compatible.
func TestNormalizeRawCredentialCamelCaseStillWorks(t *testing.T) {
	body := []byte(`{
		"refreshToken": "rt-2",
		"clientId": "c",
		"clientSecret": "s",
		"authMethod": "idc",
		"region": "us-east-1"
	}`)
	req, err := decodeImportRequest(body)
	if err != nil {
		t.Fatalf("decodeImportRequest: %v", err)
	}
	if req.AuthMethod != "idc" {
		t.Fatalf("AuthMethod = %q, want idc", req.AuthMethod)
	}
	if req.ClientID != "c" || req.ClientSecret != "s" {
		t.Fatalf("client material not mapped: id=%q secret=%q", req.ClientID, req.ClientSecret)
	}
	if req.Provider != "" {
		t.Fatalf("Provider = %q, want empty (no explicit provider supplied)", req.Provider)
	}
}

// TestNormalizeRawCredentialPreservesExplicitProvider verifies an explicitly
// supplied provider survives decode unchanged (it is never overwritten, only
// defaulted when absent).
func TestNormalizeRawCredentialPreservesExplicitProvider(t *testing.T) {
	body := []byte(`{"refreshToken":"rt","clientId":"c","authMethod":"external_idp","provider":"Okta","tokenEndpoint":"https://okta.example.com/token"}`)
	req, err := decodeImportRequest(body)
	if err != nil {
		t.Fatalf("decodeImportRequest: %v", err)
	}
	if req.Provider != "Okta" {
		t.Fatalf("Provider = %q, want Okta (explicit value preserved)", req.Provider)
	}
}

// TestNormalizeAuthMethodInference exercises the inference fallback for empty or
// unknown auth methods. normalizeAuthMethod is a standalone bucketing utility
// (used for the region-default decision only, never for Provider — see
// normalizeRawCredential's doc comment) and is tested independently here.
func TestNormalizeAuthMethodInference(t *testing.T) {
	cases := []struct {
		name                                                                        string
		raw, tokenEndpoint, clientID, secret, kiroApiKey, accessToken, refreshToken string
		want                                                                        string
	}{
		{"external via tokenEndpoint+clientId", "", "https://x/token", "cid", "", "", "", "", "external_idp"},
		{"idc via clientId+secret", "", "", "cid", "sec", "", "", "", "idc"},
		{"social bare", "", "", "", "", "", "", "", "social"},
		{"explicit azure alias", "azuread", "", "", "", "", "", "", "external_idp"},
		{"explicit m365 alias", "m365", "", "", "", "", "", "", "external_idp"},
		{"explicit github", "github", "", "", "", "", "", "", "social"},
		{"explicit builderid", "builderid", "", "", "", "", "", "", "idc"},
		// Regression coverage for the api_key detection FIX1 adds: neither of
		// these carries an explicit auth_method, so before the fix both fell
		// through to the "social" default — which forced the OAuth us-east-1
		// region default onto an api_key import (normalizeRawCredential) and, via
		// that, collapsed resolveApiKeyRegion's probe to a single wrong region
		// (see TestApiImportCredentialsBareApiKeyFieldNoAuthMethodDiscoversRegion
		// and TestApiImportCredentialsBareAccessTokenApiKeyNoAuthMethodDiscoversRegion
		// below for the end-to-end proof).
		{"kiro_api_key field present, no auth_method", "", "", "", "", "ksk_abc123", "", "", "api_key"},
		{"accessToken looks like a key, no refreshToken, no auth_method", "", "", "", "", "", "ksk_abc123", "", "api_key"},
		// A refreshToken alongside a ksk_-shaped accessToken is NOT an api_key
		// import (mirrors isAPIKeyImport's own refreshToken=="" guard in
		// handler.go) — it falls through to the ordinary inference below.
		{"ksk_-shaped accessToken WITH a refreshToken is not api_key", "", "", "", "", "", "ksk_abc123", "rt-1", "social"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeAuthMethod(tc.raw, tc.tokenEndpoint, tc.clientID, tc.secret, tc.kiroApiKey, tc.accessToken, tc.refreshToken)
			if got != tc.want {
				t.Fatalf("normalizeAuthMethod(%q,%q,%q,%q,%q,%q,%q) = %q, want %q",
					tc.raw, tc.tokenEndpoint, tc.clientID, tc.secret, tc.kiroApiKey, tc.accessToken, tc.refreshToken, got, tc.want)
			}
		})
	}
}

// TestNormalizeRawCredentialApiKeyFieldRegionNotDefaulted is the direct
// regression test for FIX1's decode-level bug: a blob carrying a kiro_api_key
// field but no explicit auth_method must NOT get the OAuth us-east-1 region
// default, because that default silently overrides an embedded "key|region"
// suffix once it reaches h.importCredential's SplitKiroAPIKeyAndRegion (the
// "if req.Region == \"\"" guard at handler.go around the api_key branch only
// applies embeddedRegion when Region arrived empty).
func TestNormalizeRawCredentialApiKeyFieldRegionNotDefaulted(t *testing.T) {
	req, err := decodeImportRequest([]byte(`{"kiroApiKey":"ksk_xxx|eu-central-1"}`))
	if err != nil {
		t.Fatalf("decodeImportRequest: %v", err)
	}
	if req.Region != "" {
		t.Fatalf("Region = %q, want empty so SplitKiroAPIKeyAndRegion's embedded eu-central-1 is not discarded", req.Region)
	}
	if req.AuthMethod != "" {
		t.Fatalf("AuthMethod = %q, want raw/empty (bucketing is internal to normalizeAuthMethod)", req.AuthMethod)
	}
}

// TestNormalizeRawCredentialBareAccessTokenApiKeyRegionNotDefaulted is the same
// regression as above for the OTHER detection form: no kiroApiKey field, just a
// refreshToken-less accessToken that looks like a ksk_ key.
func TestNormalizeRawCredentialBareAccessTokenApiKeyRegionNotDefaulted(t *testing.T) {
	req, err := decodeImportRequest([]byte(`{"accessToken":"ksk_xxx|eu-central-1"}`))
	if err != nil {
		t.Fatalf("decodeImportRequest: %v", err)
	}
	if req.Region != "" {
		t.Fatalf("Region = %q, want empty so SplitKiroAPIKeyAndRegion's embedded eu-central-1 is not discarded", req.Region)
	}
}

// TestNormalizeCliJsonArray verifies a JSON array of helper objects parses into
// one request per element.
func TestNormalizeCliJsonArray(t *testing.T) {
	raw := []byte(`[
		{"refresh_token":"r1","client_id":"c1","token_endpoint":"https://login.microsoftonline.com/t/token","auth_method":"external_idp"},
		{"refreshToken":"r2","clientId":"c2","clientSecret":"s2","authMethod":"idc"}
	]`)
	reqs, _, err := normalizeCliJson(raw)
	if err != nil {
		t.Fatalf("normalizeCliJson: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(reqs))
	}
	if reqs[0].AuthMethod != "external_idp" || reqs[1].AuthMethod != "idc" {
		t.Fatalf("auth methods = %q,%q", reqs[0].AuthMethod, reqs[1].AuthMethod)
	}
}

// TestNormalizeCliJsonConcatenated verifies several JSON objects in one blob
// (blank-line separated) all parse.
func TestNormalizeCliJsonConcatenated(t *testing.T) {
	raw := []byte(`{"refresh_token":"r1","client_id":"c1","token_endpoint":"https://login.microsoftonline.com/t/token"}

{"refresh_token":"r2","client_id":"c2","token_endpoint":"https://login.microsoftonline.com/t/token"}`)
	reqs, _, err := normalizeCliJson(raw)
	if err != nil {
		t.Fatalf("normalizeCliJson: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(reqs))
	}
}

// TestNormalizeCliJsonFixtureFile parses the committed sample fixture to ensure
// the real helper output shape imports as an external_idp account.
func TestNormalizeCliJsonFixtureFile(t *testing.T) {
	raw, err := os.ReadFile("../testdata/CLIProxyAPI_sample_external_idp.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	reqs, _, err := normalizeCliJson(raw)
	if err != nil {
		t.Fatalf("normalizeCliJson(fixture): %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req.AuthMethod != "external_idp" {
		t.Fatalf("fixture AuthMethod = %q, want external_idp", req.AuthMethod)
	}
	if req.TokenEndpoint == "" || req.IssuerURL == "" || req.ClientID == "" {
		t.Fatalf("fixture external_idp fields incomplete: %+v", req)
	}
}

// TestNormalizeCliJsonFilesWrapperImportsAllFiles is FIX2's regression test:
// {"files":[a,b,c]} must import all three entries, not just the first. Before
// the fix, the wrapper's file loop `return`ed after the FIRST parsable file,
// silently dropping every entry after it with no error and no warning.
func TestNormalizeCliJsonFilesWrapperImportsAllFiles(t *testing.T) {
	raw := []byte(`{"files":[
		"{\"refresh_token\":\"r1\",\"client_id\":\"c1\",\"token_endpoint\":\"https://login.microsoftonline.com/t/token\"}",
		"{\"refresh_token\":\"r2\",\"client_id\":\"c2\",\"token_endpoint\":\"https://login.microsoftonline.com/t/token\"}",
		"{\"refresh_token\":\"r3\",\"client_id\":\"c3\",\"token_endpoint\":\"https://login.microsoftonline.com/t/token\"}"
	]}`)
	reqs, _, err := normalizeCliJson(raw)
	if err != nil {
		t.Fatalf("normalizeCliJson: %v", err)
	}
	if len(reqs) != 3 {
		t.Fatalf("got %d requests, want 3 (all files must import, not just the first)", len(reqs))
	}
	got := map[string]bool{}
	for _, r := range reqs {
		got[r.RefreshToken] = true
	}
	for _, want := range []string{"r1", "r2", "r3"} {
		if !got[want] {
			t.Fatalf("missing request with refreshToken %q; got %+v", want, reqs)
		}
	}
}

// TestDecodeConcatenatedObjectsSurfacesTruncationWarning is FIX3's unit-level
// regression test: a blob whose second object is malformed must still return
// the first (valid) object, but with a warning naming the break point —
// previously it silently returned (out, nil), indistinguishable from a fully
// clean parse.
func TestDecodeConcatenatedObjectsSurfacesTruncationWarning(t *testing.T) {
	text := `{"refresh_token":"r1"}

{not valid json`
	out, warning, err := decodeConcatenatedObjects(text)
	if err != nil {
		t.Fatalf("decodeConcatenatedObjects: %v", err)
	}
	if len(out) != 1 || out[0].RefreshTokenSnake != "r1" {
		t.Fatalf("expected the one valid object to still parse, got %+v", out)
	}
	if warning == "" {
		t.Fatal("expected a non-empty truncation warning, got none")
	}
	if !isTruncationWarning(warning) {
		t.Fatalf("warning %q does not carry the truncationWarningPrefix", warning)
	}
	if !strings.Contains(warning, "object 2") {
		t.Fatalf("warning = %q, want it to name object 2 as the break point", warning)
	}
}

// TestNormalizeCliJsonEmpty rejects an empty document with a clear error.
func TestNormalizeCliJsonEmpty(t *testing.T) {
	if _, _, err := normalizeCliJson([]byte("   ")); err == nil {
		t.Fatalf("expected error for empty document")
	}
}
