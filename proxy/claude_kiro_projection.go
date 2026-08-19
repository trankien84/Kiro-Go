package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Keep Claude-local names as source-compatible aliases while the fidelity
// ordering itself is shared with the OpenAI projector.
type claudeProjectionFidelity = requestProjectionFidelity

const (
	claudeProjectionExact       = requestProjectionExact
	claudeProjectionEmulated    = requestProjectionEmulated
	claudeProjectionUnsupported = requestProjectionUnsupported
)

type claudeProjectionIssue struct {
	Feature  string
	Fidelity claudeProjectionFidelity
	Detail   string
}

type claudeKiroContextEdit struct {
	Type            string
	TriggerType     string
	TriggerValue    int
	KeepType        string
	KeepValue       int
	KeepAll         bool
	ExcludeTools    map[string]struct{}
	ClearToolInputs bool
}

type claudeKiroContextManagementPlan struct {
	Edits []claudeKiroContextEdit
}

type claudeKiroProjection struct {
	Fidelity          claudeProjectionFidelity
	Issues            []claudeProjectionIssue
	ContextManagement *claudeKiroContextManagementPlan
}

func newClaudeKiroProjection() claudeKiroProjection {
	return claudeKiroProjection{Fidelity: claudeProjectionExact}
}

func (projection *claudeKiroProjection) add(
	feature string,
	fidelity claudeProjectionFidelity,
	detail string,
) {
	if projection == nil {
		return
	}
	projection.Issues = append(projection.Issues, claudeProjectionIssue{
		Feature:  strings.TrimSpace(feature),
		Fidelity: fidelity,
		Detail:   strings.TrimSpace(detail),
	})
	if fidelity > projection.Fidelity {
		projection.Fidelity = fidelity
	}
}

func (projection claudeKiroProjection) AllowsKiro() bool {
	return projection.Fidelity != claudeProjectionUnsupported
}

func (projection claudeKiroProjection) Summary() string {
	if len(projection.Issues) == 0 {
		return projection.Fidelity.String()
	}
	parts := make([]string, 0, len(projection.Issues))
	for _, issue := range projection.Issues {
		part := issue.Feature + "=" + issue.Fidelity.String()
		if issue.Detail != "" {
			part += " (" + issue.Detail + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

func claudeProjectionInt(value interface{}) (int, bool) {
	switch typed := value.(type) {
	case float64:
		if typed < 0 || typed != float64(int(typed)) {
			return 0, false
		}
		return int(typed), true
	case int:
		if typed < 0 {
			return 0, false
		}
		return typed, true
	default:
		return 0, false
	}
}

func claudeProjectionTypedCount(
	value interface{},
	defaultType string,
	defaultValue int,
	allowAll bool,
) (kind string, count int, all bool, reason string) {
	if value == nil {
		return defaultType, defaultValue, false, ""
	}
	if allowAll {
		if text, ok := value.(string); ok && strings.EqualFold(strings.TrimSpace(text), "all") {
			return defaultType, 0, true, ""
		}
	}
	object, ok := value.(map[string]interface{})
	if !ok {
		return "", 0, false, "must be an object"
	}
	kind, _ = object["type"].(string)
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "", 0, false, "type must be a non-empty string"
	}
	count, ok = claudeProjectionInt(object["value"])
	if !ok {
		return "", 0, false, "value must be a non-negative integer"
	}
	return kind, count, false, ""
}

func parseClaudeKiroContextManagement(raw json.RawMessage) (*claudeKiroContextManagementPlan, string) {
	if !claudeRawMeaningful(raw) {
		return nil, ""
	}
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, "context_management must be an object"
	}
	if root == nil {
		return nil, "context_management must be an object"
	}
	for key := range root {
		if key != "edits" {
			return nil, "unmodeled context_management field " + key
		}
	}
	rawEdits, ok := root["edits"].([]interface{})
	if !ok || len(rawEdits) == 0 {
		return nil, "context_management.edits must be a non-empty array"
	}

	plan := &claudeKiroContextManagementPlan{}
	seenToolEdit := false
	for index, rawEdit := range rawEdits {
		object, ok := rawEdit.(map[string]interface{})
		if !ok {
			return nil, fmt.Sprintf("context_management.edits[%d] must be an object", index)
		}
		editType, _ := object["type"].(string)
		editType = strings.TrimSpace(editType)
		switch editType {
		case "clear_thinking_20251015":
			if seenToolEdit {
				return nil, "clear_thinking_20251015 must precede clear_tool_uses_20250919"
			}
			for key := range object {
				if key != "type" && key != "keep" {
					return nil, fmt.Sprintf("context_management.edits[%d].%s is not emulated by Kiro", index, key)
				}
			}
			kind, count, all, reason := claudeProjectionTypedCount(
				object["keep"], "thinking_turns", 0, true,
			)
			if reason != "" {
				return nil, fmt.Sprintf("context_management.edits[%d].keep %s", index, reason)
			}
			if !all && object["keep"] != nil && (kind != "thinking_turns" || count <= 0) {
				return nil, fmt.Sprintf("context_management.edits[%d].keep must be thinking_turns > 0 or all", index)
			}
			plan.Edits = append(plan.Edits, claudeKiroContextEdit{
				Type:      editType,
				KeepType:  kind,
				KeepValue: count,
				KeepAll:   all,
			})

		case "clear_tool_uses_20250919":
			seenToolEdit = true
			for key := range object {
				switch key {
				case "type", "trigger", "keep", "exclude_tools", "clear_tool_inputs":
					// supported below
				case "clear_at_least":
					// The API defines this in token units. Kiro-Go has only a local
					// token estimate, so claiming the minimum would be unsafe.
					return nil, fmt.Sprintf("context_management.edits[%d].clear_at_least cannot be reproduced reliably on Kiro", index)
				default:
					return nil, fmt.Sprintf("context_management.edits[%d].%s is not emulated by Kiro", index, key)
				}
			}
			triggerType, triggerValue, _, reason := claudeProjectionTypedCount(
				object["trigger"], "input_tokens", 100000, false,
			)
			if reason != "" || (triggerType != "input_tokens" && triggerType != "tool_uses") {
				if reason == "" {
					reason = "type must be input_tokens or tool_uses"
				}
				return nil, fmt.Sprintf("context_management.edits[%d].trigger %s", index, reason)
			}
			keepType, keepValue, _, reason := claudeProjectionTypedCount(
				object["keep"], "tool_uses", 3, false,
			)
			if reason != "" || keepType != "tool_uses" {
				if reason == "" {
					reason = "type must be tool_uses"
				}
				return nil, fmt.Sprintf("context_management.edits[%d].keep %s", index, reason)
			}
			excludeTools := make(map[string]struct{})
			if rawExclude := object["exclude_tools"]; rawExclude != nil {
				values, ok := rawExclude.([]interface{})
				if !ok {
					return nil, fmt.Sprintf("context_management.edits[%d].exclude_tools must be an array of strings", index)
				}
				for _, value := range values {
					name, ok := value.(string)
					name = strings.TrimSpace(name)
					if !ok || name == "" {
						return nil, fmt.Sprintf("context_management.edits[%d].exclude_tools must contain non-empty strings", index)
					}
					excludeTools[name] = struct{}{}
				}
			}
			clearToolInputs := false
			if value := object["clear_tool_inputs"]; value != nil {
				var ok bool
				clearToolInputs, ok = value.(bool)
				if !ok {
					return nil, fmt.Sprintf("context_management.edits[%d].clear_tool_inputs must be boolean", index)
				}
			}
			plan.Edits = append(plan.Edits, claudeKiroContextEdit{
				Type:            editType,
				TriggerType:     triggerType,
				TriggerValue:    triggerValue,
				KeepType:        keepType,
				KeepValue:       keepValue,
				ExcludeTools:    excludeTools,
				ClearToolInputs: clearToolInputs,
			})

		default:
			return nil, fmt.Sprintf("context_management.edits[%d].type %q is not emulated by Kiro", index, editType)
		}
	}
	return plan, ""
}

// buildClaudeKiroProjection classifies only semantics that Kiro intentionally
// handles differently from a terminal Anthropic Messages transport. Features
// not listed here keep their fail-closed requirements in the transport/content
// analyzers.
func buildClaudeKiroProjection(
	req *ClaudeRequest,
	inventory claudeContentInventory,
	semantic claudeSemanticRequest,
) claudeKiroProjection {
	projection := newClaudeKiroProjection()
	if req == nil {
		return projection
	}

	if req.Thinking != nil || strings.TrimSpace(req.ReasoningEffort) != "" ||
		(req.OutputConfig != nil && strings.TrimSpace(req.OutputConfig.Effort) != "") {
		projection.add(
			"thinking_effort",
			claudeProjectionEmulated,
			"mapped through the Kiro reasoning adapter",
		)
	}
	if raw, ok := req.Extra["cache_control"]; ok && claudeRawMeaningful(raw) {
		projection.add(
			"cache_control",
			claudeProjectionEmulated,
			"provider caching directive is intentionally omitted on Kiro",
		)
	}
	if raw, ok := req.Extra["metadata"]; ok && claudeRawMeaningful(raw) {
		projection.add(
			"metadata",
			claudeProjectionEmulated,
			"provider abuse/operational metadata is intentionally omitted on Kiro",
		)
	}
	if strings.TrimSpace(semantic.UserProfileID) != "" {
		projection.add(
			"anthropic_user_profile",
			claudeProjectionEmulated,
			"Anthropic provider identity is intentionally omitted on the Kiro runtime",
		)
	}
	if raw, ok := req.Extra["service_tier"]; ok && claudeRawMeaningful(raw) {
		projection.add(
			"service_tier",
			claudeProjectionEmulated,
			"provider QoS selection is intentionally omitted on Kiro",
		)
	}
	if inventory.HasMidConversationSystemText {
		projection.add(
			"mid_conversation_system",
			claudeProjectionEmulated,
			"preserved in chronological order using Kiro conversation priming",
		)
	}
	nestedToolResultImages := 0
	for _, feature := range inventory.Features {
		if feature.SourceType == "cache_control" &&
			(feature.Kind == claudeContentNativeMetadata || feature.Kind == claudeContentTransparentMetadata) {
			projection.add(
				"content_cache_control",
				claudeProjectionEmulated,
				feature.Path+" is intentionally omitted on Kiro",
			)
		}
		if feature.Kind == claudeContentImageBase64 && feature.Container == claudeContentContainerToolResult {
			nestedToolResultImages++
		}
	}
	if nestedToolResultImages > 0 {
		projection.add(
			"nested_tool_result_image",
			claudeProjectionEmulated,
			fmt.Sprintf(
				"%d inline tool_result image(s) are hoisted to userInputMessage.images while toolUseId, status, and text remain structured",
				nestedToolResultImages,
			),
		)
	}

	for _, tool := range req.Tools {
		if raw, ok := tool.Extra["cache_control"]; ok && claudeRawMeaningful(raw) {
			name := strings.TrimSpace(tool.Name)
			if name == "" {
				name = strings.TrimSpace(tool.Type)
			}
			projection.add(
				"tool_cache_control",
				claudeProjectionEmulated,
				"tool "+name+" cache directive is intentionally omitted on Kiro",
			)
		}
	}

	if raw, ok := req.Extra["context_management"]; ok && claudeRawMeaningful(raw) {
		plan, reason := parseClaudeKiroContextManagement(raw)
		if reason != "" {
			projection.add("context_management", claudeProjectionUnsupported, reason)
		} else {
			projection.ContextManagement = plan
			projection.add(
				"context_management",
				claudeProjectionEmulated,
				"known context edits are applied to a Kiro-only request clone",
			)
		}
	}

	// Keep diagnostics deterministic even when feature discovery order changes.
	sort.SliceStable(projection.Issues, func(i, j int) bool {
		return projection.Issues[i].Feature < projection.Issues[j].Feature
	})
	return projection
}

type claudeKiroToolInteraction struct {
	ID                    string
	Name                  string
	AssistantMessageIndex int
	AssistantBlockIndex   int
	UserMessageIndex      int
	UserBlockIndex        int
}

func cloneClaudeRequestLossless(req *ClaudeRequest) (*ClaudeRequest, error) {
	if req == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var cloned ClaudeRequest
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return nil, err
	}
	return &cloned, nil
}

func claudeMessageBlockMaps(content interface{}) []map[string]interface{} {
	blocks, ok := content.([]interface{})
	if !ok {
		return nil
	}
	result := make([]map[string]interface{}, 0, len(blocks))
	for _, raw := range blocks {
		if block, ok := raw.(map[string]interface{}); ok {
			result = append(result, block)
		} else {
			result = append(result, nil)
		}
	}
	return result
}

func collectClaudeKiroToolInteractions(req *ClaudeRequest) []claudeKiroToolInteraction {
	if req == nil {
		return nil
	}
	byID := make(map[string]*claudeKiroToolInteraction)
	order := make([]string, 0)
	for messageIndex := range req.Messages {
		message := &req.Messages[messageIndex]
		role := strings.ToLower(strings.TrimSpace(message.Role))
		blocks := claudeMessageBlockMaps(message.Content)
		for blockIndex, block := range blocks {
			if block == nil {
				continue
			}
			blockType, _ := block["type"].(string)
			switch {
			case role == "assistant" && blockType == "tool_use":
				id, _ := block["id"].(string)
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				name, _ := block["name"].(string)
				interaction := &claudeKiroToolInteraction{
					ID:                    id,
					Name:                  strings.TrimSpace(name),
					AssistantMessageIndex: messageIndex,
					AssistantBlockIndex:   blockIndex,
					UserMessageIndex:      -1,
					UserBlockIndex:        -1,
				}
				byID[id] = interaction
				order = append(order, id)
			case role == "user" && blockType == "tool_result":
				id, _ := block["tool_use_id"].(string)
				id = strings.TrimSpace(id)
				if interaction := byID[id]; interaction != nil {
					interaction.UserMessageIndex = messageIndex
					interaction.UserBlockIndex = blockIndex
				}
			}
		}
	}
	interactions := make([]claudeKiroToolInteraction, 0, len(order))
	for _, id := range order {
		interaction := byID[id]
		if interaction == nil || interaction.UserMessageIndex < 0 {
			continue
		}
		interactions = append(interactions, *interaction)
	}
	return interactions
}

func claudeKiroContextEditActivated(
	req *ClaudeRequest,
	edit claudeKiroContextEdit,
	interactions []claudeKiroToolInteraction,
) bool {
	switch edit.TriggerType {
	case "tool_uses":
		return len(interactions) > edit.TriggerValue
	case "input_tokens":
		return estimateClaudeRequestInputTokens(req) > edit.TriggerValue
	default:
		return false
	}
}

func setClaudeKiroToolResultPlaceholder(
	req *ClaudeRequest,
	interaction claudeKiroToolInteraction,
	clearToolInput bool,
) {
	if req == nil {
		return
	}
	if interaction.UserMessageIndex >= 0 && interaction.UserMessageIndex < len(req.Messages) {
		blocks := claudeMessageBlockMaps(req.Messages[interaction.UserMessageIndex].Content)
		if interaction.UserBlockIndex >= 0 && interaction.UserBlockIndex < len(blocks) && blocks[interaction.UserBlockIndex] != nil {
			blocks[interaction.UserBlockIndex]["content"] = "[Tool result cleared by context management]"
		}
	}
	if !clearToolInput || interaction.AssistantMessageIndex < 0 || interaction.AssistantMessageIndex >= len(req.Messages) {
		return
	}
	blocks := claudeMessageBlockMaps(req.Messages[interaction.AssistantMessageIndex].Content)
	if interaction.AssistantBlockIndex >= 0 && interaction.AssistantBlockIndex < len(blocks) && blocks[interaction.AssistantBlockIndex] != nil {
		blocks[interaction.AssistantBlockIndex]["input"] = map[string]interface{}{}
	}
}

func applyClaudeKiroContextManagement(
	req *ClaudeRequest,
	plan *claudeKiroContextManagementPlan,
) {
	if req == nil || plan == nil {
		return
	}
	for _, edit := range plan.Edits {
		if edit.Type != "clear_tool_uses_20250919" {
			// Kiro already excludes thinking blocks from model-visible history. A
			// clear_thinking edit therefore needs no additional mutation here.
			continue
		}
		interactions := collectClaudeKiroToolInteractions(req)
		if !claudeKiroContextEditActivated(req, edit, interactions) {
			continue
		}
		eligible := make([]claudeKiroToolInteraction, 0, len(interactions))
		for _, interaction := range interactions {
			if _, excluded := edit.ExcludeTools[interaction.Name]; excluded {
				continue
			}
			eligible = append(eligible, interaction)
		}
		clearCount := len(eligible) - edit.KeepValue
		if clearCount <= 0 {
			continue
		}
		for _, interaction := range eligible[:clearCount] {
			setClaudeKiroToolResultPlaceholder(req, interaction, edit.ClearToolInputs)
		}
	}
}

func applyClaudeKiroProjection(
	req *ClaudeRequest,
	projection claudeKiroProjection,
) (*ClaudeRequest, error) {
	if req == nil {
		return nil, nil
	}
	if !projection.AllowsKiro() {
		return nil, fmt.Errorf("Kiro projection is unsupported: %s", projection.Summary())
	}
	if projection.ContextManagement == nil {
		return req, nil
	}
	cloned, err := cloneClaudeRequestLossless(req)
	if err != nil {
		return nil, fmt.Errorf("clone Claude request for Kiro projection: %w", err)
	}
	applyClaudeKiroContextManagement(cloned, projection.ContextManagement)
	return cloned, nil
}
