package proxy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kiro-go/config"
)

func TestResponsesStateOwnershipConstrainsTransportFamily(t *testing.T) {
	req := &ResponsesRequest{}
	toolPlan := responsesToolChoicePlan{Kind: responsesToolChoiceAuto}
	semantics := responsesSemanticRequirements{}
	custom := &config.Account{AuthMethod: "custom_api"}
	kiro := &config.Account{AuthMethod: "idc"}

	if got, _ := responsesTransportForAccountWithOwnership(custom, req, toolPlan, responseStateLocal, semantics); got != responsesTransportUnsupported {
		t.Fatalf("local state must never be sent to custom relay, got transport=%v", got)
	}
	if got, _ := responsesTransportForAccountWithOwnership(custom, req, toolPlan, responseStateExternal, semantics); got != responsesTransportCustomRelay {
		t.Fatalf("external state must remain relay-eligible, got transport=%v", got)
	}
	if got, _ := responsesTransportForAccountWithOwnership(kiro, req, toolPlan, responseStateExternal, semantics); got != responsesTransportUnsupported {
		t.Fatalf("external state must never be interpreted as local Kiro state, got transport=%v", got)
	}
	if got, _ := responsesTransportForAccountWithOwnership(kiro, req, toolPlan, responseStateLocal, semantics); got != responsesTransportKiro {
		t.Fatalf("local state must remain Kiro-eligible, got transport=%v", got)
	}
}

func TestResponseStateUsesExactIdentity(t *testing.T) {
	namespace := strings.Repeat("a", responseInstanceNamespaceSize*2)
	got := generateResponseIDForNamespace(namespace)
	parsedNamespace, ok := responseIDInstanceNamespace(got)
	if !ok || parsedNamespace != namespace {
		t.Fatalf("new local response id must carry its instance namespace, got %q parsed=%q ok=%t", got, parsedNamespace, ok)
	}
	if sanitizeResponseID("resp_ab.cd") != sanitizeResponseID("resp_abcd") {
		t.Fatal("test precondition: legacy sanitizer should demonstrate the historical collision")
	}
	if responseStatePath("resp_ab.cd") == responseStatePath("resp_abcd") {
		t.Fatal("exact-ID hashed paths must not collide when legacy sanitized IDs do")
	}

	path := filepath.Join(t.TempDir(), "state.json")
	doc := storedResponseDoc{ID: "resp_exact", StoredAt: 1}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readStoredResponseDoc(path, "resp_other"); !errors.Is(err, errStoredResponseIdentityMismatch) {
		t.Fatalf("load must verify exact stored ID, got %v", err)
	}
}

func TestResponsesProjectionRejectsDeveloperAndEmulatesStructuredToolOutput(t *testing.T) {
	developer := []byte(`{"model":"x","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"policy"}]}]}`)
	if got := analyzeResponsesSemantics(developer); got.KiroSupported() || !strings.Contains(got.Reason(), "developer") {
		t.Fatalf("developer role must require passthrough, got %+v", got)
	}

	structured := []byte(`{"model":"x","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"ok"}]}]}`)
	got := analyzeResponsesSemantics(structured)
	if !got.KiroSupported() || !got.KiroEmulated() {
		t.Fatalf("structured text tool output should be explicit Kiro emulation, got %+v", got)
	}
	found := false
	for _, issue := range got.KiroProjection.Issues {
		if issue.Code == "structured_tool_output_items" && issue.Fidelity == openAIRequestProjectionEmulated {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("structured tool-output emulation was not recorded: %#v", got.KiroProjection.Issues)
	}
}

func TestResponsesProjectionRejectsRemoteImageAndDetail(t *testing.T) {
	remote := []byte(`{"model":"x","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.test/a.png"}]}]}`)
	if got := analyzeResponsesSemantics(remote); got.KiroSupported() {
		t.Fatalf("remote image must require transparent passthrough, got %+v", got)
	}

	detail := []byte(`{"model":"x","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo=","detail":"low"}]}]}`)
	if got := analyzeResponsesSemantics(detail); got.KiroSupported() || !strings.Contains(got.Reason(), "detail") {
		t.Fatalf("explicit image detail must not be silently discarded, got %+v", got)
	}
}

func TestKiroToolProjectionReportsBehaviorChangingMutation(t *testing.T) {
	additionalFalse := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}},"additionalProperties":false}}}]}`)
	projection := analyzeOpenAIChatKiroProjection(additionalFalse)
	if projection.Supported() || !strings.Contains(projection.Reason(), "additionalProperties") {
		t.Fatalf("additionalProperties:false removal must be reported, got %+v", projection)
	}

	blankDescription := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","description":"","parameters":{"type":"object","properties":{}}}}]}`)
	projection = analyzeOpenAIChatKiroProjection(blankDescription)
	if projection.Supported() || !strings.Contains(projection.Reason(), "description") {
		t.Fatalf("description synthesis must be reported, got %+v", projection)
	}
}

func TestOpenAISamplingPresencePreservesExplicitZeroForKiro(t *testing.T) {
	var req OpenAIRequest
	if err := json.Unmarshal([]byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"temperature":0,"top_p":0}`), &req); err != nil {
		t.Fatal(err)
	}
	payload := OpenAIToKiro(&req, false)
	if payload.InferenceConfig == nil {
		t.Fatal("explicit zero sampling controls must create inferenceConfig")
	}
	wire, err := json.Marshal(payload.InferenceConfig)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatal(err)
	}
	if value, ok := got["temperature"]; !ok || value.(float64) != 0 {
		t.Fatalf("temperature:0 missing from Kiro wire: %s", wire)
	}
	if value, ok := got["topP"]; !ok || value.(float64) != 0 {
		t.Fatalf("top_p:0 missing from Kiro wire: %s", wire)
	}
}

func TestBedrockProjectionNeverClampsOpenAISampling(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"temperature":1.7}`)
	projection := analyzeOpenAIChatBedrockProjection(body, true)
	if projection.Supported() || !strings.Contains(projection.Reason(), "[0,1]") {
		t.Fatalf("temperature 1.7 must make Bedrock ineligible, got %+v", projection)
	}
	if _, err := openAIToAnthropicMessages(body); err == nil || !strings.Contains(err.Error(), "semantic projection unsupported") {
		t.Fatalf("production converter must fail closed instead of clamping, got %v", err)
	}
}

func TestBedrockProjectionRejectsToolChoiceNoneAndDeveloperRole(t *testing.T) {
	none := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"x","description":"x","parameters":{"type":"object"}}}],"tool_choice":"none"}`)
	projection := analyzeOpenAIChatBedrockProjection(none, true)
	if projection.Supported() || !strings.Contains(projection.Reason(), "none") {
		t.Fatalf("tool_choice=none must not become Bedrock auto, got %+v", projection)
	}

	developer := []byte(`{"model":"x","messages":[{"role":"developer","content":"policy"},{"role":"user","content":"hi"}]}`)
	projection = analyzeOpenAIChatBedrockProjection(developer, true)
	if projection.Supported() || !strings.Contains(projection.Reason(), "developer") {
		t.Fatalf("developer precedence must not be hoisted silently, got %+v", projection)
	}
}

func TestKiroChatProjectionRejectsUnknownBehaviorField(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`)
	projection := analyzeOpenAIChatKiroProjection(body)
	if projection.Supported() || !strings.Contains(projection.Reason(), "response_format") {
		t.Fatalf("unsupported behavior field must require passthrough, got %+v", projection)
	}
}

func TestBedrockProjectionChecksResolvedModelSpecificToolChoiceCapability(t *testing.T) {
	body := []byte(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"x","description":"x","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"x"}}}`)
	if projection := analyzeOpenAIChatBedrockProjection(body, false); projection.Supported() || !strings.Contains(projection.Reason(), "forcing a specific tool") {
		t.Fatalf("unsupported resolved model must reject forced tool_choice, got %+v", projection)
	}
	if projection := analyzeOpenAIChatBedrockProjection(body, true); !projection.Exact() {
		t.Fatalf("supported resolved model should keep forced tool_choice eligible, got %+v", projection)
	}
}

func TestResponsesProjectionRequiresToolCorrelationIDs(t *testing.T) {
	for _, body := range []string{
		`{"model":"x","input":[{"type":"function_call","name":"lookup","arguments":"{}"}]}`,
		`{"model":"x","input":[{"type":"function_call_output","output":"ok"}]}`,
	} {
		if got := analyzeResponsesSemantics([]byte(body)); got.KiroSupported() || !strings.Contains(got.Reason(), "call_id") {
			t.Fatalf("tool item without call_id must require passthrough: body=%s got=%+v", body, got)
		}
	}
}

func TestKiroToolProjectionMutationMatrix(t *testing.T) {
	longDescription := strings.Repeat("x", maxToolDescLen+1)
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "nested additionalProperties false",
			body: `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"x","description":"x","parameters":{"type":"object","properties":{"nested":{"type":"object","additionalProperties":false}}}}}]}`,
			want: "additionalProperties",
		},
		{
			name: "missing root type",
			body: `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"x","description":"x","parameters":{"properties":{"q":{"type":"string"}}}}}]}`,
			want: "type",
		},
		{
			name: "overlong description",
			body: `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"x","description":"` + longDescription + `","parameters":{"type":"object"}}}]}`,
			want: "description",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projection := analyzeOpenAIChatKiroProjection([]byte(tc.body))
			if projection.Supported() || !strings.Contains(projection.Reason(), tc.want) {
				t.Fatalf("behavior-changing tool mutation was not reported: %+v", projection)
			}
		})
	}
}

func TestOpenAISamplingPresenceDistinguishesOmittedZeroAndNonZero(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantConfig bool
		wantTemp   *float64
		wantTopP   *float64
	}{
		{name: "omitted", body: `{"model":"x","messages":[{"role":"user","content":"hi"}]}`, wantConfig: false},
		{name: "explicit zero", body: `{"model":"x","messages":[{"role":"user","content":"hi"}],"temperature":0,"top_p":0}`, wantConfig: true, wantTemp: projectionFloat64Ptr(0), wantTopP: projectionFloat64Ptr(0)},
		{name: "nonzero", body: `{"model":"x","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"top_p":0.8}`, wantConfig: true, wantTemp: projectionFloat64Ptr(0.7), wantTopP: projectionFloat64Ptr(0.8)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req OpenAIRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatal(err)
			}
			payload := OpenAIToKiro(&req, false)
			if (payload.InferenceConfig != nil) != tc.wantConfig {
				t.Fatalf("inference config presence=%v want %v", payload.InferenceConfig != nil, tc.wantConfig)
			}
			if !tc.wantConfig {
				return
			}
			wire, err := json.Marshal(payload.InferenceConfig)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]float64
			if err := json.Unmarshal(wire, &got); err != nil {
				t.Fatal(err)
			}
			if tc.wantTemp != nil && got["temperature"] != *tc.wantTemp {
				t.Fatalf("temperature=%v want %v wire=%s", got["temperature"], *tc.wantTemp, wire)
			}
			if tc.wantTopP != nil && got["topP"] != *tc.wantTopP {
				t.Fatalf("topP=%v want %v wire=%s", got["topP"], *tc.wantTopP, wire)
			}
		})
	}
}

func projectionFloat64Ptr(v float64) *float64 { return &v }

func TestBedrockProjectionMatchesActualContentConverter(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"input_text","text":"would be dropped by Bedrock converter"}]}]}`)
	projection := analyzeOpenAIChatBedrockProjection(body, true)
	if projection.Supported() || !strings.Contains(projection.Reason(), "input_text") {
		t.Fatalf("projection must reject content types the production converter drops: %+v", projection)
	}
}

func TestBedrockProjectionRejectsConflictingTokenLimits(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"max_completion_tokens":200}`)
	projection := analyzeOpenAIChatBedrockProjection(body, true)
	if projection.Supported() || !strings.Contains(projection.Reason(), "max_completion_tokens") {
		t.Fatalf("adapter must not silently prefer one OpenAI token limit: %+v", projection)
	}
}

func TestTranslatedChatProjectionRejectsLateSystemMessages(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"first"},{"role":"system","content":"late policy"}]}`)
	for name, projection := range map[string]openAIProjection{
		"kiro":    analyzeOpenAIChatKiroProjection(body),
		"bedrock": analyzeOpenAIChatBedrockProjection(body, true),
	} {
		if projection.Supported() || !strings.Contains(projection.Reason(), "system") {
			t.Fatalf("%s projection must reject system-message reordering: %+v", name, projection)
		}
	}

	prefix := []byte(`{"model":"x","messages":[{"role":"system","content":"policy"},{"role":"user","content":"first"}]}`)
	if projection := analyzeOpenAIChatKiroProjection(prefix); !projection.Supported() || !projection.Emulated() {
		t.Fatalf("initial system prefix should be an explicit Kiro emulation: %+v", projection)
	}
}

func TestResponsesProjectionRejectsLateSystemMessages(t *testing.T) {
	body := []byte(`{"model":"x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},{"type":"message","role":"system","content":[{"type":"input_text","text":"late policy"}]}]}`)
	got := analyzeResponsesSemantics(body)
	if got.KiroSupported() || !strings.Contains(got.Reason(), "system") {
		t.Fatalf("Responses projection must reject system-message reordering: %+v", got)
	}
}

func TestKiroProjectionClassifiesHistoricalToolStateAsEmulated(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"call it"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"},{"role":"user","content":"summarize"}]}`)
	projection := analyzeOpenAIChatKiroProjection(body)
	if !projection.Supported() || !projection.Emulated() || !strings.Contains(projection.Reason(), "historical tool") {
		t.Fatalf("completed historical tool state must be explicit Kiro emulation: %+v", projection)
	}
}

func TestTranslatedProjectionRejectsBlankToolArguments(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"call"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":""}}]},{"role":"tool","tool_call_id":"c1","content":"ok"}]}`)
	for name, projection := range map[string]openAIProjection{
		"kiro":    analyzeOpenAIChatKiroProjection(body),
		"bedrock": analyzeOpenAIChatBedrockProjection(body, true),
	} {
		if projection.Supported() || !strings.Contains(projection.Reason(), "blank") {
			t.Fatalf("%s projection silently repaired blank tool arguments: %+v", name, projection)
		}
	}
}

func TestChatProjectionValidatesToolCorrelation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown result id",
			body: `{"messages":[{"role":"tool","tool_call_id":"missing","content":"x"}]}`,
			want: "unknown",
		},
		{
			name: "duplicate call id",
			body: `{"messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}},{"id":"c1","type":"function","function":{"name":"b","arguments":"{}"}}]}]}`,
			want: "repeated",
		},
		{
			name: "duplicate result",
			body: `{"messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"one"},{"role":"tool","tool_call_id":"c1","content":"two"}]}`,
			want: "more than one result",
		},
		{
			name: "partial parallel results",
			body: `{"messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"b","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"one"},{"role":"user","content":"continue"}]}`,
			want: "before all pending",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projection := analyzeOpenAIChatKiroProjection([]byte(tc.body))
			if projection.Supported() || !strings.Contains(projection.Reason(), tc.want) {
				t.Fatalf("correlation violation was not rejected: %+v", projection)
			}
		})
	}
}

func TestResponsesInstructionsRemainCurrentPrivilegedEmulation(t *testing.T) {
	body := []byte(`{"model":"x","previous_response_id":"resp_external","instructions":"current policy","input":"hello"}`)
	got := analyzeResponsesSemantics(body)
	if !got.KiroSupported() || !got.KiroEmulated() || !strings.Contains(got.Reason(), "privileged") {
		t.Fatalf("top-level current instructions must be modeled independently from late history system messages: %+v", got)
	}
}

func TestResponsesTruncationAutoRequiresTransparentRelay(t *testing.T) {
	auto := analyzeResponsesSemantics([]byte(`{"model":"x","input":"hello","truncation":"auto"}`))
	if auto.KiroSupported() || !strings.Contains(auto.Reason(), "transparent passthrough") {
		t.Fatalf("truncation=auto must remain relay-only in this phase: %+v", auto)
	}
	disabled := analyzeResponsesSemantics([]byte(`{"model":"x","input":"hello","truncation":"disabled"}`))
	if !disabled.KiroExact() {
		t.Fatalf("truncation=disabled should remain request-exact until payload budget is measured: %+v", disabled)
	}
}

func TestResponsesRawProjectionDefersCrossTurnToolCorrelation(t *testing.T) {
	// A function_call_output may legitimately answer a call stored behind
	// previous_response_id. The raw Responses gate has not expanded that local
	// state yet, so it must validate shape only and defer correlation to the
	// canonical Chat projection built by prepareResponsesKiroRequest.
	leadingOutput := analyzeResponsesSemantics([]byte(`{"model":"x","previous_response_id":"resp_previous","input":[{"type":"function_call_output","call_id":"c1","output":"ok"}]}`))
	if !leadingOutput.KiroSupported() {
		t.Fatalf("raw Responses gate rejected a result that may correlate through previous_response_id: %+v", leadingOutput)
	}

	partialCurrentInput := analyzeResponsesSemantics([]byte(`{"model":"x","input":[{"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},{"type":"function_call","call_id":"c2","name":"b","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"one"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`))
	if !partialCurrentInput.KiroSupported() {
		t.Fatalf("raw gate should leave turn-level correlation to canonical Kiro preparation: %+v", partialCurrentInput)
	}
}

func TestResponseInstanceNamespaceCreationIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), responseInstanceNamespaceFile)
	const workers = 32
	values := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := responseInstanceNamespaceAt(path)
			if err != nil {
				errs <- err
				return
			}
			values <- value
		}()
	}
	wg.Wait()
	close(values)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent namespace creation failed: %v", err)
	}
	var winner string
	for value := range values {
		if winner == "" {
			winner = value
		}
		if value != winner {
			t.Fatalf("concurrent creators observed different namespaces: %q vs %q", winner, value)
		}
	}
	persisted, err := readResponseInstanceNamespace(path)
	if err != nil || persisted != winner {
		t.Fatalf("persisted namespace=%q err=%v winner=%q", persisted, err, winner)
	}
}

func TestResponseInstanceNamespaceCorruptionPreservesMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), responseInstanceNamespaceFile)
	if err := os.WriteFile(path, []byte("corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := responseInstanceNamespaceAt(path); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt namespace metadata must not be silently regenerated: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "corrupt\n" {
		t.Fatalf("corrupt metadata was unexpectedly replaced: data=%q err=%v", data, err)
	}
}

func TestResponseStateIDClassificationIsInstanceScoped(t *testing.T) {
	local := strings.Repeat("1", responseInstanceNamespaceSize*2)
	foreign := strings.Repeat("2", responseInstanceNamespaceSize*2)
	ownID := generateResponseIDForNamespace(local)
	foreignID := generateResponseIDForNamespace(foreign)

	if got := classifyResponseID(ownID); got != responseIDNamespacedLocal {
		t.Fatalf("own namespaced id class=%v", got)
	}
	if got := classifyResponseID(foreignID); got != responseIDNamespacedLocal {
		t.Fatalf("foreign namespaced id class=%v", got)
	}
	if namespace, ok := responseIDInstanceNamespace(ownID); !ok || namespace != local {
		t.Fatalf("own namespace=%q ok=%t want=%q", namespace, ok, local)
	}
	if namespace, ok := responseIDInstanceNamespace(foreignID); !ok || namespace != foreign {
		t.Fatalf("foreign namespace=%q ok=%t want=%q", namespace, ok, foreign)
	}
	if got := classifyResponseID(localResponseIDPrefix + "legacy123"); got != responseIDLegacyAmbiguous {
		t.Fatalf("legacy prefix-only id class=%v", got)
	}
}

func TestLegacyResponseLookupRejectsUnboundedExternalFilename(t *testing.T) {
	if path := legacyResponseStatePath(strings.Repeat("external", 100)); path != "" {
		t.Fatalf("oversized external response id should not reach legacy filesystem lookup: %q", path)
	}
}

func TestKiroProjectionClassifiesPlainUserWhitespaceAsEmulated(t *testing.T) {
	projection := analyzeOpenAIChatKiroProjection([]byte(`{
		"model":"x",
		"messages":[{"role":"user","content":"  hello  "}]
	}`))
	if !projection.Supported() || !projection.Emulated() {
		t.Fatalf("plain user whitespace should be supported only as emulation: %+v", projection)
	}
}

func TestKiroProjectionRejectsBlankCurrentUserContent(t *testing.T) {
	projection := analyzeOpenAIChatKiroProjection([]byte(`{
		"model":"x",
		"messages":[{"role":"user","content":"   "}]
	}`))
	if projection.Supported() {
		t.Fatalf("blank user content would be replaced by Kiro fallback text: %+v", projection)
	}
}

func TestKiroProjectionRejectsUnknownNestedReasoningFields(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "reasoning", body: `{"model":"x","messages":[{"role":"user","content":"hi"}],"reasoning":{"effort":"high","summary":"auto"}}`, want: "reasoning.summary"},
		{name: "thinking", body: `{"model":"x","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","vendor_option":true}}`, want: "thinking.vendor_option"},
		{name: "output config", body: `{"model":"x","messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high","verbosity":"low"}}`, want: "output_config.verbosity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projection := analyzeOpenAIChatKiroProjection([]byte(tc.body))
			if projection.Supported() || !strings.Contains(projection.Reason(), tc.want) {
				t.Fatalf("nested behavior field would be lost: %+v", projection)
			}
		})
	}
}

func TestKiroProjectionRejectsConflictingReasoningEffortAliases(t *testing.T) {
	projection := analyzeOpenAIChatKiroProjection([]byte(`{
		"model":"x",
		"messages":[{"role":"user","content":"hi"}],
		"output_config":{"effort":"high"},
		"reasoning":{"effort":"low"}
	}`))
	if projection.Supported() || !strings.Contains(projection.Reason(), "conflicting") {
		t.Fatalf("builder precedence must not silently discard conflicting effort: %+v", projection)
	}
}

func TestKiroProjectionRejectsNonPositiveMaxTokens(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		projection := analyzeOpenAIChatKiroProjection([]byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"max_tokens":` + value + `}`))
		if projection.Supported() || !strings.Contains(projection.Reason(), "max_tokens") {
			t.Fatalf("max_tokens=%s would be silently omitted by Kiro: %+v", value, projection)
		}
	}
}

func TestKiroReasoningCapabilityProjectionRejectsDroppedBudgetAndDisplay(t *testing.T) {
	cases := []struct {
		name string
		req  OpenAIRequest
		cap  ModelReasoningCapability
		want string
	}{
		{
			name: "budget unsupported by model",
			req:  OpenAIRequest{Thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 100}},
			cap:  ModelReasoningCapability{SupportsBudgetTokens: false},
			want: "budget_tokens",
		},
		{
			name: "budget alone ignored",
			req:  OpenAIRequest{Thinking: &ClaudeThinkingConfig{BudgetTokens: 100}},
			cap:  ModelReasoningCapability{SupportsBudgetTokens: true},
			want: "budget_tokens",
		},
		{
			name: "display while disabled ignored",
			req:  OpenAIRequest{Thinking: &ClaudeThinkingConfig{Type: "disabled", Display: "summarized"}},
			cap:  ModelReasoningCapability{},
			want: "display",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var projection openAIProjection
			analyzeOpenAIKiroReasoningCapabilityProjection(&projection, &tc.req, tc.cap)
			if projection.Supported() || !strings.Contains(projection.Reason(), tc.want) {
				t.Fatalf("capability-specific reasoning mutation was not rejected: %+v", projection)
			}
		})
	}
}
