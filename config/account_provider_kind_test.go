package config

import "testing"

func TestAccountProviderKindLifecyclePolicy(t *testing.T) {
	tests := []struct {
		name             string
		authMethod       string
		kind             AccountKind
		directAnthropic  bool
		staticCredential bool
		kiroRuntime      bool
		kiroRefresh      bool
		kiroAccountInfo  bool
		overage          bool
		known            bool
	}{
		{name: "social", authMethod: "social", kind: AccountKindKiroOAuth, kiroRuntime: true, kiroRefresh: true, kiroAccountInfo: true, overage: true, known: true},
		{name: "idc", authMethod: "idc", kind: AccountKindKiroOAuth, kiroRuntime: true, kiroRefresh: true, kiroAccountInfo: true, overage: true, known: true},
		{name: "external idp", authMethod: "external_idp", kind: AccountKindKiroOAuth, kiroRuntime: true, kiroRefresh: true, kiroAccountInfo: true, overage: true, known: true},
		{name: "kiro api key", authMethod: "api_key", kind: AccountKindKiroAPIKey, kiroRuntime: true, staticCredential: true, kiroAccountInfo: true, overage: true, known: true},
		{name: "direct Anthropic", authMethod: "anthropic_api", kind: AccountKindDirectAnthropic, directAnthropic: true, staticCredential: true, known: true},
		{name: "custom relay", authMethod: "custom_api", kind: AccountKindCustomRelay, staticCredential: true, known: true},
		{name: "bedrock", authMethod: "bedrock", kind: AccountKindBedrock, staticCredential: true, known: true},
		{name: "unknown", authMethod: "future_provider", kind: AccountKindUnknown},
		{name: "typo", authMethod: "anthropic-ap", kind: AccountKindUnknown},
		{name: "empty", authMethod: "", kind: AccountKindUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{AuthMethod: tt.authMethod}
			if got := account.Kind(); got != tt.kind {
				t.Fatalf("Kind=%s want=%s", got, tt.kind)
			}
			if got := account.HasKnownKind(); got != tt.known {
				t.Fatalf("HasKnownKind=%v", got)
			}
			if got := account.IsDirectAnthropic(); got != tt.directAnthropic {
				t.Fatalf("IsDirectAnthropic=%v", got)
			}
			if got := account.IsStaticProviderCredential(); got != tt.staticCredential {
				t.Fatalf("IsStaticProviderCredential=%v", got)
			}
			if got := account.UsesKiroRuntime(); got != tt.kiroRuntime {
				t.Fatalf("UsesKiroRuntime=%v", got)
			}
			if got := account.SupportsKiroTokenRefresh(); got != tt.kiroRefresh {
				t.Fatalf("SupportsKiroTokenRefresh=%v", got)
			}
			if got := account.SupportsKiroAccountInfo(); got != tt.kiroAccountInfo {
				t.Fatalf("SupportsKiroAccountInfo=%v", got)
			}
			if got := account.SupportsOverageAPI(); got != tt.overage {
				t.Fatalf("SupportsOverageAPI=%v", got)
			}
		})
	}
}
