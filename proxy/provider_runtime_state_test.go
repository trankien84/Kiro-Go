package proxy

import (
	"testing"
	"time"

	"kiro-go/config"
)

func freshProviderSnapshot(models []ModelInfo, metadata map[string]directAnthropicModelMetadata) accountModelSnapshot {
	now := time.Now()
	return accountModelSnapshot{
		Models:      models,
		Metadata:    metadata,
		RefreshedAt: now,
		FreshUntil:  now.Add(time.Hour),
		StaleUntil:  now.Add(24 * time.Hour),
	}
}
func testRuntimeKey(accountID string) accountLifecycleKey {
	generation, _ := config.GetAccountLifecycleGeneration(accountID)
	if generation == 0 {
		return legacyBedrockLifecycleKey(accountID)
	}
	return accountLifecycleKey{accountID: accountID, generation: generation}
}

func testRuntimeAccount(accountID string) *config.Account {
	key := testRuntimeKey(accountID)
	return &config.Account{ID: accountID, DispatchGeneration: key.generation}
}

func testRuntimeSnapshots(input map[string]accountModelSnapshot) map[accountLifecycleKey]accountModelSnapshot {
	out := make(map[accountLifecycleKey]accountModelSnapshot, len(input))
	for accountID, snapshot := range input {
		key := testRuntimeKey(accountID)
		snapshot.LifecycleGeneration = key.generation
		out[key] = snapshot
	}
	return out
}

func testRuntimeCooldowns(input map[string]time.Time) map[accountLifecycleKey]time.Time {
	out := make(map[accountLifecycleKey]time.Time, len(input))
	for accountID, until := range input {
		out[testRuntimeKey(accountID)] = until
	}
	return out
}

func testRuntimeDenials(input map[string]map[string]time.Time) map[accountLifecycleKey]map[string]time.Time {
	out := make(map[accountLifecycleKey]map[string]time.Time, len(input))
	for accountID, denied := range input {
		out[testRuntimeKey(accountID)] = denied
	}
	return out
}

func testRuntimeSourceGenerations(input map[string]uint64) map[accountLifecycleKey]uint64 {
	out := make(map[accountLifecycleKey]uint64, len(input))
	for accountID, generation := range input {
		out[testRuntimeKey(accountID)] = generation
	}
	return out
}

func TestBuildEnabledModelUnionUsesEnabledSnapshotsOnly(t *testing.T) {
	now := time.Now()
	snapshots := map[string]accountModelSnapshot{
		"enabled": freshProviderSnapshot([]ModelInfo{
			{ModelId: "model-b", ModelName: "B"},
			{ModelId: "model-c", ModelName: "C"},
		}, nil),
		"disabled": freshProviderSnapshot([]ModelInfo{
			{ModelId: "model-a", ModelName: "A"},
		}, nil),
	}

	union := buildEnabledModelUnion(
		[]string{"enabled"},
		snapshots,
		nil,
		now,
	)
	if len(union) != 2 || union[0].ModelId != "model-b" || union[1].ModelId != "model-c" {
		t.Fatalf("union=%#v", union)
	}
}

func TestBuildEnabledModelUnionExpiresStaleSnapshots(t *testing.T) {
	now := time.Now()
	snapshots := map[string]accountModelSnapshot{
		"fresh": {
			Models:      []ModelInfo{{ModelId: "fresh-model"}},
			RefreshedAt: now.Add(-time.Minute),
			FreshUntil:  now.Add(time.Minute),
			StaleUntil:  now.Add(time.Hour),
		},
		"expired": {
			Models:      []ModelInfo{{ModelId: "expired-model"}},
			RefreshedAt: now.Add(-25 * time.Hour),
			FreshUntil:  now.Add(-2 * time.Hour),
			StaleUntil:  now.Add(-time.Minute),
			Stale:       true,
		},
	}
	union := buildEnabledModelUnion([]string{"fresh", "expired"}, snapshots, nil, now)
	if len(union) != 1 || union[0].ModelId != "fresh-model" {
		t.Fatalf("expired snapshot entered public union: %#v", union)
	}
}

func TestProviderCooldownAndVerifiedModelDenialAreScoped(t *testing.T) {
	h := &Handler{
		providerCooldownUntil:    make(map[accountLifecycleKey]time.Time),
		providerModelDeniedUntil: make(map[accountLifecycleKey]map[string]time.Time),
	}

	h.setProviderModelDenied("account-a", "claude-a", time.Minute)
	if blocked, _ := h.providerAccountBlocked("account-a", "claude-a"); !blocked {
		t.Fatal("verified model-scoped denial was ignored")
	}
	if blocked, reason := h.providerAccountBlocked("account-a", "claude-b"); blocked {
		t.Fatalf("model denial leaked to another model: %s", reason)
	}

	h.setProviderCooldown("account-a", time.Minute)
	if blocked, _ := h.providerAccountBlocked("account-a", "claude-b"); !blocked {
		t.Fatal("account cooldown was ignored")
	}
	if blocked, reason := h.providerAccountBlocked("account-b", "claude-a"); blocked {
		t.Fatalf("cooldown leaked to another account: %s", reason)
	}
}

func TestDirectAnthropicCapabilityIsDeploymentScoped(t *testing.T) {
	h := &Handler{
		accountModelSnapshots: testRuntimeSnapshots(map[string]accountModelSnapshot{
			"account-a": freshProviderSnapshot(nil, map[string]directAnthropicModelMetadata{
				"claude-shared": {
					ID:                  "claude-shared",
					CapabilitiesPresent: true,
					Capabilities: directAnthropicModelCapabilities{
						Thinking: directAnthropicThinkingCapability{
							Supported: true,
							Types: directAnthropicThinkingTypes{
								Adaptive: directAnthropicCapabilitySupport{Supported: true},
								Enabled:  directAnthropicCapabilitySupport{Supported: true},
							},
						},
					},
				},
			}),
			"account-b": freshProviderSnapshot(nil, map[string]directAnthropicModelMetadata{
				"claude-shared": {
					ID:                  "claude-shared",
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
			}),
		}),
	}
	accountA := &config.Account{
		ID:         "account-a",
		Enabled:    true,
		AuthMethod: "anthropic_api",
		BaseURL:    "https://provider-a.example/v1",
	}
	accountB := &config.Account{
		ID:         "account-b",
		Enabled:    true,
		AuthMethod: "anthropic_api",
		BaseURL:    "https://provider-b.example/v1",
	}
	capA, knownA, deploymentA := h.directAnthropicReasoningCapabilityForAccount(accountA, "claude-shared")
	capB, knownB, deploymentB := h.directAnthropicReasoningCapabilityForAccount(accountB, "claude-shared")
	if !knownA || !knownB {
		t.Fatalf("deployment capability unexpectedly unknown: A=%v B=%v", knownA, knownB)
	}
	if deploymentA == deploymentB {
		t.Fatalf("different endpoints collapsed into one deployment: %#v", deploymentA)
	}
	if _, ok := matchThinkingType("enabled", capA.ThinkingTypes); !ok {
		t.Fatalf("account A lost its deployment capability: %#v", capA)
	}
	if _, ok := matchThinkingType("enabled", capB.ThinkingTypes); ok {
		t.Fatalf("account B borrowed another deployment capability: %#v", capB)
	}
}

func TestFreshestDirectAnthropicDeploymentMetadataIsSharedByDeployment(t *testing.T) {
	now := time.Now()
	accounts := []config.Account{
		{ID: "older", Enabled: true, AuthMethod: "anthropic_api", BaseURL: "https://api.anthropic.com/v1"},
		{ID: "newer", Enabled: true, AuthMethod: "anthropic_api", BaseURL: "https://API.ANTHROPIC.COM"},
		{ID: "other", Enabled: true, AuthMethod: "anthropic_api", BaseURL: "https://compatible.example/v1"},
	}
	metadata := func(enabled bool) directAnthropicModelMetadata {
		return directAnthropicModelMetadata{
			ID:                  "claude-opus-4.8",
			CapabilitiesPresent: true,
			Capabilities: directAnthropicModelCapabilities{
				Thinking: directAnthropicThinkingCapability{
					Supported: true,
					Types: directAnthropicThinkingTypes{
						Adaptive: directAnthropicCapabilitySupport{Supported: true},
						Enabled:  directAnthropicCapabilitySupport{Supported: enabled},
					},
				},
			},
		}
	}
	snapshots := testRuntimeSnapshots(map[string]accountModelSnapshot{
		"older": {
			Metadata:    map[string]directAnthropicModelMetadata{"claude-opus-4.8": metadata(true)},
			RefreshedAt: now.Add(-time.Hour),
			FreshUntil:  now.Add(time.Hour),
			StaleUntil:  now.Add(2 * time.Hour),
		},
		"newer": {
			Metadata:    map[string]directAnthropicModelMetadata{"claude-opus-4.8": metadata(false)},
			RefreshedAt: now,
			FreshUntil:  now.Add(time.Hour),
			StaleUntil:  now.Add(2 * time.Hour),
		},
		"other": {
			Metadata:    map[string]directAnthropicModelMetadata{"claude-opus-4.8": metadata(true)},
			RefreshedAt: now.Add(time.Minute),
			FreshUntil:  now.Add(time.Hour),
			StaleUntil:  now.Add(2 * time.Hour),
		},
	})
	target := claudeDeploymentKeyForAccount(&accounts[0], "claude-opus-4.8")
	got, ok := freshestDirectAnthropicDeploymentMetadata(
		target,
		"claude-opus-4.8",
		accounts,
		snapshots,
		now,
	)
	if !ok {
		t.Fatal("same-deployment metadata was not found")
	}
	if got.Capabilities.Thinking.Types.Enabled.Supported {
		t.Fatalf("older or foreign deployment metadata won: %#v", got)
	}
}

func TestOfficialDirectAnthropicMergesDocumentedDisabledMode(t *testing.T) {
	h := &Handler{
		accountModelSnapshots: testRuntimeSnapshots(map[string]accountModelSnapshot{
			"official": freshProviderSnapshot(nil, map[string]directAnthropicModelMetadata{
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
			}),
		}),
	}
	account := &config.Account{
		ID:         "official",
		Enabled:    true,
		AuthMethod: "anthropic_api",
		BaseURL:    "https://api.anthropic.com/v1",
	}
	capability, known, deployment := h.directAnthropicReasoningCapabilityForAccount(account, "claude-opus-4.8")
	if !known || deployment.Profile != claudeProviderAnthropicOfficial {
		t.Fatalf("official deployment not recognized: known=%v deployment=%#v", known, deployment)
	}
	if _, ok := matchThinkingType("adaptive", capability.ThinkingTypes); !ok {
		t.Fatalf("live adaptive capability missing: %#v", capability)
	}
	if _, ok := matchThinkingType("disabled", capability.ThinkingTypes); !ok {
		t.Fatalf("documented disabled mode missing: %#v", capability)
	}
}

func TestStaleCompatibleMetadataDoesNotCreateHardCapabilityDecision(t *testing.T) {
	now := time.Now()
	h := &Handler{
		accountModelSnapshots: testRuntimeSnapshots(map[string]accountModelSnapshot{
			"compatible": {
				Metadata: map[string]directAnthropicModelMetadata{
					"private-model": {
						ID:                  "private-model",
						CapabilitiesPresent: true,
						Capabilities: directAnthropicModelCapabilities{
							Thinking: directAnthropicThinkingCapability{Supported: false},
						},
					},
				},
				FreshUntil: now.Add(-time.Minute),
				StaleUntil: now.Add(time.Hour),
				Stale:      true,
			},
		}),
	}
	account := &config.Account{
		ID:         "compatible",
		Enabled:    true,
		AuthMethod: "anthropic_api",
		BaseURL:    "https://compatible.example/v1",
	}
	capability, known, _ := h.directAnthropicReasoningCapabilityForAccount(account, "private-model")
	if known || capability.ModelID != "" {
		t.Fatalf("stale metadata created hard-negative capability: %#v", capability)
	}
}

func TestFreshCompatiblePartialCapabilitiesRemainUnknown(t *testing.T) {
	h := &Handler{
		accountModelSnapshots: testRuntimeSnapshots(map[string]accountModelSnapshot{
			"compatible": freshProviderSnapshot(nil, map[string]directAnthropicModelMetadata{
				"private-model": {
					ID:                  "private-model",
					CapabilitiesPresent: true,
					Capabilities: directAnthropicModelCapabilities{
						ImageInput: directAnthropicCapabilitySupport{Supported: true},
					},
				},
			}),
		}),
	}
	account := &config.Account{
		ID:         "compatible",
		Enabled:    true,
		AuthMethod: "anthropic_api",
		BaseURL:    "https://compatible.example/v1",
	}
	capability, known, _ := h.directAnthropicReasoningCapabilityForAccount(account, "private-model")
	if known || capability.ModelID != "" {
		t.Fatalf("partial compatible metadata created hard-negative reasoning: %#v", capability)
	}
}

func TestHandleDirectAnthropicPermissionFailureIsRequestLocal(t *testing.T) {
	h := &Handler{
		providerCooldownUntil:    make(map[accountLifecycleKey]time.Time),
		providerModelDeniedUntil: make(map[accountLifecycleKey]map[string]time.Time),
	}
	account := &config.Account{ID: "account-a", AuthMethod: "anthropic_api", Enabled: true}
	h.handleDirectAnthropicFailure(account, "claude-a", &directAnthropicResponseError{
		Scope: directAnthropicErrorPermission,
	})
	if blocked, reason := h.providerAccountBlocked(account.ID, "claude-a"); blocked {
		t.Fatalf("unverified 403 persisted as model denial: %s", reason)
	}
}

func TestCanonicalDirectAnthropicOriginSeparatesDeployments(t *testing.T) {
	first := canonicalDirectAnthropicOrigin("HTTPS://API.ANTHROPIC.COM/v1/")
	second := canonicalDirectAnthropicOrigin("https://api.anthropic.com")
	defaultPort := canonicalDirectAnthropicOrigin("https://api.anthropic.com:443/v1")
	trailingDot := canonicalDirectAnthropicOrigin("https://api.anthropic.com./v1")
	if first != second || first != defaultPort || first != trailingDot || first != "https://api.anthropic.com" {
		t.Fatalf(
			"official origins not canonicalized: first=%q second=%q defaultPort=%q trailingDot=%q",
			first,
			second,
			defaultPort,
			trailingDot,
		)
	}
	compatible := canonicalDirectAnthropicOrigin("https://provider.example/prefix/v1")
	if compatible != "https://provider.example/prefix" {
		t.Fatalf("compatible prefix lost: %q", compatible)
	}
}

func TestOfficialAnthropicProfileRequiresExactHTTPSOrigin(t *testing.T) {
	tests := []struct {
		baseURL string
		want    claudeProviderProfile
	}{
		{"https://api.anthropic.com", claudeProviderAnthropicOfficial},
		{"https://API.ANTHROPIC.COM/v1/", claudeProviderAnthropicOfficial},
		{"https://api.anthropic.com:443/v1", claudeProviderAnthropicOfficial},
		{"http://api.anthropic.com/v1", claudeProviderAnthropicCompatible},
		{"https://api.anthropic.com:8443/v1", claudeProviderAnthropicCompatible},
		{"https://api.anthropic.com/prefix/v1", claudeProviderAnthropicCompatible},
		{"https://user@api.anthropic.com/v1", claudeProviderAnthropicCompatible},
		{"https://api.anthropic.com/v1?tenant=x", claudeProviderAnthropicCompatible},
		{"https://api.anthropic.com/v1#fragment", claudeProviderAnthropicCompatible},
	}
	for _, test := range tests {
		account := &config.Account{AuthMethod: "anthropic_api", BaseURL: test.baseURL}
		if got := claudeProviderProfileForAccount(account); got != test.want {
			t.Fatalf("baseURL=%q profile=%s want=%s", test.baseURL, got, test.want)
		}
	}
}

func TestAccountReadyForModelDiscoveryByKind(t *testing.T) {
	tests := []struct {
		name    string
		account config.Account
		want    bool
	}{
		{name: "disabled", account: config.Account{AuthMethod: "anthropic_api", BaseURL: "https://example.com", KiroApiKey: "key"}},
		{name: "direct", account: config.Account{Enabled: true, AuthMethod: "anthropic_api", BaseURL: "https://example.com", KiroApiKey: "key"}, want: true},
		{name: "relay", account: config.Account{Enabled: true, AuthMethod: "custom_api", BaseURL: "https://example.com", KiroApiKey: "key"}, want: true},
		{name: "bedrock api key", account: config.Account{Enabled: true, AuthMethod: "bedrock", BedrockAPIKey: "key"}, want: true},
		{name: "bedrock sigv4", account: config.Account{Enabled: true, AuthMethod: "bedrock", BedrockAccessKeyID: "id", BedrockSecretAccessKey: "secret"}, want: true},
		{name: "oauth", account: config.Account{Enabled: true, AuthMethod: "social", AccessToken: "token"}, want: true},
		{name: "unknown", account: config.Account{Enabled: true, AuthMethod: "future", AccessToken: "token"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := accountReadyForModelDiscovery(&tt.account); got != tt.want {
				t.Fatalf("ready=%v want=%v account=%#v", got, tt.want, tt.account)
			}
		})
	}
}

func TestRemoveAccountProviderRuntimeStateClearsSnapshotsAndBlocks(t *testing.T) {
	h := &Handler{
		accountModelSnapshots: testRuntimeSnapshots(map[string]accountModelSnapshot{
			"account-a": {Models: []ModelInfo{{ModelId: "claude-a"}}},
		}),
		providerCooldownUntil: testRuntimeCooldowns(map[string]time.Time{
			"account-a": time.Now().Add(time.Minute),
		}),
		providerModelDeniedUntil: testRuntimeDenials(map[string]map[string]time.Time{
			"account-a": {"claude-a": time.Now().Add(time.Minute)},
		}),
	}
	h.clearAccountProviderRuntimeState("account-a")
	if _, ok := h.accountModelSnapshots[testRuntimeKey("account-a")]; ok {
		t.Fatal("model snapshot survived account removal")
	}
	if _, ok := h.providerCooldownUntil[testRuntimeKey("account-a")]; ok {
		t.Fatal("cooldown survived account removal")
	}
	if _, ok := h.providerModelDeniedUntil[testRuntimeKey("account-a")]; ok {
		t.Fatal("model denial survived account removal")
	}
	if got := h.providerSourceGeneration(testRuntimeAccount("account-a")); got != 1 {
		t.Fatalf("source generation=%d want=1", got)
	}
}

func TestBuildEnabledModelUnionPreservesConfiguredAccountOrder(t *testing.T) {
	now := time.Now()
	snapshots := map[string]accountModelSnapshot{
		"account-a": freshProviderSnapshot([]ModelInfo{{ModelId: "model-a"}}, nil),
		"account-b": freshProviderSnapshot([]ModelInfo{{ModelId: "model-b"}}, nil),
	}
	union := buildEnabledModelUnion([]string{"account-b", "account-a"}, snapshots, nil, now)
	if len(union) != 2 || union[0].ModelId != "model-b" || union[1].ModelId != "model-a" {
		t.Fatalf("union order=%#v", union)
	}
}

func TestAccountModelSourceFingerprintChangesOnlyForRoutingSource(t *testing.T) {
	base := config.Account{
		ID:         "account-a",
		Enabled:    true,
		AuthMethod: "anthropic_api",
		BaseURL:    "https://provider.example/v1",
		KiroApiKey: "key-a",
		Nickname:   "before",
	}
	first := accountModelSourceFingerprint(&base)
	base.Nickname = "after"
	if got := accountModelSourceFingerprint(&base); got != first {
		t.Fatal("non-routing metadata changed model source fingerprint")
	}
	base.KiroApiKey = "key-b"
	if got := accountModelSourceFingerprint(&base); got == first {
		t.Fatal("credential change did not invalidate model source fingerprint")
	}
}

func TestBuildEnabledModelUnionExcludesVerifiedActiveModelDenials(t *testing.T) {
	now := time.Now()
	snapshots := map[string]accountModelSnapshot{
		"account-a": freshProviderSnapshot([]ModelInfo{{ModelId: "shared"}, {ModelId: "only-a"}}, nil),
		"account-b": freshProviderSnapshot([]ModelInfo{{ModelId: "shared"}, {ModelId: "only-b"}}, nil),
	}
	denied := map[string]map[string]time.Time{
		"account-a": {
			"shared": now.Add(time.Minute),
			"only-a": now.Add(time.Minute),
		},
	}
	union := buildEnabledModelUnion([]string{"account-a", "account-b"}, snapshots, denied, now)
	if len(union) != 2 || union[0].ModelId != "shared" || union[1].ModelId != "only-b" {
		t.Fatalf("union with account-scoped denial=%#v", union)
	}
}

func TestProviderRuntimeDiagnosticsDoesNotExposeExpiredBlocks(t *testing.T) {
	now := time.Now()
	h := &Handler{
		accountModelSnapshots: testRuntimeSnapshots(map[string]accountModelSnapshot{
			"account-a": {
				Models:      []ModelInfo{{ModelId: "model-a"}},
				RefreshedAt: now,
				FreshUntil:  now.Add(-time.Minute),
				StaleUntil:  now.Add(time.Hour),
				Stale:       true,
			},
		}),
		providerCooldownUntil: testRuntimeCooldowns(map[string]time.Time{"account-a": now.Add(time.Minute)}),
		providerModelDeniedUntil: testRuntimeDenials(map[string]map[string]time.Time{
			"account-a": {
				"active":  now.Add(time.Minute),
				"expired": now.Add(-time.Minute),
			},
		}),
		providerSourceGenerations: testRuntimeSourceGenerations(map[string]uint64{"account-a": 3}),
	}
	got := h.providerRuntimeDiagnostics("account-a")
	if got.ModelCount != 1 || !got.ModelSnapshotStale || got.CooldownUntil == 0 || got.SourceGeneration != 3 {
		t.Fatalf("diagnostics=%#v", got)
	}
	if len(got.ModelDeniedUntil) != 1 || got.ModelDeniedUntil["active"] == 0 {
		t.Fatalf("model denials=%#v", got.ModelDeniedUntil)
	}
}

func TestDirectAnthropicDiscoveryFailureUsesProviderPolicy(t *testing.T) {
	h := &Handler{
		providerCooldownUntil:    make(map[accountLifecycleKey]time.Time),
		providerModelDeniedUntil: make(map[accountLifecycleKey]map[string]time.Time),
	}
	account := &config.Account{ID: "account-a", AuthMethod: "anthropic_api", Enabled: true}

	h.handleDirectAnthropicDiscoveryFailure(account, &directAnthropicResponseError{
		Scope:      directAnthropicErrorRateLimit,
		RetryAfter: 3 * time.Minute,
	})
	if blocked, reason := h.providerAccountBlocked(account.ID, "claude-a"); !blocked || reason == "" {
		t.Fatalf("rate-limit discovery failure did not cooldown account: blocked=%v reason=%q", blocked, reason)
	}

	h.providerStateMu.Lock()
	delete(h.providerCooldownUntil, testRuntimeKey(account.ID))
	h.providerStateMu.Unlock()
	h.handleDirectAnthropicDiscoveryFailure(account, &directAnthropicResponseError{
		Scope: directAnthropicErrorPermission,
	})
	if blocked, reason := h.providerAccountBlocked(account.ID, "claude-a"); blocked {
		t.Fatalf("models permission failure globally blocked messages: %s", reason)
	}
}

func TestRebuildEnabledModelUnionPrunesExpiredProviderBlocks(t *testing.T) {
	h := &Handler{
		accountModelSnapshots:     make(map[accountLifecycleKey]accountModelSnapshot),
		providerCooldownUntil:     testRuntimeCooldowns(map[string]time.Time{"account-a": time.Now().Add(-time.Minute)}),
		providerModelDeniedUntil:  testRuntimeDenials(map[string]map[string]time.Time{"account-a": {"claude-a": time.Now().Add(-time.Minute)}}),
		providerSourceGenerations: make(map[accountLifecycleKey]uint64),
	}
	h.rebuildEnabledModelUnion()
	if len(h.providerCooldownUntil) != 0 || len(h.providerModelDeniedUntil) != 0 {
		t.Fatalf("expired runtime blocks survived rebuild: cooldown=%#v denied=%#v", h.providerCooldownUntil, h.providerModelDeniedUntil)
	}
}

func TestCredentialFailureDropsLearnedModelSnapshot(t *testing.T) {
	h := &Handler{
		accountModelSnapshots: testRuntimeSnapshots(map[string]accountModelSnapshot{
			"account-a": {Models: []ModelInfo{{ModelId: "claude-a"}}},
		}),
		providerCooldownUntil:     make(map[accountLifecycleKey]time.Time),
		providerModelDeniedUntil:  make(map[accountLifecycleKey]map[string]time.Time),
		providerSourceGenerations: make(map[accountLifecycleKey]uint64),
	}
	h.handleDirectAnthropicFailure(
		&config.Account{ID: "account-a", AuthMethod: "anthropic_api", Enabled: true},
		"claude-a",
		&directAnthropicResponseError{Scope: directAnthropicErrorCredential},
	)
	if _, ok := h.accountModelSnapshots[testRuntimeKey("account-a")]; ok {
		t.Fatal("credential failure retained learned provider models")
	}
	if h.providerSourceGeneration(testRuntimeAccount("account-a")) != 1 {
		t.Fatal("credential failure did not invalidate in-flight model refreshes")
	}
}
