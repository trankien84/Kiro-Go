package auth

import (
	"net/http"
	"testing"
)

// These seams are exported (not `_test.go`) because tests in OTHER packages
// (proxy) need to call them — a `_test.go` file is invisible to an importing
// package's build even during `go test`, so moving this file to
// export_test.go would break every proxy test that calls e.g.
// auth.SetGlobalAuthClientForTest. That leaves them shipping in the
// production binary as ordinary exported API: one call to
// SetExternalIdpValidatorForTest would disable the SSRF host allow-list for
// every credential-carrying outbound request at once. testing.Testing()
// (Go 1.21+) reports whether the current process is a `go test` binary —
// true for every test in this repo, always false in a real deployment — so
// gating each setter on it closes that production exposure without a build
// tag (which `go test ./...` run without `-tags` would never define, quietly
// breaking the whole suite) or a file rename (which breaks cross-package
// callers, per above).
func blockedOutsideTestBinary() bool { return !testing.Testing() }

// SetOIDCTokenURLForTest replaces the OIDC token URL builder. Test-only; a
// no-op outside a test binary (see blockedOutsideTestBinary).
func SetOIDCTokenURLForTest(fn func(region string) string) {
	if fn == nil || blockedOutsideTestBinary() {
		return
	}
	oidcTokenURL = fn
}

// GetOIDCTokenURLForTest returns the current OIDC token URL builder so tests
// can restore it after replacement.
func GetOIDCTokenURLForTest() func(region string) string { return oidcTokenURL }

// SetGlobalAuthClientForTest swaps the global auth HTTP client. The package's
// init() installs a client whose Transport calls http.ProxyFromEnvironment, and
// that function caches env vars on first call — which corrupts later tests
// that rely on t.Setenv("HTTPS_PROXY", ...). Tests that need to issue an HTTP
// request against a httptest server should install a client whose Transport
// has Proxy=nil to keep env-proxy state clean. Returns the previous client so
// callers can restore it.
func SetGlobalAuthClientForTest(c *http.Client) *http.Client {
	old := httpClientStore.Load()
	if c != nil && !blockedOutsideTestBinary() {
		httpClientStore.Store(c)
	}
	return old
}

// SetExternalIdpValidatorForTest swaps the validator behind ValidateExternalIdpEndpoint
// and returns the previous one so callers can restore it. Tests POST against httptest
// servers (http + 127.0.0.1), which the real allow-list validator rejects, so tests
// install a no-op validator here. Mirrors SetGlobalAuthClientForTest's swap-and-restore
// shape. Test-only.
func SetExternalIdpValidatorForTest(fn func(string) error) func(string) error {
	old := externalIdpEndpointValidator
	if fn != nil && !blockedOutsideTestBinary() {
		externalIdpEndpointValidator = fn
	}
	return old
}
