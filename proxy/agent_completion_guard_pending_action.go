package proxy

import (
	"regexp"
	"strings"
	"unicode"
)

const completionGuardPendingActionReason = "announced_pending_action"

const completionGuardEnglishActionVerb = `(?:check|verify|read|open|fetch|search|inspect|run|execute|edit|write|create|implement|fix|patch|build|test|validate|continue|proceed|start|begin|acquire|generate|update|modify|review|analy[sz]e|refactor|migrate|deploy)`
const completionGuardVietnameseActionVerb = `(?:kiểm\s+tra|xác\s+minh|đọc|mở|lấy|tìm|chạy|thực\s+hiện|sửa|viết|tạo|triển\s+khai|build|test|tiếp\s+tục|bắt\s+đầu|cập\s+nhật|review|phân\s+tích|refactor|migrate|deploy)`

var (
	completionGuardEnglishFutureActionPattern = regexp.MustCompile(
		`(?i)(?:^|[.!?]\s+|\n+)\s*(?:` +
			`i(?:['’]ll|\s+will|\s+am\s+going\s+to|['’]m\s+going\s+to|['’]m\s+about\s+to)|` +
			`let\s+me|next\s+i(?:['’]ll|\s+will)?|now\s+i(?:['’]ll|\s+will)?` +
			`)\s+(?:now\s+|next\s+|then\s+|first\s+)?` +
			completionGuardEnglishActionVerb + `\b[^.!?]*[.!?]?\s*$`,
	)
	completionGuardVietnameseFutureActionPattern = regexp.MustCompile(
		`(?i)(?:^|[.!?]\s+|\n+)\s*(?:` +
			`tôi\s+sẽ|mình\s+sẽ|để\s+tôi|để\s+mình|` +
			`bây\s+giờ\s+(?:tôi|mình)\s+sẽ|giờ\s+(?:tôi|mình)\s+sẽ|` +
			`tiếp\s+theo\s+(?:tôi|mình)(?:\s+sẽ)?` +
			`)\s+` + completionGuardVietnameseActionVerb +
			`\b[^.!?]*[.!?]?\s*$`,
	)
	completionGuardTransitionActionPattern = regexp.MustCompile(
		`(?i)(?:^|[.!?]\s+|\n+)\s*(?:` +
			`starting\s+(?:with|by)|moving\s+on\s+to|continuing\s+with|proceeding\s+to` +
			`)\b[^.!?]*[.!?]?\s*$`,
	)
	completionGuardNextActionPattern = regexp.MustCompile(
		`(?i)(?:^|[.!?]\s+|\n+)\s*next\s*,?\s*` +
			completionGuardEnglishActionVerb + `\b[^.!?]*[.!?]?\s*$`,
	)
	completionGuardVietnameseTransitionActionPattern = regexp.MustCompile(
		`(?i)(?:^|[.!?]\s+|\n+)\s*(?:tiếp\s+theo|bây\s+giờ|giờ|sau\s+đó)\s*,?\s*` +
			completionGuardVietnameseActionVerb + `\b[^.!?]*[.!?]?\s*$`,
	)
	// This catches the common agent-stage fragment seen in Copilot/Kiro traces:
	// "Now the acquisition CLI that ...". A relative-clause noun phrase with no
	// main-clause completion is an announcement of the next artifact, not a
	// finished answer. Generic "Now the parser works" prose is intentionally not
	// matched.
	completionGuardNowStageFragmentPattern = regexp.MustCompile(
		`(?i)(?:^|[.!?]\s+|\n+)\s*now\s+the\b[^.!?]*(?:\bthat\b|\bwhich\b)[^.!?]*[.!?]?\s*$`,
	)
	completionGuardFinalStateTailPattern = regexp.MustCompile(
		`(?i)\b(?:is|are|was|were|has\s+been|have\s+been)\s+` +
			`(?:complete|completed|done|finished|ready|verified|validated|passing|working)\b|` +
			`\b(?:completed|finished|done|verified|validated|passed|passes|works)\b`,
	)
	// A final response may legitimately offer optional follow-up work. Those
	// conditional offers are not evidence that the current turn is unfinished.
	// Keep this narrow: clauses such as "if tests fail" still describe a pending
	// action chain and must not suppress the guard.
	completionGuardEnglishConditionalOfferTailPattern = regexp.MustCompile(
		`(?i)\b(?:` +
			`if\s+(?:you(?:['’]d|\s+would)?\s+(?:want|like|need)|needed|necessary|helpful|useful)|` +
			`when(?:ever)?\s+you(?:['’]d|\s+would)?\s+(?:want|like|need)|` +
			`on\s+request` +
			`)\b[^.!?]*[.!?]?\s*$`,
	)
	completionGuardVietnameseConditionalOfferTailPattern = regexp.MustCompile(
		`(?i)\b(?:` +
			`nếu\s+(?:bạn\s+(?:muốn|cần)|cần|muốn)|` +
			`khi\s+bạn\s+(?:muốn|cần)` +
			`)\b[^.!?]*[.!?]?\s*$`,
	)
)

func completionGuardCollapseSpace(value string) string {
	return strings.Join(strings.FieldsFunc(value, unicode.IsSpace), " ")
}

func completionGuardTrailingActionContext(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	runes := []rune(text)
	const maxRunes = 1200
	if len(runes) > maxRunes {
		runes = runes[len(runes)-maxRunes:]
	}
	return completionGuardCollapseSpace(string(runes))
}

func completionGuardHasCallableClientTool(plan agentCompletionGuardPlan) bool {
	return plan.Catalog != nil && len(plan.Catalog.ClientToolsByName) > 0
}

// completionGuardAnnouncedPendingAction is intentionally narrower than the
// semantic audit. It only catches a candidate that itself announces another
// agent action and then terminates without a client tool call. It does not try
// to infer the user's whole task, Copilot Todo state, or whether arbitrary
// prose is "complete".
func completionGuardAnnouncedPendingAction(
	text string,
	stopReason string,
	plan agentCompletionGuardPlan,
) bool {
	stopReason = strings.ToLower(strings.TrimSpace(stopReason))
	if stopReason != "end_turn" && stopReason != "stop_sequence" {
		return false
	}
	if !completionGuardHasCallableClientTool(plan) {
		return false
	}

	tail := completionGuardTrailingActionContext(text)
	if tail == "" {
		return false
	}

	isConditionalOffer := func(segment string) bool {
		return completionGuardEnglishConditionalOfferTailPattern.MatchString(segment) ||
			completionGuardVietnameseConditionalOfferTailPattern.MatchString(segment)
	}
	for _, pattern := range []*regexp.Regexp{
		completionGuardEnglishFutureActionPattern,
		completionGuardVietnameseFutureActionPattern,
		completionGuardTransitionActionPattern,
		completionGuardNextActionPattern,
		completionGuardVietnameseTransitionActionPattern,
	} {
		if match := pattern.FindString(tail); match != "" {
			if isConditionalOffer(match) {
				continue
			}
			return true
		}
	}

	match := completionGuardNowStageFragmentPattern.FindString(tail)
	if match == "" || isConditionalOffer(match) {
		return false
	}
	segment := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(match), ".!?"))
	return !completionGuardFinalStateTailPattern.MatchString(segment)
}
