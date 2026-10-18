package proxy

import (
	"fmt"
	"strings"
)

// validateNativeToolsetHistory checks only the extra correlation semantics of
// native browser/computer members. The regular tool graph still owns ordinary
// tool results, programmatic calling, and server tool lifecycle validation.
// We never manufacture tool definitions for native toolset members.
func validateNativeToolsetHistory(req *ClaudeRequest, catalog *completionGuardNormalizedCatalog) string {
	if req == nil {
		return ""
	}
	pending := make(map[string]string)
	for messageIndex, message := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		for blockIndex, block := range claudeContentBlocks(message.Content) {
			blockType, _ := block["type"].(string)
			blockType = strings.ToLower(strings.TrimSpace(blockType))
			path := fmt.Sprintf("messages[%d].content[%d]", messageIndex, blockIndex)
			switch {
			case role == "assistant" && blockType == "tool_use":
				value, present := block["toolset_name"]
				if !present {
					continue
				}
				name, ok := value.(string)
				if !ok || name == "" || catalog == nil || !catalog.NativeToolsets[name] {
					return path + ".toolset_name must reference a declared native toolset"
				}
				id, _ := block["id"].(string)
				if id != "" {
					pending[id] = name
				}
			case role == "user" && blockType == "tool_result":
				id, _ := block["tool_use_id"].(string)
				if expected, found := pending[id]; found {
					actual, ok := block["toolset_name"].(string)
					if !ok || actual != expected {
						return path + ".toolset_name must match the originating tool_use"
					}
					delete(pending, id)
				} else if _, hasToolset := block["toolset_name"]; hasToolset {
					return path + ".toolset_name has no matching native tool_use"
				}
			}
		}
	}
	return ""
}
