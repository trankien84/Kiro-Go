package proxy

import (
	"crypto/sha256"
	"encoding/base64"
	"hash"
	"os"
	"strings"
)

const (
	kiroThinkingSignaturePrefix       = "kiro-go-v1:"
	kiroOriginThinkingSignaturePrefix = "kiro-go-v2:"
)

const syntheticThinkingSignatureEnv = "KIRO_SYNTHETIC_THINKING_SIGNATURE"

type syntheticThinkingSignatureMode string

const (
	syntheticThinkingSignatureAuto   syntheticThinkingSignatureMode = "auto"
	syntheticThinkingSignatureAlways syntheticThinkingSignatureMode = "always"
	syntheticThinkingSignatureNever  syntheticThinkingSignatureMode = "never"
)

// buildKiroThinkingSignature returns a stable, proxy-owned opaque signature for
// a thinking block. It is only a compatibility fallback for upstreams that do
// not emit a provider-issued signature.
func buildKiroThinkingSignature(model, thinking string) string {
	sum := sha256.Sum256([]byte("kiro-go-thinking-v1\x00" + model + "\x00" + thinking))
	return kiroThinkingSignaturePrefix + base64.RawURLEncoding.EncodeToString(sum[:])
}

func buildKiroOriginThinkingSignature(model, upstreamSignature string) string {
	sum := sha256.Sum256([]byte("kiro-go-thinking-v2\x00" + model + "\x00" + upstreamSignature))
	return kiroOriginThinkingSignaturePrefix + base64.RawURLEncoding.EncodeToString(sum[:])
}

func isKiroThinkingSignature(signature string) bool {
	for _, prefix := range []string{kiroThinkingSignaturePrefix, kiroOriginThinkingSignaturePrefix} {
		if !strings.HasPrefix(signature, prefix) {
			continue
		}
		encoded := strings.TrimPrefix(signature, prefix)
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		return err == nil && len(decoded) == sha256.Size
	}
	return false
}

// kiroThinkingSignatureAccumulator computes the same proxy-local signature as
// buildKiroThinkingSignature without retaining an entire streamed thinking
// block solely for hashing.
type kiroThinkingSignatureAccumulator struct {
	model string
	hash  hash.Hash
	bytes int
}

func newKiroThinkingSignatureAccumulator(model string) *kiroThinkingSignatureAccumulator {
	h := sha256.New()
	_, _ = h.Write([]byte("kiro-go-thinking-v1\x00" + model + "\x00"))
	return &kiroThinkingSignatureAccumulator{model: model, hash: h}
}

func (a *kiroThinkingSignatureAccumulator) WriteString(value string) {
	if a == nil || value == "" {
		return
	}
	_, _ = a.hash.Write([]byte(value))
	a.bytes += len(value)
}

func (a *kiroThinkingSignatureAccumulator) Resolve(
	upstreamSignature string,
) (signature string, synthetic bool) {
	if upstreamSignature != "" {
		model := ""
		if a != nil {
			model = a.model
		}
		return buildKiroOriginThinkingSignature(model, upstreamSignature), true
	}
	mode := configuredSyntheticThinkingSignatureMode()
	if mode == syntheticThinkingSignatureNever {
		return "", false
	}
	if mode == syntheticThinkingSignatureAuto && (a == nil || a.bytes == 0) {
		return "", false
	}
	if a == nil || a.hash == nil {
		return "", false
	}
	return kiroThinkingSignaturePrefix +
		base64.RawURLEncoding.EncodeToString(a.hash.Sum(nil)), true
}

func configuredSyntheticThinkingSignatureMode() syntheticThinkingSignatureMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(syntheticThinkingSignatureEnv))) {
	case "", "auto":
		return syntheticThinkingSignatureAuto
	case "1", "true", "yes", "on", "enabled", "always":
		return syntheticThinkingSignatureAlways
	case "0", "false", "no", "off", "disabled", "never":
		return syntheticThinkingSignatureNever
	default:
		return syntheticThinkingSignatureAuto
	}
}

// resolveKiroThinkingSignature is used only at the Kiro compatibility boundary.
// A provider-issued signature is deliberately localized before it is exposed to
// the client because Kiro assistant history does not forward that provider state
// on the next turn. This prevents a Kiro-origin continuation from being mistaken
// for foreign provider-signed state. The configured mode controls only the
// fallback used when the upstream emitted no signature at all.
func resolveKiroThinkingSignature(
	model,
	rawThinking,
	upstreamSignature string,
) (signature string, synthetic bool) {
	if upstreamSignature != "" {
		return buildKiroOriginThinkingSignature(model, upstreamSignature), true
	}
	mode := configuredSyntheticThinkingSignatureMode()
	if mode == syntheticThinkingSignatureNever {
		return "", false
	}
	// In auto mode, an empty omitted block is expected to carry a native
	// signature. Do not hide a protocol regression by inventing one.
	if mode == syntheticThinkingSignatureAuto && rawThinking == "" {
		return "", false
	}
	return buildKiroThinkingSignature(model, rawThinking), true
}

func attachKiroThinkingSignature(response *ClaudeResponse, model, rawThinking, upstreamSignature string) {
	if response == nil {
		return
	}

	signature, _ := resolveKiroThinkingSignature(model, rawThinking, upstreamSignature)
	if signature == "" {
		return
	}

	for i := range response.Content {
		if response.Content[i].Type == "thinking" && response.Content[i].Signature == "" {
			response.Content[i].Signature = signature
		}
	}
}
