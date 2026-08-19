package proxy

import (
	"context"
	"encoding/json"
	"io"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSetDirectAnthropicStaticAPIKeyClearsBearerMirror(t *testing.T) {
	account := &config.Account{
		AuthMethod:  "anthropic_api",
		KiroApiKey:  "old-static-key",
		AccessToken: "legacy-bearer-mirror",
		ExpiresAt:   12345,
	}

	if err := setDirectAnthropicStaticAPIKey(account, "  sk-ant-new  "); err != nil {
		t.Fatalf("setDirectAnthropicStaticAPIKey: %v", err)
	}
	if account.KiroApiKey != "sk-ant-new" {
		t.Fatalf("KiroApiKey=%q, want trimmed static key", account.KiroApiKey)
	}
	if account.AccessToken != "" {
		t.Fatalf("direct Anthropic static key remained mirrored in AccessToken: %q", account.AccessToken)
	}
	if account.ExpiresAt != 0 {
		t.Fatalf("static direct Anthropic credential retained bearer expiry: %d", account.ExpiresAt)
	}

	if err := setDirectAnthropicStaticAPIKey(account, "   "); err == nil {
		t.Fatal("expected empty direct Anthropic API key to be rejected")
	}
}

func TestKiroTransportRejectsDirectProviderCredentialsBeforeNetwork(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); strings.Contains(got, "sk-ant-secret") {
			t.Errorf("Anthropic secret crossed the Kiro trust boundary: %q", got)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	for _, account := range []*config.Account{
		{ID: "anthropic", AuthMethod: "anthropic_api", KiroApiKey: "sk-ant-secret", AccessToken: "sk-ant-secret", Enabled: true},
		{ID: "bedrock", AuthMethod: "bedrock", BedrockAccessKeyID: "AKIAEXAMPLE", BedrockSecretAccessKey: "secret", Enabled: true},
		{ID: "relay", AuthMethod: "custom_api", BaseURL: "https://relay.example", KiroApiKey: "relay-key", Enabled: true},
	} {
		err := CallKiroAPIContext(context.Background(), account, &KiroPayload{}, &KiroStreamCallback{})
		if err == nil {
			t.Fatalf("account kind %s unexpectedly entered Kiro transport", account.Kind())
		}
	}
	if requests != 0 {
		t.Fatalf("unsupported provider credentials caused %d Kiro network request(s)", requests)
	}
}

func TestResponsesTransportGateFailsClosedForDirectProviders(t *testing.T) {
	req := &ResponsesRequest{}
	plan := responsesToolChoicePlan{Kind: responsesToolChoiceAuto}

	tests := []struct {
		name string
		acct config.Account
		want responsesTransportKind
	}{
		{name: "kiro-oauth", acct: config.Account{AuthMethod: "social"}, want: responsesTransportKiro},
		{name: "kiro-api-key", acct: config.Account{AuthMethod: "api_key"}, want: responsesTransportKiro},
		{name: "relay", acct: config.Account{AuthMethod: "custom_api"}, want: responsesTransportCustomRelay},
		{name: "anthropic", acct: config.Account{AuthMethod: "anthropic_api"}, want: responsesTransportUnsupported},
		{name: "bedrock", acct: config.Account{AuthMethod: "bedrock"}, want: responsesTransportUnsupported},
		{name: "unknown", acct: config.Account{AuthMethod: "future_provider"}, want: responsesTransportUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := responsesTransportForAccount(&tt.acct, req, plan)
			if got != tt.want {
				t.Fatalf("transport=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestResponsesToolChoiceIsNotSilentlyDegraded(t *testing.T) {
	toolJSON := `{"type":"function","name":"lookup","description":"lookup","parameters":{"type":"object"}}`
	var tool OpenAITool
	if err := json.Unmarshal([]byte(toolJSON), &tool); err != nil {
		t.Fatal(err)
	}
	tools := []OpenAITool{tool}

	required, err := parseResponsesToolChoice(json.RawMessage(`"required"`), tools)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := responsesKiroCompatibility(&ResponsesRequest{Tools: tools}, required); ok {
		t.Fatal("required tool_choice silently degraded to Kiro auto mode")
	}

	t.Setenv("KIRO_NATIVE_TOOL_CHOICE", "always")
	if ok, reason := responsesKiroCompatibility(&ResponsesRequest{Tools: tools}, required); !ok {
		t.Fatalf("native always mode should preserve required semantics: %s", reason)
	}

	specific, err := parseResponsesToolChoice(json.RawMessage(`{"type":"function","name":"lookup"}`), tools)
	if err != nil {
		t.Fatal(err)
	}
	payload := OpenAIToKiro(&OpenAIRequest{Messages: []OpenAIMessage{{Role: "user", Content: "use a tool"}}, Tools: tools}, false)
	applyResponsesToolChoiceToKiroPayload(payload, &ResponsesRequest{Tools: tools}, specific)
	ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil {
		t.Fatal("specific tool_choice lost tool context")
	}
	encoded, _ := json.Marshal(ctx.ToolChoice)
	if !strings.Contains(string(encoded), `"lookup"`) {
		t.Fatalf("specific tool_choice not mapped to selected tool: %s", encoded)
	}

	falseValue := false
	serialReq := &ResponsesRequest{Tools: tools, ParallelToolCalls: &falseValue}
	if ok, reason := responsesKiroCompatibility(serialReq, required); !ok {
		t.Fatalf("parallel_tool_calls=false should use buffered serial emulation: %s", reason)
	}
	if !responsesRequiresSerialToolGuard(serialReq, required) {
		t.Fatal("parallel_tool_calls=false did not enable the buffered serial guard")
	}

	future, err := parseResponsesToolChoice(json.RawMessage(`{"type":"future_tool_mode","foo":true}`), tools)
	if err != nil {
		t.Fatal(err)
	}
	if future.Kind != responsesToolChoicePassthroughOnly {
		t.Fatalf("future tool_choice was reinterpreted instead of passthrough-only: %#v", future)
	}
}

func TestResponsesResponsePreservesTopLevelToolShape(t *testing.T) {
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(`{
		"model":"claude-sonnet-4.5",
		"input":"hi",
		"tools":[{"type":"function","name":"lookup","description":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}],
		"tool_choice":{"type":"function","name":"lookup"},
		"parallel_tool_calls":false
	}`), &req); err != nil {
		t.Fatal(err)
	}
	resp := buildResponsesObject("resp_test", "claude-sonnet-4.5", "ok", nil, 1, 1, &req)
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &root); err != nil {
		t.Fatal(err)
	}
	var tools []map[string]interface{}
	if err := json.Unmarshal(root["tools"], &tools); err != nil {
		t.Fatalf("decode response tools: %v body=%s", err, encoded)
	}
	if len(tools) != 1 || tools[0]["name"] != "lookup" {
		t.Fatalf("Responses tool shape was not preserved: %#v body=%s", tools, encoded)
	}
	if _, nested := tools[0]["function"]; nested {
		t.Fatalf("Responses tool was accidentally remarshalized as Chat Completions shape: %s", encoded)
	}
	if resp.ParallelToolCalls {
		t.Fatal("parallel_tool_calls=false was not reflected in response object")
	}
}

func TestResponsesSSEWriterSequenceAndSingleTerminal(t *testing.T) {
	rec := httptest.NewRecorder()
	writer, err := newResponsesSSEWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	writer.send("response.created", map[string]interface{}{"response": map[string]interface{}{"id": "resp_1"}})
	writer.send("response.in_progress", map[string]interface{}{"response": map[string]interface{}{"id": "resp_1"}})
	writer.terminalEvent("response.completed", map[string]interface{}{"response": map[string]interface{}{"id": "resp_1"}})
	writer.terminalEvent("response.failed", map[string]interface{}{"response": map[string]interface{}{"id": "resp_1"}})
	writer.send("response.output_text.delta", map[string]interface{}{"delta": "must-not-appear"})

	body, _ := io.ReadAll(rec.Body)
	text := string(body)
	if strings.Contains(text, "must-not-appear") || strings.Contains(text, "response.failed") {
		t.Fatalf("events were emitted after terminal event:\n%s", text)
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("Responses stream incorrectly used Chat Completions [DONE] sentinel:\n%s", text)
	}

	blocks := strings.Split(strings.TrimSpace(text), "\n\n")
	if len(blocks) != 3 {
		t.Fatalf("got %d SSE blocks, want 3:\n%s", len(blocks), text)
	}
	for i, block := range blocks {
		var dataLine string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				dataLine = strings.TrimPrefix(line, "data: ")
			}
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(dataLine), &event); err != nil {
			t.Fatalf("decode event %d: %v block=%s", i, err, block)
		}
		if got := int(event["sequence_number"].(float64)); got != i+1 {
			t.Fatalf("sequence_number=%d at event %d, want %d", got, i, i+1)
		}
	}
}

func TestResponsesLateCompletionCannotMutateRecreatedAccount(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "late old lifecycle",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-sonnet-4.5","input":"hold"}`))
		h.handleOpenAIResponses(rec, req)
	}()
	<-started

	if err := config.DeleteAccount("test-account"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{
		ID: "test-account", AuthMethod: "social", Enabled: true,
		AccessToken: "new-token", ProfileArn: "arn:aws:codewhisperer:profile/new",
	}); err != nil {
		t.Fatal(err)
	}
	h.pool.Reload()

	close(release)
	<-done
	h.pool.WaitForPendingStats()

	if rec.Code != http.StatusOK {
		t.Fatalf("late in-flight request should still finish for its caller, got %d body=%s", rec.Code, rec.Body.String())
	}
	account, ok := config.GetAccountByID("test-account")
	if !ok {
		t.Fatal("recreated account missing")
	}
	if account.AccessToken != "new-token" {
		t.Fatalf("old request overwrote recreated credentials: %q", account.AccessToken)
	}
	if account.RequestCount != 0 || account.TotalTokens != 0 || account.TotalCredits != 0 {
		t.Fatalf("old request contaminated recreated stats: requests=%d tokens=%d credits=%v", account.RequestCount, account.TotalTokens, account.TotalCredits)
	}
}

func TestResponsesLateCompletionCannotUndoStatsReset(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	if err := config.UpdateAccountStats("test-account", 5, 1, 50, 2.5, 111); err != nil {
		t.Fatal(err)
	}
	h.pool.Reload()
	h.pool.SetModelList("test-account", []string{"claude-sonnet-4.5"})
	h.rebuildEnabledModelUnion()

	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "late before reset",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-sonnet-4.5","input":"hold"}`))
		h.handleOpenAIResponses(rec, req)
	}()
	<-started

	// Lowering counters is a deliberate stats reset and advances only the stats
	// generation, not the account lifecycle generation.
	if err := config.UpdateAccountStats("test-account", 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	h.pool.Reload()

	close(release)
	<-done
	h.pool.WaitForPendingStats()

	account, ok := config.GetAccountByID("test-account")
	if !ok {
		t.Fatal("account missing after stats reset")
	}
	if account.RequestCount != 0 || account.ErrorCount != 0 || account.TotalTokens != 0 || account.TotalCredits != 0 {
		t.Fatalf("pre-reset response was admitted into the new stats generation: %#v", account)
	}
}

func TestDirectAnthropicAdminUpdateClearsBearerMirror(t *testing.T) {
	account := &config.Account{
		AuthMethod: "anthropic_api", BaseURL: "https://api.anthropic.com",
		KiroApiKey: "sk-ant-old", AccessToken: "sk-ant-old", ExpiresAt: 999,
	}
	if err := applyAdminAccountUpdates(account, map[string]interface{}{"kiroApiKey": " sk-ant-updated "}); err != nil {
		t.Fatal(err)
	}
	if account.KiroApiKey != "sk-ant-updated" || account.AccessToken != "" || account.ExpiresAt != 0 {
		t.Fatalf("direct Anthropic update did not preserve the trust boundary: %#v", account)
	}
}

func TestResponsesSemanticGateFailsClosedForUnmodeledFeatures(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "built-in web search", body: `{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`},
		{name: "mcp tool", body: `{"model":"m","input":"hi","tools":[{"type":"mcp","server_label":"docs"}]}`},
		{name: "strict function semantics", body: `{"model":"m","input":"hi","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"},"strict":true}]}`},
		{name: "structured text", body: `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"x","schema":{"type":"object"}}}}`},
		{name: "max tool calls", body: `{"model":"m","input":"hi","max_tool_calls":1}`},
		{name: "stream options", body: `{"model":"m","input":"hi","stream":true,"stream_options":{"include_usage":true}}`},
		{name: "unknown thinking semantics", body: `{"model":"m","input":"hi","thinking":{"type":"enabled","budget_tokens":1024,"future_option":true}}`},
		{name: "unknown output config semantics", body: `{"model":"m","input":"hi","output_config":{"effort":"high","future_option":true}}`},
		{name: "future top-level field", body: `{"model":"m","input":"hi","future_behavior":{"enabled":true}}`},
		{name: "future input item", body: `{"model":"m","input":[{"type":"future_input_item","payload":"x"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ResponsesRequest{}
			if err := json.Unmarshal([]byte(tt.body), &req); err != nil {
				t.Fatal(err)
			}
			plan, err := parseResponsesToolChoice(req.ToolChoice, req.Tools)
			if err != nil {
				t.Fatal(err)
			}
			requirements := analyzeResponsesSemantics([]byte(tt.body))
			if requirements.KiroSupported() {
				t.Fatalf("unmodeled Responses semantics were accepted by Kiro: %s", tt.body)
			}
			if got, _ := responsesTransportForAccount(&config.Account{AuthMethod: "social"}, &req, plan, requirements); got != responsesTransportUnsupported {
				t.Fatalf("Kiro transport accepted unmodeled semantics: %v", got)
			}
			if got, _ := responsesTransportForAccount(&config.Account{AuthMethod: "custom_api"}, &req, plan, requirements); got != responsesTransportCustomRelay {
				t.Fatalf("transparent relay was incorrectly rejected: %v", got)
			}
		})
	}

	supported := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"lookup","description":"lookup","parameters":{"type":"object"}}],"reasoning":{"effort":"high"},"thinking":{"type":"enabled","budget_tokens":1024,"display":"summarized"},"output_config":{"effort":"high"}}`)
	if got := analyzeResponsesSemantics(supported); !got.KiroSupported() {
		t.Fatalf("supported Kiro subset rejected: %s", got.Reason())
	}
}

func TestOpenAIToolNamesUseOneCanonicalRoundTripCatalog(t *testing.T) {
	tools := []OpenAITool{
		mustTool(t, `{"type":"function","name":"get_weather","parameters":{"type":"object"}}`),
		mustTool(t, `{"type":"function","name":"get-weather","parameters":{"type":"object"}}`),
	}
	catalog := buildOpenAIToolNameCatalog(tools)
	underscore := catalog.kiroName("get_weather")
	dash := catalog.kiroName("get-weather")
	if underscore == "get_weather" || dash == "get-weather" {
		t.Fatalf("OpenAI tool names were not canonicalized for Kiro: %q %q", underscore, dash)
	}
	if underscore == dash {
		t.Fatalf("canonicalization collision was not disambiguated: %q", underscore)
	}
	if catalog.KiroToOriginal[underscore] != "get_weather" || catalog.KiroToOriginal[dash] != "get-weather" {
		t.Fatalf("reverse tool-name mapping is incomplete: %#v", catalog.KiroToOriginal)
	}

	payload := OpenAIToKiro(&OpenAIRequest{
		Messages: []OpenAIMessage{{Role: "user", Content: "use a tool"}},
		Tools:    tools[:1],
	}, false)
	ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.Tools) != 1 {
		t.Fatalf("missing converted tool declaration: %#v", ctx)
	}
	declared := ctx.Tools[0].ToolSpecification.Name
	plan := responsesToolChoicePlan{Kind: responsesToolChoiceSpecific, ToolName: "get_weather"}
	applyResponsesToolChoiceToKiroPayload(payload, &ResponsesRequest{Tools: tools[:1]}, plan)
	encoded, _ := json.Marshal(ctx.ToolChoice)
	if !strings.Contains(string(encoded), `"`+declared+`"`) {
		t.Fatalf("specific tool_choice differs from declaration: declaration=%q choice=%s", declared, encoded)
	}
	if payload.ToolNameMap[declared] != "get_weather" {
		t.Fatalf("payload cannot restore original client tool name: %#v", payload.ToolNameMap)
	}
}

func TestOpenAIToolNameTruncationCollisionIsDisambiguated(t *testing.T) {
	prefix := strings.Repeat("a", 70)
	tools := []OpenAITool{
		mustTool(t, `{"type":"function","name":"`+prefix+`A","parameters":{"type":"object"}}`),
		mustTool(t, `{"type":"function","name":"`+prefix+`B","parameters":{"type":"object"}}`),
	}
	catalog := buildOpenAIToolNameCatalog(tools)
	a := catalog.kiroName(prefix + "A")
	b := catalog.kiroName(prefix + "B")
	if a == b {
		t.Fatalf("64-byte truncation collision was not disambiguated: %q", a)
	}
	if len(a) > 64 || len(b) > 64 {
		t.Fatalf("disambiguated Kiro tool name exceeded 64 bytes: %d %d", len(a), len(b))
	}
	if catalog.KiroToOriginal[a] != prefix+"A" || catalog.KiroToOriginal[b] != prefix+"B" {
		t.Fatal("truncated tool names did not round-trip to originals")
	}
}

func TestResponsesCustomRelayDoesNotResolveLocalPreviousResponseState(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatal(err)
	}
	account := config.Account{
		ID:         "responses-relay",
		AuthMethod: "custom_api",
		BaseURL:    "https://relay.example",
		KiroApiKey: "relay-key",
		Enabled:    true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatal(err)
	}
	p := accountpool.GetPool()
	p.Reload()
	defer p.WaitForPendingStats()

	now := time.Now()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		accountModelSnapshots: testRuntimeSnapshots(map[string]accountModelSnapshot{
			account.ID: {
				Models:      []ModelInfo{{ModelId: "relay-model"}},
				RefreshedAt: now,
				FreshUntil:  now.Add(time.Hour),
				StaleUntil:  now.Add(time.Hour),
			},
		}),
	}
	p.SetModelList(account.ID, []string{"relay-model"})
	h.rebuildEnabledModelUnion()

	originalForward := forwardUpstreamRequest
	defer func() { forwardUpstreamRequest = originalForward }()
	var forwardedBody []byte
	forwardUpstreamRequest = func(_ context.Context, method, url, apiKey string, body []byte, stream bool, _ http.Header) (*http.Response, error) {
		forwardedBody = append([]byte(nil), body...)
		header := make(http.Header)
		responseBody := `{"id":"resp_upstream_2","object":"response","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`
		if stream {
			header.Set("Content-Type", "text/event-stream")
			responseBody = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_upstream_2\"}}\n\n"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(responseBody)),
		}, nil
	}

	rawBody := []byte(`{"model":"relay-model","input":"turn two","previous_response_id":"resp_upstream_1","store":false}`)
	var req ResponsesRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		t.Fatal(err)
	}
	plan, err := parseResponsesToolChoice(req.ToolChoice, req.Tools)
	if err != nil {
		t.Fatal(err)
	}
	semantics := analyzeResponsesSemantics(rawBody)
	prepareCalls := 0
	prepare := func() (*responsesKiroPrepared, error) {
		prepareCalls++
		return nil, &responsesPrepareError{Status: http.StatusNotFound, Message: "local state must not be consulted"}
	}

	rec := httptest.NewRecorder()
	h.handleResponsesNonStream(rec, "relay-model", "", "resp_local_unused", &req, plan, responseStateExternal, semantics, prepare, rawBody, false, context.Background())
	if rec.Code != http.StatusOK {
		t.Fatalf("relay response code=%d body=%s", rec.Code, rec.Body.String())
	}
	if prepareCalls != 0 {
		t.Fatalf("custom relay consulted local Responses state %d time(s)", prepareCalls)
	}
	if !strings.Contains(string(forwardedBody), `"previous_response_id":"resp_upstream_1"`) {
		t.Fatalf("upstream previous_response_id was not forwarded raw: %s", forwardedBody)
	}
}

func TestResponsesCustomRelayStreamDoesNotResolveLocalPreviousResponseState(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatal(err)
	}
	account := config.Account{
		ID:         "responses-relay-stream",
		AuthMethod: "custom_api",
		BaseURL:    "https://relay.example",
		KiroApiKey: "relay-key",
		Enabled:    true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatal(err)
	}
	p := accountpool.GetPool()
	p.Reload()
	defer p.WaitForPendingStats()

	now := time.Now()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		accountModelSnapshots: testRuntimeSnapshots(map[string]accountModelSnapshot{
			account.ID: {
				Models:      []ModelInfo{{ModelId: "relay-model"}},
				RefreshedAt: now,
				FreshUntil:  now.Add(time.Hour),
				StaleUntil:  now.Add(time.Hour),
			},
		}),
	}
	p.SetModelList(account.ID, []string{"relay-model"})
	h.rebuildEnabledModelUnion()

	originalForward := forwardUpstreamRequest
	defer func() { forwardUpstreamRequest = originalForward }()
	var forwardedBody []byte
	forwardUpstreamRequest = func(_ context.Context, method, url, apiKey string, body []byte, stream bool, _ http.Header) (*http.Response, error) {
		if !stream {
			t.Fatal("stream relay was forwarded as non-streaming")
		}
		forwardedBody = append([]byte(nil), body...)
		header := make(http.Header)
		header.Set("Content-Type", "text/event-stream")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     header,
			Body: io.NopCloser(strings.NewReader(
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_upstream_2\"}}\n\n",
			)),
		}, nil
	}

	rawBody := []byte(`{"model":"relay-model","input":"turn two","stream":true,"previous_response_id":"resp_upstream_1","store":false}`)
	req, err := parseResponsesRoutingEnvelope(rawBody)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := parseResponsesToolChoice(req.ToolChoice, req.Tools)
	if err != nil {
		t.Fatal(err)
	}
	semantics := analyzeResponsesSemantics(rawBody)
	prepareCalls := 0
	prepare := func() (*responsesKiroPrepared, error) {
		prepareCalls++
		return nil, &responsesPrepareError{Status: http.StatusNotFound, Message: "local state must not be consulted"}
	}

	rec := httptest.NewRecorder()
	h.handleResponsesStream(rec, "relay-model", "", "resp_local_unused", req, plan, responseStateExternal, semantics, prepare, rawBody, false, context.Background())
	if rec.Code != http.StatusOK {
		t.Fatalf("relay stream response code=%d body=%s", rec.Code, rec.Body.String())
	}
	if prepareCalls != 0 {
		t.Fatalf("custom relay stream consulted local Responses state %d time(s)", prepareCalls)
	}
	if !strings.Contains(string(forwardedBody), `"previous_response_id":"resp_upstream_1"`) {
		t.Fatalf("upstream stream previous_response_id was not forwarded: %s", forwardedBody)
	}
	if !strings.Contains(rec.Body.String(), "resp_upstream_2") {
		t.Fatalf("upstream SSE was not relayed: %s", rec.Body.String())
	}
}

func TestResponsesReasoningTextOutputIsPreserved(t *testing.T) {
	req := &ResponsesRequest{Model: "reasoning-model"}
	resp := buildResponsesObjectWithReasoning(
		"resp_test",
		"reasoning-model",
		"final answer",
		"checked the constraints",
		nil,
		12,
		9,
		req,
	)
	if len(resp.Output) != 2 {
		t.Fatalf("output items=%d, want reasoning + message: %#v", len(resp.Output), resp.Output)
	}
	if got := resp.Output[0]; got.Type != "reasoning" || len(got.Summary) != 0 || len(got.Content) != 1 || got.Content[0].Type != "reasoning_text" || got.Content[0].Text != "checked the constraints" {
		t.Fatalf("reasoning item was not preserved as Responses reasoning text: %#v", got)
	}
	if got := resp.Output[1]; got.Type != "message" || joinTextParts(got.Content) != "final answer" {
		t.Fatalf("message item changed while adding reasoning: %#v", got)
	}

	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, `"type":"reasoning"`) || !strings.Contains(text, `"type":"reasoning_text"`) || !strings.Contains(text, `"summary":[]`) {
		t.Fatalf("reasoning text wire shape missing: %s", text)
	}
	if strings.Contains(text, `"type":"reasoning_text","text":"checked the constraints","annotations"`) {
		t.Fatalf("message-only annotations leaked into reasoning_text: %s", text)
	}
}

func TestResponsesReasoningHistoryReplayIsExplicitlyEmulated(t *testing.T) {
	prev := &ResponsesObject{
		ID:          "resp_previous",
		StoredInput: json.RawMessage(`"first turn"`),
		Output: []ResponseOutputItem{
			responseReasoningOutputItem("rs_previous", "private-state summary"),
			responseMessageOutputItem("msg_previous", "answer"),
		},
	}

	messages, projection, err := expandPreviousResponseHistoryWithProjection(prev)
	if err != nil {
		t.Fatal(err)
	}
	if !projection.Emulated() || !projection.Supported() {
		t.Fatalf("reasoning continuation fidelity=%v issues=%#v, want supported Emulated", projection.Fidelity, projection.Issues)
	}
	for _, message := range messages {
		if strings.Contains(extractOpenAIMessageText(message.Content), "private-state summary") {
			t.Fatalf("reasoning text was replayed as ordinary conversational text: %#v", messages)
		}
	}
	if len(messages) != 2 || messages[0].Role != "user" || messages[1].Role != "assistant" {
		t.Fatalf("ordinary stored input/output was not preserved around reasoning item: %#v", messages)
	}
}

func TestPhase64HandleResponsesKeepsFutureShapesEligibleForCustomRelay(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatal(err)
	}
	account := config.Account{
		ID:         "responses-future-relay",
		AuthMethod: "custom_api",
		BaseURL:    "https://relay.example",
		KiroApiKey: "relay-key",
		Enabled:    true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatal(err)
	}
	p := accountpool.GetPool()
	p.Reload()
	defer p.WaitForPendingStats()

	now := time.Now()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		accountModelSnapshots: testRuntimeSnapshots(map[string]accountModelSnapshot{
			account.ID: {
				Models:      []ModelInfo{{ModelId: "relay-model"}},
				RefreshedAt: now,
				FreshUntil:  now.Add(time.Hour),
				StaleUntil:  now.Add(time.Hour),
			},
		}),
	}
	p.SetModelList(account.ID, []string{"relay-model"})
	h.rebuildEnabledModelUnion()

	originalForward := forwardUpstreamRequest
	defer func() { forwardUpstreamRequest = originalForward }()
	var forwardedBody []byte
	forwardUpstreamRequest = func(_ context.Context, method, url, apiKey string, body []byte, stream bool, _ http.Header) (*http.Response, error) {
		if stream {
			t.Fatal("future-shape non-stream request forwarded as streaming")
		}
		forwardedBody = append([]byte(nil), body...)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"id":"resp_future","object":"response","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`,
			)),
		}, nil
	}

	body := `{
		"model":"relay-model",
		"instructions":[{"type":"future_instruction","text":"provider-specific"}],
		"input":[
			{"type":"additional_tools","tools":[
				{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec","future_schema":{"opaque":true}}]},
				{"type":"future_hosted_tool","name":"provider_search"}
			]},
			{"type":"future_input_item","provider_payload":{"opaque":true}}
		],
		"tool_choice":{"type":"function","name":"provider_search"},
		"parallel_tool_calls":false,
		"future_top_level":{"opaque":true}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("future-shape custom relay status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wantJSON, gotJSON interface{}
	if err := json.Unmarshal([]byte(body), &wantJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(forwardedBody, &gotJSON); err != nil {
		t.Fatalf("forwarded body is not valid JSON: %v body=%s", err, forwardedBody)
	}
	wantEncoded, _ := json.Marshal(wantJSON)
	gotEncoded, _ := json.Marshal(gotJSON)
	if string(gotEncoded) != string(wantEncoded) {
		t.Fatalf("custom relay did not preserve the future request semantics\nwant: %s\n got: %s", wantEncoded, gotEncoded)
	}
}
