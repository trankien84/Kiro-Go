package proxy

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

func TestClaudeStopSequenceMatcherSameChunk(t *testing.T) {
	m := newClaudeStopSequenceMatcher([]string{"<STOP>"})
	if got := m.Push("before<STOP>after"); got != "before" {
		t.Fatalf("emitted %q", got)
	}
	if matched, ok := m.Matched(); !ok || matched != "<STOP>" {
		t.Fatalf("matched=%q ok=%t", matched, ok)
	}
	if got := m.Push("leak"); got != "" {
		t.Fatalf("post-stop text leaked: %q", got)
	}
}

func TestClaudeStopSequenceMatcherCrossChunk(t *testing.T) {
	m := newClaudeStopSequenceMatcher([]string{"DONE"})
	var got string
	got += m.Push("alpha DO")
	got += m.Push("NE omega")
	if got != "alpha " {
		t.Fatalf("emitted %q", got)
	}
	if matched, ok := m.Matched(); !ok || matched != "DONE" {
		t.Fatalf("matched=%q ok=%t", matched, ok)
	}
}

func TestClaudeStopSequenceMatcherFlushesFalsePrefixAtBoundary(t *testing.T) {
	m := newClaudeStopSequenceMatcher([]string{"DONE"})
	if got := m.Push("hello DO"); got != "hello " {
		t.Fatalf("emitted %q", got)
	}
	if got := m.Flush(); got != "DO" {
		t.Fatalf("flush=%q", got)
	}
	if m.Stopped() {
		t.Fatal("false prefix marked as stop")
	}
}

func TestClaudeStopSequenceMatcherEarliestCompletionWins(t *testing.T) {
	m := newClaudeStopSequenceMatcher([]string{"BC", "AB", "A"})
	if got := m.Push("zABBC"); got != "z" {
		t.Fatalf("emitted %q", got)
	}
	if matched, _ := m.Matched(); matched != "A" {
		t.Fatalf("matched %q", matched)
	}
}

func TestClaudeStopSequenceMatcherEmptySequenceStopsImmediately(t *testing.T) {
	m := newClaudeStopSequenceMatcher([]string{""})
	if !m.Stopped() {
		t.Fatal("empty sequence did not stop immediately")
	}
	if got := m.Push("hidden"); got != "" {
		t.Fatalf("leaked %q", got)
	}
}

func TestParseClaudeStopSequencesRejectsNonStringArray(t *testing.T) {
	if _, err := parseClaudeStopSequences(json.RawMessage(`["ok",7]`)); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestClaudeKiroStopControllerSuppressesMatchedSuffixAndReportsSequence(t *testing.T) {
	var text string
	callback := &KiroStreamCallback{OnText: func(value string, isThinking bool) {
		if isThinking {
			t.Fatalf("unexpected thinking text %q", value)
		}
		text += value
	}}
	controller := applyClaudeStopSequencesToKiroCallback(callback, []string{"DONE"})
	callback.OnText("before DO", false)
	callback.OnText("NE after", false)
	controller.Finish()
	if text != "before " {
		t.Fatalf("text=%q", text)
	}
	if matched, ok := controller.Matched(); !ok || matched != "DONE" {
		t.Fatalf("matched=%q ok=%t", matched, ok)
	}
}

func TestClaudeKiroStopControllerToolUseEndsStopSequenceEmulation(t *testing.T) {
	var text string
	toolUses := 0
	callback := &KiroStreamCallback{
		OnText: func(value string, isThinking bool) {
			if !isThinking {
				text += value
			}
		},
		OnToolUse: func(KiroToolUse) { toolUses++ },
	}
	controller := applyClaudeStopSequencesToKiroCallback(callback, []string{"DONE"})
	callback.OnText("before DO", false)
	callback.OnToolUse(KiroToolUse{})
	callback.OnText("DONE remains ordinary after tool", false)
	controller.Finish()
	if text != "before DODONE remains ordinary after tool" {
		t.Fatalf("text=%q", text)
	}
	if toolUses != 1 {
		t.Fatalf("toolUses=%d", toolUses)
	}
	if _, ok := controller.Matched(); ok {
		t.Fatal("stop_sequence incorrectly overrode an exposed tool_use")
	}
}

func TestClaudeStopSequenceMatcherStopsWhenShorterPrefixCompletes(t *testing.T) {
	m := newClaudeStopSequenceMatcher([]string{"zC", "z"})
	if got := m.Push("z"); got != "" {
		t.Fatalf("emitted=%q", got)
	}
	if matched, ok := m.Matched(); !ok || matched != "z" {
		t.Fatalf("matched=%q ok=%t", matched, ok)
	}
}

func TestClaudeStopSequenceMatcherUsesEarliestCompletionBoundary(t *testing.T) {
	m := newClaudeStopSequenceMatcher([]string{"ABCDE", "BC"})
	if got := m.Push("ABC"); got != "A" {
		t.Fatalf("emit=%q want A", got)
	}
	if matched, ok := m.Matched(); !ok || matched != "BC" {
		t.Fatalf("matched=%q ok=%t", matched, ok)
	}
}

func TestClaudeStopSequenceMatcherSameCompletionUsesRequestOrder(t *testing.T) {
	m := newClaudeStopSequenceMatcher([]string{"BC", "ABC"})
	if got := m.Push("ABC"); got != "A" {
		t.Fatalf("emit=%q want A", got)
	}
	if matched, ok := m.Matched(); !ok || matched != "BC" {
		t.Fatalf("matched=%q ok=%t", matched, ok)
	}
}

func TestClaudeKiroStopControllerBoundaryMatchSuppressesToolUse(t *testing.T) {
	var text string
	toolUses := 0
	callback := &KiroStreamCallback{
		OnText: func(value string, isThinking bool) {
			if !isThinking {
				text += value
			}
		},
		OnToolUse: func(KiroToolUse) { toolUses++ },
	}
	controller := applyClaudeStopSequencesToKiroCallback(callback, []string{"zC", "z"})
	callback.OnText("prefix z", false)
	callback.OnToolUse(KiroToolUse{})
	controller.Finish()
	if text != "prefix " {
		t.Fatalf("text=%q", text)
	}
	if toolUses != 0 {
		t.Fatalf("toolUses=%d", toolUses)
	}
	if matched, ok := controller.Matched(); !ok || matched != "z" {
		t.Fatalf("matched=%q ok=%t", matched, ok)
	}
}

func TestClaudeKiroStopControllerIgnoresTaggedThinkingAcrossChunks(t *testing.T) {
	var got strings.Builder
	callback := &KiroStreamCallback{OnText: func(value string, _ bool) {
		got.WriteString(value)
	}}
	controller := applyClaudeStopSequencesToKiroCallback(callback, []string{"DONE"})

	for _, chunk := range []string{
		"<think",
		"ing>DO",
		"NE</thi",
		"nking>visible DO",
		"NE hidden",
	} {
		callback.OnText(chunk, false)
	}
	controller.Finish()

	if got.String() != "<thinking>DONE</thinking>visible " {
		t.Fatalf("forwarded=%q", got.String())
	}
	if matched, ok := controller.Matched(); !ok || matched != "DONE" {
		t.Fatalf("matched=%q ok=%t", matched, ok)
	}
}

func TestClaudeKiroStopControllerDoesNotSpanDedicatedThinkingBoundary(t *testing.T) {
	var visible strings.Builder
	var reasoning strings.Builder
	callback := &KiroStreamCallback{OnText: func(value string, isThinking bool) {
		if isThinking {
			reasoning.WriteString(value)
			return
		}
		visible.WriteString(value)
	}}
	controller := applyClaudeStopSequencesToKiroCallback(callback, []string{"DONE"})

	callback.OnText("DO", false)
	callback.OnText("hidden", true)
	callback.OnText("NE", false)
	controller.Finish()

	if _, ok := controller.Matched(); ok {
		t.Fatal("visible stop sequence matched across a reasoning block")
	}
	if visible.String() != "DONE" || reasoning.String() != "hidden" {
		t.Fatalf("visible=%q reasoning=%q", visible.String(), reasoning.String())
	}
}

func referenceClaudeStopSequence(input string, sequences []string) (string, string, bool) {
	for _, sequence := range sequences {
		if sequence == "" {
			return "", "", true
		}
	}

	matchAt := -1
	matchEnd := -1
	matchIndex := -1
	for i, sequence := range sequences {
		idx := strings.Index(input, sequence)
		if idx < 0 {
			continue
		}
		end := idx + len(sequence)
		if matchEnd < 0 || end < matchEnd || (end == matchEnd && i < matchIndex) {
			matchAt = idx
			matchEnd = end
			matchIndex = i
		}
	}
	if matchEnd < 0 {
		return input, "", false
	}
	return input[:matchAt], sequences[matchIndex], true
}

func TestClaudeStopSequenceMatcherRandomChunkPartitionsMatchReference(t *testing.T) {
	rng := rand.New(rand.NewSource(0x51A0D3))
	const alphabet = "abcXYZ<>/_-012"

	for caseIndex := 0; caseIndex < 10000; caseIndex++ {
		sourceLen := rng.Intn(64)
		var sourceBuilder strings.Builder
		for i := 0; i < sourceLen; i++ {
			sourceBuilder.WriteByte(alphabet[rng.Intn(len(alphabet))])
		}
		source := sourceBuilder.String()

		sequenceCount := 1 + rng.Intn(5)
		sequences := make([]string, sequenceCount)
		for i := range sequences {
			sequenceLen := rng.Intn(7)
			var sequenceBuilder strings.Builder
			for j := 0; j < sequenceLen; j++ {
				sequenceBuilder.WriteByte(alphabet[rng.Intn(len(alphabet))])
			}
			sequences[i] = sequenceBuilder.String()
		}

		matcher := newClaudeStopSequenceMatcher(sequences)
		var emitted strings.Builder
		for offset := 0; offset < len(source); {
			chunkLen := 1 + rng.Intn(8)
			end := offset + chunkLen
			if end > len(source) {
				end = len(source)
			}
			emitted.WriteString(matcher.Push(source[offset:end]))
			offset = end
		}
		emitted.WriteString(matcher.Flush())

		wantText, wantSequence, wantMatched := referenceClaudeStopSequence(source, sequences)
		gotSequence, gotMatched := matcher.Matched()
		if emitted.String() != wantText || gotSequence != wantSequence || gotMatched != wantMatched {
			t.Fatalf(
				"case=%d source=%q sequences=%q emitted=%q matched=%q ok=%t wantText=%q wantMatched=%q wantOK=%t",
				caseIndex,
				source,
				sequences,
				emitted.String(),
				gotSequence,
				gotMatched,
				wantText,
				wantSequence,
				wantMatched,
			)
		}
	}
}

func TestClaudeKiroStopControllerTaggedThinkingChunkPartitionInvariant(t *testing.T) {
	const source = "pre<thinking>DONE</thinking>visible DONE hidden"
	const want = "pre<thinking>DONE</thinking>visible "

	for first := 0; first <= len(source); first++ {
		for second := first; second <= len(source); second++ {
			var got strings.Builder
			callback := &KiroStreamCallback{OnText: func(value string, _ bool) {
				got.WriteString(value)
			}}
			controller := applyClaudeStopSequencesToKiroCallback(callback, []string{"DONE"})
			for _, chunk := range []string{source[:first], source[first:second], source[second:]} {
				if chunk != "" {
					callback.OnText(chunk, false)
				}
			}
			controller.Finish()

			if got.String() != want {
				t.Fatalf("partition=(%d,%d) forwarded=%q want=%q", first, second, got.String(), want)
			}
			if matched, ok := controller.Matched(); !ok || matched != "DONE" {
				t.Fatalf("partition=(%d,%d) matched=%q ok=%t", first, second, matched, ok)
			}
		}
	}
}

func TestClaudeKiroStopControllerResetForRetryDropsAttemptLocalDelimiterState(t *testing.T) {
	var visible strings.Builder
	callback := &KiroStreamCallback{OnText: func(value string, isThinking bool) {
		if !isThinking {
			visible.WriteString(value)
		}
	}}
	controller := applyClaudeStopSequencesToKiroCallback(callback, []string{"DONE"})

	// Attempt 1 retains a delimiter prefix but is discarded before becoming visible.
	callback.OnText("attempt-one DO", false)
	controller.ResetForRetry()
	visible.Reset() // mirrors the fully buffered handler's attempt-state reset.

	// Attempt 2 must start from a pristine matcher. The stale "DO" from attempt 1
	// must not combine with this attempt's "NE" and synthesize a false stop.
	callback.OnText("attempt-two NE", false)
	controller.Finish()

	if got := visible.String(); got != "attempt-two NE" {
		t.Fatalf("visible=%q", got)
	}
	if matched, ok := controller.Matched(); ok {
		t.Fatalf("stale retry state synthesized stop_sequence=%q", matched)
	}
}
