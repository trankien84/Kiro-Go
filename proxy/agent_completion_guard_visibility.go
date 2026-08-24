package proxy

// completionGuardMergeAuditContinuationVisibleResponse composes the visible
// buffered response after a semantic-audit continuation has been independently
// authorized. The continuation is an internal recovery turn: its narration and
// reasoning are not part of the user-visible answer. Only real client tool_use
// blocks cross the visibility boundary, matching the streaming path.
func completionGuardMergeAuditContinuationVisibleResponse(
	candidate ClaudeResponse,
	continuation ClaudeResponse,
	candidateText string,
) ClaudeResponse {
	prepared, _ := prepareCompletionGuardVisibleRecovery(
		continuation.Content,
		candidateText,
		true,
	)

	// Reuse the existing buffered metadata merge, but do not let it account the
	// hidden continuation prose as visible output. Visible continuation usage is
	// reconstructed below with the same tools-only accounting used by streaming.
	visibleContinuation := continuation
	visibleContinuation.Content = prepared
	visibleContinuation.Usage = ClaudeUsage{}

	result := completionGuardMergeBufferedVisibleResponses(candidate, visibleContinuation)
	addCompletionGuardVisibleOutputUsage(
		&result.Usage,
		continuation.Usage,
		prepared,
		completionGuardEmitToolsOnly,
	)
	return result
}
