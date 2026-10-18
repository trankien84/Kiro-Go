package proxy

import (
	"strings"

	"kiro-go/diagnostics"
	"kiro-go/logger"
)

const claudeToolFeatureDeferredPreviewLimit = 5

type claudeToolFeatureStats struct {
	Tools                    int
	ClientToolSearch         bool
	ServerToolSearch         bool
	ServerToolSearchTools    int
	DeferredTools            int
	StrictTools              int
	InputExampleTools        int
	EagerInputStreamingTools int
	ProgrammaticRequested    bool
	DeferredPreview          []string
}

// inspectClaudeToolFeatures summarizes the normalized catalog only. It does not
// inspect input schemas, descriptions, MCP URLs, or tool arguments, so the
// diagnostic remains useful without dumping request content or credentials.
func inspectClaudeToolFeatures(catalog *completionGuardNormalizedCatalog) claudeToolFeatureStats {
	stats := claudeToolFeatureStats{}
	if catalog == nil {
		return stats
	}

	stats.Tools = catalog.DeclaredToolCount
	stats.ServerToolSearch = catalog.HasToolSearch
	stats.ProgrammaticRequested = catalog.ProgrammaticRequested

	if entry, ok := catalog.ClientEntry("tool_search"); ok && entry.Kind == completionGuardToolKindClient {
		stats.ClientToolSearch = true
	}

	for _, entry := range catalog.Entries {
		if entry.Deferred {
			stats.DeferredTools++
		}
		if entry.Deferred && len(stats.DeferredPreview) < claudeToolFeatureDeferredPreviewLimit {
			name := strings.TrimSpace(entry.Name)
			if name == "" {
				name = strings.TrimSpace(entry.Type)
			}
			if name != "" {
				stats.DeferredPreview = append(stats.DeferredPreview, name)
			}
		}
		if entry.Strict {
			stats.StrictTools++
		}
		if entry.InputExamples {
			stats.InputExampleTools++
		}
		if entry.EagerInputStreaming {
			stats.EagerInputStreamingTools++
		}
		if definition, ok := completionGuardLookupServerToolDefinition(entry.Type); ok &&
			definition.Feature == completionGuardToolFeatureToolSearch {
			stats.ServerToolSearchTools++
		}
	}

	return stats
}

func logClaudeToolFeatures(catalog *completionGuardNormalizedCatalog) {
	if !diagnostics.Reasoning() {
		return
	}
	stats := inspectClaudeToolFeatures(catalog)
	logger.Infof(
		"[ClaudeToolFeatures] tools=%d clientToolSearch=%t serverToolSearch=%t serverToolSearchTools=%d deferredTools=%d strictTools=%d inputExampleTools=%d eagerInputStreamingTools=%d programmaticRequested=%t deferredPreview=%v",
		stats.Tools,
		stats.ClientToolSearch,
		stats.ServerToolSearch,
		stats.ServerToolSearchTools,
		stats.DeferredTools,
		stats.StrictTools,
		stats.InputExampleTools,
		stats.EagerInputStreamingTools,
		stats.ProgrammaticRequested,
		stats.DeferredPreview,
	)
}
