package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
)

type phase66F2BedrockRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn phase66F2BedrockRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func phase66F2BedrockResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func phase66F2BedrockAccount(id string) *config.Account {
	return &config.Account{
		ID:            id,
		AuthMethod:    "bedrock",
		Region:        "us-east-1",
		BedrockAPIKey: "test-key",
		Enabled:       true,
	}
}

func phase66F2FoundationJSON(ids ...string) string {
	type item struct {
		ModelID                 string   `json:"modelId"`
		OutputModalities        []string `json:"outputModalities"`
		InferenceTypesSupported []string `json:"inferenceTypesSupported"`
		ModelLifecycle          struct {
			Status string `json:"status"`
		} `json:"modelLifecycle"`
	}
	items := make([]item, 0, len(ids))
	for _, id := range ids {
		one := item{
			ModelID:                 id,
			OutputModalities:        []string{"TEXT"},
			InferenceTypesSupported: []string{"ON_DEMAND"},
		}
		one.ModelLifecycle.Status = "ACTIVE"
		items = append(items, one)
	}
	payload, _ := json.Marshal(map[string]any{"modelSummaries": items})
	return string(payload)
}

func phase66F2ClearLegacyBedrockState(id string) {
	clearBedrockModelCache(id)
}

func TestBedrockInferenceProfilesPaginationPublishesCompleteCatalog(t *testing.T) {
	account := phase66F2BedrockAccount("phase66f2-pagination")
	defer phase66F2ClearLegacyBedrockState(account.ID)

	previousClient := bedrockHTTPClientFor
	defer func() { bedrockHTTPClientFor = previousClient }()
	var calls atomic.Int32
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{Transport: phase66F2BedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			switch req.URL.Path {
			case "/foundation-models":
				return phase66F2BedrockResponse(req, http.StatusOK,
					phase66F2FoundationJSON("amazon.foundation-v1:0")), nil
			case "/inference-profiles":
				if got := req.URL.Query().Get("maxResults"); got != "100" {
					t.Fatalf("maxResults = %q, want 100", got)
				}
				switch req.URL.Query().Get("nextToken") {
				case "":
					return phase66F2BedrockResponse(req, http.StatusOK,
						`{"inferenceProfileSummaries":[{"inferenceProfileId":"us.profile-one-v1:0","status":"ACTIVE"}],"nextToken":"token/2"}`), nil
				case "token/2":
					return phase66F2BedrockResponse(req, http.StatusOK,
						`{"inferenceProfileSummaries":[{"inferenceProfileId":"us.profile-two-v1:0","status":"ACTIVE"}]}`), nil
				default:
					t.Fatalf("unexpected nextToken %q", req.URL.Query().Get("nextToken"))
				}
			}
			return phase66F2BedrockResponse(req, http.StatusNotFound, `{}`), nil
		})}
	}

	models, err := (&Handler{}).cachedOrDiscoverBedrockModelsResultContext(context.Background(), account)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	want := []string{"amazon.foundation-v1:0", "us.profile-one-v1:0", "us.profile-two-v1:0"}
	if len(models) != len(want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Fatalf("models = %v, want %v", models, want)
		}
	}
	firstCalls := calls.Load()
	if firstCalls != 3 {
		t.Fatalf("network calls = %d, want 3", firstCalls)
	}
	if _, err := (&Handler{}).cachedOrDiscoverBedrockModelsResultContext(context.Background(), account); err != nil {
		t.Fatalf("cached discovery: %v", err)
	}
	if got := calls.Load(); got != firstCalls {
		t.Fatalf("cached lookup made network call: before=%d after=%d", firstCalls, got)
	}
}

func TestBedrockDiscoveryCancellationDoesNotPoisonCache(t *testing.T) {
	account := phase66F2BedrockAccount("phase66f2-cancel-foundation")
	defer phase66F2ClearLegacyBedrockState(account.ID)
	previousClient := bedrockHTTPClientFor
	defer func() { bedrockHTTPClientFor = previousClient }()

	started := make(chan struct{})
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{Transport: phase66F2BedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			close(started)
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := (&Handler{}).cachedOrDiscoverBedrockModelsResultContext(ctx, account)
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("foundation request did not start")
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled discovery did not return promptly")
	}
	if models, cached, err := getCachedBedrockModelsResultForAccount(account); cached || err != nil || len(models) != 0 {
		t.Fatalf("cancellation published cache: models=%v cached=%v err=%v", models, cached, err)
	}

	var healthyCalls atomic.Int32
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{Transport: phase66F2BedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			healthyCalls.Add(1)
			if req.URL.Path == "/foundation-models" {
				return phase66F2BedrockResponse(req, http.StatusOK, phase66F2FoundationJSON("amazon.healthy-v1:0")), nil
			}
			return phase66F2BedrockResponse(req, http.StatusOK, `{"inferenceProfileSummaries":[]}`), nil
		})}
	}
	models, err := (&Handler{}).cachedOrDiscoverBedrockModelsResultContext(context.Background(), account)
	if err != nil || len(models) != 1 || models[0] != "amazon.healthy-v1:0" {
		t.Fatalf("healthy retry = models:%v err:%v", models, err)
	}
	if healthyCalls.Load() != 2 {
		t.Fatalf("healthy retry calls = %d, want 2", healthyCalls.Load())
	}
}

func TestBedrockProfileCancellationDoesNotPublishFoundationOnly(t *testing.T) {
	account := phase66F2BedrockAccount("phase66f2-cancel-profiles")
	defer phase66F2ClearLegacyBedrockState(account.ID)
	previousClient := bedrockHTTPClientFor
	defer func() { bedrockHTTPClientFor = previousClient }()

	profileStarted := make(chan struct{})
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{Transport: phase66F2BedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/foundation-models" {
				return phase66F2BedrockResponse(req, http.StatusOK, phase66F2FoundationJSON("amazon.foundation-v1:0")), nil
			}
			close(profileStarted)
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := (&Handler{}).cachedOrDiscoverBedrockModelsResultContext(ctx, account)
		errCh <- err
	}()
	select {
	case <-profileStarted:
	case <-time.After(time.Second):
		t.Fatal("profile request did not start")
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("profile cancellation did not return promptly")
	}
	if models, cached, err := getCachedBedrockModelsResultForAccount(account); cached || err != nil || len(models) != 0 {
		t.Fatalf("partial foundation result was published: models=%v cached=%v err=%v", models, cached, err)
	}
}

func TestBedrockProfileAccessDeniedIsPermissionLimitedSuccess(t *testing.T) {
	account := phase66F2BedrockAccount("phase66f2-profile-403")
	defer phase66F2ClearLegacyBedrockState(account.ID)
	previousClient := bedrockHTTPClientFor
	defer func() { bedrockHTTPClientFor = previousClient }()
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{Transport: phase66F2BedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/foundation-models" {
				return phase66F2BedrockResponse(req, http.StatusOK, phase66F2FoundationJSON("amazon.foundation-v1:0")), nil
			}
			return phase66F2BedrockResponse(req, http.StatusForbidden, `{"message":"denied"}`), nil
		})}
	}
	models, err := (&Handler{}).cachedOrDiscoverBedrockModelsResultContext(context.Background(), account)
	if err != nil || len(models) != 1 || models[0] != "amazon.foundation-v1:0" {
		t.Fatalf("permission-limited discovery = models:%v err:%v", models, err)
	}
	key := lifecycleOrLegacyKeyForAccount(account)
	bedrockDiscoveryMu.RLock()
	entry, ok := bedrockDiscoveryCache[key]
	bedrockDiscoveryMu.RUnlock()
	if !ok || entry.failure != "" {
		t.Fatalf("permission-limited entry = %+v ok=%v", entry, ok)
	}
	if remaining := time.Until(entry.expires); remaining <= 0 || remaining > bedrockDiscoveryNegativeTTL+time.Second {
		t.Fatalf("permission-limited TTL = %v, want <= %v", remaining, bedrockDiscoveryNegativeTTL)
	}
}

func TestBedrockRepeatedNextTokenIsNegativeFailureNotPartialSuccess(t *testing.T) {
	account := phase66F2BedrockAccount("phase66f2-repeat-token")
	defer phase66F2ClearLegacyBedrockState(account.ID)
	previousClient := bedrockHTTPClientFor
	defer func() { bedrockHTTPClientFor = previousClient }()
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{Transport: phase66F2BedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/foundation-models" {
				return phase66F2BedrockResponse(req, http.StatusOK, phase66F2FoundationJSON("amazon.foundation-v1:0")), nil
			}
			return phase66F2BedrockResponse(req, http.StatusOK,
				`{"inferenceProfileSummaries":[{"inferenceProfileId":"us.partial-v1:0","status":"ACTIVE"}],"nextToken":"repeat"}`), nil
		})}
	}
	models, err := (&Handler{}).cachedOrDiscoverBedrockModelsResultContext(context.Background(), account)
	if err == nil || !strings.Contains(err.Error(), "repeated inference profiles nextToken") || len(models) != 0 {
		t.Fatalf("repeated token result = models:%v err:%v", models, err)
	}
	cachedModels, cached, cachedErr := getCachedBedrockModelsResultForAccount(account)
	if !cached || cachedErr == nil || len(cachedModels) != 0 {
		t.Fatalf("negative cache = models:%v cached:%v err:%v", cachedModels, cached, cachedErr)
	}
}

func TestBedrockDiscoveryGateWaiterHonorsCancellation(t *testing.T) {
	account := phase66F2BedrockAccount("phase66f2-gate-cancel")
	defer phase66F2ClearLegacyBedrockState(account.ID)
	key := lifecycleOrLegacyKeyForAccount(account)
	leaderRelease, err := acquireBedrockDiscoveryGateContext(context.Background(), key)
	if err != nil {
		t.Fatalf("leader gate: %v", err)
	}
	defer leaderRelease()

	ctx, cancel := context.WithCancel(context.Background())
	waiterErr := make(chan error, 1)
	go func() {
		release, err := acquireBedrockDiscoveryGateContext(ctx, key)
		if release != nil {
			release()
		}
		waiterErr <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		bedrockDiscoveryGateMu.Lock()
		gate := bedrockDiscoveryGates[key]
		waiting := gate != nil && gate.refs >= 2
		bedrockDiscoveryGateMu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter did not register on discovery gate")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-waiterErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter remained blocked behind leader")
	}
}

func TestBedrockLifecycleClearSerializesWithInflightDiscovery(t *testing.T) {
	account := phase66F2BedrockAccount("phase66f2-clear-serialize")
	account.DispatchGeneration = 77
	key := lifecycleOrLegacyKeyForAccount(account)
	leaderRelease, err := acquireBedrockDiscoveryGateContext(context.Background(), key)
	if err != nil {
		t.Fatalf("leader gate: %v", err)
	}

	clearDone := make(chan error, 1)
	go func() {
		_, err := clearBedrockModelCacheForLifecycleContext(context.Background(), account)
		clearDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		bedrockDiscoveryGateMu.Lock()
		gate := bedrockDiscoveryGates[key]
		waiting := gate != nil && gate.refs >= 2
		bedrockDiscoveryGateMu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			leaderRelease()
			t.Fatal("clear did not serialize on discovery gate")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-clearDone:
		leaderRelease()
		t.Fatalf("clear escaped gate before leader release: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	leaderRelease()
	select {
	case err := <-clearDone:
		if err != nil {
			t.Fatalf("clear after leader release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("clear did not finish after leader release")
	}
}

func TestBedrockControlPlaneBodyBoundaryIsStrict(t *testing.T) {
	_, err := readBedrockControlPlaneBody(strings.NewReader(strings.Repeat("x", int(bedrockControlPlaneMaxResponseBytes)+1)))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized body error = %v", err)
	}
}
