package proxy

import (
	"reflect"
	"testing"
)

func TestBuildClaudeKiroCandidateAdditionalFieldsUsesExactRouteReasoning(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}],
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"xhigh"}
	}`)

	narrow := resolvedProviderModel{
		ProviderID: "claude-opus-5",
		Reasoning: ModelReasoningCapability{
			ModelID:          "claude-opus-5",
			SupportsThinking: true,
			ThinkingTypes:    []string{"adaptive", "disabled"},
			EffortPath:       ReasoningSchemaOutputConfig,
			Efforts:          []string{"low", "medium", "high"},
		},
		ReasoningKnown: true,
	}
	if _, err := (&Handler{}).buildClaudeKiroCandidateAdditionalFields(body, narrow); err == nil {
		t.Fatal("narrow exact route unexpectedly accepted xhigh")
	}

	wide := narrow
	wide.Reasoning.Efforts = []string{"low", "medium", "high", "xhigh", "max"}
	fields, err := (&Handler{}).buildClaudeKiroCandidateAdditionalFields(body, wide)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]interface{}{
		"thinking":      map[string]interface{}{"type": "adaptive"},
		"output_config": map[string]interface{}{"effort": "xhigh"},
	}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields=%#v want=%#v", fields, want)
	}
}

func TestBuildClaudeKiroCandidateAdditionalFieldsMergesStructuredFormat(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}],
		"output_config":{
			"effort":"high",
			"format":{"type":"json_schema","schema":{"type":"object"}}
		}
	}`)
	route := resolvedProviderModel{
		ProviderID:                 "claude-opus-5",
		KiroStructuredOutputKnown:  true,
		KiroStructuredOutputFormat: true,
		Reasoning: ModelReasoningCapability{
			ModelID:    "claude-opus-5",
			EffortPath: ReasoningSchemaOutputConfig,
			Efforts:    []string{"low", "medium", "high"},
		},
		ReasoningKnown: true,
	}
	fields, err := (&Handler{}).buildClaudeKiroCandidateAdditionalFields(body, route)
	if err != nil {
		t.Fatal(err)
	}
	outputConfig, ok := fields["output_config"].(map[string]interface{})
	if !ok {
		t.Fatalf("output_config=%#v", fields["output_config"])
	}
	if outputConfig["effort"] != "high" {
		t.Fatalf("effort=%#v", outputConfig["effort"])
	}
	format, ok := outputConfig["format"].(map[string]interface{})
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("format=%#v", outputConfig["format"])
	}
}

func TestBuildClaudeKiroCandidateAdditionalFieldsMapsLegacyEnabledUsingExactRoute(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":8192,
		"messages":[{"role":"user","content":"hello"}],
		"thinking":{"type":"enabled","budget_tokens":4096}
	}`)
	route := resolvedProviderModel{
		ProviderID:     "claude-opus-5",
		ReasoningKnown: true,
		Reasoning: ModelReasoningCapability{
			ModelID:          "claude-opus-5",
			SupportsThinking: true,
			ThinkingTypes:    []string{"adaptive", "disabled"},
		},
	}
	fields, err := (&Handler{}).buildClaudeKiroCandidateAdditionalFields(body, route)
	if err != nil {
		t.Fatal(err)
	}
	thinking, ok := fields["thinking"].(map[string]interface{})
	if !ok || thinking["type"] != "adaptive" {
		t.Fatalf("thinking=%#v", fields["thinking"])
	}
	if _, exists := thinking["budget_tokens"]; exists {
		t.Fatalf("unsupported budget_tokens leaked: %#v", thinking)
	}
}

func TestBuildClaudeKiroCandidateAdditionalFieldsRejectsUnknownReasoningEvidence(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}],
		"thinking":{"type":"adaptive"}
	}`)
	route := resolvedProviderModel{ProviderID: "claude-opus-5"}
	if _, err := (&Handler{}).buildClaudeKiroCandidateAdditionalFields(body, route); err == nil {
		t.Fatal("unknown candidate-local reasoning evidence was accepted")
	}
}

func TestBuildClaudeKiroCandidateAdditionalFieldsRejectsUnprovenStructuredFormat(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}],
		"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}
	}`)
	route := resolvedProviderModel{ProviderID: "claude-opus-5", ReasoningKnown: true}
	if _, err := (&Handler{}).buildClaudeKiroCandidateAdditionalFields(body, route); err == nil {
		t.Fatal("unproven candidate-local structured-output capability was accepted")
	}
}
