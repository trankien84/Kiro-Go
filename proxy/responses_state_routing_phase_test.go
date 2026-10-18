package proxy

import (
	"encoding/json"
	"errors"
	"kiro-go/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResponseStateDependencyMatrixKeepsStatelessAndExternalPathsAvailable(t *testing.T) {
	resetResponseStateHealthForTest()
	t.Cleanup(resetResponseStateHealthForTest)

	sentinel := errors.New("state durability unknown")
	poisonResponseState(sentinel)

	transientID, err := generateResponseIDForStore(false)
	if err != nil {
		t.Fatalf("store=false id generation depended on poisoned durable state: %v", err)
	}
	if classifyResponseID(transientID) != responseIDTransientLocal {
		t.Fatalf("transient response id was not syntactically reserved: %q", transientID)
	}
	if _, err := generateResponseIDForStore(true); !errors.Is(err, errResponseStateUnavailable) {
		t.Fatalf("store=true did not fail closed on poisoned state: %v", err)
	}

	ownership, err := resolveResponseStateOwnership("resp_upstream_opaque_123")
	if err != nil || ownership != responseStateExternal {
		t.Fatalf("opaque external previous_response_id touched poisoned local state: ownership=%v err=%v", ownership, err)
	}
	ownership, err = resolveResponseStateOwnership(transientID)
	if ownership != responseStateLocalMissing || !errors.Is(err, errStoredResponseNotFound) {
		t.Fatalf("transient local id could be relayed or replayed: ownership=%v err=%v", ownership, err)
	}
	ownership, err = resolveResponseStateOwnership(generateResponseIDForNamespace(strings.Repeat("a", responseInstanceNamespaceSize*2)))
	if ownership != responseStateUnavailable || !errors.Is(err, errResponseStateUnavailable) {
		t.Fatalf("durable local candidate bypassed poisoned state: ownership=%v err=%v", ownership, err)
	}
	ownership, err = resolveResponseStateOwnership("resp_kiro_legacy_ambiguous")
	if ownership != responseStateUnavailable || !errors.Is(err, errResponseStateUnavailable) {
		t.Fatalf("legacy ambiguous id guessed external while local state was unavailable: ownership=%v err=%v", ownership, err)
	}
}

func TestResponseIDClassifierIsPureAndReservesTransientNamespace(t *testing.T) {
	cases := []struct {
		id   string
		want responseIDClass
	}{
		{"", responseIDNone},
		{"resp_external_123", responseIDExternal},
		{"resp_0123456789abcdef0123456789abcdef", responseIDLegacyAmbiguous},
		{"resp_1700000000000000000000000000000", responseIDLegacyAmbiguous},
		{"resp_0123456789ABCDEF0123456789ABCDEF", responseIDExternal},
		{"resp_not-a-kiro-id", responseIDExternal},
		{transientLocalResponseIDPrefix + "abc", responseIDTransientLocal},
		{generateResponseIDForNamespace(strings.Repeat("b", responseInstanceNamespaceSize*2)), responseIDNamespacedLocal},
		{"resp_kiro_legacy", responseIDLegacyAmbiguous},
	}
	for _, tc := range cases {
		if got := classifyResponseID(tc.id); got != tc.want {
			t.Fatalf("classifyResponseID(%q)=%v want=%v", tc.id, got, tc.want)
		}
	}
}

func TestHistoricalPrePrefixResponseIDGrammar(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"resp_0123456789abcdef0123456789abcdef", true},
		{"resp_1700000000000000000000000000000", true},
		{"resp_0123456789ABCDEF0123456789ABCDEF", false},
		{"resp_0123456789abcdef0123456789abcde", false},
		{"resp_0" + strings.Repeat("1", 18) + strings.Repeat("0", 12), false},
		{"resp_1700000000000000000000000000001", false},
		{"resp_upstream_opaque_123", false},
	}
	for _, tc := range cases {
		if got := isHistoricalPrePrefixResponseID(tc.id); got != tc.want {
			t.Fatalf("isHistoricalPrePrefixResponseID(%q)=%t want=%t", tc.id, got, tc.want)
		}
	}
}

func TestHistoricalPrePrefixOwnershipUsesExactLocalState(t *testing.T) {
	resetResponseStateHealthForTest()
	t.Cleanup(resetResponseStateHealthForTest)

	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if err := ensureResponsesDir(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{
		"resp_0123456789abcdef0123456789abcdef",
		"resp_1700000000000000000000000000000",
	} {
		// Reproduce the pre-hashed on-disk layout that installations using the
		// original resp_* generator can still carry across an upgrade. Version 0
		// is intentionally used because those files predate versioned state.
		doc := storedResponseDoc{
			ID:          id,
			Object:      "response",
			Status:      "completed",
			Model:       "test-model",
			StoredInput: json.RawMessage(`"hello"`),
			StoredAt:    now,
		}
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		legacyPath := legacyResponseStatePath(id)
		if legacyPath == "" {
			t.Fatalf("historical id %q has no legacy path", id)
		}
		if err := os.WriteFile(legacyPath, data, 0o600); err != nil {
			t.Fatalf("write historical response %q: %v", id, err)
		}

		ownership, err := resolveResponseStateOwnership(id)
		if err != nil || ownership != responseStateLocal {
			t.Fatalf("historical local response %q ownership=%v err=%v", id, ownership, err)
		}
		custom := &config.Account{AuthMethod: "custom_api"}
		transport, _ := responsesTransportForAccountWithOwnership(
			custom, &ResponsesRequest{}, responsesToolChoicePlan{Kind: responsesToolChoiceAuto}, ownership, responsesSemanticRequirements{},
		)
		if transport != responsesTransportUnsupported {
			t.Fatalf("historical local response %q was relay-eligible: transport=%v", id, transport)
		}
		if _, err := os.Stat(responseStatePath(id)); err != nil {
			t.Fatalf("historical response %q was not migrated to exact-ID storage: %v", id, err)
		}
	}

	missing := "resp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ownership, err := resolveResponseStateOwnership(missing)
	if err != nil || ownership != responseStateExternal {
		t.Fatalf("historical grammar miss must remain relayable: ownership=%v err=%v", ownership, err)
	}
	custom := &config.Account{AuthMethod: "custom_api"}
	transport, _ := responsesTransportForAccountWithOwnership(
		custom, &ResponsesRequest{}, responsesToolChoicePlan{Kind: responsesToolChoiceAuto}, ownership, responsesSemanticRequirements{},
	)
	if transport != responsesTransportCustomRelay {
		t.Fatalf("historical grammar miss lost transparent relay: transport=%v", transport)
	}

	poisonResponseState(errors.New("durability unknown"))
	if ownership, err := resolveResponseStateOwnership(missing); ownership != responseStateUnavailable || !errors.Is(err, errResponseStateUnavailable) {
		t.Fatalf("historical ambiguous id guessed external while state unavailable: ownership=%v err=%v", ownership, err)
	}
}

func TestResponseStateSecurityFailurePoisonsOnlyLocalStateDependency(t *testing.T) {
	resetResponseStateHealthForTest()
	t.Cleanup(resetResponseStateHealthForTest)

	oldEnsure := ensureResponseStateOwnerPrivateForRead
	t.Cleanup(func() { ensureResponseStateOwnerPrivateForRead = oldEnsure })
	sentinel := errors.New("cannot harden state ACL")
	ensureResponseStateOwnerPrivateForRead = func(string) error { return sentinel }

	path := filepath.Join(t.TempDir(), "response.json")
	if _, err := readStoredResponseDoc(path, "resp_local"); !errors.Is(err, errResponseStateUnavailable) || !errors.Is(err, sentinel) {
		t.Fatalf("state security migration failure did not poison local store: %v", err)
	}
	ownership, err := resolveResponseStateOwnership("resp_upstream_opaque")
	if err != nil || ownership != responseStateExternal {
		t.Fatalf("poisoned local state leaked into definitely-external routing: ownership=%v err=%v", ownership, err)
	}
}
