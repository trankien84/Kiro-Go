package proxy

import "encoding/json"

type ResponsesRequest struct {
	Model              string                     `json:"model"`
	Input              json.RawMessage            `json:"input"`
	Instructions       string                     `json:"instructions,omitempty"`
	Stream             bool                       `json:"stream,omitempty"`
	Tools              []OpenAITool               `json:"tools,omitempty"`
	RawTools           json.RawMessage            `json:"-"`
	ToolDeclarations   []responsesToolDeclaration `json:"-"`
	StoredToolCatalog  json.RawMessage            `json:"-"`
	ToolChoice         json.RawMessage            `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                      `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID string                     `json:"previous_response_id,omitempty"`
	Store              *bool                      `json:"store,omitempty"`
	Temperature        *float64                   `json:"temperature,omitempty"`
	TopP               *float64                   `json:"top_p,omitempty"`
	MaxOutputTokens    *int                       `json:"max_output_tokens,omitempty"`
	// OpenAI Responses API reasoning shape.
	Reasoning *ResponsesReasoningConfig `json:"reasoning,omitempty"`
	// Optional compatibility shapes.
	Thinking     *ClaudeThinkingConfig `json:"thinking,omitempty"`
	OutputConfig *ClaudeOutputConfig   `json:"output_config,omitempty"`
	Metadata     map[string]string     `json:"metadata,omitempty"`
}

func (r *ResponsesRequest) UnmarshalJSON(data []byte) error {
	type plain ResponsesRequest
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var raw struct {
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = ResponsesRequest(decoded)
	r.RawTools = append(json.RawMessage(nil), raw.Tools...)
	declarations, tools, storedCatalog, err := parseResponsesUnifiedTools(data)
	if err != nil {
		return err
	}
	r.ToolDeclarations = declarations
	r.Tools = tools
	r.StoredToolCatalog = storedCatalog
	return nil
}

type ResponsesObject struct {
	ID                 string                      `json:"id"`
	Object             string                      `json:"object"`
	CreatedAt          int64                       `json:"created_at"`
	Status             string                      `json:"status"`
	Model              string                      `json:"model"`
	Output             []ResponseOutputItem        `json:"output"`
	Usage              ResponsesUsage              `json:"usage"`
	ToolChoice         json.RawMessage             `json:"tool_choice,omitempty"`
	Tools              json.RawMessage             `json:"tools"`
	ParallelToolCalls  bool                        `json:"parallel_tool_calls"`
	PreviousResponseID string                      `json:"previous_response_id,omitempty"`
	Metadata           map[string]string           `json:"metadata,omitempty"`
	Error              *ResponsesError             `json:"error,omitempty"`
	IncompleteDetails  *ResponsesIncompleteDetails `json:"incomplete_details,omitempty"`
	Instructions       string                      `json:"instructions,omitempty"`
	StoredInput        json.RawMessage             `json:"-"`
	StoredToolCatalog  json.RawMessage             `json:"-"`
	StoredInstr        string                      `json:"-"`
	StoredAt           int64                       `json:"stored_at,omitempty"`
	StoredStateVersion int                         `json:"-"`
}

type ResponseOutputItem struct {
	ID        string                         `json:"id"`
	Type      string                         `json:"type"`
	Role      string                         `json:"role,omitempty"`
	Status    string                         `json:"status,omitempty"`
	Content   []ResponseContentPart          `json:"content,omitempty"`
	Summary   []ResponseReasoningSummaryPart `json:"summary,omitempty"`
	CallID    string                         `json:"call_id,omitempty"`
	Name      string                         `json:"name,omitempty"`
	Namespace string                         `json:"namespace,omitempty"`
	Arguments string                         `json:"arguments,omitempty"`
	Input     string                         `json:"input,omitempty"`
}

type ResponseReasoningSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// MarshalJSON keeps the existing message/function-call wire shape unchanged,
// while reasoning items use the current Responses reasoning-text content schema.
// ResponseContentPart carries message-only fields such as annotations/logprobs,
// which must not leak into a reasoning_text content part.
func (item ResponseOutputItem) MarshalJSON() ([]byte, error) {
	if item.Type != "reasoning" {
		type alias ResponseOutputItem
		return json.Marshal(alias(item))
	}
	type reasoningContentPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	content := make([]reasoningContentPart, 0, len(item.Content))
	for _, part := range item.Content {
		if part.Type == "reasoning_text" {
			content = append(content, reasoningContentPart{Type: part.Type, Text: part.Text})
		}
	}
	summary := item.Summary
	if summary == nil {
		summary = []ResponseReasoningSummaryPart{}
	}
	return json.Marshal(struct {
		ID      string                         `json:"id"`
		Type    string                         `json:"type"`
		Status  string                         `json:"status,omitempty"`
		Summary []ResponseReasoningSummaryPart `json:"summary"`
		Content []reasoningContentPart         `json:"content,omitempty"`
	}{
		ID: item.ID, Type: item.Type, Status: item.Status,
		Summary: summary, Content: content,
	})
}

type ResponseContentPart struct {
	Type        string        `json:"type"`
	Text        string        `json:"text,omitempty"`
	Annotations []interface{} `json:"annotations"`
	Logprobs    []interface{} `json:"logprobs,omitempty"`
}

type ResponsesUsage struct {
	InputTokens         int                          `json:"input_tokens"`
	InputTokensDetails  ResponsesInputTokensDetails  `json:"input_tokens_details"`
	OutputTokens        int                          `json:"output_tokens"`
	OutputTokensDetails ResponsesOutputTokensDetails `json:"output_tokens_details"`
	TotalTokens         int                          `json:"total_tokens"`
}

type ResponsesInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type ResponsesOutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type ResponsesError struct {
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type ResponsesIncompleteDetails struct {
	Reason string `json:"reason"`
}

type ResponsesReasoningConfig struct {
	Effort string `json:"effort,omitempty"`
}
