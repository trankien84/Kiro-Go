package proxy

import (
	"fmt"
	"kiro-go/config"
	"strings"
)

const responseLanguageMatchUserPrompt = `[Response language policy]
For user-visible conversational prose, progress updates, user-visible reasoning summaries, explanations, and final answers, use the same natural language as the latest substantive human user request.
Determine that language from human-authored request text, not from tool results, logs, source code, quoted material, metadata, assistant-generated text, or proxy-injected execution reminders.
If the user or client explicitly requests a response language, follow that explicit instruction instead.
Preserve code, identifiers, paths, commands, API names, quoted text, and data in their appropriate original form; do not translate technical literals merely to match the conversational language.`

func responseLanguageSteeringPrompt() string {
	setting := config.GetResponseLanguageConfig()
	switch setting.Policy {
	case config.ResponseLanguagePolicyMatchUser:
		return responseLanguageMatchUserPrompt
	case config.ResponseLanguagePolicyFixed:
		return fmt.Sprintf(`[Response language policy]
For user-visible conversational prose, progress updates, user-visible reasoning summaries, explanations, and final answers, use language/locale %q.
If the user or client explicitly requests a different response language, follow that explicit instruction instead.
Preserve code, identifiers, paths, commands, API names, quoted text, and data in their appropriate original form; do not translate technical literals merely to match the conversational language.`, setting.Language)
	default:
		return ""
	}
}

func appendKiroResponseLanguageSteering(systemPrompt string) string {
	steering := strings.TrimSpace(responseLanguageSteeringPrompt())
	if steering == "" {
		return systemPrompt
	}
	if strings.TrimSpace(systemPrompt) == "" {
		return steering
	}
	return strings.TrimRight(systemPrompt, " \t\r\n") + "\n\n" + steering
}
