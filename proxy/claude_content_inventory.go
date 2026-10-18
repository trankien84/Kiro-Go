package proxy

import (
	"encoding/base64"
	"fmt"
	"io"
	"sort"
	"strings"
)

type claudeContentKind string

const (
	claudeContentText                    claudeContentKind = "text"
	claudeContentImageBase64             claudeContentKind = "image_base64"
	claudeContentImageURL                claudeContentKind = "image_url"
	claudeContentImageFile               claudeContentKind = "image_file"
	claudeContentDocumentBase64          claudeContentKind = "document_base64"
	claudeContentDocumentURL             claudeContentKind = "document_url"
	claudeContentDocumentFile            claudeContentKind = "document_file"
	claudeContentSearchResult            claudeContentKind = "search_result"
	claudeContentToolResult              claudeContentKind = "tool_result"
	claudeContentNativeMetadata          claudeContentKind = "native_content_metadata"
	claudeContentTransparentMetadata     claudeContentKind = "transparent_content_metadata"
	claudeContentMidConversationSystem   claudeContentKind = "mid_conversation_system_text"
	claudeContentMidConversationToolAdd  claudeContentKind = "tool_addition"
	claudeContentMidConversationToolDrop claudeContentKind = "tool_removal"
	claudeContentUnknown                 claudeContentKind = "unknown"
)

type claudeContentContainer string

const (
	claudeContentContainerMessage    claudeContentContainer = "message"
	claudeContentContainerToolResult claudeContentContainer = "tool_result"
)

type claudeCitationMode uint8

const (
	claudeCitationDisabled claudeCitationMode = iota
	claudeCitationEnabled
)

type claudeContentFeature struct {
	MessageIndex int
	Path         string
	Role         string
	Kind         claudeContentKind
	SourceType   string
	Container    claudeContentContainer
	ToolUseID    string
	IsError      bool
}

type claudeContentInventory struct {
	Features                     []claudeContentFeature
	HasUserContext               bool
	FinalRole                    string
	HasAssistantPrefill          bool
	HasFinalClientToolUse        bool
	HasMidConversationSystemText bool
	HasMidConversationToolChange bool
	HasInlineToolDefinition      bool
	HasInlineMCPToolset          bool
	HasMCPToolListing            bool
	HasEffortOnlySystem          bool
	UsesFileReferences           bool
	SearchCitationsEnabled       bool
	SearchCitationsDisabled      bool
	HasWebSearchTool             bool
}

func (inventory *claudeContentInventory) add(feature claudeContentFeature) {
	if inventory == nil {
		return
	}
	inventory.Features = append(inventory.Features, feature)
	switch feature.Kind {
	case claudeContentImageFile, claudeContentDocumentFile:
		inventory.UsesFileReferences = true
	case claudeContentMidConversationSystem:
		inventory.HasMidConversationSystemText = true
	case claudeContentMidConversationToolAdd, claudeContentMidConversationToolDrop:
		inventory.HasMidConversationToolChange = true
	}
}

func inspectClaudeContentBlockMetadata(
	inventory *claudeContentInventory,
	messageIndex int,
	path string,
	role string,
	block map[string]interface{},
	coreFields []string,
	nativeFields []string,
) {
	if inventory == nil || block == nil {
		return
	}
	core := make(map[string]struct{}, len(coreFields))
	for _, key := range coreFields {
		core[key] = struct{}{}
	}
	native := make(map[string]struct{}, len(nativeFields))
	for _, key := range nativeFields {
		native[key] = struct{}{}
	}
	keys := make([]string, 0, len(block))
	for key := range block {
		if _, known := core[key]; known {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		kind := claudeContentTransparentMetadata
		if _, supportedByNative := native[key]; supportedByNative {
			kind = claudeContentNativeMetadata
		}
		inventory.add(claudeContentFeature{
			MessageIndex: messageIndex,
			Path:         path + "." + key,
			Role:         role,
			Kind:         kind,
			SourceType:   key,
		})
	}
}

func inspectClaudeToolUseCaller(
	inventory *claudeContentInventory,
	messageIndex int,
	path string,
	callerValue interface{},
) *claudeTransportValidationError {
	if callerValue == nil {
		return nil
	}
	caller, ok := callerValue.(map[string]interface{})
	if !ok {
		return &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: path + ".caller must be an object",
		}
	}
	callerType, _ := caller["type"].(string)
	callerType = strings.ToLower(strings.TrimSpace(callerType))
	switch callerType {
	case "direct":
		inspectClaudeContentBlockMetadata(
			inventory, messageIndex, path+".caller", "assistant", caller,
			[]string{"type"}, nil,
		)
		return nil
	case "code_execution_20260120", "code_execution_20260521":
		toolID, _ := caller["tool_id"].(string)
		if strings.TrimSpace(toolID) == "" {
			return &claudeTransportValidationError{
				Code:    "invalid_request_error",
				Message: path + ".caller.tool_id must be a non-empty string",
			}
		}
		inventory.add(claudeContentFeature{
			MessageIndex: messageIndex,
			Path:         path + ".caller",
			Role:         "assistant",
			Kind:         claudeContentNativeMetadata,
			SourceType:   "caller",
		})
		inspectClaudeContentBlockMetadata(
			inventory, messageIndex, path+".caller", "assistant", caller,
			[]string{"type", "tool_id"}, nil,
		)
		return nil
	default:
		inventory.add(claudeContentFeature{
			MessageIndex: messageIndex,
			Path:         path + ".caller",
			Role:         "assistant",
			Kind:         claudeContentTransparentMetadata,
			SourceType:   "caller",
		})
		return nil
	}
}

func claudeContentSourceType(block map[string]interface{}) string {
	source, _ := block["source"].(map[string]interface{})
	if source == nil {
		return ""
	}
	sourceType, _ := source["type"].(string)
	return strings.ToLower(strings.TrimSpace(sourceType))
}

func claudeContentString(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]interface{}:
		for _, key := range []string{"url", "data", "file_id"} {
			if text, ok := typed[key].(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func claudeImageContentKind(block map[string]interface{}) (claudeContentKind, string) {
	sourceType := claudeContentSourceType(block)
	source, _ := block["source"].(map[string]interface{})
	switch sourceType {
	case "base64":
		return claudeContentImageBase64, sourceType
	case "url":
		if strings.HasPrefix(strings.ToLower(claudeContentString(source["url"])), "data:") {
			return claudeContentImageBase64, "data_url"
		}
		return claudeContentImageURL, sourceType
	case "file":
		return claudeContentImageFile, sourceType
	}

	// Compatibility block shapes used by OpenAI-compatible clients.
	for _, key := range []string{"image_url", "url"} {
		if value, ok := block[key]; ok {
			if strings.HasPrefix(strings.ToLower(claudeContentString(value)), "data:") {
				return claudeContentImageBase64, "data_url"
			}
			return claudeContentImageURL, "url"
		}
	}
	if sourceType == "" {
		return claudeContentUnknown, ""
	}
	return claudeContentUnknown, sourceType
}

func inspectClaudeMediaSourceMetadata(
	inventory *claudeContentInventory,
	messageIndex int,
	path string,
	role string,
	block map[string]interface{},
	kind claudeContentKind,
) {
	if source, ok := block["source"].(map[string]interface{}); ok {
		core := []string{"type"}
		switch kind {
		case claudeContentImageBase64, claudeContentDocumentBase64:
			core = append(core, "media_type", "data")
		case claudeContentImageURL, claudeContentDocumentURL:
			core = append(core, "url")
		case claudeContentImageFile, claudeContentDocumentFile:
			core = append(core, "file_id")
		}
		inspectClaudeContentBlockMetadata(
			inventory, messageIndex, path+".source", role, source, core, nil,
		)
	}
	if imageURL, ok := block["image_url"].(map[string]interface{}); ok {
		inspectClaudeContentBlockMetadata(
			inventory, messageIndex, path+".image_url", role, imageURL,
			[]string{"url"}, nil,
		)
	}
}

func claudeDocumentContentKind(block map[string]interface{}) (claudeContentKind, string) {
	sourceType := claudeContentSourceType(block)
	switch sourceType {
	case "base64":
		return claudeContentDocumentBase64, sourceType
	case "url":
		return claudeContentDocumentURL, sourceType
	case "file":
		return claudeContentDocumentFile, sourceType
	default:
		return claudeContentUnknown, sourceType
	}
}

func claudeValidatedContentBlocks(
	content interface{},
	path string,
) ([]map[string]interface{}, *claudeTransportValidationError) {
	switch blocks := content.(type) {
	case []interface{}:
		result := make([]map[string]interface{}, 0, len(blocks))
		for index, raw := range blocks {
			block, ok := raw.(map[string]interface{})
			if !ok {
				return nil, &claudeTransportValidationError{
					Code:    "invalid_request_error",
					Message: fmt.Sprintf("%s[%d] must be a content block object", path, index),
				}
			}
			result = append(result, block)
		}
		return result, nil
	case []map[string]interface{}:
		return blocks, nil
	case []ClaudeContentBlock:
		result := claudeContentBlocks(blocks)
		if len(result) != len(blocks) {
			return nil, &claudeTransportValidationError{
				Code:    "invalid_request_error",
				Message: path + " contains a content block that cannot be encoded",
			}
		}
		return result, nil
	default:
		return nil, &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: path + " must be a string or an array of content block objects",
		}
	}
}

func claudeNonEmptyStringField(value map[string]interface{}, key string) string {
	if value == nil {
		return ""
	}
	text, _ := value[key].(string)
	return strings.TrimSpace(text)
}

func claudeBase64PayloadValid(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		reader := base64.NewDecoder(encoding, strings.NewReader(value))
		decoded, err := io.Copy(io.Discard, reader)
		if err == nil && decoded > 0 {
			return true
		}
	}
	return false
}

func claudeDataImageURLValid(value string) bool {
	value = strings.TrimSpace(value)
	comma := strings.IndexByte(value, ',')
	if comma <= 0 || comma == len(value)-1 {
		return false
	}
	metadata := strings.ToLower(value[:comma])
	if !strings.HasPrefix(metadata, "data:image/") || !strings.Contains(metadata, ";base64") {
		return false
	}
	return claudeBase64PayloadValid(value[comma+1:])
}

func claudeImageSourceValue(block map[string]interface{}) string {
	if source, ok := block["source"].(map[string]interface{}); ok {
		for _, key := range []string{"data", "url", "file_id"} {
			if value := claudeNonEmptyStringField(source, key); value != "" {
				return value
			}
		}
	}
	for _, key := range []string{"image_url", "url", "data"} {
		if value, exists := block[key]; exists {
			if text := claudeContentString(value); text != "" {
				return text
			}
		}
	}
	return ""
}

func validateClaudeImageBlock(
	block map[string]interface{},
	path string,
	kind claudeContentKind,
	sourceType string,
) *claudeTransportValidationError {
	value := claudeImageSourceValue(block)
	source, _ := block["source"].(map[string]interface{})
	switch kind {
	case claudeContentImageBase64:
		if sourceType == "base64" {
			mediaType := claudeNonEmptyStringField(source, "media_type")
			if mediaType == "" {
				return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".source.media_type must be a non-empty image media type"}
			}
			if !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
				return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".source.media_type must start with image/"}
			}
			if !claudeBase64PayloadValid(value) {
				return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".source.data must contain valid non-empty base64 image data"}
			}
			return nil
		}
		if !claudeDataImageURLValid(value) {
			return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + " must contain a valid base64 image data URL"}
		}
	case claudeContentImageURL:
		if value == "" {
			return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + " must contain a non-empty image URL"}
		}
	case claudeContentImageFile:
		if claudeNonEmptyStringField(source, "file_id") == "" {
			return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".source.file_id must be a non-empty string"}
		}
	}
	return nil
}

func validateClaudeDocumentBlock(
	block map[string]interface{},
	path string,
	kind claudeContentKind,
) *claudeTransportValidationError {
	source, _ := block["source"].(map[string]interface{})
	switch kind {
	case claudeContentDocumentBase64:
		if claudeNonEmptyStringField(source, "media_type") == "" {
			return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".source.media_type must be a non-empty string"}
		}
		if !claudeBase64PayloadValid(claudeNonEmptyStringField(source, "data")) {
			return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".source.data must contain valid non-empty base64 document data"}
		}
	case claudeContentDocumentURL:
		if claudeNonEmptyStringField(source, "url") == "" {
			return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".source.url must be a non-empty string"}
		}
	case claudeContentDocumentFile:
		if claudeNonEmptyStringField(source, "file_id") == "" {
			return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".source.file_id must be a non-empty string"}
		}
	}
	return nil
}

func validateClaudeTextBlock(
	block map[string]interface{},
	path string,
) (string, *claudeTransportValidationError) {
	text, ok := block["text"].(string)
	if !ok {
		return "", &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: path + ".text must be a string",
		}
	}
	if _, cached := block["cache_control"]; cached && strings.TrimSpace(text) == "" {
		return "", &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: path + ".cache_control is not allowed on empty text blocks",
		}
	}
	return text, nil
}

func validateClaudeSearchResultBlock(
	block map[string]interface{},
	path string,
) *claudeTransportValidationError {
	if claudeNonEmptyStringField(block, "source") == "" {
		return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".source must be a non-empty string"}
	}
	if claudeNonEmptyStringField(block, "title") == "" {
		return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".title must be a non-empty string"}
	}
	content, ok := block["content"].([]interface{})
	if !ok || len(content) == 0 {
		return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".content must be a non-empty array of text blocks"}
	}
	for index, raw := range content {
		textBlock, ok := raw.(map[string]interface{})
		if !ok {
			return &claudeTransportValidationError{Code: "invalid_request_error", Message: fmt.Sprintf("%s.content[%d] must be a text block", path, index)}
		}
		blockType, _ := textBlock["type"].(string)
		if !strings.EqualFold(strings.TrimSpace(blockType), "text") || claudeNonEmptyStringField(textBlock, "text") == "" {
			return &claudeTransportValidationError{Code: "invalid_request_error", Message: fmt.Sprintf("%s.content[%d] must contain non-empty text", path, index)}
		}
	}
	return nil
}

func claudeSearchResultCitationMode(
	block map[string]interface{},
	path string,
) (claudeCitationMode, *claudeTransportValidationError) {
	raw, present := block["citations"]
	if !present {
		return claudeCitationDisabled, nil
	}
	citations, ok := raw.(map[string]interface{})
	if !ok {
		return claudeCitationDisabled, &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: path + ".citations must be an object",
		}
	}
	for key := range citations {
		if key != "enabled" {
			return claudeCitationDisabled, &claudeTransportValidationError{
				Code:    "invalid_request_error",
				Message: path + ".citations." + key + " is not supported",
			}
		}
	}
	enabled, ok := citations["enabled"].(bool)
	if !ok {
		return claudeCitationDisabled, &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: path + ".citations.enabled must be a boolean",
		}
	}
	if enabled {
		return claudeCitationEnabled, nil
	}
	return claudeCitationDisabled, nil
}

func (inventory *claudeContentInventory) recordSearchCitation(mode claudeCitationMode) {
	if inventory == nil {
		return
	}
	if mode == claudeCitationEnabled {
		inventory.SearchCitationsEnabled = true
	} else {
		inventory.SearchCitationsDisabled = true
	}
}

func inspectClaudeSearchResultContent(
	inventory *claudeContentInventory,
	messageIndex int,
	path string,
	role string,
	block map[string]interface{},
) *claudeTransportValidationError {
	content, _ := block["content"].([]interface{})
	for index, raw := range content {
		textBlock, _ := raw.(map[string]interface{})
		blockPath := fmt.Sprintf("%s.content[%d]", path, index)
		if _, present := textBlock["cache_control"]; present {
			return &claudeTransportValidationError{
				Code:    "invalid_request_error",
				Message: blockPath + ".cache_control is not allowed on search-result sub-content blocks",
			}
		}
		inspectClaudeContentBlockMetadata(
			inventory, messageIndex, blockPath, role, textBlock,
			[]string{"type", "text"}, nil,
		)
	}
	return nil
}

func inspectClaudeToolResultContent(
	inventory *claudeContentInventory,
	messageIndex int,
	path string,
	toolUseID string,
	isError bool,
	content interface{},
) *claudeTransportValidationError {
	if text, ok := content.(string); ok {
		if strings.TrimSpace(text) != "" {
			inventory.add(claudeContentFeature{
				MessageIndex: messageIndex,
				Path:         path,
				Role:         "user",
				Kind:         claudeContentText,
				Container:    claudeContentContainerToolResult,
				ToolUseID:    toolUseID,
				IsError:      isError,
			})
		}
		return nil
	}

	blocks, validationErr := claudeValidatedContentBlocks(content, path)
	if validationErr != nil {
		return validationErr
	}
	searchCount := 0
	for _, block := range blocks {
		blockType, _ := block["type"].(string)
		if strings.EqualFold(strings.TrimSpace(blockType), "search_result") {
			searchCount++
		}
	}
	if searchCount > 0 && searchCount != len(blocks) {
		return &claudeTransportValidationError{
			Code: "invalid_request_error",
			Message: fmt.Sprintf(
				"%s cannot mix search_result with other content block types",
				path,
			),
		}
	}

	for index, block := range blocks {
		blockPath := fmt.Sprintf("%s[%d]", path, index)
		blockType, _ := block["type"].(string)
		blockType = strings.ToLower(strings.TrimSpace(blockType))
		switch blockType {
		case "text", "input_text":
			if _, validationErr := validateClaudeTextBlock(block, blockPath); validationErr != nil {
				return validationErr
			}
			inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: blockPath, Role: "user", Kind: claudeContentText, Container: claudeContentContainerToolResult, ToolUseID: toolUseID, IsError: isError})
			inspectClaudeContentBlockMetadata(inventory, messageIndex, blockPath, "user", block, []string{"type", "text"}, []string{"cache_control"})
		case "image", "image_url", "input_image":
			kind, sourceType := claudeImageContentKind(block)
			if validationErr := validateClaudeImageBlock(block, blockPath, kind, sourceType); validationErr != nil {
				return validationErr
			}
			inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: blockPath, Role: "user", Kind: kind, SourceType: sourceType, Container: claudeContentContainerToolResult, ToolUseID: toolUseID, IsError: isError})
			inspectClaudeMediaSourceMetadata(inventory, messageIndex, blockPath, "user", block, kind)
			inspectClaudeContentBlockMetadata(inventory, messageIndex, blockPath, "user", block, []string{"type", "source", "image_url", "url", "data"}, []string{"cache_control"})
		case "document":
			kind, sourceType := claudeDocumentContentKind(block)
			if validationErr := validateClaudeDocumentBlock(block, blockPath, kind); validationErr != nil {
				return validationErr
			}
			inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: blockPath, Role: "user", Kind: kind, SourceType: sourceType, Container: claudeContentContainerToolResult, ToolUseID: toolUseID, IsError: isError})
			inspectClaudeMediaSourceMetadata(inventory, messageIndex, blockPath, "user", block, kind)
			inspectClaudeContentBlockMetadata(inventory, messageIndex, blockPath, "user", block, []string{"type", "source", "title", "context", "citations"}, []string{"cache_control"})
		case "search_result":
			if validationErr := validateClaudeSearchResultBlock(block, blockPath); validationErr != nil {
				return validationErr
			}
			citationMode, validationErr := claudeSearchResultCitationMode(block, blockPath)
			if validationErr != nil {
				return validationErr
			}
			inventory.recordSearchCitation(citationMode)
			if validationErr := inspectClaudeSearchResultContent(inventory, messageIndex, blockPath, "user", block); validationErr != nil {
				return validationErr
			}
			inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: blockPath, Role: "user", Kind: claudeContentSearchResult, Container: claudeContentContainerToolResult, ToolUseID: toolUseID, IsError: isError})
			inspectClaudeContentBlockMetadata(inventory, messageIndex, blockPath, "user", block, []string{"type", "source", "title", "content", "citations"}, []string{"cache_control"})
		default:
			inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: blockPath, Role: "user", Kind: claudeContentUnknown, SourceType: blockType, Container: claudeContentContainerToolResult, ToolUseID: toolUseID, IsError: isError})
		}
	}
	return nil
}

// Structural validation is provider-neutral. Resolution against the declaration
// state at *this* point in history happens in validateClaudeInlineToolHistory.
func validateClaudeMidConversationToolReference(block map[string]interface{}, path string, _ *completionGuardNormalizedCatalog) *claudeTransportValidationError {
	tool, ok := block["tool"].(map[string]interface{})
	if !ok || tool == nil {
		return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".tool must be an object"}
	}
	kind, _ := tool["type"].(string)
	switch kind {
	case "tool_reference", "mcp_tool_reference":
		name, _ := tool["name"].(string)
		if strings.TrimSpace(name) == "" {
			return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".tool.name must be non-empty"}
		}
		fallthrough
	case "mcp_toolset_reference":
		if kind == "mcp_toolset_reference" || kind == "mcp_tool_reference" {
			server, _ := tool["server_name"].(string)
			if strings.TrimSpace(server) == "" {
				return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".tool.server_name must be non-empty"}
			}
		}
	case "tool_definition":
		if _, ok := tool["definition"].(map[string]interface{}); !ok {
			return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".tool.definition must be an object"}
		}
	default:
		return &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".tool.type is unsupported"}
	}
	return nil
}

func claudeAssistantMessageIsPrefill(message ClaudeMessage) bool {
	if !strings.EqualFold(strings.TrimSpace(message.Role), "assistant") {
		return false
	}
	if _, ok := message.Content.(string); ok {
		return true
	}
	blocks := claudeContentBlocks(message.Content)
	if len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		blockType, _ := block["type"].(string)
		switch strings.ToLower(strings.TrimSpace(blockType)) {
		case "text":
			continue
		default:
			// A final assistant message containing tool or provider state is a
			// continuation/replay (for example pause_turn), not an assistant
			// text prefill. Its transport requirements are derived separately.
			return false
		}
	}
	return true
}

func claudeAssistantMessageHasClientToolUse(message ClaudeMessage) bool {
	if !strings.EqualFold(strings.TrimSpace(message.Role), "assistant") {
		return false
	}
	for _, block := range claudeContentBlocks(message.Content) {
		blockType, _ := block["type"].(string)
		if strings.EqualFold(strings.TrimSpace(blockType), "tool_use") {
			return true
		}
	}
	return false
}

func claudeRequestHasMidConversationToolChangesRaw(messages []ClaudeMessage) bool {
	for _, message := range messages {
		if !strings.EqualFold(strings.TrimSpace(message.Role), "system") {
			continue
		}
		for _, block := range claudeContentBlocks(message.Content) {
			blockType, _ := block["type"].(string)
			switch strings.ToLower(strings.TrimSpace(blockType)) {
			case "tool_addition", "tool_removal":
				return true
			}
		}
	}
	return false
}

func buildClaudeContentInventory(
	req *ClaudeRequest,
	catalog *completionGuardNormalizedCatalog,
) (claudeContentInventory, *claudeTransportValidationError) {
	inventory := claudeContentInventory{}
	if req == nil {
		return inventory, nil
	}

	// The top-level system field is protocol content too. Kiro can preserve text
	// blocks, but any future/non-text block must use a transparent transport
	// rather than being silently discarded by extractSystemPrompt.
	if systemText, ok := req.System.(string); ok {
		if strings.TrimSpace(systemText) != "" {
			inventory.add(claudeContentFeature{Path: "system", Role: "system", Kind: claudeContentText})
		}
	} else if req.System != nil {
		blocks, validationErr := claudeValidatedContentBlocks(req.System, "system")
		if validationErr != nil {
			return inventory, validationErr
		}
		for blockIndex, block := range blocks {
			path := fmt.Sprintf("system[%d]", blockIndex)
			blockType, _ := block["type"].(string)
			blockType = strings.ToLower(strings.TrimSpace(blockType))
			if blockType == "text" {
				if _, validationErr := validateClaudeTextBlock(block, path); validationErr != nil {
					return inventory, validationErr
				}
				inventory.add(claudeContentFeature{Path: path, Role: "system", Kind: claudeContentText})
				inspectClaudeContentBlockMetadata(&inventory, -1, path, "system", block, []string{"type", "text"}, []string{"cache_control"})
				continue
			}
			inventory.add(claudeContentFeature{Path: path, Role: "system", Kind: claudeContentUnknown, SourceType: blockType})
		}
	}

	for messageIndex, message := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		switch role {
		case "user", "assistant", "system":
			inventory.FinalRole = role
		default:
			return inventory, &claudeTransportValidationError{
				Code:    "invalid_request_error",
				Message: fmt.Sprintf("messages[%d].role must be user, assistant, or system", messageIndex),
			}
		}
		if role == "system" && message.Extra != nil {
			if raw, ok := message.Extra["output_config"]; ok && claudeRawMeaningful(raw) {
				inventory.HasEffortOnlySystem = true
			}
		}
		if len(message.Extra) > 0 {
			keys := make([]string, 0, len(message.Extra))
			for key, raw := range message.Extra {
				if claudeRawMeaningful(raw) {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			for _, key := range keys {
				kind := claudeContentTransparentMetadata
				if role == "system" && key == "clear_at" {
					kind = claudeContentNativeMetadata
				}
				inventory.add(claudeContentFeature{
					MessageIndex: messageIndex,
					Path:         fmt.Sprintf("messages[%d].%s", messageIndex, key),
					Role:         role,
					Kind:         kind,
					SourceType:   key,
				})
			}
		}
		if text, ok := message.Content.(string); ok {
			if strings.TrimSpace(text) != "" {
				kind := claudeContentText
				if role == "system" {
					kind = claudeContentMidConversationSystem
				}
				inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: fmt.Sprintf("messages[%d].content", messageIndex), Role: role, Kind: kind})
				if role == "user" {
					inventory.HasUserContext = true
				}
			}
			continue
		}

		blocks, validationErr := claudeValidatedContentBlocks(
			message.Content,
			fmt.Sprintf("messages[%d].content", messageIndex),
		)
		if validationErr != nil {
			return inventory, validationErr
		}
		for blockIndex, block := range blocks {
			path := fmt.Sprintf("messages[%d].content[%d]", messageIndex, blockIndex)
			blockType, _ := block["type"].(string)
			blockType = strings.ToLower(strings.TrimSpace(blockType))

			if role == "system" {
				switch blockType {
				case "text":
					if _, validationErr := validateClaudeTextBlock(block, path); validationErr != nil {
						return inventory, validationErr
					}
					inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: path, Role: role, Kind: claudeContentMidConversationSystem})
					inspectClaudeContentBlockMetadata(&inventory, messageIndex, path, role, block, []string{"type", "text"}, []string{"cache_control"})
				case "tool_addition", "tool_removal":
					if validationErr := validateClaudeMidConversationToolReference(block, path, catalog); validationErr != nil {
						return inventory, validationErr
					}
					kind := claudeContentMidConversationToolAdd
					if blockType == "tool_removal" {
						kind = claudeContentMidConversationToolDrop
					}
					if blockType == "tool_addition" {
						tool, _ := block["tool"].(map[string]interface{})
						if tool["type"] == "tool_definition" {
							inventory.HasInlineToolDefinition = true
							definition, _ := tool["definition"].(map[string]interface{})
							if definition["type"] == "mcp_toolset" {
								inventory.HasInlineMCPToolset = true
							}
						}
					}
					inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: path, Role: role, Kind: kind})
					inspectClaudeContentBlockMetadata(&inventory, messageIndex, path, role, block, []string{"type", "tool"}, []string{"cache_control"})
				default:
					inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: path, Role: role, Kind: claudeContentUnknown, SourceType: blockType})
				}
				continue
			}

			if role == "assistant" {
				if blockType == "mcp_tool_listing" {
					server, _ := block["mcp_server_name"].(string)
					_, ok := block["tools"].([]interface{})
					if strings.TrimSpace(server) == "" || !ok {
						return inventory, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + " must contain mcp_server_name and tools array"}
					}
					if catalog != nil {
						if _, known := catalog.MCPServersByName[server]; !known {
							return inventory, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + " references undeclared MCP server"}
						}
					}
					inventory.HasMCPToolListing = true
					inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: path, Role: role, Kind: claudeContentNativeMetadata, SourceType: "mcp_tool_listing"})
					continue
				}
				switch blockType {
				case "text":
					if _, validationErr := validateClaudeTextBlock(block, path); validationErr != nil {
						return inventory, validationErr
					}
					inspectClaudeContentBlockMetadata(&inventory, messageIndex, path, role, block, []string{"type", "text"}, []string{"cache_control"})
				case "tool_use":
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" {
						return inventory, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + " requires non-empty id and name"}
					}
					if _, ok := block["input"].(map[string]interface{}); !ok {
						return inventory, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".input must be an object"}
					}
					if validationErr := inspectClaudeToolUseCaller(&inventory, messageIndex, path, block["caller"]); validationErr != nil {
						return inventory, validationErr
					}
					inspectClaudeContentBlockMetadata(&inventory, messageIndex, path, role, block, []string{"type", "id", "name", "input", "caller"}, []string{"cache_control"})
				case "thinking":
					if _, present := block["cache_control"]; present {
						return inventory, &claudeTransportValidationError{
							Code:    "invalid_request_error",
							Message: path + ".cache_control is not allowed on thinking blocks",
						}
					}
					inspectClaudeContentBlockMetadata(&inventory, messageIndex, path, role, block, []string{"type", "thinking", "signature"}, nil)
				case "redacted_thinking":
					if _, present := block["cache_control"]; present {
						return inventory, &claudeTransportValidationError{
							Code:    "invalid_request_error",
							Message: path + ".cache_control is not allowed on thinking blocks",
						}
					}
					inspectClaudeContentBlockMetadata(&inventory, messageIndex, path, role, block, []string{"type", "data"}, nil)
				}
				continue
			}

			if role != "user" {
				continue
			}
			switch blockType {
			case "text", "input_text":
				text, validationErr := validateClaudeTextBlock(block, path)
				if validationErr != nil {
					return inventory, validationErr
				}
				if strings.TrimSpace(text) != "" {
					inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: path, Role: role, Kind: claudeContentText})
					inspectClaudeContentBlockMetadata(&inventory, messageIndex, path, role, block, []string{"type", "text"}, []string{"cache_control"})
					inventory.HasUserContext = true
				}
			case "image", "image_url", "input_image":
				kind, sourceType := claudeImageContentKind(block)
				if validationErr := validateClaudeImageBlock(block, path, kind, sourceType); validationErr != nil {
					return inventory, validationErr
				}
				inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: path, Role: role, Kind: kind, SourceType: sourceType})
				inspectClaudeMediaSourceMetadata(&inventory, messageIndex, path, role, block, kind)
				inspectClaudeContentBlockMetadata(&inventory, messageIndex, path, role, block, []string{"type", "source", "image_url", "url", "data"}, []string{"cache_control"})
				inventory.HasUserContext = true
			case "document":
				kind, sourceType := claudeDocumentContentKind(block)
				if validationErr := validateClaudeDocumentBlock(block, path, kind); validationErr != nil {
					return inventory, validationErr
				}
				inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: path, Role: role, Kind: kind, SourceType: sourceType})
				inspectClaudeMediaSourceMetadata(&inventory, messageIndex, path, role, block, kind)
				inspectClaudeContentBlockMetadata(&inventory, messageIndex, path, role, block, []string{"type", "source", "title", "context", "citations"}, []string{"cache_control"})
				inventory.HasUserContext = true
			case "search_result":
				if validationErr := validateClaudeSearchResultBlock(block, path); validationErr != nil {
					return inventory, validationErr
				}
				citationMode, validationErr := claudeSearchResultCitationMode(block, path)
				if validationErr != nil {
					return inventory, validationErr
				}
				inventory.recordSearchCitation(citationMode)
				if validationErr := inspectClaudeSearchResultContent(&inventory, messageIndex, path, role, block); validationErr != nil {
					return inventory, validationErr
				}
				inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: path, Role: role, Kind: claudeContentSearchResult})
				inspectClaudeContentBlockMetadata(&inventory, messageIndex, path, role, block, []string{"type", "source", "title", "content", "citations"}, []string{"cache_control"})
				inventory.HasUserContext = true
			case "tool_result":
				toolUseID, _ := block["tool_use_id"].(string)
				if strings.TrimSpace(toolUseID) == "" {
					return inventory, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".tool_use_id must be a non-empty string"}
				}
				if isError, present := block["is_error"]; present {
					if _, ok := isError.(bool); !ok {
						return inventory, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".is_error must be a boolean"}
					}
				}
				isErrorValue, _ := block["is_error"].(bool)
				inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: path, Role: role, Kind: claudeContentToolResult, Container: claudeContentContainerMessage, ToolUseID: toolUseID, IsError: isErrorValue})
				inspectClaudeContentBlockMetadata(&inventory, messageIndex, path, role, block, []string{"type", "tool_use_id", "content", "is_error"}, []string{"cache_control"})
				inventory.HasUserContext = true
				if validationErr := inspectClaudeToolResultContent(&inventory, messageIndex, path+".content", toolUseID, isErrorValue, block["content"]); validationErr != nil {
					return inventory, validationErr
				}
			default:
				inventory.add(claudeContentFeature{MessageIndex: messageIndex, Path: path, Role: role, Kind: claudeContentUnknown, SourceType: blockType})
				inventory.HasUserContext = true
			}
		}
	}
	if len(req.Messages) > 0 {
		finalMessage := req.Messages[len(req.Messages)-1]
		inventory.HasAssistantPrefill = claudeAssistantMessageIsPrefill(finalMessage)
		inventory.HasFinalClientToolUse = claudeAssistantMessageHasClientToolUse(finalMessage)
	}
	if catalog != nil {
		for _, entry := range catalog.Entries {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(entry.Type)), "web_search") {
				inventory.HasWebSearchTool = true
				break
			}
		}
	}
	if inventory.SearchCitationsEnabled && inventory.SearchCitationsDisabled {
		return inventory, &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: "all search_result blocks in a request must use the same citations.enabled setting",
		}
	}
	if inventory.HasWebSearchTool && inventory.SearchCitationsDisabled {
		return inventory, &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: "search_result blocks must enable citations when a web_search tool is active",
		}
	}
	return inventory, nil
}

func validateClaudeContentBetas(
	inventory claudeContentInventory,
	semantic claudeSemanticRequest,
) *claudeTransportValidationError {
	if inventory.UsesFileReferences && !semantic.Betas.Contains("files-api-2025-04-14") {
		return &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: "file_id content sources require anthropic-beta files-api-2025-04-14",
		}
	}
	if inventory.HasMidConversationToolChange && !semantic.Betas.Contains("mid-conversation-tool-changes-2026-07-01") && !semantic.Betas.Contains("inline-tools-2026-09-15") {
		return &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: "tool_addition and tool_removal require anthropic-beta mid-conversation-tool-changes-2026-07-01 or inline-tools-2026-09-15",
		}
	}
	if inventory.HasInlineToolDefinition && !semantic.Betas.Contains("inline-tools-2026-09-15") {
		return &claudeTransportValidationError{Code: "invalid_request_error", Message: "tool_definition requires anthropic-beta inline-tools-2026-09-15"}
	}
	if inventory.HasInlineMCPToolset && !semantic.Betas.Contains("mcp-client-2026-09-15") {
		return &claudeTransportValidationError{Code: "invalid_request_error", Message: "inline mcp_toolset requires anthropic-beta mcp-client-2026-09-15"}
	}
	if inventory.HasMCPToolListing && !semantic.Betas.Contains("mcp-client-2026-09-15") {
		return &claudeTransportValidationError{Code: "invalid_request_error", Message: "mcp_tool_listing requires anthropic-beta mcp-client-2026-09-15"}
	}
	return nil
}

func claudeContentTransportRequirements(
	requirements *claudeTransportRequirements,
	inventory claudeContentInventory,
) {
	for _, feature := range inventory.Features {
		switch feature.Kind {
		case claudeContentText:
			continue
		case claudeContentToolResult:
			if feature.IsError {
				requirements.Require(
					"tool_result_error_status",
					claudeTransportMaskKiro|claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
					feature.Path+" uses is_error, whose model-specific Converse support is not proven",
				)
			}
			continue
		case claudeContentNativeMetadata:
			allowed := claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke
			reason := feature.Path + " is not preserved by the Kiro or Converse adapters"
			if feature.SourceType == "cache_control" {
				allowed |= claudeTransportMaskKiro
				reason = feature.Path + " is intentionally omitted by Kiro as provider caching metadata"
			}
			requirements.Require(
				"native_content_metadata:"+feature.SourceType,
				allowed,
				reason,
			)
		case claudeContentTransparentMetadata:
			allowed := claudeTransportMaskCustom
			reason := feature.Path + " is an unmodeled content-block field"
			if feature.SourceType == "cache_control" {
				allowed |= claudeTransportMaskKiro
				reason = feature.Path + " is intentionally omitted by Kiro as provider caching metadata"
			}
			requirements.Require(
				"transparent_content_metadata:"+feature.SourceType,
				allowed,
				reason,
			)
		case claudeContentImageBase64:
			if feature.Container == claudeContentContainerToolResult {
				requirements.Require(
					"nested_tool_result_media",
					claudeTransportMaskKiro|claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
					feature.Path+" is emulated on Kiro by keeping the structured tool result and hoisting inline image bytes to userInputMessage.images; model-specific Converse support is not proven",
				)
				continue
			}
			// Top-level base64 images are explicitly supported by all adapters.
			continue
		case claudeContentImageURL, claudeContentImageFile,
			claudeContentDocumentURL, claudeContentDocumentFile:
			requirements.Require(
				"transparent_content:"+string(feature.Kind),
				claudeTransportMaskCustom,
				feature.Path+" requires Claude-hosted URL/file semantics",
			)
		case claudeContentDocumentBase64, claudeContentSearchResult:
			requirements.Require(
				"native_content:"+string(feature.Kind),
				claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
				feature.Path+" is not representable by the Kiro or Converse adapters",
			)
		case claudeContentMidConversationSystem,
			claudeContentMidConversationToolAdd,
			claudeContentMidConversationToolDrop:
			// Model capability and beta validation add the precise requirements.
			continue
		case claudeContentUnknown:
			name := feature.SourceType
			if name == "" {
				name = "unknown"
			}
			requirements.Require(
				"transparent_unknown_content:"+name,
				claudeTransportMaskCustom,
				feature.Path+" contains an unmodeled Claude content block",
			)
		}
	}
}

type claudeTranslationError struct {
	Path   string
	Kind   claudeContentKind
	Reason string
}

func (err *claudeTranslationError) Error() string {
	if err == nil {
		return ""
	}
	if err.Path == "" {
		return err.Reason
	}
	return fmt.Sprintf("%s: %s", err.Path, err.Reason)
}

func claudeKiroContentCompatibilityError(
	inventory claudeContentInventory,
) *claudeTranslationError {
	for _, feature := range inventory.Features {
		switch feature.Kind {
		case claudeContentText, claudeContentToolResult, claudeContentMidConversationSystem:
			continue
		case claudeContentNativeMetadata, claudeContentTransparentMetadata:
			if feature.SourceType == "cache_control" {
				continue
			}
			return &claudeTranslationError{
				Path:   feature.Path,
				Kind:   feature.Kind,
				Reason: "content metadata is not representable by the Kiro adapter",
			}
		case claudeContentImageBase64:
			// Inline images inside tool_result are a deliberate Kiro emulation:
			// the toolUseId/status/text remain structured while image bytes are
			// hoisted to the same Kiro user turn's images array. Validation above
			// already guarantees that only decodable inline base64/data-URL images
			// reach this path. URL/file-backed media remains fail-closed below.
			continue
		default:
			return &claudeTranslationError{
				Path:   feature.Path,
				Kind:   feature.Kind,
				Reason: "content block is not losslessly representable by the Kiro adapter",
			}
		}
	}
	return nil
}

func ClaudeToKiroChecked(
	req *ClaudeRequest,
	thinking bool,
	inventory claudeContentInventory,
) (*KiroPayload, error) {
	projection := buildClaudeKiroProjection(req, inventory, claudeSemanticRequest{})
	return ClaudeToKiroCheckedWithProjection(req, thinking, inventory, projection)
}

func ClaudeToKiroCheckedWithProjection(
	req *ClaudeRequest,
	thinking bool,
	inventory claudeContentInventory,
	projection claudeKiroProjection,
) (*KiroPayload, error) {
	if compatibilityErr := claudeKiroContentCompatibilityError(inventory); compatibilityErr != nil {
		return nil, compatibilityErr
	}
	effectiveReq, err := applyClaudeKiroProjection(req, projection)
	if err != nil {
		return nil, err
	}
	return ClaudeToKiro(effectiveReq, thinking), nil
}
