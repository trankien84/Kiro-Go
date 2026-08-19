package proxy

import "net/http"

// Completion Guard keeps compatibility wrappers so its protocol validation and
// existing tests consume the endpoint-wide semantic normalizer rather than a
// second parser.
type completionGuardBetaSet = claudeBetaSet
type completionGuardSemanticRequest = claudeSemanticRequest

func completionGuardNewBetaSet() completionGuardBetaSet {
	return newClaudeBetaSet()
}

func completionGuardCompatibilityBodyBetas(
	req *ClaudeRequest,
) ([]string, bool, string) {
	return claudeCompatibilityBodyBetas(req)
}

func completionGuardBodyWithoutCompatibilityBetas(rawBody []byte) ([]byte, string) {
	return claudeBodyWithoutCompatibilityBetas(rawBody)
}

func completionGuardBuildSemanticRequest(
	req *ClaudeRequest,
	source http.Header,
	rawBody []byte,
) (completionGuardSemanticRequest, string) {
	return buildClaudeSemanticRequest(req, source, rawBody)
}

func completionGuardApplySemanticHeaders(
	destination http.Header,
	semantic http.Header,
) {
	applyClaudeSemanticHeaders(destination, semantic)
}
