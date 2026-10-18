package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestParseEventStreamForwardsReasoningRedacted(t *testing.T) {
	const want = "opaque-redacted-data"
	stream := bytes.NewReader(awsEventStreamFrame(
		t,
		"reasoningContentEvent",
		map[string]interface{}{"redactedContent": want},
	))

	var got string
	err := parseEventStream(stream, &KiroStreamCallback{
		OnReasoningRedacted: func(data string) {
			got = data
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if got != want {
		t.Fatalf("redacted data = %q, want %q", got, want)
	}
}

func TestParseEventStreamBudgetsFragmentedToolInput(t *testing.T) {
	first := awsEventStreamFrame(
		t,
		"toolUseEvent",
		map[string]interface{}{
			"toolUseId": "t",
			"name":      "x",
			"input":     `{"a":`,
		},
	)
	second := awsEventStreamFrame(
		t,
		"toolUseEvent",
		map[string]interface{}{"input": `"123456"}`},
	)
	stream := append(append([]byte(nil), first...), second...)

	errBudget := errors.New("test retained budget exceeded")
	retained := 0
	var callbackErr error
	toolCalled := false
	err := parseEventStream(bytes.NewReader(stream), &KiroStreamCallback{
		OnToolUse: func(KiroToolUse) {
			toolCalled = true
		},
		ReserveRetained: func(size int) error {
			if retained+size > 8 {
				callbackErr = errBudget
				return callbackErr
			}
			retained += size
			return nil
		},
		ReleaseRetained: func(size int) {
			retained -= size
		},
		Err: func() error {
			return callbackErr
		},
	})
	if !errors.Is(err, errBudget) {
		t.Fatalf("parse error=%v, want retained-budget error", err)
	}
	if toolCalled {
		t.Fatal("partial tool was emitted after the retained budget failed")
	}
	if retained != 0 {
		t.Fatalf("parser retained %d bytes after abort", retained)
	}
}

func TestParseEventStreamReleasesDecoderRetention(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(
		t,
		"assistantResponseEvent",
		map[string]interface{}{"content": "hello"},
	))
	retained := 0
	peak := 0
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(string, bool) {},
		ReserveRetained: func(size int) error {
			retained += size
			if retained > peak {
				peak = retained
			}
			return nil
		},
		ReleaseRetained: func(size int) {
			retained -= size
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if peak < len("hello") {
		t.Fatalf("decoder retention was not budgeted: peak=%d", peak)
	}
	if retained != 0 {
		t.Fatalf("decoder retained %d bytes after EOF", retained)
	}
}

func TestFallbackClaudeReasoningCapabilityForFableAndMythos(t *testing.T) {
	for _, model := range []string{"claude-fable-5", "claude-mythos-5"} {
		t.Run(model, func(t *testing.T) {
			capability := fallbackClaudeReasoningCapability(model)
			if !capability.SupportsThinking {
				t.Fatal("expected thinking support")
			}
			if got := strings.Join(capability.ThinkingTypes, ","); got != "adaptive" {
				t.Fatalf("thinking types = %q, want adaptive", got)
			}
			if capability.DefaultThinkingType != "adaptive" {
				t.Fatalf("default thinking = %q", capability.DefaultThinkingType)
			}
			if capability.DefaultThinkingDisplay != "omitted" {
				t.Fatalf("default display = %q", capability.DefaultThinkingDisplay)
			}
			if got := strings.Join(capability.Efforts, ","); got != "low,medium,high,xhigh,max" {
				t.Fatalf("efforts = %q", got)
			}
		})
	}
}

func TestValidateClaudeThinkingForCapability(t *testing.T) {
	adaptiveOnly := ModelReasoningCapability{
		SupportsThinking: true,
		ThinkingTypes:    []string{"adaptive", "disabled"},
	}
	manual := ModelReasoningCapability{
		SupportsThinking:     true,
		ThinkingTypes:        []string{"enabled"},
		SupportsBudgetTokens: true,
	}

	if got := validateClaudeThinkingForCapability(
		&ClaudeThinkingConfig{Type: "enabled"},
		4096,
		false,
		false,
		adaptiveOnly,
	); got != "" {
		t.Fatalf("adaptive compatibility was rejected: %s", got)
	}

	if got := validateClaudeThinkingForCapability(
		&ClaudeThinkingConfig{Type: "enabled"},
		4096,
		false,
		false,
		manual,
	); !strings.Contains(got, "required") {
		t.Fatalf("missing manual budget error = %q", got)
	}

	if got := validateClaudeThinkingForCapability(
		&ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 512},
		4096,
		false,
		false,
		manual,
	); !strings.Contains(got, "at least 1024") {
		t.Fatalf("small manual budget error = %q", got)
	}

	if got := validateClaudeThinkingForCapability(
		&ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 4096},
		4096,
		true,
		true,
		manual,
	); got != "" {
		t.Fatalf("interleaved tool budget was rejected: %s", got)
	}

	if got := validateClaudeThinkingForCapability(
		&ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 4096},
		4096,
		true,
		false,
		manual,
	); !strings.Contains(got, "less than max_tokens") {
		t.Fatalf("non-interleaved tool budget error = %q", got)
	}
}

func TestClaudeManualInterleavedThinkingRequiresCapabilityAndBeta(t *testing.T) {
	manual := ModelReasoningCapability{
		SupportsThinking:            true,
		ThinkingTypes:               []string{"enabled"},
		SupportsBudgetTokens:        true,
		SupportsInterleavedThinking: true,
	}
	thinking := &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 4096}

	withoutBeta := httptest.NewRequest("POST", "/v1/messages", nil)
	if claudeManualInterleavedThinkingEnabled(withoutBeta, "claude-opus-4", thinking, manual) {
		t.Fatal("manual interleaved thinking enabled without beta header")
	}

	withBeta := httptest.NewRequest("POST", "/v1/messages", nil)
	withBeta.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	if !claudeManualInterleavedThinkingEnabled(withBeta, "claude-opus-4", thinking, manual) {
		t.Fatal("manual interleaved thinking not enabled with capability and beta")
	}

	for _, tc := range []struct {
		model string
		want  bool
	}{
		{model: "claude-opus-4", want: true},
		{model: "claude-opus-4.1", want: true},
		{model: "claude-opus-4.5", want: true},
		{model: "claude-sonnet-4.5", want: true},
		{model: "claude-sonnet-4.6", want: true},
		{model: "claude-opus-4.6", want: false},
		{model: "claude-haiku-4.5", want: false},
		{model: "claude-opus-4.7", want: false},
	} {
		capability := fallbackClaudeReasoningCapability(tc.model)
		got := claudeManualInterleavedThinkingEnabled(
			withBeta, tc.model, thinking, capability,
		)
		if got != tc.want {
			t.Fatalf("model=%s interleaved=%t, want %t", tc.model, got, tc.want)
		}
	}

	adaptiveOnly := ModelReasoningCapability{
		SupportsThinking:            true,
		ThinkingTypes:               []string{"adaptive"},
		SupportsInterleavedThinking: true,
	}
	if claudeManualInterleavedThinkingEnabled(withBeta, "claude-opus-4.6", thinking, adaptiveOnly) {
		t.Fatal("manual beta path must not override adaptive-only capability")
	}
}

func TestClaudeNonStreamBlockCollectorPreservesOrder(t *testing.T) {
	t.Setenv(syntheticThinkingSignatureEnv, "never")
	c := newClaudeNonStreamBlockCollector("claude-opus-5", false)
	c.AppendText("first reasoning", true)
	c.AppendSignature("sig-1")
	c.AppendRedacted("opaque")
	c.AppendText("second reasoning", true)
	c.AppendSignature("sig-2")
	c.AppendText("answer", false)
	c.AppendToolUse(KiroToolUse{
		ToolUseID: "tool-1",
		Name:      "read_file",
		Input:     map[string]interface{}{"path": "x"},
	})

	blocks := c.Blocks()
	wantTypes := []string{
		"thinking",
		"redacted_thinking",
		"thinking",
		"text",
		"tool_use",
	}
	if len(blocks) != len(wantTypes) {
		t.Fatalf("blocks=%#v", blocks)
	}
	for i, want := range wantTypes {
		if blocks[i].Type != want {
			t.Fatalf("block %d type=%q, want %q", i, blocks[i].Type, want)
		}
	}
	wantFirst := buildKiroOriginThinkingSignature("claude-opus-5", "sig-1")
	wantSecond := buildKiroOriginThinkingSignature("claude-opus-5", "sig-2")
	if blocks[0].Signature != wantFirst || blocks[2].Signature != wantSecond {
		t.Fatalf("signatures were not localized and associated per block: %#v", blocks)
	}
	if blocks[1].Data != "opaque" {
		t.Fatalf("redacted data=%q", blocks[1].Data)
	}
}

func TestClaudeNonStreamCollectorOmittedKeepsSignature(t *testing.T) {
	c := newClaudeNonStreamBlockCollector("claude-opus-5", true)
	c.AppendText("hidden summary", true)
	c.AppendSignature("provider-signature")
	blocks := c.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("blocks=%#v", blocks)
	}
	if blocks[0].Thinking != "" {
		t.Fatalf("omitted thinking leaked: %q", blocks[0].Thinking)
	}
	want := buildKiroOriginThinkingSignature("claude-opus-5", "provider-signature")
	if blocks[0].Signature != want {
		t.Fatalf("signature=%q want=%q", blocks[0].Signature, want)
	}
}

func TestSyntheticThinkingSignatureModes(t *testing.T) {
	old, had := os.LookupEnv(syntheticThinkingSignatureEnv)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(syntheticThinkingSignatureEnv, old)
		} else {
			_ = os.Unsetenv(syntheticThinkingSignatureEnv)
		}
	})

	_ = os.Setenv(syntheticThinkingSignatureEnv, "never")
	if sig, synthetic := resolveKiroThinkingSignature("m", "t", ""); sig != "" || synthetic {
		t.Fatalf("never mode returned %q, synthetic=%t", sig, synthetic)
	}

	_ = os.Setenv(syntheticThinkingSignatureEnv, "auto")
	if sig, synthetic := resolveKiroThinkingSignature("m", "t", ""); sig == "" || !synthetic {
		t.Fatalf("auto mode returned %q, synthetic=%t", sig, synthetic)
	}
	if sig, synthetic := resolveKiroThinkingSignature("m", "", ""); sig != "" || synthetic {
		t.Fatalf("auto mode hid an empty omitted-block regression: %q synthetic=%t", sig, synthetic)
	}

	_ = os.Setenv(syntheticThinkingSignatureEnv, "never")
	if sig, synthetic := resolveKiroThinkingSignature("m", "t", "native"); sig != buildKiroOriginThinkingSignature("m", "native") || !synthetic {
		t.Fatalf("Kiro-origin provider signature was not localized: %q, synthetic=%t", sig, synthetic)
	}
}

func TestSyntheticThinkingSignatureRoundTripsThroughKiro(t *testing.T) {
	signature := buildKiroThinkingSignature("claude-opus-5", "summary")
	req := ClaudeRequest{
		Model: "claude-opus-5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Inspect a file."},
			{Role: "assistant", Content: []ClaudeContentBlock{
				{Type: "thinking", Thinking: "summary", Signature: signature},
				{Type: "tool_use", ID: "tool-1", Name: "read", Input: map[string]interface{}{"path": "a.go"}},
			}},
			{Role: "user", Content: []ClaudeContentBlock{{Type: "tool_result", ToolUseID: "tool-1", Content: "contents"}}},
		},
		Tools: []ClaudeTool{{Name: "read", InputSchema: map[string]interface{}{"type": "object"}}},
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	semantic, reason := buildClaudeSemanticRequest(&req, nil, encoded)
	if reason != "" {
		t.Fatal(reason)
	}
	requirements, validationErr := analyzeClaudeTransportRequirements(&req, semantic, req.Model)
	if validationErr != nil || requirements.Allowed != claudeTransportMaskKiro {
		t.Fatalf("requirements=%#v validation=%v", requirements, validationErr)
	}
	if !isKiroThinkingSignature(signature) {
		t.Fatalf("proxy-local signature was not recognized: %q", signature)
	}
}

func TestReasoningChunkDefaultIsDelta(t *testing.T) {
	t.Setenv(reasoningChunkModeEnv, "")
	mode, invalid := configuredStreamTextMode(
		reasoningChunkModeEnv,
		streamTextModeDelta,
	)
	if invalid != "" || mode != streamTextModeDelta {
		t.Fatalf("mode=%q invalid=%q", mode, invalid)
	}
}
