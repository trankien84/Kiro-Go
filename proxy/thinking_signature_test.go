package proxy

import (
	"strings"
	"testing"
)

func TestBuildKiroThinkingSignatureIsStableAndScoped(t *testing.T) {
	first := buildKiroThinkingSignature("claude-opus-5", "reasoning")
	second := buildKiroThinkingSignature("claude-opus-5", "reasoning")
	if first != second {
		t.Fatalf("signature must be stable: %q != %q", first, second)
	}
	if !strings.HasPrefix(first, kiroThinkingSignaturePrefix) {
		t.Fatalf("signature %q is missing prefix %q", first, kiroThinkingSignaturePrefix)
	}
	if first == buildKiroThinkingSignature("claude-opus-5", "different") {
		t.Fatal("different thinking content must produce a different signature")
	}
	if first == buildKiroThinkingSignature("claude-sonnet-5", "reasoning") {
		t.Fatal("different models must produce a different signature")
	}
}

func TestKiroThinkingSignatureAccumulatorMatchesBufferedSignature(t *testing.T) {
	t.Setenv(syntheticThinkingSignatureEnv, "always")
	accumulator := newKiroThinkingSignatureAccumulator("claude-opus-5")
	accumulator.WriteString("first ")
	accumulator.WriteString("second")
	got, synthetic := accumulator.Resolve("")
	want := buildKiroThinkingSignature("claude-opus-5", "first second")
	if !synthetic || got != want {
		t.Fatalf("signature=%q synthetic=%t, want %q", got, synthetic, want)
	}
}

func TestAttachKiroThinkingSignatureLocalizesUpstreamSignature(t *testing.T) {
	response := &ClaudeResponse{
		Content: []ClaudeContentBlock{
			{Type: "thinking", Thinking: "summary"},
			{Type: "text", Text: "answer"},
		},
	}

	attachKiroThinkingSignature(response, "claude-opus-5", "full internal summary", "provider-signature")

	got := response.Content[0].Signature
	if got == "provider-signature" {
		t.Fatal("Kiro response leaked a provider-issued signature unchanged")
	}
	if !strings.HasPrefix(got, kiroOriginThinkingSignaturePrefix) || !isKiroThinkingSignature(got) {
		t.Fatalf("got signature %q, want Kiro-origin signature", got)
	}
	want := buildKiroOriginThinkingSignature("claude-opus-5", "provider-signature")
	if got != want {
		t.Fatalf("got signature %q, want %q", got, want)
	}
	if response.Content[1].Signature != "" {
		t.Fatal("text block must not receive a thinking signature")
	}
}

func TestAttachKiroThinkingSignatureFallsBackWhenUpstreamMissing(t *testing.T) {
	response := &ClaudeResponse{
		Content: []ClaudeContentBlock{{Type: "thinking", Thinking: "summary"}},
	}

	attachKiroThinkingSignature(response, "claude-opus-5", "full internal summary", "")

	if !strings.HasPrefix(response.Content[0].Signature, kiroThinkingSignaturePrefix) {
		t.Fatalf("expected fallback signature, got %q", response.Content[0].Signature)
	}
}

func TestKiroThinkingSignatureAccumulatorLocalizesProviderSignature(t *testing.T) {
	accumulator := newKiroThinkingSignatureAccumulator("claude-opus-5")
	accumulator.WriteString("hidden reasoning")
	got, synthetic := accumulator.Resolve("provider-signature")
	if !synthetic {
		t.Fatal("localized Kiro-origin signature must be marked proxy-owned")
	}
	if got == "provider-signature" || !isKiroThinkingSignature(got) {
		t.Fatalf("provider signature was not localized: %q", got)
	}
	if want := buildKiroOriginThinkingSignature("claude-opus-5", "provider-signature"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestKiroProviderSignatureLocalizationIgnoresFallbackDisable(t *testing.T) {
	t.Setenv(syntheticThinkingSignatureEnv, "never")
	got, synthetic := resolveKiroThinkingSignature(
		"claude-opus-5",
		"",
		"provider-signature",
	)
	if !synthetic || !isKiroThinkingSignature(got) {
		t.Fatalf("provider signature localization must remain enabled: signature=%q synthetic=%t", got, synthetic)
	}
}

func TestKiroOriginThinkingSignatureScopesProviderState(t *testing.T) {
	first := buildKiroOriginThinkingSignature("claude-opus-5", "provider-a")
	if first == buildKiroOriginThinkingSignature("claude-opus-5", "provider-b") {
		t.Fatal("different provider signatures must not collapse to one Kiro-origin signature")
	}
	if first == buildKiroOriginThinkingSignature("claude-sonnet-5", "provider-a") {
		t.Fatal("provider signature localization must be model scoped")
	}
	if !isKiroThinkingSignature(first) {
		t.Fatalf("localized signature %q was not recognized as Kiro state", first)
	}
}
