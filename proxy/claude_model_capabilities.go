package proxy

import (
	"regexp"
	"strings"
)

type claudeFeatureSupport uint8

const (
	claudeFeatureUnknown claudeFeatureSupport = iota
	claudeFeatureUnsupported
	claudeFeatureSupported
)

type claudeModelCapabilities struct {
	AssistantPrefill        claudeFeatureSupport
	MidConversationSystem   claudeFeatureSupport
	MidConversationTools    claudeFeatureSupport
	StreamedRefusal         claudeFeatureSupport
	ProgrammaticToolCalling claudeFeatureSupport
	ToolSearch              claudeFeatureSupport
}

var (
	claudeCanonicalModelPattern = regexp.MustCompile(
		`^claude-(fable|mythos|sonnet|opus|haiku)-(\d+)(?:[.-](\d{1,2}))?(?:-\d{8})?(?:-v\d+)?$`,
	)
	claudeLegacyModelPattern = regexp.MustCompile(
		`^claude-(\d+)(?:[.-](\d{1,2}))?-(sonnet|opus|haiku)(?:-\d{8})?(?:-v\d+)?$`,
	)
	claudeLegacyVersionOnlyPattern = regexp.MustCompile(
		`^claude-(\d+)(?:[.-](\d{1,2}))?(?:-\d{8})?(?:-v\d+)?$`,
	)
)

// Anthropic publishes model compatibility for these features as explicit
// matrices, not as an open-ended family/version promise. Keep exact supported
// sets so a future model remains unknown until the provider documents it.
var claudeProgrammaticToolCallingModels = map[string]struct{}{
	"fable-5":    {},
	"mythos-5":   {},
	"opus-5":     {},
	"opus-4.8":   {},
	"opus-4.7":   {},
	"opus-4.6":   {},
	"sonnet-5":   {},
	"sonnet-4.6": {},
	"opus-4.5":   {},
	"sonnet-4.5": {},
}

var claudeToolSearchModels = map[string]struct{}{
	"fable-5":    {},
	"mythos-5":   {},
	"opus-5":     {},
	"opus-4.8":   {},
	"opus-4.7":   {},
	"opus-4.6":   {},
	"sonnet-4.6": {},
	"opus-4.5":   {},
	"sonnet-4.5": {},
	"haiku-4.5":  {},
}

func canonicalClaudeModelFamilyVersion(model string) (family, version string, ok bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	// Kiro-Go exposes a configurable thinking suffix, and Completion Guard
	// historically normalized the default suffix before classifying a model.
	// Keep the capability registry behavior-compatible so every consumer sees
	// the same canonical family instead of treating the suffixed ID as private.
	model = strings.TrimSuffix(model, "-thinking")
	if slash := strings.LastIndex(model, "/"); slash >= 0 {
		model = model[slash+1:]
	}
	// Bedrock and other providers commonly add a version suffix after a colon.
	if colon := strings.Index(model, ":"); colon >= 0 {
		model = model[:colon]
	}
	// Accept the bare Anthropic ID and known Bedrock provider-qualified IDs,
	// but do not classify arbitrary private aliases merely because they contain
	// the substring "claude-". Slash-qualified IDs were reduced above; Bedrock
	// IDs use prefixes such as anthropic. or us.anthropic.
	if index := strings.Index(model, "claude-"); index > 0 {
		prefix := model[:index]
		if !strings.HasSuffix(prefix, "anthropic.") {
			return "", "", false
		}
		model = model[index:]
	}
	if match := claudeCanonicalModelPattern.FindStringSubmatch(model); len(match) == 4 {
		family = match[1]
		version = match[2]
		if match[3] != "" && match[3] != "0" {
			version += "." + match[3]
		}
		return family, version, true
	}
	if match := claudeLegacyModelPattern.FindStringSubmatch(model); len(match) == 4 {
		version = match[1]
		if match[2] != "" && match[2] != "0" {
			version += "." + match[2]
		}
		return match[3], version, true
	}
	if match := claudeLegacyVersionOnlyPattern.FindStringSubmatch(model); len(match) == 3 {
		version = match[1]
		if match[2] != "" && match[2] != "0" {
			version += "." + match[2]
		}
		return "legacy", version, true
	}
	return "", "", false
}

func claudeVersionAtLeast(version string, major, minor int) bool {
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) == 0 {
		return false
	}
	parsedMajor := 0
	parsedMinor := 0
	for _, ch := range parts[0] {
		if ch < '0' || ch > '9' {
			return false
		}
		parsedMajor = parsedMajor*10 + int(ch-'0')
	}
	if len(parts) > 1 {
		for _, ch := range parts[1] {
			if ch < '0' || ch > '9' {
				return false
			}
			parsedMinor = parsedMinor*10 + int(ch-'0')
		}
	}
	return parsedMajor > major || (parsedMajor == major && parsedMinor >= minor)
}

func resolveClaudeModelCapabilities(model string) claudeModelCapabilities {
	family, version, ok := canonicalClaudeModelFamilyVersion(model)
	if !ok {
		return claudeModelCapabilities{}
	}

	caps := claudeModelCapabilities{}
	if family != "legacy" {
		if claudeVersionAtLeast(version, 4, 0) {
			caps.StreamedRefusal = claudeFeatureSupported
		} else {
			caps.StreamedRefusal = claudeFeatureUnsupported
		}

		key := family + "-" + version
		caps.ProgrammaticToolCalling = claudeExactFeatureSupport(
			key,
			family,
			version,
			claudeProgrammaticToolCallingModels,
			key == "haiku-4.5",
		)
		caps.ToolSearch = claudeExactFeatureSupport(
			key,
			family,
			version,
			claudeToolSearchModels,
			key == "sonnet-5",
		)
	}

	// Anthropic rejects assistant-prefill on Claude 4.6 and newer. Older
	// canonical Claude models retain the legacy prefill contract; private aliases
	// remain unknown and are routed only through transparent transports.
	if claudeVersionAtLeast(version, 4, 6) {
		caps.AssistantPrefill = claudeFeatureUnsupported
	} else {
		caps.AssistantPrefill = claudeFeatureSupported
	}

	switch family + "-" + version {
	case "fable-5", "mythos-5", "opus-4.8", "opus-5":
		caps.MidConversationSystem = claudeFeatureSupported
		caps.MidConversationTools = claudeFeatureSupported
	case "sonnet-5":
		caps.MidConversationSystem = claudeFeatureUnsupported
		caps.MidConversationTools = claudeFeatureUnsupported
	}
	return caps
}

func claudeExactFeatureSupport(
	key string,
	family string,
	version string,
	supported map[string]struct{},
	explicitlyUnsupported bool,
) claudeFeatureSupport {
	if _, ok := supported[key]; ok {
		return claudeFeatureSupported
	}
	if explicitlyUnsupported {
		return claudeFeatureUnsupported
	}

	// Older released families are known not to implement these newer tool
	// protocols. Versions newer than the newest documented matrix remain unknown
	// rather than being guessed supported or unsupported.
	switch family {
	case "opus", "sonnet", "haiku":
		if !claudeVersionAtLeast(version, 4, 5) {
			return claudeFeatureUnsupported
		}
	case "fable", "mythos":
		if !claudeVersionAtLeast(version, 5, 0) {
			return claudeFeatureUnsupported
		}
	}
	return claudeFeatureUnknown
}

func directAnthropicReasoningCapability(model string, metadata directAnthropicModelMetadata) ModelReasoningCapability {
	if !metadata.CapabilitiesPresent {
		if _, _, ok := canonicalClaudeModelFamilyVersion(model); !ok {
			return ModelReasoningCapability{}
		}
		capability := fallbackClaudeReasoningCapability(model)
		if !capability.SupportsThinking && len(capability.Efforts) == 0 {
			return ModelReasoningCapability{}
		}
		return capability
	}

	capability := ModelReasoningCapability{
		ModelID:    model,
		EffortPath: ReasoningSchemaNone,
	}
	thinking := metadata.Capabilities.Thinking
	capability.SupportsThinking = thinking.Supported
	if thinking.Supported {
		if thinking.Types.Adaptive.Supported {
			capability.ThinkingTypes = append(capability.ThinkingTypes, "adaptive")
		}
		if thinking.Types.Enabled.Supported {
			capability.ThinkingTypes = append(capability.ThinkingTypes, "enabled")
			capability.SupportsBudgetTokens = true
		}
	}

	effort := metadata.Capabilities.Effort
	if effort.Supported {
		capability.EffortPath = ReasoningSchemaOutputConfig
		if effort.Low.Supported {
			capability.Efforts = append(capability.Efforts, "low")
		}
		if effort.Medium.Supported {
			capability.Efforts = append(capability.Efforts, "medium")
		}
		if effort.High.Supported {
			capability.Efforts = append(capability.Efforts, "high")
		}
		if effort.XHigh.Supported {
			capability.Efforts = append(capability.Efforts, "xhigh")
		}
		if effort.Max.Supported {
			capability.Efforts = append(capability.Efforts, "max")
		}
	}
	return capability
}

// resolveClaudeProviderReasoningCapability returns only model/provider facts
// from the maintained public Claude fallback table. Kiro's live
// additionalModelRequestFieldsSchema is deliberately not consulted here: it
// describes one adapter, not the Messages contract on another provider.
func resolveClaudeProviderReasoningCapability(model string) ModelReasoningCapability {
	// The fallback table uses prefix matching for historical Kiro aliases. Do
	// not let that classify an arbitrary private alias such as
	// "claude-opus-5-private" as an official provider model.
	if _, _, ok := canonicalClaudeModelFamilyVersion(model); !ok {
		return ModelReasoningCapability{}
	}
	capability := fallbackClaudeReasoningCapability(model)
	if !capability.SupportsThinking && len(capability.Efforts) == 0 {
		// Canonical but not present in the maintained capability table. Treat it
		// as unknown rather than guessing that a future model is unsupported.
		return ModelReasoningCapability{}
	}
	return capability
}
