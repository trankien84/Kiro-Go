package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// RefreshFailureDisposition describes whether repeating the same refresh-token
// operation can reasonably succeed without operator/user intervention.
type RefreshFailureDisposition uint8

const (
	RefreshFailureRetryable RefreshFailureDisposition = iota
	RefreshFailureCredentialTerminal
	RefreshFailureConfigurationTerminal
)

// OAuthTokenEndpointError preserves the structured OAuth error returned by a
// token endpoint. Keeping the code separate from the human-readable description
// prevents callers from deriving retry policy from prose or from HTTP 400 alone.
type OAuthTokenEndpointError struct {
	StatusCode  int
	Code        string
	Description string
}

func (e *OAuthTokenEndpointError) Error() string {
	if e == nil {
		return "OAuth token endpoint error"
	}
	code := strings.TrimSpace(e.Code)
	description := strings.TrimSpace(e.Description)
	switch {
	case code != "" && description != "":
		// Keep the legacy structured rendering used by the Microsoft token
		// boundary while retaining the typed fields for errors.As/classification.
		return fmt.Sprintf("HTTP %d: %s: %s", e.StatusCode, code, description)
	case code != "":
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, code)
	default:
		return fmt.Sprintf("OAuth token endpoint failed (HTTP %d)", e.StatusCode)
	}
}

// RefreshConfigurationError marks a refresh that cannot succeed until persisted
// client/endpoint configuration is repaired. It is intentionally distinct from
// microsoftValidationShapeError, whose only purpose is to permit the strict
// Microsoft path to fall back to generic external-IdP OAuth before any network
// request has been made.
type RefreshConfigurationError struct {
	Err error
}

func (e *RefreshConfigurationError) Error() string {
	if e == nil || e.Err == nil {
		return "OAuth refresh configuration is invalid"
	}
	return e.Err.Error()
}

func (e *RefreshConfigurationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func newRefreshConfigurationError(err error) error {
	if err == nil {
		return nil
	}
	return &RefreshConfigurationError{Err: err}
}

func boundedOAuthErrorDescription(value string) string {
	value = strings.TrimSpace(value)
	const max = 512
	if len(value) <= max {
		return value
	}
	return value[:max] + "…"
}

// newOAuthTokenEndpointError extracts only the standard OAuth error fields. Raw
// response bodies are deliberately not retained: provider error pages can carry
// request metadata or secrets that should not be copied into BanReason/logs.
func newOAuthTokenEndpointError(statusCode int, body []byte) error {
	var payload struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &payload)
	return &OAuthTokenEndpointError{
		StatusCode:  statusCode,
		Code:        strings.ToLower(strings.TrimSpace(payload.Error)),
		Description: boundedOAuthErrorDescription(payload.ErrorDescription),
	}
}

// RefreshFailureCode returns the normalized structured OAuth error code when
// one survived the provider boundary. It returns "" for transport/local errors.
func RefreshFailureCode(err error) string {
	var endpointErr *OAuthTokenEndpointError
	if errors.As(err, &endpointErr) && endpointErr != nil {
		return strings.ToLower(strings.TrimSpace(endpointErr.Code))
	}
	return ""
}

func refreshFailureStatus(err error) (int, bool) {
	var endpointErr *OAuthTokenEndpointError
	if errors.As(err, &endpointErr) && endpointErr != nil {
		return endpointErr.StatusCode, true
	}
	return 0, false
}

func legacyHTTPStatusToken(message string, status int) bool {
	needle := strconv.Itoa(status)
	for offset := 0; ; {
		idx := strings.Index(message[offset:], needle)
		if idx < 0 {
			return false
		}
		idx += offset
		beforeOK := idx == 0 || message[idx-1] < '0' || message[idx-1] > '9'
		after := idx + len(needle)
		afterOK := after == len(message) || message[after] < '0' || message[after] > '9'
		if beforeOK && afterOK {
			prefix := strings.ToLower(strings.TrimSpace(message[:idx]))
			if strings.HasSuffix(prefix, "http") || strings.HasSuffix(prefix, "status") || strings.HasSuffix(prefix, "failed:") {
				return true
			}
		}
		offset = idx + len(needle)
		if offset >= len(message) {
			return false
		}
	}
}

// ClassifyRefreshFailure centralizes retry policy for refresh-token operations.
// Structured OAuth codes always win over HTTP status. In particular, AWS returns
// slow_down with HTTP 400, which must remain retryable. Unknown/malformed errors
// fail safe to retry rather than permanently banning a credential without proof.
func ClassifyRefreshFailure(err error) RefreshFailureDisposition {
	if err == nil {
		return RefreshFailureRetryable
	}
	if isOAuthTokenResponseValidationError(err) {
		return RefreshFailureRetryable
	}

	var configErr *RefreshConfigurationError
	if errors.As(err, &configErr) {
		return RefreshFailureConfigurationTerminal
	}

	if code := RefreshFailureCode(err); code != "" {
		switch code {
		case "slow_down", "server_error", "temporarily_unavailable", "authorization_pending":
			return RefreshFailureRetryable
		case "invalid_grant", "invalid_token", "expired_token":
			return RefreshFailureCredentialTerminal
		case "invalid_client", "unauthorized_client", "invalid_request", "invalid_scope",
			"unsupported_grant_type", "invalid_resource", "interaction_required",
			"consent_required", "login_required", "access_denied":
			return RefreshFailureConfigurationTerminal
		default:
			return RefreshFailureRetryable
		}
	}

	if status, ok := refreshFailureStatus(err); ok {
		switch {
		case status == 408 || status == 429 || status >= 500:
			return RefreshFailureRetryable
		case status == 401 || status == 403:
			return RefreshFailureCredentialTerminal
		default:
			return RefreshFailureRetryable
		}
	}

	// Compatibility for older/custom refresh implementations that still return
	// prose. New built-in token endpoints return OAuthTokenEndpointError instead.
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "invalid_grant"), strings.Contains(lower, "invalid grant"),
		strings.Contains(lower, "invalid_token"), strings.Contains(lower, "invalid token"),
		strings.Contains(lower, "token expired"), strings.Contains(lower, "token has expired"):
		return RefreshFailureCredentialTerminal
	case strings.Contains(lower, "invalid_client"), strings.Contains(lower, "unauthorized_client"),
		strings.Contains(lower, "invalid_scope"), strings.Contains(lower, "unsupported_grant_type"):
		return RefreshFailureConfigurationTerminal
	case legacyHTTPStatusToken(lower, 401), legacyHTTPStatusToken(lower, 403):
		return RefreshFailureCredentialTerminal
	default:
		return RefreshFailureRetryable
	}
}

func RefreshFailureIsTerminal(err error) bool {
	return ClassifyRefreshFailure(err) != RefreshFailureRetryable
}
