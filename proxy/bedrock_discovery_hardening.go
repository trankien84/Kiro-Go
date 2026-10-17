package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"kiro-go/config"
)

const (
	bedrockDiscoveryOverallTimeout      = 60 * time.Second
	bedrockControlPlaneMaxResponseBytes = int64(1 << 20)
	bedrockInferenceProfilePageSize     = 100
	bedrockInferenceProfileMaxPages     = 100
)

type bedrockDiscoveryEvidence struct {
	models             []string
	foundationCount    int
	profileCount       int
	cacheTTL           time.Duration
	profilesRestricted bool
}

func readBedrockControlPlaneBody(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, errors.New("bedrock control-plane response has no body")
	}
	payload, err := io.ReadAll(io.LimitReader(body, bedrockControlPlaneMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read bedrock control-plane response: %w", err)
	}
	if int64(len(payload)) > bedrockControlPlaneMaxResponseBytes {
		return nil, fmt.Errorf("bedrock control-plane response exceeds %d-byte limit", bedrockControlPlaneMaxResponseBytes)
	}
	return payload, nil
}

func bedrockInferenceProfilePath(nextToken string) string {
	query := url.Values{}
	query.Set("maxResults", fmt.Sprintf("%d", bedrockInferenceProfilePageSize))
	if nextToken != "" {
		query.Set("nextToken", nextToken)
	}
	return "/inference-profiles?" + query.Encode()
}

func validBedrockNextToken(token string) bool {
	return token != "" && len(token) <= 2048 && strings.IndexFunc(token, unicode.IsSpace) < 0
}

// listBedrockInferenceProfilesContext enumerates the complete profile catalog.
// A first-page 403 is a permission-limited result: foundation-model discovery is
// still usable, but it receives the short cache TTL so newly granted permission
// is observed quickly. Any partial-page/transient failure is an error and cannot
// be published as a complete catalog.
func (h *Handler) listBedrockInferenceProfilesContext(ctx context.Context, account *config.Account) ([]string, bool, error) {
	profiles := make([]string, 0)
	seenTokens := make(map[string]struct{})
	nextToken := ""
	for page := 0; page < bedrockInferenceProfileMaxPages; page++ {
		code, body, err := h.signedBedrockControlGetContext(ctx, account, bedrockInferenceProfilePath(nextToken))
		if err != nil {
			return nil, false, err
		}
		if code == http.StatusForbidden && page == 0 {
			return nil, true, nil
		}
		if code != http.StatusOK {
			return nil, false, fmt.Errorf("bedrock discovery: list inference profiles HTTP %d: %s", code, strings.TrimSpace(string(body)))
		}

		var response bedrockInferenceProfilesResponse
		if err := json.Unmarshal(body, &response); err != nil {
			return nil, false, fmt.Errorf("bedrock discovery: parse inference profiles page %d: %w", page+1, err)
		}
		profiles = append(profiles, inferenceProfileIDs(response)...)
		if response.NextToken == "" {
			return profiles, false, nil
		}
		if !validBedrockNextToken(response.NextToken) {
			return nil, false, errors.New("bedrock discovery: invalid inference profiles nextToken")
		}
		if _, repeated := seenTokens[response.NextToken]; repeated {
			return nil, false, errors.New("bedrock discovery: repeated inference profiles nextToken")
		}
		seenTokens[response.NextToken] = struct{}{}
		nextToken = response.NextToken
	}
	return nil, false, fmt.Errorf("bedrock discovery: inference profile pagination exceeded %d pages", bedrockInferenceProfileMaxPages)
}

func (h *Handler) discoverBedrockModelEvidenceContext(ctx context.Context, account *config.Account) (bedrockDiscoveryEvidence, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, bedrockDiscoveryOverallTimeout)
	defer cancel()

	code, body, err := h.signedBedrockControlGetContext(discoveryCtx, account, "/foundation-models")
	if err != nil {
		return bedrockDiscoveryEvidence{}, err
	}
	if code != http.StatusOK {
		return bedrockDiscoveryEvidence{}, fmt.Errorf("bedrock discovery: list models HTTP %d: %s", code, strings.TrimSpace(string(body)))
	}
	var list bedrockListModelsResponse
	if err := json.Unmarshal(body, &list); err != nil {
		return bedrockDiscoveryEvidence{}, fmt.Errorf("bedrock discovery: parse list: %w", err)
	}
	foundation := onDemandTextModelIDs(list)

	profiles, restricted, err := h.listBedrockInferenceProfilesContext(discoveryCtx, account)
	if err != nil {
		return bedrockDiscoveryEvidence{}, err
	}
	// The overall discovery deadline is separate from the caller deadline.
	// Neither an expired internal deadline nor caller cancellation is evidence
	// for a complete catalog.
	if err := discoveryCtx.Err(); err != nil {
		return bedrockDiscoveryEvidence{}, err
	}

	all := dedupeSortedModelIDs(foundation, profiles)
	ttl := bedrockDiscoveryTTL
	if len(all) == 0 || restricted {
		ttl = bedrockDiscoveryNegativeTTL
	}
	return bedrockDiscoveryEvidence{
		models:             all,
		foundationCount:    len(foundation),
		profileCount:       len(profiles),
		cacheTTL:           ttl,
		profilesRestricted: restricted,
	}, nil
}

// bedrockDiscoveryGate is a lifecycle-scoped, context-aware singleflight gate.
// The registry mutex protects only lookup/refcounts; network/cache work runs with
// the gate token held but never with bedrockDiscoveryGateMu held.
type bedrockDiscoveryGate struct {
	token chan struct{}
	refs  int
}

var (
	bedrockDiscoveryGateMu sync.Mutex
	bedrockDiscoveryGates  = map[accountLifecycleKey]*bedrockDiscoveryGate{}
)

func newBedrockDiscoveryGate() *bedrockDiscoveryGate {
	gate := &bedrockDiscoveryGate{token: make(chan struct{}, 1)}
	gate.token <- struct{}{}
	return gate
}

func acquireBedrockDiscoveryGateContext(ctx context.Context, key accountLifecycleKey) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if key.accountID == "" {
		return nil, errors.New("bedrock discovery gate requires account ID")
	}

	bedrockDiscoveryGateMu.Lock()
	gate := bedrockDiscoveryGates[key]
	if gate == nil {
		gate = newBedrockDiscoveryGate()
		bedrockDiscoveryGates[key] = gate
	}
	gate.refs++
	bedrockDiscoveryGateMu.Unlock()

	cancelReference := func() {
		bedrockDiscoveryGateMu.Lock()
		gate.refs--
		if gate.refs == 0 && bedrockDiscoveryGates[key] == gate {
			delete(bedrockDiscoveryGates, key)
		}
		bedrockDiscoveryGateMu.Unlock()
	}

	if err := ctx.Err(); err != nil {
		cancelReference()
		return nil, err
	}
	select {
	case <-gate.token:
	case <-ctx.Done():
		cancelReference()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		gate.token <- struct{}{}
		cancelReference()
		return nil, err
	}

	release := func() {
		bedrockDiscoveryGateMu.Lock()
		gate.refs--
		if gate.refs == 0 && bedrockDiscoveryGates[key] == gate {
			// Return the token while the registry is locked, then remove the entry.
			// A replacement gate cannot overlap the end of this critical section.
			gate.token <- struct{}{}
			delete(bedrockDiscoveryGates, key)
			bedrockDiscoveryGateMu.Unlock()
			return
		}
		bedrockDiscoveryGateMu.Unlock()
		gate.token <- struct{}{}
	}
	return release, nil
}

func clearBedrockModelCacheForLifecycleContext(ctx context.Context, account *config.Account) (bool, error) {
	if account == nil || strings.TrimSpace(account.ID) == "" {
		return false, nil
	}
	key := lifecycleKeyForAccount(account)
	if !key.valid() {
		return false, nil
	}
	release, err := acquireBedrockDiscoveryGateContext(ctx, key)
	if err != nil {
		return false, err
	}
	defer release()

	bedrockDiscoveryMu.Lock()
	delete(bedrockDiscoveryCache, key)
	bedrockDiscoveryMu.Unlock()
	return true, nil
}
