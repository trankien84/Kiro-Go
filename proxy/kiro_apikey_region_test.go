package proxy

import (
	"bytes"
	"io"
	"kiro-go/config"
	"kiro-go/pool"
	"net/http"
	"testing"
)

// reprobeApiKeyRegion must discover the region a ksk_ key actually serves when the
// stored region is wrong, and mutate the in-memory account so the caller's
// immediate retry targets the new region — but it must NOT persist that region to
// config itself. probeKiroApiKey only validates the REST plane (GetUsageLimits);
// the auth failure that triggers a reprobe comes from the RUNTIME plane
// (runtime.{region}.kiro.dev), a different service that can disagree. Persisting
// here, before the runtime plane confirms the candidate, is exactly the bug M1
// fixed: it could permanently strand an account on a region the runtime plane never
// actually accepted. Persistence now happens only in CallKiroAPIContext, after its
// runtime-plane retry succeeds — see TestCallKiroAPIRegionReprobeRetriesOnAuthFailure
// (persists) and TestCallKiroAPIRegionReprobeDoesNotPersistWhenRuntimeRetryFails
// (does not).
func TestReprobeApiKeyRegionRedetectsButDoesNotPersist(t *testing.T) {
	mustInitConfig(t)

	acc := config.Account{
		ID:          "acct-1",
		AuthMethod:  "api_key",
		KiroApiKey:  "ksk_key",
		AccessToken: "ksk_key",
		Region:      "us-east-1", // wrong region — upstream rejects the bearer token here
		Enabled:     true,
	}
	if err := config.AddAccount(acc); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	// The key is only valid in eu-central-1.
	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		if region != "eu-central-1" {
			return nil, errTest("HTTP 403: not served in " + region)
		}
		return &config.AccountInfo{Email: "a@example.com", UserId: "u-1"}, nil
	}

	account := acc // local copy the caller would hold
	region, ok := reprobeApiKeyRegion(&account)
	if !ok {
		t.Fatalf("expected region re-detection to succeed")
	}
	if region != "eu-central-1" {
		t.Fatalf("expected eu-central-1, got %q", region)
	}
	// In-memory account updated so the immediate retry targets the new region.
	if account.Region != "eu-central-1" {
		t.Fatalf("in-memory account region not updated, got %q", account.Region)
	}
	// NOT persisted to config: reprobeApiKeyRegion only proves the REST plane; the
	// caller (CallKiroAPIContext) persists only once the runtime-plane retry
	// actually succeeds.
	var persisted string
	for _, a := range config.GetAccounts() {
		if a.ID == "acct-1" {
			persisted = a.Region
		}
	}
	if persisted != "us-east-1" {
		t.Fatalf("reprobeApiKeyRegion must not persist by itself, got persisted region %q", persisted)
	}
}

// A genuinely dead key (rejected in every region) must return ok=false and leave the
// stored region untouched, so the caller surfaces the original auth error.
func TestReprobeApiKeyRegionDeadKey(t *testing.T) {
	mustInitConfig(t)

	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		return nil, errTest("HTTP 403: The bearer token included in the request is invalid.")
	}

	account := config.Account{
		ID:         "acct-2",
		AuthMethod: "api_key",
		KiroApiKey: "ksk_dead",
		Region:     "us-east-1",
	}
	if region, ok := reprobeApiKeyRegion(&account); ok {
		t.Fatalf("expected no region for a dead key, got %q", region)
	}
	if account.Region != "us-east-1" {
		t.Fatalf("region must be unchanged for a dead key, got %q", account.Region)
	}
}

// Non-api_key accounts must never trigger the api_key reprobe path.
func TestReprobeApiKeyRegionSkipsNonApiKey(t *testing.T) {
	account := &config.Account{ID: "acct-3", AuthMethod: "social", Region: "us-east-1"}
	if _, ok := reprobeApiKeyRegion(account); ok {
		t.Fatalf("expected no reprobe for non-api_key account")
	}
}

// End-to-end: CallKiroAPI must transparently retry once against a re-detected
// region after an upstream 403, and the client must see only the successful
// second attempt's output (no double-emission — the auth error is returned
// before any streaming callback runs).
func TestCallKiroAPIRegionReprobeRetriesOnAuthFailure(t *testing.T) {
	mustInitConfig(t)

	acc := config.Account{
		ID:         "acct-reprobe",
		AuthMethod: "api_key",
		KiroApiKey: "ksk_test",
		Region:     "us-east-1", // wrong region
		ProxyURL:   kiroRetryTestProxyURL,
	}
	if err := config.AddAccount(acc); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		if region == "eu-central-1" {
			return &config.AccountInfo{Email: "a@example.com", UserId: "u-1"}, nil
		}
		return nil, errTest("HTTP 403: not served in " + region)
	}

	var hosts []string
	installKiroStreamTestClient(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		if len(hosts) == 1 {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(bytes.NewReader([]byte(`{"message":"The bearer token included in the request is invalid."}`))),
				Header:     make(http.Header),
			}, nil
		}
		return kiroStreamTestResponse(bytes.NewReader(awsEventStreamFrame(t,
			"assistantResponseEvent", map[string]interface{}{"content": "recovered"}))), nil
	}))

	account := acc // local copy, mirrors how a caller holds *config.Account
	var text string
	err := CallKiroAPI(&account, newKiroRetryTestPayload(), &KiroStreamCallback{
		OnText: func(chunk string, _ bool) { text += chunk },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "recovered" {
		t.Fatalf("text = %q, want the retried call's output only (no double-emission)", text)
	}
	if len(hosts) != 2 {
		t.Fatalf("expected exactly 2 upstream calls (original + one bounded retry), got %d: %v", len(hosts), hosts)
	}
	if hosts[0] != "runtime.us-east-1.kiro.dev" || hosts[1] != "runtime.eu-central-1.kiro.dev" {
		t.Fatalf("hosts = %v, want the retry to target the re-detected region", hosts)
	}
	if account.Region != "eu-central-1" {
		t.Fatalf("in-memory account region not updated, got %q", account.Region)
	}
	var persisted string
	for _, a := range config.GetAccounts() {
		if a.ID == "acct-reprobe" {
			persisted = a.Region
		}
	}
	if persisted != "eu-central-1" {
		t.Fatalf("persisted region not updated, got %q", persisted)
	}
}

// A genuinely dead api_key (rejected in every candidate region) must surface the
// ORIGINAL auth error unchanged after exactly one bounded reprobe attempt — not be
// swallowed, and not retried forever. This is what keeps a real credential failure
// flowing into the normal account-failure / auto-ban path instead of being masked
// by the region-reprobe feature.
func TestCallKiroAPIRegionReprobeSurfacesOriginalAuthErrorWhenKeyIsDead(t *testing.T) {
	mustInitConfig(t)

	acc := config.Account{
		ID:         "acct-dead",
		AuthMethod: "api_key",
		KiroApiKey: "ksk_dead",
		Region:     "us-east-1",
		ProxyURL:   kiroRetryTestProxyURL,
	}
	if err := config.AddAccount(acc); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	var probeCalls int
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		probeCalls++
		return nil, errTest("HTTP 403: The bearer token included in the request is invalid.")
	}

	var upstreamCalls int
	installKiroStreamTestClient(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		upstreamCalls++
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"message":"The bearer token included in the request is invalid."}`))),
			Header:     make(http.Header),
		}, nil
	}))

	account := acc
	err := CallKiroAPI(&account, newKiroRetryTestPayload(), &KiroStreamCallback{})
	if err == nil {
		t.Fatalf("expected the original auth error to surface for a dead key")
	}
	if !pool.IsAuthFailure(err) {
		t.Fatalf("expected an auth-classified error so the caller's failover/ban logic still fires, got: %v", err)
	}
	// Exactly one upstream call: the reprobe found nothing, so there is no retry —
	// a dead key must not turn into an unbounded fan-out of region probes or
	// upstream calls.
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want exactly 1 (no retry for a key that is dead everywhere)", upstreamCalls)
	}
	if probeCalls != len(kiroApiKeyCandidateRegions())-1 {
		t.Fatalf("probeCalls = %d, want one probe per non-current candidate region (bounded, not unbounded)", probeCalls)
	}
	if account.Region != "us-east-1" {
		t.Fatalf("region must be left unchanged when no candidate region accepts the key, got %q", account.Region)
	}
}

// M1 regression test: a candidate region can pass the REST-plane probe
// (probeKiroApiKey -> GetUsageLimits -> q.{region}.amazonaws.com) while the RUNTIME
// plane (runtime.{region}.kiro.dev, the service the original request actually
// failed against) still rejects it — the two are different services and can
// disagree. Before the M1 fix, config.UpdateAccountRegion was called inside the
// probe loop and never rolled back, so this scenario permanently stranded the
// account on a region the runtime plane never accepted (unbanning it afterwards did
// not restore service). The fix defers persistence to after the runtime retry
// succeeds, so a wrong candidate must cost exactly one failed retry and leave both
// the persisted and in-memory region untouched.
func TestCallKiroAPIRegionReprobeDoesNotPersistWhenRuntimeRetryFails(t *testing.T) {
	mustInitConfig(t)

	acc := config.Account{
		ID:         "acct-runtime-mismatch",
		AuthMethod: "api_key",
		KiroApiKey: "ksk_test",
		Region:     "us-east-1", // wrong region
		ProxyURL:   kiroRetryTestProxyURL,
	}
	if err := config.AddAccount(acc); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	// REST plane accepts eu-central-1 as a candidate...
	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		if region == "eu-central-1" {
			return &config.AccountInfo{Email: "a@example.com", UserId: "u-1"}, nil
		}
		return nil, errTest("HTTP 403: not served in " + region)
	}

	// ...but the runtime plane rejects BOTH the original and the re-detected region
	// (e.g. a transient runtime-side outage independent of the REST plane).
	var hosts []string
	installKiroStreamTestClient(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"message":"The bearer token included in the request is invalid."}`))),
			Header:     make(http.Header),
		}, nil
	}))

	account := acc
	err := CallKiroAPI(&account, newKiroRetryTestPayload(), &KiroStreamCallback{})
	if err == nil {
		t.Fatalf("expected the runtime-plane auth error to surface")
	}
	if !pool.IsAuthFailure(err) {
		t.Fatalf("expected an auth-classified error so the caller's failover/ban logic still fires, got: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("expected exactly 2 upstream calls (original + one bounded retry), got %d: %v", len(hosts), hosts)
	}
	if hosts[0] != "runtime.us-east-1.kiro.dev" || hosts[1] != "runtime.eu-central-1.kiro.dev" {
		t.Fatalf("hosts = %v, want the retry to target the REST-plane candidate", hosts)
	}
	// The optimistic in-memory region set for the retry must be rolled back once the
	// runtime plane rejects the candidate too.
	if account.Region != "us-east-1" {
		t.Fatalf("in-memory account region must be rolled back to the original, got %q", account.Region)
	}
	// And the wrong candidate must never have reached persistent config.
	var persisted string
	for _, a := range config.GetAccounts() {
		if a.ID == "acct-runtime-mismatch" {
			persisted = a.Region
		}
	}
	if persisted != "us-east-1" {
		t.Fatalf("persisted region must be unchanged when the runtime-plane retry fails, got %q", persisted)
	}
}

// M2 regression test: kiroApiKeyCandidateRegions can be sourced from
// KIRO_PROFILE_REGIONS (an operator-controlled env var, unlike a customer request,
// but still not pattern-checked at its own definition site, unlike
// kiroProfileRegionCandidates). A malformed entry must never reach probeKiroApiKey
// (which turns it into an outbound host) and must never be considered a valid
// candidate to persist.
func TestReprobeApiKeyRegionRejectsMalformedCandidateRegion(t *testing.T) {
	mustInitConfig(t)
	t.Setenv("KIRO_PROFILE_REGIONS", "evil.amazonaws.com,../../etc,eu-west-1")

	account := &config.Account{
		ID:         "acct-malformed-region",
		AuthMethod: "api_key",
		KiroApiKey: "ksk_x",
		Region:     "us-east-1",
	}

	var probed []string
	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		probed = append(probed, region)
		if region == "eu-west-1" {
			return &config.AccountInfo{Email: "a@example.com", UserId: "u-1"}, nil
		}
		return nil, errTest("HTTP 403: not served in " + region)
	}

	region, ok := reprobeApiKeyRegion(account)
	if !ok {
		t.Fatalf("expected the well-formed candidate to be found")
	}
	if region != "eu-west-1" {
		t.Fatalf("region = %q, want eu-west-1", region)
	}
	for _, r := range probed {
		if r != "eu-west-1" {
			t.Fatalf("probed malformed candidate region %q — it must be filtered before reaching probeKiroApiKey", r)
		}
	}
}

// Bedrock and custom_api accounts must never enter the api_key reprobe path: the
// AuthMethod string is mutually exclusive (IsApiKeyCredential/IsBedrock/IsCustomApi
// all key off the same field), but this pins that contract at the CallKiroAPI
// wiring level, not just inside reprobeApiKeyRegion.
func TestCallKiroAPIRegionReprobeSkipsBedrockAndCustomApi(t *testing.T) {
	for _, method := range []string{"bedrock", "custom_api"} {
		t.Run(method, func(t *testing.T) {
			// KiroApiKey is deliberately non-empty here: reprobeApiKeyRegion checks
			// IsApiKeyCredential() before the empty-key check, but with an empty key
			// both accounts would short-circuit on the key check first and this test
			// would pass even if the AuthMethod guard were deleted entirely.
			account := &config.Account{ID: "acct-" + method, AuthMethod: method, Region: "us-east-1", KiroApiKey: "ksk_x"}
			if account.IsApiKeyCredential() {
				t.Fatalf("%s account must not classify as an api_key credential", method)
			}
			// reprobeApiKeyRegion itself is the guarded chokepoint CallKiroAPI calls
			// into; confirm it refuses directly regardless of dispatch-order changes
			// elsewhere.
			if _, ok := reprobeApiKeyRegion(account); ok {
				t.Fatalf("expected no reprobe for a %s account", method)
			}
		})
	}
}

// pool.IsAuthFailure is a substring/status-token matcher over the error message,
// not a "this happened before any bytes reached the client" signal — an error
// surfaced AFTER partial streaming can still contain "403" or "unauthorized" (an
// upstream error event relayed mid-stream, a proxy-layer rejection, etc). The
// region-reprobe retry must stay off in that case: retrying would re-run the whole
// endpoint loop after the client already received output, concatenating two
// attempts into one response. This pins that the gate is emittedAny (threaded out
// of callKiroEndpointsOnce), not just "the error looks like an auth failure".
func TestCallKiroAPIRegionReprobeNeverRetriesAfterPartialEmission(t *testing.T) {
	mustInitConfig(t)

	acc := config.Account{
		ID:         "acct-partial-emit",
		AuthMethod: "api_key",
		KiroApiKey: "ksk_test",
		Region:     "us-east-1",
		ProxyURL:   kiroRetryTestProxyURL,
	}
	if err := config.AddAccount(acc); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	var probeCalls int
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		probeCalls++
		return &config.AccountInfo{}, nil // would succeed if ever called — must not be
	}

	var upstreamCalls int
	installKiroStreamTestClient(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		upstreamCalls++
		frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "partial"})
		// The stream emits one complete frame, then the connection dies with an
		// error whose text is auth-shaped (contains a standalone "403" token) even
		// though this is a mid-stream transport failure, not an upstream 403
		// response. pool.IsAuthFailure would classify this as an auth failure by
		// text alone.
		return kiroStreamTestResponse(&truncatedReader{
			data: append(frame, 0, 0, 1),
			err:  errTest("connection reset: upstream returned 403 mid-stream"),
		}), nil
	}))

	account := acc
	var text string
	err := CallKiroAPI(&account, newKiroRetryTestPayload(), &KiroStreamCallback{
		OnText: func(chunk string, _ bool) { text += chunk },
	})
	if err == nil {
		t.Fatalf("expected the mid-stream error to surface")
	}
	if !pool.IsAuthFailure(err) {
		t.Fatalf("test setup invariant broken: error must classify as auth-shaped by text, got: %v", err)
	}
	if text != "partial" {
		t.Fatalf("text = %q, want the partial output that reached the client before the error", text)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want exactly 1 — no retry once output has already been emitted", upstreamCalls)
	}
	if probeCalls != 0 {
		t.Fatalf("probeCalls = %d, want 0 — the region reprobe must never fire once output has been emitted", probeCalls)
	}
	if account.Region != "us-east-1" {
		t.Fatalf("region must be unchanged, got %q", account.Region)
	}
}
