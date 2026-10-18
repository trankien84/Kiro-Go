package proxy

import (
	"sort"
	"strings"
)

// completionGuardProjectionReplaySafety describes whether Completion Guard may
// safely issue hidden Kiro attempts for a request whose Kiro projection is not
// exact. This is deliberately independent from requestProjectionFidelity:
// fidelity answers whether Kiro can represent the request, while replay safety
// answers whether the same compatibility projection remains stable when the
// guard appends private recovery turns.
type completionGuardProjectionReplaySafety uint8

const (
	completionGuardProjectionReplayExact completionGuardProjectionReplaySafety = iota
	completionGuardProjectionReplaySafe
	completionGuardProjectionReplayUnsafe
)

func (safety completionGuardProjectionReplaySafety) String() string {
	switch safety {
	case completionGuardProjectionReplayExact:
		return "exact"
	case completionGuardProjectionReplaySafe:
		return "replay_safe"
	case completionGuardProjectionReplayUnsafe:
		return "replay_unsafe"
	default:
		return "unknown"
	}
}

// completionGuardReplaySafeClaudeProjectionFeature is intentionally a
// whitelist. New emulation features are replay-unsafe until their behavior is
// reviewed against hidden Completion Guard turns.
func completionGuardReplaySafeClaudeProjectionFeature(feature string) bool {
	switch strings.TrimSpace(feature) {
	case "thinking_effort",
		"cache_control",
		"metadata",
		"anthropic_user_profile",
		"service_tier",
		"content_cache_control",
		"tool_cache_control",
		"nested_tool_result_image":
		return true
	default:
		return false
	}
}

func completionGuardClaudeProjectionReplaySafety(
	projection claudeKiroProjection,
) (completionGuardProjectionReplaySafety, string) {
	if projection.Fidelity == claudeProjectionUnsupported {
		return completionGuardProjectionReplayUnsafe, "projection_unsupported"
	}
	if projection.Fidelity == claudeProjectionExact {
		return completionGuardProjectionReplayExact, ""
	}

	unsafe := make([]string, 0)
	for _, issue := range projection.Issues {
		if issue.Fidelity != claudeProjectionEmulated {
			continue
		}
		if !completionGuardReplaySafeClaudeProjectionFeature(issue.Feature) {
			feature := strings.TrimSpace(issue.Feature)
			if feature == "" {
				feature = "<unnamed>"
			}
			unsafe = append(unsafe, feature)
		}
	}
	if len(unsafe) > 0 {
		sort.Strings(unsafe)
		return completionGuardProjectionReplayUnsafe,
			"emulation_requires_original_projection:" + strings.Join(unsafe, ",")
	}

	return completionGuardProjectionReplaySafe, ""
}
