package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestOpus55ExactModelCapabilities(t *testing.T) {
	for _, name := range []string{"claude-opus-5-5", "claude-opus-5.5", "anthropic.claude-opus-5-5:0", "us.anthropic.claude-opus-5-5-v1:0"} {
		if !isClaudeOpus55Model(name) {
			t.Fatalf("%s did not resolve as opus 5.5", name)
		}
		caps := resolveClaudeModelCapabilities(name)
		if caps.ToolSearch != claudeFeatureSupported || caps.ProgrammaticToolCalling != claudeFeatureSupported || caps.MidConversationSystem != claudeFeatureSupported || caps.MidConversationTools != claudeFeatureSupported {
			t.Fatalf("opus 5.5 capabilities for %q: %#v", name, caps)
		}
		reasoning := fallbackClaudeReasoningCapability(name)
		if !reasoning.SupportsThinking || reasoning.SupportsBudgetTokens || len(reasoning.ThinkingTypes) != 1 || reasoning.ThinkingTypes[0] != "adaptive" || reasoning.DefaultThinkingType != "adaptive" || reasoning.DefaultThinkingDisplay != "omitted" || len(reasoning.Efforts) != 5 {
			t.Fatalf("opus 5.5 reasoning for %q: %#v", name, reasoning)
		}
	}
	for _, name := range []string{"claude-opus-5", "claude-opus-5-0", "private-opus-5-5", "claude-opus-5-5-private"} {
		if isClaudeOpus55Model(name) {
			t.Fatalf("%q was misclassified as official opus 5.5", name)
		}
	}
	opus5 := fallbackClaudeReasoningCapability("claude-opus-5")
	if len(opus5.ThinkingTypes) != 2 || opus5.ThinkingTypes[1] != "disabled" || !modelCapsDisabledThinkingAtHigh("claude-opus-5") || modelCapsDisabledThinkingAtHigh("claude-opus-5-5") {
		t.Fatalf("opus 5.0 legacy contract changed or leaked to 5.5: %#v", opus5)
	}
}

func TestOpus55RequestContractAndWirePreservation(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  string
	}{
		{"omitted controls", "", ""},
		{"adaptive thinking", `,"thinking":{"type":"adaptive"}`, ""},
		{"disabled thinking", `,"thinking":{"type":"disabled"}`, "adaptive thinking"},
		{"legacy enabled", `,"thinking":{"type":"enabled","budget_tokens":1024}`, "adaptive thinking"},
		{"adaptive budget explicitly zero", `,"thinking":{"type":"adaptive","budget_tokens":0}`, "budget_tokens"},
		{"effort max", `,"output_config":{"effort":"max"}`, ""},
		{"bad effort", `,"output_config":{"effort":"extreme"}`, "effort"},
		{"auto tool choice", `,"tool_choice":{"type":"auto"}`, ""},
		{"none tool choice", `,"tool_choice":{"type":"none"}`, ""},
		{"forced any", `,"tool_choice":{"type":"any"}`, "forced tool_choice"},
		{"forced named tool", `,"tool_choice":{"type":"tool","name":"run"}`, "forced tool_choice"},
		{"temperature default", `,"temperature":1`, ""},
		{"temperature zero explicitly", `,"temperature":0`, "temperature"},
		{"top p default", `,"top_p":1`, ""},
		{"top p compatible", `,"top_p":0.99`, ""},
		{"top p nondefault", `,"top_p":0.5`, "top_p"},
		{"top k nondefault", `,"top_k":1`, "top_k"},
		{"top k explicit zero", `,"top_k":0`, "top_k"},
		{"top k null", `,"top_k":null`, "top_k"},
	}
	official := &config.Account{ID: "official", AuthMethod: "anthropic_api", BaseURL: "https://api.anthropic.com/v1"}
	compatible := &config.Account{ID: "compatible", AuthMethod: "anthropic_api", BaseURL: "https://provider.example/v1"}
	bedrock := &config.Account{ID: "bedrock", AuthMethod: "bedrock"}
	var handler Handler
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"claude-opus-5-5","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`)
			if tc.extra != "" {
				body = append(append([]byte(nil), body[:len(body)-1]...), []byte(tc.extra+"}")...)
			}
			var req ClaudeRequest
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatal(err)
			}
			got := claudeOpus55RequestViolation(&req)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("violation=%q, expected substring %q", got, tc.want)
			}
			reqProfile := claudeTransportRequirements{Opus55ContractError: got}
			if ok, _ := handler.claudeAccountSupportsRequestedProtocol(official, "claude-opus-5-5", reqProfile); ok != (tc.want == "") {
				t.Fatal("native official provider accepted a forbidden Opus 5.5 request")
			}
			if ok, _ := handler.claudeAccountSupportsRequestedProtocol(bedrock, "anthropic.claude-opus-5-5-v1:0", reqProfile); ok != (tc.want == "") {
				t.Fatal("native Bedrock provider accepted a forbidden Opus 5.5 request")
			}
			if ok, _ := handler.claudeAccountSupportsRequestedProtocol(compatible, "claude-opus-5-5", reqProfile); !ok {
				t.Fatal("transparent custom provider must own its API contract")
			}
			if ok, _ := handler.claudeAccountSupportsRequestedProtocol(official, "claude-opus-5", reqProfile); !ok {
				t.Fatal("Opus 5.0 contract was changed")
			}
		})
	}
}

func TestOpus55ContractFlowsThroughCanonicalPlan(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5-5","max_tokens":128,"tool_choice":{"type":"any"},"messages":[{"role":"user","content":"hello"}]}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(body, nil, "-thinking", resolveClaudeProviderReasoningCapability)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !strings.Contains(plan.Requirements.Opus55ContractError, "forced tool_choice") {
		t.Fatalf("model contract missing from canonical request requirements: %#v", plan.Requirements)
	}
	if plan.ResolvedModel != "claude-opus-5-5" {
		t.Fatalf("normalized model=%q", plan.ResolvedModel)
	}
	if !strings.Contains(string(plan.Semantic.Body), `"type":"any"`) {
		t.Fatalf("tool choice was silently mutated: %s", plan.Semantic.Body)
	}
}

func TestOpus55ExactWireModelAndLegacyNormalization(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		thinking    bool
	}{
		{"claude-opus-5-5", "claude-opus-5-5", false},
		{"claude-opus-5-5-thinking", "claude-opus-5-5", true},
		{"claude-opus-4-8", "claude-opus-4.8", false},
	} {
		got, thinking := ParseModelAndThinking(tc.input, "-thinking")
		if got != tc.want || thinking != tc.thinking {
			t.Fatalf("model=%s got=%s thinking=%v", tc.input, got, thinking)
		}
	}
}
