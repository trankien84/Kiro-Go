package auth

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

type oauthTrustBoundaryRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn oauthTrustBoundaryRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func oauthTrustBoundaryResponse(request *http.Request, statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func TestOAuthTokenResponseTrustBoundaryHelpers(t *testing.T) {
	t.Run("bounded body", func(t *testing.T) {
		_, err := readOAuthTokenResponseBody(strings.NewReader(strings.Repeat("x", int(oauthTokenResponseMaxBytes)+1)))
		if !isOAuthTokenResponseValidationError(err) {
			t.Fatalf("oversized response error = %v, want validation error", err)
		}
	})

	t.Run("oversized non-success preserves status classification", func(t *testing.T) {
		_, readErr := readOAuthTokenResponseBody(strings.NewReader(strings.Repeat("x", int(oauthTokenResponseMaxBytes)+1)))
		err := oauthTokenResponseReadError(http.StatusUnauthorized, false, readErr)
		if got := ClassifyRefreshFailure(err); got != RefreshFailureCredentialTerminal {
			t.Fatalf("disposition = %v, want credential-terminal; err=%v", got, err)
		}
		if strings.Contains(err.Error(), "xxxx") {
			t.Fatalf("oversized body leaked into error: %v", err)
		}
	})

	t.Run("strict JSON", func(t *testing.T) {
		var payload map[string]any
		err := decodeOAuthTokenJSON([]byte(`{"access_token":"a"} trailing`), &payload)
		if !isOAuthTokenResponseValidationError(err) {
			t.Fatalf("trailing JSON error = %v, want validation error", err)
		}
	})

	t.Run("expiry bounds", func(t *testing.T) {
		for _, expiresIn := range []int64{0, -1} {
			if _, err := oauthExpiresAt(100, expiresIn); !isOAuthTokenResponseValidationError(err) {
				t.Fatalf("expiresIn=%d error=%v, want validation error", expiresIn, err)
			}
		}
		if _, err := oauthExpiresAt(math.MaxInt64-1, 2); !isOAuthTokenResponseValidationError(err) {
			t.Fatalf("overflow error = %v, want validation error", err)
		}
		if got, err := oauthExpiresAt(100, 60); err != nil || got != 160 {
			t.Fatalf("valid expiry = %d, %v; want 160, nil", got, err)
		}
	})

	t.Run("expiry int compatibility", func(t *testing.T) {
		if got, err := oauthExpiresInInt(3600); err != nil || got != 3600 {
			t.Fatalf("oauthExpiresInInt = %d, %v; want 3600, nil", got, err)
		}
		maxInt := int64(^uint(0) >> 1)
		if maxInt < math.MaxInt64 {
			if _, err := oauthExpiresInInt(maxInt + 1); !isOAuthTokenResponseValidationError(err) {
				t.Fatalf("platform overflow error = %v, want validation error", err)
			}
		}
	})

	t.Run("optional replacement preserves bytes", func(t *testing.T) {
		if got := oauthOptionalReplacement("   ", "old"); got != "old" {
			t.Fatalf("whitespace replacement = %q, want old", got)
		}
		if got := oauthOptionalReplacement(" new ", "old"); got != " new " {
			t.Fatalf("non-empty replacement = %q, want exact bytes", got)
		}
	})
}

func TestRefreshOIDCTokenRejectsMalformedSuccess(t *testing.T) {
	orig := oidcTokenURL
	defer func() { oidcTokenURL = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	oidcTokenURL = func(string) string { return srv.URL }

	_, _, _, _, err := refreshOIDCToken("refresh", "client", "secret", "us-east-1", srv.Client())
	if !isOAuthTokenResponseValidationError(err) {
		t.Fatalf("refreshOIDCToken error = %v, want validation error", err)
	}
	if got := ClassifyRefreshFailure(err); got != RefreshFailureRetryable {
		t.Fatalf("disposition = %v, want retryable", got)
	}
}

func TestRefreshOIDCTokenTreatsHTTP200OAuthErrorAsStructuredFailure(t *testing.T) {
	orig := oidcTokenURL
	defer func() { oidcTokenURL = orig }()

	tests := []struct {
		name string
		code string
		want RefreshFailureDisposition
	}{
		{name: "invalid grant", code: "invalid_grant", want: RefreshFailureCredentialTerminal},
		{name: "slow down", code: "slow_down", want: RefreshFailureRetryable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":             tc.code,
					"error_description": "provider response",
				})
			}))
			defer srv.Close()
			oidcTokenURL = func(string) string { return srv.URL }

			_, _, _, _, err := refreshOIDCToken("refresh", "client", "secret", "us-east-1", srv.Client())
			if code := RefreshFailureCode(err); code != tc.code {
				t.Fatalf("RefreshFailureCode = %q, want %q; err=%v", code, tc.code, err)
			}
			if got := ClassifyRefreshFailure(err); got != tc.want {
				t.Fatalf("disposition = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRefreshSocialTokenPreservesRefreshTokenWhenWhitespaceOnly(t *testing.T) {
	orig := socialTokenURL
	defer func() { socialTokenURL = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken":  "new-access",
			"refreshToken": "   ",
			"expiresIn":    3600,
		})
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	access, refresh, expiresAt, _, err := refreshSocialToken("old-refresh", srv.Client())
	if err != nil {
		t.Fatalf("refreshSocialToken: %v", err)
	}
	if access != "new-access" || refresh != "old-refresh" {
		t.Fatalf("access=%q refresh=%q, want new-access/old-refresh", access, refresh)
	}
	if expiresAt <= time.Now().Unix() {
		t.Fatalf("expiresAt=%d is not in the future", expiresAt)
	}
}

func TestPostGenericExternalIdpTokenRequiresExpiry(t *testing.T) {
	previous := SetExternalIdpValidatorForTest(func(string) error { return nil })
	t.Cleanup(func() { SetExternalIdpValidatorForTest(previous) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access"}`))
	}))
	defer srv.Close()

	_, _, _, err := postGenericExternalIdpToken(srv.Client(), srv.URL, url.Values{})
	if !isOAuthTokenResponseValidationError(err) {
		t.Fatalf("postGenericExternalIdpToken error = %v, want validation error", err)
	}
}

func TestRefreshGenericExternalIdpTokenPreservesRefreshTokenWhenWhitespaceOnly(t *testing.T) {
	previous := SetExternalIdpValidatorForTest(func(string) error { return nil })
	t.Cleanup(func() { SetExternalIdpValidatorForTest(previous) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "   ",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	access, refresh, expiresAt, _, err := refreshGenericExternalIdpToken(
		"old-refresh", "client", srv.URL, "", srv.Client(),
	)
	if err != nil {
		t.Fatalf("refreshGenericExternalIdpToken: %v", err)
	}
	if access != "new-access" || refresh != "old-refresh" {
		t.Fatalf("access=%q refresh=%q, want new-access/old-refresh", access, refresh)
	}
	if expiresAt <= time.Now().Unix() {
		t.Fatalf("expiresAt=%d is not in the future", expiresAt)
	}
}

func TestRefreshSocialTokenRejectsOversizedResponse(t *testing.T) {
	orig := socialTokenURL
	defer func() { socialTokenURL = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", int(oauthTokenResponseMaxBytes)+1)))
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	_, _, _, _, err := refreshSocialToken("old-refresh", srv.Client())
	if !isOAuthTokenResponseValidationError(err) {
		t.Fatalf("oversized social response error = %v, want validation error", err)
	}
}

func TestRefreshTokenForceMalformedSocialSuccessDoesNotPublish(t *testing.T) {
	const childEnv = "KIRO_GO_OAUTH_TRUST_BOUNDARY_CHILD"
	if !runBoundedTestSubprocess(t, childEnv, "^TestRefreshTokenForceMalformedSocialSuccessDoesNotPublish$", 20*time.Second, nil) {
		return
	}

	orig := socialTokenURL
	defer func() { socialTokenURL = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const id = "oauth-trust-boundary-malformed"
	if err := config.AddAccount(config.Account{
		ID:           id,
		AuthMethod:   "social",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    1,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}
	account, ok := config.GetAccountByID(id)
	if !ok {
		t.Fatal("account not found")
	}

	_, _, _, _, err := RefreshTokenForce(&account)
	if !isOAuthTokenResponseValidationError(err) {
		t.Fatalf("RefreshTokenForce error = %v, want validation error", err)
	}
	canonical, ok := config.GetAccountByID(id)
	if !ok {
		t.Fatal("canonical account disappeared")
	}
	if canonical.AccessToken != "old-access" || canonical.RefreshToken != "old-refresh" || canonical.ExpiresAt != 1 {
		t.Fatalf("malformed success mutated canonical credential: access=%q refresh=%q expiresAt=%d",
			canonical.AccessToken, canonical.RefreshToken, canonical.ExpiresAt)
	}
}

func TestRefreshFailureValidationErrorIsExplicitlyRetryable(t *testing.T) {
	err := newOAuthTokenResponseValidationError("missing access token")
	if !isOAuthTokenResponseValidationError(err) {
		t.Fatalf("unexpected validation error type: %T", err)
	}
	if got := ClassifyRefreshFailure(err); got != RefreshFailureRetryable {
		t.Fatalf("disposition = %v, want retryable", got)
	}
}

func TestExchangeExternalIdpCodeConvertsValidatedExpiry(t *testing.T) {
	previous := SetExternalIdpValidatorForTest(func(string) error { return nil })
	t.Cleanup(func() { SetExternalIdpValidatorForTest(previous) })

	client := &http.Client{Transport: oauthTrustBoundaryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return oauthTrustBoundaryResponse(request, http.StatusOK,
			`{"access_token":"access","refresh_token":"refresh","expires_in":3600}`), nil
	})}

	access, refresh, expiresIn, err := exchangeExternalIdpCode(
		client,
		"https://idp.example/token",
		"client",
		"code",
		"verifier",
		kiroRedirectURI+kiroOAuthCallbackPath,
		"offline_access",
	)
	if err != nil {
		t.Fatalf("exchangeExternalIdpCode: %v", err)
	}
	if access != "access" || refresh != "refresh" || expiresIn != 3600 {
		t.Fatalf("access=%q refresh=%q expiresIn=%d, want access/refresh/3600", access, refresh, expiresIn)
	}
}

func TestExchangeSocialCodeUsesOAuthTrustBoundary(t *testing.T) {
	t.Run("valid response", func(t *testing.T) {
		client := &http.Client{Transport: oauthTrustBoundaryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			return oauthTrustBoundaryResponse(request, http.StatusOK,
				`{"accessToken":"access","refreshToken":"refresh","expiresIn":3600,"profileArn":"profile"}`), nil
		})}
		access, refresh, expiresIn, profileArn, err := exchangeSocialCode(client, "code", "verifier")
		if err != nil {
			t.Fatalf("exchangeSocialCode: %v", err)
		}
		if access != "access" || refresh != "refresh" || expiresIn != 3600 || profileArn != "profile" {
			t.Fatalf("access=%q refresh=%q expiresIn=%d profileArn=%q", access, refresh, expiresIn, profileArn)
		}
	})

	t.Run("malformed success", func(t *testing.T) {
		client := &http.Client{Transport: oauthTrustBoundaryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			return oauthTrustBoundaryResponse(request, http.StatusOK, `{}`), nil
		})}
		_, _, _, _, err := exchangeSocialCode(client, "code", "verifier")
		if !isOAuthTokenResponseValidationError(err) {
			t.Fatalf("exchangeSocialCode error = %v, want validation error", err)
		}
		if got := ClassifyRefreshFailure(err); got != RefreshFailureRetryable {
			t.Fatalf("disposition = %v, want retryable", got)
		}
	})

	t.Run("HTTP 200 OAuth error", func(t *testing.T) {
		client := &http.Client{Transport: oauthTrustBoundaryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			return oauthTrustBoundaryResponse(request, http.StatusOK,
				`{"error":"invalid_grant","error_description":"expired code"}`), nil
		})}
		_, _, _, _, err := exchangeSocialCode(client, "code", "verifier")
		if code := RefreshFailureCode(err); code != "invalid_grant" {
			t.Fatalf("RefreshFailureCode = %q, want invalid_grant; err=%v", code, err)
		}
	})

	t.Run("oversized response", func(t *testing.T) {
		client := &http.Client{Transport: oauthTrustBoundaryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			return oauthTrustBoundaryResponse(request, http.StatusOK,
				strings.Repeat("x", int(oauthTokenResponseMaxBytes)+1)), nil
		})}
		_, _, _, _, err := exchangeSocialCode(client, "code", "verifier")
		if !isOAuthTokenResponseValidationError(err) {
			t.Fatalf("oversized exchangeSocialCode error = %v, want validation error", err)
		}
	})
}
