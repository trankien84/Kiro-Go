package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

func TestClaudeRequestPlanRejectsObjectMessageContent(t *testing.T) {
	body := []byte(`{"model":"private-model","max_tokens":64,"messages":[{"role":"user","content":{"type":"text","text":"hi"}}]}`)
	_, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr == nil {
		t.Fatal("expected object message content to be rejected")
	}
	if !strings.Contains(validationErr.Error(), "must be a string or an array of content blocks") {
		t.Fatalf("unexpected validation error: %v", validationErr)
	}
}

func TestClaudeRequestPlanRejectsObjectToolResultContent(t *testing.T) {
	body := []byte(`{"model":"private-model","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":{"type":"text","text":"result"}}]}]}`)
	_, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr == nil {
		t.Fatal("expected object tool_result content to be rejected")
	}
	if !strings.Contains(validationErr.Error(), "tool_result") &&
		!strings.Contains(validationErr.Error(), "must be a string or an array of content blocks") {
		t.Fatalf("unexpected validation error: %v", validationErr)
	}
}

func TestClaudeRequestPlanRejectsNonCanonicalKnownBlockType(t *testing.T) {
	body := []byte(`{"model":"private-model","max_tokens":64,"messages":[{"role":"user","content":[{"type":"Text","text":"hi"}]}]}`)
	_, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr == nil {
		t.Fatal("expected non-canonical known block type to be rejected")
	}
	if !strings.Contains(validationErr.Error(), "canonical lowercase discriminator text") {
		t.Fatalf("unexpected validation error: %v", validationErr)
	}
}

func TestNormalizeDirectAnthropicBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "origin", raw: "https://api.anthropic.com", want: "https://api.anthropic.com"},
		{name: "v1 suffix", raw: "https://provider.example/anthropic/v1/", want: "https://provider.example/anthropic"},
		{name: "endpoint", raw: "https://provider.example/v1/messages", wantErr: true},
		{name: "query", raw: "https://provider.example/v1?tenant=x", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeDirectAnthropicBaseURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}

	endpoint, err := directAnthropicEndpointURL("https://provider.example/v1", "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://provider.example/v1/messages" {
		t.Fatalf("endpoint=%q", endpoint)
	}
}

func TestProbeDirectAnthropicModelsPaginates(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/models" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if got := r.URL.Query().Get("limit"); got != "1000" {
			t.Errorf("limit=%q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version=%q", got)
		}
		if got := r.Header.Get("x-api-key"); got != "sk-direct" {
			t.Errorf("x-api-key=%q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("direct model probe leaked Authorization=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after_id") == "" {
			io.WriteString(w, `{"data":[{"id":"claude-a","display_name":"Claude A","max_input_tokens":200000,"max_tokens":64000,"capabilities":{"thinking":{"supported":true,"types":{"adaptive":{"supported":true}}},"effort":{"supported":true,"high":{"supported":true}},"image_input":{"supported":true}}}],"has_more":true,"first_id":"claude-a","last_id":"claude-a"}`)
			return
		}
		if got := r.URL.Query().Get("after_id"); got != "claude-a" {
			t.Errorf("after_id=%q", got)
		}
		io.WriteString(w, `{"data":[{"id":"claude-b","display_name":"Claude B"}],"has_more":false,"first_id":"claude-b","last_id":"claude-b"}`)
	}))
	defer server.Close()

	models, err := probeDirectAnthropicModels(context.Background(), server.URL+"/v1", "sk-direct")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
	if len(models) != 2 || models[0].ModelId != "claude-a" || models[1].ModelId != "claude-b" {
		t.Fatalf("models=%#v", models)
	}
	if models[0].ModelName != "Claude A" || models[1].ModelName != "Claude B" {
		t.Fatalf("display names=%#v", models)
	}
	if !modelSupportsImage(models[0].InputTypes) || modelSupportsImage(models[1].InputTypes) {
		t.Fatalf("input types=%#v", models)
	}

	_, metadata, err := probeDirectAnthropicModelsDetailed(context.Background(), server.URL+"/v1", "sk-direct")
	if err != nil {
		t.Fatal(err)
	}
	meta := metadata["claude-a"]
	if meta.MaxInputTokens != 200000 || !meta.CapabilitiesPresent || !meta.Capabilities.Thinking.Supported || !meta.Capabilities.Effort.High.Supported {
		t.Fatalf("metadata=%#v", meta)
	}
}

func TestProbeDirectAnthropicModelsPreservesCaseSensitiveProviderIDsAndMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"Model-A","capabilities":{"thinking":{"supported":true,"types":{"adaptive":{"supported":true}}}}},{"id":"model-a","capabilities":{"thinking":{"supported":false}}}],"has_more":false}`)
	}))
	defer server.Close()

	models, metadata, err := probeDirectAnthropicModelsDetailed(context.Background(), server.URL, "sk-direct")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ModelId != "Model-A" || models[1].ModelId != "model-a" {
		t.Fatalf("case-sensitive provider IDs collapsed: %#v", models)
	}
	upper, upperOK := metadata["Model-A"]
	lower, lowerOK := metadata["model-a"]
	if !upperOK || !lowerOK {
		t.Fatalf("exact metadata missing: %#v", metadata)
	}
	if !upper.Capabilities.Thinking.Supported || lower.Capabilities.Thinking.Supported {
		t.Fatalf("capabilities crossed exact identities: upper=%#v lower=%#v", upper, lower)
	}
}

func TestProbeDirectAnthropicModelsRejectsStalledCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[],"has_more":true,"last_id":"same"}`)
	}))
	defer server.Close()

	_, err := probeDirectAnthropicModels(context.Background(), server.URL, "sk-direct")
	if err == nil || !strings.Contains(err.Error(), "cursor did not advance") {
		t.Fatalf("expected stalled cursor error, got %v", err)
	}
}

func TestDirectAnthropicResponseErrorPolicy(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		errorType string
		retryable bool
		scope     directAnthropicErrorScope
	}{
		{name: "bad request", status: http.StatusBadRequest, errorType: "invalid_request_error", scope: directAnthropicErrorRequest},
		{name: "credential", status: http.StatusUnauthorized, errorType: "authentication_error", retryable: true, scope: directAnthropicErrorCredential},
		{name: "billing", status: http.StatusPaymentRequired, errorType: "billing_error", retryable: true, scope: directAnthropicErrorBilling},
		{name: "permission is resource scoped", status: http.StatusForbidden, errorType: "permission_error", retryable: true, scope: directAnthropicErrorPermission},
		{name: "not found is resource scoped", status: http.StatusNotFound, errorType: "not_found_error", retryable: true, scope: directAnthropicErrorNotFound},
		{name: "rate limit", status: http.StatusTooManyRequests, errorType: "rate_limit_error", retryable: true, scope: directAnthropicErrorRateLimit},
		{name: "server", status: http.StatusInternalServerError, errorType: "api_error", retryable: true, scope: directAnthropicErrorTransient},
		{name: "overloaded", status: 529, errorType: "overloaded_error", retryable: true, scope: directAnthropicErrorTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responseErr := newDirectAnthropicResponseError(&http.Response{
				StatusCode: tt.status,
				Header:     make(http.Header),
			}, []byte(`{"type":"error","error":{"type":"`+tt.errorType+`","message":"test"},"request_id":"req_test"}`))
			if responseErr.Retryable != tt.retryable || responseErr.Scope != tt.scope {
				t.Fatalf("status=%d retryable=%v scope=%v", tt.status, responseErr.Retryable, responseErr.Scope)
			}
			if responseErr.ErrorType != tt.errorType || responseErr.RequestID != "req_test" {
				t.Fatalf("parsed error=%#v", responseErr)
			}
		})
	}
}

func TestDirectAnthropicRetryAfterParsing(t *testing.T) {
	seconds := newDirectAnthropicResponseError(&http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"17"}},
	}, []byte(`{"error":{"type":"rate_limit_error"}}`))
	if seconds.RetryAfter != 17*time.Second {
		t.Fatalf("seconds RetryAfter=%s", seconds.RetryAfter)
	}

	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	date := newDirectAnthropicResponseError(&http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{future}},
	}, []byte(`{"error":{"type":"rate_limit_error"}}`))
	if date.RetryAfter < 28*time.Second || date.RetryAfter > 31*time.Second {
		t.Fatalf("date RetryAfter=%s", date.RetryAfter)
	}
}

func TestDirectAnthropicRetryAfterIsBounded(t *testing.T) {
	header := make(http.Header)
	header.Set("Retry-After", "9223372036854775807")
	if got := parseRetryAfter(header, time.Now()); got != 24*time.Hour {
		t.Fatalf("bounded retry-after=%s", got)
	}
}

func TestForwardToUpstreamPreservesDirectAnthropicError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%q", r.Method)
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "sk-direct" {
			t.Errorf("x-api-key=%q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("direct request leaked Authorization=%q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_test")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "0")
		w.Header().Set("Anthropic-Ratelimit-Future-Bucket", "future")
		w.Header().Set("X-Internal-Secret", "must-not-forward")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"},"request_id":"req_test"}`)
	}))
	defer server.Close()

	handler := &Handler{}
	recorder := httptest.NewRecorder()
	err := handler.forwardToUpstream(recorder, nil, forwardParams{
		account: &config.Account{
			ID:         "anthropic-1",
			AuthMethod: "anthropic_api",
			BaseURL:    server.URL + "/v1",
			KiroApiKey: "sk-direct",
		},
		body:            []byte(`{"model":"claude","messages":[]}`),
		endpoint:        "anthropic",
		requestContext:  context.Background(),
		semanticHeaders: make(http.Header),
	})
	var responseErr *directAnthropicResponseError
	if !errors.As(err, &responseErr) {
		t.Fatalf("expected directAnthropicResponseError, got %T: %v", err, err)
	}
	if responseErr.StatusCode != http.StatusBadRequest || responseErr.Retryable {
		t.Fatalf("responseErr=%#v", responseErr)
	}

	writeDirectAnthropicResponseError(recorder, responseErr)
	result := recorder.Result()
	defer result.Body.Close()
	if result.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", result.StatusCode)
	}
	if result.Header.Get("Request-Id") != "req_test" {
		t.Fatalf("request-id=%q", result.Header.Get("Request-Id"))
	}
	if result.Header.Get("X-Internal-Secret") != "" {
		t.Fatalf("internal header leaked: %q", result.Header.Get("X-Internal-Secret"))
	}
	if !strings.Contains(strings.ToLower(result.Header.Get("Access-Control-Expose-Headers")),
		"anthropic-ratelimit-future-bucket") {
		t.Fatalf("future rate-limit header was not exposed to CORS: %q", result.Header.Get("Access-Control-Expose-Headers"))
	}
	responseBody, _ := io.ReadAll(result.Body)
	if !strings.Contains(string(responseBody), "invalid_request_error") {
		t.Fatalf("body=%s", responseBody)
	}
}

func TestProbeDirectAnthropicModelsHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := probeDirectAnthropicModels(ctx, server.URL, "sk-direct")
		done <- err
	}()
	<-started
	cancel()

	select {
	case err := <-done:
		if err == nil || (!errors.Is(err, context.Canceled) && !strings.Contains(strings.ToLower(err.Error()), "canceled")) {
			t.Fatalf("cancellation error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("model probe ignored context cancellation")
	}
}

func TestProbeDirectAnthropicModelsReturnsTypedProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_models")
		w.Header().Set("Retry-After", "11")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"},"request_id":"req_models"}`)
	}))
	defer server.Close()

	_, err := probeDirectAnthropicModels(context.Background(), server.URL, "sk-direct")
	var responseErr *directAnthropicResponseError
	if !errors.As(err, &responseErr) {
		t.Fatalf("error=%T %v", err, err)
	}
	if responseErr.Scope != directAnthropicErrorRateLimit || responseErr.RetryAfter != 11*time.Second {
		t.Fatalf("response error=%#v", responseErr)
	}
	if responseErr.RequestID != "req_models" || responseErr.Header.Get("Request-Id") != "req_models" {
		t.Fatalf("request id/header=%#v", responseErr)
	}
}

func TestDirectAnthropicTestReplyReturnsTypedProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_test_account")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"permission_error","message":"model denied"},"request_id":"req_test_account"}`)
	}))
	defer server.Close()

	_, err := directAnthropicTestReply(
		context.Background(),
		server.URL,
		"sk-direct",
		"claude-private",
	)
	var responseErr *directAnthropicResponseError
	if !errors.As(err, &responseErr) {
		t.Fatalf("error=%T %v", err, err)
	}
	if responseErr.Scope != directAnthropicErrorPermission || responseErr.RequestID != "req_test_account" {
		t.Fatalf("response error=%#v", responseErr)
	}
}
