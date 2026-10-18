package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

var ErrAccountLifecycleChanged = errors.New("account lifecycle changed during token refresh")

// AccountCredentialPersistenceError marks an IdP-success/local-publication split.
// Published distinguishes a pre-publication storage failure from a visible C2
// image whose trailing durability barrier failed. Neither case is an OAuth
// authentication failure.
type AccountCredentialPersistenceError struct {
	AccountID string
	Published bool
	Err       error
}

func (e *AccountCredentialPersistenceError) Error() string {
	if e == nil {
		return "persist refreshed credential"
	}
	state := "before publication"
	if e.Published {
		state = "after publication"
	}
	if e.Err == nil {
		return fmt.Sprintf("persist refreshed credential for account %s (%s)", e.AccountID, state)
	}
	return fmt.Sprintf("persist refreshed credential for account %s (%s): %v", e.AccountID, state, e.Err)
}

func (e *AccountCredentialPersistenceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type refreshLockKey struct {
	id         string
	generation uint64
}

type refreshLockEntry struct {
	gate    chan struct{}
	refs    int
	pending *pendingCredentialPublication
}

var (
	refreshMu    sync.Mutex
	refreshLocks = map[refreshLockKey]*refreshLockEntry{}
)

// acquireRefreshLock serializes refresh-token rotation within one exact account
// lifecycle. Reusing a textual account ID after delete/recreate gets a distinct
// lock, so an old lifecycle can finish without blocking or sharing refresh state
// with the replacement. Reference counting keeps the registry bounded by active
// refresh/waiter lifecycles rather than every account ID ever observed.
func acquireRefreshLock(key refreshLockKey) func() {
	_, release := acquireRefreshEntry(key)
	return release
}

// refreshTokenDirect resolves the proxy-aware HTTP client and dispatches to the
// provider-specific refresh. It performs no locking or persistence — used both
// for id-less accounts (login validation, which has no shared state to
// coordinate against) and as the inner step of the locked RefreshToken.
func refreshTokenDirect(account *config.Account) (string, string, int64, string, error) {
	return refreshTokenDirectContext(context.Background(), account)
}

func refreshTokenDirectContext(ctx context.Context, account *config.Account) (string, string, int64, string, error) {
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
	client := bindRefreshAttemptClient(ctx, GetAuthClientForProxy(proxyURL))

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

// RefreshToken refreshes the account's access token using the request-path
// safety skew. Duplicate callers for one lifecycle collapse under the same lock.
func RefreshToken(account *config.Account) (string, string, int64, string, error) {
	return RefreshTokenContext(context.Background(), account)
}

func RefreshTokenContext(ctx context.Context, account *config.Account) (string, string, int64, string, error) {
	return refreshTokenLockedContext(ctx, account, false, time.Duration(refreshSkewSeconds)*time.Second)
}

// RefreshTokenIfExpiringWithin refreshes when the canonical token has less than
// minValidity remaining. Reconciler callers use their deterministic proactive
// lead while request callers keep RefreshToken's normal two-minute safety skew.
// The due check is repeated under the same lifecycle lock, so callers with
// different thresholds still collapse to one IdP exchange.
func RefreshTokenIfExpiringWithin(account *config.Account, minValidity time.Duration) (string, string, int64, string, error) {
	return RefreshTokenIfExpiringWithinContext(context.Background(), account, minValidity)
}

func RefreshTokenIfExpiringWithinContext(ctx context.Context, account *config.Account, minValidity time.Duration) (string, string, int64, string, error) {
	if minValidity < 0 {
		minValidity = 0
	}
	return refreshTokenLockedContext(ctx, account, false, minValidity)
}

// RefreshTokenForce is RefreshToken without the "already fresh → skip the POST"
// short-circuit. It still takes the per-account lock (so a forced refresh cannot
// interleave with an implicit one and lose a rotated refresh token), but it
// normally performs an IdP round-trip. If the lifecycle has a pending C2 from an
// earlier IdP-success/local-publication failure, that C2 is published locally
// first and this invocation never issues a second IdP POST.
//
// This exists because the skip is a refresh-storm optimization for IMPLICIT
// refreshes, and silently applying it to an explicit "refresh this account now"
// request (admin panel, credential re-validation) would make that request a
// no-op whenever the cached token merely looks fresh. Callers that pass a force
// flag must use this entry point; everything else should use RefreshToken.
func RefreshTokenForce(account *config.Account) (string, string, int64, string, error) {
	return RefreshTokenForceContext(context.Background(), account)
}

func RefreshTokenForceContext(ctx context.Context, account *config.Account) (string, string, int64, string, error) {
	return refreshTokenLockedContext(ctx, account, true, 0)
}

func refreshTokenLocked(account *config.Account, force bool, minValidity time.Duration) (string, string, int64, string, error) {
	return refreshTokenLockedContext(context.Background(), account, force, minValidity)
}

func refreshTokenLockedContext(ctx context.Context, account *config.Account, force bool, minValidity time.Duration) (string, string, int64, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if account == nil {
		return "", "", 0, "", fmt.Errorf("RefreshToken: nil account")
	}

	if account.ID == "" {
		if err := ctx.Err(); err != nil {
			return "", "", 0, "", err
		}
		return refreshTokenDirectContext(ctx, account)
	}

	pruneStaleRefreshEntries()
	requestedGeneration := account.DispatchGeneration
	live, ok := config.GetAccountByID(account.ID)
	if !ok {
		if requestedGeneration != 0 {
			return "", "", 0, "", ErrAccountLifecycleChanged
		}
		if err := ctx.Err(); err != nil {
			return "", "", 0, "", err
		}
		// Detached ID-bearing values are pre-persistence validation state: update
		// only the caller-owned credential and never create a config lifecycle.
		accessToken, refreshToken, expiresAt, profileArn, err := refreshTokenDirectContext(ctx, account)
		if err != nil {
			return "", "", 0, "", err
		}
		account.AccessToken = accessToken
		if refreshToken != "" {
			account.RefreshToken = refreshToken
		}
		account.ExpiresAt = expiresAt
		if profileArn != "" {
			account.ProfileArn = profileArn
		}
		return accessToken, refreshToken, expiresAt, profileArn, nil
	}
	if requestedGeneration != 0 && live.DispatchGeneration != requestedGeneration {
		return "", "", 0, "", ErrAccountLifecycleChanged
	}
	generation := requestedGeneration
	if generation == 0 {
		generation = live.DispatchGeneration
	}
	if generation == 0 {
		return "", "", 0, "", ErrAccountLifecycleChanged
	}

	key := refreshLockKey{id: account.ID, generation: generation}
	entry, release, acquireErr := acquireRefreshEntryContext(ctx, key)
	if acquireErr != nil {
		return "", "", 0, "", acquireErr
	}
	defer release()

	live, ok = config.GetAccountByID(account.ID)
	if !ok || live.DispatchGeneration != generation {
		entry.pending = nil
		return "", "", 0, "", ErrAccountLifecycleChanged
	}
	*account = live.Clone()

	// A pending C2 is already beyond the IdP commit point. Publish it locally
	// before honoring caller cancellation so cancellation can never replay C1.
	if entry.pending != nil {
		persisted, err := publishPendingCredential(entry, key, account)
		return persisted.AccessToken, persisted.RefreshToken, persisted.ExpiresAt, persisted.ProfileArn, err
	}
	if err := ctx.Err(); err != nil {
		return "", "", 0, "", err
	}

	working := live.Clone()
	if !force && strings.TrimSpace(working.AccessToken) != "" && working.ExpiresAt > 0 &&
		time.Now().Before(time.Unix(working.ExpiresAt, 0).Add(-minValidity)) {
		return working.AccessToken, working.RefreshToken, working.ExpiresAt, working.ProfileArn, nil
	}

	// Keep local validation caller-cancelable. bindRefreshAttemptClient performs
	// the final ctx.Err check at RoundTrip and only then detaches cancellation for
	// the commit-critical token exchange.
	if err := ctx.Err(); err != nil {
		return "", "", 0, "", err
	}
	accessToken, refreshToken, expiresAt, profileArn, err := refreshTokenDirectContext(ctx, &working)
	if err != nil {
		return "", "", 0, "", err
	}
	refreshToken = oauthOptionalReplacement(refreshToken, working.RefreshToken)
	profileArn = oauthOptionalReplacement(profileArn, working.ProfileArn)

	// Stage C2 in RAM before local publication. If persistence fails before the
	// destination becomes visible, a later caller retries this candidate locally.
	entry.pending = &pendingCredentialPublication{
		observed:     working.Clone(),
		accessToken:  accessToken,
		refreshToken: refreshToken,
		expiresAt:    expiresAt,
		profileArn:   profileArn,
	}
	persisted, err := publishPendingCredential(entry, key, account)
	return persisted.AccessToken, persisted.RefreshToken, persisted.ExpiresAt, persisted.ProfileArn, err
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
		return "", "", 0, "", newRefreshConfigurationError(fmt.Errorf("external IdP refresh requires clientId and tokenEndpoint"))
	}
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if scopes != "" {
		form.Set("scope", scopes)
	}
	accessToken, newRefreshToken, expiresAt, err := postGenericExternalIdpToken(client, tokenEndpoint, form)
	if err != nil {
		return "", "", 0, "", err
	}
	// Some IdPs rotate refresh tokens; others omit it on refresh. Whitespace-only
	// values are treated as omitted without trimming a real token.
	newRefreshToken = oauthOptionalReplacement(newRefreshToken, refreshToken)
	return accessToken, newRefreshToken, expiresAt, "", nil
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
func postGenericExternalIdpToken(client *http.Client, tokenEndpoint string, form url.Values) (accessToken, refreshToken string, expiresAt int64, err error) {
	if strings.TrimSpace(tokenEndpoint) == "" {
		return "", "", 0, newRefreshConfigurationError(fmt.Errorf("external IdP token endpoint is empty"))
	}
	if err := ValidateExternalIdpEndpoint(tokenEndpoint); err != nil {
		return "", "", 0, newRefreshConfigurationError(fmt.Errorf("external IdP token endpoint rejected: %w", err))
	}
	req, err := http.NewRequest("POST", tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", 0, newRefreshConfigurationError(fmt.Errorf("failed to build external IdP token request: %w", err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("external IdP token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := readOAuthTokenResponseBody(resp.Body)
	if err != nil {
		return "", "", 0, oauthTokenResponseReadError(resp.StatusCode, resp.StatusCode >= 200 && resp.StatusCode < 300, err)
	}
	if endpointErr := oauthEndpointErrorFromBody(resp.StatusCode, body); endpointErr != nil {
		return "", "", 0, endpointErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", 0, newOAuthTokenEndpointError(resp.StatusCode, body)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := decodeOAuthTokenJSON(body, &out); err != nil {
		return "", "", 0, err
	}
	if err := validateOAuthAccessToken(out.AccessToken); err != nil {
		return "", "", 0, err
	}
	expiresAt, err = oauthExpiresAt(time.Now().Unix(), out.ExpiresIn)
	if err != nil {
		return "", "", 0, err
	}
	return out.AccessToken, out.RefreshToken, expiresAt, nil
}

// refreshOIDCToken IdC/Builder ID token 刷新
func refreshOIDCToken(refreshToken, clientID, clientSecret, region string, client *http.Client) (string, string, int64, string, error) {
	if clientID == "" || clientSecret == "" {
		return "", "", 0, "", newRefreshConfigurationError(fmt.Errorf("OIDC refresh requires clientId and clientSecret"))
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

	respBody, err := readOAuthTokenResponseBody(resp.Body)
	if err != nil {
		return "", "", 0, "", oauthTokenResponseReadError(resp.StatusCode, resp.StatusCode == http.StatusOK, err)
	}
	if endpointErr := oauthEndpointErrorFromBody(resp.StatusCode, respBody); endpointErr != nil {
		return "", "", 0, "", endpointErr
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, "", newOAuthTokenEndpointError(resp.StatusCode, respBody)
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}
	if err := decodeOAuthTokenJSON(respBody, &result); err != nil {
		return "", "", 0, "", err
	}
	if err := validateOAuthAccessToken(result.AccessToken); err != nil {
		return "", "", 0, "", err
	}
	expiresAt, err := oauthExpiresAt(time.Now().Unix(), result.ExpiresIn)
	if err != nil {
		return "", "", 0, "", err
	}
	result.RefreshToken = oauthOptionalReplacement(result.RefreshToken, refreshToken)
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

	respBody, err := readOAuthTokenResponseBody(resp.Body)
	if err != nil {
		return "", "", 0, "", oauthTokenResponseReadError(resp.StatusCode, resp.StatusCode == http.StatusOK, err)
	}
	if endpointErr := oauthEndpointErrorFromBody(resp.StatusCode, respBody); endpointErr != nil {
		return "", "", 0, "", endpointErr
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, "", newOAuthTokenEndpointError(resp.StatusCode, respBody)
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}
	if err := decodeOAuthTokenJSON(respBody, &result); err != nil {
		return "", "", 0, "", err
	}
	if err := validateOAuthAccessToken(result.AccessToken); err != nil {
		return "", "", 0, "", err
	}
	expiresAt, err := oauthExpiresAt(time.Now().Unix(), result.ExpiresIn)
	if err != nil {
		return "", "", 0, "", err
	}
	result.RefreshToken = oauthOptionalReplacement(result.RefreshToken, refreshToken)
	return result.AccessToken, result.RefreshToken, expiresAt, result.ProfileArn, nil
}
