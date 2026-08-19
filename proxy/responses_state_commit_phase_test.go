package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"kiro-go/config"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoredResponsesOutputProjectsOneAssistantTurn(t *testing.T) {
	tools := json.RawMessage(`[
		{"type":"function","name":"read_file","parameters":{"type":"object"}},
		{"type":"function","name":"grep","parameters":{"type":"object"}}
	]`)
	items := []ResponseOutputItem{
		{ID: "rs_1", Type: "reasoning", Content: []ResponseContentPart{{Type: "reasoning_text", Text: "private"}}},
		{ID: "msg_1", Type: "message", Role: "assistant", Content: []ResponseContentPart{{Type: "output_text", Text: "Checking "}}},
		{ID: "msg_2", Type: "message", Role: "assistant", Content: []ResponseContentPart{{Type: "output_text", Text: "both."}}},
		{ID: "fc_1", Type: "function_call", CallID: "call_1", Name: "read_file", Arguments: `{"path":"a"}`},
		{ID: "fc_2", Type: "function_call", CallID: "call_2", Name: "grep", Arguments: `{"q":"x"}`},
	}
	catalog, err := responsesToolCatalogFromStoredRaw(tools, items, 1)
	if err != nil {
		t.Fatal(err)
	}
	var projection openAIProjection
	messages, err := projectStoredResponsesOutputTurnVersion(items, catalog, &projection, "$.output", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages=%#v, want one assistant turn", messages)
	}
	if messages[0].Role != "assistant" || extractOpenAIMessageText(messages[0].Content) != "Checking both." {
		t.Fatalf("visible output was not preserved in one turn: %#v", messages[0])
	}
	if len(messages[0].ToolCalls) != 2 || messages[0].ToolCalls[0].ID != "call_1" || messages[0].ToolCalls[1].ID != "call_2" {
		t.Fatalf("parallel calls were not preserved together: %#v", messages[0].ToolCalls)
	}
	if !projection.Emulated() || !projection.Supported() {
		t.Fatalf("reasoning replay fidelity=%v issues=%#v, want supported Emulated", projection.Fidelity, projection.Issues)
	}
	if strings.Contains(extractOpenAIMessageText(messages[0].Content), "private") {
		t.Fatal("reasoning text leaked into ordinary assistant history")
	}
}

func TestStoredResponsesV1OutputAcceptsAlreadyCanonicalKiroToolName(t *testing.T) {
	catalog := openAIToolNameCatalog{
		OriginalToKiro: map[string]string{"foo-bar": "foo_bar"},
		KiroToOriginal: map[string]string{"foo_bar": "foo-bar"},
	}
	messages, err := projectStoredResponsesOutputTurnVersion(
		[]ResponseOutputItem{{Type: "function_call", CallID: "c1", Name: "foo_bar", Arguments: `{}`}},
		catalog,
		nil,
		"$.output",
		1,
	)
	if err != nil {
		t.Fatalf("already-canonical persisted tool call rejected: %v", err)
	}
	if len(messages) != 1 || len(messages[0].ToolCalls) != 1 || messages[0].ToolCalls[0].Function.Name != "foo_bar" {
		t.Fatalf("canonical tool name changed during replay: %#v", messages)
	}
}

func TestStoredResponsesOutputRejectsUnknownFamily(t *testing.T) {
	_, err := projectStoredResponsesOutputTurn(
		[]ResponseOutputItem{{Type: "web_search_call", ID: "ws_1"}},
		openAIToolNameCatalog{},
		nil,
		"$.output",
	)
	if err == nil || !strings.Contains(err.Error(), "web_search_call") {
		t.Fatalf("unknown stored output was not rejected: %v", err)
	}
}

func TestStoredResponsesInputFailsClosed(t *testing.T) {
	cases := []json.RawMessage{
		json.RawMessage(`[not-json]`),
		json.RawMessage(`[{"type":"future_item","value":"x"}]`),
		json.RawMessage(`[{"type":"function_call","call_id":"c","name":"f","arguments":"not-json"}]`),
	}
	for _, raw := range cases {
		if _, err := parseStoredResponsesInput(raw); err == nil {
			t.Fatalf("malformed/unknown persisted input was accepted: %s", raw)
		}
	}

	valid := json.RawMessage(`[
		{"type":"function_call_output","call_id":"c1","output":"one"},
		{"type":"function_call_output","call_id":"c2","output":"two"}
	]`)
	messages, err := parseStoredResponsesInput(valid)
	if err != nil {
		t.Fatalf("valid persisted tool outputs rejected: %v", err)
	}
	if len(messages) != 2 || messages[0].ToolCallID != "c1" || messages[1].ToolCallID != "c2" {
		t.Fatalf("tool outputs changed during strict replay: %#v", messages)
	}
}

func TestStoredResponsesToolCatalogVersionRules(t *testing.T) {
	items := []ResponseOutputItem{{Type: "function_call", CallID: "c1", Name: "lookup", Arguments: `{}`}}
	if _, err := responsesToolCatalogFromStoredRaw(nil, items, storedResponseStateVersion); err == nil {
		t.Fatal("versioned state with a function call but no persisted tools was accepted")
	}
	legacy, err := responsesToolCatalogFromStoredRaw(nil, items, 0)
	if err != nil {
		t.Fatalf("legacy state should use exact identity fallback: %v", err)
	}
	if got := legacy.kiroName("lookup"); got != "lookup" {
		t.Fatalf("legacy tool name changed: %q", got)
	}
	if _, err := responsesToolCatalogFromStoredRaw(json.RawMessage(`{bad`), items, storedResponseStateVersion); err == nil {
		t.Fatal("malformed persisted tools were accepted")
	}
}

func TestStoredResponseDocumentPersistsReplaySemanticsAndVersion(t *testing.T) {
	resp := &ResponsesObject{
		ID:                "resp_test",
		ToolChoice:        json.RawMessage(`{"type":"function","name":"lookup"}`),
		Tools:             json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`),
		ParallelToolCalls: true,
		StoredInput:       json.RawMessage(`"hello"`),
	}
	doc := storedResponseDocument(resp)
	if doc.StateVersion != storedResponseStateVersion {
		t.Fatalf("state_version=%d want=%d", doc.StateVersion, storedResponseStateVersion)
	}
	if string(doc.Tools) != string(resp.Tools) || string(doc.ToolChoice) != string(resp.ToolChoice) || !doc.ParallelToolCalls {
		t.Fatalf("replay semantics were not persisted: %#v", doc)
	}
}

func TestReadStoredResponseRejectsFutureVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "response.json")
	doc := storedResponseDoc{
		StateVersion: storedResponseStateVersion + 1,
		ID:           "resp_future",
		Object:       "response",
		StoredAt:     time.Now().Unix(),
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readStoredResponseDoc(path, doc.ID); !errors.Is(err, errStoredResponseVersionUnsupported) {
		t.Fatalf("future state version error=%v", err)
	}
}

func validStoredResponseDocV1ForTest() storedResponseDoc {
	now := time.Now().Unix()
	return storedResponseDoc{
		StateVersion: 1,
		ID:           "resp_valid_v1",
		Object:       "response",
		CreatedAt:    now,
		Status:       "completed",
		Model:        "test-model",
		Output: []ResponseOutputItem{{
			ID: "msg_1", Type: "message", Role: "assistant", Status: "completed",
			Content: []ResponseContentPart{{Type: "output_text", Text: "ok"}},
		}},
		StoredInput: json.RawMessage(`"hello"`),
		StoredAt:    now,
	}
}

func TestStoredResponseV1SchemaRejectsImpossibleEnvelopeAndOutput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*storedResponseDoc)
	}{
		{name: "object", mutate: func(doc *storedResponseDoc) { doc.Object = "not-response" }},
		{name: "status", mutate: func(doc *storedResponseDoc) { doc.Status = "failed" }},
		{name: "model", mutate: func(doc *storedResponseDoc) { doc.Model = "" }},
		{name: "created_at", mutate: func(doc *storedResponseDoc) { doc.CreatedAt = 0 }},
		{name: "stored_at", mutate: func(doc *storedResponseDoc) { doc.StoredAt = 0 }},
		{name: "item_status", mutate: func(doc *storedResponseDoc) { doc.Output[0].Status = "in_progress" }},
		{name: "message_content_type", mutate: func(doc *storedResponseDoc) { doc.Output[0].Content[0].Type = "input_text" }},
		{name: "duplicate_item_id", mutate: func(doc *storedResponseDoc) { doc.Output = append(doc.Output, doc.Output[0]) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := validStoredResponseDocV1ForTest()
			tc.mutate(&doc)
			if err := validateStoredResponseDoc(&doc, doc.ID); err == nil {
				t.Fatalf("invalid v1 state was accepted: %#v", doc)
			}
		})
	}
}

func TestReadStoredResponseV1RejectsFailedOrForeignEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*storedResponseDoc)
	}{
		{name: "failed", mutate: func(doc *storedResponseDoc) { doc.Status = "failed" }},
		{name: "foreign_object", mutate: func(doc *storedResponseDoc) { doc.Object = "chat.completion" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetResponseStateHealthForTest()
			t.Cleanup(resetResponseStateHealthForTest)
			doc := validStoredResponseDocV1ForTest()
			tc.mutate(&doc)
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "response.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readStoredResponseDoc(path, doc.ID); err == nil {
				t.Fatalf("invalid persisted envelope was replayable: %#v", doc)
			}
		})
	}
}

func TestStoredResponseV1RejectsFunctionOutsidePersistedToolCatalog(t *testing.T) {
	doc := validStoredResponseDocV1ForTest()
	doc.Tools = json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`)
	doc.Output = []ResponseOutputItem{{
		ID: "fc_1", Type: "function_call", Status: "completed", CallID: "call_1", Name: "other", Arguments: `{}`,
	}}
	if err := validateStoredResponseDoc(&doc, doc.ID); err == nil || !strings.Contains(err.Error(), "persisted tool declaration") {
		t.Fatalf("unknown persisted function name error=%v", err)
	}
}

func TestStoredResponseV1RequiresExplicitFunctionCallID(t *testing.T) {
	doc := validStoredResponseDocV1ForTest()
	doc.Tools = json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`)
	doc.Output = []ResponseOutputItem{{
		ID: "fc_1", Type: "function_call", Status: "completed", Name: "lookup", Arguments: `{}`,
	}}
	if err := validateStoredResponseDoc(&doc, doc.ID); err == nil || !strings.Contains(err.Error(), "call_id") {
		t.Fatalf("v1 state without call_id error=%v", err)
	}

	catalog, err := responsesToolCatalogFromStoredRaw(doc.Tools, doc.Output, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projectStoredResponsesOutputTurnVersion(doc.Output, catalog, nil, "$.output", 1); err == nil {
		t.Fatal("v1 projector fell back from missing call_id to output item id")
	}
}

func TestStoredResponseV0KeepsLegacyFunctionCallIDFallback(t *testing.T) {
	items := []ResponseOutputItem{{ID: "legacy_call", Type: "function_call", Name: "lookup", Arguments: `{}`}}
	catalog, err := responsesToolCatalogFromStoredRaw(nil, items, 0)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := projectStoredResponsesOutputTurnVersion(items, catalog, nil, "$.output", 0)
	if err != nil {
		t.Fatalf("legacy function_call fallback rejected: %v", err)
	}
	if len(messages) != 1 || len(messages[0].ToolCalls) != 1 || messages[0].ToolCalls[0].ID != "legacy_call" {
		t.Fatalf("legacy call id fallback changed: %#v", messages)
	}
}

func TestImmutableResponseStateDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "response.json")
	if state, err := writeImmutableResponseState(path, []byte("first")); err != nil || state != responseStatePublishedDurable {
		t.Fatalf("first publish state=%v err=%v", state, err)
	}
	state, err := writeImmutableResponseState(path, []byte("second"))
	if state != responseStateNotPublished || !errors.Is(err, os.ErrExist) {
		t.Fatalf("second immutable publish state=%v err=%v", state, err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != "first" {
		t.Fatalf("winner was overwritten: data=%q err=%v", data, readErr)
	}
}

func TestResponseInstanceNamespaceConcurrentPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".instance-id")
	const workers = 32
	values := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
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
		t.Fatalf("concurrent namespace publish failed: %v", err)
	}
	var winner string
	for value := range values {
		if winner == "" {
			winner = value
		}
		if value != winner {
			t.Fatalf("split namespace ownership: winner=%q got=%q", winner, value)
		}
	}
	if disk, err := readResponseInstanceNamespace(path); err != nil || disk != winner {
		t.Fatalf("published namespace mismatch disk=%q winner=%q err=%v", disk, winner, err)
	}
}

func TestResponseInstanceNamespaceCorruptionReturnsTypedError(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".instance-id")
	if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := responseInstanceNamespaceAt(path); !errors.Is(err, errResponseInstanceNamespaceCorrupt) {
		t.Fatalf("corrupt final namespace was not rejected: %v", err)
	}
}

func TestImmutableResponseStateDurabilityUnknownPoisonsStateStore(t *testing.T) {
	resetResponseStateHealthForTest()
	t.Cleanup(resetResponseStateHealthForTest)

	oldSync := syncResponseStateDirectoryForWrite
	t.Cleanup(func() { syncResponseStateDirectoryForWrite = oldSync })
	sentinel := errors.New("directory sync failed")
	syncResponseStateDirectoryForWrite = func(string) error { return sentinel }

	path := filepath.Join(t.TempDir(), "response.json")
	state, err := writeImmutableResponseState(path, []byte("committed"))
	var durabilityErr *responseStateDurabilityError
	if state != responseStateDurabilityUnknown || !errors.As(err, &durabilityErr) || !errors.Is(err, errResponseStateUnavailable) || !errors.Is(err, sentinel) {
		t.Fatalf("durability failure state=%v err=%v", state, err)
	}
	if data, readErr := os.ReadFile(path); readErr != nil || string(data) != "committed" {
		t.Fatalf("published state disappeared after durability error: data=%q err=%v", data, readErr)
	}
	if nextState, nextErr := writeImmutableResponseState(filepath.Join(filepath.Dir(path), "next.json"), []byte("next")); nextState != responseStateNotPublished || !errors.Is(nextErr, errResponseStateUnavailable) {
		t.Fatalf("poisoned store accepted another commit: state=%v err=%v", nextState, nextErr)
	}
}

func TestImmutableResponseStateDoesNotAcknowledgeConcurrentPoisonAfterPublish(t *testing.T) {
	resetResponseStateHealthForTest()
	t.Cleanup(resetResponseStateHealthForTest)

	oldPublish := publishResponseStateNoReplaceForWrite
	oldSync := syncResponseStateDirectoryForWrite
	t.Cleanup(func() {
		publishResponseStateNoReplaceForWrite = oldPublish
		syncResponseStateDirectoryForWrite = oldSync
	})

	sentinel := errors.New("concurrent durability failure")
	publishResponseStateNoReplaceForWrite = func(source, destination string) (bool, error) {
		if err := os.Link(source, destination); err != nil {
			return false, err
		}
		if err := os.Remove(source); err != nil {
			return true, err
		}
		poisonResponseState(&responseStateDurabilityError{err: sentinel})
		return true, nil
	}
	syncResponseStateDirectoryForWrite = func(string) error { return nil }

	path := filepath.Join(t.TempDir(), "response.json")
	state, err := writeImmutableResponseState(path, []byte("durable-but-not-acknowledged"))
	if state != responseStatePublishedDurable || !errors.Is(err, errResponseStateUnavailable) || !errors.Is(err, sentinel) {
		t.Fatalf("concurrent poison result state=%v err=%v", state, err)
	}
	if data, readErr := os.ReadFile(path); readErr != nil || string(data) != "durable-but-not-acknowledged" {
		t.Fatalf("published data=%q err=%v", data, readErr)
	}
}

func TestPublishedButUnsyncedResponseCannotContinueUntilRestart(t *testing.T) {
	resetResponseStateHealthForTest()
	t.Cleanup(resetResponseStateHealthForTest)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := ensureResponsesDir(); err != nil {
		t.Fatalf("ensure responses dir: %v", err)
	}

	oldSync := syncResponseStateDirectoryForWrite
	t.Cleanup(func() { syncResponseStateDirectoryForWrite = oldSync })
	sentinel := errors.New("directory sync failed")
	syncResponseStateDirectoryForWrite = func(string) error { return sentinel }

	now := time.Now().Unix()
	resp := &ResponsesObject{
		ID:          "resp_durability_unknown",
		Object:      "response",
		CreatedAt:   now,
		Status:      "completed",
		Model:       "test-model",
		Output:      []ResponseOutputItem{responseMessageOutputItem("msg_test", "done")},
		StoredInput: json.RawMessage(`"hello"`),
	}
	if err := saveResponse(resp); !errors.Is(err, errResponseStateUnavailable) || !errors.Is(err, sentinel) {
		t.Fatalf("saveResponse durability error=%v", err)
	}
	if _, err := loadResponse(resp.ID); !errors.Is(err, errResponseStateUnavailable) {
		t.Fatalf("poisoned store allowed continuation: %v", err)
	}

	// Simulate process restart: the in-memory fail-stop latch clears. If the
	// published file is still visible and validates structurally, it may be used
	// again after a fresh process establishes a healthy state-store session.
	resetResponseStateHealthForTest()
	syncResponseStateDirectoryForWrite = oldSync
	loaded, err := loadResponse(resp.ID)
	if err != nil {
		t.Fatalf("restart-style recovery could not load visible valid state: %v", err)
	}
	if loaded.ID != resp.ID || loaded.Status != "completed" {
		t.Fatalf("recovered response=%#v", loaded)
	}
}

func TestPersistResponsesCompletionIsRequiredOnlyWhenStoreTrue(t *testing.T) {
	oldSave := saveResponseForCompletion
	defer func() { saveResponseForCompletion = oldSave }()
	sentinel := errors.New("disk full")
	calls := 0
	saveResponseForCompletion = func(*ResponsesObject) error {
		calls++
		return sentinel
	}

	resp := &ResponsesObject{ID: "resp_commit"}
	if err := persistResponsesCompletion(resp, false); err != nil || calls != 0 {
		t.Fatalf("store=false touched persistence: calls=%d err=%v", calls, err)
	}
	if err := persistResponsesCompletion(resp, true); !errors.Is(err, sentinel) || calls != 1 {
		t.Fatalf("store=true did not make persistence part of commit: calls=%d err=%v", calls, err)
	}
}

func TestEnsureResponsesDirSyncsParentOnFirstCreate(t *testing.T) {
	resetResponseStateHealthForTest()
	t.Cleanup(resetResponseStateHealthForTest)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = os.RemoveAll(responsesDir())

	oldSync := syncResponseStateDirectoryForWrite
	t.Cleanup(func() { syncResponseStateDirectoryForWrite = oldSync })
	var synced []string
	syncResponseStateDirectoryForWrite = func(path string) error {
		synced = append(synced, filepath.Clean(path))
		return nil
	}
	if err := ensureResponsesDir(); err != nil {
		t.Fatalf("ensureResponsesDir: %v", err)
	}
	wantParent := filepath.Clean(filepath.Dir(responsesDir()))
	wantDir := filepath.Clean(responsesDir())
	if len(synced) != 2 || synced[0] != wantParent || synced[1] != wantDir {
		t.Fatalf("synced=%v want=[%s %s]", synced, wantParent, wantDir)
	}
}

func TestPurgeExpiredResponsesRemovesOnlyStaleOwnedTemps(t *testing.T) {
	resetResponseStateHealthForTest()
	t.Cleanup(resetResponseStateHealthForTest)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := ensureResponsesDir(); err != nil {
		t.Fatal(err)
	}
	dir := responsesDir()
	hash := strings.Repeat("0", sha256.Size*2)
	staleResponseTemp := filepath.Join(dir, ".response_"+hash+".json.tmp-stale")
	staleNamespaceTemp := filepath.Join(dir, "..instance-id.tmp-stale")
	recentResponseTemp := filepath.Join(dir, ".response_"+hash+".json.tmp-recent")
	foreignTemp := filepath.Join(dir, ".other.json.tmp-stale")
	for _, path := range []string{staleResponseTemp, staleNamespaceTemp, recentResponseTemp, foreignTemp} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-responsesTempCleanupGrace - time.Hour)
	for _, path := range []string{staleResponseTemp, staleNamespaceTemp, foreignTemp} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	purgeExpiredResponses(365 * 24 * time.Hour)
	for _, path := range []string{staleResponseTemp, staleNamespaceTemp} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale owned temp still exists %s: %v", filepath.Base(path), err)
		}
	}
	for _, path := range []string{recentResponseTemp, foreignTemp} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected temp was removed %s: %v", filepath.Base(path), err)
		}
	}
}

func TestPoisonedResponseStateCannotReplayAlreadyLoadedHistory(t *testing.T) {
	resetResponseStateHealthForTest()
	t.Cleanup(resetResponseStateHealthForTest)
	sentinel := errors.New("durability unknown")
	poisonResponseState(&responseStateDurabilityError{err: sentinel})

	prev := &ResponsesObject{
		ID:                 "resp_loaded_before_poison",
		StoredStateVersion: storedResponseStateVersion,
		StoredInput:        json.RawMessage(`"hello"`),
		Output:             []ResponseOutputItem{responseMessageOutputItem("msg_1", "world")},
	}
	if _, err := expandPreviousResponseHistory(prev); !errors.Is(err, errResponseStateUnavailable) {
		t.Fatalf("poisoned state replay error=%v", err)
	}
}

func TestMissingResponsesDirectoryDoesNotPoisonStateStore(t *testing.T) {
	resetResponseStateHealthForTest()
	t.Cleanup(resetResponseStateHealthForTest)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = os.RemoveAll(responsesDir())

	purgeExpiredResponses(responsesDefaultTTL)
	if err := responseStateHealthError(); err != nil {
		t.Fatalf("startup purge poisoned an absent state directory: %v", err)
	}
	if _, err := loadResponse("resp_missing_external"); !errors.Is(err, errStoredResponseNotFound) {
		t.Fatalf("missing state error=%v", err)
	}
	if err := responseStateHealthError(); err != nil {
		t.Fatalf("missing read poisoned state store: %v", err)
	}
}

func TestLegacyMigrationDurabilityUnknownFailsCurrentLoadClosed(t *testing.T) {
	resetResponseStateHealthForTest()
	t.Cleanup(resetResponseStateHealthForTest)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := ensureResponsesDir(); err != nil {
		t.Fatal(err)
	}
	id := "resp_legacy_migration_poison"
	doc := storedResponseDoc{ID: id, Object: "response", Status: "completed", Model: "legacy", StoredAt: time.Now().Unix()}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyResponseStatePath(id), data, 0o600); err != nil {
		t.Fatal(err)
	}

	oldSync := syncResponseStateDirectoryForWrite
	t.Cleanup(func() { syncResponseStateDirectoryForWrite = oldSync })
	sentinel := errors.New("migration directory sync failed")
	syncResponseStateDirectoryForWrite = func(string) error { return sentinel }

	if _, err := loadResponse(id); !errors.Is(err, errResponseStateUnavailable) || !errors.Is(err, sentinel) {
		t.Fatalf("legacy migration durability failure did not fail load closed: %v", err)
	}
}
