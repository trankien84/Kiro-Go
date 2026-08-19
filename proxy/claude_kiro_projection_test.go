package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestClaudeCodeObservedBundleKeepsAllKiroCredentialKindsEligible(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":64000,
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"xhigh","format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}}},
		"cache_control":{"type":"ephemeral"},
		"metadata":{"user_id":"opaque-user"},
		"service_tier":"auto",
		"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]},
		"messages":[
			{"role":"user","content":"start"},
			{"role":"system","content":"new operator context"}
		]
	}`)
	headers := http.Header{
		"Anthropic-Version":         []string{"2023-06-01"},
		"Anthropic-User-Profile-Id": []string{"profile-123"},
		"Anthropic-Beta": []string{strings.Join([]string{
			"claude-code-20250219",
			"interleaved-thinking-2025-05-14",
			"thinking-token-count-2026-05-13",
			"context-management-2025-06-27",
			"prompt-caching-scope-2026-01-05",
			"structured-outputs-2025-12-15",
			"mid-conversation-system-2026-04-07",
			"advisor-tool-2026-03-01",
			"effort-2025-11-24",
			"fallback-credit-2026-06-01",
			"afk-mode-2026-01-31",
			"user-profiles-2026-03-24",
		}, ",")},
	}

	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		headers,
		"-thinking",
		resolveClaudeProviderReasoningCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !plan.Requirements.AllowsKiro() {
		t.Fatalf("Kiro excluded by observed Claude Code bundle: %s", plan.Requirements.Summary())
	}
	if plan.KiroProjection.Fidelity != claudeProjectionEmulated {
		t.Fatalf("projection fidelity=%s summary=%s", plan.KiroProjection.Fidelity, plan.KiroProjection.Summary())
	}

	accounts := []struct {
		name    string
		account *config.Account
	}{
		{name: "social github", account: &config.Account{ID: "github", AuthMethod: "social"}},
		{name: "social google", account: &config.Account{ID: "google", AuthMethod: "social"}},
		{name: "builder id or idc", account: &config.Account{ID: "idc", AuthMethod: "idc"}},
		{name: "external idp", account: &config.Account{ID: "external", AuthMethod: "external_idp"}},
		{name: "kiro api key", account: &config.Account{ID: "api-key", AuthMethod: "api_key"}},
	}
	for _, tc := range accounts {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.account.UsesKiroRuntime() {
				t.Fatal("credential did not normalize to the Kiro runtime")
			}
			if got := claudeTransportKindForAccount(tc.account); got != claudeTransportKiroNative {
				t.Fatalf("transport=%s", got)
			}
			if !claudeAccountCompatibleWithRequirements(tc.account, false, plan.Requirements) {
				t.Fatalf("Kiro credential rejected requirements: %s", plan.Requirements.Summary())
			}
		})
	}
}

func TestClaudeNestedToolResultImageKeepsEveryKiroCredentialKindEligible(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":64,
		"messages":[
			{"role":"user","content":"capture"},
			{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"capture","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}
		]
	}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		http.Header{"Anthropic-Beta": []string{"interleaved-thinking-2025-05-14,advanced-tool-use-2025-11-20"}},
		"-thinking",
		adaptiveClaudeCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !plan.Requirements.AllowsKiro() || plan.KiroProjection.Fidelity != claudeProjectionEmulated {
		t.Fatalf("nested tool-result image excluded Kiro: requirements=%s projection=%s", plan.Requirements.Summary(), plan.KiroProjection.Summary())
	}

	accounts := []*config.Account{
		{ID: "github", AuthMethod: "social"},
		{ID: "google", AuthMethod: "social"},
		{ID: "idc", AuthMethod: "idc"},
		{ID: "external", AuthMethod: "external_idp"},
		{ID: "api-key", AuthMethod: "api_key"},
	}
	for _, account := range accounts {
		if !account.UsesKiroRuntime() {
			t.Fatalf("account %s did not normalize to Kiro runtime", account.ID)
		}
		if !claudeAccountCompatibleWithRequirements(account, false, plan.Requirements) {
			t.Fatalf("account %s rejected nested image requirements: %s", account.ID, plan.Requirements.Summary())
		}
	}
}

func TestClaudeCodeProjectionContractIsIdenticalForStreamAndNonStream(t *testing.T) {
	headers := http.Header{
		"Anthropic-Beta": []string{
			"claude-code-20250219,effort-2025-11-24,structured-outputs-2025-12-15,context-management-2025-06-27,mid-conversation-system-2026-04-07",
		},
	}
	var first claudeRequestPlan
	for index, stream := range []bool{false, true} {
		body := []byte(fmt.Sprintf(`{
			"model":"claude-opus-5",
			"max_tokens":64,
			"stream":%t,
			"thinking":{"type":"adaptive"},
			"output_config":{"effort":"xhigh","format":{"type":"json_schema","schema":{"type":"object"}}},
			"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]},
			"messages":[{"role":"user","content":"start"},{"role":"system","content":"late system"}]
		}`, stream))
		plan, validationErr := buildClaudeRequestPlanWithResolver(
			body,
			headers,
			"-thinking",
			resolveClaudeProviderReasoningCapability,
		)
		if validationErr != nil {
			t.Fatal(validationErr)
		}
		if !plan.Requirements.AllowsKiro() || plan.KiroProjection.Fidelity != claudeProjectionEmulated {
			t.Fatalf("stream=%t requirements=%s projection=%s", stream, plan.Requirements.Summary(), plan.KiroProjection.Summary())
		}
		if index == 0 {
			first = plan
			continue
		}
		if plan.Requirements.Allowed != first.Requirements.Allowed ||
			plan.KiroProjection.Fidelity != first.KiroProjection.Fidelity {
			t.Fatalf("stream changed selection contract: nonstream=%#v stream=%#v", first.Requirements, plan.Requirements)
		}
	}
}

func TestClaudeBetaDeclarationDoesNotBecomeKiroTransportCapability(t *testing.T) {
	for _, beta := range []string{
		"effort-2025-11-24",
		"interleaved-thinking-2025-05-14",
		"fallback-credit-2026-06-09",
		"mid-conversation-system-2026-04-07",
		"structured-outputs-2025-11-13",
		"structured-outputs-2025-12-15",
	} {
		body := `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
		req, semantic := testClaudeSemantic(t, body, http.Header{"Anthropic-Beta": []string{beta}})
		requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
		if validationErr != nil {
			t.Fatalf("beta=%s validation=%v", beta, validationErr)
		}
		if !requirements.AllowsKiro() {
			t.Fatalf("beta declaration %s excluded Kiro: %s", beta, requirements.Summary())
		}
	}

	body := `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req, semantic := testClaudeSemantic(t, body, http.Header{"Anthropic-Beta": []string{"future-semantic-2099-01-01"}})
	requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if requirements.AllowsKiro() || !requirements.Allows(claudeTransportDirectAnthropic) {
		t.Fatalf("unknown beta did not fail closed: %#v", requirements)
	}
}

func TestClaudeKiroContextManagementClearsOldPairsWithoutBreakingCorrelation(t *testing.T) {
	var req ClaudeRequest
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":64,
		"context_management":{"edits":[{
			"type":"clear_tool_uses_20250919",
			"trigger":{"type":"tool_uses","value":2},
			"keep":{"type":"tool_uses","value":1},
			"exclude_tools":["keep_forever"],
			"clear_tool_inputs":true
		}]},
		"messages":[
			{"role":"user","content":"start"},
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"old","input":{"path":"a"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"old result"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"keep_forever","input":{"path":"b"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"preserved result"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"new","input":{"path":"c"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","content":"new result"}]}
		]
	}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	inventory, validationErr := buildClaudeContentInventory(&req, nil)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	projection := buildClaudeKiroProjection(&req, inventory, claudeSemanticRequest{})
	if projection.Fidelity != claudeProjectionEmulated || projection.ContextManagement == nil {
		t.Fatalf("projection=%#v", projection)
	}
	effective, err := applyClaudeKiroProjection(&req, projection)
	if err != nil {
		t.Fatal(err)
	}
	if effective == &req {
		t.Fatal("context-managed Kiro projection mutated the canonical request")
	}

	interactions := collectClaudeKiroToolInteractions(effective)
	if len(interactions) != 3 {
		t.Fatalf("interaction count=%d", len(interactions))
	}
	resultContent := func(messageIndex int) interface{} {
		blocks := claudeMessageBlockMaps(effective.Messages[messageIndex].Content)
		return blocks[0]["content"]
	}
	if got := resultContent(2); got != "[Tool result cleared by context management]" {
		t.Fatalf("old result=%#v", got)
	}
	if got := resultContent(4); got != "preserved result" {
		t.Fatalf("excluded result=%#v", got)
	}
	if got := resultContent(6); got != "new result" {
		t.Fatalf("new result=%#v", got)
	}
	oldUse := claudeMessageBlockMaps(effective.Messages[1].Content)[0]
	if input, ok := oldUse["input"].(map[string]interface{}); !ok || len(input) != 0 {
		t.Fatalf("old tool input=%#v", oldUse["input"])
	}
	for index, wantID := range []string{"t1", "t2", "t3"} {
		if interactions[index].ID != wantID {
			t.Fatalf("tool correlation changed: %#v", interactions)
		}
	}
}

func TestClaudeKiroContextManagementFailsClosedWhenMinimumTokenClearCannotBeReproduced(t *testing.T) {
	var req ClaudeRequest
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":64,
		"context_management":{"edits":[{
			"type":"clear_tool_uses_20250919",
			"clear_at_least":{"type":"input_tokens","value":5000}
		}]},
		"messages":[{"role":"user","content":"hi"}]
	}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	inventory, validationErr := buildClaudeContentInventory(&req, nil)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	projection := buildClaudeKiroProjection(&req, inventory, claudeSemanticRequest{})
	if projection.Fidelity != claudeProjectionUnsupported {
		t.Fatalf("projection=%s summary=%s", projection.Fidelity, projection.Summary())
	}
}

func TestClaudeMidConversationSystemKiroProjectionPreservesChronology(t *testing.T) {
	var req ClaudeRequest
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":64,
		"messages":[
			{"role":"user","content":"first user"},
			{"role":"system","content":"new operator context"},
			{"role":"assistant","content":"acknowledged"},
			{"role":"user","content":"final user"}
		]
	}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	inventory, validationErr := buildClaudeContentInventory(&req, nil)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	projection := buildClaudeKiroProjection(&req, inventory, claudeSemanticRequest{})
	payload, err := ClaudeToKiroCheckedWithProjection(&req, false, inventory, projection)
	if err != nil {
		t.Fatal(err)
	}
	positions := map[string]int{}
	for index, item := range payload.ConversationState.History {
		if item.UserInputMessage != nil {
			positions[item.UserInputMessage.Content] = index
		}
		if item.AssistantResponseMessage != nil {
			positions[item.AssistantResponseMessage.Content] = index
		}
	}
	first, firstOK := positions["first user"]
	system, systemOK := positions["new operator context"]
	ack, ackOK := positions["acknowledged"]
	if !firstOK || !systemOK || !ackOK || !(first < system && system < ack) {
		t.Fatalf("history chronology not preserved: positions=%#v history=%#v", positions, payload.ConversationState.History)
	}
	if payload.ConversationState.CurrentMessage.UserInputMessage.Content != "final user" {
		t.Fatalf("current content=%q", payload.ConversationState.CurrentMessage.UserInputMessage.Content)
	}
}

func TestClaudeKiroDropsToolCacheMetadataButNotModelVisibleInputExamples(t *testing.T) {
	cacheBody := `{"model":"claude-opus-5","max_tokens":64,"tools":[{"name":"read_file","description":"read","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`
	cacheReq, cacheSemantic := testClaudeSemantic(t, cacheBody, nil)
	cacheInventory, validationErr := buildClaudeContentInventory(cacheReq, nil)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	cacheProjection := buildClaudeKiroProjection(cacheReq, cacheInventory, cacheSemantic)
	cacheRequirements, validationErr := analyzeClaudeTransportRequirements(cacheReq, cacheSemantic, "claude-opus-5")
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if cacheProjection.Fidelity != claudeProjectionEmulated || !cacheRequirements.AllowsKiro() {
		t.Fatalf("tool cache metadata should be emulated on Kiro: projection=%s requirements=%s", cacheProjection.Summary(), cacheRequirements.Summary())
	}

	inputExamplesBody := `{"model":"claude-opus-5","max_tokens":64,"tools":[{"name":"read_file","description":"read","input_schema":{"type":"object"},"input_examples":[{"path":"README.md"}]}],"messages":[{"role":"user","content":"hi"}]}`
	inputReq, inputSemantic := testClaudeSemantic(t, inputExamplesBody, nil)
	inputRequirements, validationErr := analyzeClaudeTransportRequirements(inputReq, inputSemantic, "claude-opus-5")
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if inputRequirements.AllowsKiro() {
		t.Fatalf("model-visible tool input examples must remain fail-closed on Kiro: %s", inputRequirements.Summary())
	}
}
