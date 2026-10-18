package proxy

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"kiro-go/config"
)

func providerStaleSnapshotTestAccount(t *testing.T, accountID string) config.Account {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{
		ID:         accountID,
		Enabled:    true,
		AuthMethod: "anthropic_api",
		BaseURL:    "https://api.anthropic.com/v1",
		KiroApiKey: "test-key",
	}); err != nil {
		t.Fatal(err)
	}
	account, ok := config.GetAccountByID(accountID)
	if !ok {
		t.Fatalf("account %q not found after add", accountID)
	}
	if account.DispatchGeneration == 0 {
		t.Fatalf("account %q has no lifecycle generation", accountID)
	}
	return account
}

func TestProviderModelIDIsOpaque(t *testing.T) {
	custom := &config.Account{AuthMethod: "anthropic_api", BaseURL: "https://relay.example/v1"}
	if got := claudeDeploymentKeyForAccount(custom, "gpt-4o").ProviderModelID; got != ProviderModelID("gpt-4o") {
		t.Fatalf("provider ID passed through client mapper: %q", got)
	}
	bedrockID := "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	bedrock := &config.Account{AuthMethod: "bedrock"}
	if got := claudeDeploymentKeyForAccount(bedrock, bedrockID).ProviderModelID; got != ProviderModelID(bedrockID) {
		t.Fatalf("Bedrock ID changed: got=%q want=%q", got, bedrockID)
	}
}

func TestSnapshotWithoutRefreshedAtFailsClosed(t *testing.T) {
	now := time.Now()
	snapshot := accountModelSnapshot{
		Models:     []ModelInfo{{ModelId: "claude-sonnet-4.5"}},
		FreshUntil: now.Add(time.Hour),
		StaleUntil: now.Add(24 * time.Hour),
	}
	if snapshotRoutable(snapshot, now) {
		t.Fatal("snapshot without RefreshedAt was routable")
	}
	if got := buildEnabledModelUnion([]string{"a"}, map[string]accountModelSnapshot{"a": snapshot}, nil, now); len(got) != 0 {
		t.Fatalf("unproven snapshot entered public union: %#v", got)
	}
}

func TestFailedRefreshNeverRenewsStaleDeadline(t *testing.T) {
	account := providerStaleSnapshotTestAccount(t, "a")
	fingerprint := accountModelSourceFingerprint(&account)
	key := lifecycleOrLegacyKeyForAccount(&account)
	now := time.Now()
	originalDeadline := now.Add(time.Hour)
	h := &Handler{
		accountModelSnapshots: map[accountLifecycleKey]accountModelSnapshot{
			key: {
				LifecycleGeneration: key.generation,
				Models:              []ModelInfo{{ModelId: "claude-sonnet-4.5"}},
				SourceFingerprint:   fingerprint,
				SourceGeneration:    1,
				RefreshedAt:         now.Add(-time.Hour),
				FreshUntil:          now.Add(-time.Minute),
				StaleUntil:          originalDeadline,
			},
		},
		providerSourceGenerations: map[accountLifecycleKey]uint64{key: 1},
	}
	h.markAccountModelSnapshotStale(&account, fingerprint, 1)
	h.providerStateMu.RLock()
	snapshot := h.accountModelSnapshots[key]
	h.providerStateMu.RUnlock()
	if !snapshot.Stale {
		t.Fatal("failed refresh did not mark the current lifecycle snapshot stale")
	}
	if !snapshot.StaleUntil.Equal(originalDeadline) {
		t.Fatalf("stale deadline renewed: got=%s want=%s", snapshot.StaleUntil, originalDeadline)
	}
}

func TestBedrockConverseOnlyDoesNotAdvertiseThinking(t *testing.T) {
	now := time.Now()
	account := config.Account{ID: "bedrock", Enabled: true, AuthMethod: "bedrock", BedrockUseConverse: true}
	snapshot := accountModelSnapshot{
		Models:      []ModelInfo{{ModelId: "us.anthropic.claude-sonnet-4-20250514-v1:0"}},
		RefreshedAt: now.Add(-time.Minute),
		FreshUntil:  now.Add(time.Hour),
		StaleUntil:  now.Add(24 * time.Hour),
	}
	registry := buildProviderRegistry(1, []config.Account{account}, map[string]accountModelSnapshot{"bedrock": snapshot}, nil, nil, false, now)
	foundExact := false
	for _, public := range registry.PublicModels {
		if public.Model.ModelId != snapshot.Models[0].ModelId {
			continue
		}
		foundExact = true
		if public.AnyThinking {
			t.Fatal("Converse-only Bedrock account advertised -thinking")
		}
	}
	if !foundExact {
		t.Fatalf("exact Bedrock model missing: %#v", registry.PublicModels)
	}
}

func TestBedrockBuiltinAliasRequiresConcreteEvidence(t *testing.T) {
	now := time.Now()
	account := &config.Account{ID: "bedrock", Enabled: true, AuthMethod: "bedrock"}
	withoutTarget := accountModelSnapshot{
		Models:      []ModelInfo{{ModelId: "anthropic.other-model-v1:0"}},
		RefreshedAt: now,
		FreshUntil:  now.Add(time.Hour),
		StaleUntil:  now.Add(time.Hour),
	}
	if _, ok := buildAccountRoutes(account, withoutTarget, now).resolve("claude-sonnet-4-5"); ok {
		t.Fatal("built-in default became provider evidence")
	}
	target := defaultBedrockModelMap["claude-sonnet-4-5"]
	withTarget := withoutTarget
	withTarget.Models = []ModelInfo{{ModelId: target}}
	resolved, ok := buildAccountRoutes(account, withTarget, now).resolve("claude-sonnet-4-5")
	if !ok || string(resolved.ProviderID) != target {
		t.Fatalf("discovered alias did not resolve: %#v ok=%t", resolved, ok)
	}
}

func TestCandidateProviderTargetIsCapturedAcrossRegistryPublication(t *testing.T) {
	h := &Handler{}
	firstRoutes := newProviderRouteBuilder()
	firstRoutes.addExact("provider-v1", providerModelEvidenceDiscovered, ModelReasoningCapability{}, false)
	firstRoutes.addAlias("claude-sonnet-4.5", "provider-v1", providerModelEvidenceUniqueAlias)
	first := &providerRegistrySnapshot{Version: 1, Accounts: map[string]providerAccountAvailability{
		"a": {Routes: firstRoutes.routes},
	}}
	h.providerRegistry.Store(first)
	captured, version, ok := h.resolveProviderModelForAccount("a", "claude-sonnet-4.5")
	if !ok {
		t.Fatal("first target did not resolve")
	}
	secondRoutes := newProviderRouteBuilder()
	secondRoutes.addExact("provider-v2", providerModelEvidenceDiscovered, ModelReasoningCapability{}, false)
	secondRoutes.addAlias("claude-sonnet-4.5", "provider-v2", providerModelEvidenceUniqueAlias)
	h.providerRegistry.Store(&providerRegistrySnapshot{Version: 2, Accounts: map[string]providerAccountAvailability{
		"a": {Routes: secondRoutes.routes},
	}})
	if version != 1 || captured != "provider-v1" {
		t.Fatalf("captured target changed: version=%d target=%q", version, captured)
	}
}

func TestRegistrySchedulesCooldownExpiryAndOmitsCoolingAccount(t *testing.T) {
	now := time.Now()
	account := config.Account{ID: "a", Enabled: true, AuthMethod: "social"}
	snapshot := accountModelSnapshot{
		Models:      []ModelInfo{{ModelId: "claude-sonnet-4.5"}},
		RefreshedAt: now,
		FreshUntil:  now.Add(time.Hour),
		StaleUntil:  now.Add(24 * time.Hour),
	}
	cooldown := now.Add(5 * time.Minute)
	registry := buildProviderRegistry(1, []config.Account{account}, map[string]accountModelSnapshot{"a": snapshot}, map[string]time.Time{"a": cooldown}, nil, false, now)
	if len(registry.PublicModels) != 0 {
		t.Fatalf("cooling account entered public view: %#v", registry.PublicModels)
	}
	if !registry.NextExpiry.Equal(cooldown) {
		t.Fatalf("cooldown expiry not scheduled: got=%s want=%s", registry.NextExpiry, cooldown)
	}
}

func TestQuotaBlockedAccountDoesNotContributePublicCapability(t *testing.T) {
	now := time.Now()
	account := config.Account{
		ID: "a", Enabled: true, AuthMethod: "social",
		UsageCurrent: 100, UsageLimit: 100,
	}
	snapshot := accountModelSnapshot{
		Models:      []ModelInfo{{ModelId: "claude-sonnet-4.5"}},
		RefreshedAt: now,
		FreshUntil:  now.Add(time.Hour),
		StaleUntil:  now.Add(24 * time.Hour),
	}
	registry := buildProviderRegistry(1, []config.Account{account}, map[string]accountModelSnapshot{"a": snapshot}, nil, nil, false, now)
	if len(registry.PublicModels) != 0 || len(registry.Accounts) != 0 {
		t.Fatalf("quota-blocked account contributed registry state: %#v", registry)
	}
}

func TestHandlerCloseStopsProviderExpiryLoop(t *testing.T) {
	h := &Handler{
		stopRefresh:        make(chan struct{}),
		stopStatsSaver:     make(chan struct{}),
		providerExpiryWake: make(chan struct{}, 1),
		providerExpiryStop: make(chan struct{}),
	}
	h.backgroundWG.Add(1)
	go func() {
		defer h.backgroundWG.Done()
		h.runProviderExpiryLoop()
	}()
	h.scheduleProviderModelUnionRebuild(time.Now().Add(time.Hour))
	h.Close()
	if !h.closed.Load() {
		t.Fatal("handler was not marked closed")
	}
}

func TestLogicalAliasRoutesOnlyWhenTargetExistsAndNeverShadowsExactProviderID(t *testing.T) {
	now := time.Now()
	target := MapModel("gpt-4o")
	kiro := &config.Account{ID: "kiro", Enabled: true, AuthMethod: "social"}
	kiroSnapshot := accountModelSnapshot{
		Models:      []ModelInfo{{ModelId: target}},
		RefreshedAt: now,
		FreshUntil:  now.Add(time.Hour),
		StaleUntil:  now.Add(time.Hour),
	}
	alias, ok := buildAccountRoutes(kiro, kiroSnapshot, now).resolve("gpt-4o")
	if !ok || string(alias.ProviderID) != target {
		t.Fatalf("logical alias did not resolve to current target: %#v ok=%t", alias, ok)
	}

	custom := &config.Account{ID: "custom", Enabled: true, AuthMethod: "custom_api"}
	customSnapshot := accountModelSnapshot{
		Models:      []ModelInfo{{ModelId: "gpt-4o"}, {ModelId: target}},
		RefreshedAt: now,
		FreshUntil:  now.Add(time.Hour),
		StaleUntil:  now.Add(time.Hour),
	}
	exact, ok := buildAccountRoutes(custom, customSnapshot, now).resolve("gpt-4o")
	if !ok || exact.ProviderID != ProviderModelID("gpt-4o") {
		t.Fatalf("logical alias shadowed exact provider ID: %#v ok=%t", exact, ok)
	}
}

func TestBedrockExplicitMappingIsOperatorEvidenceWithoutDiscoveryTarget(t *testing.T) {
	now := time.Now()
	account := &config.Account{
		ID: "bedrock", Enabled: true, AuthMethod: "bedrock",
		BedrockModelMap: map[string]string{"private-alias": "profile.operator.target"},
	}
	snapshot := accountModelSnapshot{
		RefreshedAt: now,
		FreshUntil:  now.Add(time.Hour),
		StaleUntil:  now.Add(time.Hour),
	}
	routes := buildAccountRoutes(account, snapshot, now)
	alias, ok := routes.resolve("private-alias")
	if !ok || alias.ProviderID != ProviderModelID("profile.operator.target") {
		t.Fatalf("explicit mapping was not routable: %#v ok=%t", alias, ok)
	}
	if exact, ok := routes.resolve("profile.operator.target"); !ok || exact.ProviderID != alias.ProviderID {
		t.Fatalf("explicit provider target was not first-class: %#v ok=%t", exact, ok)
	}
}

func TestBedrockNegativeCachePreservesDiscoveryFailure(t *testing.T) {
	const accountID = "bedrock-negative-cache-test"
	clearBedrockModelCache(accountID)
	t.Cleanup(func() { clearBedrockModelCache(accountID) })
	setCachedBedrockDiscoveryFailure(accountID, errors.New("list denied"), time.Minute)
	models, cached, err := getCachedBedrockModelsResult(accountID)
	if !cached || err == nil || len(models) != 0 {
		t.Fatalf("negative cache lost failure state: models=%v cached=%t err=%v", models, cached, err)
	}
}

func TestForwardBodyUsesCapturedProviderModelID(t *testing.T) {
	body, err := rewriteForwardBodyModel([]byte(`{"model":"gpt-4o","messages":[]}`), "provider/exact-model")
	if err != nil {
		t.Fatal(err)
	}
	if got := requestedModelFromJSON(body, ""); got != "provider/exact-model" {
		t.Fatalf("forwarded model=%q", got)
	}
}

func TestProviderRouteAmbiguityCannotBeRevivedByLaterInsertion(t *testing.T) {
	builder := newProviderRouteBuilder()
	builder.add("alias", "provider-a", providerModelEvidenceOperatorConfigured, false)
	builder.add("alias", "provider-b", providerModelEvidenceOperatorConfigured, false)
	builder.add("alias", "provider-c", providerModelEvidenceOperatorConfigured, false)
	if _, ok := builder.routes.resolveAlias("alias"); ok {
		t.Fatal("ambiguous alias was revived")
	}
}

func TestRequestedThinkingSuffixIsRemovedOnlyFromRoutingKey(t *testing.T) {
	if got := stripRequestedModelThinkingSuffix("gpt-4o-thinking", "-thinking"); got != "gpt-4o" {
		t.Fatalf("routing key=%q", got)
	}
	if got := stripRequestedModelThinkingSuffix("provider-thinking-v2", "-thinking"); got != "provider-thinking-v2" {
		t.Fatalf("non-suffix provider ID changed: %q", got)
	}
}

func TestResolveCandidateProviderModelPrefersExactRequestedProviderID(t *testing.T) {
	account := &config.Account{ID: "custom", AuthMethod: "custom_api"}
	h := &Handler{}
	routes := newProviderRouteBuilder()
	routes.addExact("gpt-4o", providerModelEvidenceDiscovered, ModelReasoningCapability{}, false)
	routes.addExact(ProviderModelID(MapModel("gpt-4o")), providerModelEvidenceDiscovered, ModelReasoningCapability{}, false)
	h.providerRegistry.Store(&providerRegistrySnapshot{
		Version: 9,
		Accounts: map[string]providerAccountAvailability{
			"custom": {Routes: routes.routes},
		},
	})
	providerID, version, ok := h.resolveCandidateProviderModel(account, "gpt-4o", MapModel("gpt-4o"))
	if !ok || version != 9 || providerID != "gpt-4o" {
		t.Fatalf("providerID=%q version=%d ok=%t", providerID, version, ok)
	}
}

func TestExpiredSnapshotRemainsDiagnosticOnlyAfterFailedRefresh(t *testing.T) {
	account := providerStaleSnapshotTestAccount(t, "a")
	fingerprint := accountModelSourceFingerprint(&account)
	key := lifecycleOrLegacyKeyForAccount(&account)
	now := time.Now()
	h := &Handler{
		accountModelSnapshots: map[accountLifecycleKey]accountModelSnapshot{
			key: {
				LifecycleGeneration: key.generation,
				Models:              []ModelInfo{{ModelId: "model-a"}},
				SourceFingerprint:   fingerprint,
				SourceGeneration:    1,
				RefreshedAt:         now.Add(-25 * time.Hour),
				FreshUntil:          now.Add(-24 * time.Hour),
				StaleUntil:          now.Add(-time.Hour),
			},
		},
		providerSourceGenerations: map[accountLifecycleKey]uint64{key: 1},
	}
	h.markAccountModelSnapshotStale(&account, fingerprint, 1)
	h.providerStateMu.RLock()
	snapshot, exists := h.accountModelSnapshots[key]
	h.providerStateMu.RUnlock()
	if !exists {
		t.Fatal("expired snapshot was deleted instead of retained for diagnostics")
	}
	if !snapshot.Stale {
		t.Fatal("failed refresh did not mark the expired current-lifecycle snapshot stale")
	}
	if snapshotRoutable(snapshot, now) {
		t.Fatal("expired diagnostic snapshot remained routable")
	}
}

func TestExactProviderIDsRemainCaseSensitive(t *testing.T) {
	now := time.Now()
	account := &config.Account{ID: "custom", Enabled: true, AuthMethod: "anthropic_api", BaseURL: "https://relay.example"}
	snapshot := accountModelSnapshot{
		Models:      []ModelInfo{{ModelId: "Model-A"}, {ModelId: "model-a"}},
		RefreshedAt: now,
		FreshUntil:  now.Add(time.Hour),
		StaleUntil:  now.Add(time.Hour),
	}
	routes := buildAccountRoutes(account, snapshot, now)
	upper, upperOK := routes.resolve("Model-A")
	lower, lowerOK := routes.resolve("model-a")
	if !upperOK || !lowerOK || upper.ProviderID == lower.ProviderID {
		t.Fatalf("exact case variants collapsed: upper=%#v lower=%#v", upper, lower)
	}
	if _, ok := routes.resolve("MODEL-A"); ok {
		t.Fatal("ambiguous folded provider ID was routed")
	}
}

func TestFoldedMetadataDoesNotBleedAcrossExactProviderIDs(t *testing.T) {
	metadata := map[string]directAnthropicModelMetadata{
		"Model-A": {ID: "Model-A", CapabilitiesPresent: true, Capabilities: directAnthropicModelCapabilities{Thinking: directAnthropicThinkingCapability{Supported: true}}},
		"model-a": {ID: "model-a", CapabilitiesPresent: true},
	}
	if _, ok := directAnthropicMetadataForProviderID(metadata, "MODEL-A"); ok {
		t.Fatal("ambiguous folded metadata resolved")
	}
	upper, ok := directAnthropicMetadataForProviderID(metadata, "Model-A")
	if !ok || !upper.Capabilities.Thinking.Supported {
		t.Fatalf("exact metadata not preserved: %#v ok=%t", upper, ok)
	}
}

func TestLogicalAliasRegistryContainsDynamicAutoForFirstAvailableModel(t *testing.T) {
	now := time.Now()
	target := "provider-first-model"
	account := config.Account{ID: "a", Enabled: true, AuthMethod: "social"}
	snapshot := accountModelSnapshot{
		Models:      []ModelInfo{{ModelId: target}, {ModelId: "provider-second-model"}},
		RefreshedAt: now,
		FreshUntil:  now.Add(time.Hour),
		StaleUntil:  now.Add(time.Hour),
	}
	routes := buildAccountRoutes(&account, snapshot, now)
	resolved, ok := routes.resolve("auto")
	if !ok || string(resolved.ProviderID) != target {
		t.Fatalf("dynamic auto route=%#v ok=%t", resolved, ok)
	}
	registry := buildProviderRegistry(1, []config.Account{account}, map[string]accountModelSnapshot{"a": snapshot}, nil, nil, false, now)
	found := false
	for _, item := range registry.PublicModels {
		if item.Model.ModelId == "auto" {
			found = true
			if !item.Alias {
				t.Fatal("auto was not marked as a public alias")
			}
		}
	}
	if !found {
		t.Fatalf("auto alias not published for target %q: %#v", target, registry.PublicModels)
	}
}

func TestCapturedRegistryAvailabilityRejectsCooldownAndDenial(t *testing.T) {
	now := time.Now()
	account := &config.Account{ID: "direct", AuthMethod: "anthropic_api", BaseURL: "https://api.anthropic.com"}
	routes := newProviderRouteBuilder()
	routes.addExact("Model-A", providerModelEvidenceDiscovered, ModelReasoningCapability{}, false)
	route, ok := routes.routes.resolve("Model-A")
	if !ok {
		t.Fatal("route missing")
	}
	registry := &providerRegistrySnapshot{Accounts: map[string]providerAccountAvailability{
		"direct": {
			Routes:        routes.routes,
			CooldownUntil: now.Add(time.Minute),
			StaleUntil:    now.Add(time.Hour),
		},
	}}
	if blocked, reason := providerCandidateUnavailable(registry, account, "Model-A", route, now); !blocked || reason != "provider-cooldown" {
		t.Fatalf("cooldown blocked=%t reason=%q", blocked, reason)
	}
	availability := registry.Accounts["direct"]
	availability.CooldownUntil = time.Time{}
	availability.DeniedUntil = map[string]time.Time{exactProviderDenialKey("Model-A"): now.Add(time.Minute)}
	registry.Accounts["direct"] = availability
	if blocked, reason := providerCandidateUnavailable(registry, account, "Model-A", route, now); !blocked || reason != "model-denied" {
		t.Fatalf("denial blocked=%t reason=%q", blocked, reason)
	}
}

func TestTerminalProviderCannotBypassMissingRegistry(t *testing.T) {
	account := &config.Account{ID: "direct", AuthMethod: "anthropic_api", BaseURL: "https://api.anthropic.com"}
	h := &Handler{}
	if providerID, _, ok := h.resolveCandidateProviderModel(account, "claude-opus", "claude-opus"); ok {
		t.Fatalf("terminal provider bypassed missing registry with %q", providerID)
	}
}

func TestKiroStructuredOutputCapabilityIsCapturedPerAccountRoute(t *testing.T) {
	now := time.Now()
	withFormat := &config.Account{ID: "kiro-format", Enabled: true, AuthMethod: "social"}
	withoutFormat := &config.Account{ID: "kiro-no-format", Enabled: true, AuthMethod: "api_key", KiroApiKey: "test"}
	base := func(schema string) accountModelSnapshot {
		return accountModelSnapshot{
			Models: []ModelInfo{{
				ModelId:                            "claude-opus-5",
				AdditionalModelRequestFieldsSchema: []byte(schema),
			}},
			RefreshedAt: now,
			FreshUntil:  now.Add(time.Hour),
			StaleUntil:  now.Add(2 * time.Hour),
		}
	}

	formatRoute, ok := buildAccountRoutes(withFormat, base(`{"type":"object","properties":{"output_config":{"type":"object","properties":{"format":{"type":"object"}}}}}`), now).resolve("claude-opus-5")
	if !ok || !formatRoute.KiroStructuredOutputKnown || !formatRoute.KiroStructuredOutputFormat {
		t.Fatalf("format-capable route lost exact evidence: %#v ok=%t", formatRoute, ok)
	}

	noFormatRoute, ok := buildAccountRoutes(withoutFormat, base(`{"type":"object","properties":{"output_config":{"type":"object","properties":{"effort":{"type":"string"}}}}}`), now).resolve("claude-opus-5")
	if !ok || !noFormatRoute.KiroStructuredOutputKnown || noFormatRoute.KiroStructuredOutputFormat {
		t.Fatalf("narrower route borrowed capability: %#v ok=%t", noFormatRoute, ok)
	}
}

func TestKiroStructuredOutputConflictingDuplicateEvidenceFailsClosed(t *testing.T) {
	now := time.Now()
	account := &config.Account{ID: "kiro-conflict", Enabled: true, AuthMethod: "social"}
	snapshot := accountModelSnapshot{
		Models: []ModelInfo{
			{
				ModelId:                            "claude-opus-5",
				AdditionalModelRequestFieldsSchema: []byte(`{"type":"object","properties":{"output_config":{"type":"object","properties":{"format":{"type":"object"}}}}}`),
			},
			{
				ModelId:                            "claude-opus-5",
				AdditionalModelRequestFieldsSchema: []byte(`{"type":"object","properties":{"output_config":{"type":"object","properties":{"effort":{"type":"string"}}}}}`),
			},
			{
				ModelId:                            "claude-opus-5",
				AdditionalModelRequestFieldsSchema: []byte(`{"type":"object","properties":{"output_config":{"type":"object","properties":{"format":{"type":"object"}}}}}`),
			},
		},
		RefreshedAt: now,
		FreshUntil:  now.Add(time.Hour),
		StaleUntil:  now.Add(2 * time.Hour),
	}
	route, ok := buildAccountRoutes(account, snapshot, now).resolve("claude-opus-5")
	if !ok {
		t.Fatal("route missing")
	}
	if route.KiroStructuredOutputKnown || route.KiroStructuredOutputFormat || !route.KiroStructuredOutputConflict {
		t.Fatalf("conflicting duplicate evidence was treated as capability proof: %#v", route)
	}
}
