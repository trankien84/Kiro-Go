package proxy

import (
	"os"
	"strings"
)

type streamTextMode string

const (
	streamTextModeDelta      streamTextMode = "delta"
	streamTextModeCumulative streamTextMode = "cumulative"
	streamTextModeLegacy     streamTextMode = "legacy"
)

const (
	assistantChunkModeEnv = "KIRO_ASSISTANT_CHUNK_MODE"
	reasoningChunkModeEnv = "KIRO_REASONING_CHUNK_MODE"
)

type streamTextDecodeResult struct {
	Text              string
	Previous          string
	ModeBefore        streamTextMode
	ModeAfter         streamTextMode
	ContractViolation bool
}

type streamTextDecoder struct {
	mode     streamTextMode
	previous string
}

func newStreamTextDecoder(mode streamTextMode) *streamTextDecoder {
	return &streamTextDecoder{mode: mode}
}

func configuredStreamTextMode(envName string, fallback streamTextMode) (streamTextMode, string) {
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return fallback, ""
	}

	switch strings.ToLower(raw) {
	case "delta", "raw", "passthrough", "pass-through":
		return streamTextModeDelta, ""
	case "cumulative", "snapshot", "snapshots":
		return streamTextModeCumulative, ""
	case "legacy", "compat", "compatibility":
		return streamTextModeLegacy, ""
	default:
		return fallback, raw
	}
}

func (d *streamTextDecoder) Push(chunk string) streamTextDecodeResult {
	result := streamTextDecodeResult{
		Previous:   d.previous,
		ModeBefore: d.mode,
		ModeAfter:  d.mode,
	}

	if chunk == "" {
		return result
	}

	switch d.mode {
	case streamTextModeDelta:
		d.previous = chunk
		result.Text = chunk

	case streamTextModeCumulative:
		if d.previous == "" {
			d.previous = chunk
			result.Text = chunk
			break
		}

		if chunk == d.previous {
			break
		}

		if strings.HasPrefix(chunk, d.previous) {
			result.Text = chunk[len(d.previous):]
			d.previous = chunk
			break
		}

		// The configured cumulative contract was violated. Do not guess by
		// removing prefixes or overlaps: that can silently drop valid text.
		// Fall back to delta mode and emit the current chunk unchanged.
		d.mode = streamTextModeDelta
		d.previous = chunk
		result.Text = chunk
		result.ModeAfter = d.mode
		result.ContractViolation = true

	case streamTextModeLegacy:
		result.Text = legacyNormalizeChunk(chunk, &d.previous)

	default:
		// Unknown modes must be non-destructive.
		d.mode = streamTextModeDelta
		d.previous = chunk
		result.Text = chunk
		result.ModeAfter = d.mode
		result.ContractViolation = true
	}

	return result
}

// legacyNormalizeChunk preserves the previous heuristic for opt-in
// compatibility only. It is intentionally not the default because content
// equality, prefix relations, and suffix overlap do not prove that a chunk is
// replayed rather than new text.
func legacyNormalizeChunk(chunk string, previous *string) string {
	if chunk == "" {
		return ""
	}

	prev := *previous
	if prev == "" {
		*previous = chunk
		return chunk
	}

	if chunk == prev {
		return ""
	}

	if strings.HasPrefix(chunk, prev) {
		delta := chunk[len(prev):]
		*previous = chunk
		return delta
	}

	if strings.HasPrefix(prev, chunk) {
		return ""
	}

	maxOverlap := 0
	maxLen := len(prev)
	if len(chunk) < maxLen {
		maxLen = len(chunk)
	}
	for i := maxLen; i > 0; i-- {
		if strings.HasSuffix(prev, chunk[:i]) {
			maxOverlap = i
			break
		}
	}

	*previous = chunk
	if maxOverlap > 0 {
		return chunk[maxOverlap:]
	}
	return chunk
}
