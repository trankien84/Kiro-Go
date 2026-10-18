package auth

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildAuthTransportUsesExplicitProxyURL(t *testing.T) {
	transport := buildAuthTransport("http://proxy.local:8080")
	req := &http.Request{URL: mustParseURL(t, "https://oidc.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://proxy.local:8080")
}

const authEnvProxySubprocess = "KIRO_GO_TEST_AUTH_ENV_PROXY_SUBPROCESS"

func TestBuildAuthTransportFallsBackToEnvironmentProxy(t *testing.T) {
	if !runBoundedTestSubprocess(
		t,
		authEnvProxySubprocess,
		"^TestBuildAuthTransportFallsBackToEnvironmentProxy$",
		20*time.Second,
		authProxySubprocessEnv(os.Environ()),
	) {
		return
	}

	transport := buildAuthTransport("")
	req := &http.Request{URL: mustParseURL(t, "https://oidc.us-east-1.amazonaws.com")}
	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://env-proxy.local:2323")
}

func authProxySubprocessEnv(base []string) []string {
	out := make([]string, 0, len(base)+3)
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		switch {
		case strings.EqualFold(key, "HTTP_PROXY"),
			strings.EqualFold(key, "HTTPS_PROXY"),
			strings.EqualFold(key, "NO_PROXY"),
			strings.EqualFold(key, authEnvProxySubprocess):
			continue
		}
		out = append(out, item)
	}
	return append(out,
		"HTTPS_PROXY=http://env-proxy.local:2323",
		"NO_PROXY=",
	)
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("invalid test URL: %v", err)
	}
	return parsed
}

func assertProxyURL(t *testing.T, got *url.URL, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("expected proxy URL %q, got nil", want)
	}
	if got.String() != want {
		t.Fatalf("expected proxy URL %q, got %q", want, got.String())
	}
}
