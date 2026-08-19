package proxy

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestModelAdvertisesKiroStructuredOutputFormat(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		want   bool
	}{
		{
			name:   "format advertised",
			schema: `{"type":"object","properties":{"output_config":{"type":"object","properties":{"effort":{"type":"string"},"format":{"type":"object"}}}}}`,
			want:   true,
		},
		{
			name:   "only effort advertised",
			schema: `{"type":"object","properties":{"output_config":{"type":"object","properties":{"effort":{"type":"string"}}}}}`,
			want:   false,
		},
		{
			name:   "no output config",
			schema: `{"type":"object","properties":{"thinking":{"type":"object"}}}`,
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := modelAdvertisesKiroStructuredOutputFormat(ModelInfo{
				ModelId:                            "claude-opus-5",
				AdditionalModelRequestFieldsSchema: json.RawMessage(tt.schema),
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %t, want %t", got, tt.want)
			}
		})
	}
}

func TestMergeKiroOutputConfigFieldPreservesExistingEffort(t *testing.T) {
	fields := map[string]interface{}{
		"output_config": map[string]interface{}{
			"effort": "xhigh",
		},
	}
	format := map[string]interface{}{
		"type": "json_schema",
		"schema": map[string]interface{}{
			"type": "object",
		},
	}

	got := mergeKiroOutputConfigField(fields, "format", format)
	output, _ := got["output_config"].(map[string]interface{})
	if output["effort"] != "xhigh" {
		t.Fatalf("effort lost: %#v", output)
	}
	if !reflect.DeepEqual(output["format"], format) {
		t.Fatalf("format changed: got %#v want %#v", output["format"], format)
	}
}

func TestClaudeKiroStructuredOutputFormatIgnoresMissingAndNull(t *testing.T) {
	if _, ok := claudeKiroStructuredOutputFormat(&ClaudeRequest{}); ok {
		t.Fatal("missing output_config.format reported as requested")
	}

	req := &ClaudeRequest{OutputConfig: &ClaudeOutputConfig{Extra: map[string]json.RawMessage{
		"format": json.RawMessage(`null`),
	}}}
	if _, ok := claudeKiroStructuredOutputFormat(req); ok {
		t.Fatal("null output_config.format reported as requested")
	}
}
