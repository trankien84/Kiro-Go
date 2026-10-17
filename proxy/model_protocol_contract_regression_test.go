package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"kiro-go/config"
)

func protocolContractPlan(t *testing.T, body string) claudeRequestPlan {
	t.Helper()
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		[]byte(body), http.Header{}, "-thinking", resolveClaudeProviderReasoningCapability,
	)
	if validationErr != nil {
		t.Fatalf("unexpected planner error: %+v\n%s", validationErr, body)
	}
	return plan
}

func TestOpaqueModelIdentityAndLegacyAliasResolution(t *testing.T) {
	for _, id := range []string{
		"claude-sonnet-4-5-private", "claude-opus-4-8-custom",
		"claude-haiku-5-5", "claude-haiku-5-5-variant", "custom/claude-haiku-5-5",
		"claude-sonnet-4-5-20251001", "claude-opus-4-8+edge", "Claude-Sonnet-4-6",
	} {
		actual, _ := ParseModelAndThinking(id, "-thinking")
		if actual != id {
			t.Fatalf("opaque provider ID rewritten: %q -> %q", id, actual)
		}
	}
	for input, want := range map[string]string{
		"claude-opus-4-8":   "claude-opus-4.8",
		"claude-sonnet-4-6": "claude-sonnet-4.6",
		"claude-haiku-4-5":  "claude-haiku-4.5",
		"claude-haiku-5-5":  "claude-haiku-5-5",
	} {
		actual, _ := ParseModelAndThinking(input, "-thinking")
		if actual != want {
			t.Fatalf("legacy normalization %q got %q, want %q", input, actual, want)
		}
	}
	if isClaudeHaiku55Model("claude-haiku-5-0") || isClaudeHaiku55Model("private/claude-haiku-5-5") || isClaudeHaiku55Model("private/claude-haiku-5-5-extra") || !isClaudeHaiku55Model("us.anthropic.claude-haiku-5-5-v1:0") {
		t.Fatal("Haiku 5.5 exact provider identity classification failed")
	}
}

func TestBrowserToolsetRoundTripAndRouting(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":2048,"tools":[{"type":"browser_toolset_20260801","configs":{"read_console":{"enabled":true}}},{"name":"navigate","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"Open docs"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu-browser-1","name":"navigate","toolset_name":"browser","input":{"url":"https://example.com"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu-browser-1","toolset_name":"browser","content":[{"type":"browser_state","tabs":[{"tab_id":"t1","active":true,"url":"https://example.com"}]}]}]}]}`
	plan := protocolContractPlan(t, body)
	if !plan.Catalog.NativeToolsets["browser"] || plan.Requirements.AllowsKiro() || plan.Requirements.Allows(claudeTransportBedrockInvoke) || plan.Requirements.Allows(claudeTransportBedrockConverse) || !plan.Requirements.Allows(claudeTransportDirectAnthropic) {
		t.Fatalf("invalid browser routing or toolset declaration: %s", plan.Requirements.Summary())
	}
	if !plan.Catalog.NativeToolsets["browser"] || !plan.Requirements.UsesBrowserToolset {
		t.Fatal("browser capability not registered")
	}
	var before, after interface{}
	if err := json.Unmarshal([]byte(body), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(plan.Semantic.Body, &after); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("native browser history changed: %v\n%s", err, plan.Semantic.Body)
	}
	var h Handler
	acc := &config.Account{AuthMethod: "anthropic_api", BaseURL: "https://api.anthropic.com/v1"}
	if ok, reason := h.claudeAccountSupportsRequestedProtocol(acc, "claude-opus-5-5", plan.Requirements); !ok {
		t.Fatalf("supported native browser route rejected: %s", reason)
	}
	if ok, _ := h.claudeAccountSupportsRequestedProtocol(acc, "claude-sonnet-4-6", plan.Requirements); ok {
		t.Fatal("unsupported older native browser model accepted")
	}
	// A native browser member and a custom function may share a member name.
	if _, ok := plan.Catalog.ClientEntry("navigate"); !ok {
		t.Fatal("custom member-name overlap should remain callable by name")
	}
	if bytes.Contains(plan.Semantic.Body, []byte(`"name":"browser"`)) {
		t.Fatal("gateway synthesized forbidden browser tool name")
	}
}

func TestBrowserToolsetStructuralValidation(t *testing.T) {
	for _, tc := range []struct {
		tools string
		want  string
	}{
		{`[{"type":"browser_toolset_20260801","name":"browser"}]`, "name"},
		{`[{"type":"browser_toolset_20260801"},{"type":"browser_toolset_20260801"}]`, "only once"},
		{`[{"type":"browser_toolset_20260801"},{"name":"browser","input_schema":{"type":"object"}}]`, "named browser"},
		{`[{"name":"browser","input_schema":{"type":"object"}},{"type":"browser_toolset_20260801"}]`, "named browser"},
		{`[{"type":"browser_toolset_20260801","configs":null}]`, "configs"},
		{`[{"type":"browser_toolset_20260801","configs":{"read_console":{"enabled":"yes"}}}]`, "enabled"},
		{`[{"type":"browser_toolset_20260801","defer_loading":true}]`, "defer_loading"},
	} {
		body := `{"model":"claude-opus-5-5","max_tokens":1024,"tools":` + tc.tools + `,"messages":[{"role":"user","content":"hey"}]}`
		if _, err := buildClaudeRequestPlanWithResolver([]byte(body), nil, "-thinking", resolveClaudeProviderReasoningCapability); err == nil {
			t.Fatalf("invalid toolset was accepted: %s", tc.tools)
		} else if tc.want == "named browser" && !strings.Contains(err.Message, "tool named browser") || tc.want != "named browser" && !strings.Contains(err.Message, tc.want) {
			t.Fatalf("unexpected error %s: %+v", tc.tools, err)
		}
	}
	// User result must echo the provider-defined toolset_name of its tool_use.
	wrongResult := `{"model":"claude-opus-5-5","max_tokens":1024,"tools":[{"type":"browser_toolset_20260801"}],"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"tool_use","id":"abc","name":"navigate","toolset_name":"browser","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"abc","toolset_name":"computer","content":"done"}]}]}`
	if _, err := buildClaudeRequestPlanWithResolver([]byte(wrongResult), nil, "-thinking", resolveClaudeProviderReasoningCapability); err == nil || !strings.Contains(err.Message, "toolset_name") {
		t.Fatalf("mismatched member result accepted: %+v", err)
	}
}

func TestHaiku55FallbackCapabilityContract(t *testing.T) {
	for _, id := range []string{"claude-haiku-5-5", "anthropic.claude-haiku-5-5:0"} {
		cap := resolveClaudeProviderReasoningCapability(id)
		if !cap.SupportsThinking || cap.DefaultThinkingType != "adaptive" || len(cap.ThinkingTypes) != 2 || cap.ThinkingTypes[1] != "disabled" || len(cap.Efforts) != 5 {
			t.Fatalf("bad Haiku 5.5 reasoning fallback: %+v", cap)
		}
		protocol := resolveClaudeModelCapabilities(id)
		if protocol.ToolSearch != claudeFeatureSupported || protocol.ProgrammaticToolCalling != claudeFeatureSupported || protocol.MidConversationSystem != claudeFeatureSupported || protocol.MidConversationTools != claudeFeatureSupported || protocol.AssistantPrefill != claudeFeatureUnsupported {
			t.Fatalf("bad Haiku 5.5 protocol: %+v", protocol)
		}
	}
	if cap := resolveClaudeProviderReasoningCapability("claude-haiku-5-private"); cap.SupportsThinking {
		t.Fatal("private Haiku model borrowed public capability")
	}
}

func TestHaiku55ProviderContractMatrix(t *testing.T) {
	official := &config.Account{AuthMethod: "anthropic_api", BaseURL: "https://api.anthropic.com/v1"}
	compatible := &config.Account{AuthMethod: "anthropic_api", BaseURL: "https://other.example/v1"}
	bedrock := &config.Account{AuthMethod: "bedrock"}
	var h Handler
	for _, tc := range []struct{ name, fields, reject string }{
		{"default", "", ""},
		{"adaptive", `,"thinking":{"type":"adaptive"}`, ""},
		{"disabled medium", `,"thinking":{"type":"disabled"},"output_config":{"effort":"medium"}`, ""},
		{"disabled high", `,"thinking":{"type":"disabled"},"output_config":{"effort":"high"}`, ""},
		{"disabled xhigh", `,"thinking":{"type":"disabled"},"output_config":{"effort":"xhigh"}`, "disabled"},
		{"disabled max", `,"thinking":{"type":"disabled"},"output_config":{"effort":"max"}`, "disabled"},
		{"enabled", `,"thinking":{"type":"enabled","budget_tokens":1024}`, "adaptive or disabled"},
		{"disabled display empty", `,"thinking":{"type":"disabled","display":""}`, "display"},
		{"between tools", `,"thinking":{"type":"between_tools"}`, "adaptive or disabled"},
		{"temperature default", `,"temperature":1`, ""},
		{"temperature zero", `,"temperature":0`, "temperature"},
		{"top p default", `,"top_p":0.99`, ""},
		{"top p one", `,"top_p":1`, "top_p"},
		{"both sampling", `,"top_p":0.99,"temperature":1`, "combine"},
		{"top k zero", `,"top_k":0`, "top_k"},
		{"unknown effort", `,"output_config":{"effort":"super"}`, "effort"},
		{"forced choice", `,"tool_choice":{"type":"any"}`, ""},
		{"changed message effort", `,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi","output_config":{"effort":"high"}}]`, "per-message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := `{"model":"claude-haiku-5-5","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]`
			// Message override case needs to replace the default message array.
			if strings.Contains(tc.fields, `,"messages":`) {
				base = `{"model":"claude-haiku-5-5","max_tokens":2048`
			}
			body := base + tc.fields + `}`
			var request ClaudeRequest
			if err := json.Unmarshal([]byte(body), &request); err != nil {
				t.Fatal(err)
			}
			got := claudeHaiku55RequestViolation(&request)
			if tc.reject != "" && !strings.Contains(got, tc.reject) || tc.reject == "" && got != "" {
				t.Fatalf("violation=%q want substring=%q", got, tc.reject)
			}
			req := claudeTransportRequirements{Haiku55ContractError: got}
			for _, account := range []*config.Account{official, bedrock} {
				ok, _ := h.claudeAccountSupportsRequestedProtocol(account, "claude-haiku-5-5", req)
				if ok != (tc.reject == "") {
					t.Fatalf("official accepted=%t violation=%q", ok, got)
				}
			}
			if ok, why := h.claudeAccountSupportsRequestedProtocol(compatible, "claude-haiku-5-5", req); !ok {
				t.Fatalf("compatible contract incorrectly constrained: %q", why)
			}
		})
	}
	const legacy = `{"model":"claude-haiku-5-5","max_tokens":2048,"tools":[{"type":"computer_20251124","name":"computer","display_width_px":800,"display_height_px":600}],"messages":[{"role":"user","content":"hi"}]}`
	plan := protocolContractPlan(t, legacy)
	if ok, _ := h.claudeAccountSupportsRequestedProtocol(official, "claude-haiku-5-5", plan.Requirements); ok {
		t.Fatal("official accepted legacy computer")
	}
	if ok, reason := h.claudeAccountSupportsRequestedProtocol(bedrock, "anthropic.claude-haiku-5-5:0", plan.Requirements); !ok {
		t.Fatalf("Bedrock legacy computer should remain supported: %q", reason)
	}
	req := claudeTransportRequirements{RequiresStructuredOutputFormat: true}
	if ok, _ := h.claudeAccountSupportsRequestedProtocol(bedrock, "claude-haiku-5-5", req); ok {
		t.Fatal("Bedrock Haiku structured outputs accepted")
	}
}

func TestLiveDisabledCapabilityTriState(t *testing.T) {
	for _, tc := range []struct {
		name, jsonText string
		want           bool
	}{
		{"supported", `{"capabilities":{"thinking":{"supported":true,"types":{"adaptive":{"supported":true},"disabled":{"supported":true}}}}}`, true},
		{"denied", `{"capabilities":{"thinking":{"supported":true,"types":{"adaptive":{"supported":true},"disabled":{"supported":false}}}}}`, false},
		{"missing", `{"capabilities":{"thinking":{"supported":true,"types":{"adaptive":{"supported":true}}}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire struct {
				Capabilities directAnthropicModelCapabilities `json:"capabilities"`
			}
			if err := json.Unmarshal([]byte(tc.jsonText), &wire); err != nil {
				t.Fatal(err)
			}
			metadata := directAnthropicModelMetadata{ID: "claude-haiku-5-5", CapabilitiesPresent: true, Capabilities: wire.Capabilities}
			cap := directAnthropicReasoningCapability("claude-haiku-5-5", metadata)
			cap = mergeOfficialDirectAnthropicReasoningSemantics("claude-haiku-5-5", cap, metadata)
			_, allowed := matchThinkingType("disabled", cap.ThinkingTypes)
			if allowed != tc.want {
				t.Fatalf("disabled allowed=%v want=%v with %s", allowed, tc.want, tc.jsonText)
			}
		})
	}
}
