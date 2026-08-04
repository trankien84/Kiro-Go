package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// TestEffortOnlyRequestDoesNotInjectThinkingIntoTextStream pins F3: a client
// that sends output_config.effort (or any other native-schema reasoning
// trigger) WITHOUT a `thinking` object must not have reasoning content
// surfaced in the response — regardless of the operator's configured
// thinkingCfg.ClaudeFormat.
//
// Before the fix, `thinking := legacyOrClientThinking || nativeRequested`
// went true purely from nativeRequested (built from output_config.effort
// alone), while thinkingResponseOpts stayed keyed off req.Thinking (nil here)
// and fell back to the operator's ClaudeFormat. With ClaudeFormat=="think"
// that meant Kiro's reasoningContentEvent text got inlined into the plain
// assistant text stream as `<think>...</think>` for a client that never
// mentioned "thinking" at all — an unrequested response-shape change.
//
// Reverting handler.go's `thinking := legacyOrClientThinking ||
// (req.Thinking != nil && nativeRequested)` back to
// `legacyOrClientThinking || nativeRequested` makes this fail: the stream
// body would contain "<think>" and the leaked reasoning text.
func TestEffortOnlyRequestDoesNotInjectThinkingIntoTextStream(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// Operator opted into the legacy inline tag format for whatever thinking
	// IS shown — this is exactly the configuration the bug report calls out as
	// the surprising leak path.
	if err := config.UpdateThinkingConfig("-thinking", "reasoning_content", "think"); err != nil {
		t.Fatalf("UpdateThinkingConfig: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "acct-effort-only",
		Enabled:     true,
		AccessToken: "token-effort-only",
		ProfileArn:  "arn:aws:codewhisperer:profile/effort-only",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
			"text": "secret internal reasoning",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "the visible answer",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	t.Cleanup(swapKiroEndpointsForTest(t, server))

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}

	// A model schema that supports native effort (output_config path) but was
	// never asked for a `thinking` object — the exact shape that made
	// nativeRequested true without req.Thinking being set.
	h.cachedModels = []ModelInfo{{
		ModelId: "claude-sonnet-4.5",
		AdditionalModelRequestFieldsSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"output_config": {
					"type": "object",
					"properties": {
						"effort": { "enum": ["low", "medium", "high"] }
					}
				}
			}
		}`),
	}}

	reqBody := `{"model":"claude-sonnet-4.5","stream":true,"max_tokens":100,` +
		`"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high"}}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "the visible answer") {
		t.Fatalf("expected the normal answer to still be delivered, got %s", body)
	}
	if strings.Contains(body, "<think>") {
		t.Fatalf("effort-only request (no thinking object) must not get inline <think> tags from the operator's default format, got %s", body)
	}
	if strings.Contains(body, "secret internal reasoning") {
		t.Fatalf("effort-only request (no thinking object) must not surface reasoning content at all, got %s", body)
	}
}
