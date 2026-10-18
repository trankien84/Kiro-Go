package proxy

import (
	"fmt"
	"reflect"
	"testing"
)

func TestInspectClaudeToolFeaturesSummarizesNormalizedCatalog(t *testing.T) {
	catalog := &completionGuardNormalizedCatalog{
		DeclaredToolCount:        9,
		ClientToolsByName:        map[string]completionGuardCatalogTool{},
		HasToolSearch:            true,
		ProgrammaticRequested:    true,
		ExplicitToolsByName:      map[string]completionGuardCatalogTool{},
		ServerCapabilitiesByName: map[string]completionGuardServerToolCapability{},
	}
	clientSearch := completionGuardCatalogTool{
		Name: "tool_search",
		Kind: completionGuardToolKindClient,
	}
	catalog.ClientToolsByName[clientSearch.Name] = clientSearch
	catalog.Entries = append(catalog.Entries,
		clientSearch,
		completionGuardCatalogTool{
			Name: "tool_search_tool_regex",
			Type: "tool_search_tool_regex_20251119",
			Kind: completionGuardToolKindServer,
		},
		completionGuardCatalogTool{
			Name:                "mcp_example_one",
			Kind:                completionGuardToolKindClient,
			Deferred:            true,
			Strict:              true,
			InputExamples:       true,
			EagerInputStreaming: true,
		},
	)
	for i := 2; i <= 7; i++ {
		catalog.Entries = append(catalog.Entries, completionGuardCatalogTool{
			Name:     fmt.Sprintf("mcp_example_%d", i),
			Kind:     completionGuardToolKindClient,
			Deferred: true,
		})
	}

	got := inspectClaudeToolFeatures(catalog)
	if got.Tools != 9 || !got.ClientToolSearch || !got.ServerToolSearch || got.ServerToolSearchTools != 1 {
		t.Fatalf("tool-search stats=%+v", got)
	}
	if got.DeferredTools != 7 || got.StrictTools != 1 || got.InputExampleTools != 1 || got.EagerInputStreamingTools != 1 {
		t.Fatalf("feature stats=%+v", got)
	}
	if !got.ProgrammaticRequested {
		t.Fatalf("programmaticRequested=false stats=%+v", got)
	}
	wantPreview := []string{"mcp_example_one", "mcp_example_2", "mcp_example_3", "mcp_example_4", "mcp_example_5"}
	if !reflect.DeepEqual(got.DeferredPreview, wantPreview) {
		t.Fatalf("deferredPreview=%v want=%v", got.DeferredPreview, wantPreview)
	}
}

func TestInspectClaudeToolFeaturesNilCatalog(t *testing.T) {
	if got := inspectClaudeToolFeatures(nil); !reflect.DeepEqual(got, claudeToolFeatureStats{}) {
		t.Fatalf("nil catalog stats=%+v", got)
	}
}
