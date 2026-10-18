package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// parseClaudeStopSequences preserves request order. The emulated matcher stops
// at the first completion boundary observed in generated text; if multiple
// sequences complete at that same boundary, request order is the deterministic
// local tie-breaker for the reported stop_sequence.
func parseClaudeStopSequences(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var sequences []string
	if err := json.Unmarshal(trimmed, &sequences); err != nil {
		return nil, fmt.Errorf("stop_sequences must be an array of strings: %w", err)
	}
	return append([]string(nil), sequences...), nil
}

func claudeRequestStopSequences(req *ClaudeRequest) ([]string, error) {
	if req == nil || req.Extra == nil {
		return nil, nil
	}
	raw, ok := req.Extra["stop_sequences"]
	if !ok {
		return nil, nil
	}
	return parseClaudeStopSequences(raw)
}

// claudeStopSequenceMatcher is a streaming delimiter matcher. It retains only
// the longest suffix that can still become the prefix of a stop sequence, so a
// match split across arbitrary Kiro chunks is never leaked to the client.
// It operates only on assistant text; callers flush pending text before a
// structural block such as tool_use because a text stop sequence cannot span
// across that protocol boundary.
type claudeStopSequenceMatcher struct {
	sequences []string
	pending   string
	matched   string
	stopped   bool
}

func newClaudeStopSequenceMatcher(sequences []string) *claudeStopSequenceMatcher {
	m := &claudeStopSequenceMatcher{sequences: append([]string(nil), sequences...)}
	for _, sequence := range m.sequences {
		if sequence == "" {
			m.matched = sequence
			m.stopped = true
			break
		}
	}
	return m
}

func (m *claudeStopSequenceMatcher) Enabled() bool {
	return m != nil && len(m.sequences) > 0
}

func (m *claudeStopSequenceMatcher) Stopped() bool {
	return m != nil && m.stopped
}

func (m *claudeStopSequenceMatcher) Matched() (string, bool) {
	if m == nil || !m.stopped {
		return "", false
	}
	return m.matched, true
}

func (m *claudeStopSequenceMatcher) Push(text string) string {
	if m == nil || len(m.sequences) == 0 {
		return text
	}
	if m.stopped || text == "" {
		return ""
	}

	buffer := m.pending + text
	matchAt := -1
	matchEnd := -1
	matchIndex := -1
	for i, sequence := range m.sequences {
		if sequence == "" {
			continue
		}
		idx := strings.Index(buffer, sequence)
		if idx < 0 {
			continue
		}
		end := idx + len(sequence)
		// Stop generation as soon as a sequence becomes complete. If multiple
		// sequences complete at the same byte boundary, use request order as a
		// deterministic tie-breaker for the emulated stop_sequence field.
		if matchEnd < 0 || end < matchEnd || (end == matchEnd && i < matchIndex) {
			matchAt = idx
			matchEnd = end
			matchIndex = i
		}
	}
	if matchEnd >= 0 {
		emit := buffer[:matchAt]
		m.pending = ""
		m.matched = m.sequences[matchIndex]
		m.stopped = true
		return emit
	}

	// No stop sequence has completed yet. Retain the longest suffix that can
	// still grow into one, so arbitrary Kiro chunk boundaries never leak a
	// partial delimiter to the client.
	keep := 0
	for _, sequence := range m.sequences {
		if sequence == "" {
			continue
		}
		max := len(sequence) - 1
		if max > len(buffer) {
			max = len(buffer)
		}
		for n := max; n > keep; n-- {
			if strings.HasSuffix(buffer, sequence[:n]) {
				keep = n
				break
			}
		}
	}
	if keep == 0 {
		m.pending = ""
		return buffer
	}
	emit := buffer[:len(buffer)-keep]
	m.pending = buffer[len(buffer)-keep:]
	return emit
}

func (m *claudeStopSequenceMatcher) Flush() string {
	if m == nil || m.stopped || m.pending == "" {
		return ""
	}
	pending := m.pending
	m.pending = ""
	return pending
}

// claudeKiroStopController adapts a Kiro callback in place. It is intentionally
// response-boundary only: Kiro does not receive stop_sequences upstream, so the
// projection is Emulated rather than Exact.
//
// Kiro can surface reasoning either through a dedicated reasoning event or via
// legacy <thinking>...</thinking> tags embedded in assistant text. Stop
// sequences apply only to visible assistant text, so the controller must not
// match hidden/tagged reasoning before the downstream Claude formatter has a
// chance to classify it.
type claudeKiroStopController struct {
	matcher        *claudeStopSequenceMatcher
	onText         func(string, bool)
	toolUseSeen    bool
	taggedThinking bool
	tagPending     string
}

const (
	claudeThinkingOpenTag  = "<thinking>"
	claudeThinkingCloseTag = "</thinking>"
)

func longestSuffixThatPrefixes(value, marker string) int {
	if value == "" || marker == "" {
		return 0
	}
	max := len(marker) - 1
	if max > len(value) {
		max = len(value)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(value, marker[:n]) {
			return n
		}
	}
	return 0
}

func (c *claudeKiroStopController) emitVisibleText(text string) {
	if c == nil || c.matcher == nil || c.matcher.Stopped() || text == "" {
		return
	}
	emit := c.matcher.Push(text)
	if emit != "" && c.onText != nil {
		c.onText(emit, false)
	}
}

func (c *claudeKiroStopController) flushVisibleMatcher() {
	if c == nil || c.matcher == nil || c.matcher.Stopped() {
		return
	}
	if pending := c.matcher.Flush(); pending != "" && c.onText != nil {
		c.onText(pending, false)
	}
}

// pushRawAssistantText mirrors the downstream <thinking> tag semantics while
// preserving the original tagged bytes for that downstream parser. Only bytes
// known to be visible assistant text are fed into the stop matcher. Potential
// tag prefixes are retained across arbitrary Kiro chunk boundaries.
func (c *claudeKiroStopController) pushRawAssistantText(text string) {
	if c == nil || c.matcher == nil || c.matcher.Stopped() || text == "" {
		return
	}
	c.tagPending += text

	for c.tagPending != "" && !c.matcher.Stopped() {
		if c.taggedThinking {
			if idx := strings.Index(c.tagPending, claudeThinkingCloseTag); idx >= 0 {
				end := idx + len(claudeThinkingCloseTag)
				if c.onText != nil {
					c.onText(c.tagPending[:end], false)
				}
				c.tagPending = c.tagPending[end:]
				c.taggedThinking = false
				continue
			}

			keep := longestSuffixThatPrefixes(c.tagPending, claudeThinkingCloseTag)
			safeLen := len(c.tagPending) - keep
			if safeLen > 0 && c.onText != nil {
				c.onText(c.tagPending[:safeLen], false)
			}
			c.tagPending = c.tagPending[safeLen:]
			return
		}

		if idx := strings.Index(c.tagPending, claudeThinkingOpenTag); idx >= 0 {
			c.emitVisibleText(c.tagPending[:idx])
			if c.matcher.Stopped() {
				c.tagPending = ""
				return
			}
			// A hidden reasoning block is a structural boundary. A visible stop
			// sequence cannot span across it, so release any retained false prefix.
			c.flushVisibleMatcher()
			if c.onText != nil {
				c.onText(claudeThinkingOpenTag, false)
			}
			c.tagPending = c.tagPending[idx+len(claudeThinkingOpenTag):]
			c.taggedThinking = true
			continue
		}

		keep := longestSuffixThatPrefixes(c.tagPending, claudeThinkingOpenTag)
		safeLen := len(c.tagPending) - keep
		if safeLen > 0 {
			c.emitVisibleText(c.tagPending[:safeLen])
			if c.matcher.Stopped() {
				c.tagPending = ""
				return
			}
		}
		c.tagPending = c.tagPending[safeLen:]
		return
	}
}

// flushTextBoundary releases text that was retained only to disambiguate a
// partial thinking tag. A structural boundary ends any current tagged-thinking
// region exactly like the downstream force-flush path does.
func (c *claudeKiroStopController) flushTextBoundary(resetTaggedThinking bool) {
	if c == nil || c.matcher == nil || c.matcher.Stopped() {
		return
	}
	if c.tagPending != "" {
		pending := c.tagPending
		c.tagPending = ""
		if c.taggedThinking {
			if c.onText != nil {
				c.onText(pending, false)
			}
		} else {
			c.emitVisibleText(pending)
		}
	}
	if !c.matcher.Stopped() && !c.taggedThinking {
		c.flushVisibleMatcher()
	}
	if resetTaggedThinking {
		c.taggedThinking = false
	}
}

func applyClaudeStopSequencesToKiroCallback(
	callback *KiroStreamCallback,
	sequences []string,
) *claudeKiroStopController {
	controller := &claudeKiroStopController{
		matcher: newClaudeStopSequenceMatcher(sequences),
	}
	if callback == nil || !controller.matcher.Enabled() {
		return controller
	}

	originalText := callback.OnText
	originalToolUse := callback.OnToolUse
	originalSignature := callback.OnReasoningSignature
	originalRedacted := callback.OnReasoningRedacted
	controller.onText = originalText

	callback.OnText = func(text string, isThinking bool) {
		if controller.matcher.Stopped() {
			return
		}
		if isThinking {
			// Dedicated reasoning events are structural boundaries for visible text.
			// If the legacy tag parser is active, preserve its pending bytes and state;
			// otherwise release any visible false-prefix before the hidden block.
			controller.flushTextBoundary(false)
			if controller.matcher.Stopped() {
				return
			}
			if originalText != nil {
				originalText(text, true)
			}
			return
		}
		if controller.toolUseSeen {
			if originalText != nil {
				originalText(text, false)
			}
			return
		}
		controller.pushRawAssistantText(text)
	}
	callback.OnToolUse = func(toolUse KiroToolUse) {
		if controller.matcher.Stopped() {
			return
		}
		// A stop sequence is a visible-text sequence and cannot span a protocol
		// block. Release retained text/tag prefixes before exposing tool_use.
		controller.flushTextBoundary(true)
		if controller.matcher.Stopped() {
			return
		}
		// Once a tool_use is exposed, Anthropic-compatible termination for this
		// response is tool_use. Do not later synthesize stop_sequence from text
		// that Kiro happens to emit after the structured action.
		controller.toolUseSeen = true
		if originalToolUse != nil {
			originalToolUse(toolUse)
		}
	}
	callback.OnReasoningSignature = func(signature string) {
		if controller.matcher.Stopped() {
			return
		}
		controller.flushTextBoundary(false)
		if controller.matcher.Stopped() {
			return
		}
		if originalSignature != nil {
			originalSignature(signature)
		}
	}
	callback.OnReasoningRedacted = func(data string) {
		if controller.matcher.Stopped() {
			return
		}
		controller.flushTextBoundary(false)
		if controller.matcher.Stopped() {
			return
		}
		if originalRedacted != nil {
			originalRedacted(data)
		}
	}
	return controller
}

// ResetForRetry discards all attempt-local delimiter/tag state while keeping
// the client's configured stop sequences and downstream callback wiring. It is
// used only by fully buffered handlers before a semantic-integrity retry; a
// visible streaming response never opts into this reset path.
func (c *claudeKiroStopController) ResetForRetry() {
	if c == nil || c.matcher == nil {
		return
	}
	sequences := append([]string(nil), c.matcher.sequences...)
	c.matcher = newClaudeStopSequenceMatcher(sequences)
	c.toolUseSeen = false
	c.taggedThinking = false
	c.tagPending = ""
}

func (c *claudeKiroStopController) Finish() {
	if c == nil || c.matcher == nil || c.matcher.Stopped() {
		return
	}
	c.flushTextBoundary(true)
}

func (c *claudeKiroStopController) Matched() (string, bool) {
	if c == nil || c.matcher == nil {
		return "", false
	}
	return c.matcher.Matched()
}
