package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

func TestSonnet55ExactIdentityAndCapability(t *testing.T) {
	for _, id := range []string{"claude-sonnet-5-5", "claude-sonnet-5.5", "anthropic.claude-sonnet-5-5:0", "us.anthropic.claude-sonnet-5-5-v1:0"} {
		if !isClaudeSonnet55Model(id) {
			t.Fatalf("Sonnet 5.5 ID %q not recognized", id)
		}
		actual, _ := ParseModelAndThinking(id, "-thinking")
		if actual != id {
			t.Fatalf("provider ID rewritten: %q -> %q", id, actual)
		}
		caps := resolveClaudeModelCapabilities(id)
		if caps.MidConversationSystem != claudeFeatureSupported || caps.MidConversationTools != claudeFeatureSupported || caps.ToolSearch != claudeFeatureSupported || caps.ProgrammaticToolCalling != claudeFeatureSupported {
			t.Fatalf("incorrect Sonnet 5.5 protocol capabilities: %+v", caps)
		}
		thinking := fallbackClaudeReasoningCapability(id)
		if len(thinking.ThinkingTypes) != 2 || thinking.ThinkingTypes[0] != "adaptive" || thinking.ThinkingTypes[1] != "between_tools" || thinking.DefaultThinkingType != "adaptive" {
			t.Fatalf("Sonnet 5.5 inherited Sonnet 5 thinking: %+v", thinking)
		}
	}
	if isClaudeSonnet55Model("claude-sonnet-5") || isClaudeSonnet55Model("vendor/claude-sonnet-5-5-private") {
		t.Fatal("prefix/opaque alias misclassified as official Sonnet 5.5")
	}
	previous := fallbackClaudeReasoningCapability("claude-sonnet-5")
	if len(previous.ThinkingTypes) != 2 || previous.ThinkingTypes[1] != "disabled" {
		t.Fatal("Sonnet 5.0 regression")
	}
}

func TestSonnet55NativeRequestContractProviderBoundary(t *testing.T) {
	official := &config.Account{AuthMethod: "anthropic_api", BaseURL: "https://api.anthropic.com/v1"}
	compatible := &config.Account{AuthMethod: "anthropic_api", BaseURL: "https://custom.invalid/v1"}
	bedrock := &config.Account{AuthMethod: "bedrock"}
	cases := []struct{ name, fields, want string }{
		{"default", "", ""},
		{"adaptive", `,"thinking":{"type":"adaptive"}`, ""},
		{"between tools", `,"thinking":{"type":"between_tools"}`, ""},
		{"between tools high", `,"thinking":{"type":"between_tools"},"output_config":{"effort":"high"}`, ""},
		{"between tools medium", `,"thinking":{"type":"between_tools"},"output_config":{"effort":"medium"}`, ""},
		{"between tools xhigh", `,"thinking":{"type":"between_tools"},"output_config":{"effort":"xhigh"}`, "between_tools"},
		{"between tools max", `,"thinking":{"type":"between_tools"},"output_config":{"effort":"max"}`, "between_tools"},
		{"between tools display empty", `,"thinking":{"type":"between_tools","display":""}`, "display"},
		{"between tools budget zero", `,"thinking":{"type":"between_tools","budget_tokens":0}`, "budget_tokens"},
		{"between tools binding", `,"thinking":{"type":"between_tools","block_binding":null}`, "block_binding"},
		{"disabled", `,"thinking":{"type":"disabled"}`, "adaptive or between_tools"},
		{"enabled", `,"thinking":{"type":"enabled","budget_tokens":1024}`, "adaptive or between_tools"},
		{"forced any", `,"tool_choice":{"type":"any"}`, "forced tool_choice"},
		{"forced tool", `,"tool_choice":{"type":"tool","name":"run"}`, "forced tool_choice"},
		{"auto", `,"tool_choice":{"type":"auto"}`, ""},
		{"temperature 0", `,"temperature":0`, "temperature"},
		{"temperature default", `,"temperature":1`, ""},
		{"top p 0.99", `,"top_p":0.99`, "top_p"},
		{"top p default", `,"top_p":1`, ""},
		{"top k zero", `,"top_k":0`, "top_k"},
	}
	var handler Handler
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"claude-sonnet-5-5","max_tokens":2048,"messages":[{"role":"user","content":"hello"}]` + tc.fields + `}`
			var req ClaudeRequest
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatal(err)
			}
			got := claudeSonnet55RequestViolation(&req)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("violation=%q, want substring=%q", got, tc.want)
			}
			profile := claudeTransportRequirements{Sonnet55ContractError: got}
			for _, acc := range []*config.Account{official, bedrock} {
				ok, _ := handler.claudeAccountSupportsRequestedProtocol(acc, "claude-sonnet-5-5", profile)
				if ok != (tc.want == "") {
					t.Fatalf("native provider accepted/rejected incorrectly: %s", got)
				}
			}
			if ok, _ := handler.claudeAccountSupportsRequestedProtocol(compatible, "claude-sonnet-5-5", profile); !ok {
				t.Fatal("compatible provider incorrectly received official contract")
			}
		})
	}
	var metadata directAnthropicModelMetadata
	metadata.CapabilitiesPresent = true
	metadata.Capabilities.Thinking.Supported = true
	metadata.Capabilities.Thinking.Types.BetweenTools.Supported = true
	cap := directAnthropicReasoningCapability("claude-sonnet-5-5", metadata)
	if _, ok := matchThinkingType("between_tools", cap.ThinkingTypes); !ok {
		t.Fatal("live Models API between_tools lost")
	}
}

func TestKiroModelPaginationHardening(t *testing.T) {
	cases := []struct {
		name      string
		pages     map[string]string
		statuses  map[string]int
		wantCount int
		wantErr   string
	}{
		{"two pages", map[string]string{"": `{"models":[{"modelId":"claude-sonnet-5-5"}],"nextToken":"p2"}`, "p2": `{"models":[{"modelId":"gpt-5.6"}]}`}, nil, 2, ""},
		{"empty catalog", map[string]string{"": `{"models":[]}`}, nil, 0, ""},
		{"duplicate", map[string]string{"": `{"models":[{"modelId":"A"}],"nextToken":"p2"}`, "p2": `{"models":[{"modelId":"A"}]}`}, nil, 0, "duplicate"},
		{"repeat cursor", map[string]string{"": `{"models":[],"nextToken":"again"}`, "again": `{"models":[],"nextToken":"again"}`}, nil, 0, "repeated"},
		{"failed second page", map[string]string{"": `{"models":[{"modelId":"A"}],"nextToken":"p2"}`, "p2": `{"error":"bad"}`}, map[string]int{"p2": 503}, 0, "page 2"},
		{"invalid JSON", map[string]string{"": `{"models":[}`}, nil, 0, "malformed"},
		{"missing models", map[string]string{"": `{"nextToken":"p2"}`}, nil, 0, "missing models"},
		{"null models", map[string]string{"": `{"models":null}`}, nil, 0, "missing models"},
		{"wrong type", map[string]string{"": `{"models":{}}`}, nil, 0, "invalid models"},
		{"trailing JSON", map[string]string{"": `{"models":[]} {"models":[]}`}, nil, 0, "trailing"},
		{"invalid ID", map[string]string{"": `{"models":[{"modelId":" "}]}`}, nil, 0, "invalid model ID"},
		{"too large", map[string]string{"": strings.Repeat("a", maxKiroModelsPageBytes+1)}, nil, 0, "too large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				token := r.URL.Query().Get("nextToken")
				if status := tc.statuses[token]; status > 0 {
					w.WriteHeader(status)
				}
				if body, exists := tc.pages[token]; exists {
					fmt.Fprint(w, body)
				} else {
					fmt.Fprint(w, `{"models":[]}`)
				}
			}))
			defer srv.Close()
			models, err := fetchKiroModelsPagesContext(context.Background(), srv.URL+"/ListAvailableModels?origin=AI_EDITOR&maxResults=50", srv.Client().Do)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected %q, got models=%v err=%v", tc.wantErr, models, err)
				}
				if models != nil {
					t.Fatalf("published incomplete catalog: %v", models)
				}
			} else if err != nil || len(models) != tc.wantCount {
				t.Fatalf("models=%v err=%v", models, err)
			}
			if hits == 0 {
				t.Fatal("no HTTP requests")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetchKiroModelsPagesContext(ctx, "https://example.invalid/ListAvailableModels", func(*http.Request) (*http.Response, error) { t.Fatal("called despite cancel"); return nil, nil }); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
}

func TestAccountSpecificContextUsageWindow(t *testing.T) {
	accountA := &config.Account{ID: "acctA", DispatchGeneration: 1}
	accountB := &config.Account{ID: "acctB", DispatchGeneration: 1}
	snapshot := func(limit int) accountModelSnapshot {
		info := ModelInfo{ModelId: "same-model"}
		info.TokenLimits = &struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}{MaxInputTokens: limit}
		return accountModelSnapshot{LifecycleGeneration: 1, Models: []ModelInfo{info}, FreshUntil: time.Now().Add(time.Hour)}
	}
	h := &Handler{accountModelSnapshots: map[accountLifecycleKey]accountModelSnapshot{
		{accountID: "acctA", generation: 1}: snapshot(1_000_000),
		{accountID: "acctB", generation: 1}: snapshot(200_000),
	}}
	if got := h.usageInputFromContextPercentage(accountA, "same-model", "same-model", 50); got != 500_000 {
		t.Fatalf("account A tokens %d", got)
	}
	if got := h.usageInputFromContextPercentage(accountB, "same-model", "same-model", 50); got != 100_000 {
		t.Fatalf("account B tokens %d", got)
	}
	if got := h.usageInputFromContextPercentage(accountA, "unknown-model", "unknown-model", 50); got != 100_000 {
		t.Fatalf("missing metadata must use conservative fallback, got %d", got)
	}
}

func TestSonnet55BetweenToolsPerMessageEffort(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  string
	}{
		{"medium", ""},
		{"high", "per-message"},
	} {
		t.Run(tc.level, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"claude-sonnet-5-5","max_tokens":1000,"thinking":{"type":"between_tools"},"output_config":{"effort":"medium"},"messages":[{"role":"user","content":"hi","output_config":{"effort":%q}}]}`, tc.level)
			var req ClaudeRequest
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatal(err)
			}
			got := claudeSonnet55RequestViolation(&req)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("same/different effort got violation %q want %q", got, tc.want)
			}
		})
	}
}

func TestKiroPaginationCancellationAfterFirstPage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("nextToken") {
			t.Error("requested page after cancel")
		}
		fmt.Fprint(w, `{"models":[{"modelId":"A"}],"nextToken":"p2"}`)
	}))
	defer srv.Close()
	_, err := fetchKiroModelsPagesContext(ctx, srv.URL, func(req *http.Request) (*http.Response, error) {
		result, fetchErr := srv.Client().Do(req)
		cancel()
		return result, fetchErr
	})
	if err == nil {
		t.Fatal("cancellation must not publish incomplete catalog")
	}
}
