// Package config provides configuration management for Kiro API Proxy.
//
// This package handles persistent storage and retrieval of:
//   - Account credentials and authentication tokens
//   - Server settings (port, host, API keys)
//   - Usage statistics and metrics
//   - Thinking mode configuration for AI responses
//
// All configuration is stored in a JSON file with thread-safe access
// via read-write mutex protection.
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GenerateMachineId generates a UUID v4 format machine identifier.
// This ID is used to uniquely identify the proxy instance in Kiro API requests,
// helping with request tracking and rate limiting on the server side.
func GenerateMachineId() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	bytes[6] = (bytes[6] & 0x0f) | 0x40 // 版本 4
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // 变体
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// Account represents a Kiro API account with authentication credentials and usage statistics.
type Account struct {
	// Basic identification
	ID string `json:"id"` // Unique account identifier (UUID)
	// DispatchGeneration is a process-local lifecycle fence copied into request
	// snapshots. It is never persisted and prevents state keyed by a reused textual
	// ID from leaking across delete/recreate or source replacement.
	DispatchGeneration uint64 `json:"-"`
	Email              string `json:"email,omitempty"`    // User email address
	UserId             string `json:"userId,omitempty"`   // Kiro user ID
	Nickname           string `json:"nickname,omitempty"` // Display name for admin panel

	// AccountInfoRevision is a process-local publication fence stamped into
	// account snapshots. It is never persisted; the authoritative counter lives
	// beside lifecycle bookkeeping and orders asynchronous account-info results.
	AccountInfoRevision uint64 `json:"-"`

	// Custom API (pool-linking) fields. Present only when AuthMethod == "custom_api":
	// the account is a transparent proxy to ANOTHER Kiro-Go pool rather than a direct
	// Kiro credential. The upstream bearer token is stored in KiroApiKey (its existing
	// "upstream bearer, never refreshed" role); these fields carry the rest.
	BaseURL string   `json:"baseUrl,omitempty"` // Upstream pool root, e.g. https://pool.example.com (no trailing /v1)
	OrderID string   `json:"orderId,omitempty"` // Order id; also used as the account name/nickname
	Tags    []string `json:"tags,omitempty"`    // Labels; custom_api accounts carry ["Custom API"]

	// Authentication credentials
	AccessToken  string `json:"accessToken"`  // OAuth access token for API calls
	RefreshToken string `json:"refreshToken"` // OAuth refresh token for token renewal
	// RefreshTokenFingerprint is a one-way identifier for the credential that
	// originally created this account. It prevents a previously imported token
	// from being imported again after the provider rotates it.
	RefreshTokenFingerprint string `json:"refreshTokenFingerprint,omitempty"`
	ClientID                string `json:"clientId,omitempty"`     // OIDC client ID (for IdC auth)
	ClientSecret            string `json:"clientSecret,omitempty"` // OIDC client secret (for IdC auth)
	KiroApiKey              string `json:"kiroApiKey,omitempty"`   // Kiro API key credential (ksk_...); used directly as the upstream bearer token for headless "api_key" auth. Never refreshed.
	AuthMethod              string `json:"authMethod"`             // Authentication method: "idc" (AWS IdC), "social" (GitHub/Google), "external_idp" (enterprise SSO, e.g. Azure AD), or "api_key" (headless Kiro API key)
	Provider                string `json:"provider,omitempty"`     // Identity provider name (e.g., "BuilderId", "GitHub", "AzureAD")
	Region                  string `json:"region"`                 // AWS region for OIDC endpoints
	StartUrl                string `json:"startUrl,omitempty"`     // AWS SSO start URL
	ExpiresAt               int64  `json:"expiresAt,omitempty"`    // Token expiration timestamp (Unix seconds)
	MachineId               string `json:"machineId,omitempty"`    // UUID machine identifier for request tracking
	ProfileArn              string `json:"profileArn,omitempty"`   // CodeWhisperer/Kiro profile ARN for generation requests

	// External IdP (enterprise SSO, e.g. Microsoft 365 / Entra ID / Azure AD) refresh material.
	// When AuthMethod == "external_idp" the credential is an IdP-issued OAuth token refreshed
	// against TokenEndpoint using ClientID and Scopes (refresh_token grant), NOT the AWS SSO
	// OIDC endpoint. IssuerURL is the OIDC issuer the endpoints were discovered from.
	TokenEndpoint string `json:"tokenEndpoint,omitempty"` // External IdP OAuth2 token endpoint (refresh)
	IssuerURL     string `json:"issuerUrl,omitempty"`     // External IdP OIDC issuer URL
	Scopes        string `json:"scopes,omitempty"`        // Space-separated scopes granted by the external IdP

	// Native Amazon Bedrock fields. Present only when AuthMethod == "bedrock":
	// the account calls the Bedrock Runtime invoke endpoints directly with a static
	// IAM access key, SigV4-signed. Region reuses the existing Region field above.
	// BedrockModelMap optionally overrides client-model -> Bedrock-model-id resolution
	// per account; when nil the env BEDROCK_MODEL_MAP and built-in defaults apply.
	// NOTE: the secret is stored as-is in the config JSON, matching how OAuth tokens
	// and Kiro API keys are already persisted here; protect the config file at rest.
	BedrockAccessKeyID     string `json:"bedrockAccessKeyId,omitempty"`
	BedrockSecretAccessKey string `json:"bedrockSecretAccessKey,omitempty"`
	BedrockSessionToken    string `json:"bedrockSessionToken,omitempty"` // set only for STS/temporary credentials
	// BedrockAPIKey is a Bedrock API key (bearer token, "ABSK..."). When set it is
	// used as an Authorization: Bearer header and SigV4 is skipped; it authenticates
	// principals whose raw IAM access key is denied InvokeModel. Either this OR the
	// access key/secret pair is required for a bedrock account.
	BedrockAPIKey string `json:"bedrockApiKey,omitempty"`
	// BedrockRegions are EXTRA candidate regions (beyond Region) to try for this
	// account. Bedrock model access is per-region: a model denied in the primary
	// region may be callable in another. The request path tries Region first, then
	// these, caching the callable region per model. Empty = single-region (Region).
	BedrockRegions  []string          `json:"bedrockRegions,omitempty"`
	BedrockModelMap map[string]string `json:"bedrockModelMap,omitempty"`
	// BedrockUseConverse opts this account into the Bedrock Converse API path
	// (bedrock-runtime /converse[-stream]) instead of the native Anthropic invoke
	// path. Required for non-Anthropic models (Nova, Llama, DeepSeek, ...), which do
	// not accept the Anthropic Messages wire format. Defaults false: Claude models
	// stay on the zero-translation native invoke path.
	BedrockUseConverse bool `json:"bedrockUseConverse,omitempty"`

	// Per-account outbound proxy (falls back to global ProxyURL if empty)
	ProxyURL string `json:"proxyURL,omitempty"`

	// Weight is a soft health/load factor used as a tie-breaker inside an
	// already-selected routing tier. It never bypasses account eligibility or
	// the hard Priority tier.
	Weight int `json:"weight,omitempty"` // 0 or 1 = normal, 2+ = higher soft weight

	// Priority is a hard routing tier. Lower positive values are preferred.
	// Zero/unset uses DefaultAccountPriority for backward-compatible configs.
	Priority int `json:"priority,omitempty"`

	// Upstream Overages state (mirrored from AWS Q `setUserPreference` / `getUsageLimits`).
	// OverageStatus is the only switch that decides whether to keep dispatching once UsageLimit is reached.
	// Allowed values: "ENABLED", "DISABLED", "UNKNOWN" (or empty when not yet fetched).
	OverageStatus     string  `json:"overageStatus,omitempty"`
	OverageCapability string  `json:"overageCapability,omitempty"` // "OVERAGE_CAPABLE" / "NOT_OVERAGE_CAPABLE"
	OverageCap        float64 `json:"overageCap,omitempty"`        // Hard upper bound (USD)
	OverageRate       float64 `json:"overageRate,omitempty"`       // Per-invocation rate (USD)
	CurrentOverages   float64 `json:"currentOverages,omitempty"`   // Cumulative overage charges (USD)
	OverageCheckedAt  int64   `json:"overageCheckedAt,omitempty"`  // Last successful upstream sync (Unix seconds)

	// LegacyAllowOverage is kept for backward-compatible JSON loading only.
	// Pre-Overages-switch deployments persisted `allowOverage: true` to mean
	// "keep dispatching when quota is exhausted". On first load we migrate it
	// into OverageStatus="ENABLED" and zero this field so it does not get
	// re-emitted on future saves. Do not read this field elsewhere.
	LegacyAllowOverage bool `json:"allowOverage,omitempty"`

	// Account status
	Enabled   bool   `json:"enabled"`             // Whether account is active in the pool
	BanStatus string `json:"banStatus,omitempty"` // Runtime state: "ACTIVE", "BANNED", "BILLING_BLOCKED", "SUSPENDED", or "DISABLED"
	BanReason string `json:"banReason,omitempty"` // Reason for ban/suspension
	BanTime   int64  `json:"banTime,omitempty"`   // Timestamp when ban was detected

	// Subscription information
	SubscriptionType  string `json:"subscriptionType,omitempty"`  // Tier: FREE, PRO, PRO_PLUS, or POWER
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"` // Human-readable subscription name
	DaysRemaining     int    `json:"daysRemaining,omitempty"`     // Days until subscription expires

	// Usage tracking
	UsageCurrent  float64 `json:"usageCurrent,omitempty"`  // Current period usage (credits)
	UsageLimit    float64 `json:"usageLimit,omitempty"`    // Maximum allowed usage per period
	UsagePercent  float64 `json:"usagePercent,omitempty"`  // Usage percentage (0.0-1.0)
	NextResetDate string  `json:"nextResetDate,omitempty"` // Date when usage resets (YYYY-MM-DD)
	LastRefresh   int64   `json:"lastRefresh,omitempty"`   // Last info refresh timestamp

	// Trial usage tracking
	TrialUsageCurrent float64 `json:"trialUsageCurrent,omitempty"` // Trial quota current usage
	TrialUsageLimit   float64 `json:"trialUsageLimit,omitempty"`   // Trial quota total limit
	TrialUsagePercent float64 `json:"trialUsagePercent,omitempty"` // Trial quota usage percentage (0.0-1.0)
	TrialStatus       string  `json:"trialStatus,omitempty"`       // Trial status: ACTIVE, EXPIRED, NONE
	TrialExpiresAt    int64   `json:"trialExpiresAt,omitempty"`    // Trial expiration timestamp (Unix seconds)

	// Runtime statistics (updated during operation)
	RequestCount int     `json:"requestCount,omitempty"` // Total requests processed
	ErrorCount   int     `json:"errorCount,omitempty"`   // Total errors encountered
	LastUsed     int64   `json:"lastUsed,omitempty"`     // Last request timestamp
	TotalTokens  int     `json:"totalTokens,omitempty"`  // Cumulative tokens processed
	TotalCredits float64 `json:"totalCredits,omitempty"` // Cumulative credits consumed
}

// AccountKind is the normalized runtime category for an account credential.
// Unknown values deliberately fail closed instead of inheriting Kiro OAuth/AWS
// lifecycle behavior.
type AccountKind uint8

const (
	AccountKindUnknown AccountKind = iota
	AccountKindKiroOAuth
	AccountKindKiroAPIKey
	AccountKindCustomRelay
	AccountKindBedrock
	AccountKindDirectAnthropic
)

func (k AccountKind) String() string {
	switch k {
	case AccountKindKiroOAuth:
		return "kiro_oauth"
	case AccountKindKiroAPIKey:
		return "kiro_api_key"
	case AccountKindCustomRelay:
		return "custom_relay"
	case AccountKindBedrock:
		return "bedrock"
	case AccountKindDirectAnthropic:
		return "direct_anthropic"
	default:
		return "unknown"
	}
}

// Kind returns the explicit account kind. New/unknown auth methods never fall
// through to Kiro token refresh, account-info, or overage APIs.
func (a *Account) Kind() AccountKind {
	if a == nil {
		return AccountKindUnknown
	}
	switch strings.ToLower(strings.TrimSpace(a.AuthMethod)) {
	case "idc", "social", "external_idp":
		return AccountKindKiroOAuth
	case "api_key":
		return AccountKindKiroAPIKey
	case "custom_api":
		return AccountKindCustomRelay
	case "bedrock":
		return AccountKindBedrock
	case "anthropic_api":
		return AccountKindDirectAnthropic
	default:
		return AccountKindUnknown
	}
}

func (a *Account) HasKnownKind() bool { return a != nil && a.Kind() != AccountKindUnknown }

// UsesKiroRuntime reports whether this credential ultimately uses the Kiro
// inference runtime. Authentication lifecycle is intentionally separate: Kiro
// OAuth identities and Kiro API keys share inference capabilities even though
// only OAuth identities are refreshable.
func (a *Account) UsesKiroRuntime() bool {
	if a == nil {
		return false
	}
	switch a.Kind() {
	case AccountKindKiroOAuth, AccountKindKiroAPIKey:
		return true
	default:
		return false
	}
}

// IsApiKeyCredential reports whether the account authenticates with a Kiro API
// key (ksk_...) rather than an OAuth token.
func (a *Account) IsApiKeyCredential() bool { return a != nil && a.Kind() == AccountKindKiroAPIKey }

// IsCustomApi reports whether the account is a linked Kiro-Go relay.
func (a *Account) IsCustomApi() bool { return a != nil && a.Kind() == AccountKindCustomRelay }

// IsBedrock reports whether the account directly uses Amazon Bedrock.
func (a *Account) IsBedrock() bool { return a != nil && a.Kind() == AccountKindBedrock }

// IsDirectAnthropic reports whether the account is a terminal Anthropic
// Messages API credential.
func (a *Account) IsDirectAnthropic() bool { return a != nil && a.Kind() == AccountKindDirectAnthropic }

// IsStaticProviderCredential reports whether the account authenticates with a
// long-lived provider credential rather than a refreshable Kiro OAuth token.
func (a *Account) IsStaticProviderCredential() bool {
	if a == nil {
		return false
	}
	switch a.Kind() {
	case AccountKindKiroAPIKey, AccountKindCustomRelay, AccountKindBedrock, AccountKindDirectAnthropic:
		return true
	default:
		return false
	}
}

// SupportsKiroTokenRefresh reports whether auth.RefreshToken is valid.
func (a *Account) SupportsKiroTokenRefresh() bool {
	return a != nil && a.Kind() == AccountKindKiroOAuth
}

// SupportsKiroAccountInfo reports whether Kiro/AWS account-info APIs apply.
func (a *Account) SupportsKiroAccountInfo() bool {
	if a == nil {
		return false
	}
	switch a.Kind() {
	case AccountKindKiroOAuth, AccountKindKiroAPIKey:
		return true
	default:
		return false
	}
}

// SupportsOverageAPI reports whether AWS Q overage endpoints apply.
func (a *Account) SupportsOverageAPI() bool { return a != nil && a.SupportsKiroAccountInfo() }

// PromptFilterRule defines a single custom prompt sanitization rule.
// Type can be: "regex" (regexp find/replace within prompt) or
// "lines-containing" (remove lines containing the match substring).
type PromptFilterRule struct {
	ID      string `json:"id"`                // Unique rule identifier
	Name    string `json:"name"`              // Human-readable rule name
	Type    string `json:"type"`              // "regex" or "lines-containing"
	Match   string `json:"match"`             // Pattern to match (regex pattern or substring)
	Replace string `json:"replace,omitempty"` // Replacement string (only for regex; empty = delete match)
	Enabled bool   `json:"enabled"`           // Whether this rule is active
}

// ApiKeyEntry represents a single API key with optional usage limits and counters.
// Limits with value 0 are treated as "no limit". Counters are cumulative and never reset
// automatically; operators can use the admin endpoint to manually reset them.
type ApiKeyEntry struct {
	ID         string `json:"id"`                 // Unique identifier (UUID)
	Name       string `json:"name,omitempty"`     // Human-readable label
	Key        string `json:"key"`                // The actual key value clients send
	Enabled    bool   `json:"enabled"`            // Whether this key may authenticate
	Migrated   bool   `json:"migrated,omitempty"` // True if migrated from legacy single ApiKey field
	CreatedAt  int64  `json:"createdAt"`          // Creation timestamp (Unix seconds)
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`

	// Limits (0 = unlimited)
	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`

	// Cumulative usage (never auto-reset)
	TokensUsed    int64   `json:"tokensUsed,omitempty"`
	CreditsUsed   float64 `json:"creditsUsed,omitempty"`
	RequestsCount int64   `json:"requestsCount,omitempty"`
}

// Config represents the global application configuration.
type Config struct {
	// Server settings
	Password      string        `json:"password"`          // Admin panel password
	Port          int           `json:"port"`              // HTTP server port (default: 8080)
	Host          string        `json:"host"`              // HTTP server bind address (default: 0.0.0.0)
	ApiKey        string        `json:"apiKey,omitempty"`  // [Deprecated] Legacy single API key, migrated into ApiKeys on first load
	RequireApiKey bool          `json:"requireApiKey"`     // [Deprecated] Whether to enforce API key validation; with multi-key support, len(ApiKeys)>0 implicitly enforces auth
	ApiKeys       []ApiKeyEntry `json:"apiKeys,omitempty"` // Multiple API keys, each with independent quota
	KiroVersion   string        `json:"kiroVersion,omitempty"`
	SystemVersion string        `json:"systemVersion,omitempty"`
	NodeVersion   string        `json:"nodeVersion,omitempty"`
	Accounts      []Account     `json:"accounts"` // Registered Kiro accounts

	// Thinking mode configuration for extended reasoning output
	ThinkingSuffix       string `json:"thinkingSuffix,omitempty"`       // Model suffix to trigger thinking mode (default: "-thinking")
	OpenAIThinkingFormat string `json:"openaiThinkingFormat,omitempty"` // OpenAI output format: "reasoning_content", "thinking", or "think"
	ClaudeThinkingFormat string `json:"claudeThinkingFormat,omitempty"` // Claude output format: "reasoning_content", "thinking", or "think"

	// Endpoint configuration: "auto", "kiro", "codewhisperer", or "amazonq"
	PreferredEndpoint string `json:"preferredEndpoint,omitempty"`

	// EndpointFallback controls whether to try other endpoints when the preferred one fails.
	// Defaults to true. Set to false to only use the preferred endpoint.
	EndpointFallback *bool `json:"endpointFallback,omitempty"`

	// AllowOverUsage allows accounts to continue serving requests even when their
	// usage quota has been exhausted. When enabled, the pool will not skip accounts
	// solely because usageCurrent >= usageLimit.
	AllowOverUsage bool `json:"allowOverUsage,omitempty"`

	// AutoRecoverEnabled controls whether disabled accounts (auth failure) are
	// periodically re-probed with a token refresh. Default true. Set false to
	// require manual re-enable.
	AutoRecoverEnabled *bool `json:"autoRecoverEnabled,omitempty"`

	// SessionAffinityEnabled binds consecutive requests from the same API key to
	// the same account (sticky routing) for a TTL window. Default false.
	SessionAffinityEnabled bool `json:"sessionAffinityEnabled,omitempty"`

	// AccountSelectionStrategy controls routing after the existing eligibility
	// gates and session-affinity lookup. Values: "round-robin" (default),
	// "priority", or "fill-first".
	AccountSelectionStrategy string `json:"accountSelectionStrategy,omitempty"`

	// AgentToolSteering injects extra instructions (a system-prompt addendum
	// plus a per-turn reminder, see proxy/translator.go's
	// agentToolExecutionPrompt/agentToolTurnReminder) into tool-bearing Claude
	// requests. Default false: transparent proxy behavior must not silently
	// modify a customer's own system prompt/user turn. Operators that prefer
	// stronger tool-use steering can opt in explicitly.
	AgentToolSteering bool `json:"agentToolSteering,omitempty"`

	// ResponseLanguagePolicy controls optional Kiro-native language steering.
	// Values: "preserve" (default/no injection), "match-user", or "fixed".
	// ResponseLanguage carries the BCP47-like language tag used by fixed mode.
	// The setting affects only requests translated to Kiro native; transparent
	// relay and other native provider transports keep the original request.
	ResponseLanguagePolicy string `json:"responseLanguagePolicy,omitempty"`
	ResponseLanguage       string `json:"responseLanguage,omitempty"`

	// Proxy configuration: optional outbound proxy for Kiro API requests
	// Format: "socks5://host:port", "socks5://user:pass@host:port",
	//         "http://host:port",  "http://user:pass@host:port"
	// Leave empty to connect directly.
	ProxyURL string `json:"proxyURL,omitempty"`

	// ProxyURLs is an optional pool of outbound proxies rotated round-robin every
	// ProxyRotateMinutes. When non-empty it drives the GLOBAL proxy (the single
	// ProxyURL above is ignored for routing); per-account ProxyURL overrides still
	// take precedence. Each entry uses the same scheme format as ProxyURL.
	ProxyURLs []string `json:"proxyURLs,omitempty"`

	// ProxyRotateMinutes is the round-robin interval for ProxyURLs. <=0 falls back to
	// DefaultProxyRotateMinutes. Ignored when ProxyURLs is empty.
	ProxyRotateMinutes int `json:"proxyRotateMinutes,omitempty"`

	// SanitizeClaudeCodePrompt is kept for backward-compatible JSON loading only.
	// Migrated to FilterClaudeCode on first load. Do not use directly.
	SanitizeClaudeCodePrompt bool `json:"sanitizeClaudeCodePrompt,omitempty"`

	// FilterClaudeCode detects the Claude Code CLI built-in system prompt and replaces it
	// with a compact backend-only prompt, reducing token usage significantly.
	FilterClaudeCode bool `json:"filterClaudeCode,omitempty"`

	// FilterEnvNoise strips environment metadata lines from system prompts:
	// git status, recent commits, environment sections, fast_mode_info tags, etc.
	FilterEnvNoise bool `json:"filterEnvNoise,omitempty"`

	// FilterStripBoundaries removes --- SYSTEM PROMPT --- / --- END SYSTEM PROMPT --- markers.
	FilterStripBoundaries bool `json:"filterStripBoundaries,omitempty"`

	// PromptFilterRules is a list of user-defined prompt sanitization rules (regex or line-filter).
	PromptFilterRules []PromptFilterRule `json:"promptFilterRules,omitempty"`

	// LogLevel controls verbosity of application logs.
	// Accepted values: "debug", "info", "warn", "error". Defaults to "info".
	// Can be overridden by the LOG_LEVEL environment variable.
	LogLevel string `json:"logLevel,omitempty"`

	// PromptCacheMaxRatio caps the fraction of input tokens reported as cache_read
	// in a single turn. Default 0.85. Raise to 0.95 for "continue"-heavy workloads
	// where the newest content is minimal and >85% of input is genuinely from cache.
	PromptCacheMaxRatio float64 `json:"promptCacheMaxRatio,omitempty"`

	// PromptCacheMaxEntries bounds the in-memory prompt-cache map; once exceeded,
	// the least-recently-used entries are evicted (LRU). Default 131072. Sized so
	// the prefix write-rate × TTL does not evict multi-turn history prefixes
	// before the next turn reuses them (mirrors kiro-rs's 131072 default). The
	// tracker clamps explicit small values up to 256.
	PromptCacheMaxEntries int `json:"promptCacheMaxEntries,omitempty"`

	// Global statistics (persisted across restarts)
	TotalRequests   int     `json:"totalRequests,omitempty"`   // Total API requests received
	SuccessRequests int     `json:"successRequests,omitempty"` // Successful requests count
	FailedRequests  int     `json:"failedRequests,omitempty"`  // Failed requests count
	TotalTokens     int     `json:"totalTokens,omitempty"`     // Total tokens processed
	TotalCredits    float64 `json:"totalCredits,omitempty"`    // Total credits consumed
}

// AccountInfo contains account metadata retrieved from Kiro API.
// Used for updating subscription and usage information.
type AccountInfo struct {
	Email             string
	UserId            string
	SubscriptionType  string
	SubscriptionTitle string
	DaysRemaining     int
	UsageCurrent      float64
	UsageLimit        float64
	UsagePercent      float64
	NextResetDate     string
	LastRefresh       int64
	TrialUsageCurrent float64
	TrialUsageLimit   float64
	TrialUsagePercent float64
	TrialStatus       string
	TrialExpiresAt    int64
}

// Version current version
const Version = "1.1.5"

var (
	cfg     *Config
	cfgLock sync.RWMutex
	cfgPath string
)

// Init initializes the configuration system with the specified file path.
// If the file doesn't exist, a default configuration is created. The loaded
// image and its path are published together only after any required migration
// write has made the new image visible.
func Init(path string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	return loadPathLocked(path)
}

func Load() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	return loadPathLocked(cfgPath)
}

func loadPathLocked(path string) error {
	if path == "" {
		return fmt.Errorf("configuration path is empty")
	}
	if err := ensureConfigOwnerPrivate(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("secure configuration file: %w", err)
	}
	previous := cfg
	data, err := os.ReadFile(path)
	needsPersist := false
	var next *Config
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		next = &Config{Password: "changeme", Port: 8080, Host: "0.0.0.0", RequireApiKey: false, Accounts: []Account{}}
		needsPersist = true
	} else {
		var decoded Config
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		next = &decoded
	}

	if next.ApiKey != "" && len(next.ApiKeys) == 0 {
		next.ApiKeys = append(next.ApiKeys, ApiKeyEntry{ID: newUUID(), Name: "legacy", Key: next.ApiKey, Enabled: next.RequireApiKey, Migrated: true, CreatedAt: time.Now().Unix()})
		needsPersist = true
	}
	for i := range next.Accounts {
		if next.Accounts[i].Kind() == AccountKindDirectAnthropic && next.Accounts[i].AccessToken != "" {
			// Legacy admin paths mirrored the static Anthropic x-api-key into the
			// generic bearer-token field. Preserve an older bearer-only configuration
			// by promoting it into the provider-specific slot first, then clear the
			// duplicate bearer representation so all configurations converge to the
			// direct-Anthropic trust boundary.
			if strings.TrimSpace(next.Accounts[i].KiroApiKey) == "" {
				next.Accounts[i].KiroApiKey = strings.TrimSpace(next.Accounts[i].AccessToken)
			}
			next.Accounts[i].AccessToken = ""
			next.Accounts[i].ExpiresAt = 0
			needsPersist = true
		}
	}
	for i := range next.Accounts {
		if !next.Accounts[i].LegacyAllowOverage {
			continue
		}
		if next.Accounts[i].OverageStatus == "" {
			next.Accounts[i].OverageStatus = "ENABLED"
		}
		next.Accounts[i].LegacyAllowOverage = false
		needsPersist = true
	}

	if needsPersist {
		serialized, marshalErr := json.MarshalIndent(next, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		published, writeErr := writeConfigBytes(path, serialized, 0600)
		if !published {
			return writeErr
		}
		cfgPath = path
		cfg = next
		if previous != nil {
			reconcileAccountStatsGenerationsLocked(previous.Accounts, next.Accounts)
			reconcileAccountLifecycleGenerationsLocked(previous.Accounts, next.Accounts)
		} else {
			reconcileAccountStatsGenerationsLocked(nil, next.Accounts)
			reconcileAccountLifecycleGenerationsLocked(nil, next.Accounts)
		}
		return writeErr
	}

	cfgPath = path
	cfg = next
	if previous != nil {
		reconcileAccountStatsGenerationsLocked(previous.Accounts, next.Accounts)
		reconcileAccountLifecycleGenerationsLocked(previous.Accounts, next.Accounts)
	} else {
		reconcileAccountStatsGenerationsLocked(nil, next.Accounts)
		reconcileAccountLifecycleGenerationsLocked(nil, next.Accounts)
	}
	return nil
}

// saveLocked persists the already-published live image. New mutators should use
// publishConfigTransactionLocked so pre-publication failures cannot leave memory
// ahead of disk. Caller MUST hold cfgLock.
func saveLocked() error {
	_, err := persistConfigImageLocked(cfg)
	return err
}

func newUUID() string { return GenerateMachineId() }

// Save persists the current image while holding cfgLock so a detached snapshot
// cannot be published after a newer transaction and overwrite it on disk.
func Save() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return fmt.Errorf("configuration is not initialized")
	}
	_, err := persistConfigImageLocked(cfg)
	return err
}

// SetPassword updates the admin password.
// Primarily used for environment variable override in containerized deployments.
func SetPassword(password string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Password = password
}

// GetConfigDir returns the directory containing the config JSON file.
// Useful for sibling state (e.g. stored Responses, caches) that should live
// alongside the configuration file.
func GetConfigDir() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfgPath == "" {
		return "."
	}
	dir := cfgPath
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			return dir[:i]
		}
	}
	return "."
}

func Get() *Config {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	return cfg.Clone()
}

func GetPassword() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Password
}

func GetPort() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Port == 0 {
		return 8080
	}
	return cfg.Port
}

func GetHost() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Host == "" {
		return "127.0.0.1"
	}
	return cfg.Host
}

func GetAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	accounts := make([]Account, len(cfg.Accounts))
	for i := range cfg.Accounts {
		accounts[i] = cloneAccountWithLifecycleLocked(cfg.Accounts[i])
	}
	return accounts
}

// GetAccountByID returns a copy of the account with the given ID, or ok=false
// if no such account exists. Used by auth.RefreshToken's double-checked
// locking to read the canonical token state.
func GetAccountByID(id string) (Account, bool) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			return cloneAccountWithLifecycleLocked(cfg.Accounts[i]), true
		}
	}
	return Account{}, false
}

// AccountIDExists reports whether an account with the given ID is already stored.
// Used by the credential-import path to reuse a pasted record's id when it does
// not collide, so re-importing a backup never creates a duplicate entry.
func AccountIDExists(id string) bool {
	if id == "" {
		return false
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for _, a := range cfg.Accounts {
		if a.ID == id {
			return true
		}
	}
	return false
}

func GetEnabledAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	var accounts []Account
	for _, a := range cfg.Accounts {
		if a.Enabled {
			accounts = append(accounts, cloneAccountWithLifecycleLocked(a))
		}
	}
	return accounts
}

func AddAccount(account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	account = account.Clone()
	if err := prepareAccountForAdd(&account); err != nil {
		return err
	}
	_, err := publishConfigAccountsLocked(func(next *Config) error {
		if err := rejectDuplicateAccount(next, &account); err != nil {
			return err
		}
		next.Accounts = append(next.Accounts, account.Clone())
		return nil
	})
	return err
}

func UpdateAccount(id string, account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	account = account.Clone()
	found, _, err := publishAccountByIDLocked(id, true, func(current *Account) error {
		preserveAuthoritativeCredentialState(current.Clone(), &account)
		*current = account.Clone()
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		// Preserve the historical UpdateAccount contract: a missing account is a no-op.
		return nil
	}
	return nil
}

func UpdateAccountOverageStatus(id, status, capability string, cap, rate, current float64, checkedAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, _, err := publishAccountByIDLocked(id, false, func(account *Account) error {
		if status != "" {
			account.OverageStatus = status
		}
		if capability != "" {
			account.OverageCapability = capability
		}
		account.OverageCap = cap
		account.OverageRate = rate
		account.CurrentOverages = current
		if checkedAt > 0 {
			account.OverageCheckedAt = checkedAt
		}
		return nil
	})
	return err
}

func SetAccountEnabled(id string, enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, _, err := publishAccountByIDLocked(id, false, func(account *Account) error {
		account.Enabled = enabled
		if !enabled {
			account.BanStatus = "DISABLED"
			account.BanTime = time.Now().Unix()
		}
		return nil
	})
	return err
}

func SetAccountBanStatus(id, status, reason string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, _, err := publishAccountByIDLocked(id, false, func(account *Account) error {
		account.BanStatus = status
		account.BanReason = reason
		account.BanTime = time.Now().Unix()
		if status == "BANNED" || status == "DISABLED" || status == "BILLING_BLOCKED" {
			account.Enabled = false
		}
		return nil
	})
	return err
}

func UpdateAccountProfileArn(id, profileArn string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	found, _, err := publishAccountByIDLocked(id, false, func(account *Account) error { account.ProfileArn = profileArn; return nil })
	if !found {
		return fmt.Errorf("account not found: %s", id)
	}
	return err
}

func DeleteAccount(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return fmt.Errorf("configuration is not initialized")
	}
	index := -1
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return nil
	}
	_, err := publishConfigAccountsLocked(func(next *Config) error {
		next.Accounts = append(next.Accounts[:index], next.Accounts[index+1:]...)
		return nil
	})
	return err
}

func UpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error {
	return UpdateAccountCredentialState(id, accessToken, refreshToken, expiresAt, "")
}

func GetApiKey() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ApiKey
}

func IsApiKeyRequired() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.RequireApiKey
}

func UpdateSettings(apiKey string, requireApiKey bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error {
		next.ApiKey = apiKey
		next.RequireApiKey = requireApiKey
		if password != "" {
			next.Password = password
		}
		return nil
	})
	return err
}

func UpdateSettingsPatch(apiKey *string, requireApiKey *bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error {
		if apiKey != nil {
			next.ApiKey = *apiKey
		}
		if requireApiKey != nil {
			next.RequireApiKey = *requireApiKey
		}
		if password != "" {
			next.Password = password
		}
		return nil
	})
	return err
}

func UpdateStats(totalReq, successReq, failedReq, totalTokens int, totalCredits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error {
		next.TotalRequests = totalReq
		next.SuccessRequests = successReq
		next.FailedRequests = failedReq
		next.TotalTokens = totalTokens
		next.TotalCredits = totalCredits
		return nil
	})
	return err
}

func GetStats() (int, int, int, int, float64) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TotalRequests, cfg.SuccessRequests, cfg.FailedRequests, cfg.TotalTokens, cfg.TotalCredits
}

func UpdateAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	// A caller may use this legacy setter to reset cumulative counters. Reconcile
	// the account stats generation so an in-flight request holding the pre-reset
	// lease cannot resurrect those counters after the reset is published.
	_, _, err := publishAccountByIDLocked(id, true, func(account *Account) error {
		account.RequestCount = requestCount
		account.ErrorCount = errorCount
		account.TotalTokens = totalTokens
		account.TotalCredits = totalCredits
		account.LastUsed = lastUsed
		return nil
	})
	return err
}

func UpdateAccountInfo(id string, info AccountInfo) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	found, published, err := publishAccountByIDLocked(id, false, func(account *Account) error { applyAccountInfo(account, info); return nil })
	if found && published {
		bumpAccountInfoRevisionLocked(id)
	}
	return err
}

func GetFilterClaudeCode() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt
}

// GetFilterEnvNoise returns whether environment noise line stripping is enabled.
func GetFilterEnvNoise() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterEnvNoise
}

// GetFilterStripBoundaries returns whether boundary marker stripping is enabled.
func GetFilterStripBoundaries() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterStripBoundaries
}

// PromptFilterConfig holds all prompt filter settings for API responses.
type PromptFilterConfig struct {
	FilterClaudeCode      bool               `json:"filterClaudeCode"`
	FilterEnvNoise        bool               `json:"filterEnvNoise"`
	FilterStripBoundaries bool               `json:"filterStripBoundaries"`
	Rules                 []PromptFilterRule `json:"rules"`
}

// GetPromptFilterConfig returns all prompt filter settings.
func GetPromptFilterConfig() PromptFilterConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return PromptFilterConfig{Rules: []PromptFilterRule{}}
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return PromptFilterConfig{
		FilterClaudeCode:      cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt,
		FilterEnvNoise:        cfg.FilterEnvNoise,
		FilterStripBoundaries: cfg.FilterStripBoundaries,
		Rules:                 rules,
	}
}

// UpdatePromptFilterConfig saves all prompt filter settings atomically.
func UpdatePromptFilterConfig(filterClaudeCode, filterEnvNoise, filterStripBoundaries bool, rules []PromptFilterRule) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error {
		next.FilterClaudeCode = filterClaudeCode
		next.FilterEnvNoise = filterEnvNoise
		next.FilterStripBoundaries = filterStripBoundaries
		next.SanitizeClaudeCodePrompt = false
		if rules != nil {
			next.PromptFilterRules = append([]PromptFilterRule(nil), rules...)
		}
		return nil
	})
	return err
}

func GetPromptFilterRules() []PromptFilterRule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return rules
}

// ThinkingConfig holds settings for AI thinking/reasoning mode.
// When enabled, models output their reasoning process alongside the response.
type ThinkingConfig struct {
	Suffix       string `json:"suffix"`       // Model name suffix that triggers thinking mode
	OpenAIFormat string `json:"openaiFormat"` // Output format for OpenAI-compatible responses
	ClaudeFormat string `json:"claudeFormat"` // Output format for Claude-compatible responses
}

// GetThinkingConfig 获取 thinking 配置
func GetThinkingConfig() ThinkingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	suffix := cfg.ThinkingSuffix
	if suffix == "" {
		suffix = "-thinking"
	}
	openaiFormat := cfg.OpenAIThinkingFormat
	if openaiFormat == "" {
		openaiFormat = "reasoning_content"
	}
	claudeFormat := cfg.ClaudeThinkingFormat
	if claudeFormat == "" {
		claudeFormat = "thinking"
	}

	return ThinkingConfig{
		Suffix:       suffix,
		OpenAIFormat: openaiFormat,
		ClaudeFormat: claudeFormat,
	}
}

// UpdateThinkingConfig 更新 thinking 配置
func UpdateThinkingConfig(suffix, openaiFormat, claudeFormat string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error {
		next.ThinkingSuffix = suffix
		next.OpenAIThinkingFormat = openaiFormat
		next.ClaudeThinkingFormat = claudeFormat
		return nil
	})
	return err
}

func GetPreferredEndpoint() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.PreferredEndpoint == "" {
		return "auto"
	}
	return cfg.PreferredEndpoint
}

// UpdatePreferredEndpoint 更新首选端点配置
func UpdatePreferredEndpoint(endpoint string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error { next.PreferredEndpoint = endpoint; return nil })
	return err
}

func GetEndpointFallback() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.EndpointFallback == nil {
		return true
	}
	return *cfg.EndpointFallback
}

// UpdateEndpointFallback sets the endpoint fallback switch and persists the change.
func UpdateEndpointFallback(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error { value := enabled; next.EndpointFallback = &value; return nil })
	return err
}

// DefaultProxyRotateMinutes is the round-robin interval used when a proxy pool is configured without an explicit interval.
const DefaultProxyRotateMinutes = 10

func GetProxyURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ProxyURL
}

// GetProxyURLs returns the outbound proxy rotation pool (may be empty).
func GetProxyURLs() []string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	// Copy so callers cannot mutate the shared config slice.
	out := make([]string, len(cfg.ProxyURLs))
	copy(out, cfg.ProxyURLs)
	return out
}

// GetProxyRotateMinutes returns the rotation interval, normalized to a positive value.
func GetProxyRotateMinutes() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.ProxyRotateMinutes <= 0 {
		return DefaultProxyRotateMinutes
	}
	return cfg.ProxyRotateMinutes
}

// UpdateProxySettings 更新出站代理配置 (single proxy + rotation pool + interval).
func UpdateProxySettings(proxyURL string, proxyURLs []string, rotateMinutes int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error {
		next.ProxyURL = proxyURL
		next.ProxyURLs = append([]string(nil), proxyURLs...)
		next.ProxyRotateMinutes = rotateMinutes
		return nil
	})
	return err
}

func GetAllowOverUsage() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.AllowOverUsage
}

// GetAutoRecoverEnabled returns whether auto-recovery of disabled accounts is
// enabled. Defaults to true.
func GetAutoRecoverEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.AutoRecoverEnabled == nil {
		return true
	}
	return *cfg.AutoRecoverEnabled
}

// GetSessionAffinityEnabled returns whether session affinity (sticky routing per
// API key) is enabled. Defaults to false.
func GetSessionAffinityEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.SessionAffinityEnabled
}

// SetSessionAffinityEnabled sets the session-affinity flag and persists it.
func SetSessionAffinityEnabled(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error { next.SessionAffinityEnabled = enabled; return nil })
	return err
}

func UpdateAllowOverUsage(allow bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error { next.AllowOverUsage = allow; return nil })
	return err
}

func GetLogLevel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.LogLevel == "" {
		return "info"
	}
	return cfg.LogLevel
}

// GetPromptCacheMaxRatio returns the cache-read cap ratio (0.0-1.0). Defaults to 0.85.
func GetPromptCacheMaxRatio() float64 {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.PromptCacheMaxRatio <= 0 || cfg.PromptCacheMaxRatio > 1 {
		return 0.85
	}
	return cfg.PromptCacheMaxRatio
}

// UpdatePromptCacheMaxRatio sets the cache-read cap ratio and persists the change.
func UpdatePromptCacheMaxRatio(ratio float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error { next.PromptCacheMaxRatio = ratio; return nil })
	return err
}

const defaultPromptCacheMaxEntries = 131072
const minPromptCacheEntries = 256

func GetPromptCacheMaxEntries() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.PromptCacheMaxEntries <= 0 {
		return defaultPromptCacheMaxEntries
	}
	if cfg.PromptCacheMaxEntries < minPromptCacheEntries {
		return minPromptCacheEntries
	}
	return cfg.PromptCacheMaxEntries
}

// UpdatePromptCacheMaxEntries sets the prompt-cache LRU bound and persists it.
// Applies on the next tracker construction (restart); it does not resize a
// live tracker.
func UpdatePromptCacheMaxEntries(n int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error { next.PromptCacheMaxEntries = n; return nil })
	return err
}

func UpdateLogLevel(level string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error { next.LogLevel = level; return nil })
	return err
}

type KiroClientConfig struct {
	KiroVersion   string
	SystemVersion string
	NodeVersion   string
}

func GetKiroClientConfig() KiroClientConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	kiroVersion := "0.11.107"
	if cfg != nil && cfg.KiroVersion != "" {
		kiroVersion = cfg.KiroVersion
	}

	systemVersion := ""
	if cfg != nil {
		systemVersion = cfg.SystemVersion
	}
	if systemVersion == "" {
		systemVersion = defaultSystemVersion()
	}

	nodeVersion := "22.22.0"
	if cfg != nil && cfg.NodeVersion != "" {
		nodeVersion = cfg.NodeVersion
	}

	return KiroClientConfig{
		KiroVersion:   kiroVersion,
		SystemVersion: systemVersion,
		NodeVersion:   nodeVersion,
	}
}

func defaultSystemVersion() string {
	switch runtime.GOOS {
	case "windows":
		return "win32#10.0.22631"
	case "darwin":
		return "darwin#24.6.0"
	default:
		return "linux#6.6.87"
	}
}

const defaultMaxRequestBodyBytes int64 = 64 * 1024 * 1024

// GetMaxRequestBodyBytes returns the maximum accepted body size for public LLM
// JSON endpoints. A non-positive or invalid environment value falls back to the
// bounded default rather than disabling the guard accidentally.
func GetMaxRequestBodyBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("KIRO_MAX_REQUEST_BODY_BYTES"))
	if raw == "" {
		return defaultMaxRequestBodyBytes
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return defaultMaxRequestBodyBytes
	}
	return value
}
