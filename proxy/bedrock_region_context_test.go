package proxy

import (
	"context"
	"kiro-go/config"
	"net/http"
	"testing"
	"time"
)

type bedrockContextRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn bedrockContextRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestPrewarmBedrockRegionsContextCancelsCanary(t *testing.T) {
	account := &config.Account{
		ID:            "bedrock-context-cancel",
		AuthMethod:    "bedrock",
		Region:        "us-east-1",
		BedrockAPIKey: "ABSK-test",
	}
	setCachedBedrockModels(account.ID, []string{"model-for-cancel"})
	defer clearBedrockModelCache(account.ID)
	defer clearBedrockRegionRoutes(account.ID)

	requestStarted := make(chan struct{})
	previous := bedrockHTTPClientFor
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{Transport: bedrockContextRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			close(requestStarted)
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}
	}
	defer func() { bedrockHTTPClientFor = previous }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h := &Handler{}
		h.prewarmBedrockRegionsContext(ctx, account)
		close(done)
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("Bedrock canary did not reach test transport")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled Bedrock prewarm did not return promptly")
	}
}

func TestCanaryBedrockRegionContextHasInternalTimeout(t *testing.T) {
	account := &config.Account{
		ID:            "bedrock-canary-timeout",
		AuthMethod:    "bedrock",
		Region:        "us-east-1",
		BedrockAPIKey: "ABSK-test",
	}
	previousClient := bedrockHTTPClientFor
	previousTimeout := bedrockCanaryTimeout
	bedrockCanaryTimeout = 40 * time.Millisecond
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{Transport: bedrockContextRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}
	}
	t.Cleanup(func() {
		bedrockHTTPClientFor = previousClient
		bedrockCanaryTimeout = previousTimeout
	})

	started := time.Now()
	callable, reached := (&Handler{}).canaryBedrockRegionContext(context.Background(), account, "model-timeout", "us-east-1")
	if callable || reached {
		t.Fatalf("canary result = callable:%v reached:%v, want false/false on internal timeout", callable, reached)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canary internal timeout took too long: %v", elapsed)
	}
}
