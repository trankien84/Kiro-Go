package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestClaudeCompatibilityAliasesCanonicalizeBeforeRouting(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":64,
		"messages":[{"role":"user","content":[
			{"type":"input_text","text":"hello"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}},
			{"type":"tool_result","tool_use_id":"tool-1","content":[{"type":"input_text","text":"done"}]}
		]}]
	}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		resolveClaudeProviderReasoningCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	wire := string(plan.Semantic.Body)
	for _, alias := range []string{`"input_text"`, `"image_url"`, `"input_image"`} {
		if strings.Contains(wire, alias) {
			t.Fatalf("compatibility alias leaked into canonical body: %s", wire)
		}
	}
	if !strings.Contains(wire, `"type":"text"`) ||
		!strings.Contains(wire, `"type":"image"`) ||
		!strings.Contains(wire, `"type":"base64"`) {
		t.Fatalf("canonical blocks missing: %s", wire)
	}
}

func TestClaudeCompatibilityImageRejectsUnmappedDetail(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":64,
		"messages":[{"role":"user","content":[{
			"type":"image_url",
			"image_url":{"url":"https://example.com/a.png","detail":"high"}
		}]}]
	}`)
	_, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		resolveClaudeProviderReasoningCapability,
	)
	if validationErr == nil || !strings.Contains(validationErr.Message, "no lossless Anthropic Messages mapping") {
		t.Fatalf("validation=%#v", validationErr)
	}
}

func TestClaudeCompatibilityInputImageFileCanonicalizes(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":64,
		"messages":[{"role":"user","content":[{
			"type":"input_image","file_id":"file-123"
		}]}]
	}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		http.Header{"Anthropic-Beta": []string{"files-api-2025-04-14"}},
		"-thinking",
		resolveClaudeProviderReasoningCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !strings.Contains(string(plan.Semantic.Body), `"source":{"file_id":"file-123","type":"file"}`) {
		t.Fatalf("file input was not canonicalized: %s", plan.Semantic.Body)
	}
	if plan.Requirements.Allows(claudeTransportKiroNative) ||
		plan.Requirements.Allows(claudeTransportBedrockInvoke) {
		t.Fatalf("file reference escaped direct-native routing: %#v", plan.Requirements)
	}
}

func TestClaudeCompatibilityImageRejectsAmbiguousOrNestedUnknownSource(t *testing.T) {
	cases := []string{
		`{"type":"input_image","image_url":"https://example.com/a.png","file_id":"file-123"}`,
		`{"type":"input_image","source":{"type":"url","url":"https://example.com/a.png","detail":"high"}}`,
		`{"type":"input_image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="},"media_type":"image/png"}`,
	}
	for _, content := range cases {
		body := []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[` + content + `]}]}`)
		_, validationErr := buildClaudeRequestPlanWithResolver(
			body,
			nil,
			"-thinking",
			resolveClaudeProviderReasoningCapability,
		)
		if validationErr == nil {
			t.Fatalf("ambiguous/lossy source was accepted: %s", content)
		}
	}
}

func TestClaudeNestedInlineToolResultImageIsEmulatedByKiro(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":64,
		"stream":true,
		"output_config":{"effort":"xhigh"},
		"messages":[
			{"role":"user","content":"capture the page"},
			{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"capture","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","cache_control":{"type":"ephemeral"},"content":[{
				"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}
			}]}]}
		]
	}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		http.Header{
			"Anthropic-Beta": []string{"interleaved-thinking-2025-05-14,advanced-tool-use-2025-11-20"},
		},
		"-thinking",
		adaptiveClaudeCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !plan.Requirements.Allows(claudeTransportKiroNative) {
		t.Fatalf("Kiro rejected supported inline tool-result image: %s", plan.Requirements.Summary())
	}
	if plan.Requirements.Allows(claudeTransportBedrockConverse) {
		t.Fatalf("Converse support was claimed without a proven nested-media projection: %#v", plan.Requirements)
	}
	if !plan.Requirements.Allows(claudeTransportDirectAnthropic) ||
		!plan.Requirements.Allows(claudeTransportBedrockInvoke) {
		t.Fatalf("native transports were rejected: %#v", plan.Requirements)
	}
	if plan.KiroProjection.Fidelity != claudeProjectionEmulated ||
		!strings.Contains(plan.KiroProjection.Summary(), "nested_tool_result_image=emulated") {
		t.Fatalf("Kiro nested image projection=%s", plan.KiroProjection.Summary())
	}

	payload, err := ClaudeToKiroCheckedWithProjection(
		&plan.Request,
		false,
		plan.Content,
		plan.KiroProjection,
	)
	if err != nil {
		t.Fatalf("Kiro checked translation rejected inline nested image: %v", err)
	}
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 || cur.Images[0].Format != "png" || cur.Images[0].Source.Bytes != "aGVsbG8=" {
		t.Fatalf("nested image projection=%#v", cur.Images)
	}
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("structured tool result missing: %#v", cur.UserInputMessageContext)
	}
	result := cur.UserInputMessageContext.ToolResults[0]
	if result.ToolUseID != "tool-1" || result.Status != "success" || strings.TrimSpace(result.Content[0].Text) == "" {
		t.Fatalf("tool-result identity/status/text lost: %#v", result)
	}
}

func TestClaudeNestedURLToolResultImageStillFailsClosedForKiro(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":64,
		"messages":[
			{"role":"user","content":"capture"},
			{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"capture","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":[{
				"type":"image","source":{"type":"url","url":"https://example.com/screenshot.png"}
			}]}]}
		]
	}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		resolveClaudeProviderReasoningCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if plan.Requirements.Allows(claudeTransportKiroNative) {
		t.Fatalf("Kiro accepted URL-backed nested media: %s", plan.Requirements.Summary())
	}
	if !plan.Requirements.Allows(claudeTransportDirectAnthropic) {
		t.Fatalf("direct Anthropic should preserve URL-backed nested media: %s", plan.Requirements.Summary())
	}
	if _, err := ClaudeToKiroCheckedWithProjection(&plan.Request, false, plan.Content, plan.KiroProjection); err == nil {
		t.Fatal("Kiro checked translation accepted URL-backed nested media")
	}
}

func TestClaudeSearchResultCitationConsistency(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":64,
		"messages":[{"role":"user","content":[
			{"type":"search_result","source":"https://a","title":"A","content":[{"type":"text","text":"a"}],"citations":{"enabled":true}},
			{"type":"search_result","source":"https://b","title":"B","content":[{"type":"text","text":"b"}]}
		]}]
	}`)
	_, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		resolveClaudeProviderReasoningCapability,
	)
	if validationErr == nil || !strings.Contains(validationErr.Message, "same citations.enabled") {
		t.Fatalf("validation=%#v", validationErr)
	}
}

func TestClaudeWebSearchRequiresSearchResultCitations(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":64,
		"tools":[{"type":"web_search_20260209","name":"web_search"}],
		"messages":[{"role":"user","content":[{
			"type":"search_result","source":"https://a","title":"A","content":[{"type":"text","text":"a"}]
		}]}]
	}`)
	_, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		http.Header{"Anthropic-Beta": []string{"web-search-2025-03-05"}},
		"-thinking",
		resolveClaudeProviderReasoningCapability,
	)
	if validationErr == nil || !strings.Contains(validationErr.Message, "must enable citations") {
		t.Fatalf("validation=%#v", validationErr)
	}
}

func TestClaudeTransportSeparatesRelayFromDirectAnthropic(t *testing.T) {
	relay := &config.Account{AuthMethod: "custom_api"}
	direct := &config.Account{AuthMethod: "anthropic_api"}
	if got := claudeTransportKindForAccount(relay); got != claudeTransportCustomRelay {
		t.Fatalf("relay kind=%s", got)
	}
	if got := claudeTransportKindForAccount(direct); got != claudeTransportDirectAnthropic {
		t.Fatalf("direct kind=%s", got)
	}

	requirements := newClaudeTransportRequirements()
	requirements.Require("native_only", claudeTransportMaskCustom, "test")
	if claudeAccountCompatibleWithRequirements(relay, false, requirements) {
		t.Fatal("relay was treated as a terminal native Messages endpoint")
	}
	if !claudeAccountCompatibleWithRequirements(direct, false, requirements) {
		t.Fatal("direct Anthropic endpoint was rejected")
	}
}

func TestBedrockConverseMapsToolResultErrorAndImage(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}},
		{"type":"text","text":"caption"}
	]`)
	content := anthropicToolResultContentToConverse(raw)
	if len(content) != 2 {
		t.Fatalf("content=%#v", content)
	}
	if _, ok := content[0]["image"]; !ok {
		t.Fatalf("image block lost: %#v", content)
	}

	messageRaw := json.RawMessage(`[
		{"type":"tool_result","tool_use_id":"tool-1","is_error":true,"content":"failed"}
	]`)
	blocks := anthropicContentToConverse(messageRaw)
	if len(blocks) != 1 {
		t.Fatalf("blocks=%#v", blocks)
	}
	toolResult, _ := blocks[0]["toolResult"].(map[string]interface{})
	if toolResult["status"] != "error" {
		t.Fatalf("tool result status=%#v", toolResult)
	}
}

func TestClaudeCompatibilityCanonicalizationIsIdempotent(t *testing.T) {
	req := ClaudeRequest{Messages: []ClaudeMessage{{
		Role: "user",
		Content: []interface{}{
			map[string]interface{}{"type": "input_text", "text": "hello"},
			map[string]interface{}{
				"type":      "image_url",
				"image_url": map[string]interface{}{"url": "data:image/png;base64,aGVsbG8="},
			},
		},
	}}}
	if validationErr := canonicalizeClaudeCompatibilityContent(&req); validationErr != nil {
		t.Fatal(validationErr)
	}
	first, _ := json.Marshal(req.Messages)
	if validationErr := canonicalizeClaudeCompatibilityContent(&req); validationErr != nil {
		t.Fatal(validationErr)
	}
	second, _ := json.Marshal(req.Messages)
	if !bytes.Equal(first, second) {
		t.Fatalf("canonicalization is not idempotent:\n%s\n%s", first, second)
	}
}

func TestClaudeProviderCapabilityFallbackUsesProviderContract(t *testing.T) {
	fable := resolveClaudeProviderReasoningCapability("claude-fable-5")
	if _, disabled := matchThinkingType("disabled", fable.ThinkingTypes); disabled {
		t.Fatalf("Fable 5 incorrectly allows disabled thinking: %#v", fable)
	}
	if _, adaptive := matchThinkingType("adaptive", fable.ThinkingTypes); !adaptive {
		t.Fatalf("Fable 5 missing adaptive thinking: %#v", fable)
	}

	sonnet46 := resolveClaudeProviderReasoningCapability("claude-sonnet-4-6")
	for _, mode := range []string{"enabled", "adaptive", "disabled"} {
		if _, ok := matchThinkingType(mode, sonnet46.ThinkingTypes); !ok {
			t.Fatalf("Sonnet 4.6 missing still-valid mode %q: %#v", mode, sonnet46)
		}
	}

	haiku := resolveClaudeProviderReasoningCapability("claude-haiku-4-5")
	if len(haiku.Efforts) != 0 {
		t.Fatalf("Haiku 4.5 incorrectly advertises effort: %#v", haiku)
	}
	for _, alias := range []string{"private-claude-alias", "claude-opus-5-private"} {
		unknown := resolveClaudeProviderReasoningCapability(alias)
		if unknown.ModelID != "" {
			t.Fatalf("private alias %q was guessed as a known provider model: %#v", alias, unknown)
		}
	}
}

func TestDirectAnthropicForwardAddsDefaultVersion(t *testing.T) {
	observed := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Header.Get("Anthropic-Version")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	resp, err := forwardDirectAnthropicRequest(
		context.Background(), http.MethodPost, server.URL, "sk-test",
		[]byte(`{}`), false, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := <-observed; got != "2023-06-01" {
		t.Fatalf("Anthropic-Version=%q", got)
	}
}

func TestDirectAnthropicCreditsUseDedicatedRate(t *testing.T) {
	t.Setenv("ANTHROPIC_CREDITS_PER_1K_TOKENS", "2.5")
	t.Setenv("CUSTOM_API_CREDITS_PER_1K_TOKENS", "999")
	if got := directAnthropicCreditsForTokens(2000); got != 5 {
		t.Fatalf("direct Anthropic credits=%v want=5", got)
	}
}

func TestForwardUpstreamRequestSeparatesRelayAndDirectAnthropicHeaders(t *testing.T) {
	type observed struct {
		forwarded string
		version   string
		auth      string
		xAPIKey   string
	}
	observations := make(chan observed, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observations <- observed{
			forwarded: r.Header.Get(forwardHeader),
			version:   r.Header.Get("Anthropic-Version"),
			auth:      r.Header.Get("Authorization"),
			xAPIKey:   r.Header.Get("x-api-key"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	headers := make(http.Header)
	headers.Set("Anthropic-Version", "2023-06-01")
	tests := []struct {
		name          string
		markForwarded bool
		direct        bool
		wantForwarded string
		wantAuth      string
	}{
		{name: "relay with marker", markForwarded: true, wantForwarded: "1", wantAuth: "Bearer sk-test"},
		{name: "relay without marker", wantAuth: "Bearer sk-test"},
		{name: "direct Anthropic", direct: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp, err := doForwardUpstreamRequest(
				context.Background(), http.MethodPost, server.URL, "sk-test",
				[]byte(`{}`), false, headers, test.markForwarded, test.direct,
			)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			got := <-observations
			if got.forwarded != test.wantForwarded || got.auth != test.wantAuth {
				t.Fatalf("headers=%#v wantForwarded=%q wantAuth=%q", got, test.wantForwarded, test.wantAuth)
			}
			if got.version != "2023-06-01" || got.xAPIKey != "sk-test" {
				t.Fatalf("semantic/auth headers=%#v", got)
			}
		})
	}
}
