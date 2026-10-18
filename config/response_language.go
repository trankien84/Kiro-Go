package config

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	ResponseLanguagePolicyPreserve  = "preserve"
	ResponseLanguagePolicyMatchUser = "match-user"
	ResponseLanguagePolicyFixed     = "fixed"
)

// ResponseLanguageConfig controls optional Kiro-native response-language
// steering. Preserve is the zero-value/default so existing deployments remain
// transparent unless an operator explicitly opts in.
type ResponseLanguageConfig struct {
	Policy   string `json:"policy"`
	Language string `json:"language,omitempty"`
}

var responseLanguageTagPattern = regexp.MustCompile(`^[A-Za-z]{2,8}(?:-[A-Za-z0-9]{1,8})*$`)

func normalizeResponseLanguagePolicy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", ResponseLanguagePolicyPreserve, "off", "none":
		return ResponseLanguagePolicyPreserve
	case ResponseLanguagePolicyMatchUser, "match_user", "matchuser", "user", "auto":
		return ResponseLanguagePolicyMatchUser
	case ResponseLanguagePolicyFixed:
		return ResponseLanguagePolicyFixed
	default:
		return ""
	}
}

func normalizeResponseLanguageTag(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "_", "-"))
	if value == "" || len(value) > 64 || !responseLanguageTagPattern.MatchString(value) {
		return ""
	}
	parts := strings.Split(value, "-")
	for i, part := range parts {
		switch {
		case i == 0:
			parts[i] = strings.ToLower(part)
		case len(part) == 2:
			parts[i] = strings.ToUpper(part)
		default:
			parts[i] = strings.ToLower(part)
		}
	}
	return strings.Join(parts, "-")
}

// NormalizeResponseLanguageConfig validates and canonicalizes a requested
// language policy. Fixed mode requires a conservative BCP47-like language tag
// (for example vi, en-US, pt-BR, zh-CN). Other modes intentionally discard the
// fixed-language field so stale UI state cannot influence generation.
func NormalizeResponseLanguageConfig(policy, language string) (ResponseLanguageConfig, error) {
	normalizedPolicy := normalizeResponseLanguagePolicy(policy)
	if normalizedPolicy == "" {
		return ResponseLanguageConfig{}, fmt.Errorf("invalid response language policy %q", policy)
	}

	result := ResponseLanguageConfig{Policy: normalizedPolicy}
	if normalizedPolicy != ResponseLanguagePolicyFixed {
		return result, nil
	}

	normalizedLanguage := normalizeResponseLanguageTag(language)
	if normalizedLanguage == "" {
		return ResponseLanguageConfig{}, fmt.Errorf("fixed response language requires a valid language tag such as vi, en-US, or zh-CN")
	}
	result.Language = normalizedLanguage
	return result, nil
}

// GetResponseLanguageConfig returns the persisted setting in a fail-closed
// normalized form. Invalid hand-edited config never injects an unknown prompt;
// it falls back to transparent preserve behavior.
func GetResponseLanguageConfig() ResponseLanguageConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return ResponseLanguageConfig{Policy: ResponseLanguagePolicyPreserve}
	}
	result, err := NormalizeResponseLanguageConfig(cfg.ResponseLanguagePolicy, cfg.ResponseLanguage)
	if err != nil {
		return ResponseLanguageConfig{Policy: ResponseLanguagePolicyPreserve}
	}
	return result
}

// UpdateResponseLanguageConfig persists the language-steering setting through
// the same publish-before-memory transaction boundary as other global config.
func UpdateResponseLanguageConfig(policy, language string) error {
	normalized, err := NormalizeResponseLanguageConfig(policy, language)
	if err != nil {
		return err
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err = publishConfigFieldsLocked(func(next *Config) error {
		next.ResponseLanguagePolicy = normalized.Policy
		next.ResponseLanguage = normalized.Language
		return nil
	})
	return err
}
