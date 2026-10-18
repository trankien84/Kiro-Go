package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/pool"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// handleSelectedKiroWebSearch executes the pure native Anthropic web_search
// server tool on the exact Kiro account selected by the provider-aware Claude
// request planner. It deliberately does not perform its own account selection:
// doing so would bypass AccountSelection lifecycle fencing, session affinity,
// provider capability filtering, and the request's immutable registry snapshot.
// On failure it writes nothing so the caller may safely fail over to another
// compatible candidate.
func (h *Handler) handleSelectedKiroWebSearch(
	ctx context.Context,
	w http.ResponseWriter,
	rawBody []byte,
	model string,
	estimatedInputTokens int,
	apiKeyID string,
	selection *pool.AccountSelection,
	account *config.Account,
) error {
	if h == nil || selection == nil || account == nil {
		return fmt.Errorf("invalid Kiro web_search dispatch state")
	}
	if !account.UsesKiroRuntime() {
		return fmt.Errorf("selected account is not a Kiro MCP transport")
	}
	var req ClaudeRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		return fmt.Errorf("decode native web_search request: %w", err)
	}
	if model != "" {
		req.Model = model
	}
	query := extractSearchQuery(&req)
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("unable to extract web_search query")
	}

	started := time.Now()
	toolUseID, mcpReq := createMcpRequest(query)
	mcpResp, err := callMcpAPIContext(ctx, account, mcpReq)
	if err != nil {
		return err
	}
	if !h.pool.IsSelectionCurrent(*selection) {
		return errStaleAccountSelection
	}
	results := parseSearchResults(mcpResp)
	if results == nil {
		return fmt.Errorf("MCP web_search returned unparseable or error search payload")
	}

	summary := generateSearchSummary(query, results)
	outputTokens := (len([]rune(summary)) + 3) / 4
	if outputTokens < 1 {
		outputTokens = 1
	}
	inputTokens := estimatedInputTokens
	if inputTokens < 0 {
		inputTokens = 0
	}

	h.pool.RecordSuccessForSelection(*selection)
	h.pool.RecordLatencyForSelection(*selection, float64(time.Since(started).Milliseconds()))
	h.pool.UpdateStatsForSelection(*selection, inputTokens+outputTokens, 0)
	h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, 0)
	h.recordSuccessLog("claude", req.Model, account.ID, apiKeyID, inputTokens+outputTokens, 0, time.Since(started).Milliseconds())

	if req.Stream {
		h.streamWebSearchSSE(w, req.Model, query, toolUseID, results, inputTokens, outputTokens)
		return nil
	}

	content := buildWebSearchContentBlocks(query, toolUseID, results)
	messageID := "msg_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	if len(messageID) > 4+24 {
		messageID = messageID[:4+24]
	}
	resp := map[string]interface{}{
		"id":            messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         req.Model,
		"content":       content,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":                inputTokens,
			"output_tokens":               outputTokens,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
			"server_tool_use": map[string]interface{}{
				"web_search_requests": 1,
			},
		},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	return json.NewEncoder(w).Encode(resp)
}
