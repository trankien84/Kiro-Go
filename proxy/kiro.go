// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"kiro-go/config"
	"kiro-go/diagnostics"
	"kiro-go/logger"
	"kiro-go/pool"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Endpoint configuration (auto-fallback on quota exhaustion).
type kiroEndpoint struct {
	URL       string
	Origin    string
	AmzTarget string
	Name      string
}

var (
	errEmptyKiroStream         = errors.New("upstream stream ended before any output")
	errIncompleteKiroToolInput = errors.New("upstream stream ended with incomplete tool input")
	errInvalidKiroEventStream  = errors.New("invalid upstream event stream")
	errKiroEventStreamUpstream = errors.New("upstream event stream error")
)

var kiroEndpoints = []kiroEndpoint{
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "",
		Name:      "Kiro IDE",
	},
	{
		URL:       "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		Name:      "CodeWhisperer",
	},
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonQDeveloperStreamingService.SendMessage",
		Name:      "AmazonQ",
	},
}

// kiroCLIEndpoint is the headless/API-key route used by Kiro CLI. The region
// is substituted by cliRuntimeURL for each account.
var kiroCLIEndpoint = kiroEndpoint{
	URL:       "https://runtime.us-east-1.kiro.dev/",
	Origin:    "KIRO_CLI",
	AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
	Name:      "Kiro CLI",
}

// Global HTTP clients, swappable at runtime to apply proxy reconfiguration without restart.
var kiroHttpStore atomic.Pointer[http.Client]
var kiroRestHttpStore atomic.Pointer[http.Client]

// Streaming requests have no wall-clock Client.Timeout. A no-progress watchdog
// is reset on every body read so healthy long-running agent streams can continue
// while genuinely stalled sockets are still interrupted.
const kiroStreamingIdleTimeout = 15 * time.Minute

// proxyClientCache caches http.Client instances keyed by proxy URL for per-account proxy support.
var proxyClientCache sync.Map

func init() {
	InitKiroHttpClient("")
}

// GetClientForProxy returns an http.Client configured for the given proxy URL.
// If proxyURL is empty, returns the global kiro HTTP client.
func GetClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroHttpStore.Load()
	}
	if cached, ok := proxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(proxyURL, client)
	return client
}

// GetRestClientForProxy returns a rest http.Client (30s timeout) for the given proxy URL.
// If proxyURL is empty, returns the global kiro REST HTTP client.
func GetRestClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroRestHttpStore.Load()
	}
	cacheKey := "rest:" + proxyURL
	if cached, ok := proxyClientCache.Load(cacheKey); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(cacheKey, client)
	return client
}

// ResolveAccountProxyURL returns the effective proxy URL for an account.
// Falls back to global config.GetProxyURL() if the account has no per-account proxy.
func ResolveAccountProxyURL(account *config.Account) string {
	if account != nil && account.ProxyURL != "" {
		return account.ProxyURL
	}
	return config.GetProxyURL()
}

// buildKiroTransport constructs an HTTP Transport with optional outbound proxy support.
func buildKiroTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			// Proxied connections cannot negotiate HTTP/2.
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}

// InitKiroHttpClient initializes (or reinitializes) the HTTP clients used for Kiro API requests.
func InitKiroHttpClient(proxyURL string) {
	client := &http.Client{
		Transport: buildKiroTransport(proxyURL),
	}
	kiroHttpStore.Store(client)

	restClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroRestHttpStore.Store(restClient)
}

// ==================== Request Structs ====================

// KiroPayload is the top-level request body sent to the Kiro API.
type KiroPayload struct {
	ConversationState struct {
		AgentContinuationId string `json:"agentContinuationId,omitempty"`
		AgentTaskType       string `json:"agentTaskType,omitempty"`
		ChatTriggerType     string `json:"chatTriggerType"`
		ConversationID      string `json:"conversationId"`
		CurrentMessage      struct {
			UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
		} `json:"currentMessage"`
		History []KiroHistoryMessage `json:"history,omitempty"`
	} `json:"conversationState"`
	ProfileArn      string           `json:"profileArn,omitempty"`
	InferenceConfig *InferenceConfig `json:"inferenceConfig,omitempty"`

	// Native model-specific request configuration discovered from
	// ListAvailableModels.additionalModelRequestFieldsSchema.
	AdditionalModelRequestFields map[string]interface{} `json:"additionalModelRequestFields,omitempty"`

	// ToolNameMap maps sanitized tool names (sent to Kiro) back to the
	// original names supplied by the client. Used to restore original names
	// in tool_use responses so the client can match them to its tool registry.
	// Not serialized to the Kiro API request body.
	ToolNameMap map[string]string `json:"-"`
	// ToolIdentityMap preserves Responses namespace/custom identity across Kiro's
	// flat tool-name surface. It is keyed by the exact Kiro wire name.
	ToolIdentityMap map[string]responsesToolIdentity `json:"-"`
}

type KiroUserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin"`
	Images                  []KiroImage              `json:"images,omitempty"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type UserInputMessageContext struct {
	Tools       []KiroToolWrapper `json:"tools,omitempty"`
	ToolResults []KiroToolResult  `json:"toolResults,omitempty"`

	// ToolChoice is an experimental Kiro/Bedrock-compatible hint.
	// The completion guard only attaches it as a bounded fallback.
	ToolChoice interface{} `json:"toolChoice,omitempty"`
}

type KiroToolWrapper struct {
	ToolSpecification struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		InputSchema InputSchema `json:"inputSchema"`
	} `json:"toolSpecification"`
}

type InputSchema struct {
	JSON interface{} `json:"json"`
}

type KiroToolResult struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []KiroResultContent `json:"content"`
	Status    string              `json:"status"`
}

type KiroResultContent struct {
	Text string `json:"text"`
}

type KiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

type KiroHistoryMessage struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type KiroAssistantResponseMessage struct {
	Content  string        `json:"content"`
	ToolUses []KiroToolUse `json:"toolUses,omitempty"`

	// ClaudeContentBlocks is an internal lossless sidecar. It is deliberately
	// excluded from the Kiro wire payload because Kiro's assistant history only
	// supports text plus client toolUses. The sidecar lets routing and tests
	// retain exact Claude server/MCP/thinking state while transparent transports
	// receive the original raw request.
	ClaudeContentBlocks []ClaudeContentBlock `json:"-"`
}

type KiroToolUse struct {
	ToolUseID string                 `json:"toolUseId"`
	Name      string                 `json:"name"`
	Input     map[string]interface{} `json:"input"`
	Namespace string                 `json:"-"`
	ToolType  string                 `json:"-"`
}

type InferenceConfig struct {
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"topP,omitempty"`

	// Explicit zero is meaningful for sampling controls. These flags preserve
	// temperature:0/topP:0 on the Kiro wire without changing the public numeric
	// fields used throughout the translator.
	TemperaturePresent bool `json:"-"`
	TopPPresent        bool `json:"-"`
}

func (c InferenceConfig) MarshalJSON() ([]byte, error) {
	result := make(map[string]interface{}, 3)
	if c.MaxTokens != 0 {
		result["maxTokens"] = c.MaxTokens
	}
	if c.TemperaturePresent || c.Temperature != 0 {
		result["temperature"] = c.Temperature
	}
	if c.TopPPresent || c.TopP != 0 {
		result["topP"] = c.TopP
	}
	return json.Marshal(result)
}

// ==================== Stream Callbacks ====================

// KiroStreamCallback stream response callbacks
type KiroStreamCallback struct {
	OnText               func(text string, isThinking bool)
	OnReasoningSignature func(signature string)
	OnReasoningRedacted  func(data string)
	OnToolUse            func(toolUse KiroToolUse)
	OnComplete           func(inputTokens, outputTokens int)
	OnError              func(err error)
	OnCredits            func(credits float64)
	OnContextUsage       func(percentage float64)
	OnMeteringEvent      func()
	OnStopReason         func(reason string)

	// CanRetry/ResetForRetry let fully-buffered protocol adapters opt into a
	// same-account retry after callbacks mutated only invisible attempt-local
	// state. Streaming callers leave them nil and retain the strict no-replay
	// boundary after the first semantic callback.
	CanRetry      func() bool
	ResetForRetry func()
	// Err lets a retaining consumer abort the upstream stream without panicking
	// (for example when a shared response-memory budget is exhausted).
	Err func() error
	// ReserveRetained and ReleaseRetained account for buffers owned temporarily
	// by the parser, most importantly fragmented tool input JSON.
	ReserveRetained func(size int) error
	ReleaseRetained func(size int)
}

// ==================== API Call ====================

func setPayloadProfileArnForAccount(payload *KiroPayload, account *config.Account) {
	if payload == nil {
		return
	}
	// API-key credentials are headless and must not carry IDE/profile semantics.
	if config.IsAPIKeyAccount(account) {
		payload.ProfileArn = ""
		return
	}
	payload.ProfileArn = strings.TrimSpace(payload.ProfileArn)
	if account != nil {
		if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
			payload.ProfileArn = profileArn
		}
	}
}

func endpointsForAccount(account *config.Account) []kiroEndpoint {
	if config.IsAPIKeyAccount(account) {
		return []kiroEndpoint{kiroCLIEndpoint}
	}
	return getSortedEndpoints(config.GetPreferredEndpoint())
}

func cliRuntimeURL(account *config.Account) string {
	region := "us-east-1"
	if account != nil {
		if r := strings.TrimSpace(strings.ToLower(account.Region)); r != "" && kiroRegionPattern.MatchString(r) {
			region = r
		}
	}
	return fmt.Sprintf("https://runtime.%s.kiro.dev/", region)
}

// getSortedEndpoints returns endpoints ordered by user preference, with optional fallback.
func getSortedEndpoints(preferred string) []kiroEndpoint {
	fallback := config.GetEndpointFallback()

	var primary int
	switch preferred {
	case "kiro":
		primary = 0
	case "codewhisperer":
		primary = 1
	case "amazonq":
		primary = 2
	default:
		// "auto": Kiro first, then fallback to others
		return []kiroEndpoint{kiroEndpoints[0], kiroEndpoints[1], kiroEndpoints[2]}
	}

	if !fallback {
		// No fallback: only use the selected endpoint
		return []kiroEndpoint{kiroEndpoints[primary]}
	}

	// With fallback: selected first, then others in order
	result := []kiroEndpoint{kiroEndpoints[primary]}
	for i, ep := range kiroEndpoints {
		if i != primary {
			result = append(result, ep)
		}
	}
	return result
}

// upstreamError builds a classifiable error from a non-200 Kiro response.
// 402 is tagged "overage" so the failover layer routes it to overage handling
// (disableAccountOverage → refresh OverageStatus) instead of falling through to
// the generic RecordError path. All other codes produce "HTTP <code> ...", which
// pool.IsAuthFailure reads via its digit-boundary status-token matcher.
func upstreamError(statusCode int, endpoint, body string) error {
	if statusCode == 402 {
		return fmt.Errorf("HTTP 402 overage from %s: %s", endpoint, body)
	}
	return fmt.Errorf("HTTP %d from %s: %s", statusCode, endpoint, body)
}

// parseAndStream wraps parseEventStream so CallKiroAPI's 200 path can defer the
// upstream body close (closing even on a callback panic, so the TCP connection
// is returned to the transport pool instead of leaking). Without the defer, a
// panic in OnText/OnToolUse/parseEventStream unwinds past a plain Close and
// leaks the connection (repeated panics exhaust MaxIdleConnsPerHost=20 / FDs;
// net/http's per-request recover catches the panic but the body stays open).
func parseAndStream(body io.ReadCloser, callback *KiroStreamCallback) error {
	defer body.Close()
	return parseEventStream(body, callback)
}

func parseAndStreamTracked(body io.ReadCloser, callback *KiroStreamCallback) (bool, error) {
	defer body.Close()
	return parseEventStreamTracked(body, callback)
}

// CallKiroAPI calls the Kiro streaming API using a background context. New
// request paths should call CallKiroAPIContext so downstream cancellation is
// propagated to the upstream socket.
func CallKiroAPI(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	return CallKiroAPIContext(context.Background(), account, payload, callback)
}

// CallKiroAPIContext calls the Kiro streaming API, trying each configured
// endpoint with automatic fallback while honoring cancellation for the entire
// outgoing request and response-body lifetime.
func validateKiroTransportAccount(account *config.Account) error {
	if account == nil {
		return errors.New("kiro transport: account is nil")
	}
	switch account.Kind() {
	case config.AccountKindKiroOAuth, config.AccountKindKiroAPIKey:
		return nil
	default:
		return fmt.Errorf("kiro transport: unsupported account kind %s", account.Kind())
	}
}

func persistConfirmedAPIKeyRegion(account *config.Account, region string) {
	if account == nil || strings.TrimSpace(account.ID) == "" {
		return
	}
	if account.DispatchGeneration != 0 {
		applied, err := config.UpdateAccountRegionIfLifecycle(account.ID, account.DispatchGeneration, region)
		if err != nil {
			logger.Warnf("[KiroAPI] failed to persist re-detected region %s for %s: %v", region, accountEmailForLog(account), err)
			return
		}
		if !applied {
			logger.Warnf("[KiroAPI] ignored stale re-detected region account=%s generation=%d region=%s", account.ID, account.DispatchGeneration, region)
		}
		return
	}
	if err := config.UpdateAccountRegion(account.ID, region); err != nil {
		logger.Warnf("[KiroAPI] failed to persist re-detected region %s for %s: %v", region, accountEmailForLog(account), err)
	}
}

func callKiroAPIContextOnce(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if err := validateKiroTransportAccount(account); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if payload == nil {
		return errors.New("kiro transport: payload is nil")
	}
	originalProfileArn := payload.ProfileArn
	defer func() { payload.ProfileArn = originalProfileArn }()
	setPayloadProfileArnForAccount(payload, account)

	if _, err := json.Marshal(payload); err != nil {
		return err
	}
	if payloadJSON, err := json.Marshal(payload); err == nil {
		logger.Debugf("[KiroAPI] Request payload: %s", string(payloadJSON))
	}

	// Restore client-visible tool names after Kiro's sanitised names return.
	if callback != nil && callback.OnToolUse != nil && (len(payload.ToolNameMap) > 0 || len(payload.ToolIdentityMap) > 0) {
		originalOnToolUse := callback.OnToolUse
		nameMap := payload.ToolNameMap
		identityMap := payload.ToolIdentityMap
		wrapped := *callback
		wrapped.OnToolUse = func(tu KiroToolUse) {
			if identity, ok := identityMap[tu.Name]; ok {
				tu.Name = identity.Name
				tu.Namespace = identity.Namespace
				tu.ToolType = identity.Kind
			} else if original, ok := nameMap[tu.Name]; ok {
				tu.Name = original
			}
			originalOnToolUse(tu)
		}
		callback = &wrapped
	}

	if strings.TrimSpace(payload.ProfileArn) == "" && !config.IsAPIKeyAccount(account) {
		if profileArn, err := ResolveProfileArn(account); err == nil {
			payload.ProfileArn = profileArn
		} else if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Skipped profile ARN resolution for %s: %v", accountEmailForLog(account), err)
		} else {
			logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmailForLog(account), err)
		}
	}

	emittedAny, err := callKiroEndpointsOnce(ctx, account, payload, callback)
	if err == nil {
		return nil
	}

	// API keys are bound to a Kiro runtime region. An auth failure on a request
	// that has not emitted semantic output may be a wrong-region failure rather
	// than an invalid key. Re-probe the REST plane, retry the candidate once on
	// the runtime plane, and persist only after runtime success.
	if !emittedAny && account.IsApiKeyCredential() &&
		pool.IsAuthFailure(err) && !pool.IsSuspensionError(err) {
		if account.DispatchGeneration != 0 &&
			!config.AccountLifecycleMatches(account.ID, account.DispatchGeneration) {
			return err
		}
		previousRegion := account.Region
		if region, ok := reprobeApiKeyRegion(account); ok {
			// The potentially slow REST probe may have raced a delete/recreate.
			// Never issue the confirming runtime request for a dispatch lease that
			// ceased to own this account while probing.
			if account.DispatchGeneration != 0 &&
				!config.AccountLifecycleMatches(account.ID, account.DispatchGeneration) {
				account.Region = previousRegion
				return err
			}
			_, retryErr := callKiroEndpointsOnce(ctx, account, payload, callback)
			if retryErr == nil {
				persistConfirmedAPIKeyRegion(account, region)
				return nil
			}
			// REST and runtime can disagree. Never leave the request snapshot on an
			// unconfirmed region after the runtime retry fails.
			account.Region = previousRegion
			return retryErr
		}
	}
	return err
}

// callKiroEndpointsOnce performs exactly one pass over the endpoint chain. It
// intentionally contains no transport retry loop: kiro_transport_recovery.go is
// the single owner of same-account transport/integrity retry budgeting.
func callKiroEndpointsOnce(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) (bool, error) {
	endpoints := endpointsForAccount(account)
	isAPIKey := config.IsAPIKeyAccount(account)
	var lastErr error

	for _, ep := range endpoints {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin

		epURL := regionalizeURLForProfile(ep.URL, account, payload.ProfileArn)
		if isAPIKey {
			epURL = cliRuntimeURL(account)
		}

		if diagnostics.Reasoning() && len(payload.AdditionalModelRequestFields) > 0 {
			logger.Infof("[KiroOutboundReasoning] endpoint=%s fields=%s", ep.Name, reasoningFieldsJSON(payload.AdditionalModelRequestFields))
		}

		reqBody, _ := json.Marshal(payload)
		requestCtx, cancelRequest := context.WithCancel(ctx)
		req, reqErr := http.NewRequestWithContext(requestCtx, http.MethodPost, epURL, bytes.NewReader(reqBody))
		if reqErr != nil {
			cancelRequest()
			lastErr = reqErr
			continue
		}

		host := ""
		if parsedURL, parseErr := url.Parse(epURL); parseErr == nil {
			host = parsedURL.Host
		}
		headerValues := buildStreamingHeaderValues(account, host)
		if isAPIKey {
			req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "*/*")
		if ep.AmzTarget != "" {
			req.Header.Set("X-Amz-Target", ep.AmzTarget)
		}
		applyKiroBaseHeaders(req, account, headerValues)
		if !isAPIKey {
			req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
		}
		if isAPIKey {
			req.Header.Set("x-amzn-codewhisperer-optout", "false")
		} else {
			req.Header.Set("x-amzn-codewhisperer-optout", "true")
		}
		applyKiroAttemptHeaders(req, ctx)

		resp, doErr := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
		if doErr != nil {
			cancelRequest()
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			lastErr = doErr
			logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, doErr)
			continue
		}
		recordKiroHTTPResponseTrace(ctx, resp)
		resp.Body = newKiroTraceReadCloser(ctx, resp.Body)
		resp.Body = newIdleWatchdogReadCloser(resp.Body, kiroStreamingIdleTimeout, cancelRequest)

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			cancelRequest()
			lastErr = fmt.Errorf("quota exhausted on %s", ep.Name)
			logger.Warnf("[KiroAPI] Endpoint %s quota exhausted (429), trying next...", ep.Name)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancelRequest()
			lastErr = upstreamError(resp.StatusCode, ep.Name, string(errBody))
			if resp.StatusCode == http.StatusUnauthorized ||
				resp.StatusCode == http.StatusForbidden ||
				resp.StatusCode == http.StatusPaymentRequired {
				return false, lastErr
			}
			logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, lastErr)
			continue
		}

		emitted, parseErr := parseAndStreamTracked(resp.Body, callback)
		cancelRequest()
		if parseErr != nil {
			logger.Warnf("[KiroAPI] Endpoint %s stream failed: %v", ep.Name, parseErr)
			return emitted, parseErr
		}
		return emitted, nil
	}
	if lastErr != nil {
		return false, lastErr
	}
	return false, errors.New("all endpoints failed")
}

func accountEmailForLog(account *config.Account) string {
	if account == nil {
		return "<nil>"
	}
	return account.Email
}

// ==================== Event Stream Parsing ====================

// maxEventStreamMessageBytes caps a single AWS event-stream message's total
// length before parseEventStream allocates a buffer for it. Real Kiro/AWS
// event-stream messages are small (text deltas + tool JSON, low KB); a corrupt
// or malicious 32-bit totalLength near 2^32 would otherwise drive a multi-GB
// make([]byte, …) (alloc-panic under net/http's recover → connection dropped
// with no terminal event → client hang) or hold a multi-GB buffer to the
// 5-minute client timeout (memory-pressure DoS). 16 MiB is a generous ceiling.
const maxEventStreamMessageBytes = 16 * 1024 * 1024

// errEventStreamFrameTooLarge is returned by parseEventStream when a frame's
// totalLength exceeds maxEventStreamMessageBytes, so the caller (CallKiroAPI)
// funnels it into the upstream-error / response.failed path instead of
// allocating gigabytes.
var errEventStreamFrameTooLarge = errors.New("event-stream: frame totalLength exceeds maximum")

func diagnosticPreview(value string) string {
	const maxRunes = 512

	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}

	return string(runes[:maxRunes]) + "…[truncated]"
}

// parseEventStream decodes an AWS binary Event Stream response body.
func parseEventStream(body io.Reader, callback *KiroStreamCallback) error {
	_, err := parseEventStreamTracked(body, callback)
	return err
}

// parseEventStreamTracked validates AWS Event Stream framing while reporting
// whether any semantic callback was invoked. The outer transport recovery uses
// this conservative boundary to prevent replay after visible output.
func parseEventStreamTracked(body io.Reader, callback *KiroStreamCallback) (emitted bool, err error) {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}

	var inputTokens, outputTokens int
	var totalCredits float64
	var contextUsagePercentages []float64
	var sawOutput bool
	pending := &pendingToolUses{}

	trackedCallback := *callback
	originalOnToolUse := trackedCallback.OnToolUse
	trackedCallback.OnToolUse = func(toolUse KiroToolUse) {
		sawOutput = true
		if originalOnToolUse != nil {
			emitted = true
			originalOnToolUse(toolUse)
		}
	}
	callback = &trackedCallback
	defer pending.releaseAll(callback)

	var eventCount int
	var assistantEventCount int
	var reasoningEventCount int
	var reasoningSignatureEventCount int
	var reasoningRedactedEventCount int
	var toolEventCount int
	var lastEventType string
	var assistantChars int
	var reasoningChars int
	var wireBytes int64
	var completeFrames int

	assistantMode, invalidAssistantMode := configuredStreamTextMode(
		assistantChunkModeEnv,
		streamTextModeDelta,
	)
	reasoningMode, invalidReasoningMode := configuredStreamTextMode(
		reasoningChunkModeEnv,
		streamTextModeDelta,
	)
	assistantDecoder := newStreamTextDecoder(assistantMode)
	reasoningDecoder := newStreamTextDecoder(reasoningMode)
	assistantDecoderRetained := 0
	reasoningDecoderRetained := 0
	adjustDecoderRetention := func(tracked *int, size int) error {
		if tracked == nil || callback.ReserveRetained == nil {
			return nil
		}
		delta := size - *tracked
		if delta > 0 {
			if err := callback.ReserveRetained(delta); err != nil {
				return err
			}
		} else if delta < 0 && callback.ReleaseRetained != nil {
			callback.ReleaseRetained(-delta)
		}
		*tracked = size
		return nil
	}
	defer func() {
		if callback.ReleaseRetained == nil {
			return
		}
		callback.ReleaseRetained(assistantDecoderRetained)
		callback.ReleaseRetained(reasoningDecoderRetained)
	}()

	if invalidAssistantMode != "" {
		logger.Warnf(
			"[StreamChunkMode] invalid %s=%q; using %s",
			assistantChunkModeEnv,
			invalidAssistantMode,
			assistantMode,
		)
	}
	if invalidReasoningMode != "" {
		logger.Warnf(
			"[StreamChunkMode] invalid %s=%q; using %s",
			reasoningChunkModeEnv,
			invalidReasoningMode,
			reasoningMode,
		)
	}

	for {
		// AWS Event Stream prelude: total length, headers length, prelude CRC.
		prelude := make([]byte, 12)
		n, readErr := io.ReadFull(body, prelude)
		wireBytes += int64(n)
		if readErr != nil && !(readErr == io.EOF && n == 0) {
			return emitted, newKiroStreamReadError(
				kiroStreamReadPreludeCRC,
				n,
				len(prelude),
				wireBytes,
				completeFrames,
				lastEventType,
				readErr,
			)
		}
		if readErr == io.EOF {
			if diagnostics.Stream() {
				logger.Infof(
					"[KiroStream] EOF events=%d last=%q assistantEvents=%d reasoningEvents=%d reasoningSignatures=%d reasoningRedacted=%d reasoningChars=%d toolEvents=%d assistantChars=%d inputTokens=%d outputTokens=%d credits=%.3f",
					eventCount,
					lastEventType,
					assistantEventCount,
					reasoningEventCount,
					reasoningSignatureEventCount,
					reasoningRedactedEventCount,
					reasoningChars,
					toolEventCount,
					assistantChars,
					inputTokens,
					outputTokens,
					totalCredits,
				)
			}
			break
		}

		totalLength := int(eventStreamUint32(prelude[0:4]))
		headersLength := int(eventStreamUint32(prelude[4:8]))
		if totalLength < 16 {
			return emitted, fmt.Errorf("%w: invalid frame length %d", errInvalidKiroEventStream, totalLength)
		}
		if totalLength > maxEventStreamMessageBytes {
			return emitted, errEventStreamFrameTooLarge
		}
		if headersLength > totalLength-16 {
			return emitted, fmt.Errorf("%w: invalid headers length %d", errInvalidKiroEventStream, headersLength)
		}
		if got, want := eventStreamUint32(prelude[8:12]), crc32.ChecksumIEEE(prelude[:8]); got != want {
			return emitted, fmt.Errorf("%w: prelude CRC mismatch", errInvalidKiroEventStream)
		}

		remaining := totalLength - len(prelude)
		message := make([]byte, remaining)
		n, readErr = io.ReadFull(body, message)
		wireBytes += int64(n)
		if readErr != nil {
			return emitted, newKiroStreamReadError(
				kiroStreamReadFrameBody,
				n,
				len(message),
				wireBytes,
				completeFrames,
				lastEventType,
				readErr,
			)
		}
		completeFrames++
		if got, want := eventStreamUint32(message[len(message)-4:]), eventStreamChecksum(prelude, message[:len(message)-4]); got != want {
			return emitted, fmt.Errorf("%w: message CRC mismatch", errInvalidKiroEventStream)
		}

		headers, headerErr := parseEventStreamHeaders(message[:headersLength])
		if headerErr != nil {
			return emitted, fmt.Errorf("%w: %v", errInvalidKiroEventStream, headerErr)
		}
		eventType := headers[":event-type"]
		lastEventType = eventType

		payloadBytes := message[headersLength : len(message)-4]
		event := make(map[string]interface{})
		if len(payloadBytes) > 0 {
			if err := json.Unmarshal(payloadBytes, &event); err != nil {
				return emitted, fmt.Errorf("%w: decode payload: %v", errInvalidKiroEventStream, err)
			}
		}

		if messageType := headers[":message-type"]; messageType == "error" || messageType == "exception" {
			detail, _ := event["message"].(string)
			if detail == "" {
				detail = messageType
			}
			return emitted, fmt.Errorf("%w: %s", errKiroEventStreamUpstream, detail)
		}

		if diagnostics.Stream() {
			eventCount++
		}

		if diagnostics.Reasoning() && eventType == "reasoningContentEvent" {
			keys := make([]string, 0, len(event))
			for key := range event {
				keys = append(keys, key)
			}
			sort.Strings(keys)

			text, _ := event["text"].(string)
			signature, _ := event["signature"].(string)
			redactedContent, _ := event["redactedContent"].(string)
			blockID := firstStringField(event, "id", "blockId", "contentBlockId", "reasoningId")
			toolUseID := firstStringField(event, "toolUseId", "toolUseID", "callId")
			logger.Infof(
				"[KiroReasoningEvent] keys=%v text_len=%d signature_present=%t signature_len=%d redacted_present=%t redacted_len=%d block_id=%q tool_use_id=%q",
				keys,
				len([]rune(text)),
				signature != "",
				len(signature),
				redactedContent != "",
				len(redactedContent),
				blockID,
				toolUseID,
			)
		}

		inputTokens, outputTokens = updateTokensFromEvent(event, inputTokens, outputTokens)

		switch eventType {
		case "assistantResponseEvent":
			if content, ok := event["content"].(string); ok && content != "" {
				sawOutput = true
				result := assistantDecoder.Push(content)
				if err := adjustDecoderRetention(&assistantDecoderRetained, len(assistantDecoder.previous)); err != nil {
					return emitted, err
				}
				if result.ContractViolation {
					logger.Warnf(
						"[StreamChunkMode] type=assistant contract_violation mode=%s previous=%q raw=%q fallback=%s",
						result.ModeBefore,
						diagnosticPreview(result.Previous),
						diagnosticPreview(content),
						result.ModeAfter,
					)
				}
				if diagnostics.Stream() && result.Text != "" {
					assistantEventCount++
					assistantChars += len([]rune(result.Text))
				}
				if diagnostics.Chunks() {
					logger.Infof(
						"[StreamChunk] type=assistant mode=%s previous=%q raw=%q emitted=%q fallback=%t",
						result.ModeBefore,
						diagnosticPreview(result.Previous),
						diagnosticPreview(content),
						diagnosticPreview(result.Text),
						result.ContractViolation,
					)
				}
				if result.Text != "" && callback.OnText != nil {
					emitted = true
					callback.OnText(result.Text, false)
				}
			}

		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				sawOutput = true
				result := reasoningDecoder.Push(text)
				if err := adjustDecoderRetention(&reasoningDecoderRetained, len(reasoningDecoder.previous)); err != nil {
					return emitted, err
				}
				if result.ContractViolation {
					logger.Warnf(
						"[StreamChunkMode] type=reasoning contract_violation mode=%s previous=%q raw=%q fallback=%s",
						result.ModeBefore,
						diagnosticPreview(result.Previous),
						diagnosticPreview(text),
						result.ModeAfter,
					)
				}
				if diagnostics.Stream() && result.Text != "" {
					reasoningEventCount++
					reasoningChars += len([]rune(result.Text))
				}
				if diagnostics.Chunks() {
					logger.Infof(
						"[StreamChunk] type=reasoning mode=%s previous=%q raw=%q emitted=%q fallback=%t",
						result.ModeBefore,
						diagnosticPreview(result.Previous),
						diagnosticPreview(text),
						diagnosticPreview(result.Text),
						result.ContractViolation,
					)
				}
				if result.Text != "" && callback.OnText != nil {
					emitted = true
					callback.OnText(result.Text, true)
				}
			} else if signature, ok := event["signature"].(string); ok && signature != "" {
				sawOutput = true
				if diagnostics.Stream() {
					reasoningSignatureEventCount++
				}
				if callback.OnReasoningSignature != nil {
					emitted = true
					callback.OnReasoningSignature(signature)
				}
			} else if redacted, ok := event["redactedContent"].(string); ok && redacted != "" {
				sawOutput = true
				if diagnostics.Stream() {
					reasoningRedactedEventCount++
				}
				if callback.OnReasoningRedacted != nil {
					emitted = true
					callback.OnReasoningRedacted(redacted)
				}
			}

		case "toolUseEvent":
			if diagnostics.Stream() {
				toolEventCount++
			}
			if toolErr := handleToolUseEvent(event, pending, callback); toolErr != nil {
				return emitted, toolErr
			}

		case "meteringEvent":
			if usage, ok := event["usage"].(float64); ok {
				totalCredits += usage
			}
			if callback.OnMeteringEvent != nil {
				callback.OnMeteringEvent()
			}

		case "contextUsageEvent":
			if pct, ok := event["contextUsagePercentage"].(float64); ok {
				contextUsagePercentages = append(contextUsagePercentages, pct)
			}

		case "metadataEvent":
			if reason := firstStringField(event, "stopReason", "stop_reason"); reason != "" && callback.OnStopReason != nil {
				callback.OnStopReason(reason)
			}

		default:
			logger.Debugf("[KiroStream] unhandled event type=%q", eventType)
		}

		if callback.Err != nil {
			if err := callback.Err(); err != nil {
				return emitted, err
			}
		}
	}

	if err := pending.flushAll(callback); err != nil {
		return emitted, err
	}
	if callback.Err != nil {
		if err := callback.Err(); err != nil {
			return emitted, err
		}
	}
	if !sawOutput {
		return emitted, errEmptyKiroStream
	}
	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}
	if callback.OnContextUsage != nil {
		for _, percentage := range contextUsagePercentages {
			callback.OnContextUsage(percentage)
		}
	}
	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	return emitted, nil
}

func updateTokensFromEvent(event map[string]interface{}, currentInputTokens, currentOutputTokens int) (int, int) {
	candidates := []map[string]interface{}{event}
	collectUsageMaps(event, &candidates)

	inputTokens := currentInputTokens
	outputTokens := currentOutputTokens

	for _, usage := range candidates {
		if usage == nil {
			continue
		}

		if v, ok := readTokenNumber(usage,
			"outputTokens", "completionTokens", "totalOutputTokens",
			"output_tokens", "completion_tokens", "total_output_tokens",
		); ok {
			outputTokens = v
		}

		if v, ok := readTokenNumber(usage,
			"inputTokens", "promptTokens", "totalInputTokens",
			"input_tokens", "prompt_tokens", "total_input_tokens",
		); ok {
			inputTokens = v
			continue
		}

		uncached, _ := readTokenNumber(usage, "uncachedInputTokens", "uncached_input_tokens")
		cacheRead, _ := readTokenNumber(usage, "cacheReadInputTokens", "cache_read_input_tokens")
		cacheWrite, _ := readTokenNumber(usage, "cacheWriteInputTokens", "cache_write_input_tokens", "cacheCreationInputTokens", "cache_creation_input_tokens")
		if uncached+cacheRead+cacheWrite > 0 {
			inputTokens = uncached + cacheRead + cacheWrite
			continue
		}

		total, ok := readTokenNumber(usage, "totalTokens", "total_tokens")
		if ok && total > 0 {
			candidateOutput := outputTokens
			if v, vok := readTokenNumber(usage,
				"outputTokens", "completionTokens", "totalOutputTokens",
				"output_tokens", "completion_tokens", "total_output_tokens",
			); vok {
				candidateOutput = v
			}
			if total-candidateOutput > 0 {
				inputTokens = total - candidateOutput
			}
		}
	}

	return inputTokens, outputTokens
}

// getContextWindowSize returns the context window size (in tokens) for a model.
//
// Per Kiro's ListAvailableModels, the 1M-token context window applies to
// Claude 4.6 and newer (sonnet-4.6, opus-4.6, opus-4.7, opus-4.8, and future
// 4.x releases), while 4.5 and earlier (opus-4.5, sonnet-4.5, sonnet-4,
// haiku-4.5) use a 200K window. This value is used to convert the upstream
// contextUsagePercentage into an absolute input-token count that clients rely
// on to decide when to compact; an undersized window under-reports tokens and
// prevents clients from compacting in time.
func getContextWindowSize(model string) int {
	// Conservative, name-based fallback only. Selected-account live token limits
	// are preferred by usageInputFromContextPercentage.
	base := strings.ToLower(strings.TrimSpace(model))
	if base == "gpt-5.6" || base == "gpt-5.6-sol" || base == "gpt-5.6-terra" || base == "gpt-5.6-luna" {
		return 1_000_000
	}
	if isLargeContextModel(model) {
		return 1_000_000
	}
	return 200_000
}

// claudeVersionExtractor matches "claude-<family>-<major>[.<minor>]" (dot or
// dash form). The minor component is optional so major-only identifiers such as
// "claude-opus-5" classify as 1M-context models instead of falling back to 200K.
var claudeVersionExtractor = regexp.MustCompile(`claude-(?:opus|sonnet|haiku)-(\d+)(?:[.-](\d+))?`)

func isLargeContextModel(model string) bool {
	m := strings.ToLower(model)
	if match := claudeVersionExtractor.FindStringSubmatch(m); match != nil {
		major, errMaj := strconv.Atoi(match[1])
		if errMaj == nil {
			if major > 4 {
				return true
			}
			minor := 0
			if match[2] != "" {
				parsed, errMin := strconv.Atoi(match[2])
				if errMin != nil {
					return false
				}
				minor = parsed
			}
			return major == 4 && minor >= 6
		}
	}
	// Fallback substring checks for non-standard identifiers.
	for _, tag := range []string{"4.6", "4-6", "4.7", "4-7", "4.8", "4-8", "4.9", "4-9"} {
		if strings.Contains(m, tag) {
			return true
		}
	}
	return false
}

func collectUsageMaps(v interface{}, out *[]map[string]interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			lk := strings.ToLower(k)
			if lk == "usage" || lk == "tokenusage" || lk == "token_usage" {
				if m, ok := child.(map[string]interface{}); ok {
					*out = append(*out, m)
				}
			}
			collectUsageMaps(child, out)
		}
	case []interface{}:
		for _, child := range t {
			collectUsageMaps(child, out)
		}
	}
}

func readTokenNumber(m map[string]interface{}, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		case json.Number:
			if parsed, err := n.Int64(); err == nil {
				return int(parsed), true
			}
		case string:
			if parsed, err := strconv.Atoi(n); err == nil {
				return parsed, true
			}
			if parsed, err := strconv.ParseFloat(n, 64); err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}

// ==================== Tool Use Handling ====================

type toolUseState struct {
	ToolUseID     string
	Name          string
	InputBuffer   strings.Builder
	GeneratedID   bool
	retainedBytes int
	err           error
}

func newToolUseState(toolUseID, name string, generatedID bool, callback *KiroStreamCallback) *toolUseState {
	state := &toolUseState{
		ToolUseID:   toolUseID,
		Name:        name,
		GeneratedID: generatedID,
	}
	reserveToolUseState(state, callback, len(toolUseID)+len(name))
	return state
}

func reserveToolUseState(state *toolUseState, callback *KiroStreamCallback, size int) bool {
	if state == nil || size <= 0 || callback == nil || callback.ReserveRetained == nil {
		return true
	}
	if state.err != nil {
		return false
	}
	if err := callback.ReserveRetained(size); err != nil {
		state.err = err
		return false
	}
	state.retainedBytes += size
	return true
}

func releaseToolUseStateBytes(state *toolUseState, callback *KiroStreamCallback, size int) {
	if state == nil || size <= 0 || state.retainedBytes <= 0 {
		return
	}
	if size > state.retainedBytes {
		size = state.retainedBytes
	}
	if callback != nil && callback.ReleaseRetained != nil {
		callback.ReleaseRetained(size)
	}
	state.retainedBytes -= size
}

func resetToolUseInput(state *toolUseState, callback *KiroStreamCallback) {
	if state == nil {
		return
	}
	releaseToolUseStateBytes(state, callback, state.InputBuffer.Len())
	state.InputBuffer = strings.Builder{}
}

func releaseToolUseState(state *toolUseState, callback *KiroStreamCallback) {
	if state == nil {
		return
	}
	releaseToolUseStateBytes(state, callback, state.retainedBytes)
	state.InputBuffer = strings.Builder{}
}

// pendingToolUses tracks independent, possibly interleaved tool calls by id.
// order preserves upstream arrival order, which is semantic for clients.
type pendingToolUses struct {
	byID   map[string]*toolUseState
	order  []string
	lastID string
}

func (p *pendingToolUses) get(id string) *toolUseState {
	if p == nil || p.byID == nil {
		return nil
	}
	return p.byID[id]
}

func (p *pendingToolUses) add(state *toolUseState) {
	if p.byID == nil {
		p.byID = make(map[string]*toolUseState)
	}
	p.byID[state.ToolUseID] = state
	p.order = append(p.order, state.ToolUseID)
	p.lastID = state.ToolUseID
}

// rekey moves a synthetic opening id to the real id revealed by a later frame,
// while preserving arrival order and retained-memory accounting.
func (p *pendingToolUses) rekey(state *toolUseState, newID string, callback *KiroStreamCallback) error {
	if p == nil || state == nil || newID == "" || state.ToolUseID == newID {
		return nil
	}
	if existing := p.get(newID); existing != nil && existing != state {
		return fmt.Errorf("%w: duplicate tool use id %q", errInvalidKiroEventStream, newID)
	}
	if !reserveToolUseState(state, callback, len(newID)) {
		return state.err
	}
	oldID := state.ToolUseID
	delete(p.byID, oldID)
	for i, id := range p.order {
		if id == oldID {
			p.order[i] = newID
			break
		}
	}
	releaseToolUseStateBytes(state, callback, len(oldID))
	state.ToolUseID = newID
	state.GeneratedID = false
	p.byID[newID] = state
	if p.lastID == oldID {
		p.lastID = newID
	}
	return nil
}

func (p *pendingToolUses) remove(id string) {
	if p == nil {
		return
	}
	delete(p.byID, id)
	for i, existing := range p.order {
		if existing == id {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	if p.lastID == id {
		p.lastID = ""
	}
}

func (p *pendingToolUses) releaseAll(callback *KiroStreamCallback) {
	if p == nil {
		return
	}
	for _, state := range p.byID {
		releaseToolUseState(state, callback)
	}
	p.byID = nil
	p.order = nil
	p.lastID = ""
}

// flushAll validates and emits tools that ended only when the stream itself
// ended. Invalid/incomplete JSON remains retryable rather than becoming an
// empty or malformed tool call.
func (p *pendingToolUses) flushAll(callback *KiroStreamCallback) error {
	if p == nil {
		return nil
	}
	order := append([]string(nil), p.order...)
	byID := p.byID
	p.byID = nil
	p.order = nil
	p.lastID = ""
	for _, id := range order {
		state := byID[id]
		if state == nil {
			continue
		}
		if err := finishToolUse(state, callback); err != nil {
			// Release states that were not reached after the first validation error.
			for _, remainingID := range order {
				if remainingID == id {
					continue
				}
				if remaining := byID[remainingID]; remaining != nil {
					releaseToolUseState(remaining, callback)
					delete(byID, remainingID)
				}
			}
			return err
		}
		delete(byID, id)
	}
	return nil
}

// handleToolUseEvent accumulates one tool frame into its independent pending
// state. Frames without an id continue the most recently seen tool, while a
// late real id rekeys an earlier synthetic opening instead of splitting one call.
func handleToolUseEvent(event map[string]interface{}, pending *pendingToolUses, callback *KiroStreamCallback) error {
	if pending == nil {
		return errors.New("tool use: nil pending state")
	}
	toolUseID := firstStringField(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstStringField(event, "name", "toolName", "tool_name")
	isStop := firstBoolField(event, "stop", "isStop", "done")

	var state *toolUseState
	switch {
	case toolUseID != "":
		state = pending.get(toolUseID)
		if state == nil && pending.lastID != "" {
			if prev := pending.get(pending.lastID); prev != nil && prev.GeneratedID && (name == "" || prev.Name == name) {
				if err := pending.rekey(prev, toolUseID, callback); err != nil {
					return err
				}
				state = prev
			}
		}
		if state == nil {
			if name == "" {
				return nil
			}
			state = newToolUseState(toolUseID, name, false, callback)
			pending.add(state)
		} else {
			if name != "" && state.Name == "" {
				if !reserveToolUseState(state, callback, len(name)) {
					return state.err
				}
				state.Name = name
			}
			pending.lastID = state.ToolUseID
		}

	case pending.lastID != "" && pending.get(pending.lastID) != nil:
		state = pending.get(pending.lastID)
		if name != "" && state.Name != name {
			oldID := state.ToolUseID
			if err := finishToolUse(state, callback); err != nil {
				return err
			}
			pending.remove(oldID)
			state = newToolUseState("toolu_"+uuid.New().String(), name, true, callback)
			pending.add(state)
		}

	case name != "":
		state = newToolUseState("toolu_"+uuid.New().String(), name, true, callback)
		pending.add(state)

	default:
		return nil
	}

	if state.err != nil {
		return state.err
	}
	if input, ok := event["input"].(string); ok {
		if !reserveToolUseState(state, callback, len(input)) {
			return state.err
		}
		state.InputBuffer.WriteString(input)
	} else if inputObj, ok := event["input"].(map[string]interface{}); ok {
		data, err := json.Marshal(inputObj)
		if err != nil {
			return fmt.Errorf("%w: encode tool input: %v", errIncompleteKiroToolInput, err)
		}
		resetToolUseInput(state, callback)
		if !reserveToolUseState(state, callback, len(data)) {
			return state.err
		}
		state.InputBuffer.Write(data)
	}

	if isStop {
		id := state.ToolUseID
		if err := finishToolUse(state, callback); err != nil {
			return err
		}
		pending.remove(id)
	}
	return nil
}

func finishToolUse(state *toolUseState, callback *KiroStreamCallback) error {
	if state == nil {
		return nil
	}
	defer releaseToolUseState(state, callback)
	if state.err != nil {
		return state.err
	}
	if state.Name == "" {
		return nil
	}
	if state.ToolUseID == "" {
		state.ToolUseID = "toolu_" + uuid.New().String()
	}
	var input map[string]interface{}
	if state.InputBuffer.Len() > 0 {
		if err := json.Unmarshal([]byte(state.InputBuffer.String()), &input); err != nil {
			return fmt.Errorf("%w: %v", errIncompleteKiroToolInput, err)
		}
	}
	if input == nil {
		input = make(map[string]interface{})
	}
	if callback != nil && callback.OnToolUse != nil {
		callback.OnToolUse(KiroToolUse{
			ToolUseID: state.ToolUseID,
			Name:      state.Name,
			Input:     input,
		})
	}
	return nil
}

func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstBoolField(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if v, ok := m[key].(bool); ok {
			return v
		}
	}
	return false
}

func eventStreamUint32(data []byte) uint32 {
	return uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
}

func eventStreamChecksum(prelude, message []byte) uint32 {
	checksum := crc32.Update(0, crc32.IEEETable, prelude)
	return crc32.Update(checksum, crc32.IEEETable, message)
}

// extractEventType is shared with other AWS Event Stream consumers. Delegating
// to the bounds-checked parser prevents divergent framing rules.
func extractEventType(headers []byte) string {
	parsed, err := parseEventStreamHeaders(headers)
	if err != nil {
		return ""
	}
	return parsed[":event-type"]
}

func parseEventStreamHeaders(data []byte) (map[string]string, error) {
	headers := make(map[string]string)
	for offset := 0; offset < len(data); {
		nameLength := int(data[offset])
		offset++
		if nameLength == 0 || offset+nameLength >= len(data) {
			return nil, errors.New("malformed event header name")
		}
		name := string(data[offset : offset+nameLength])
		offset += nameLength
		valueType := data[offset]
		offset++

		valueLength := 0
		switch valueType {
		case 0, 1:
			continue
		case 2:
			valueLength = 1
		case 3:
			valueLength = 2
		case 4:
			valueLength = 4
		case 5, 8:
			valueLength = 8
		case 9:
			valueLength = 16
		case 6, 7:
			if offset+2 > len(data) {
				return nil, errors.New("malformed variable event header")
			}
			valueLength = int(data[offset])<<8 | int(data[offset+1])
			offset += 2
		default:
			return nil, fmt.Errorf("unsupported event header type %d", valueType)
		}
		if offset+valueLength > len(data) {
			return nil, errors.New("truncated event header value")
		}
		if valueType == 7 {
			headers[name] = string(data[offset : offset+valueLength])
		}
		offset += valueLength
	}
	return headers, nil
}
