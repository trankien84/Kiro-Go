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

func TestResponsesParseStringInput(t *testing.T) {
	raw := json.RawMessage(`"hello world"`)
	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse string input: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Fatalf("expected user role, got %q", msgs[0].Role)
	}
	if got, _ := msgs[0].Content.(string); got != "hello world" {
		t.Fatalf("expected hello world, got %v", msgs[0].Content)
	}
}

func TestResponsesParseArrayInput(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},
		{"type":"input_text","text":"loose part"},
		{"type":"function_call_output","call_id":"call_1","output":"42"}
	]`)
	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse array input: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d (msgs=%+v)", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Fatalf("expected first message user, got %q", msgs[0].Role)
	}
	if got, _ := msgs[0].Content.(string); got != "first" {
		t.Fatalf("expected first text, got %v", msgs[0].Content)
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "call_1" {
		t.Fatalf("expected tool result with call_id call_1, got %+v", msgs[2])
	}
	if got, _ := msgs[2].Content.(string); got != "42" {
		t.Fatalf("expected tool output 42, got %v", msgs[2].Content)
	}
}

func TestResponsesStoreAndLoad(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	resp := &ResponsesObject{
		ID:        "resp_unit_test_001",
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "completed",
		Model:     "claude-sonnet-4.5",
		Output: []ResponseOutputItem{{
			ID:     "msg_x",
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: "stored hello",
			}},
		}},
		StoredInput: json.RawMessage(`"hi"`),
	}

	if err := saveResponse(resp); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadResponse(resp.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.ID != resp.ID || loaded.Model != resp.Model {
		t.Fatalf("loaded mismatch: %+v", loaded)
	}
	if len(loaded.Output) != 1 || loaded.Output[0].Content[0].Text != "stored hello" {
		t.Fatalf("loaded output mismatch: %+v", loaded.Output)
	}
	if string(loaded.StoredInput) != `"hi"` {
		t.Fatalf("stored input mismatch: %s", string(loaded.StoredInput))
	}

	if _, err := loadResponse("does_not_exist"); err == nil {
		t.Fatalf("expected load error for missing id")
	}
}

func TestResponsesNonStreamPreservesUpstreamIncompleteStatus(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "partial answer",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "MAX_TOKENS",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"hello"
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var response ResponsesObject
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if response.Status != "incomplete" {
		t.Fatalf("status=%q, want incomplete", response.Status)
	}
	if response.IncompleteDetails == nil || response.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("incomplete_details=%#v, want max_output_tokens", response.IncompleteDetails)
	}
}

func TestMapResponsesCompletion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		status string
		detail string
	}{
		{name: "max tokens", reason: "MAX_TOKENS", status: "incomplete", detail: "max_output_tokens"},
		{name: "max output tokens alias", reason: "max_output_tokens", status: "incomplete", detail: "max_output_tokens"},
		{name: "length alias", reason: "length", status: "incomplete", detail: "max_output_tokens"},
		{name: "context limit", reason: "CONTEXT_WINDOW_EXCEEDED", status: "incomplete", detail: "max_output_tokens"},
		{name: "model context limit alias", reason: "model_context_window_exceeded", status: "incomplete", detail: "max_output_tokens"},
		{name: "content filter", reason: "CONTENT_FILTERED", status: "incomplete", detail: "content_filter"},
		{name: "refusal alias", reason: "refusal", status: "incomplete", detail: "content_filter"},
		{name: "guardrail alias", reason: "guardrail_intervened", status: "incomplete", detail: "content_filter"},
		// END_TURN is the literal the Kiro IDE bundle compares against.
		{name: "end turn", reason: "END_TURN", status: "completed"},
		// Absent metadataEvent: reported as completed here on purpose. Detecting
		// that as truncation is the stream integrity layer's job, not this map's.
		{name: "empty reports completed", reason: "", status: "completed"},
		{name: "normal completion", reason: "COMPLETE", status: "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, detail := mapResponsesCompletion(tc.reason)
			if status != tc.status || detail != tc.detail {
				t.Fatalf("mapResponsesCompletion(%q) = (%q, %q), want (%q, %q)", tc.reason, status, detail, tc.status, tc.detail)
			}
		})
	}
}

func TestResponsesPreviousResponseIDExpands(t *testing.T) {
	prev := &ResponsesObject{
		ID:          "resp_prev",
		StoredInput: json.RawMessage(`"earlier user"`),
		Output: []ResponseOutputItem{
			{
				Type: "message",
				Role: "assistant",
				Content: []ResponseContentPart{{
					Type: "output_text",
					Text: "earlier assistant reply",
				}},
			},
			{
				Type:      "function_call",
				CallID:    "call_prev",
				Name:      "lookup",
				Arguments: `{"q":"x"}`,
			},
		},
	}

	expanded, err := expandPreviousResponseHistory(prev)
	if err != nil {
		t.Fatalf("expand history: %v", err)
	}
	if len(expanded) != 2 {
		t.Fatalf("expected user + one semantic assistant turn, got %d (%+v)", len(expanded), expanded)
	}
	if expanded[0].Role != "user" {
		t.Fatalf("expected first message to be user, got %+v", expanded[0])
	}
	if expanded[1].Role != "assistant" {
		t.Fatalf("expected second message to be assistant, got %+v", expanded[1])
	}
	if expanded[1].Role != "assistant" || len(expanded[1].ToolCalls) != 1 {
		t.Fatalf("expected assistant turn to preserve tool_calls, got %+v", expanded[1])
	}
	if got, _ := expanded[1].Content.(string); got != "earlier assistant reply" {
		t.Fatalf("expected assistant text to share the tool-call turn, got %+v", expanded[1])
	}
	if expanded[1].ToolCalls[0].ID != "call_prev" {
		t.Fatalf("expected tool call id call_prev, got %+v", expanded[1].ToolCalls[0])
	}
}

// A → B → C: when expanding history starting from C, all of A's and B's
// inputs/outputs must appear before C's. Previously only C's direct parent
// (B) was emitted, dropping A entirely.
func TestResponsesPreviousResponseIDExpandsFullChain(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	a := &ResponsesObject{
		ID:           "resp_a",
		Object:       "response",
		CreatedAt:    time.Now().Unix(),
		Status:       "completed",
		Model:        "claude-sonnet-4.5",
		StoredInput:  json.RawMessage(`"turn A user"`),
		StoredAt:     time.Now().Unix(),
		Instructions: "be terse",
		Output: []ResponseOutputItem{{
			ID: "msg_a", Type: "message", Role: "assistant", Status: "completed",
			Content: []ResponseContentPart{{Type: "output_text", Text: "turn A assistant"}},
		}},
	}
	b := &ResponsesObject{
		ID:                 "resp_b",
		Object:             "response",
		CreatedAt:          time.Now().Unix(),
		Status:             "completed",
		Model:              "claude-sonnet-4.5",
		StoredInput:        json.RawMessage(`"turn B user"`),
		StoredAt:           time.Now().Unix(),
		PreviousResponseID: a.ID,
		Output: []ResponseOutputItem{{
			ID: "msg_b", Type: "message", Role: "assistant", Status: "completed",
			Content: []ResponseContentPart{{Type: "output_text", Text: "turn B assistant"}},
		}},
	}
	if err := saveResponse(a); err != nil {
		t.Fatalf("save a: %v", err)
	}
	if err := saveResponse(b); err != nil {
		t.Fatalf("save b: %v", err)
	}

	expanded, err := expandPreviousResponseHistory(b)
	if err != nil {
		t.Fatalf("expand full chain: %v", err)
	}

	var transcript []string
	for _, m := range expanded {
		role := m.Role
		text, _ := m.Content.(string)
		transcript = append(transcript, role+":"+text)
	}
	got := strings.Join(transcript, "|")
	want := "user:turn A user|assistant:turn A assistant|user:turn B user|assistant:turn B assistant"
	if got != want {
		t.Fatalf("chain order mismatch:\n got=%s\nwant=%s", got, want)
	}
}

func TestResponsesPreviousResponseIDRejectsBrokenAncestorChain(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	prev := &ResponsesObject{
		ID:                 "resp_child_with_missing_parent",
		PreviousResponseID: "resp_missing_parent",
		StoredInput:        json.RawMessage(`"child input"`),
	}
	if _, err := expandPreviousResponseHistory(prev); err == nil {
		t.Fatal("broken previous_response_id chain was silently truncated")
	}
}

func TestResponsesPreviousInstructionsDoNotCarryForward(t *testing.T) {
	prev := &ResponsesObject{
		ID:           "resp_old_instruction",
		Instructions: "OLD INSTRUCTION MUST NOT CARRY",
		StoredInput:  json.RawMessage(`"earlier user"`),
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "earlier assistant"}},
		}},
	}
	expanded, err := expandPreviousResponseHistory(prev)
	if err != nil {
		t.Fatalf("expand history: %v", err)
	}
	for _, message := range expanded {
		if text, _ := message.Content.(string); strings.Contains(text, "OLD INSTRUCTION") {
			t.Fatalf("ancestor instructions leaked into continuation history: %#v", message)
		}
	}
}

func TestResponsesPreviousToolCallsReuseAncestorCanonicalToolNames(t *testing.T) {
	tools := []OpenAITool{
		mustTool(t, `{"type":"function","name":"foo-bar","parameters":{"type":"object"}}`),
		mustTool(t, `{"type":"function","name":"foo_bar","parameters":{"type":"object"}}`),
	}
	catalog := buildOpenAIToolNameCatalog(tools)
	want := catalog.kiroName("foo_bar")
	if want == catalog.kiroName("foo-bar") {
		t.Fatal("test setup did not produce a canonicalization collision")
	}
	rawTools, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	prev := &ResponsesObject{
		ID:          "resp_tool_history",
		StoredInput: json.RawMessage(`"earlier user"`),
		Tools:       rawTools,
		Output: []ResponseOutputItem{{
			Type:      "function_call",
			CallID:    "call_ancestor",
			Name:      "foo_bar",
			Arguments: `{}`,
		}},
	}
	expanded, err := expandPreviousResponseHistory(prev)
	if err != nil {
		t.Fatalf("expand history: %v", err)
	}
	if len(expanded) != 2 || len(expanded[1].ToolCalls) != 1 {
		t.Fatalf("unexpected expanded history: %#v", expanded)
	}
	if got := expanded[1].ToolCalls[0].Function.Name; got != want {
		t.Fatalf("ancestor tool call used different Kiro canonical name: got=%q want=%q", got, want)
	}
}

// New instructions sent on a continuation request must take effect, even when
// previous_response_id is set. The bug: the old code only attached
// req.Instructions when previous_response_id was empty, silently dropping
// updated system prompts on follow-up turns.
func TestResponsesContinuationKeepsNewInstructions(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	prevID, err := generateResponseID()
	if err != nil {
		t.Fatalf("generate local response id: %v", err)
	}
	prev := &ResponsesObject{
		ID:          prevID,
		Object:      "response",
		CreatedAt:   time.Now().Unix(),
		Status:      "completed",
		Model:       "claude-sonnet-4.5",
		StoredInput: json.RawMessage(`"first user message"`),
		StoredAt:    time.Now().Unix(),
		Output: []ResponseOutputItem{{
			ID: "msg_continuation", Type: "message", Role: "assistant", Status: "completed",
			Content: []ResponseContentPart{{Type: "output_text", Text: "first reply"}},
		}},
	}
	if err := saveResponse(prev); err != nil {
		t.Fatalf("save prev: %v", err)
	}

	var capturedSystem string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedSystem = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "second reply",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"second user turn",
		"previous_response_id":"` + prev.ID + `",
		"instructions":"speak only French",
		"store":false
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(capturedSystem, "speak only French") {
		t.Fatalf("expected new instructions to reach upstream, payload=%s", capturedSystem)
	}
}

func setupResponsesTestHandler(t *testing.T) (*Handler, func()) {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "test-account",
		AuthMethod:  "social",
		Enabled:     true,
		AccessToken: "token-test",
		ProfileArn:  "arn:aws:codewhisperer:profile/test",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable fallback: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}
	now := time.Now()
	h.accountModelSnapshots = testRuntimeSnapshots(map[string]accountModelSnapshot{
		"test-account": {
			Models:      []ModelInfo{{ModelId: "claude-sonnet-4.5"}},
			RefreshedAt: now,
			FreshUntil:  now.Add(time.Hour),
			StaleUntil:  now.Add(time.Hour),
		},
	})
	p.SetModelList("test-account", []string{"claude-sonnet-4.5"})
	h.rebuildEnabledModelUnion()
	cleanup := func() {
		p.WaitForPendingStats()
	}
	return h, cleanup
}

func swapKiroEndpointsForTest(t *testing.T, server *httptest.Server) func() {
	t.Helper()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{
		URL:    server.URL,
		Origin: "AI_EDITOR",
		Name:   "test",
	}}
	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})

	// reasoningCapabilityForModel (handler.go) warms the model cache off the
	// request goroutine on a cache miss (warmModelsCacheAsync), so ANY test
	// using this helper that reaches a Claude/OpenAI/count_tokens entrypoint
	// with an empty cachedModels can now kick a background
	// refreshModelsCache -> ListAvailableModels call. That goes through
	// kiroRestHttpStore's REAL client (buildKiroTransport,
	// Proxy: http.ProxyFromEnvironment) — NOT the local server swapped above,
	// since ListAvailableModels targets kiroRestAPIBase, a separate constant
	// this helper does not touch. Firing http.ProxyFromEnvironment ANYWHERE
	// in the process caches env vars in a process-wide sync.Once on first
	// call, permanently poisoning any later test (e.g.
	// TestBuildKiroTransportFallsBackToEnvironmentProxy) that relies on
	// t.Setenv("HTTPS_PROXY", ...) actually taking effect. Stub it to fail
	// fast without touching the network, and always restore an equally inert
	// stub (never the real ProxyFromEnvironment-backed client) since a
	// background warm goroutine may still be in flight after the test
	// returns and must not be able to re-poison anything when it completes.
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errTest("network disabled in test")
		}),
	})

	return func() {
		kiroEndpoints = oldEndpoints
		kiroHttpStore.Store(oldClient)
		kiroRestHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	}
}

func TestResponsesNonStreamRoundTrip(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "responses non-stream OK",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","input":"hi from test"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp ResponsesObject
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp.Object != "response" {
		t.Fatalf("expected object=response, got %q", resp.Object)
	}
	if resp.Status != "completed" {
		t.Fatalf("expected status=completed, got %q", resp.Status)
	}
	if len(resp.Output) == 0 {
		t.Fatalf("expected output items, got none")
	}
	if resp.Output[0].Type != "message" || len(resp.Output[0].Content) == 0 {
		t.Fatalf("expected message with content, got %+v", resp.Output[0])
	}
	if resp.Output[0].Content[0].Text != "responses non-stream OK" {
		t.Fatalf("unexpected text: %q", resp.Output[0].Content[0].Text)
	}

	loaded, err := loadResponse(resp.ID)
	if err != nil {
		t.Fatalf("loadResponse: %v", err)
	}
	if loaded.ID != resp.ID {
		t.Fatalf("stored response id mismatch")
	}
}

func TestResponsesStreamPreservesUpstreamIncompleteStatus(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "partial answer",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "MAX_TOKENS",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"hello",
		"stream":true,
		"store":false
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"incomplete"`) || !strings.Contains(rec.Body.String(), `"reason":"max_output_tokens"`) {
		t.Fatalf("expected incomplete response.completed event, got %s", rec.Body.String())
	}
}

func TestResponsesStreamSSE(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "stream chunk",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","input":"stream please","stream":true,"store":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	bodyBytes, _ := io.ReadAll(rec.Body)
	bodyStr := string(bodyBytes)

	requiredEvents := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	lastOffset := -1
	for _, eventName := range requiredEvents {
		offset := strings.Index(bodyStr, "event: "+eventName)
		if offset < 0 {
			t.Fatalf("missing event %q in stream body:\n%s", eventName, bodyStr)
		}
		if offset <= lastOffset {
			t.Fatalf("event %q is out of Responses lifecycle order:\n%s", eventName, bodyStr)
		}
		lastOffset = offset
	}
	if strings.Contains(bodyStr, "[DONE]") {
		t.Fatalf("Responses stream must terminate semantically, not with Chat Completions [DONE]:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "stream chunk") {
		t.Fatalf("expected stream content delta, got:\n%s", bodyStr)
	}

	blocks := strings.Split(strings.TrimSpace(bodyStr), "\n\n")
	var doneItemID, completedItemID string
	for i, block := range blocks {
		var eventName, dataLine string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				eventName = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				dataLine = strings.TrimPrefix(line, "data: ")
			}
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(dataLine), &event); err != nil {
			t.Fatalf("decode SSE block %d: %v\n%s", i, err, block)
		}
		if got := int(event["sequence_number"].(float64)); got != i+1 {
			t.Fatalf("sequence_number=%d at block=%d, want=%d", got, i, i+1)
		}
		switch eventName {
		case "response.output_item.done":
			item, _ := event["item"].(map[string]interface{})
			doneItemID, _ = item["id"].(string)
		case "response.completed":
			response, _ := event["response"].(map[string]interface{})
			output, _ := response["output"].([]interface{})
			if len(output) > 0 {
				item, _ := output[0].(map[string]interface{})
				completedItemID, _ = item["id"].(string)
			}
		}
	}
	if doneItemID == "" || completedItemID != doneItemID {
		t.Fatalf("completed response output did not match streamed output item: done=%q completed=%q", doneItemID, completedItemID)
	}
}
