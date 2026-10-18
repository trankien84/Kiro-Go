package proxy

import (
	"encoding/json"
	"strings"
)

type claudeCollectedBlock struct {
	block       ClaudeContentBlock
	text        strings.Builder
	rawThinking strings.Builder
	sealed      bool
}

// claudeNonStreamBlockCollector preserves Kiro event order for Anthropic
// non-stream responses. A flat content/thinking/signature accumulator cannot
// represent multiple thinking blocks or redacted blocks around tool calls.
type claudeNonStreamBlockCollector struct {
	model           string
	omitThinking    bool
	nativeReasoning bool
	blocks          []claudeCollectedBlock
	budget          *completionGuardRetainedBudget
	retained        int
	err             error
}

func newClaudeNonStreamBlockCollector(
	model string,
	omitThinking bool,
	budgets ...*completionGuardRetainedBudget,
) *claudeNonStreamBlockCollector {
	collector := &claudeNonStreamBlockCollector{
		model:        model,
		omitThinking: omitThinking,
	}
	if len(budgets) > 0 {
		collector.budget = budgets[0]
	}
	return collector
}

func (c *claudeNonStreamBlockCollector) reserve(size int) bool {
	if c == nil || c.err != nil {
		return false
	}
	if c.budget != nil && !c.budget.reserve(size) {
		c.err = c.budget.Err()
		return false
	}
	c.retained += size
	return true
}

func (c *claudeNonStreamBlockCollector) Err() error {
	if c == nil {
		return nil
	}
	if c.err != nil {
		return c.err
	}
	if c.budget != nil {
		return c.budget.Err()
	}
	return nil
}

func (c *claudeNonStreamBlockCollector) Release() {
	if c == nil {
		return
	}
	if c.budget != nil {
		c.budget.release(c.retained)
	}
	c.retained = 0
	c.blocks = nil
}

func (c *claudeNonStreamBlockCollector) AppendText(
	text string,
	isThinking bool,
) {
	if text == "" {
		return
	}
	if !c.reserve(len(text)) {
		return
	}
	if isThinking {
		c.nativeReasoning = true
		idx := c.appendableBlock("thinking")
		if idx < 0 {
			block := ClaudeContentBlock{Type: "thinking"}
			block.presentKnownFields = map[string]struct{}{"thinking": {}}
			c.blocks = append(c.blocks, claudeCollectedBlock{
				block: block,
			})
			idx = len(c.blocks) - 1
		}
		c.blocks[idx].rawThinking.WriteString(text)
		return
	}

	idx := c.appendableBlock("text")
	if idx < 0 {
		c.blocks = append(c.blocks, claudeCollectedBlock{
			block: ClaudeContentBlock{Type: "text"},
		})
		idx = len(c.blocks) - 1
	}
	c.blocks[idx].text.WriteString(text)
}

func (c *claudeNonStreamBlockCollector) AppendSignature(signature string) {
	if signature == "" {
		return
	}
	signature, _ = resolveKiroThinkingSignature(c.model, "", signature)
	if signature == "" {
		return
	}
	if !c.reserve(len(signature)) {
		return
	}
	c.nativeReasoning = true
	for i := len(c.blocks) - 1; i >= 0; i-- {
		if c.blocks[i].block.Type == "thinking" &&
			c.blocks[i].block.Signature == "" &&
			!c.blocks[i].sealed {

			c.blocks[i].block.Signature = signature
			c.blocks[i].sealed = true
			return
		}
	}
	block := ClaudeContentBlock{
		Type:      "thinking",
		Signature: signature,
	}
	block.presentKnownFields = map[string]struct{}{"thinking": {}}
	c.blocks = append(c.blocks, claudeCollectedBlock{
		block:  block,
		sealed: true,
	})
}

func (c *claudeNonStreamBlockCollector) AppendRedacted(data string) {
	if data == "" {
		return
	}
	if !c.reserve(len(data)) {
		return
	}
	c.nativeReasoning = true
	c.sealLast()
	c.blocks = append(c.blocks, claudeCollectedBlock{
		block: ClaudeContentBlock{
			Type: "redacted_thinking",
			Data: data,
		},
		sealed: true,
	})
}

func (c *claudeNonStreamBlockCollector) AppendToolUse(tu KiroToolUse) bool {
	input, _ := json.Marshal(tu.Input)
	if !c.reserve(len(tu.ToolUseID) + len(tu.Name) + len(input)) {
		return false
	}
	c.sealLast()
	c.blocks = append(c.blocks, claudeCollectedBlock{
		block: ClaudeContentBlock{
			Type:  "tool_use",
			ID:    tu.ToolUseID,
			Name:  tu.Name,
			Input: tu.Input,
		},
		sealed: true,
	})
	return true
}

func (c *claudeNonStreamBlockCollector) HasNativeReasoning() bool {
	return c.nativeReasoning
}

func (c *claudeNonStreamBlockCollector) HasToolUse() bool {
	for i := range c.blocks {
		if c.blocks[i].block.Type == "tool_use" {
			return true
		}
	}
	return false
}

func (c *claudeNonStreamBlockCollector) Blocks() []ClaudeContentBlock {
	out := make([]ClaudeContentBlock, 0, len(c.blocks))
	for i := range c.blocks {
		block := c.blocks[i].block
		switch block.Type {
		case "text":
			block.Text = c.blocks[i].text.String()
		case "thinking":
			if !c.omitThinking {
				block.Thinking = c.blocks[i].rawThinking.String()
			}
			if block.presentKnownFields == nil {
				block.presentKnownFields = make(map[string]struct{})
			}
			block.presentKnownFields["thinking"] = struct{}{}
		}
		if block.Type == "thinking" && block.Signature == "" {
			block.Signature, _ = resolveKiroThinkingSignature(
				c.model,
				c.blocks[i].rawThinking.String(),
				"",
			)
		}
		out = append(out, block)
	}
	return out
}

func (c *claudeNonStreamBlockCollector) joined(blockType string) string {
	total := 0
	for i := range c.blocks {
		switch blockType {
		case "text":
			if c.blocks[i].block.Type == "text" {
				total += c.blocks[i].text.Len()
			}
		case "thinking":
			if c.blocks[i].block.Type == "thinking" {
				total += c.blocks[i].rawThinking.Len()
			}
		}
	}
	if total == 0 || !c.reserve(total) {
		return ""
	}
	var result strings.Builder
	result.Grow(total)
	for i := range c.blocks {
		switch blockType {
		case "text":
			if c.blocks[i].block.Type == "text" {
				result.WriteString(c.blocks[i].text.String())
			}
		case "thinking":
			if c.blocks[i].block.Type == "thinking" {
				result.WriteString(c.blocks[i].rawThinking.String())
			}
		}
	}
	return result.String()
}

func (c *claudeNonStreamBlockCollector) Text() string {
	return c.joined("text")
}

func (c *claudeNonStreamBlockCollector) RawThinking() string {
	return c.joined("thinking")
}

func (c *claudeNonStreamBlockCollector) appendableBlock(
	blockType string,
) int {
	if len(c.blocks) == 0 {
		return -1
	}
	idx := len(c.blocks) - 1
	if c.blocks[idx].sealed || c.blocks[idx].block.Type != blockType {
		return -1
	}
	return idx
}

func (c *claudeNonStreamBlockCollector) sealLast() {
	if len(c.blocks) > 0 {
		c.blocks[len(c.blocks)-1].sealed = true
	}
}
