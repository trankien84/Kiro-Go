package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPhase64ResponsesLiteMergesTopLevelAndAdditionalTools(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"tools":[{"type":"function","name":"lookup","description":"top","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}],
		"input":[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"namespace","name":"functions","description":"","tools":[
					{"type":"custom","name":"exec","description":"run a patch","format":{"type":"text"}},
					{"type":"function","name":"wait","description":"wait","parameters":{"type":"object"}}
				]}
			]},
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"function","name":"request_user_input","description":"ask","parameters":{"type":"object"}}
			]},
			{"role":"user","content":[{"type":"input_text","text":"hello"}]}
		],
		"parallel_tool_calls":false
	}`)

	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.ToolDeclarations) != 4 || len(req.Tools) != 4 {
		t.Fatalf("unified catalog declarations=%d tools=%d", len(req.ToolDeclarations), len(req.Tools))
	}

	byIdentity := map[string]OpenAITool{}
	for _, tool := range req.Tools {
		id := responsesToolIdentity{Namespace: tool.ResponsesNamespace, Name: tool.Function.Name}.key()
		byIdentity[id] = tool
	}
	if _, ok := byIdentity[(responsesToolIdentity{Name: "lookup"}).key()]; !ok {
		t.Fatal("top-level tool missing from unified catalog")
	}
	exec, ok := byIdentity[(responsesToolIdentity{Namespace: "functions", Name: "exec"}).key()]
	if !ok || exec.ResponsesKind != responsesToolKindCustom {
		t.Fatalf("namespaced custom exec=%#v", exec)
	}
	schema, ok := exec.Function.Parameters.(map[string]interface{})
	if !ok {
		t.Fatalf("custom schema type=%T", exec.Function.Parameters)
	}
	props, _ := schema["properties"].(map[string]interface{})
	input, _ := props["input"].(map[string]interface{})
	if schema["type"] != "object" || input["type"] != "string" {
		t.Fatalf("custom tool did not use fixed {input:string} schema: %#v", schema)
	}
	if len(req.RawTools) == 0 || !strings.Contains(string(req.RawTools), `"lookup"`) {
		t.Fatalf("top-level raw tools were not preserved: %s", req.RawTools)
	}
}

func TestPhase64UnifiedToolCatalogDedupesExactAndRejectsConflict(t *testing.T) {
	exact := []byte(`{
		"tools":[{"type":"function","name":"lookup","description":"same","parameters":{"type":"object"}}],
		"input":[{"type":"additional_tools","tools":[{"type":"function","name":"lookup","description":"same","parameters":{"type":"object"}}]}]
	}`)
	declarations, _, _, err := parseResponsesUnifiedTools(exact)
	if err != nil {
		t.Fatal(err)
	}
	if len(declarations) != 1 {
		t.Fatalf("exact duplicate count=%d want=1", len(declarations))
	}

	conflict := []byte(`{
		"tools":[{"type":"function","name":"lookup","description":"one","parameters":{"type":"object"}}],
		"input":[{"type":"additional_tools","tools":[{"type":"function","name":"lookup","description":"two","parameters":{"type":"object"}}]}]
	}`)
	if _, _, _, err := parseResponsesUnifiedTools(conflict); err == nil || !strings.Contains(err.Error(), "conflicting tool declarations") {
		t.Fatalf("semantic conflict error=%v", err)
	}

	kindConflict := []byte(`{
		"tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}],
		"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"exec"}]}]
	}`)
	if _, _, _, err := parseResponsesUnifiedTools(kindConflict); err == nil {
		t.Fatal("same {namespace,name} with function/custom semantics was accepted")
	}
}

func TestPhase64NamespaceDescriptionIsFlattenedIntoChildTools(t *testing.T) {
	body := []byte(`{
		"input":[{"type":"additional_tools","tools":[
			{"type":"namespace","name":"functions","description":"Built-in function tools","tools":[
				{"type":"function","name":"lookup","parameters":{"type":"object"}},
				{"type":"custom","name":"exec"}
			]}
		]}]
	}`)
	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("tools=%d want=2", len(req.Tools))
	}
	for _, tool := range req.Tools {
		if tool.ResponsesNamespace != "functions" {
			t.Fatalf("namespace=%q want=functions", tool.ResponsesNamespace)
		}
		if tool.Function.Description != "Built-in function tools" {
			t.Fatalf("%s description=%q", tool.Function.Name, tool.Function.Description)
		}
	}

	semantics := analyzeResponsesSemantics(body)
	if !semantics.KiroSupported() {
		t.Fatalf("namespace description made request non-Kiro-compatible: %s", semantics.Reason())
	}
	foundFlattening := false
	for _, issue := range semantics.KiroProjection.Issues {
		if issue.Fidelity == openAIRequestProjectionEmulated && issue.Code == "namespace_description_flattening" {
			foundFlattening = true
			break
		}
	}
	if !foundFlattening {
		t.Fatalf("namespace description flattening was not recorded: %#v", semantics.KiroProjection.Issues)
	}
}

func TestPhase64NamespaceDescriptionParticipatesInDuplicateSemantics(t *testing.T) {
	body := []byte(`{
		"input":[
			{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","description":"first","tools":[{"type":"custom","name":"exec"}]}]},
			{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","description":"second","tools":[{"type":"custom","name":"exec"}]}]}
		]
	}`)
	if _, _, _, err := parseResponsesUnifiedTools(body); err == nil || !strings.Contains(err.Error(), "conflicting tool declarations") {
		t.Fatalf("namespace-description conflict error=%v", err)
	}
}

func TestPhase64NamespaceIdentityAvoidsBareNameCollision(t *testing.T) {
	body := []byte(`{
		"input":[{"type":"additional_tools","tools":[
			{"type":"namespace","name":"alpha","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]},
			{"type":"namespace","name":"beta","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]}
		]}]
	}`)
	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	catalog := buildOpenAIToolNameCatalog(req.Tools)
	alpha := catalog.kiroNameFor("alpha", "exec")
	beta := catalog.kiroNameFor("beta", "exec")
	if alpha == "" || beta == "" || alpha == beta {
		t.Fatalf("namespace collision alpha=%q beta=%q", alpha, beta)
	}
	if got := catalog.KiroToIdentity[alpha]; got.Namespace != "alpha" || got.Name != "exec" {
		t.Fatalf("alpha reverse identity=%#v", got)
	}
	if got := catalog.KiroToIdentity[beta]; got.Namespace != "beta" || got.Name != "exec" {
		t.Fatalf("beta reverse identity=%#v", got)
	}
}

func TestPhase64CustomToolFormatIsExplicitlyClassifiedAsEmulation(t *testing.T) {
	body := []byte(`{
		"input":[{"type":"additional_tools","tools":[{
			"type":"custom","name":"exec","description":"execute code",
			"format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}
		}]}]
	}`)
	semantics := analyzeResponsesSemantics(body)
	if !semantics.KiroSupported() || !semantics.KiroEmulated() {
		t.Fatalf("custom freeform format should be explicit Kiro emulation, supported=%v exact=%v reason=%q issues=%#v", semantics.KiroSupported(), semantics.KiroExact(), semantics.Reason(), semantics.KiroProjection.Issues)
	}
	found := false
	for _, issue := range semantics.KiroProjection.Issues {
		if issue.Code == "custom_tool_format_emulation" && issue.Fidelity == openAIRequestProjectionEmulated {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("custom format emulation was not recorded: %#v", semantics.KiroProjection.Issues)
	}
}

func TestPhase64NamespaceAwareToolChoiceRejectsBareAmbiguity(t *testing.T) {
	body := []byte(`{"input":[{"type":"additional_tools","tools":[
		{"type":"namespace","name":"alpha","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]},
		{"type":"namespace","name":"beta","tools":[{"type":"custom","name":"exec"}]}
	]}]}`)
	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if _, err := parseResponsesToolChoice(json.RawMessage(`{"type":"tool","name":"exec"}`), req.Tools); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("bare collision error=%v", err)
	}
	plan, err := parseResponsesToolChoice(json.RawMessage(`{"type":"custom","name":"exec","namespace":"beta"}`), req.Tools)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != responsesToolChoiceSpecific || plan.Namespace != "beta" || plan.ToolName != "exec" || plan.ToolType != responsesToolKindCustom {
		t.Fatalf("explicit namespace plan=%#v", plan)
	}
}

func TestPhase64CustomToolCallAndOutputRoundTripShape(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"exec","namespace":"functions","input":"*** Begin Patch"},
		{"type":"custom_tool_call_output","call_id":"call_1","output":"Done!"}
	]`)
	messages, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || len(messages[0].ToolCalls) != 1 {
		t.Fatalf("messages=%#v", messages)
	}
	call := messages[0].ToolCalls[0]
	if call.Namespace != "functions" || call.ResponsesKind != responsesToolKindCustom || call.Function.Name != "exec" {
		t.Fatalf("custom call metadata=%#v", call)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args["input"] != "*** Begin Patch" {
		t.Fatalf("custom call internal arguments=%q err=%v", call.Function.Arguments, err)
	}
	if messages[1].Role != "tool" || messages[1].ToolCallID != "call_1" || messages[1].Content != "Done!" {
		t.Fatalf("custom output=%#v", messages[1])
	}

	item := responseToolOutputItem("ctc_fixed", KiroToolUse{
		ToolUseID: "call_1", Name: "exec", Namespace: "functions", ToolType: responsesToolKindCustom,
		Input: map[string]interface{}{"input": "*** Begin Patch"},
	})
	if item.Type != "custom_tool_call" || item.Namespace != "functions" || item.Input != "*** Begin Patch" || item.Arguments != "" {
		t.Fatalf("custom output item=%#v", item)
	}
}

func TestPhase64ResponsesToolOutputKeepsInlineImagesForKiroProjection(t *testing.T) {
	const image = "data:image/png;base64,aGVsbG8="
	for _, tc := range []struct {
		name       string
		callType   string
		outputType string
	}{
		{name: "function", callType: "function_call", outputType: "function_call_output"},
		{name: "custom", callType: "custom_tool_call", outputType: "custom_tool_call_output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := `{"type":"` + tc.callType + `","call_id":"call_1","name":"capture"`
			if tc.callType == "custom_tool_call" {
				call += `,"input":"capture"}`
			} else {
				call += `,"arguments":"{}"}`
			}
			rawText := `[` + call + `,{"type":"` + tc.outputType + `","call_id":"call_1","output":[` +
				`{"type":"input_text","text":"screenshot"},` +
				`{"type":"input_image","image_url":"` + image + `"}` +
				`]}]`
			raw := json.RawMessage(rawText)
			messages, err := parseResponsesInput(raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 2 || messages[1].Role != "tool" || messages[1].ToolCallID != "call_1" {
				t.Fatalf("Responses tool output projection=%#v", messages)
			}
			parts, ok := messages[1].Content.([]interface{})
			if !ok || len(parts) != 2 {
				t.Fatalf("structured tool output was flattened before Kiro projection: %#v", messages[1].Content)
			}

			semantics := analyzeResponsesSemantics([]byte(`{"input":` + rawText + `}`))
			if !semantics.KiroSupported() || !semantics.KiroEmulated() {
				t.Fatalf("multimodal tool output semantics=%#v", semantics.KiroProjection.Issues)
			}

			payload := OpenAIToKiro(&OpenAIRequest{
				Model:    "claude-opus-5",
				Messages: append([]OpenAIMessage{{Role: "user", Content: "capture"}}, messages...),
			}, false)
			cur := payload.ConversationState.CurrentMessage.UserInputMessage
			if len(cur.Images) != 1 || cur.Images[0].Format != "png" || cur.Images[0].Source.Bytes != "aGVsbG8=" {
				t.Fatalf("Responses tool image projection=%#v", cur.Images)
			}
			if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
				t.Fatalf("Responses structured tool result missing: %#v", cur.UserInputMessageContext)
			}
			result := cur.UserInputMessageContext.ToolResults[0]
			if result.ToolUseID != "call_1" || result.Content[0].Text != "screenshot" {
				t.Fatalf("Responses tool result identity/text=%#v", result)
			}
		})
	}
}

func TestPhase64ResponsesToolOutputUnknownNonMediaItemsAreNotSilentlyDropped(t *testing.T) {
	raw := json.RawMessage(`[{"type":"function_call_output","call_id":"call_1","output":[{"type":"future_metadata","value":"keep-me"}]}]`)
	messages, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages=%#v", messages)
	}
	text := extractOpenAIMessageText(messages[0].Content)
	if !strings.Contains(text, "future_metadata") || !strings.Contains(text, "keep-me") {
		t.Fatalf("unknown tool output item was dropped: %q", text)
	}
	semantics := analyzeResponsesSemantics([]byte(`{"input":` + string(raw) + `}`))
	if !semantics.KiroSupported() || !semantics.KiroEmulated() {
		t.Fatalf("unknown non-media output should be text-emulated: %#v", semantics.KiroProjection.Issues)
	}
}

func TestPhase64ResponsesUnsupportedToolOutputMediaFailsClosedForKiro(t *testing.T) {
	for _, content := range []string{
		`{"type":"input_audio","audio_url":"data:audio/wav;base64,AA=="}`,
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}`,
		`{"type":"input_image","image_url":"https://example.com/a.png"}`,
	} {
		raw := []byte(`{"input":[{"type":"function_call_output","call_id":"call_1","output":[` + content + `]}]}`)
		semantics := analyzeResponsesSemantics(raw)
		if semantics.KiroSupported() {
			t.Fatalf("unsupported/remote tool-output media was accepted: content=%s issues=%#v", content, semantics.KiroProjection.Issues)
		}
	}
}

func TestPhase64ResponsesOpaqueEncryptedToolOutputFailsClosedForKiro(t *testing.T) {
	raw := json.RawMessage(`[{"type":"function_call_output","call_id":"call_1","output":[{"type":"encrypted_content","encrypted_content":"ocx1:abcdef"}]}]`)
	if _, err := parseResponsesInput(raw); err == nil || !strings.Contains(err.Error(), "opaque") {
		t.Fatalf("opaque tool output error=%v", err)
	}
	semantics := analyzeResponsesSemantics([]byte(`{"input":` + string(raw) + `}`))
	if semantics.KiroSupported() || !strings.Contains(semantics.Reason(), "opaque") {
		t.Fatalf("opaque tool-output semantics=%#v", semantics.KiroProjection.Issues)
	}
}

func TestPhase64KiroToolIdentityRestoresNamespaceAndKind(t *testing.T) {
	var custom OpenAITool
	custom.Type = "function"
	custom.Function.Name = "exec"
	custom.Function.Parameters = map[string]interface{}{"type": "object"}
	custom.ResponsesNamespace = "functions"
	custom.ResponsesKind = responsesToolKindCustom

	var fn OpenAITool
	fn.Type = "function"
	fn.Function.Name = "exec"
	fn.Function.Parameters = map[string]interface{}{"type": "object"}
	fn.ResponsesNamespace = "shell"
	fn.ResponsesKind = responsesToolKindFunction

	payload := OpenAIToKiro(&OpenAIRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []OpenAIMessage{{Role: "user", Content: "go"}},
		Tools:    []OpenAITool{custom, fn},
	}, false)
	if len(payload.ToolIdentityMap) != 2 {
		t.Fatalf("tool identity map=%#v", payload.ToolIdentityMap)
	}
	for wire, identity := range payload.ToolIdentityMap {
		if wire == identity.Name && identity.Namespace != "" {
			t.Fatalf("namespaced identity was not flattened internally: wire=%q identity=%#v", wire, identity)
		}
		if identity.Name != "exec" || (identity.Namespace != "functions" && identity.Namespace != "shell") {
			t.Fatalf("unexpected identity=%#v", identity)
		}
	}
}

func TestPhase64EncryptedSchemaKeywordIsRemovedRecursivelyWithoutMutatingSource(t *testing.T) {
	source := map[string]interface{}{
		"type":      "object",
		"encrypted": true,
		"properties": map[string]interface{}{
			"nested": map[string]interface{}{
				"type":      "object",
				"encrypted": map[string]interface{}{"provider": "client"},
			},
		},
	}
	cleaned := ensureObjectSchema(source).(map[string]interface{})
	if _, ok := cleaned["encrypted"]; ok {
		t.Fatal("root encrypted keyword survived")
	}
	props := cleaned["properties"].(map[string]interface{})
	nested := props["nested"].(map[string]interface{})
	if _, ok := nested["encrypted"]; ok {
		t.Fatal("nested encrypted keyword survived")
	}
	if _, ok := source["encrypted"]; !ok {
		t.Fatal("schema sanitizer mutated caller-owned source map")
	}
	originalNested := source["properties"].(map[string]interface{})["nested"].(map[string]interface{})
	if _, ok := originalNested["encrypted"]; !ok {
		t.Fatal("schema sanitizer mutated caller-owned nested map")
	}
}

func TestPhase64AgentMessagePlaintextAndOpaqueEncryptedContent(t *testing.T) {
	plain := json.RawMessage(`[{
		"type":"agent_message","author":"agent-a","recipient":"agent-b","content":[
			{"type":"input_text","text":"Header"},
			{"type":"encrypted_content","encrypted_content":"plain delegated task"}
		]
	}]`)
	messages, err := parseResponsesInput(plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("agent message projection=%#v", messages)
	}
	text := extractOpenAIMessageText(messages[0].Content)
	if !strings.Contains(text, "Header") || !strings.Contains(text, "plain delegated task") {
		t.Fatalf("agent plaintext lost: %q", text)
	}

	opaque := json.RawMessage(`[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"ocx1:abcdef"}]}]`)
	if _, err := parseResponsesInput(opaque); err == nil || !strings.Contains(err.Error(), "opaque") {
		t.Fatalf("opaque encrypted content error=%v", err)
	}
}

func TestPhase64ResponsesSemanticClassifierUnderstandsNewClientShapes(t *testing.T) {
	plain := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[
			{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","description":"","tools":[{"type":"custom","name":"exec","description":"execute text"}]}]},
			{"type":"agent_message","author":"a","recipient":"b","content":[{"type":"input_text","text":"task"}]},
			{"type":"custom_tool_call","call_id":"c1","name":"exec","namespace":"functions","input":"patch"},
			{"type":"custom_tool_call_output","call_id":"c1","output":"ok"}
		]
	}`)
	sem := analyzeResponsesSemantics(plain)
	if !sem.KiroSupported() || !sem.KiroEmulated() {
		t.Fatalf("new Responses shapes projection fidelity=%v issues=%#v", sem.KiroProjection.Fidelity, sem.KiroProjection.Issues)
	}

	opaque := []byte(`{"input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"gAAAAABxyz"}]}]}`)
	sem = analyzeResponsesSemantics(opaque)
	if sem.KiroSupported() || !strings.Contains(sem.Reason(), "opaque") {
		t.Fatalf("opaque agent_message projection fidelity=%v issues=%#v", sem.KiroProjection.Fidelity, sem.KiroProjection.Issues)
	}
}

func TestPhase64CustomToolVisibilityConstraintsFailClosedForKiro(t *testing.T) {
	deferred := analyzeResponsesSemantics([]byte(`{
		"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"exec","defer_loading":true}]}]
	}`))
	if deferred.KiroSupported() || !strings.Contains(deferred.Reason(), "defer_loading") {
		t.Fatalf("deferred custom tool projection=%#v", deferred.KiroProjection.Issues)
	}
	callers := analyzeResponsesSemantics([]byte(`{
		"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"exec","allowed_callers":["code_mode"]}]}]
	}`))
	if callers.KiroSupported() || !strings.Contains(callers.Reason(), "allowed_callers") {
		t.Fatalf("allowed_callers custom tool projection=%#v", callers.KiroProjection.Issues)
	}
}

func TestPhase64StoredStateV2PersistsUnifiedToolCatalogAndInheritsIt(t *testing.T) {
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-5.6-sol",
		"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec","description":"execute text"}]}]},{"role":"user","content":"go"}],
		"parallel_tool_calls":false
	}`), &req); err != nil {
		t.Fatal(err)
	}
	resp := buildResponsesObjectWithOutput("resp_v2", "gpt-5.6-sol", []ResponseOutputItem{{
		ID: "ctc_1", Type: "custom_tool_call", Status: "completed", CallID: "call_1", Name: "exec", Namespace: "functions", Input: "patch",
	}}, 1, 1, &req)
	resp.StoredInput = json.RawMessage(`"go"`)
	resp.StoredAt = time.Now().Unix()
	doc := storedResponseDocument(resp)
	if doc.StateVersion != 2 || len(doc.ToolCatalog) == 0 {
		t.Fatalf("stored state version/catalog=%d/%s", doc.StateVersion, doc.ToolCatalog)
	}
	if err := validateStoredResponseDoc(&doc, doc.ID); err != nil {
		t.Fatalf("v2 document rejected: %v", err)
	}
	loaded := responseObjectFromStoredDoc(&doc)
	follow := ResponsesRequest{PreviousResponseID: doc.ID}
	if err := responsesInheritStoredTools(&follow, loaded); err != nil {
		t.Fatal(err)
	}
	if len(follow.Tools) != 1 || follow.Tools[0].ResponsesNamespace != "functions" || follow.Tools[0].ResponsesKind != responsesToolKindCustom {
		t.Fatalf("inherited unified catalog=%#v", follow.Tools)
	}
}

func TestPhase64StoredStateV1FunctionOnlyCompatibility(t *testing.T) {
	now := time.Now().Unix()
	doc := storedResponseDoc{
		StateVersion: 1,
		ID:           "resp_v1_compat", Object: "response", CreatedAt: now, Status: "completed", Model: "old", StoredAt: now,
		StoredInput: json.RawMessage(`"hello"`),
		Tools:       json.RawMessage(`[{"type":"function","name":"lookup","description":"old","parameters":{"type":"object"}}]`),
		Output:      []ResponseOutputItem{{ID: "fc_1", Type: "function_call", Status: "completed", CallID: "call_1", Name: "lookup", Arguments: `{}`}},
	}
	if err := validateStoredResponseDoc(&doc, doc.ID); err != nil {
		t.Fatalf("v1 function-only state rejected: %v", err)
	}
	loaded := responseObjectFromStoredDoc(&doc)
	follow := ResponsesRequest{}
	if err := responsesInheritStoredTools(&follow, loaded); err != nil {
		t.Fatal(err)
	}
	if len(follow.Tools) != 1 || follow.Tools[0].Function.Name != "lookup" || follow.Tools[0].ResponsesKind != responsesToolKindFunction {
		t.Fatalf("v1 inherited tools=%#v", follow.Tools)
	}
}

func TestPhase64ContinuationToolChoiceCanResolveInheritedNamespace(t *testing.T) {
	previous := &ResponsesObject{StoredStateVersion: 2}
	previous.StoredToolCatalog = json.RawMessage(`[
		{"identity":{"namespace":"functions","name":"exec","kind":"custom"},"semantic":{"type":"custom","name":"exec","namespace":"functions","description":"","format":null}}
	]`)
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(`{
		"previous_response_id":"resp_previous",
		"input":"continue",
		"tool_choice":{"type":"custom","name":"exec","namespace":"functions"}
	}`), &req); err != nil {
		t.Fatal(err)
	}
	initial, err := parseResponsesToolChoice(req.ToolChoice, req.Tools)
	if err != nil || initial.Kind != responsesToolChoiceSpecific {
		t.Fatalf("routing-phase deferred choice plan=%#v err=%v", initial, err)
	}
	if err := responsesInheritStoredTools(&req, previous); err != nil {
		t.Fatal(err)
	}
	resolved, err := parseResponsesToolChoice(req.ToolChoice, req.Tools)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Namespace != "functions" || resolved.ToolType != responsesToolKindCustom {
		t.Fatalf("resolved inherited tool choice=%#v", resolved)
	}
}

func TestPhase64SerialToolGuardContract(t *testing.T) {
	falseValue := false
	req := &ResponsesRequest{ParallelToolCalls: &falseValue, Tools: []OpenAITool{{Type: "function"}}}
	if !responsesRequiresSerialToolGuard(req, responsesToolChoicePlan{Kind: responsesToolChoiceAuto}) {
		t.Fatal("parallel_tool_calls=false with tools did not enable serial guard")
	}
	if responsesRequiresSerialToolGuard(req, responsesToolChoicePlan{Kind: responsesToolChoiceNone}) {
		t.Fatal("tool_choice=none unnecessarily enabled serial guard")
	}
	trueValue := true
	req.ParallelToolCalls = &trueValue
	if responsesRequiresSerialToolGuard(req, responsesToolChoicePlan{Kind: responsesToolChoiceAuto}) {
		t.Fatal("parallel_tool_calls=true enabled serial guard")
	}
}

func TestPhase64NonStreamSerialGuardCommitsOneCustomToolCall(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	body := `{
		"model":"claude-sonnet-4.5",
		"parallel_tool_calls":false,
		"input":[
			{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]},
			{"role":"user","content":[{"type":"input_text","text":"run it"}]}
		]
	}`
	var parsed ResponsesRequest
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	wireName := buildOpenAIToolNameCatalog(parsed.Tools).kiroNameFor("functions", "exec")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_serial_nonstream_1", "name": wireName, "input": `{"input":"echo hi"}`, "stop": true,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	for _, want := range []string{
		`"type":"custom_tool_call"`,
		`"name":"exec"`,
		`"namespace":"functions"`,
		`"input":"echo hi"`,
		`"parallel_tool_calls":false`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("non-stream response missing %q: %s", want, got)
		}
	}
}

func TestPhase64NonStreamSerialGuardRejectsMultipleToolCalls(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	body := `{
		"model":"claude-sonnet-4.5",
		"parallel_tool_calls":false,
		"tools":[{"type":"function","name":"lookup","description":"lookup","parameters":{"type":"object"}}],
		"input":"use lookup"
	}`
	var parsed ResponsesRequest
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	wireName := buildOpenAIToolNameCatalog(parsed.Tools).kiroNameFor("", "lookup")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for _, id := range []string{"toolu_nonstream_hidden_1", "toolu_nonstream_hidden_2"} {
			_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
				"toolUseId": id, "name": wireName, "input": `{"q":"x"}`, "stop": true,
			}))
		}
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=400 body=%s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	if !strings.Contains(got, "parallel_tool_calls=false candidate emitted 2 tool calls") {
		t.Fatalf("serial guard reason missing: %s", got)
	}
	for _, leaked := range []string{"toolu_nonstream_hidden_1", "toolu_nonstream_hidden_2"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("serial guard leaked %q: %s", leaked, got)
		}
	}
}

func TestPhase64StreamingSerialGuardCommitsOneCustomToolCall(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	body := `{
		"model":"claude-sonnet-4.5",
		"stream":true,
		"parallel_tool_calls":false,
		"input":[
			{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]},
			{"role":"user","content":[{"type":"input_text","text":"run it"}]}
		]
	}`
	var parsed ResponsesRequest
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	wireName := buildOpenAIToolNameCatalog(parsed.Tools).kiroNameFor("functions", "exec")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_serial_1", "name": wireName, "input": `{"input":"*** Begin Patch"}`, "stop": true,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	for _, want := range []string{
		`event: response.custom_tool_call_input.delta`,
		`event: response.custom_tool_call_input.done`,
		`"type":"custom_tool_call"`,
		`"name":"exec"`,
		`"namespace":"functions"`,
		`"input":"*** Begin Patch"`,
		`event: response.completed`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"namespace":""`) {
		t.Fatalf("stream emitted an empty namespace field:\n%s", got)
	}
}

func TestPhase64StreamingSerialGuardRejectsMultipleToolCallsBeforeProtocolCommit(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	body := `{
		"model":"claude-sonnet-4.5",
		"stream":true,
		"parallel_tool_calls":false,
		"tools":[{"type":"function","name":"lookup","description":"lookup","parameters":{"type":"object"}}],
		"input":"use lookup"
	}`
	var parsed ResponsesRequest
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	wireName := buildOpenAIToolNameCatalog(parsed.Tools).kiroNameFor("", "lookup")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for _, id := range []string{"toolu_hidden_1", "toolu_hidden_2"} {
			_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
				"toolUseId": id, "name": wireName, "input": `{"q":"x"}`, "stop": true,
			}))
		}
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=400 body=%s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	if !strings.Contains(got, "parallel_tool_calls=false candidate emitted 2 tool calls") {
		t.Fatalf("serial guard reason missing: %s", got)
	}
	for _, leaked := range []string{"toolu_hidden_1", "toolu_hidden_2", "response.output_item.added", "response.created"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("serial guard leaked %q before rejecting candidate: %s", leaked, got)
		}
	}
}

func TestPhase64RoutingEnvelopeStaysPermissiveForFutureResponsesShapes(t *testing.T) {
	body := []byte(`{
		"model":"relay-model",
		"stream":false,
		"instructions":[{"type":"future_instruction","text":"provider-specific"}],
		"input":[
			{"type":"additional_tools","tools":[
				{"type":"namespace","name":"functions","description":"","tools":[
					{"type":"custom","name":"exec","future_schema":{"opaque":true}},
					{"type":"future_hosted_tool","name":"provider_search"}
				]}
			]},
			{"type":"future_input_item","provider_payload":{"opaque":true}}
		],
		"future_top_level":{"opaque":true},
		"tool_choice":{"type":"custom","name":"exec","namespace":"functions"},
		"parallel_tool_calls":false
	}`)

	req, err := parseResponsesRoutingEnvelope(body)
	if err != nil {
		t.Fatalf("permissive routing envelope rejected future Responses shape: %v", err)
	}
	if req.Model != "relay-model" || req.Stream {
		t.Fatalf("routing envelope lost core fields: %#v", req)
	}
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
		t.Fatalf("routing envelope lost parallel_tool_calls=false: %#v", req.ParallelToolCalls)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("routing envelope known tool count=%d, want 1: %#v", len(req.Tools), req.Tools)
	}
	if req.Tools[0].Function.Name != "exec" || req.Tools[0].ResponsesNamespace != "functions" || req.Tools[0].ResponsesKind != responsesToolKindCustom {
		t.Fatalf("routing envelope lost namespaced custom identity: %#v", req.Tools[0])
	}
	plan, err := parseResponsesToolChoice(req.ToolChoice, req.Tools)
	if err != nil {
		t.Fatalf("routing tool choice failed: %v", err)
	}
	if plan.Kind != responsesToolChoiceSpecific || plan.ToolName != "exec" || plan.Namespace != "functions" || plan.ToolType != responsesToolKindCustom {
		t.Fatalf("routing tool choice identity=%#v", plan)
	}
}

func TestPhase64StoredStateV2RejectsToolCallKindMismatch(t *testing.T) {
	now := time.Now().Unix()
	doc := storedResponseDoc{
		StateVersion: 2,
		ID:           "resp_v2_kind_mismatch", Object: "response", CreatedAt: now, Status: "completed", Model: "gpt-5.6-sol", StoredAt: now,
		StoredInput: json.RawMessage(`"go"`),
		ToolCatalog: json.RawMessage(`[
			{"identity":{"namespace":"functions","name":"exec","kind":"custom"},"description":"execute","semantic":{"type":"custom","name":"exec","namespace":"functions","description":"execute","format":null}}
		]`),
		Output: []ResponseOutputItem{{
			ID: "fc_1", Type: "function_call", Status: "completed", CallID: "call_1", Name: "exec", Namespace: "functions", Arguments: `{}`,
		}},
	}
	if err := validateStoredResponseDoc(&doc, doc.ID); err == nil || !strings.Contains(err.Error(), "conflicts with persisted custom declaration") {
		t.Fatalf("v2 function/custom kind mismatch error=%v", err)
	}
}
