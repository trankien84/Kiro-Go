package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// Token responses are tiny in normal OAuth flows. Keep one hard upper bound at
// the trust boundary so malformed or hostile IdP/proxy responses cannot make a
// refresh allocate unbounded memory before the error is classified.
const oauthTokenResponseMaxBytes int64 = 1 << 20

// oauthTokenResponseValidationError marks a provider response that reached the
// token endpoint successfully enough to be read, but cannot produce a usable
// credential candidate. It is retryable: malformed/unknown provider output is
// not proof that the canonical credential itself is terminally invalid.
type oauthTokenResponseValidationError struct {
	Reason string
}

func (e *oauthTokenResponseValidationError) Error() string {
	if e == nil || strings.TrimSpace(e.Reason) == "" {
		return "OAuth token response is invalid"
	}
	return "OAuth token response is invalid: " + e.Reason
}

func newOAuthTokenResponseValidationError(reason string) error {
	return &oauthTokenResponseValidationError{Reason: reason}
}

func readOAuthTokenResponseBody(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, newOAuthTokenResponseValidationError("missing response body")
	}
	body, err := io.ReadAll(io.LimitReader(r, oauthTokenResponseMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read OAuth token response: %w", err)
	}
	if int64(len(body)) > oauthTokenResponseMaxBytes {
		return nil, newOAuthTokenResponseValidationError("response exceeds 1 MiB limit")
	}
	return body, nil
}

func oauthTokenResponseReadError(statusCode int, successStatus bool, err error) error {
	if err == nil {
		return nil
	}
	// Once a non-success HTTP status is available it remains the fallback even
	// if the response body is oversized, truncated, or otherwise unreadable.
	// Structured OAuth codes cannot be recovered without the body, but 401/403,
	// 408/429, and 5xx still retain their established disposition.
	if !successStatus {
		return newOAuthTokenEndpointError(statusCode, nil)
	}
	return err
}

func decodeOAuthTokenJSON(body []byte, dst any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return newOAuthTokenResponseValidationError("empty response body")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return newOAuthTokenResponseValidationError("malformed JSON")
	}
	return nil
}

// oauthEndpointErrorFromBody recognizes a standard OAuth error envelope even
// when a non-compliant provider returns it with HTTP 2xx. Structured error codes
// must reach ClassifyRefreshFailure instead of being flattened into a generic
// success-validation failure.
func oauthEndpointErrorFromBody(statusCode int, body []byte) error {
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || strings.TrimSpace(envelope.Error) == "" {
		return nil
	}
	return newOAuthTokenEndpointError(statusCode, body)
}

func validateOAuthAccessToken(token string) error {
	if strings.TrimSpace(token) == "" {
		return newOAuthTokenResponseValidationError("missing access_token")
	}
	return nil
}

func oauthExpiresAt(nowUnix, expiresIn int64) (int64, error) {
	if expiresIn <= 0 {
		return 0, newOAuthTokenResponseValidationError("invalid expires_in")
	}
	if nowUnix > math.MaxInt64-expiresIn {
		return 0, newOAuthTokenResponseValidationError("expires_in overflows Unix time")
	}
	return nowUnix + expiresIn, nil
}

func oauthExpiresInInt(expiresIn int64) (int, error) {
	if expiresIn <= 0 {
		return 0, newOAuthTokenResponseValidationError("invalid expires_in")
	}
	maxInt := int64(^uint(0) >> 1)
	if expiresIn > maxInt {
		return 0, newOAuthTokenResponseValidationError("expires_in exceeds platform int range")
	}
	return int(expiresIn), nil
}

// oauthOptionalReplacement treats whitespace-only optional fields as omitted
// without trimming a real token/value. OAuth token bytes remain exact.
func oauthOptionalReplacement(candidate, current string) string {
	if strings.TrimSpace(candidate) == "" {
		return current
	}
	return candidate
}

func isOAuthTokenResponseValidationError(err error) bool {
	var validationErr *oauthTokenResponseValidationError
	return errors.As(err, &validationErr)
}
