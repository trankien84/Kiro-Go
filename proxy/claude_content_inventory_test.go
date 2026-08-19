package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func decodeClaudeRequestForInventory(t *testing.T, body string) ClaudeRequest {
	t.Helper()
	var req ClaudeRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return req
}

func TestClaudeContentInventoryPreservesNativeBlocksForRouting(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		kind    claudeContentKind
		allowed claudeTransportMask
	}{
		{
			name:    "base64 document",
			body:    `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}]}]}`,
			kind:    claudeContentDocumentBase64,
			allowed: claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke,
		},
		{
			name:    "url image",
			body:    `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}`,
			kind:    claudeContentImageURL,
			allowed: claudeTransportMaskCustom,
		},
		{
			name:    "search result",
			body:    `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"search_result","source":"https://example.com","title":"Example","content":[{"type":"text","text":"result"}]}]}]}`,
			kind:    claudeContentSearchResult,
			allowed: claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := decodeClaudeRequestForInventory(t, tc.body)
			catalog, reason := completionGuardBuildNormalizedCatalog(&req)
			if reason != "" {
				t.Fatalf("catalog: %s", reason)
			}
			inventory, validationErr := buildClaudeContentInventory(&req, catalog)
			if validationErr != nil {
				t.Fatal(validationErr)
			}
			if !inventory.HasUserContext {
				t.Fatal("native block did not count as user context")
			}
			found := false
			for _, feature := range inventory.Features {
				if feature.Kind == tc.kind {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing kind %s: %#v", tc.kind, inventory.Features)
			}
			semantic, reason := buildClaudeSemanticRequest(&req, nil, []byte(tc.body))
			if reason != "" {
				t.Fatal(reason)
			}
			requirements, validationErr := analyzeClaudeTransportRequirementsWithInventory(
				&req, semantic, req.Model, inventory, catalog,
			)
			if validationErr != nil {
				t.Fatal(validationErr)
			}
			if requirements.Allowed != tc.allowed {
				t.Fatalf("allowed=%08b want=%08b (%s)", requirements.Allowed, tc.allowed, requirements.Summary())
			}
		})
	}
}

func TestClaudeContentInventoryTreatsDataURLAsKiroImage(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`
	req := decodeClaudeRequestForInventory(t, body)
	inventory, validationErr := buildClaudeContentInventory(&req, nil)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if len(inventory.Features) != 1 || inventory.Features[0].Kind != claudeContentImageBase64 {
		t.Fatalf("features=%#v", inventory.Features)
	}
}

func TestClaudeToolResultSearchResultsCannotMixContentTypes(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"run"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"search","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"search_result","source":"https://example.com","title":"x","content":[{"type":"text","text":"x"}]},{"type":"text","text":"mixed"}]}]}]}`
	req := decodeClaudeRequestForInventory(t, body)
	_, validationErr := buildClaudeContentInventory(&req, nil)
	if validationErr == nil || !strings.Contains(validationErr.Message, "cannot mix search_result") {
		t.Fatalf("validation=%#v", validationErr)
	}
}

func TestClaudeFileReferenceRequiresExactFilesBeta(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file_123"}}]}]}`
	req := decodeClaudeRequestForInventory(t, body)
	catalog, reason := completionGuardBuildNormalizedCatalog(&req)
	if reason != "" {
		t.Fatal(reason)
	}
	inventory, validationErr := buildClaudeContentInventory(&req, catalog)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	semantic, reason := buildClaudeSemanticRequest(&req, nil, []byte(body))
	if reason != "" {
		t.Fatal(reason)
	}
	if validationErr := validateClaudeContentBetas(inventory, semantic); validationErr == nil {
		t.Fatal("missing Files API beta was accepted")
	}

	req = decodeClaudeRequestForInventory(t, body)
	semantic, reason = buildClaudeSemanticRequest(&req, http.Header{
		"Anthropic-Beta": []string{"files-api-2025-04-14"},
	}, []byte(body))
	if reason != "" {
		t.Fatal(reason)
	}
	inventory, validationErr = buildClaudeContentInventory(&req, catalog)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if validationErr := validateClaudeContentBetas(inventory, semantic); validationErr != nil {
		t.Fatal(validationErr)
	}
}

func TestClaudeContentInventoryRejectsMalformedKiroImageInsteadOfDroppingIt(t *testing.T) {
	for _, body := range []string{
		`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"not-base64!"}}]}]}`,
		`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,not-base64!"}}]}]}`,
		`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"run"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"capture","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"not-base64!"}}]}]}]}`,
	} {
		req := decodeClaudeRequestForInventory(t, body)
		_, validationErr := buildClaudeContentInventory(&req, nil)
		if validationErr == nil || !strings.Contains(validationErr.Message, "base64") {
			t.Fatalf("malformed image validation=%#v", validationErr)
		}
	}
}

func TestClaudeContentInventoryValidatesToolContinuationIdentity(t *testing.T) {
	badToolUse := decodeClaudeRequestForInventory(t, `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"run"},{"role":"assistant","content":[{"type":"tool_use","id":"","name":"echo","input":{}}]}]}`)
	if _, validationErr := buildClaudeContentInventory(&badToolUse, nil); validationErr == nil || !strings.Contains(validationErr.Message, "id and name") {
		t.Fatalf("tool_use validation=%#v", validationErr)
	}

	badToolResult := decodeClaudeRequestForInventory(t, `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"","content":"done"}]}]}`)
	if _, validationErr := buildClaudeContentInventory(&badToolResult, nil); validationErr == nil || !strings.Contains(validationErr.Message, "tool_use_id") {
		t.Fatalf("tool_result validation=%#v", validationErr)
	}
}

func TestClaudeSearchResultRequiresOfficialTextShape(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"search_result","source":"https://example.com","title":"Example","content":[{"type":"image","text":"wrong"}]}]}]}`
	req := decodeClaudeRequestForInventory(t, body)
	_, validationErr := buildClaudeContentInventory(&req, nil)
	if validationErr == nil || !strings.Contains(validationErr.Message, "non-empty text") {
		t.Fatalf("search_result validation=%#v", validationErr)
	}
}

func TestClaudeContentInventoryRejectsRolesAndContentShapesThatWouldBeDropped(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown role",
			body: `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"developer","content":"hidden"},{"role":"user","content":"hi"}]}`,
			want: ".role must be user, assistant, or system",
		},
		{
			name: "non-object message block",
			body: `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":["hidden"]}]}`,
			want: "must be a content block object",
		},
		{
			name: "non-object system block",
			body: `{"model":"claude-opus-5","max_tokens":64,"system":["hidden"],"messages":[{"role":"user","content":"hi"}]}`,
			want: "must be a content block object",
		},
		{
			name: "user text field wrong type",
			body: `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":{"hidden":true}}]}]}`,
			want: ".text must be a string",
		},
		{
			name: "assistant text field missing",
			body: `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text"}]}]}`,
			want: ".text must be a string",
		},
		{
			name: "empty cached text",
			body: `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"","cache_control":{"type":"ephemeral"}}]}]}`,
			want: "cache_control is not allowed on empty text blocks",
		},
		{
			name: "invalid tool result content",
			body: `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":{"text":"hidden"}}]}]}`,
			want: "must be a string or an array",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := decodeClaudeRequestForInventory(t, tc.body)
			_, validationErr := buildClaudeContentInventory(&req, nil)
			if validationErr == nil || !strings.Contains(validationErr.Message, tc.want) {
				t.Fatalf("validation=%#v want substring %q", validationErr, tc.want)
			}
		})
	}
}

func TestClaudeContentInventoryValidatesSearchResultSubContentMetadata(t *testing.T) {
	cached := decodeClaudeRequestForInventory(t, `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"search_result","source":"https://example.com","title":"Example","content":[{"type":"text","text":"result","cache_control":{"type":"ephemeral"}}]}]}]}`)
	if _, validationErr := buildClaudeContentInventory(&cached, nil); validationErr == nil || !strings.Contains(validationErr.Message, "search-result sub-content") {
		t.Fatalf("cache validation=%#v", validationErr)
	}

	body := `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"search_result","source":"https://example.com","title":"Example","content":[{"type":"text","text":"result","future_field":true}]}]}]}`
	req := decodeClaudeRequestForInventory(t, body)
	catalog, reason := completionGuardBuildNormalizedCatalog(&req)
	if reason != "" {
		t.Fatal(reason)
	}
	inventory, validationErr := buildClaudeContentInventory(&req, catalog)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	semantic, reason := buildClaudeSemanticRequest(&req, nil, []byte(body))
	if reason != "" {
		t.Fatal(reason)
	}
	requirements, validationErr := analyzeClaudeTransportRequirementsWithInventory(&req, semantic, req.Model, inventory, catalog)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("allowed=%08b requirements=%s", requirements.Allowed, requirements.Summary())
	}
}

func TestClaudeContentInventoryRejectsCacheControlOnThinkingBlocks(t *testing.T) {
	for _, block := range []string{
		`{"type":"thinking","thinking":"reason","signature":"sig","cache_control":{"type":"ephemeral"}}`,
		`{"type":"redacted_thinking","data":"opaque","cache_control":{"type":"ephemeral"}}`,
	} {
		body := `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[` + block + `]},{"role":"user","content":"continue"}]}`
		req := decodeClaudeRequestForInventory(t, body)
		_, validationErr := buildClaudeContentInventory(&req, nil)
		if validationErr == nil || !strings.Contains(validationErr.Message, "cache_control is not allowed on thinking blocks") {
			t.Fatalf("block=%s validation=%#v", block, validationErr)
		}
	}
}

func TestClaudeContentMetadataUsesExplicitKiroFidelity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		allowed claudeTransportMask
	}{
		{
			name:    "explicit cache breakpoint",
			body:    `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`,
			allowed: claudeTransportMaskKiro | claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke,
		},
		{
			name:    "unknown text metadata",
			body:    `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hi","future_field":{"v":1}}]}]}`,
			allowed: claudeTransportMaskCustom,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := decodeClaudeRequestForInventory(t, tc.body)
			catalog, reason := completionGuardBuildNormalizedCatalog(&req)
			if reason != "" {
				t.Fatal(reason)
			}
			inventory, validationErr := buildClaudeContentInventory(&req, catalog)
			if validationErr != nil {
				t.Fatal(validationErr)
			}
			semantic, reason := buildClaudeSemanticRequest(&req, nil, []byte(tc.body))
			if reason != "" {
				t.Fatal(reason)
			}
			requirements, validationErr := analyzeClaudeTransportRequirementsWithInventory(
				&req, semantic, req.Model, inventory, catalog,
			)
			if validationErr != nil {
				t.Fatal(validationErr)
			}
			if requirements.Allowed != tc.allowed {
				t.Fatalf("allowed=%08b want=%08b %s", requirements.Allowed, tc.allowed, requirements.Summary())
			}
		})
	}
}

func TestClaudeToolUseCallerRoutingPreservesProgrammaticState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		caller  string
		allowed claudeTransportMask
	}{
		{
			name:    "direct caller remains Kiro compatible",
			caller:  `{"type":"direct"}`,
			allowed: claudeTransportMaskAll,
		},
		{
			name:    "programmatic caller requires native messages",
			caller:  `{"type":"code_execution_20260120","tool_id":"srv_1"}`,
			allowed: claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"private-model","max_tokens":64,"messages":[{"role":"user","content":"run"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"echo","input":{},"caller":` + tc.caller + `}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"done"}]}]}`
			req := decodeClaudeRequestForInventory(t, body)
			catalog, reason := completionGuardBuildNormalizedCatalog(&req)
			if reason != "" {
				t.Fatal(reason)
			}
			inventory, validationErr := buildClaudeContentInventory(&req, catalog)
			if validationErr != nil {
				t.Fatal(validationErr)
			}
			semantic, reason := buildClaudeSemanticRequest(&req, nil, []byte(body))
			if reason != "" {
				t.Fatal(reason)
			}
			requirements, validationErr := analyzeClaudeTransportRequirementsWithInventory(
				&req, semantic, req.Model, inventory, catalog,
			)
			if validationErr != nil {
				t.Fatal(validationErr)
			}
			if requirements.Allowed != tc.allowed {
				t.Fatalf("allowed=%08b want=%08b %s", requirements.Allowed, tc.allowed, requirements.Summary())
			}
		})
	}
}

func TestClaudeToolResultErrorFlagMustBeBoolean(t *testing.T) {
	body := `{"model":"private-model","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"failed","is_error":"yes"}]}]}`
	req := decodeClaudeRequestForInventory(t, body)
	_, validationErr := buildClaudeContentInventory(&req, nil)
	if validationErr == nil || !strings.Contains(validationErr.Message, "is_error must be a boolean") {
		t.Fatalf("validation=%#v", validationErr)
	}
}

func TestClaudeContentInventoryRoutesUnmodeledMediaSourceFieldsTransparently(t *testing.T) {
	for _, body := range []string{
		`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA==","future_source":true}}]}]}`,
		`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA==","detail":"high"}}]}]}`,
	} {
		req := decodeClaudeRequestForInventory(t, body)
		catalog, reason := completionGuardBuildNormalizedCatalog(&req)
		if reason != "" {
			t.Fatal(reason)
		}
		inventory, validationErr := buildClaudeContentInventory(&req, catalog)
		if validationErr != nil {
			t.Fatal(validationErr)
		}
		semantic, reason := buildClaudeSemanticRequest(&req, nil, []byte(body))
		if reason != "" {
			t.Fatal(reason)
		}
		requirements, validationErr := analyzeClaudeTransportRequirementsWithInventory(&req, semantic, req.Model, inventory, catalog)
		if validationErr != nil {
			t.Fatal(validationErr)
		}
		if requirements.Allowed != claudeTransportMaskCustom {
			t.Fatalf("allowed=%08b requirements=%s body=%s", requirements.Allowed, requirements.Summary(), body)
		}
	}
}
