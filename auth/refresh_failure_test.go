package auth

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassifyRefreshFailureStructuredOAuthCodeBeatsHTTPStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   RefreshFailureDisposition
	}{
		{"invalid-grant", 400, `{"error":"invalid_grant","error_description":"revoked"}`, RefreshFailureCredentialTerminal},
		{"invalid-client", 401, `{"error":"invalid_client"}`, RefreshFailureConfigurationTerminal},
		{"invalid-scope", 400, `{"error":"invalid_scope"}`, RefreshFailureConfigurationTerminal},
		{"unauthorized-client", 400, `{"error":"unauthorized_client"}`, RefreshFailureConfigurationTerminal},
		{"unsupported-grant", 400, `{"error":"unsupported_grant_type"}`, RefreshFailureConfigurationTerminal},
		{"slow-down-is-400", 400, `{"error":"slow_down"}`, RefreshFailureRetryable},
		{"temporarily-unavailable", 400, `{"error":"temporarily_unavailable"}`, RefreshFailureRetryable},
		{"server-error", 500, `{"error":"server_error"}`, RefreshFailureRetryable},
		{"unknown-400", 400, `{"error":"provider_extension"}`, RefreshFailureRetryable},
		{"malformed-400", 400, `not-json`, RefreshFailureRetryable},
		{"empty-401", 401, `not-json`, RefreshFailureCredentialTerminal},
		{"empty-429", 429, `not-json`, RefreshFailureRetryable},
		{"empty-500", 500, `not-json`, RefreshFailureRetryable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := newOAuthTokenEndpointError(tc.status, []byte(tc.body))
			if got := ClassifyRefreshFailure(err); got != tc.want {
				t.Fatalf("ClassifyRefreshFailure(%v)=%v want %v", err, got, tc.want)
			}
		})
	}
}

func TestClassifyRefreshFailureLocalConfigurationAndTransport(t *testing.T) {
	if got := ClassifyRefreshFailure(newRefreshConfigurationError(errors.New("missing clientId"))); got != RefreshFailureConfigurationTerminal {
		t.Fatalf("configuration disposition=%v", got)
	}
	if got := ClassifyRefreshFailure(fmt.Errorf("dial token endpoint: %w", errors.New("connection reset"))); got != RefreshFailureRetryable {
		t.Fatalf("transport disposition=%v", got)
	}
}

func TestOAuthTokenEndpointErrorDoesNotEchoUnstructuredBody(t *testing.T) {
	err := newOAuthTokenEndpointError(400, []byte(`SECRET_RAW_BODY`))
	if got := err.Error(); got != "OAuth token endpoint failed (HTTP 400)" {
		t.Fatalf("unexpected unstructured error rendering: %q", got)
	}
}

func TestOAuthTokenEndpointErrorPreservesStructuredLegacyRendering(t *testing.T) {
	err := newOAuthTokenEndpointError(400, []byte(`{"error":"invalid_grant","error_description":"expired code"}`))
	if got := err.Error(); got != "HTTP 400: invalid_grant: expired code" {
		t.Fatalf("unexpected structured error rendering: %q", got)
	}
}

func TestClassifyRefreshFailureUnwrapsProviderContext(t *testing.T) {
	inner := newOAuthTokenEndpointError(400, []byte(`{"error":"invalid_grant"}`))
	wrapped := fmt.Errorf("refresh external identity: %w", inner)
	if got := ClassifyRefreshFailure(wrapped); got != RefreshFailureCredentialTerminal {
		t.Fatalf("wrapped disposition=%v", got)
	}
	if code := RefreshFailureCode(wrapped); code != "invalid_grant" {
		t.Fatalf("wrapped code=%q", code)
	}
}
