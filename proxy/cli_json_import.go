package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// importCredentialRequest is the single normalized shape every credential-import
// path funnels into: the raw HTTP body of apiImportCredentials, the
// /auth/import-cli-json endpoint, the Kiro IDE cache import, and the directory
// watcher. Funnelling all of them through h.importCredential (handler.go) means
// every path gets IDENTICAL Microsoft-vs-generic classification, SSRF
// allow-listing, and trust-on-import behavior — there is exactly one classifier
// in this codebase, never a second one that could silently drift from it.
//
// AuthMethod is left as the RAW, caller-supplied alias (e.g. "AzureAD",
// "external_idp", "entra", "" ...) — NOT pre-bucketed here. h.importCredential's
// own normalizeImportAuthMethod call, plus its "explicitMicrosoft" alias check,
// both depend on seeing that raw value: the alias check runs BEFORE
// normalizeImportAuthMethod's canonical bucket overwrites req.AuthMethod, so
// bucketing it here in advance would silently discard the "caller explicitly
// said Microsoft" signal for any request that carries no accessToken yet and
// whose tokenEndpoint host is not literally on the Microsoft login host
// allow-list (e.g. a still-being-derived tenant) — exactly the field-presence
// heuristic this classifier was rewritten to stop relying on.
type importCredentialRequest struct {
	ID, Email, UserID, Nickname, ProfileARN string
	AccessToken, RefreshToken, KiroApiKey   string
	ClientID, ClientSecret                  string
	AuthMethod, Provider, Region            string
	TokenEndpoint, IssuerURL, Scopes        string
}

// rawCredential captures every key the import paths understand, accepting both
// the helper's native snake_case (CLIProxyAPI_*.json, produced by
// kiro-login-helper.py) and the camelCase the web UI / existing API already
// send. Both casings are decoded; normalizeRawCredential resolves which wins
// (snake_case is the helper's native format, so it is preferred when both are
// present and non-empty).
type rawCredential struct {
	// snake_case (kiro-login-helper.py build_auth_json output)
	AccessTokenSnake   string `json:"access_token"`
	RefreshTokenSnake  string `json:"refresh_token"`
	ClientIDSnake      string `json:"client_id"`
	ClientSecretSnake  string `json:"client_secret"`
	AuthMethodSnake    string `json:"auth_method"`
	TokenEndpointSnake string `json:"token_endpoint"`
	IssuerURLSnake     string `json:"issuer_url"`
	ProfileArnSnake    string `json:"profile_arn"`

	// camelCase (existing API / web UI)
	AccessTokenCamel   string `json:"accessToken"`
	RefreshTokenCamel  string `json:"refreshToken"`
	ClientIDCamel      string `json:"clientId"`
	ClientSecretCamel  string `json:"clientSecret"`
	AuthMethodCamel    string `json:"authMethod"`
	TokenEndpointCamel string `json:"tokenEndpoint"`
	IssuerURLCamel     string `json:"issuerUrl"`
	ProfileArnCamel    string `json:"profileArn"`

	// Shared / single-casing keys.
	Scopes   string `json:"scopes"`
	Region   string `json:"region"`
	Provider string `json:"provider"`
	IDP      string `json:"idp"`
	Type     string `json:"type"`

	// Account identity. userId (Kiro Account Manager exports, account level)
	// embeds the Azure tenant used to derive missing external_idp endpoints; id
	// preserves an account id across a re-import.
	UserIDSnake string `json:"user_id"`
	UserIDCamel string `json:"userId"`
	ID          string `json:"id"`

	// Kiro API-key credential (ksk_...), both casings.
	KiroApiKeySnake string `json:"kiro_api_key"`
	KiroApiKeyCamel string `json:"kiroApiKey"`

	// Label material — email may arrive as a token claim alias.
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	UPN               string `json:"upn"`
	Nickname          string `json:"nickname"`
}

// firstNonBlank returns the first argument that is non-empty after trimming.
func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// normalizeAuthMethod maps the many ways a caller can spell an auth method onto
// the canonical values the rest of the system understands. It exists as a
// standalone, independently-testable bucketing utility for callers that want a
// coarse classification (currently: the isAPIKeyImport region-default decision
// below). It must NEVER be used to pre-fill importCredentialRequest.Provider —
// see the CAUTION below and importCredentialRequest's doc comment for why.
//
// When the method is absent or unrecognized it is inferred from which
// credential material is present (Kiro API key > external IdP material > IdC
// client secret > social).
func normalizeAuthMethod(raw, tokenEndpoint, clientID, clientSecret, kiroApiKey, accessToken, refreshToken string) string {
	// Kiro API-key detection MUST mirror h.importCredential's isAPIKeyImport
	// check (handler.go) exactly and run first, ahead of every other signal:
	// an explicit kiro_api_key/kiroApiKey field, or a refreshToken-less blob
	// whose accessToken is ksk_-shaped, is unambiguously an API-key credential
	// even when the caller also supplied a stale/absent auth_method. Before this
	// check existed, a blob like {"kiro_api_key":"ksk_..."} or a bare
	// {"accessToken":"ksk_..."} with no refreshToken fell through to the
	// "social" default below, which wrongly forced the OAuth us-east-1 region
	// default onto an api_key import and collapsed resolveApiKeyRegion's probe
	// to that single region (see normalizeRawCredential's region-default
	// comment) — the exact regression this function's doc comment always
	// claimed was handled ("Kiro API key > ...") but never actually was.
	if strings.TrimSpace(kiroApiKey) != "" ||
		(strings.TrimSpace(refreshToken) == "" && looksLikeKiroAPIKey(accessToken)) {
		return "api_key"
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "external_idp", "azure", "azuread", "entra", "entraid", "m365", "microsoft365", "microsoft":
		return "external_idp"
	case "idc", "builderid", "enterprise":
		return "idc"
	case "social", "google", "github":
		return "social"
	case "api_key", "apikey", "kiro_api_key":
		// Kiro API-key accounts carry no refresh token; h.importCredential handles
		// them ahead of the OAuth path (they never reach the classifier below).
		return "api_key"
	}
	// Inference for empty/unknown values.
	if strings.TrimSpace(tokenEndpoint) != "" && strings.TrimSpace(clientID) != "" {
		return "external_idp"
	}
	if strings.TrimSpace(clientID) != "" && strings.TrimSpace(clientSecret) != "" {
		return "idc"
	}
	return "social"
}

// normalizeRawCredential resolves a decoded rawCredential into the canonical
// importCredentialRequest, applying casing precedence and the us-east-1 region
// default (OAuth methods only). AuthMethod AND Provider are both carried
// through RAW/trimmed — NOT defaulted here.
//
// CAUTION (this is the bug this whole classifier was rewritten to avoid, so it
// is spelled out): h.importCredential's explicitMicrosoft check reads
// strings.ToLower(req.Provider) as one of its OR'd signals ("azuread" among
// them). Pre-filling Provider to "AzureAD" here — before that classifier runs —
// would make EVERY external_idp import with no explicit Microsoft signal
// (any authMethod:"external_idp" blob, Microsoft or not) evaluate
// explicitMicrosoft=true and get routed into the strict tenant-bound refresh,
// which then rejects a non-UUID client_id with "Microsoft client_id is not a
// valid UUID" — misclassifying every generic external_idp credential as
// Microsoft. h.importCredential itself applies the Provider default, but only
// AFTER classification decides which branch runs (unconditionally for a
// confirmed-Microsoft account, only-if-still-empty for a confirmed-generic
// one), so defaulting there cannot contaminate the decision that produced it.
func normalizeRawCredential(rc rawCredential) importCredentialRequest {
	accessToken := firstNonBlank(rc.AccessTokenSnake, rc.AccessTokenCamel)
	refreshToken := firstNonBlank(rc.RefreshTokenSnake, rc.RefreshTokenCamel)
	clientID := firstNonBlank(rc.ClientIDSnake, rc.ClientIDCamel)
	clientSecret := firstNonBlank(rc.ClientSecretSnake, rc.ClientSecretCamel)
	tokenEndpoint := firstNonBlank(rc.TokenEndpointSnake, rc.TokenEndpointCamel)
	issuerURL := firstNonBlank(rc.IssuerURLSnake, rc.IssuerURLCamel)
	profileArn := firstNonBlank(rc.ProfileArnSnake, rc.ProfileArnCamel)
	authMethodRaw := strings.TrimSpace(firstNonBlank(rc.AuthMethodSnake, rc.AuthMethodCamel))
	provider := firstNonBlank(rc.Provider, rc.IDP)
	kiroApiKey := firstNonBlank(rc.KiroApiKeySnake, rc.KiroApiKeyCamel)

	// bucket is used ONLY for the region-default decision immediately below —
	// NOT for Provider. The actual Microsoft-vs-generic classification happens
	// exactly once, inside h.importCredential, off the RAW authMethodRaw
	// preserved in the returned struct's AuthMethod field. It is passed the same
	// kiroApiKey/accessToken/refreshToken material h.importCredential's own
	// isAPIKeyImport check uses, so this bucket call can never disagree with
	// that check about whether a blob is an api_key import.
	bucket := normalizeAuthMethod(authMethodRaw, tokenEndpoint, clientID, clientSecret, kiroApiKey, accessToken, refreshToken)

	// Region default is deliberately NOT applied to api_key credentials: those are
	// never refreshed, so apiImportCredentials probes the key to discover its real
	// region, and a us-east-1 placeholder here would be validated as the answer and
	// restore an EU key as a permanently-403ing pool slot. OAuth methods still get
	// the default (h.importCredential applies the same fallback for the legacy
	// camelCase-only payload, so this is just consistent, not load-bearing).
	region := firstNonBlank(rc.Region)
	if region == "" && bucket != "api_key" {
		region = "us-east-1"
	}

	return importCredentialRequest{
		AccessToken:   accessToken,
		RefreshToken:  refreshToken,
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		AuthMethod:    authMethodRaw,
		Provider:      provider,
		Region:        region,
		TokenEndpoint: tokenEndpoint,
		IssuerURL:     issuerURL,
		Scopes:        strings.TrimSpace(rc.Scopes),
		ProfileARN:    profileArn,
		UserID:        firstNonBlank(rc.UserIDSnake, rc.UserIDCamel),
		ID:            strings.TrimSpace(rc.ID),
		KiroApiKey:    kiroApiKey,
		Email:         firstNonBlank(rc.Email, rc.PreferredUsername, rc.UPN),
		Nickname:      strings.TrimSpace(rc.Nickname),
	}
}

// decodeImportRequest reads a single JSON object (snake_case or camelCase) into a
// normalized importCredentialRequest. Used by apiImportCredentials so the existing
// API keeps working while also accepting the helper's native format.
func decodeImportRequest(body []byte) (importCredentialRequest, error) {
	var rc rawCredential
	if err := json.Unmarshal(body, &rc); err != nil {
		return importCredentialRequest{}, fmt.Errorf("invalid JSON: %w", err)
	}
	return normalizeRawCredential(rc), nil
}

// normalizeCliJson parses a CLIProxyAPI helper document into one or more
// normalized requests. It accepts:
//   - a single JSON object,
//   - a JSON array of objects,
//   - a wrapper { "accounts": [...] } or { "files": ["<json string>", ...] },
//   - raw text containing several JSON objects separated by blank lines.
//
// type:"kiro" is validated as a soft signal only (a warning, never a hard reject)
// so a slightly-different export still imports.
func normalizeCliJson(raw []byte) ([]importCredentialRequest, []string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil, fmt.Errorf("empty document")
	}

	var warnings []string
	collect := func(rcs []rawCredential) []importCredentialRequest {
		out := make([]importCredentialRequest, 0, len(rcs))
		for _, rc := range rcs {
			if rc.Type != "" && !strings.EqualFold(strings.TrimSpace(rc.Type), "kiro") {
				warnings = append(warnings, fmt.Sprintf("unexpected type %q (expected \"kiro\")", rc.Type))
			}
			out = append(out, normalizeRawCredential(rc))
		}
		return out
	}

	// Fast path: a single JSON value (object or array).
	switch trimmed[0] {
	case '[':
		var arr []rawCredential
		if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
			return collect(arr), warnings, nil
		}
	case '{':
		// Try a { "files": [...] } / { "accounts": [...] } wrapper first.
		var wrapper struct {
			Files    []json.RawMessage `json:"files"`
			Accounts []rawCredential   `json:"accounts"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapper); err == nil && (len(wrapper.Files) > 0 || len(wrapper.Accounts) > 0) {
			// Accumulate every wrapper "accounts" entry AND every parsed "files"
			// entry across the WHOLE loop, returning only after it finishes. The
			// previous code `return`ed from inside this loop after the FIRST
			// parsable file, so for {"files":[a,b,c]} only `a` ever imported — `b`
			// and `c` were silently dropped with no error and no warning.
			out := collect(wrapper.Accounts)
			for _, f := range wrapper.Files {
				inner, innerWarnings, ferr := normalizeCliJsonInner(f)
				warnings = append(warnings, innerWarnings...)
				if ferr != nil {
					warnings = append(warnings, ferr.Error())
					continue
				}
				out = append(out, inner...)
			}
			return out, warnings, nil
		}
		// Plain single object.
		var rc rawCredential
		if err := json.Unmarshal([]byte(trimmed), &rc); err == nil {
			return collect([]rawCredential{rc}), warnings, nil
		}
	}

	// Slow path: multiple JSON objects in one blob (concatenated or
	// blank-line-separated). Use a streaming decoder over the whole text.
	rcs, truncWarning, decErr := decodeConcatenatedObjects(trimmed)
	if decErr != nil {
		return nil, warnings, decErr
	}
	if truncWarning != "" {
		warnings = append(warnings, truncWarning)
	}
	if len(rcs) == 0 {
		return nil, warnings, fmt.Errorf("no credential objects found")
	}
	return collect(rcs), warnings, nil
}

// normalizeCliJsonInner is normalizeCliJson for an embedded file string; it returns
// the parsed requests so the wrapper path can flatten them.
func normalizeCliJsonInner(raw []byte) ([]importCredentialRequest, []string, error) {
	// A "files" entry may itself be a JSON string (escaped) or a nested object.
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return normalizeCliJson([]byte(asString))
	}
	return normalizeCliJson(raw)
}

// truncationWarningPrefix marks a warning produced when decodeConcatenatedObjects
// had to stop partway through a blob because an object failed to parse. Callers
// that decide whether a source is "fully handled" (the import watcher's
// processed/ vs failed/ routing, see import_watcher.go) key off this prefix so a
// partially-consumed blob is never treated as done just because SOME of its
// objects happened to import successfully before the break point.
const truncationWarningPrefix = "partial parse: "

// isTruncationWarning reports whether w was produced by a stopped-early
// decodeConcatenatedObjects parse (see truncationWarningPrefix).
func isTruncationWarning(w string) bool {
	return strings.HasPrefix(w, truncationWarningPrefix)
}

// decodeConcatenatedObjects streams successive JSON objects out of a single blob,
// tolerating whitespace/newlines between them. It returns the raw decoded
// credentials so the caller's collect() applies type-warning and normalization
// uniformly across every parse path, plus a warning (isTruncationWarning) when it
// had to stop early because an object failed to parse — silently discarding that
// signal previously let a partially-imported drop-folder file get moved to
// processed/ as if every credential in it had been handled.
func decodeConcatenatedObjects(text string) ([]rawCredential, string, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	var out []rawCredential
	for {
		var rc rawCredential
		err := dec.Decode(&rc)
		if err != nil {
			if err == io.EOF {
				break
			}
			// Stop at the first malformed object; return what parsed so far (if
			// any) PLUS a warning naming the 1-indexed object position that broke
			// and why, so the caller can tell "fully imported" apart from
			// "partially imported, the rest was silently dropped".
			if len(out) > 0 {
				warning := fmt.Sprintf("%sobject %d: %v (parsing stopped here; anything after it in this document was not imported)",
					truncationWarningPrefix, len(out)+1, err)
				return out, warning, nil
			}
			return nil, "", fmt.Errorf("could not parse credential JSON: %w", err)
		}
		out = append(out, rc)
	}
	return out, "", nil
}
