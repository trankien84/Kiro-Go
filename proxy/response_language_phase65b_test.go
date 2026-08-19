package proxy

import (
	"bytes"
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const phase65bLanguageMarker = "[Response language policy]"

func initPhase65BConfig(t *testing.T) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = config.UpdateResponseLanguageConfig(config.ResponseLanguagePolicyPreserve, "")
	})
}

func firstKiroPrimingContent(t *testing.T, payload *KiroPayload) string {
	t.Helper()
	if payload == nil || len(payload.ConversationState.History) == 0 {
		t.Fatalf("missing Kiro priming history: %#v", payload)
	}
	first := payload.ConversationState.History[0].UserInputMessage
	if first == nil {
		t.Fatalf("first history item is not user priming: %#v", payload.ConversationState.History[0])
	}
	return first.Content
}

func TestPhase65BPreserveModeDoesNotInjectLanguageSteering(t *testing.T) {
	initPhase65BConfig(t)
	if err := config.UpdateResponseLanguageConfig(config.ResponseLanguagePolicyPreserve, ""); err != nil {
		t.Fatal(err)
	}

	req := &ClaudeRequest{
		Model:  "claude-opus-5",
		System: "CLIENT_SYSTEM_RULE",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Hãy kiểm tra file này."},
		},
	}
	payload := ClaudeToKiro(req, false)
	priming := firstKiroPrimingContent(t, payload)
	if strings.Contains(priming, phase65bLanguageMarker) {
		t.Fatalf("preserve mode injected language steering: %q", priming)
	}
	if !strings.Contains(priming, "CLIENT_SYSTEM_RULE") {
		t.Fatalf("client system prompt was lost: %q", priming)
	}
}

func TestPhase65BMatchUserSteeringAppliesToClaudeAndOpenAIKiroProjection(t *testing.T) {
	initPhase65BConfig(t)
	if err := config.UpdateResponseLanguageConfig(config.ResponseLanguagePolicyMatchUser, ""); err != nil {
		t.Fatal(err)
	}

	claudeReq := &ClaudeRequest{
		Model: "claude-opus-5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Hãy sửa lỗi rồi chạy test giúp mình."},
		},
	}
	claudePayload := ClaudeToKiro(claudeReq, false)
	claudePriming := firstKiroPrimingContent(t, claudePayload)
	for _, want := range []string{
		phase65bLanguageMarker,
		"latest substantive human user request",
		"not from tool results, logs, source code, quoted material, metadata, assistant-generated text, or proxy-injected execution reminders",
		"explicitly requests a response language",
	} {
		if !strings.Contains(claudePriming, want) {
			t.Fatalf("Claude priming missing %q: %q", want, claudePriming)
		}
	}
	if got := claudePayload.ConversationState.CurrentMessage.UserInputMessage.Content; got != "Hãy sửa lỗi rồi chạy test giúp mình." {
		t.Fatalf("user content was mutated: %q", got)
	}

	openAIReq := &OpenAIRequest{
		Model: "claude-opus-5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Hãy tiếp tục công việc."},
		},
	}
	openAIPayload := OpenAIToKiro(openAIReq, false)
	openAIPriming := firstKiroPrimingContent(t, openAIPayload)
	if !strings.Contains(openAIPriming, phase65bLanguageMarker) || !strings.Contains(openAIPriming, "latest substantive human user request") {
		t.Fatalf("OpenAI Kiro projection missing match-user steering: %q", openAIPriming)
	}
	if got := extractOpenAIMessageText(openAIReq.Messages[0].Content); got != "Hãy tiếp tục công việc." {
		t.Fatalf("OpenAI client request was mutated: %q", got)
	}
}

func TestPhase65BMatchUserPolicyDoesNotTreatToolOutputAsLanguageSource(t *testing.T) {
	initPhase65BConfig(t)
	if err := config.UpdateResponseLanguageConfig(config.ResponseLanguagePolicyMatchUser, ""); err != nil {
		t.Fatal(err)
	}

	req := &ClaudeRequest{
		Model: "claude-opus-5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Hãy mở trang rồi kiểm tra nội dung."},
			{Role: "assistant", Content: []interface{}{map[string]interface{}{
				"type": "tool_use", "id": "tool_1", "name": "browser_open", "input": map[string]interface{}{"url": "https://example.com"},
			}}},
			{Role: "user", Content: []interface{}{map[string]interface{}{
				"type": "tool_result", "tool_use_id": "tool_1", "content": "PAGE LOADED SUCCESSFULLY IN ENGLISH",
			}}},
		},
	}
	payload := ClaudeToKiro(req, false)
	priming := firstKiroPrimingContent(t, payload)
	if !strings.Contains(priming, "not from tool results") {
		t.Fatalf("tool-result exclusion rule missing: %q", priming)
	}
	ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.ToolResults) != 1 || ctx.ToolResults[0].ToolUseID != "tool_1" {
		t.Fatalf("tool continuation was not preserved: %#v", ctx)
	}
}

func TestPhase65BFixedLanguageUsesValidatedTagWithoutTranslatingTechnicalLiterals(t *testing.T) {
	initPhase65BConfig(t)
	if err := config.UpdateResponseLanguageConfig(config.ResponseLanguagePolicyFixed, "vi"); err != nil {
		t.Fatal(err)
	}

	prompt := responseLanguageSteeringPrompt()
	for _, want := range []string{`language/locale "vi"`, "code, identifiers, paths, commands, API names", "explicitly requests a different response language"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("fixed-language prompt missing %q: %q", want, prompt)
		}
	}
}

func TestPhase65BSettingsAPIValidatesAndPersistsLanguagePolicy(t *testing.T) {
	initPhase65BConfig(t)
	h := &Handler{}

	badBody := []byte(`{"requireApiKey":true,"responseLanguagePolicy":"fixed","responseLanguage":"../../vi"}`)
	badReq := httptest.NewRequest(http.MethodPost, "/admin/api/settings", bytes.NewReader(badBody))
	badRec := httptest.NewRecorder()
	h.apiUpdateSettings(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("invalid language status=%d body=%s", badRec.Code, badRec.Body.String())
	}
	if got := config.GetResponseLanguageConfig(); got.Policy != config.ResponseLanguagePolicyPreserve {
		t.Fatalf("invalid settings request mutated language config: %#v", got)
	}
	if config.IsApiKeyRequired() {
		t.Fatal("invalid language request partially mutated unrelated settings")
	}

	goodBody := []byte(`{"responseLanguagePolicy":"match-user","responseLanguage":"vi"}`)
	goodReq := httptest.NewRequest(http.MethodPost, "/admin/api/settings", bytes.NewReader(goodBody))
	goodRec := httptest.NewRecorder()
	h.apiUpdateSettings(goodRec, goodReq)
	if goodRec.Code != http.StatusOK {
		t.Fatalf("valid language status=%d body=%s", goodRec.Code, goodRec.Body.String())
	}
	if got := config.GetResponseLanguageConfig(); got.Policy != config.ResponseLanguagePolicyMatchUser || got.Language != "" {
		t.Fatalf("persisted=%#v", got)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil)
	getRec := httptest.NewRecorder()
	h.apiGetSettings(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get settings status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(getRec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["responseLanguagePolicy"] != config.ResponseLanguagePolicyMatchUser || body["responseLanguage"] != "" {
		t.Fatalf("settings response=%#v", body)
	}
}
