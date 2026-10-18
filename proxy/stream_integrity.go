package proxy

import (
	"errors"
	"strings"
)

// errUpstreamTruncatedResponse is a soft failure raised when a transport-clean
// stream never produced a terminal stop signal. It is an upstream integrity
// failure, not an account-health failure.
var errUpstreamTruncatedResponse = errors.New("upstream truncated response without stop reason")

// classifyStreamIntegrity validates semantic completeness after one transport
// attempt. The retry policy deliberately lives in kiro_transport_recovery.go so
// transport failures and clean-EOF truncation share one bounded retry budget.
func classifyStreamIntegrity(contentChars, toolCallCount int, stopReason string, sawReasoning bool) error {
	if strings.TrimSpace(stopReason) != "" {
		return nil
	}
	if toolCallCount > 0 {
		return nil
	}
	if contentChars > 0 || sawReasoning {
		return errUpstreamTruncatedResponse
	}
	// Empty output should normally be rejected by parseEventStreamTracked first.
	// Keep the classifier fail-closed so a future bypass cannot ship an empty turn
	// as successful completion.
	return errUpstreamTruncatedResponse
}

func isStreamIntegrityError(err error) bool {
	return errors.Is(err, errUpstreamTruncatedResponse)
}
