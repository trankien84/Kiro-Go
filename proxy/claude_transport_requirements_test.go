package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

func testClaudeSemantic(t *testing.T, body string, headers http.Header) (*ClaudeRequest, claudeSemanticRequest) {
	t.Helper()
	var req ClaudeRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	semantic, reason := buildClaudeSemanticRequest(&req, headers, []byte(body))
	if reason != "" {
		t.Fatalf("semantic reason: %s", reason)
	}
	return &req, semantic
}

func initClaudeTransportRoutingTestConfig(t *testing.T) {
	t.Helper()
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
}

func TestResolveClaudeRoutingContextRebuildsForSyntheticBody(t *testing.T) {
	initClaudeTransportRoutingTestConfig(t)
	bodyA := []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"first"}]}`)
	bodyB := []byte(`{"model":"claude-opus-5","max_tokens":64,"stop_sequences":["<END>"],"messages":[{"role":"user","content":"audit"}]}`)

	routingA, err := resolveClaudeRoutingContext(
		context.Background(),
		bodyA,
		nil,
		"claude-opus-5",
	)
	if err != nil {
		t.Fatalf("build outer routing: %v", err)
	}
	if routingA.Requirements.RequiresStopSequences {
		t.Fatalf("outer request unexpectedly requires stop sequences: %#v", routingA.Requirements.StopSequences)
	}

	ctx := withClaudeRoutingContext(context.Background(), routingA)
	routingB, err := resolveClaudeRoutingContext(
		ctx,
		bodyB,
		nil,
		"claude-opus-5",
	)
	if err != nil {
		t.Fatalf("rebuild synthetic routing: %v", err)
	}
	if !routingB.Requirements.RequiresStopSequences ||
		!reflect.DeepEqual(routingB.Requirements.StopSequences, []string{"<END>"}) {
		t.Fatalf("synthetic body inherited stale outer routing: %#v", routingB.Requirements)
	}
	if string(routingB.Semantic.Body) == string(routingA.Semantic.Body) {
		t.Fatalf("synthetic invocation reused outer semantic body: %s", routingB.Semantic.Body)
	}
}

func TestResolveClaudeRoutingContextDoesNotLeakOuterTransparentOnlyRequirement(t *testing.T) {
	initClaudeTransportRoutingTestConfig(t)
	bodyA := []byte(`{"model":"claude-opus-5","max_tokens":64,"future_native_state":{"opaque":true},"messages":[{"role":"user","content":"outer"}]}`)
	bodyB := []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"audit"}]}`)

	routingA, err := resolveClaudeRoutingContext(context.Background(), bodyA, nil, "claude-opus-5")
	if err != nil {
		t.Fatalf("build transparent-only outer routing: %v", err)
	}
	if routingA.Requirements.AllowsKiro() {
		t.Fatalf("outer request with unknown native state unexpectedly allows Kiro: %s", routingA.Requirements.Summary())
	}

	ctx := withClaudeRoutingContext(context.Background(), routingA)
	routingB, err := resolveClaudeRoutingContext(ctx, bodyB, nil, "claude-opus-5")
	if err != nil {
		t.Fatalf("rebuild plain synthetic routing: %v", err)
	}
	if !routingB.Requirements.AllowsKiro() {
		t.Fatalf("plain synthetic request inherited transparent-only outer requirements: %s", routingB.Requirements.Summary())
	}
}

func TestResolveClaudeRoutingContextRebuildsForSemanticHeaderChange(t *testing.T) {
	initClaudeTransportRoutingTestConfig(t)
	body := []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	outerHeaders := http.Header{
		"Anthropic-Beta": []string{"effort-2025-11-24"},
	}

	routingA, err := resolveClaudeRoutingContext(
		context.Background(),
		body,
		outerHeaders,
		"claude-opus-5",
	)
	if err != nil {
		t.Fatalf("build outer routing: %v", err)
	}
	if got := routingA.Semantic.Headers.Get("Anthropic-Beta"); got == "" {
		t.Fatal("outer semantic beta header was not preserved")
	}

	ctx := withClaudeRoutingContext(context.Background(), routingA)
	routingB, err := resolveClaudeRoutingContext(
		ctx,
		body,
		nil,
		"claude-opus-5",
	)
	if err != nil {
		t.Fatalf("rebuild routing after header change: %v", err)
	}
	if got := routingB.Semantic.Headers.Get("Anthropic-Beta"); got != "" {
		t.Fatalf("stale outer semantic headers leaked into synthetic invocation: %q", got)
	}
}

func TestResolveClaudeRoutingContextReusesExactInvocation(t *testing.T) {
	initClaudeTransportRoutingTestConfig(t)
	body := []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	headers := http.Header{
		"Anthropic-Beta": []string{"effort-2025-11-24"},
	}
	routing, err := resolveClaudeRoutingContext(
		context.Background(),
		body,
		headers,
		"claude-opus-5",
	)
	if err != nil {
		t.Fatalf("build routing: %v", err)
	}
	// A sentinel that cannot come from analysis proves the cached plan was reused
	// only when both body and semantic headers identify the same invocation.
	routing.Requirements.StopSequences = []string{"cached-sentinel"}
	ctx := withClaudeRoutingContext(context.Background(), routing)

	reused, err := resolveClaudeRoutingContext(ctx, body, headers, "claude-opus-5")
	if err != nil {
		t.Fatalf("reuse routing: %v", err)
	}
	if !reflect.DeepEqual(reused.Requirements.StopSequences, []string{"cached-sentinel"}) {
		t.Fatalf("exact invocation did not reuse cached routing: %#v", reused.Requirements.StopSequences)
	}
}

func TestClaudeSemanticHeaderAndBodyBetasAreEquivalent(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	reqHeader, semanticHeader := testClaudeSemantic(t, body, http.Header{
		"Anthropic-Beta": []string{"effort-2025-11-24"},
	})
	bodyWithBetas := `{"model":"claude-opus-5","max_tokens":64,"betas":["effort-2025-11-24"],"messages":[{"role":"user","content":"hi"}]}`
	reqBody, semanticBody := testClaudeSemantic(t, bodyWithBetas, nil)

	headerRequirements, headerErr := analyzeClaudeTransportRequirements(reqHeader, semanticHeader, "claude-opus-5")
	bodyRequirements, bodyErr := analyzeClaudeTransportRequirements(reqBody, semanticBody, "claude-opus-5")
	if headerErr != nil || bodyErr != nil {
		t.Fatalf("analysis errors: header=%v body=%v", headerErr, bodyErr)
	}
	if headerRequirements.Allowed != bodyRequirements.Allowed ||
		!reflect.DeepEqual(semanticHeader.Betas.Values(), semanticBody.Betas.Values()) {
		t.Fatalf("semantic mismatch: header=%#v body=%#v", headerRequirements, bodyRequirements)
	}
	if strings.Contains(string(semanticBody.Body), `"betas"`) {
		t.Fatalf("compatibility body field leaked: %s", semanticBody.Body)
	}
}

func TestClaudeSyntheticThinkingIsKiroOnly(t *testing.T) {
	signature := buildKiroThinkingSignature("claude-opus-5", "x")
	body := fmt.Sprintf(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":%q},{"type":"tool_use","id":"t1","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`, signature)
	req, semantic := testClaudeSemantic(t, body, nil)
	requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !requirements.Allows(claudeTransportKiroNative) ||
		requirements.Allows(claudeTransportCustomRelay) ||
		requirements.Allows(claudeTransportDirectAnthropic) ||
		requirements.Allows(claudeTransportBedrockInvoke) ||
		requirements.Allows(claudeTransportBedrockConverse) {
		t.Fatalf("unexpected allowed mask: %#v", requirements)
	}
}

func TestClaudeSyntheticThinkingConflictsWithMCP(t *testing.T) {
	signature := buildKiroThinkingSignature("claude-opus-5", "x")
	body := fmt.Sprintf(`{"model":"claude-opus-5","max_tokens":64,"mcp_servers":[{"type":"url","name":"docs","url":"https://example.com/mcp"}],"tools":[{"type":"mcp_toolset","mcp_server_name":"docs"}],"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":%q}]},{"role":"user","content":"continue"}]}`, signature)
	req, semantic := testClaudeSemantic(t, body, nil)
	_, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr == nil || validationErr.Code != "invalid_request_error" ||
		validationErr.DetailCode != "unsupported_transport_feature" {
		t.Fatalf("validation error=%#v", validationErr)
	}
}

func TestClaudeKiroOriginProviderThinkingRoundTripStaysOnKiro(t *testing.T) {
	response := &ClaudeResponse{Content: []ClaudeContentBlock{{Type: "thinking", Thinking: "summary"}}}
	attachKiroThinkingSignature(response, "claude-opus-5", "", "provider-issued-opaque-signature")
	signature := response.Content[0].Signature
	if !isKiroThinkingSignature(signature) {
		t.Fatalf("Kiro response signature was not localized: %q", signature)
	}

	body := fmt.Sprintf(`{
		"model":"claude-opus-5",
		"max_tokens":64,
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"xhigh","format":{"type":"json_schema","schema":{"type":"object"}}},
		"messages":[
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"summary","signature":%q},
				{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"pwd"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"/tmp"}]}
		]
	}`, signature)
	req, semantic := testClaudeSemantic(t, body, http.Header{
		"Anthropic-Beta": []string{"claude-code-20250219,structured-outputs-2025-12-15,effort-2025-11-24"},
	})
	requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !requirements.Allows(claudeTransportKiroNative) {
		t.Fatalf("Kiro-origin continuation excluded Kiro: %s", requirements.Summary())
	}
	if requirements.Allows(claudeTransportDirectAnthropic) ||
		requirements.Allows(claudeTransportBedrockInvoke) ||
		requirements.Allows(claudeTransportCustomRelay) {
		t.Fatalf("Kiro-origin signature must pin continuation to Kiro: %#v", requirements)
	}
}

func TestClaudeProviderThinkingAllowsOnlyTransparentOrBedrockInvoke(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"provider-signature"}]},{"role":"user","content":"continue"}]}`
	req, semantic := testClaudeSemantic(t, body, nil)
	requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if requirements.Allows(claudeTransportKiroNative) ||
		requirements.Allows(claudeTransportCustomRelay) ||
		!requirements.Allows(claudeTransportDirectAnthropic) ||
		!requirements.Allows(claudeTransportBedrockInvoke) ||
		requirements.Allows(claudeTransportBedrockConverse) {
		t.Fatalf("unexpected allowed mask: %#v", requirements)
	}
}

func TestClaudeServiceTierAndMetadataAreEmulatedByKiro(t *testing.T) {
	for _, extra := range []string{
		`"service_tier":"auto"`,
		`"service_tier":"standard_only"`,
		`"metadata":{"user_id":"opaque"}`,
	} {
		body := `{"model":"claude-opus-5","max_tokens":64,` + extra + `,"messages":[{"role":"user","content":"hi"}]}`
		req, semantic := testClaudeSemantic(t, body, nil)
		requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
		if validationErr != nil {
			t.Fatalf("%s: %v", extra, validationErr)
		}
		want := claudeTransportMaskKiro | claudeTransportMaskCustom
		if requirements.Allowed != want {
			t.Fatalf("%s allowed=%08b want=%08b", extra, requirements.Allowed, want)
		}
	}
}

func TestClaudeInvalidServiceTierFailsLocally(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":64,"service_tier":"priority","messages":[{"role":"user","content":"hi"}]}`
	req, semantic := testClaudeSemantic(t, body, nil)
	_, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr == nil || validationErr.Code != "invalid_request_error" {
		t.Fatalf("validation error=%#v", validationErr)
	}
}

func TestClaudeMidConversationModelCapabilities(t *testing.T) {
	body := `{"model":"placeholder","max_tokens":64,"messages":[{"role":"user","content":"hi"},{"role":"system","content":"new policy"}]}`
	for _, tc := range []struct {
		model   string
		code    string
		allowed claudeTransportMask
	}{
		{model: "claude-opus-5", allowed: claudeTransportMaskKiro | claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke},
		{model: "claude-opus-4-8", allowed: claudeTransportMaskKiro | claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke},
		{model: "claude-sonnet-5", allowed: claudeTransportMaskKiro | claudeTransportMaskCustom},
		{model: "private-alias", allowed: claudeTransportMaskKiro | claudeTransportMaskCustom},
	} {
		req, semantic := testClaudeSemantic(t, body, nil)
		requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, tc.model)
		if tc.code != "" {
			if validationErr == nil || validationErr.Code != tc.code {
				t.Fatalf("model=%s validation=%#v", tc.model, validationErr)
			}
			continue
		}
		if validationErr != nil || requirements.Allowed != tc.allowed {
			t.Fatalf("model=%s requirements=%#v validation=%v", tc.model, requirements, validationErr)
		}
	}
}

func TestClaudeUserProfileRequiresBetaAndAllowsKiroIdentityOmission(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req, semantic := testClaudeSemantic(t, body, http.Header{
		"Anthropic-User-Profile-Id": []string{"user-123"},
	})
	_, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr == nil || validationErr.Code != "invalid_request_error" {
		t.Fatalf("missing beta validation=%#v", validationErr)
	}

	req, semantic = testClaudeSemantic(t, body, http.Header{
		"Anthropic-User-Profile-Id": []string{"user-123"},
		"Anthropic-Beta":            []string{"user-profiles-2026-03-24"},
	})
	requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	want := claudeTransportMaskKiro | claudeTransportMaskCustom
	if validationErr != nil || requirements.Allowed != want || !requirements.AllowsKiro() {
		t.Fatalf("requirements=%#v validation=%v", requirements, validationErr)
	}
	if requirements.KiroProjection.Fidelity != claudeProjectionEmulated {
		t.Fatalf("projection=%s summary=%s", requirements.KiroProjection.Fidelity, requirements.KiroProjection.Summary())
	}
}

func TestClaudeProtocolCapabilitiesAreEvaluatedPerSelectedDeployment(t *testing.T) {
	official := &config.Account{
		ID:         "official",
		AuthMethod: "anthropic_api",
		BaseURL:    "https://api.anthropic.com/v1",
	}
	compatible := &config.Account{
		ID:         "compatible",
		AuthMethod: "anthropic_api",
		BaseURL:    "https://provider.example/anthropic/v1",
	}
	bedrock := &config.Account{ID: "bedrock", AuthMethod: "bedrock"}

	tests := []struct {
		name         string
		account      *config.Account
		model        string
		requirements claudeTransportRequirements
		want         bool
	}{
		{
			name:    "official sonnet 5 rejects tool search",
			account: official,
			model:   "claude-sonnet-5",
			requirements: claudeTransportRequirements{
				ProtocolCapabilities: resolveClaudeModelCapabilities("claude-sonnet-5"),
				RequiresToolSearch:   true,
			},
			want: false,
		},
		{
			name:    "compatible endpoint owns tool search semantics",
			account: compatible,
			model:   "claude-sonnet-5",
			requirements: claudeTransportRequirements{
				ProtocolCapabilities: resolveClaudeModelCapabilities("claude-sonnet-5"),
				RequiresToolSearch:   true,
			},
			want: true,
		},
		{
			name:    "official opus supports programmatic calling",
			account: official,
			model:   "claude-opus-4.8",
			requirements: claudeTransportRequirements{
				ProtocolCapabilities:            resolveClaudeModelCapabilities("claude-opus-4.8"),
				RequiresProgrammaticToolCalling: true,
			},
			want: true,
		},
		{
			name:    "official sonnet 4.6 rejects prefill",
			account: official,
			model:   "claude-sonnet-4.6",
			requirements: claudeTransportRequirements{
				ProtocolCapabilities:     resolveClaudeModelCapabilities("claude-sonnet-4.6"),
				RequiresAssistantPrefill: true,
			},
			want: false,
		},
		{
			name:    "compatible endpoint may accept prefill",
			account: compatible,
			model:   "claude-sonnet-4.6",
			requirements: claudeTransportRequirements{
				ProtocolCapabilities:     resolveClaudeModelCapabilities("claude-sonnet-4.6"),
				RequiresAssistantPrefill: true,
			},
			want: true,
		},
		{
			name:    "bedrock invoke rejects unsupported prefill",
			account: bedrock,
			model:   "claude-sonnet-4.6",
			requirements: claudeTransportRequirements{
				ProtocolCapabilities:     resolveClaudeModelCapabilities("claude-sonnet-4.6"),
				RequiresAssistantPrefill: true,
			},
			want: false,
		},
	}
	var handler Handler
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _ := handler.claudeAccountSupportsRequestedProtocol(
				test.account,
				test.model,
				test.requirements,
			)
			if got != test.want {
				t.Fatalf("supported=%t want=%t", got, test.want)
			}
		})
	}
}

func TestClaudeToolSearchUsesBedrockInvokeOnlyWhenModelMatrixSupportsIt(t *testing.T) {
	body := `{"model":"placeholder","max_tokens":64,"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}],"messages":[{"role":"user","content":"find it"}]}`
	for _, test := range []struct {
		model   string
		allowed claudeTransportMask
	}{
		{"claude-opus-4.8", claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke},
		{"claude-sonnet-5", claudeTransportMaskCustom},
	} {
		req, semantic := testClaudeSemantic(t, body, nil)
		requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, test.model)
		if validationErr != nil {
			t.Fatalf("model=%s validation=%v", test.model, validationErr)
		}
		if requirements.Allowed != test.allowed || !requirements.RequiresToolSearch {
			t.Fatalf("model=%s requirements=%#v", test.model, requirements)
		}
	}
}

func TestClaudeTransportKindDistinguishesBedrockConverse(t *testing.T) {
	cases := []struct {
		account *config.Account
		kind    claudeTransportKind
	}{
		{account: nil, kind: claudeTransportUnsupported},
		{account: &config.Account{}, kind: claudeTransportUnsupported},
		{account: &config.Account{AuthMethod: "social"}, kind: claudeTransportKiroNative},
		{account: &config.Account{AuthMethod: "api_key"}, kind: claudeTransportKiroNative},
		{account: &config.Account{AuthMethod: "custom_api"}, kind: claudeTransportCustomRelay},
		{account: &config.Account{AuthMethod: "anthropic_api"}, kind: claudeTransportDirectAnthropic},
		{account: &config.Account{AuthMethod: "bedrock"}, kind: claudeTransportBedrockInvoke},
		{account: &config.Account{AuthMethod: "bedrock", BedrockUseConverse: true}, kind: claudeTransportBedrockConverse},
	}
	for _, tc := range cases {
		if got := claudeTransportKindForAccount(tc.account); got != tc.kind {
			t.Fatalf("account=%#v got=%s want=%s", tc.account, got, tc.kind)
		}
	}
}

func TestClaudeKiroRoutingConsumesAuthoritativeProjection(t *testing.T) {
	requirements := newClaudeTransportRequirements()
	requirements.KiroProjection.add(
		"test_feature",
		claudeProjectionUnsupported,
		"projection says Kiro cannot represent this feature",
	)
	account := &config.Account{ID: "kiro", AuthMethod: "social"}
	if claudeAccountCompatibleWithRequirements(account, false, requirements) {
		t.Fatal("Kiro routing ignored the authoritative unsupported projection")
	}
}

func TestBedrockBodyReceivesExactSupportedBetaSemantics(t *testing.T) {
	body, err := buildBedrockBodyWithSemanticHeaders(
		[]byte(`{"model":"claude-opus-5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		http.Header{
			"Anthropic-Version": []string{"2023-06-01"},
			"Anthropic-Beta":    []string{"effort-2025-11-24,tool-search-tool-2025-10-19"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["anthropic_version"] != bedrockAnthropicVersion {
		t.Fatalf("version=%#v", got["anthropic_version"])
	}
	want := []interface{}{"effort-2025-11-24", "tool-search-tool-2025-10-19"}
	if !reflect.DeepEqual(got["anthropic_beta"], want) {
		t.Fatalf("betas=%#v want=%#v", got["anthropic_beta"], want)
	}
	if _, exists := got["model"]; exists {
		t.Fatal("model leaked into Bedrock body")
	}
	if _, exists := got["stream"]; exists {
		t.Fatal("stream leaked into Bedrock body")
	}
}

func TestBedrockBodyMapsProviderSpecificBetaAndDropsDeclarationMarkers(t *testing.T) {
	body, err := buildBedrockBodyWithSemanticHeaders(
		[]byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"anthropic_beta":["fallback-credit-2026-06-01"]}`),
		http.Header{
			"Anthropic-Beta": []string{
				"fallback-credit-2026-07-01,claude-code-20250219,effort-2025-11-24",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := []interface{}{"fallback-credit-2026-06-09", "effort-2025-11-24"}
	if !reflect.DeepEqual(got["anthropic_beta"], want) {
		t.Fatalf("betas=%#v want=%#v", got["anthropic_beta"], want)
	}
}

func TestBedrockBodyRejectsUnsupportedBetaAndVersion(t *testing.T) {
	raw := []byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := buildBedrockBodyWithSemanticHeaders(raw, http.Header{
		"Anthropic-Beta": []string{"private-beta"},
	}); err == nil || !strings.Contains(err.Error(), "unsupported Anthropic beta") {
		t.Fatalf("beta error=%v", err)
	}
	if _, err := buildBedrockBodyWithSemanticHeaders(raw, http.Header{
		"Anthropic-Version": []string{"2099-01-01"},
	}); err == nil || !strings.Contains(err.Error(), "unsupported Anthropic version") {
		t.Fatalf("version error=%v", err)
	}
}

func TestClaudeAccountCompatibilityUsesTransportIntersection(t *testing.T) {
	kiroOnly := newClaudeTransportRequirements()
	kiroOnly.Require("synthetic", claudeTransportMaskKiro, "test")
	providerState := newClaudeTransportRequirements()
	providerState.Require(
		"provider_state",
		claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
		"test",
	)

	kiro := &config.Account{AuthMethod: "social"}
	relay := &config.Account{AuthMethod: "custom_api"}
	direct := &config.Account{AuthMethod: "anthropic_api"}
	invoke := &config.Account{AuthMethod: "bedrock"}
	converse := &config.Account{AuthMethod: "bedrock", BedrockUseConverse: true}

	if !claudeAccountCompatibleWithRequirements(kiro, false, kiroOnly) ||
		claudeAccountCompatibleWithRequirements(relay, false, kiroOnly) ||
		claudeAccountCompatibleWithRequirements(direct, false, kiroOnly) ||
		claudeAccountCompatibleWithRequirements(invoke, false, kiroOnly) {
		t.Fatal("Kiro-local continuation escaped Kiro transport affinity")
	}
	if claudeAccountCompatibleWithRequirements(kiro, false, providerState) ||
		claudeAccountCompatibleWithRequirements(relay, false, providerState) ||
		!claudeAccountCompatibleWithRequirements(direct, false, providerState) ||
		!claudeAccountCompatibleWithRequirements(invoke, false, providerState) ||
		claudeAccountCompatibleWithRequirements(converse, false, providerState) {
		t.Fatal("provider thinking state used an incompatible transport")
	}
	if claudeAccountCompatibleWithRequirements(relay, true, newClaudeTransportRequirements()) {
		t.Fatal("forwarded request was allowed to recurse through custom_api relay")
	}
}

func TestDirectAnthropicReasoningCompatibilityIsCandidateSpecific(t *testing.T) {
	now := time.Now()
	h := &Handler{
		accountModelSnapshots: testRuntimeSnapshots(map[string]accountModelSnapshot{
			"official": {
				Metadata: map[string]directAnthropicModelMetadata{
					"claude-opus-4.8": {
						ID:                  "claude-opus-4.8",
						CapabilitiesPresent: true,
						Capabilities: directAnthropicModelCapabilities{
							Thinking: directAnthropicThinkingCapability{
								Supported: true,
								Types: directAnthropicThinkingTypes{
									Adaptive: directAnthropicCapabilitySupport{Supported: true},
								},
							},
						},
					},
				},
				FreshUntil: now.Add(time.Hour),
				StaleUntil: now.Add(24 * time.Hour),
			},
		}),
	}
	account := &config.Account{
		ID:         "official",
		Enabled:    true,
		AuthMethod: "anthropic_api",
		BaseURL:    "https://api.anthropic.com/v1",
	}
	requirements := newClaudeTransportRequirements()
	requirements.RequestedThinkingType = "enabled"
	requirements.RequestedBudgetTokens = 4096
	route := resolvedProviderModel{
		ProviderID: "claude-opus-4.8",
		Reasoning: ModelReasoningCapability{
			ModelID: "claude-opus-4.8", SupportsThinking: true, ThinkingTypes: []string{"adaptive"},
		},
		ReasoningKnown: true,
	}
	if supported, _ := h.claudeAccountSupportsRequestedReasoning(account, route, requirements); supported {
		t.Fatal("direct official candidate accepted enabled on adaptive-only model")
	}

	// Kiro adapters are assessed separately and may perform the compatibility
	// mapping after account selection.
	kiro := &config.Account{ID: "kiro", Enabled: true, AuthMethod: "social"}
	if supported, reason := h.claudeAccountSupportsRequestedReasoning(kiro, route, requirements); !supported {
		t.Fatalf("candidate-local direct check leaked to Kiro: %s", reason)
	}
}

func TestBedrockInvokeRejectsLegacyEnabledForAdaptiveOnlyModel(t *testing.T) {
	h := &Handler{}
	account := &config.Account{ID: "bedrock", Enabled: true, AuthMethod: "bedrock"}
	requirements := newClaudeTransportRequirements()
	requirements.RequestedThinkingType = "enabled"
	requirements.RequestedBudgetTokens = 4096
	requirements.ProviderReasoning = ModelReasoningCapability{
		ModelID:              "claude-opus-4.8",
		SupportsThinking:     true,
		ThinkingTypes:        []string{"adaptive", "disabled"},
		SupportsBudgetTokens: false,
	}
	requirements.ProviderReasoningKnown = true
	route := resolvedProviderModel{
		ProviderID: "claude-opus-4.8",
		Reasoning: ModelReasoningCapability{
			ModelID: "claude-opus-4.8", SupportsThinking: true, ThinkingTypes: []string{"adaptive"},
		},
		ReasoningKnown: true,
	}
	if supported, _ := h.claudeAccountSupportsRequestedReasoning(account, route, requirements); supported {
		t.Fatal("Bedrock Invoke accepted legacy enabled on adaptive-only model")
	}
}

func TestCompatibleDirectEndpointCanUpstreamValidateDisabledMode(t *testing.T) {
	now := time.Now()
	h := &Handler{
		accountModelSnapshots: testRuntimeSnapshots(map[string]accountModelSnapshot{
			"compatible": {
				Metadata: map[string]directAnthropicModelMetadata{
					"private-model": {
						ID:                  "private-model",
						CapabilitiesPresent: true,
						Capabilities: directAnthropicModelCapabilities{
							Thinking: directAnthropicThinkingCapability{
								Supported: true,
								Types: directAnthropicThinkingTypes{
									Adaptive: directAnthropicCapabilitySupport{Supported: true},
								},
							},
						},
					},
				},
				FreshUntil: now.Add(time.Hour),
				StaleUntil: now.Add(24 * time.Hour),
			},
		}),
	}
	account := &config.Account{
		ID:         "compatible",
		Enabled:    true,
		AuthMethod: "anthropic_api",
		BaseURL:    "https://compatible.example/v1",
	}
	requirements := newClaudeTransportRequirements()
	requirements.RequestedThinkingType = "disabled"
	route := resolvedProviderModel{
		ProviderID:     "private-model",
		Reasoning:      ModelReasoningCapability{ModelID: "private-model", SupportsThinking: true, ThinkingTypes: []string{"adaptive"}},
		ReasoningKnown: true,
	}
	if supported, reason := h.claudeAccountSupportsRequestedReasoning(account, route, requirements); !supported {
		t.Fatalf("compatible endpoint was not allowed to validate disabled upstream: %s", reason)
	}
}

func TestClaudeTransportBodyLimits(t *testing.T) {
	if ok, _ := claudeTransportAcceptsBodySize(claudeTransportBedrockInvoke, claudeBedrockMaxBodyBytes); !ok {
		t.Fatal("exact Bedrock body boundary rejected")
	}
	if ok, _ := claudeTransportAcceptsBodySize(claudeTransportBedrockInvoke, claudeBedrockMaxBodyBytes+1); ok {
		t.Fatal("oversized Bedrock request accepted")
	}
	if ok, _ := claudeTransportAcceptsBodySize(claudeTransportDirectAnthropic, claudeDirectAnthropicMaxBodyBytes+1); ok {
		t.Fatal("oversized direct Anthropic request accepted")
	}
}

func TestClaudeSelectionExhaustionTaxonomy(t *testing.T) {
	requirements := newClaudeTransportRequirements()
	requirements.Require(
		"test_feature",
		claudeTransportMaskKiro,
		"test-only requirement",
	)

	if err := claudeSelectionExhaustionError(requirements, nil, false, false); err != nil {
		t.Fatalf("empty pool should not fabricate incompatibility: %v", err)
	}
	if err := claudeSelectionExhaustionError(
		requirements, []string{"direct:model-not-in-registry"}, true, false,
	); !errors.Is(err, errClaudeTransportIncompatible) {
		t.Fatalf("deterministic incompatibility=%v", err)
	}
	if err := claudeSelectionExhaustionError(
		requirements, []string{"kiro:cooldown"}, false, true,
	); !errors.Is(err, errClaudeTransportUnavailable) {
		t.Fatalf("availability failure=%v", err)
	}
	if err := claudeSelectionExhaustionError(
		requirements, []string{"direct:model-not-in-registry", "kiro:cooldown"}, true, true,
	); !errors.Is(err, errClaudeTransportUnavailable) {
		t.Fatalf("a compatible-but-unavailable candidate must keep 503 semantics: %v", err)
	}
}

func TestKiroCandidateStructuredOutputRequiresExactRouteEvidence(t *testing.T) {
	account := &config.Account{ID: "kiro", Enabled: true, AuthMethod: "social"}
	requirements := newClaudeTransportRequirements()
	requirements.RequiresStructuredOutputFormat = true

	if ok, _ := claudeKiroCandidateSupportsProjection(account, resolvedProviderModel{
		ProviderID:                 "claude-opus-5",
		KiroStructuredOutputKnown:  true,
		KiroStructuredOutputFormat: false,
	}, requirements); ok {
		t.Fatal("candidate without output_config.format support was accepted")
	}
	if ok, reason := claudeKiroCandidateSupportsProjection(account, resolvedProviderModel{
		ProviderID:                 "claude-opus-5",
		KiroStructuredOutputKnown:  true,
		KiroStructuredOutputFormat: true,
	}, requirements); !ok {
		t.Fatalf("candidate with exact format support was rejected: %s", reason)
	}
}

func TestKiroReasoningDefersNativeTypeMappingToExactCandidateAdapter(t *testing.T) {
	account := &config.Account{ID: "kiro", Enabled: true, AuthMethod: "social"}
	requirements := newClaudeTransportRequirements()
	requirements.RequestedThinkingType = "enabled"
	requirements.RequestedBudgetTokens = 4096
	route := resolvedProviderModel{
		ProviderID:     "claude-opus-5",
		ReasoningKnown: true,
		Reasoning: ModelReasoningCapability{
			ModelID:          "claude-opus-5",
			SupportsThinking: true,
			ThinkingTypes:    []string{"adaptive", "disabled"},
		},
	}
	if ok, reason := (&Handler{}).claudeAccountSupportsRequestedReasoning(account, route, requirements); !ok {
		t.Fatalf("Kiro native-type precheck bypassed compatibility adapter: %s", reason)
	}
}

func TestStopSequencesKeepKiroEligibleAsEmulatedProjection(t *testing.T) {
	req := &ClaudeRequest{Extra: map[string]json.RawMessage{
		"stop_sequences": json.RawMessage(`["DONE"]`),
	}}
	requirements, validationErr := analyzeClaudeTransportRequirementsWithInventoryAndCapabilities(
		req,
		claudeSemanticRequest{},
		"claude-opus-5",
		claudeContentInventory{},
		nil,
		claudeModelCapabilities{},
		buildClaudeKiroProjection(req, claudeContentInventory{}, claudeSemanticRequest{}),
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !requirements.AllowsKiro() || !requirements.RequiresStopSequences {
		t.Fatalf("Kiro was not eligible for stop emulation: %#v", requirements)
	}
	found := false
	for _, issue := range requirements.KiroProjection.Issues {
		if issue.Feature == "stop_sequences" {
			found = true
			if issue.Fidelity != claudeProjectionEmulated {
				t.Fatalf("fidelity=%v want emulated", issue.Fidelity)
			}
		}
	}
	if !found {
		t.Fatal("stop_sequences emulation was not reported in Kiro projection")
	}
}

func TestCompletionGuardAuditRequestRebuildsOuterRoutingContext(t *testing.T) {
	initClaudeTransportRoutingTestConfig(t)
	original := ClaudeRequest{
		Model:     "claude-opus-5",
		MaxTokens: 64,
		Messages: []ClaudeMessage{{
			Role:    "user",
			Content: "Fix the file and run tests.",
		}},
		Extra: map[string]json.RawMessage{
			"future_native_state": json.RawMessage(`{"opaque":true}`),
		},
	}
	outerBody, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	outerRouting, err := resolveClaudeRoutingContext(
		context.Background(),
		outerBody,
		nil,
		"claude-opus-5",
	)
	if err != nil {
		t.Fatalf("build outer routing: %v", err)
	}
	if outerRouting.Requirements.AllowsKiro() {
		t.Fatalf("outer transparent-only request unexpectedly allows Kiro: %s", outerRouting.Requirements.Summary())
	}

	auditReq := buildCompletionGuardAuditRequest(
		original,
		completionGuardAuditContext{UserRequest: "Fix the file and run tests."},
		"The change is complete.",
		"audit",
	)
	auditBody, err := json.Marshal(auditReq)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withClaudeRoutingContext(context.Background(), outerRouting)
	auditRouting, err := resolveClaudeRoutingContext(
		ctx,
		auditBody,
		nil,
		"claude-opus-5",
	)
	if err != nil {
		t.Fatalf("build audit routing: %v", err)
	}
	if !auditRouting.Requirements.AllowsKiro() {
		t.Fatalf("audit inherited outer transparent-only routing: %s", auditRouting.Requirements.Summary())
	}
	if strings.Contains(string(auditRouting.Semantic.Body), "future_native_state") {
		t.Fatalf("outer-only field leaked into audit semantic body: %s", auditRouting.Semantic.Body)
	}
	if string(auditRouting.Semantic.Body) == string(outerRouting.Semantic.Body) {
		t.Fatal("audit invocation reused the outer semantic body")
	}
}

func TestCompletionGuardContinuationRebuildsRoutingForKiroOriginThinking(t *testing.T) {
	initClaudeTransportRoutingTestConfig(t)
	original := ClaudeRequest{
		Model:     "claude-opus-5",
		MaxTokens: 64,
		Messages: []ClaudeMessage{{
			Role:    "user",
			Content: "Inspect the file and make the requested change.",
		}},
		Tools: []ClaudeTool{{
			Name:        "edit",
			Description: "Edit a file",
			InputSchema: map[string]interface{}{"type": "object"},
		}},
	}
	outerBody, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	outerRouting, err := resolveClaudeRoutingContext(
		context.Background(),
		outerBody,
		nil,
		"claude-opus-5",
	)
	if err != nil {
		t.Fatalf("build outer routing: %v", err)
	}
	if !outerRouting.Requirements.Allows(claudeTransportDirectAnthropic) {
		t.Fatalf("plain outer request unexpectedly excludes Direct Anthropic: %s", outerRouting.Requirements.Summary())
	}

	signature := buildKiroThinkingSignature("claude-opus-5", "summary")
	candidate := ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{
				Type:      "thinking",
				Thinking:  "summary",
				Signature: signature,
			},
			{
				Type: "text",
				Text: "I found the change but have not edited the file yet.",
			},
		},
	}
	continuation, err := buildCompletionGuardContinuationRequest(
		original,
		candidate,
		[]string{"edit the requested file"},
	)
	if err != nil {
		t.Fatal(err)
	}
	continuationBody, err := json.Marshal(continuation)
	if err != nil {
		t.Fatal(err)
	}

	ctx := withClaudeRoutingContext(context.Background(), outerRouting)
	continuationRouting, err := resolveClaudeRoutingContext(
		ctx,
		continuationBody,
		nil,
		"claude-opus-5",
	)
	if err != nil {
		t.Fatalf("build continuation routing: %v", err)
	}
	if !continuationRouting.Requirements.Allows(claudeTransportKiroNative) {
		t.Fatalf("Kiro-origin continuation excluded Kiro: %s", continuationRouting.Requirements.Summary())
	}
	if continuationRouting.Requirements.Allows(claudeTransportDirectAnthropic) ||
		continuationRouting.Requirements.Allows(claudeTransportBedrockInvoke) ||
		continuationRouting.Requirements.Allows(claudeTransportCustomRelay) {
		t.Fatalf("Kiro-origin continuation reused outer transparent routing: %s", continuationRouting.Requirements.Summary())
	}
	if string(continuationRouting.Semantic.Body) == string(outerRouting.Semantic.Body) {
		t.Fatal("continuation invocation reused the outer semantic body")
	}
}

func TestClaudeAdvancedToolUseDeclarationIsClientNeutral(t *testing.T) {
	tools := make([]map[string]interface{}, 207)
	for index := range tools {
		tools[index] = map[string]interface{}{
			"name":        fmt.Sprintf("client_tool_%03d", index),
			"description": "client-executed tool",
			"input_schema": map[string]interface{}{
				"type": "object",
			},
		}
	}
	bodyBytes, err := json.Marshal(map[string]interface{}{
		"model":      "claude-opus-5",
		"max_tokens": 64,
		"tools":      tools,
		"messages": []map[string]interface{}{{
			"role": "user", "content": "inspect the workspace",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, semantic := testClaudeSemantic(t, string(bodyBytes), http.Header{
		"Anthropic-Beta": []string{"advanced-tool-use-2025-11-20"},
	})
	requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !requirements.AllowsKiro() {
		t.Fatalf("declaration-only advanced-tool-use excluded Kiro: %s", requirements.Summary())
	}
	if requirements.RequiresToolSearch {
		t.Fatalf("beta declaration fabricated a concrete Tool Search requirement: %#v", requirements)
	}
	if requirements.Allowed != claudeTransportMaskKiro|claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke {
		t.Fatalf("allowed=%08b requirements=%s", requirements.Allowed, requirements.Summary())
	}
}

func TestClaudeAdvancedToolUseConcreteToolSearchStillFailsClosedOnKiro(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":64,"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},{"name":"workspace_read","description":"read files","input_schema":{"type":"object"},"defer_loading":true}],"messages":[{"role":"user","content":"find a file"}]}`
	req, semantic := testClaudeSemantic(t, body, http.Header{
		"Anthropic-Beta": []string{"advanced-tool-use-2025-11-20"},
	})
	requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if requirements.AllowsKiro() || !requirements.RequiresToolSearch {
		t.Fatalf("concrete Tool Search was not kept fail-closed on Kiro: %#v", requirements)
	}
	want := claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke
	if requirements.Allowed != want {
		t.Fatalf("allowed=%08b want=%08b requirements=%s", requirements.Allowed, want, requirements.Summary())
	}
}

func TestClaudeBM25ToolSearchDoesNotClaimBedrockInvokeSupport(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":64,"tools":[{"type":"tool_search_tool_bm25_20251119","name":"tool_search_tool_bm25"},{"name":"workspace_read","description":"read files","input_schema":{"type":"object"},"defer_loading":true}],"messages":[{"role":"user","content":"find a file"}]}`
	req, semantic := testClaudeSemantic(t, body, nil)
	requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("BM25 Tool Search was advertised outside native Messages: %#v", requirements)
	}
}

func TestClaudeUnknownBetaRemainsFailClosed(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req, semantic := testClaudeSemantic(t, body, http.Header{
		"Anthropic-Beta": []string{"future-client-feature-2099-01-01"},
	})
	requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if requirements.Allowed != claudeTransportMaskCustom || requirements.AllowsKiro() {
		t.Fatalf("unknown beta was not fail-closed: %#v", requirements)
	}
}

func TestClaudeEagerInputStreamingUsesNormalizedConcreteFeature(t *testing.T) {
	streamBody := `{"model":"claude-opus-5","max_tokens":64,"stream":true,"tools":[{"name":"write_file","description":"write","input_schema":{"type":"object"},"eager_input_streaming":true}],"messages":[{"role":"user","content":"write"}]}`
	req, semantic := testClaudeSemantic(t, streamBody, nil)
	requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	want := claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke
	if requirements.Allowed != want || requirements.AllowsKiro() {
		t.Fatalf("streaming eager input requirements=%#v", requirements)
	}

	nonStreamBody := strings.Replace(streamBody, `"stream":true,`, `"stream":false,`, 1)
	req, semantic = testClaudeSemantic(t, nonStreamBody, nil)
	requirements, validationErr = analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !requirements.AllowsKiro() {
		t.Fatalf("non-stream request was restricted by an inactive streaming hint: %#v", requirements)
	}
}

func TestBedrockBodyDerivesConcreteToolFeatureBetas(t *testing.T) {
	raw := []byte(`{"model":"claude-opus-5","stream":true,"max_tokens":64,"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},{"name":"workspace_read","description":"read","input_schema":{"type":"object"},"defer_loading":true},{"name":"get_weather","description":"weather","input_schema":{"type":"object"},"input_examples":[{"location":"Tokyo"}]}],"messages":[{"role":"user","content":"hi"}]}`)
	body, err := buildBedrockBodyWithSemanticHeaders(raw, http.Header{
		"Anthropic-Beta": []string{"advanced-tool-use-2025-11-20"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	wantBetas := []interface{}{"tool-search-tool-2025-10-19", "tool-examples-2025-10-29"}
	if !reflect.DeepEqual(got["anthropic_beta"], wantBetas) {
		t.Fatalf("betas=%#v want=%#v body=%s", got["anthropic_beta"], wantBetas, body)
	}
	tools, _ := got["tools"].([]interface{})
	first, _ := tools[0].(map[string]interface{})
	if first["type"] != "tool_search_tool_regex" {
		t.Fatalf("Bedrock Tool Search type was not normalized: %#v", first)
	}
}

func TestBedrockBodyOmitsAdvancedMarkerWithoutConcreteAdvancedFeature(t *testing.T) {
	raw := []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	body, err := buildBedrockBodyWithSemanticHeaders(raw, http.Header{
		"Anthropic-Beta": []string{"advanced-tool-use-2025-11-20"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["anthropic_beta"]; exists {
		t.Fatalf("declaration-only client beta leaked to Bedrock: %s", body)
	}
}

func TestBedrockBodyRejectsUndocumentedBM25ToolSearchVariant(t *testing.T) {
	raw := []byte(`{"model":"claude-opus-5","max_tokens":64,"tools":[{"type":"tool_search_tool_bm25_20251119","name":"tool_search_tool_bm25"},{"name":"workspace_read","input_schema":{"type":"object"},"defer_loading":true}],"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := buildBedrockBodyWithSemanticHeaders(raw, nil); err == nil || !strings.Contains(err.Error(), "not documented for InvokeModel") {
		t.Fatalf("BM25 projection error=%v", err)
	}
}

func TestClaudeEagerInputStreamingFieldIsUserDefinedToolOnly(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		body := fmt.Sprintf(`{"model":"claude-opus-5","max_tokens":64,"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex","eager_input_streaming":%s}],"messages":[{"role":"user","content":"hi"}]}`, value)
		req, semantic := testClaudeSemantic(t, body, nil)
		_, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
		if validationErr == nil || !strings.Contains(validationErr.Error(), "eager_input_streaming is only valid on user-defined tools") {
			t.Fatalf("value=%s validationErr=%v", value, validationErr)
		}
	}
}

func TestClaudeServerToolRejectsInputExamplesEvenWhenEmpty(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":64,"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex","input_examples":[]}],"messages":[{"role":"user","content":"hi"}]}`
	req, semantic := testClaudeSemantic(t, body, nil)
	_, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr == nil || !strings.Contains(validationErr.Error(), "input_examples is not valid on server-side tools") {
		t.Fatalf("validationErr=%v", validationErr)
	}
}

func TestClaudeEmptyInputExamplesAreSemanticallyInactiveOnClientTool(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":64,"tools":[{"name":"workspace_read","description":"read files","input_schema":{"type":"object"},"input_examples":[]}],"messages":[{"role":"user","content":"hi"}]}`
	req, semantic := testClaudeSemantic(t, body, nil)
	requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, "claude-opus-5")
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !requirements.AllowsKiro() {
		t.Fatalf("empty input_examples unnecessarily excluded Kiro: %s", requirements.Summary())
	}
}
