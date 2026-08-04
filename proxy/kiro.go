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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	// streamRetryBackoff spaces out a retry of a stream that died before
	// delivering any output callback. Upstream drops cluster in time, so an
	// immediate retry tends to hit the same blip.
	streamRetryBackoff           = 700 * time.Millisecond
	maxStreamAttemptsPerEndpoint = 2
)

var (
	errEmptyKiroStream         = errors.New("upstream stream ended before any output")
	errIncompleteKiroToolInput = errors.New("upstream stream ended with incomplete tool input")
	errInvalidKiroEventStream  = errors.New("invalid upstream event stream")
	errKiroEventStreamUpstream = errors.New("upstream event stream error")
	streamRetryWait            = waitForStreamRetry
	resolveKiroEndpoints       = endpointsForAccount
)

// Endpoint configuration (auto-fallback on quota exhaustion).
type kiroEndpoint struct {
	URL       string
	Origin    string
	AmzTarget string
	Name      string
}

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

// kiroCLIEndpoint is the headless / API Key path used by Kiro CLI:
// POST https://runtime.{region}.kiro.dev/ with AWS JSON 1.0 protocol.
var kiroCLIEndpoint = kiroEndpoint{
	URL:       "https://runtime.us-east-1.kiro.dev/",
	Origin:    "KIRO_CLI",
	AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
	Name:      "Kiro CLI",
}

// Global HTTP clients, swappable at runtime to apply proxy reconfiguration without restart.
var kiroHttpStore atomic.Pointer[http.Client]
var kiroRestHttpStore atomic.Pointer[http.Client]

// Allow long-running agent streams to finish.
const kiroStreamingTimeout = 15 * time.Minute

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
		Timeout:   kiroStreamingTimeout,
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
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
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
		Timeout:   kiroStreamingTimeout,
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
}

type KiroToolUse struct {
	ToolUseID string                 `json:"toolUseId"`
	Name      string                 `json:"name"`
	Input     map[string]interface{} `json:"input"`
}

type InferenceConfig struct {
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"topP,omitempty"`
}

// ==================== Stream Callbacks ====================

// KiroStreamCallback stream response callbacks
type KiroStreamCallback struct {
	OnText         func(text string, isThinking bool)
	OnToolUse      func(toolUse KiroToolUse)
	OnComplete     func(inputTokens, outputTokens int)
	OnError        func(err error)
	OnCredits      func(credits float64)
	OnContextUsage func(percentage float64)
	OnStopReason   func(reason string)
}

// ==================== API Call ====================

func setPayloadProfileArnForAccount(payload *KiroPayload, account *config.Account) {
	if payload == nil {
		return
	}

	// API Key credentials must not carry IDE/profile semantics.
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

// endpointsForAccount returns the upstream endpoint list for a credential.
// API Key accounts always use the CLI runtime protocol; OAuth accounts keep
// the configured preferred-endpoint fallback chain.
func endpointsForAccount(account *config.Account) []kiroEndpoint {
	if config.IsAPIKeyAccount(account) {
		return []kiroEndpoint{kiroCLIEndpoint}
	}
	return getSortedEndpoints(config.GetPreferredEndpoint())
}

// cliRuntimeURL builds the regional Kiro CLI runtime URL.
func cliRuntimeURL(account *config.Account) string {
	region := "us-east-1"
	if account != nil {
		if r := strings.TrimSpace(account.Region); r != "" {
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

// parseAndStreamTracked is parseAndStream's retry-aware form: it reports whether
// any output callback ran before the error, which is what lets the stream-retry
// loop tell "died before the client saw anything" (safe to retry) from "died
// mid-stream" (must not retry, or the client gets two concatenated attempts).
// The deferred close is the point of the wrapper — see parseAndStream.
func parseAndStreamTracked(body io.ReadCloser, callback *KiroStreamCallback) (emitted bool, err error) {
	defer body.Close()
	return parseEventStreamTracked(body, callback)
}

// CallKiroAPI calls the Kiro streaming API, trying each configured endpoint with automatic fallback.
func CallKiroAPI(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	return CallKiroAPIContext(context.Background(), account, payload, callback)
}

// CallKiroAPIContext calls Kiro while respecting cancellation from the client request.
func CallKiroAPIContext(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
	originalProfileArn := ""
	if payload != nil {
		originalProfileArn = payload.ProfileArn
		defer func() {
			payload.ProfileArn = originalProfileArn
		}()
	}
	setPayloadProfileArnForAccount(payload, account)

	if _, err := json.Marshal(payload); err != nil {
		return err
	}

	// Debug: dump full payload for troubleshooting upstream rejections
	if payloadJSON, err := json.Marshal(payload); err == nil {
		logger.Debugf("[KiroAPI] Request payload: %s", string(payloadJSON))
	}

	// Wrap OnToolUse to restore original tool names for the client.
	if callback != nil && callback.OnToolUse != nil && len(payload.ToolNameMap) > 0 {
		originalOnToolUse := callback.OnToolUse
		nameMap := payload.ToolNameMap
		wrapped := *callback
		wrapped.OnToolUse = func(tu KiroToolUse) {
			if original, ok := nameMap[tu.Name]; ok {
				tu.Name = original
			}
			originalOnToolUse(tu)
		}
		callback = &wrapped
	}

	if payload != nil && strings.TrimSpace(payload.ProfileArn) == "" && !config.IsAPIKeyAccount(account) {
		if profileArn, err := ResolveProfileArn(account); err == nil {
			payload.ProfileArn = profileArn
		} else if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Skipped profile ARN resolution for %s: %v", accountEmailForLog(account), err)
		} else {
			logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmailForLog(account), err)
		}
	}

	emittedAny, err := callKiroEndpointsOnce(ctx, account, payload, callback)
	// A Kiro API-key (ksk_) key is region-bound: a request sent to the wrong
	// data-plane region is rejected as an invalid bearer token (HTTP 401/403) even
	// though the key is valid elsewhere. On that auth failure, re-detect the key's
	// region and, if a different region accepts it, retry once there.
	//
	// The retry is gated on !emittedAny — NOT merely on the error being pre-stream
	// by construction — because pool.IsAuthFailure is a substring/status-token
	// matcher over the error message (it also matches "unauthorized", "invalid
	// token", a bare "403" in an upstream error body, etc.), so it can in principle
	// be satisfied by an error surfaced AFTER streaming already began. emittedAny
	// is the one signal callKiroEndpointsOnce threads out that is actually tied to
	// "did any output already reach the client" (see its "Emitted" comment below),
	// so gating on it is what makes "cannot double-emit" true by construction
	// instead of by argument about where 401/403 happen to be checked today.
	// IsSuspensionError is excluded too: a suspended account fails every candidate
	// region, so without this it would cost N extra live probe calls per request
	// for an error the reprobe can never fix.
	if err != nil && !emittedAny && account != nil && account.IsApiKeyCredential() &&
		pool.IsAuthFailure(err) && !pool.IsSuspensionError(err) {
		if _, ok := reprobeApiKeyRegion(account); ok {
			_, err = callKiroEndpointsOnce(ctx, account, payload, callback)
		}
	}
	return err
}

// callKiroEndpointsOnce performs a single pass over the configured Kiro endpoints
// for the account's currently-resolved region, returning on the first success or on
// a terminal (401/403/402) error. CallKiroAPIContext wraps it with a bounded (single
// retry) api_key region-failover: this function itself never retries the region.
//
// The bool return is "emitted" — whether any output callback ran before the
// returned error — so CallKiroAPIContext's region-failover retry can refuse to
// fire once the client has already received bytes, regardless of what the error
// text happens to say.
func callKiroEndpointsOnce(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) (bool, error) {
	// Build endpoint list ordered by configuration / credential type.
	endpoints := resolveKiroEndpoints(account)
	isAPIKey := config.IsAPIKeyAccount(account)

	var lastErr error
endpointLoop:
	for epIndex, ep := range endpoints {
		// Update the origin field for the selected endpoint.
		payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin

		// Target the profile's data-plane region; endpoint URLs are declared for us-east-1.
		// API Key accounts use the CLI runtime host instead of IDE/Q hosts.
		epURL := regionalizeURLForProfile(ep.URL, account, payload.ProfileArn)
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if isAPIKey {
			epURL = cliRuntimeURL(account)
		}

		// Log reasoning fields without exposing the full payload.
		if diagnostics.Reasoning() && len(payload.AdditionalModelRequestFields) > 0 {
			logger.Infof("[KiroOutboundReasoning] endpoint=%s fields=%s", ep.Name, reasoningFieldsJSON(
				payload.AdditionalModelRequestFields),
			)
		}

		reqBody, _ := json.Marshal(payload)
		host := ""
		if parsedURL, parseErr := url.Parse(epURL); parseErr == nil {
			host = parsedURL.Host
		}
		headerValues := buildStreamingHeaderValues(account, host)
		invocationID := uuid.New().String()

		// A stream that dies before any output reached the client is retried against
		// the SAME endpoint first (transport hiccup), then falls through to the next
		// endpoint. The invocation id is held constant across attempts so upstream
		// can correlate them as retries of one logical call.
		for streamAttempt := 1; streamAttempt <= maxStreamAttemptsPerEndpoint; streamAttempt++ {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			// Requests and bodies cannot be reused after an HTTP attempt.
			req, err := http.NewRequestWithContext(ctx, "POST", epURL, bytes.NewReader(reqBody))
			if err != nil {
				lastErr = err
				continue endpointLoop
			}
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
			// CLI captures use optout=false; IDE path keeps true.
			if isAPIKey {
				req.Header.Set("x-amzn-codewhisperer-optout", "false")
			} else {
				req.Header.Set("x-amzn-codewhisperer-optout", "true")
			}
			req.Header.Set("Amz-Sdk-Request", fmt.Sprintf("attempt=%d; max=%d", streamAttempt, maxStreamAttemptsPerEndpoint))
			req.Header.Set("Amz-Sdk-Invocation-Id", invocationID)

			resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
			if err != nil {
				lastErr = err
				logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, err)
				if !isRetryableStreamError(err) {
					return false, err
				}
				continue endpointLoop
			}

			if resp.StatusCode == 429 {
				resp.Body.Close()
				logger.Warnf("[KiroAPI] Endpoint %s quota exhausted (429), trying next...", ep.Name)
				lastErr = fmt.Errorf("quota exhausted on %s", ep.Name)
				continue endpointLoop
			}

			if resp.StatusCode != 200 {
				errBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				// upstreamError tags 402 as "overage" so the failover layer can run
				// the overage path (disableAccountOverage) instead of the generic
				// RecordError path; every other code keeps the "HTTP <code> ..."
				// shape pool.IsAuthFailure matches on.
				lastErr = upstreamError(resp.StatusCode, ep.Name, string(errBody))
				// Auth failures (401/403) and overage (402) are account-level: do not
				// retry across endpoints. Other status codes fall through to the next
				// endpoint.
				if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
					return false, lastErr
				}
				logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, lastErr)
				continue endpointLoop
			}

			// parseAndStreamTracked defers the body close, so a panic in a streaming
			// callback (OnText/OnToolUse/parseEventStream) still returns the upstream
			// TCP connection to the transport pool instead of leaking it.
			emitted, err := parseAndStreamTracked(resp.Body, callback)
			if err == nil {
				return true, nil
			}
			lastErr = err
			if ctxErr := ctx.Err(); ctxErr != nil {
				return emitted, ctxErr
			}
			// "Emitted" deliberately means that an output callback ran. This
			// conservative boundary also protects buffered/non-stream callers:
			// retrying after their callback mutated state would concatenate two
			// attempts even if no network bytes were flushed yet. It is also what
			// CallKiroAPIContext's region-reprobe retry gates on, so a client that
			// already received bytes can never see a second, region-shifted attempt.
			if emitted {
				return true, err
			}
			if !isRetryableStreamError(err) {
				return false, err
			}

			hasSameEndpointRetry := streamAttempt < maxStreamAttemptsPerEndpoint
			hasEndpointFallback := epIndex+1 < len(endpoints)
			if !hasSameEndpointRetry && !hasEndpointFallback {
				break endpointLoop
			}

			logger.Warnf("[KiroAPI] Endpoint %s stream failed before any output (attempt %d/%d): %v",
				ep.Name, streamAttempt, maxStreamAttemptsPerEndpoint, err)
			if err := streamRetryWait(ctx, streamRetryBackoff); err != nil {
				return false, err
			}
			if !hasSameEndpointRetry {
				continue endpointLoop
			}
		}
	}

	if lastErr != nil {
		return false, lastErr
	}
	return false, fmt.Errorf("all endpoints failed")
}

func isRetryableStreamError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	return !errors.As(err, &netErr) || !netErr.Timeout()
}

func accountEmailForLog(account *config.Account) string {
	if account == nil {
		return "<nil>"
	}
	return account.Email
}

func waitForStreamRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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

// parseEventStreamTracked is parseEventStream plus a report of whether an
// output callback was invoked. A failure before the first callback is safe to
// retry because it cannot duplicate or concatenate caller state.
func parseEventStreamTracked(body io.Reader, callback *KiroStreamCallback) (emitted bool, err error) {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}

	// Read directly without bufio to avoid buffering latency in streaming responses.
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

	// Stream diagnostics counters. Populated only when diagnostics.Stream() is
	// enabled (default off); see diagnostics/flags.go.
	var eventCount, assistantEventCount, reasoningEventCount, toolEventCount int
	var assistantChars, reasoningChars int
	var lastEventType string

	for {
		// Prelude: total length, headers length, and prelude CRC.
		prelude := make([]byte, 12)
		_, readErr := io.ReadFull(body, prelude)
		if readErr == io.EOF {
			// Collect stream metrics only when diagnostics are enabled.
			if diagnostics.Stream() {
				logger.Infof(
					"[KiroStream] EOF events=%d last=%q assistantEvents=%d reasoningEvents=%d reasoningChars=%d toolEvents=%d assistantChars=%d inputTokens=%d outputTokens=%d credits=%.3f",
					eventCount,
					lastEventType,
					assistantEventCount,
					reasoningEventCount,
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
		if readErr != nil {
			return emitted, readErr
		}

		totalLength := int(eventStreamUint32(prelude[0:4]))
		headersLength := int(eventStreamUint32(prelude[4:8]))
		if totalLength < 16 {
			return emitted, fmt.Errorf("%w: invalid frame length %d", errInvalidKiroEventStream, totalLength)
		}
		// Reject a corrupt/malicious totalLength BEFORE the make([]byte, remaining)
		// below — and before the CRC checks, which would otherwise mask it behind a
		// generic "prelude CRC mismatch". A single bit-flip in this 32-bit field
		// would drive make([]byte, ~4GB): an alloc panic under net/http's
		// per-request recover (connection dropped with no terminal event → client
		// hang) or a multi-GB buffer held to the 5-minute client timeout
		// (memory-pressure DoS). The distinct sentinel funnels the frame into the
		// upstream-error / response.failed path.
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
		if _, err := io.ReadFull(body, message); err != nil {
			return emitted, err
		}
		if got, want := eventStreamUint32(message[len(message)-4:]), eventStreamChecksum(prelude, message[:len(message)-4]); got != want {
			return emitted, fmt.Errorf("%w: message CRC mismatch", errInvalidKiroEventStream)
		}

		headers, headerErr := parseEventStreamHeaders(message[:headersLength])
		if headerErr != nil {
			return emitted, fmt.Errorf("%w: %v", errInvalidKiroEventStream, headerErr)
		}
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

		// Collect stream metrics only when diagnostics are enabled.
		if diagnostics.Stream() {
			eventCount++
			lastEventType = headers[":event-type"]
		}

		inputTokens, outputTokens = updateTokensFromEvent(event, inputTokens, outputTokens)

		switch headers[":event-type"] {
		case "assistantResponseEvent":
			if content, ok := event["content"].(string); ok && content != "" {
				sawOutput = true

				if diagnostics.Stream() {
					assistantEventCount++
					assistantChars += len([]rune(content))
				}
				if diagnostics.Chunks() {
					logger.Infof(
						"[StreamChunk] type=assistant raw=%q",
						diagnosticPreview(content),
					)
				}

				if callback.OnText != nil {
					emitted = true
					callback.OnText(content, false)
				}
			}

		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				sawOutput = true

				if diagnostics.Stream() {
					reasoningEventCount++
					reasoningChars += len([]rune(text))
				}
				if diagnostics.Chunks() {
					logger.Infof(
						"[StreamChunk] type=reasoning raw=%q",
						diagnosticPreview(text),
					)
				}

				if callback.OnText != nil {
					emitted = true
					callback.OnText(text, true)
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

		case "contextUsageEvent":
			if pct, ok := event["contextUsagePercentage"].(float64); ok {
				contextUsagePercentages = append(contextUsagePercentages, pct)
			}
		case "metadataEvent":
			// stopReason rides inside metadataEvent on the wire; there is no
			// standalone stop reason event type. Its absence after content is
			// how callers detect a truncated stream.
			if reason := firstStringField(event, "stopReason", "stop_reason"); reason != "" && callback.OnStopReason != nil {
				callback.OnStopReason(reason)
			}

		default:
			logger.Debugf(
				"[KiroStream] unhandled event type=%q",
				headers[":event-type"],
			)
		}
	}

	// Flush tools that never received a stop frame, in arrival order.
	if err := pending.flushAll(callback); err != nil {
		return emitted, err
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
	if isLargeContextModel(model) {
		return 1_000_000
	}
	return 200_000
}

// claudeVersionExtractor matches "claude-<family>-<major>[.<minor>]" (dot or
// dash form) and is used to classify 1M-window models by version. The minor
// component is optional so major-only identifiers such as "claude-opus-5"
// classify correctly instead of falling through to the 200K default.
var claudeVersionExtractor = regexp.MustCompile(`claude-(?:opus|sonnet|haiku)-(\d+)(?:[.-](\d+))?`)

func isLargeContextModel(model string) bool {
	m := strings.ToLower(model)
	if match := claudeVersionExtractor.FindStringSubmatch(m); match != nil {
		major, errMaj := strconv.Atoi(match[1])
		if errMaj == nil {
			// 1M window for any major >= 5 (claude-opus-5, claude-opus-5.1, ...).
			if major > 4 {
				return true
			}
			// Within Claude 4.x the window depends on the minor version, so an
			// absent minor (claude-sonnet-4) is treated as 4.0 -> 200K.
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
	ToolUseID   string
	Name        string
	InputBuffer strings.Builder
	GeneratedID bool
}

// pendingToolUses tracks the tool calls in flight for one stream. Entries are
// keyed by toolUseId so interleaved parallel frames accumulate independently,
// and `order` preserves arrival sequence: tool call order is semantic for
// clients, so a bare map range (randomised in Go) must never decide it.
type pendingToolUses struct {
	byID   map[string]*toolUseState
	order  []string
	lastID string
}

func (p *pendingToolUses) get(id string) *toolUseState {
	if p.byID == nil {
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

// rekey moves an entry from a locally generated id to the real id from
// upstream, keeping its position in the arrival order.
func (p *pendingToolUses) rekey(state *toolUseState, newID string) {
	oldID := state.ToolUseID
	delete(p.byID, oldID)
	for i, id := range p.order {
		if id == oldID {
			p.order[i] = newID
			break
		}
	}
	state.ToolUseID = newID
	state.GeneratedID = false
	p.byID[newID] = state
	if p.lastID == oldID {
		p.lastID = newID
	}
}

func (p *pendingToolUses) remove(id string) {
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

// flushAll emits every tool still open when the stream ended, in arrival order.
// It stops at the first incomplete tool so the caller can retry the request
// rather than deliver a call with missing arguments.
func (p *pendingToolUses) flushAll(callback *KiroStreamCallback) error {
	order := p.order
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
			return err
		}
	}
	return nil
}

// handleToolUseEvent accumulates one toolUseEvent frame into pending.
// Frames for different toolUseIds stay open concurrently so interleaved
// parallel tool calls reassemble correctly; a fragment that omits toolUseId
// continues the most recently seen tool.
func handleToolUseEvent(event map[string]interface{}, pending *pendingToolUses, callback *KiroStreamCallback) error {
	toolUseID := firstStringField(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstStringField(event, "name", "toolName", "tool_name")
	isStop := firstBoolField(event, "stop", "isStop", "done")

	var state *toolUseState

	switch {
	case toolUseID != "":
		state = pending.get(toolUseID)
		if state == nil && pending.lastID != "" {
			// Upstream may send the opening fragment without an id and only
			// reveal the real one later. Adopt the synthetic entry we opened
			// for it instead of splitting one logical call across two entries
			// (which also splits its argument JSON, so neither half parses).
			if prev := pending.get(pending.lastID); prev != nil && prev.GeneratedID && (name == "" || prev.Name == name) {
				pending.rekey(prev, toolUseID)
				state = prev
			}
		}
		if state == nil {
			if name == "" {
				// Orphan fragment: an id never seen before and no name to open with.
				return nil
			}
			state = &toolUseState{ToolUseID: toolUseID, Name: name}
			pending.add(state)
		} else {
			if name != "" && state.Name == "" {
				state.Name = name
			}
			pending.lastID = state.ToolUseID
		}
	case pending.lastID != "" && pending.get(pending.lastID) != nil:
		state = pending.get(pending.lastID)
		if name != "" && state.Name != name {
			// Name changed with no id: close the tool in flight rather than
			// appending foreign arguments to it, then open a new one.
			if err := finishToolUse(state, callback); err != nil {
				return err
			}
			pending.remove(state.ToolUseID)
			state = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
			pending.add(state)
		}
	case name != "":
		state = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
		pending.add(state)
	default:
		return nil
	}

	if input, ok := event["input"].(string); ok {
		state.InputBuffer.WriteString(input)
	} else if inputObj, ok := event["input"].(map[string]interface{}); ok {
		data, _ := json.Marshal(inputObj)
		state.InputBuffer.Reset()
		state.InputBuffer.Write(data)
	}

	if isStop {
		if err := finishToolUse(state, callback); err != nil {
			return err
		}
		pending.remove(state.ToolUseID)
	}
	return nil
}

func finishToolUse(state *toolUseState, callback *KiroStreamCallback) error {
	if state == nil || state.Name == "" {
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
	if callback == nil || callback.OnToolUse == nil {
		return nil
	}
	callback.OnToolUse(KiroToolUse{
		ToolUseID: state.ToolUseID,
		Name:      state.Name,
		Input:     input,
	})
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

// extractEventType returns the ":event-type" header of one AWS event-stream
// frame, or "" when the headers are malformed or carry no event type.
//
// It is kept here, next to the Kiro event-stream parser, because the native
// Bedrock provider (bedrock_eventstream.go / bedrock_converse.go) parses the
// SAME AWS framing and deliberately reuses this one implementation rather than
// hand-rolling a second header walk — see CLAUDE.md. It now delegates to
// parseEventStreamHeaders so both paths share the hardened, bounds-checked
// header parser instead of two independent byte walks.
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
