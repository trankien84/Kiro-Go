package proxy

import "testing"

func TestExtractClaudeUserContentPreservesToolResultErrorStatus(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{
			"type":        "tool_result",
			"tool_use_id": "tool_1",
			"content":     "failed",
			"is_error":    true,
		},
	}
	_, _, results := extractClaudeUserContent(content)
	if len(results) != 1 {
		t.Fatalf("results=%#v", results)
	}
	if results[0].Status != "error" {
		t.Fatalf("status=%q want=error", results[0].Status)
	}
}
