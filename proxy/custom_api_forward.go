package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
)

// customApiCreditsPer1kTokens is the operator-configured credit price for traffic
// served by a Custom API (pool-linking) account. The upstream pool's reply carries
// no credit figure, so the customer's key is billed from token usage at this rate —
// which represents the OPERATOR's own resale pricing, independent of what the
// upstream pool charges them. Without this, credit-limited keys would never exhaust
// on forwarded traffic (free usage). Override with CUSTOM_API_CREDITS_PER_1K_TOKENS;
// default 1.0 credit per 1000 tokens.
func customApiCreditsPer1kTokens() float64 {
	if v := strings.TrimSpace(os.Getenv("CUSTOM_API_CREDITS_PER_1K_TOKENS")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return 1.0
}

// custom_api "pool linking" support. A custom_api account is a transparent proxy
// to ANOTHER Kiro-Go pool: incoming Anthropic/OpenAI requests are forwarded to
// that pool's base URL using a key it issued to us, instead of being translated
// into Amazon CodeWhisperer calls. This file holds the add-time quota probe and
// the runtime forwarding path.

// forwardHeader marks a request that has already been forwarded once, so a
// downstream pool that links back to us refuses to add another custom_api hop.
const forwardHeader = "X-KiroGo-Forwarded"

// customApiQuota is the subset of an upstream Kiro-Go pool's GET /api/me
// response used to gate adding a Custom API account. OK is false when the
// upstream returned a non-2xx status (bad/expired key).
type customApiQuota struct {
	CreditsRemaining float64
	CreditsUsed      float64
	TokensRemaining  int64
	TokensUsed       int64
	CreditLimit      float64
	TokenLimit       int64
	OK               bool
}

// toAccountInfo maps an upstream /api/me quota onto the AccountInfo usage fields the
// admin panel renders (main quota = usageCurrent/usageLimit). Credit-based, matching
// how these accounts are sold. now is the LastRefresh timestamp.
func (q *customApiQuota) toAccountInfo(now int64) config.AccountInfo {
	info := config.AccountInfo{
		SubscriptionType: "Custom API",
		UsageCurrent:     q.CreditsUsed,
		UsageLimit:       q.CreditLimit,
		LastRefresh:      now,
	}
	if q.CreditLimit > 0 {
		info.UsagePercent = q.CreditsUsed / q.CreditLimit
	}
	return info
}

// quotaFields is the credit/token shape both upstream quota endpoints share
// (/api/me on a Kiro-Go pool, /checkkey/info on other pool software). `found` is
// only present on /checkkey/info; a false value means the key does not exist.
type quotaFields struct {
	Found            *bool   `json:"found"`
	CreditsRemaining float64 `json:"creditsRemaining"`
	CreditsUsed      float64 `json:"creditsUsed"`
	TokensRemaining  int64   `json:"tokensRemaining"`
	TokensUsed       int64   `json:"tokensUsed"`
	CreditLimit      float64 `json:"creditLimit"`
	TokenLimit       int64   `json:"tokenLimit"`
}

// toQuota converts the shared field shape into a customApiQuota.
func (f quotaFields) toQuota() *customApiQuota {
	return &customApiQuota{
		CreditsRemaining: f.CreditsRemaining,
		CreditsUsed:      f.CreditsUsed,
		TokensRemaining:  f.TokensRemaining,
		TokensUsed:       f.TokensUsed,
		CreditLimit:      f.CreditLimit,
		TokenLimit:       f.TokenLimit,
		OK:               true,
	}
}

const (
	customAPIProbeTimeout         = 5 * time.Second
	customAPIModelProbeTimeout    = 10 * time.Second
	customAPINonStreamIdleTimeout = 2 * time.Minute
	customAPIStreamIdleTimeout    = 15 * time.Minute
)

var errCustomAPIResponseTooLarge = errors.New(
	"custom API response body exceeds configured limit",
)

func readCustomAPIResponseBody(body io.Reader, maxBytes int64) ([]byte, error) {
	if body == nil {
		return nil, errors.New("custom API response body is nil")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("custom API response body limit must be positive: %d", maxBytes)
	}

	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf(
			"%w: limit=%d bytes",
			errCustomAPIResponseTooLarge,
			maxBytes,
		)
	}
	return data, nil
}

var customAPIDirectTransport = &http.Transport{
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
	ForceAttemptHTTP2:     true,
}

var customAPIDirectClient = &http.Client{
	Transport: customAPIDirectTransport,
}

// customApiHTTPClient is a shared direct-transport client (Proxy nil) so
// pool-to-pool probes and forwarding reuse connections without touching the
// process-global ProxyFromEnvironment transport.
func customApiHTTPClient() *http.Client {
	return customAPIDirectClient
}

// quotaEndpointPref caches which quota endpoint answered for a given baseURL. The
// per-request real-cost probe calls probeCustomApiQuota on the hot path, so without
// this a pool that only serves /checkkey/info would eat a wasted /api/me 404 on
// every single request. baseURL -> "apime" | "checkkey".
var quotaEndpointPref sync.Map

// probeCustomApiQuota fetches the upstream key's quota. The context-aware
// implementation is used by request/admin paths so cancellation propagates all
// the way to the outbound request. The legacy package variable remains for
// focused tests and background callers that intentionally use a bounded context.
var probeCustomApiQuota = func(baseURL, apiKey string) (*customApiQuota, error) {
	return probeCustomApiQuotaWithContext(context.Background(), baseURL, apiKey)
}

func boundedProbeContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, timeout)
}

func probeCustomApiQuotaWithContext(ctx context.Context, baseURL, apiKey string) (*customApiQuota, error) {
	ctx, cancel := boundedProbeContext(ctx, customAPIProbeTimeout)
	defer cancel()

	if pref, ok := quotaEndpointPref.Load(baseURL); ok && pref == "checkkey" {
		if q, err := probeQuotaViaCheckKey(ctx, baseURL, apiKey); err == nil {
			return q, nil
		}
	}
	q, err := probeQuotaViaApiMe(ctx, baseURL, apiKey)
	if err == nil {
		quotaEndpointPref.Store(baseURL, "apime")
		return q, nil
	}
	q2, err2 := probeQuotaViaCheckKey(ctx, baseURL, apiKey)
	if err2 == nil {
		quotaEndpointPref.Store(baseURL, "checkkey")
		return q2, nil
	}
	return &customApiQuota{OK: false}, fmt.Errorf("quota check failed (api/me: %v; checkkey/info: %v)", err, err2)
}

func probeQuotaViaApiMe(ctx context.Context, baseURL, apiKey string) (*customApiQuota, error) {
	u := strings.TrimRight(baseURL, "/") + "/api/me"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-api-key", apiKey)
	resp, err := customApiHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := readCustomAPIResponseBody(resp.Body, 1<<20)
	if resp.StatusCode != http.StatusOK {
		if readErr != nil {
			return nil, fmt.Errorf("/api/me returned %d and response body read failed: %w", resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("/api/me returned %d", resp.StatusCode)
	}
	if readErr != nil {
		return nil, fmt.Errorf("/api/me: read response body: %w", readErr)
	}
	var raw quotaFields
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("/api/me: bad JSON: %w", err)
	}
	return raw.toQuota(), nil
}

func probeQuotaViaCheckKey(ctx context.Context, baseURL, apiKey string) (*customApiQuota, error) {
	u := strings.TrimRight(baseURL, "/") + "/checkkey/info"
	reqBody, _ := json.Marshal(map[string]string{"key": apiKey})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := customApiHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := readCustomAPIResponseBody(resp.Body, 1<<20)
	if resp.StatusCode != http.StatusOK {
		if readErr != nil {
			return nil, fmt.Errorf("/checkkey/info returned %d and response body read failed: %w", resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("/checkkey/info returned %d", resp.StatusCode)
	}
	if readErr != nil {
		return nil, fmt.Errorf("/checkkey/info: read response body: %w", readErr)
	}
	var raw quotaFields
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("/checkkey/info: bad JSON: %w", err)
	}
	if raw.Found != nil && !*raw.Found {
		return nil, fmt.Errorf("/checkkey/info: key not found")
	}
	return raw.toQuota(), nil
}

// probeCustomApiModels is retained for focused tests and callers without an
// existing request context.
var probeCustomApiModels = func(baseURL, apiKey string) ([]ModelInfo, error) {
	return probeCustomApiModelsWithContext(context.Background(), baseURL, apiKey)
}

func probeCustomApiModelsWithContext(ctx context.Context, baseURL, apiKey string) ([]ModelInfo, error) {
	ctx, cancel := boundedProbeContext(ctx, customAPIModelProbeTimeout)
	defer cancel()
	u := strings.TrimRight(baseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-api-key", apiKey)
	resp, err := customApiHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := readCustomAPIResponseBody(resp.Body, 4<<20)
	if resp.StatusCode != http.StatusOK {
		if readErr != nil {
			return nil, fmt.Errorf("upstream /v1/models returned %d and response body read failed: %w", resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("upstream /v1/models returned %d", resp.StatusCode)
	}
	if readErr != nil {
		return nil, fmt.Errorf("upstream /v1/models: read response body: %w", readErr)
	}
	var raw struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("upstream /v1/models: bad JSON: %w", err)
	}
	models := make([]ModelInfo, 0, len(raw.Data))
	for _, m := range raw.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		models = append(models, ModelInfo{ModelId: id, ModelName: id})
	}
	return models, nil
}

const (
	directAnthropicModelsMaxPages = 100
	directAnthropicModelsMaxItems = 10000
)

// probeDirectAnthropicModels follows Anthropic's cursor pagination contract.
// The compatibility wrapper returns ModelInfo while the detailed variant also
// preserves provider capability metadata for the shared capability registry.
var probeDirectAnthropicModels = func(ctx context.Context, baseURL, apiKey string) ([]ModelInfo, error) {
	models, _, err := probeDirectAnthropicModelsDetailed(ctx, baseURL, apiKey)
	return models, err
}

func probeDirectAnthropicModelsDetailed(
	ctx context.Context,
	baseURL,
	apiKey string,
) ([]ModelInfo, map[string]directAnthropicModelMetadata, error) {
	ctx, cancel := boundedProbeContext(ctx, customAPIModelProbeTimeout)
	defer cancel()

	endpoint, err := directAnthropicEndpointURL(baseURL, "/v1/models")
	if err != nil {
		return nil, nil, err
	}

	models := make([]ModelInfo, 0)
	metadata := make(map[string]directAnthropicModelMetadata)
	seenModels := make(map[string]struct{})
	seenCursors := make(map[string]struct{})
	afterID := ""

	for page := 0; page < directAnthropicModelsMaxPages; page++ {
		u, parseErr := url.Parse(endpoint)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("direct Anthropic /v1/models URL: %w", parseErr)
		}
		query := u.Query()
		query.Set("limit", "1000")
		if afterID != "" {
			query.Set("after_id", afterID)
		}
		u.RawQuery = query.Encode()

		req, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if requestErr != nil {
			return nil, nil, requestErr
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Accept", "application/json")

		resp, requestErr := customApiHTTPClient().Do(req)
		if requestErr != nil {
			return nil, nil, requestErr
		}
		maxBodyBytes := int64(4 << 20)
		if resp.StatusCode != http.StatusOK {
			maxBodyBytes = maxDirectAnthropicErrorBodyBytes
		}
		body, readErr := readCustomAPIResponseBody(resp.Body, maxBodyBytes)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, nil, fmt.Errorf("direct Anthropic /v1/models: read response body: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, nil, newDirectAnthropicResponseError(resp, body)
		}

		var raw struct {
			Data []struct {
				ID             string                            `json:"id"`
				DisplayName    string                            `json:"display_name"`
				CreatedAt      string                            `json:"created_at"`
				MaxInputTokens int64                             `json:"max_input_tokens"`
				MaxTokens      int64                             `json:"max_tokens"`
				Capabilities   *directAnthropicModelCapabilities `json:"capabilities"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, nil, fmt.Errorf("direct Anthropic /v1/models: bad JSON: %w", err)
		}

		for _, item := range raw.Data {
			id := strings.TrimSpace(item.ID)
			if id == "" {
				continue
			}
			key := id
			if _, exists := seenModels[key]; exists {
				continue
			}
			seenModels[key] = struct{}{}
			name := strings.TrimSpace(item.DisplayName)
			if name == "" {
				name = id
			}
			inputTypes := []string{"TEXT"}
			if item.Capabilities != nil && item.Capabilities.ImageInput.Supported {
				inputTypes = append(inputTypes, "IMAGE")
			}
			models = append(models, ModelInfo{ModelId: id, ModelName: name, InputTypes: inputTypes})
			modelMetadata := directAnthropicModelMetadata{
				ID:             id,
				DisplayName:    name,
				CreatedAt:      item.CreatedAt,
				MaxInputTokens: item.MaxInputTokens,
				MaxTokens:      item.MaxTokens,
			}
			if item.Capabilities != nil {
				modelMetadata.Capabilities = *item.Capabilities
				modelMetadata.CapabilitiesPresent = true
			}
			metadata[key] = modelMetadata
			if len(models) > directAnthropicModelsMaxItems {
				return nil, nil, fmt.Errorf("direct Anthropic /v1/models exceeded %d models", directAnthropicModelsMaxItems)
			}
		}

		if !raw.HasMore {
			return models, metadata, nil
		}
		nextCursor := strings.TrimSpace(raw.LastID)
		if nextCursor == "" {
			return nil, nil, errors.New("direct Anthropic /v1/models has_more=true without last_id")
		}
		if nextCursor == afterID {
			return nil, nil, fmt.Errorf("direct Anthropic /v1/models cursor did not advance: %q", nextCursor)
		}
		if _, exists := seenCursors[nextCursor]; exists {
			return nil, nil, fmt.Errorf("direct Anthropic /v1/models repeated cursor: %q", nextCursor)
		}
		seenCursors[nextCursor] = struct{}{}
		afterID = nextCursor
	}

	return nil, nil, fmt.Errorf("direct Anthropic /v1/models exceeded %d pages", directAnthropicModelsMaxPages)
}

// customApiTestReply sends a minimal chat request through the linked upstream pool
// and returns the assistant reply text, so the admin "test account" button verifies
// the whole path (auth + routing) end-to-end. model defaults when empty.
func customApiTestReply(
	ctx context.Context,
	baseURL,
	apiKey,
	model string,
) (string, error) {
	if ctx == nil {
		return "", errors.New("custom_api test requires a non-nil context")
	}
	if strings.TrimSpace(model) == "" {
		model = "claude-sonnet-4"
	}
	reqBody := map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "say ok"}},
		"max_tokens": 16,
		"stream":     false,
	}
	body, _ := json.Marshal(reqBody)
	url := strings.TrimRight(baseURL, "/") + "/v1/chat/completions"
	resp, err := forwardUpstreamRequest(
		ctx,
		http.MethodPost,
		url,
		apiKey,
		body,
		false,
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("custom_api test connect: %w", err)
	}
	defer resp.Body.Close()
	raw, readErr := readCustomAPIResponseBody(resp.Body, 1<<20)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if readErr != nil {
			return "", fmt.Errorf(
				"custom_api upstream HTTP %d and response body read failed: %w",
				resp.StatusCode,
				readErr,
			)
		}
		return "", fmt.Errorf("custom_api upstream HTTP %d", resp.StatusCode)
	}
	if readErr != nil {
		return "", fmt.Errorf("custom_api test read response body: %w", readErr)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("custom_api test: bad JSON: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("custom_api test: empty response")
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}

// directAnthropicTestReply verifies an authMethod=anthropic_api account against
// the native Messages endpoint. It deliberately does not add the Kiro-Go relay
// marker and preserves the required Anthropic version header.
func directAnthropicTestReply(
	ctx context.Context,
	baseURL,
	apiKey,
	model string,
) (string, error) {
	if ctx == nil {
		return "", errors.New("direct Anthropic test requires a non-nil context")
	}
	if strings.TrimSpace(model) == "" {
		model = directAnthropicDefaultTestModel
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model": model,
		"messages": []map[string]interface{}{{
			"role": "user", "content": "say ok",
		}},
		"max_tokens": 16,
		"stream":     false,
	})
	semanticHeaders := make(http.Header)
	semanticHeaders.Set("Anthropic-Version", "2023-06-01")
	endpoint, err := directAnthropicEndpointURL(baseURL, "/v1/messages")
	if err != nil {
		return "", err
	}
	resp, err := forwardDirectAnthropicRequest(
		ctx,
		http.MethodPost,
		endpoint,
		apiKey,
		body,
		false,
		semanticHeaders,
	)
	if err != nil {
		return "", fmt.Errorf("direct Anthropic test connect: %w", err)
	}
	defer resp.Body.Close()
	raw, readErr := readCustomAPIResponseBody(resp.Body, 1<<20)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if readErr != nil {
			return "", &directAnthropicTransportError{Err: fmt.Errorf(
				"HTTP %d error body: %w",
				resp.StatusCode,
				readErr,
			)}
		}
		return "", newDirectAnthropicResponseError(resp, raw)
	}
	if readErr != nil {
		return "", fmt.Errorf("direct Anthropic test read response body: %w", readErr)
	}
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("direct Anthropic test: bad JSON: %w", err)
	}
	for _, block := range parsed.Content {
		if strings.EqualFold(strings.TrimSpace(block.Type), "text") && strings.TrimSpace(block.Text) != "" {
			return strings.TrimSpace(block.Text), nil
		}
	}
	return "", errors.New("direct Anthropic test: empty text response")
}

// customApiQuotaAcceptable is the add-time gate: accept when the upstream key is
// valid and either has remaining quota, or is unlimited (both limits zero).
func customApiQuotaAcceptable(q *customApiQuota) bool {
	if q == nil || !q.OK {
		return false
	}
	if q.CreditLimit == 0 && q.TokenLimit == 0 {
		return true // unlimited key
	}
	return q.CreditsRemaining > 0 || q.TokensRemaining > 0
}

// normalizeBaseURL validates and canonicalizes an upstream base URL so it is always
// safe to concatenate with /v1/... paths. Requires an http(s) scheme and a non-empty
// host, and rejects query/fragment (a base URL must not carry them). Returns
// scheme://host + trimmed path.
func normalizeBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("baseUrl is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("baseUrl must start with http:// or https://")
	}
	if u.Host == "" {
		return "", fmt.Errorf("baseUrl must include a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("baseUrl must not contain a query or fragment")
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/"), nil
}

// normalizeDirectAnthropicBaseURL accepts either an Anthropic-compatible API
// origin or the same URL ending in /v1. It stores a canonical API root so callers
// can append /v1/messages and /v1/models exactly once. Concrete endpoint URLs are
// rejected because silently appending another route would produce ambiguous paths.
func normalizeDirectAnthropicBaseURL(raw string) (string, error) {
	normalized, err := normalizeBaseURL(raw)
	if err != nil {
		return "", err
	}

	u, err := url.Parse(normalized)
	if err != nil {
		return "", fmt.Errorf("baseUrl is not a valid URL: %w", err)
	}

	path := strings.TrimRight(u.Path, "/")
	lowerPath := strings.ToLower(path)
	if strings.HasSuffix(lowerPath, "/v1/messages") ||
		strings.HasSuffix(lowerPath, "/v1/models") {
		return "", fmt.Errorf("baseUrl must be an API root or end in /v1, not a concrete Anthropic endpoint")
	}
	if strings.HasSuffix(lowerPath, "/v1") {
		path = path[:len(path)-len("/v1")]
	}

	return u.Scheme + "://" + u.Host + strings.TrimRight(path, "/"), nil
}

func directAnthropicEndpointURL(baseURL, endpointPath string) (string, error) {
	base, err := normalizeDirectAnthropicBaseURL(baseURL)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(endpointPath, "/") {
		return "", fmt.Errorf("Anthropic endpoint path must start with /: %q", endpointPath)
	}
	return strings.TrimRight(base, "/") + endpointPath, nil
}

// forwardParams bundles everything forwardToUpstream needs from a dispatch handler.
type forwardParams struct {
	selection       *pool.AccountSelection
	account         *config.Account
	body            []byte
	endpoint        string // "anthropic" | "openai"
	streaming       bool
	model           string // client-visible/requested model
	providerModelID string // exact provider target captured by the candidate plan
	apiKeyID        string
	forwarded       bool // incoming request already carried forwardHeader
	semanticHeaders http.Header
	requestContext  context.Context
}

// upstreamPath maps the incoming API surface to the upstream pool's path.
func upstreamPath(endpoint string) string {
	switch endpoint {
	case "openai":
		return "/v1/chat/completions"
	case "responses":
		return "/v1/responses"
	default:
		return "/v1/messages"
	}
}

func requestedModelFromJSON(body []byte, fallback string) string {
	var envelope struct {
		Model string `json:"model"`
	}
	if len(body) > 0 && json.Unmarshal(body, &envelope) == nil {
		if model := strings.TrimSpace(envelope.Model); model != "" {
			return model
		}
	}
	return strings.TrimSpace(fallback)
}

func rewriteForwardBodyModel(body []byte, providerModelID string) ([]byte, error) {
	providerModelID = strings.TrimSpace(providerModelID)
	if providerModelID == "" || len(body) == 0 {
		return body, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("rewrite forwarded request model: %w", err)
	}
	encodedModel, err := json.Marshal(providerModelID)
	if err != nil {
		return nil, fmt.Errorf("encode forwarded provider model: %w", err)
	}
	envelope["model"] = encodedModel
	rewritten, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("rewrite forwarded request: %w", err)
	}
	return rewritten, nil
}

// forwardUpstreamRequest performs the actual round-trip to the upstream pool. It
// is a package var so tests can stub it; the default does a real HTTP call.
func cloneAnthropicSemanticHeaders(source http.Header) http.Header {
	if len(source) == 0 {
		return nil
	}
	// Keep this as an explicit request-semantic allowlist. A prefix match would
	// also forward future credential, routing, or internal headers supplied by
	// an untrusted client.
	result := make(http.Header)
	for key, values := range source {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "anthropic-beta", "anthropic-version",
			"anthropic-user-profile-id":
		default:
			continue
		}
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" {
				result.Add(key, value)
			}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

var forwardUpstreamRequest = func(
	ctx context.Context,
	method,
	url,
	apiKey string,
	body []byte,
	stream bool,
	semanticHeaders http.Header,
) (*http.Response, error) {
	return doForwardUpstreamRequest(
		ctx, method, url, apiKey, body, stream, semanticHeaders, true, false,
	)
}

// forwardDirectAnthropicRequest uses the same bounded transport as a relay but
// does not add the Kiro-Go loop marker. It is used only for accounts explicitly
// configured with authMethod=anthropic_api.
var forwardDirectAnthropicRequest = func(
	ctx context.Context,
	method,
	url,
	apiKey string,
	body []byte,
	stream bool,
	semanticHeaders http.Header,
) (*http.Response, error) {
	headers := cloneAnthropicSemanticHeaders(semanticHeaders)
	if headers == nil {
		headers = make(http.Header)
	}
	if strings.TrimSpace(headers.Get("Anthropic-Version")) == "" {
		headers.Set("Anthropic-Version", "2023-06-01")
	}
	return doForwardUpstreamRequest(
		ctx, method, url, apiKey, body, stream, headers, false, true,
	)
}

func doForwardUpstreamRequest(
	ctx context.Context,
	method,
	url,
	apiKey string,
	body []byte,
	stream bool,
	semanticHeaders http.Header,
	markForwarded bool,
	directAnthropic bool,
) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		url,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	for key, values := range cloneAnthropicSemanticHeaders(semanticHeaders) {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	if !directAnthropic {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("x-api-key", apiKey)
	if markForwarded {
		req.Header.Set(forwardHeader, "1")
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	requestCtx, cancelRequest := context.WithCancel(ctx)
	req = req.WithContext(requestCtx)
	resp, err := customApiHTTPClient().Do(req)
	if err != nil {
		cancelRequest()
		return nil, err
	}
	idleTimeout := customAPINonStreamIdleTimeout
	if stream {
		idleTimeout = customAPIStreamIdleTimeout
	}
	resp.Body = newIdleWatchdogReadCloser(resp.Body, idleTimeout, cancelRequest)
	return resp, nil
}

// parseUpstreamUsage best-effort extracts token counts from a JSON reply (or a
// single SSE data payload). Anthropic uses usage.input_tokens/output_tokens;
// OpenAI uses usage.prompt_tokens/completion_tokens.
func parseUpstreamUsage(endpoint string, body []byte) (int, int) {
	var raw struct {
		Usage struct {
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, 0
	}
	if endpoint == "openai" {
		return raw.Usage.PromptTokens, raw.Usage.CompletionTokens
	}
	return raw.Usage.InputTokens, raw.Usage.OutputTokens
}

const (
	maxDirectAnthropicErrorBodyBytes = 1 << 20
	directAnthropicDefaultTestModel  = "claude-sonnet-4-6"
)

// directAnthropicResponseError preserves a terminal provider response across
// account failover. Client errors remain terminal; credential and transient
// failures may try another account without collapsing the final response to 500.
type directAnthropicTransportError struct {
	Err error
}

func (e *directAnthropicTransportError) Error() string {
	if e == nil || e.Err == nil {
		return "direct Anthropic transport failed"
	}
	return "direct Anthropic transport failed: " + e.Err.Error()
}

func (e *directAnthropicTransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type directAnthropicErrorScope uint8

const (
	directAnthropicErrorRequest directAnthropicErrorScope = iota
	directAnthropicErrorCredential
	directAnthropicErrorBilling
	directAnthropicErrorPermission
	directAnthropicErrorNotFound
	directAnthropicErrorRateLimit
	directAnthropicErrorTransient
)

type directAnthropicResponseError struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	ErrorType  string
	Message    string
	RequestID  string
	Scope      directAnthropicErrorScope
	RetryAfter time.Duration
	Retryable  bool
}

func (e *directAnthropicResponseError) Error() string {
	if e == nil {
		return "direct Anthropic response error"
	}
	if strings.TrimSpace(e.ErrorType) != "" {
		return fmt.Sprintf("direct Anthropic upstream HTTP %d (%s)", e.StatusCode, e.ErrorType)
	}
	return fmt.Sprintf("direct Anthropic upstream HTTP %d", e.StatusCode)
}

type directAnthropicErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

func parseRetryAfter(header http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		const maxRetryAfterSeconds = int64((24 * time.Hour) / time.Second)
		if seconds > maxRetryAfterSeconds {
			seconds = maxRetryAfterSeconds
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if delay := at.Sub(now); delay > 0 {
			return delay
		}
	}
	return 0
}

func classifyDirectAnthropicError(status int, errorType string) directAnthropicErrorScope {
	switch strings.ToLower(strings.TrimSpace(errorType)) {
	case "authentication_error":
		return directAnthropicErrorCredential
	case "billing_error":
		return directAnthropicErrorBilling
	case "permission_error":
		return directAnthropicErrorPermission
	case "not_found_error":
		return directAnthropicErrorNotFound
	case "rate_limit_error":
		return directAnthropicErrorRateLimit
	case "api_error", "timeout_error", "overloaded_error":
		return directAnthropicErrorTransient
	}

	switch {
	case status == http.StatusUnauthorized:
		return directAnthropicErrorCredential
	case status == http.StatusPaymentRequired:
		return directAnthropicErrorBilling
	case status == http.StatusForbidden:
		return directAnthropicErrorPermission
	case status == http.StatusNotFound:
		return directAnthropicErrorNotFound
	case status == http.StatusTooManyRequests:
		return directAnthropicErrorRateLimit
	case status >= 500:
		return directAnthropicErrorTransient
	default:
		return directAnthropicErrorRequest
	}
}

func directAnthropicScopeRetryable(scope directAnthropicErrorScope) bool {
	switch scope {
	case directAnthropicErrorCredential,
		directAnthropicErrorBilling,
		directAnthropicErrorPermission,
		directAnthropicErrorNotFound,
		directAnthropicErrorRateLimit,
		directAnthropicErrorTransient:
		return true
	default:
		return false
	}
}

func newDirectAnthropicResponseError(resp *http.Response, body []byte) *directAnthropicResponseError {
	status := 0
	header := make(http.Header)
	if resp != nil {
		status = resp.StatusCode
		header = cloneDirectAnthropicResponseHeaders(resp.Header)
	}
	var payload directAnthropicErrorBody
	_ = json.Unmarshal(body, &payload)
	errorType := strings.TrimSpace(payload.Error.Type)
	scope := classifyDirectAnthropicError(status, errorType)
	requestID := strings.TrimSpace(payload.RequestID)
	if requestID == "" {
		requestID = strings.TrimSpace(header.Get("Request-Id"))
	}
	return &directAnthropicResponseError{
		StatusCode: status,
		Header:     header,
		Body:       append([]byte(nil), body...),
		ErrorType:  errorType,
		Message:    strings.TrimSpace(payload.Error.Message),
		RequestID:  requestID,
		Scope:      scope,
		RetryAfter: parseRetryAfter(header, time.Now()),
		Retryable:  directAnthropicScopeRetryable(scope),
	}
}

func cloneDirectAnthropicResponseHeaders(source http.Header) http.Header {
	result := make(http.Header)
	for key, values := range source {
		lower := strings.ToLower(strings.TrimSpace(key))
		if lower != "content-type" && lower != "request-id" && lower != "retry-after" &&
			!strings.HasPrefix(lower, "anthropic-ratelimit-") {
			continue
		}
		for _, value := range values {
			result.Add(key, value)
		}
	}
	return result
}

func appendAccessControlExposeHeader(dst http.Header, headerName string) {
	name := strings.TrimSpace(headerName)
	if dst == nil || name == "" {
		return
	}
	existing := dst.Values("Access-Control-Expose-Headers")
	seen := make(map[string]struct{})
	ordered := make([]string, 0)
	for _, value := range existing {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			key := strings.ToLower(item)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			ordered = append(ordered, item)
		}
	}
	if _, ok := seen[strings.ToLower(name)]; !ok {
		ordered = append(ordered, name)
	}
	dst.Set("Access-Control-Expose-Headers", strings.Join(ordered, ", "))
}

func copyDirectAnthropicResponseHeaders(dst, source http.Header) {
	for key, values := range cloneDirectAnthropicResponseHeaders(source) {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
		lower := strings.ToLower(strings.TrimSpace(key))
		if lower == "request-id" || lower == "retry-after" ||
			strings.HasPrefix(lower, "anthropic-ratelimit-") {
			appendAccessControlExposeHeader(dst, key)
		}
	}
}

func writeDirectAnthropicResponseError(w http.ResponseWriter, responseErr *directAnthropicResponseError) {
	if responseErr == nil {
		return
	}
	copyDirectAnthropicResponseHeaders(w.Header(), responseErr.Header)
	if strings.TrimSpace(w.Header().Get("Content-Type")) == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(responseErr.StatusCode)
	_, _ = w.Write(responseErr.Body)
}

// forwardToUpstream proxies the raw request to the account's upstream pool. On any
// failure BEFORE a 2xx reply it returns an error and writes nothing, so the caller
// can fail over to another account. After a 2xx it writes the reply and returns nil.
func (h *Handler) forwardToUpstream(w http.ResponseWriter, flusher http.Flusher, p forwardParams) error {
	// Loop guard applies only to Kiro-Go pool relays. A direct Anthropic
	// endpoint is terminal and must not inherit relay hop semantics.
	if p.forwarded && p.account != nil && p.account.IsCustomApi() {
		return fmt.Errorf("custom_api: refusing to forward an already-forwarded request (loop guard)")
	}

	reqStart := time.Now()
	requestURL := strings.TrimRight(p.account.BaseURL, "/") + upstreamPath(p.endpoint)
	requestFn := forwardUpstreamRequest
	if p.account.IsDirectAnthropic() {
		var endpointErr error
		requestURL, endpointErr = directAnthropicEndpointURL(p.account.BaseURL, upstreamPath(p.endpoint))
		if endpointErr != nil {
			return &directAnthropicTransportError{Err: endpointErr}
		}
		requestFn = forwardDirectAnthropicRequest
	}
	forwardBody, rewriteErr := rewriteForwardBodyModel(p.body, p.providerModelID)
	if rewriteErr != nil {
		if p.account.IsDirectAnthropic() {
			return &directAnthropicTransportError{Err: rewriteErr}
		}
		return rewriteErr
	}
	resp, err := requestFn(
		p.requestContext,
		http.MethodPost,
		requestURL,
		p.account.KiroApiKey,
		forwardBody,
		p.streaming,
		p.semanticHeaders,
	)
	if err != nil {
		if p.account.IsDirectAnthropic() {
			return &directAnthropicTransportError{Err: err}
		}
		return fmt.Errorf("custom_api upstream connect: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if p.account.IsDirectAnthropic() {
			body, readErr := readCustomAPIResponseBody(resp.Body, maxDirectAnthropicErrorBodyBytes)
			if readErr != nil {
				return &directAnthropicTransportError{Err: fmt.Errorf("HTTP %d error body: %w", resp.StatusCode, readErr)}
			}
			return newDirectAnthropicResponseError(resp, body)
		}

		// Linked pools are not terminal providers. Keep their raw error body private
		// while preserving the status code for existing auth/failover classification.
		msg, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		logger.Warnf("[CustomApi] upstream %s HTTP %d: %s", p.account.ID, resp.StatusCode, strings.TrimSpace(string(msg)))
		if readErr != nil {
			return fmt.Errorf(
				"custom_api upstream HTTP %d and error body read failed: %w",
				resp.StatusCode,
				readErr,
			)
		}
		return fmt.Errorf("custom_api upstream HTTP %d", resp.StatusCode)
	}

	if p.streaming {
		return h.streamUpstream(w, flusher, resp, p, reqStart)
	}

	// Non-streaming: copy the JSON reply verbatim, then meter. Cap the read so a
	// misbehaving upstream cannot balloon memory (32 MiB is far above any real reply).
	const maxNonStreamResponseBytes = 32 << 20
	body, err := io.ReadAll(io.LimitReader(
		resp.Body,
		maxNonStreamResponseBytes+1,
	))
	if err != nil {
		if p.account.IsDirectAnthropic() {
			return &directAnthropicTransportError{Err: err}
		}
		return fmt.Errorf("custom_api upstream read: %w", err)
	}
	if len(body) > maxNonStreamResponseBytes {
		err := fmt.Errorf("upstream response exceeds %d bytes", maxNonStreamResponseBytes)
		if p.account.IsDirectAnthropic() {
			return &directAnthropicTransportError{Err: err}
		}
		return fmt.Errorf("custom_api %w", err)
	}
	if p.account.IsDirectAnthropic() {
		copyDirectAnthropicResponseHeaders(w.Header(), resp.Header)
	}
	if strings.TrimSpace(w.Header().Get("Content-Type")) == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(resp.StatusCode)
	n, writeErr := w.Write(body)
	if writeErr != nil || n != len(body) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		// The response is already committed, so failover would corrupt the
		// downstream response. Treat this as terminal client I/O, but do not
		// record a successful upstream request or bill it as delivered.
		logger.Warnf(
			"[CustomApi] downstream write failed after commit account=%s: %v",
			p.account.ID,
			writeErr,
		)
		return nil
	}

	in, out := parseUpstreamUsage(p.endpoint, body)
	h.recordForwardedUpstreamSuccess(p, in, out, reqStart)
	return nil
}

// streamUpstream copies the upstream SSE stream to the client byte-for-byte,
// flushing as data arrives, and taps token usage from the stream on the way past.
// Called only after a 2xx upstream status, so headers are safe to commit here.
func (h *Handler) streamUpstream(w http.ResponseWriter, flusher http.Flusher, resp *http.Response, p forwardParams, reqStart time.Time) error {
	if p.account.IsDirectAnthropic() {
		copyDirectAnthropicResponseHeaders(w.Header(), resp.Header)
	}
	if strings.TrimSpace(w.Header().Get("Content-Type")) == "" {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	// tail keeps a bounded window of the most recent stream bytes so we can parse
	// the terminal usage event without holding the entire stream in memory.
	var tail bytes.Buffer
	const tailCap = 64 * 1024
	buf := make([]byte, 16*1024)
	var streamErr error
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := w.Write(chunk); werr != nil {
				return nil // client gone; nothing to fail over to
			}
			if flusher != nil {
				flusher.Flush()
			}
			tail.Write(chunk)
			if tail.Len() > tailCap {
				trimmed := append([]byte(nil), tail.Bytes()[tail.Len()-tailCap:]...)
				tail.Reset()
				tail.Write(trimmed)
			}
		}
		if readErr != nil {
			// io.EOF is a clean end; any other error is a mid-stream upstream drop.
			if readErr != io.EOF {
				streamErr = readErr
			}
			break
		}
	}

	// Mid-stream failure: the client already has committed headers and a partial
	// stream, so we cannot fail over. Emit a best-effort terminal error event (as the
	// Kiro path does) and record a FAILURE — never a success — so pool health reflects
	// reality. Tokens from a truncated stream are not metered.
	if streamErr != nil {
		if p.requestContext != nil &&
			p.requestContext.Err() != nil {
			// The downstream request was canceled. Its context has already
			// closed the upstream socket; this is neither an upstream health
			// failure nor a response that should be metered as delivered.
			return nil
		}
		msg := "custom_api upstream stream interrupted: " + streamErr.Error()
		if p.endpoint == "anthropic" {
			// Anthropic SSE error event shape.
			fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":%q}}\n\n", msg)
		} else {
			// OpenAI (chat + responses) SSE error chunk shape.
			fmt.Fprintf(w, "data: {\"error\":{\"type\":\"api_error\",\"message\":%q}}\n\n", msg)
		}
		if flusher != nil {
			flusher.Flush()
		}
		endpoint := "claude"
		if p.endpoint == "openai" || p.endpoint == "responses" {
			endpoint = "openai"
		}
		if p.selection != nil {
			h.pool.RecordErrorForSelection(*p.selection, false)
		} else {
			h.pool.RecordError(p.account.ID, false)
		}
		h.recordFailureWithDetails(endpoint, p.model, p.account.ID, p.apiKeyID, streamErr)
		return nil
	}

	in, out := parseStreamUsage(p.endpoint, tail.Bytes())
	h.recordForwardedUpstreamSuccess(p, in, out, reqStart)
	return nil
}

// parseStreamUsage scans SSE tail bytes for the last usage object. Anthropic emits
// usage in message_delta; OpenAI emits it in the final chunk when stream_options
// include_usage is set. Best-effort: returns 0,0 if none found.
func parseStreamUsage(endpoint string, tail []byte) (int, int) {
	lines := bytes.Split(tail, []byte("\n"))
	var lastIn, lastOut int
	for _, ln := range lines {
		ln = bytes.TrimSpace(ln)
		if !bytes.HasPrefix(ln, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(ln, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if in, out := parseUpstreamUsage(endpoint, payload); in != 0 || out != 0 {
			if in != 0 {
				lastIn = in
			}
			if out != 0 {
				lastOut = out
			}
		}
	}
	return lastIn, lastOut
}

// recordForwardedUpstreamSuccess keeps relay billing and terminal-provider
// billing separate. A direct Anthropic endpoint has no Kiro-Go quota API, so it
// must never enter the custom_api real-cost probe path.
func (h *Handler) recordForwardedUpstreamSuccess(
	p forwardParams,
	inputTokens,
	outputTokens int,
	reqStart time.Time,
) {
	if p.account.IsDirectAnthropic() {
		h.recordDirectAnthropicSuccess(p, inputTokens, outputTokens, reqStart)
		return
	}
	h.recordCustomApiSuccess(p, inputTokens, outputTokens, reqStart)
}

// recordDirectAnthropicSuccess meters a terminal Anthropic-compatible account
// without probing Kiro-Go-specific quota endpoints. The optional operator
// credit rate is deliberately separate from CUSTOM_API_CREDITS_PER_1K_TOKENS.
func (h *Handler) recordDirectAnthropicSuccess(
	p forwardParams,
	inputTokens,
	outputTokens int,
	reqStart time.Time,
) {
	tokens := inputTokens + outputTokens
	credits := directAnthropicCreditsForTokens(tokens)
	h.recordSuccessForApiKey(p.apiKeyID, inputTokens, outputTokens, credits)
	if p.selection != nil {
		h.pool.RecordSuccessForSelection(*p.selection)
		h.pool.RecordLatencyForSelection(*p.selection, float64(time.Since(reqStart).Milliseconds()))
		h.pool.UpdateStatsForSelection(*p.selection, tokens, credits)
	} else {
		h.pool.RecordSuccess(p.account.ID)
		h.pool.RecordLatency(p.account.ID, float64(time.Since(reqStart).Milliseconds()))
		h.pool.UpdateStats(p.account.ID, tokens, credits)
	}
	h.recordSuccessLog(
		"claude",
		p.model,
		p.account.ID,
		p.apiKeyID,
		tokens,
		credits,
		time.Since(reqStart).Milliseconds(),
	)
}

func directAnthropicCreditsForTokens(tokens int) float64 {
	rate := envFloatDefault("ANTHROPIC_CREDITS_PER_1K_TOKENS", 0)
	if rate <= 0 || tokens <= 0 {
		return 0
	}
	return float64(tokens) / 1000.0 * rate
}

// recordCustomApiSuccess mirrors the Kiro success path's metering so custom_api
// traffic shows up in pool stats, per-key usage, and request logs. Runs AFTER the
// client already has the full response, so the real-cost probe it does adds no
// client-visible latency.
func (h *Handler) recordCustomApiSuccess(p forwardParams, inputTokens, outputTokens int, reqStart time.Time) {
	endpoint := "claude"
	if p.endpoint == "openai" || p.endpoint == "responses" {
		endpoint = "openai"
	}
	// Bill the REAL cost the upstream pool deducted for this request, read as the
	// delta of its reported creditsUsed, instead of a flat token-derived price.
	credits := h.billCustomApiRealCost(p, inputTokens, outputTokens)
	h.recordSuccessForApiKey(p.apiKeyID, inputTokens, outputTokens, credits)
	if p.selection != nil {
		h.pool.RecordSuccessForSelection(*p.selection)
		h.pool.RecordLatencyForSelection(*p.selection, float64(time.Since(reqStart).Milliseconds()))
		h.pool.UpdateStatsForSelection(*p.selection, inputTokens+outputTokens, credits)
	} else {
		h.pool.RecordSuccess(p.account.ID)
		h.pool.RecordLatency(p.account.ID, float64(time.Since(reqStart).Milliseconds()))
		h.pool.UpdateStats(p.account.ID, inputTokens+outputTokens, credits)
	}
	h.recordSuccessLog(endpoint, p.model, p.account.ID, p.apiKeyID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())
}

// customApiRateTTL bounds how long a cached upstream price is reused before the next
// request re-probes. The pool's blended per-token rate moves slowly (it is a
// cumulative average), so a couple of minutes keeps billing current without a probe
// per request.
const customApiRateTTL = 2 * time.Minute

// customApiCreditLedger caches, per custom_api account, the upstream pool's blended
// per-token price (its cumulative creditsUsed / tokensUsed from /checkkey/info). Each
// request is billed reqTokens * that rate.
//
// Why a cached pool rate rather than per-request deltas or a running token-share: many
// customer keys share ONE upstream pool key and the pool reports only the key-wide
// cumulative creditsUsed. Charging the delta-since-last-probe hands one request the
// concurrent spend of every other customer (random huge/zero charges); a running
// realTotal/sumTokens average does not telescope to real spend when per-request rates
// vary (mixed models), so it drifts. The pool's own creditsUsed/tokensUsed ratio is a
// stable, authoritative price: billing each request its own tokens at that rate is
// smooth, fair across customers, needs no per-account accumulation, and has no
// seed/reset/ordering races. Guarded by a mutex covering only the cache map.
type customApiLifecycleKey struct {
	accountID  string
	generation uint64
}

type customApiCreditLedger struct {
	mu    sync.Mutex
	rates map[customApiLifecycleKey]customApiRate
}

type customApiRate struct {
	perToken float64   // upstream credits per token
	at       time.Time // when this rate was probed
}

func newCustomApiCreditLedger() *customApiCreditLedger {
	return &customApiCreditLedger{rates: make(map[customApiLifecycleKey]customApiRate)}
}

// billCustomApiRealCost returns the credits to charge this request: its token count
// times the upstream pool's blended per-token price. The price is cached per account
// (customApiRateTTL); on a miss it probes the pool's quota API — after the client
// already has the response, so no client latency.
//
// Fallbacks: a stale cached rate if the refresh probe fails; otherwise the flat token
// estimate (CUSTOM_API_CREDITS_PER_1K_TOKENS) when no rate is known at all. Set that
// env near the pool's real per-1k rate so a cold cache does not spike.
//
// Accuracy note: aggregate billed = ourTokens * poolRate, where poolRate is computed
// from the pool's OWN token count. If our token counting under-reports relative to the
// pool's, this under-bills proportionally (safe direction). Fixing that requires
// aligning our token counts with the pool's, tracked separately.
func customApiLifecycleKeyForForward(p forwardParams) customApiLifecycleKey {
	generation := uint64(0)
	if p.selection != nil {
		generation = p.selection.LifecycleGeneration
	} else if p.account != nil {
		generation = p.account.DispatchGeneration
	}
	id := ""
	if p.account != nil {
		id = p.account.ID
	}
	return customApiLifecycleKey{accountID: id, generation: generation}
}

func (h *Handler) billCustomApiRealCost(p forwardParams, inputTokens, outputTokens int) float64 {
	tokens := float64(inputTokens + outputTokens)
	estimate := tokens / 1000.0 * customApiCreditsPer1kTokens()

	l := h.customApiLedger
	if l == nil { // defensive: handlers built outside NewHandler (older tests)
		return estimate
	}

	now := time.Now()
	ledgerKey := customApiLifecycleKeyForForward(p)
	l.mu.Lock()
	cached, ok := l.rates[ledgerKey]
	fresh := ok && now.Sub(cached.at) < customApiRateTTL
	if !fresh && ok {
		// Claim this refresh window: bump the timestamp (keeping the old rate) before
		// probing so concurrent requests reuse the cached rate instead of all probing
		// at once, and so a FAILING probe backs off for a full TTL instead of every
		// request re-probing throughout an outage. A successful probe below overwrites
		// the rate. (Cold start with no cached entry still lets a one-time burst probe.)
		l.rates[ledgerKey] = customApiRate{perToken: cached.perToken, at: now}
	}
	l.mu.Unlock()
	if fresh {
		return tokens * cached.perToken
	}

	// Refresh the rate. Probe runs outside the lock — never hold the mutex across
	// network I/O.
	q, err := probeCustomApiQuotaWithContext(p.requestContext, p.account.BaseURL, p.account.KiroApiKey)
	if err == nil && q != nil && q.OK && q.TokensUsed > 0 && q.CreditsUsed > 0 {
		rate := q.CreditsUsed / float64(q.TokensUsed)
		l.mu.Lock()
		l.rates[ledgerKey] = customApiRate{perToken: rate, at: now}
		l.mu.Unlock()
		return tokens * rate
	}

	// Probe failed or reported no usable rate. Prefer a stale cached rate over the flat
	// estimate — it is still the pool's real price, just not freshly confirmed.
	if ok {
		logger.Warnf("[CustomApi] rate refresh failed for %s, reusing cached rate %.6g: %v", p.account.ID, cached.perToken, err)
		return tokens * cached.perToken
	}
	logger.Warnf("[CustomApi] no upstream rate for %s, billing estimate %.4f: %v", p.account.ID, estimate, err)
	return estimate
}
