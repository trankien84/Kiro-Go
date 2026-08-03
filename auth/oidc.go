package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// oidcTokenURL 构造 idc/builderId 刷新 endpoint。测试可替换以拦截网络调用。
var oidcTokenURL = func(region string) string {
	return fmt.Sprintf("https://oidc.%s.amazonaws.com/token", region)
}

// socialTokenURL 构造 social 刷新 endpoint。测试可替换以拦截网络调用。
var socialTokenURL = func() string {
	return "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
}

// refreshSkewSeconds mirrors pool.tokenRefreshSkewSeconds: a token within this
// many seconds of expiry is treated as expiring and refreshed proactively.
// Kept local to auth to avoid a pool→auth import cycle.
const refreshSkewSeconds int64 = 120

var (
	// refreshMu guards refreshLocks creation only; each per-account lock is then
	// held for the duration of one refresh.
	refreshMu sync.Mutex
	// refreshLocks maps accountID → that account's refresh mutex. One lock per
	// account serializes only that account's background refreshes, reducing the
	// global contention the handler-level tokenRefreshMu imposed. tokenRefreshMu
	// still guards the request-path refresh (handler.go:2148) and remains part of
	// the documented lock order (account.go:250) — intentionally not removed.
	// Account IDs are stable UUIDs; the map is bounded by the total number of
	// accounts ever seen, which is negligible for this deployment.
	refreshLocks = map[string]*sync.Mutex{}
)

// refreshLockFor returns the per-account refresh mutex, creating it on first use.
func refreshLockFor(id string) *sync.Mutex {
	refreshMu.Lock()
	lock, ok := refreshLocks[id]
	if !ok {
		lock = &sync.Mutex{}
		refreshLocks[id] = lock
	}
	refreshMu.Unlock()
	return lock
}

// refreshTokenDirect resolves the proxy-aware HTTP client and dispatches to the
// provider-specific refresh. It performs no locking or persistence — used both
// for id-less accounts (login validation, which has no shared state to
// coordinate against) and as the inner step of the locked RefreshToken.
func refreshTokenDirect(account *config.Account) (string, string, int64, string, error) {
	// Kiro API-key credentials (ksk_...) ARE the bearer token: there is no
	// refresh grant behind them, so a refresh attempt would only produce a
	// confusing upstream error. Reject explicitly (upstream behaviour).
	if config.IsAPIKeyAccount(account) {
		return "", "", 0, "", fmt.Errorf("API Key credentials do not support token refresh")
	}
	// Resolve per-account proxy: account.ProxyURL > global config
	proxyURL := account.ProxyURL
	if proxyURL == "" {
		proxyURL = config.GetProxyURL()
	}
	client := GetAuthClientForProxy(proxyURL)

	// External-IdP (enterprise SSO) accounts refresh against the IdP's own token
	// endpoint, not the AWS SSO OIDC endpoint. EqualFold/TrimSpace tolerates the
	// casing/whitespace variance that pasted-credential imports produce.
	//
	// MicrosoftSSOAuthMethod is literally "external_idp", which our fork also
	// persists for GENERIC (non-Microsoft) enterprise IdPs. Dispatching on that
	// string alone would push every generic account into the Microsoft refresh,
	// which unconditionally uuid.Parse()es the client_id and tenant-matches the
	// issuer — so a perfectly valid Okta/Ping/Keycloak credential could never
	// refresh, failing locally with "Microsoft client_id is not a valid UUID"
	// before the IdP was ever contacted. Provider is the persisted classification
	// written at import/login time, so it — not the shared AuthMethod string — is
	// what selects the strict path.
	if strings.EqualFold(strings.TrimSpace(account.AuthMethod), MicrosoftSSOAuthMethod) {
		if isMicrosoftExternalIdp(account.Provider) {
			// IssuerURL is passed so the refresh binds the token endpoint to the
			// same tenant as the issuer before the refresh token leaves the process.
			accessToken, refreshToken, expiresAt, profileArn, err := refreshExternalIdpToken(
				account.RefreshToken,
				account.ClientID,
				account.TokenEndpoint,
				account.IssuerURL,
				account.Scopes,
				client,
			)
			if err != nil && IsMicrosoftValidationShapeError(err) {
				// The interactive enterprise-SSO login (kiro_sso.go's exchange())
				// hardcodes Provider="AzureAD" for every external_idp login,
				// regardless of which IdP the portal descriptor actually named —
				// so isMicrosoftExternalIdp above can be wrong for a genuinely
				// generic (Okta/Ping/Keycloak/v1-issuer) account. A LOCAL
				// validation-shape failure (unexpected scope, non-UUID client_id,
				// non-Entra issuer/tenant shape) — never a network call or an
				// actual IdP rejection — is exactly that mis-classification
				// surfacing, so retry once via the generic OAuth2 refresh, which
				// is what this account would have used pre-classification and
				// imposes no Microsoft-specific shape. postGenericExternalIdpToken
				// still re-runs the SSRF host allow-list, so this does not relax
				// where the refresh token may be sent.
				logger.Warnf("[ExternalIdP] account %s classified Microsoft (Provider=%q) but failed strict validation (%v); falling back to generic OAuth2 refresh", account.Email, account.Provider, err)
				return refreshGenericExternalIdpToken(
					account.RefreshToken,
					account.ClientID,
					account.TokenEndpoint,
					account.Scopes,
					client,
				)
			}
			return accessToken, refreshToken, expiresAt, profileArn, err
		}
		return refreshGenericExternalIdpToken(
			account.RefreshToken,
			account.ClientID,
			account.TokenEndpoint,
			account.Scopes,
			client,
		)
	}
	if account.AuthMethod == "social" {
		return refreshSocialToken(account.RefreshToken, client)
	}
	return refreshOIDCToken(account.RefreshToken, account.ClientID, account.ClientSecret, account.Region, client)
}

// RefreshToken refreshes the account's access token. For id-bearing accounts it
// serializes per account with double-checked locking: concurrent callers for one
// account collapse to a single IdP POST (the leader refreshes + persists under
// the lock; followers re-read the now-fresh token from config and skip the
// POST). This replaces the refresh-storm + lost-update race that occurred when
// every concurrent request POSTed independently and raced writing back the
// (rotated) refresh token.
//
// Returns: accessToken, refreshToken, expiresAt, profileArn, error.
func RefreshToken(account *config.Account) (string, string, int64, string, error) {
	return refreshTokenLocked(account, false)
}

// RefreshTokenForce is RefreshToken without the "already fresh → skip the POST"
// short-circuit. It still takes the per-account lock (so a forced refresh cannot
// interleave with an implicit one and lose a rotated refresh token), but it
// always performs the IdP round-trip.
//
// This exists because the skip is a refresh-storm optimization for IMPLICIT
// refreshes, and silently applying it to an explicit "refresh this account now"
// request (admin panel, credential re-validation) would make that request a
// no-op whenever the cached token merely looks fresh. Callers that pass a force
// flag must use this entry point; everything else should use RefreshToken.
func RefreshTokenForce(account *config.Account) (string, string, int64, string, error) {
	return refreshTokenLocked(account, true)
}

func refreshTokenLocked(account *config.Account, force bool) (string, string, int64, string, error) {
	if account == nil {
		return "", "", 0, "", fmt.Errorf("RefreshToken: nil account")
	}

	// Id-less accounts (e.g. the login/add-account validation flow builds a
	// temporary account) have no persisted state to coordinate against, so skip
	// the lock + double-check and refresh directly (original behavior).
	if account.ID == "" {
		return refreshTokenDirect(account)
	}

	lock := refreshLockFor(account.ID)
	lock.Lock()
	defer lock.Unlock()

	// Double-checked locking: a concurrent refresh may have renewed the token
	// while we waited. Re-read the canonical fields either way — a concurrent
	// refresh may have rotated the refresh token, and replaying the stale one
	// would be rejected (Entra refresh tokens are single-use). When not forced and
	// the canonical token is still valid, propagate it and skip the IdP POST; that
	// skip is what collapses a refresh storm into one round-trip.
	if live, ok := config.GetAccountByID(account.ID); ok {
		*account = live
		if !force {
			if now := time.Now().Unix(); live.ExpiresAt > 0 && now < live.ExpiresAt-refreshSkewSeconds {
				return live.AccessToken, live.RefreshToken, live.ExpiresAt, live.ProfileArn, nil
			}
		}
	}

	accessToken, refreshToken, expiresAt, profileArn, err := refreshTokenDirect(account)
	if err != nil {
		return "", "", 0, "", err
	}

	// Persist under the lock so the next caller's double-check sees the fresh
	// token — this is what collapses N concurrent refreshes into one IdP POST.
	// (Existing handler call sites also persist; those writes are now idempotent
	// no-ops and are left untouched per the no-call-site-change constraint.)
	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	if profileArn != "" {
		account.ProfileArn = profileArn
	}
	if perr := config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt); perr != nil {
		logger.Warnf("[RefreshToken] failed to persist refreshed token for %s: %v", account.Email, perr)
	}
	if profileArn != "" {
		if perr := config.UpdateAccountProfileArn(account.ID, profileArn); perr != nil {
			logger.Warnf("[RefreshToken] failed to persist profileArn for %s: %v", account.Email, perr)
		}
	}
	return accessToken, refreshToken, expiresAt, profileArn, nil
}

// microsoftExternalIdpProviders is the set of Provider values that mean "this
// external_idp account is a Microsoft Entra / Azure AD tenant" and therefore
// must use the strict, tenant-bound refresh. Mirrors the provider spellings the
// credential-import path accepts (handler.go) so classification at import and
// dispatch at refresh cannot drift apart.
var microsoftExternalIdpProviders = map[string]bool{
	"azuread": true, "azure": true, "azure-ad": true, "azure_ad": true,
	"entra": true, "entra-id": true, "entra_id": true,
	"microsoft": true, "m365": true, "office365": true,
}

// isMicrosoftExternalIdp reports whether an external_idp account is Microsoft
// classified. An unknown or empty Provider is deliberately treated as GENERIC:
// the generic refresh is a plain OAuth2 refresh_token grant that also works
// against Entra, so mis-classifying Microsoft as generic merely skips extra
// validation, whereas the reverse would hard-fail a valid non-Microsoft account
// that can never satisfy the UUID/tenant checks.
func isMicrosoftExternalIdp(provider string) bool {
	return microsoftExternalIdpProviders[strings.ToLower(strings.TrimSpace(provider))]
}

// refreshGenericExternalIdpToken refreshes a NON-Microsoft enterprise-IdP access
// token: a standard OAuth2 refresh_token grant for a public client (no client
// secret; offline_access in the original scopes is what makes a refresh token
// available). It imposes no Microsoft-specific shape on client_id, issuer, or
// scopes, which is the whole point of the generic path.
//
// The IdP issues no profileArn (that is resolved separately via
// ListAvailableProfiles with the EXTERNAL_IDP token type), so "" is returned for it.
func refreshGenericExternalIdpToken(refreshToken, clientID, tokenEndpoint, scopes string, client *http.Client) (string, string, int64, string, error) {
	if clientID == "" || tokenEndpoint == "" {
		return "", "", 0, "", fmt.Errorf("external IdP refresh requires clientId and tokenEndpoint")
	}
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if scopes != "" {
		form.Set("scope", scopes)
	}
	accessToken, newRefreshToken, expiresIn, err := postGenericExternalIdpToken(client, tokenEndpoint, form)
	if err != nil {
		return "", "", 0, "", err
	}
	// Some IdPs rotate refresh tokens; others omit it on refresh. Keep the
	// existing refresh token when the response does not carry a new one.
	if newRefreshToken == "" {
		newRefreshToken = refreshToken
	}
	return accessToken, newRefreshToken, time.Now().Unix() + int64(expiresIn), "", nil
}

// postGenericExternalIdpToken performs the form-encoded POST and maps the
// snake_case OAuth2 token response onto the standard return shape.
//
// It re-validates the endpoint at the outbound-POST boundary through the
// exported ValidateExternalIdpEndpoint so the refresh token is never sent to a
// non-allow-listed host — even if a persisted TokenEndpoint was set out-of-band
// (backup restore, an external file write, a caller that stored it without
// validating). Going through the exported symbol keeps the test seam
// (SetExternalIdpValidatorForTest) effective for httptest servers.
func postGenericExternalIdpToken(client *http.Client, tokenEndpoint string, form url.Values) (accessToken, refreshToken string, expiresIn int, err error) {
	if strings.TrimSpace(tokenEndpoint) == "" {
		return "", "", 0, fmt.Errorf("external IdP token endpoint is empty")
	}
	if err := ValidateExternalIdpEndpoint(tokenEndpoint); err != nil {
		return "", "", 0, fmt.Errorf("external IdP token endpoint rejected: %w", err)
	}
	req, err := http.NewRequest("POST", tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", 0, fmt.Errorf("failed to build external IdP token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("external IdP token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", 0, fmt.Errorf("failed to read external IdP token response: %w", err)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || out.AccessToken == "" {
		if out.Error != "" {
			return "", "", 0, fmt.Errorf("external IdP token exchange failed (status %d): %s: %s", resp.StatusCode, out.Error, out.ErrorDesc)
		}
		return "", "", 0, fmt.Errorf("external IdP token exchange failed (status %d): %s", resp.StatusCode, string(body))
	}
	return out.AccessToken, out.RefreshToken, out.ExpiresIn, nil
}

// refreshOIDCToken IdC/Builder ID token 刷新
func refreshOIDCToken(refreshToken, clientID, clientSecret, region string, client *http.Client) (string, string, int64, string, error) {
	if clientID == "" || clientSecret == "" {
		return "", "", 0, "", fmt.Errorf("OIDC refresh requires clientId and clientSecret")
	}
	if region == "" {
		region = "us-east-1"
	}

	url := oidcTokenURL(region)

	payload := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", 0, "", fmt.Errorf("refresh failed: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", 0, "", err
	}

	expiresAt := time.Now().Unix() + int64(result.ExpiresIn)
	return result.AccessToken, result.RefreshToken, expiresAt, result.ProfileArn, nil
}

// refreshSocialToken Social (GitHub/Google) token 刷新
func refreshSocialToken(refreshToken string, client *http.Client) (string, string, int64, string, error) {
	url := socialTokenURL()

	payload := map[string]string{
		"refreshToken": refreshToken,
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", 0, "", fmt.Errorf("refresh failed: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", 0, "", err
	}

	expiresAt := time.Now().Unix() + int64(result.ExpiresIn)
	return result.AccessToken, result.RefreshToken, expiresAt, result.ProfileArn, nil
}
