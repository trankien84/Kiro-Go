package proxy

import (
	"sort"
	"strings"
	"time"

	"kiro-go/config"
)

// Model IDs live in three different trust domains. RequestedModelID is supplied
// by the client and may be alias-normalized. ProviderModelID is opaque provider
// data and must be preserved byte-for-byte apart from surrounding whitespace.
// PublicModelID is presentation data advertised by /v1/models.
type RequestedModelID string
type ProviderModelID string
type PublicModelID string

type providerModelEvidence uint8

const (
	providerModelEvidenceUnknown providerModelEvidence = iota
	providerModelEvidenceDiscovered
	providerModelEvidenceOperatorConfigured
	providerModelEvidenceUniqueAlias
)

const (
	providerExactDenialSentinel = "\x00exact:"
	providerAliasDenialSentinel = "\x00alias:"
)

type resolvedProviderModel struct {
	RequestedID      RequestedModelID
	ProviderID       ProviderModelID
	Evidence         providerModelEvidence
	SupportsThinking bool
	Reasoning        ModelReasoningCapability
	ReasoningKnown   bool

	// KiroStructuredOutputFormat is candidate-local evidence captured from the
	// exact account/model snapshot that produced this route. Never infer this
	// from the public model union or from another Kiro credential.
	KiroStructuredOutputFormat   bool
	KiroStructuredOutputKnown    bool
	KiroStructuredOutputConflict bool
}

// providerRouteIndex deliberately keeps provider IDs and client aliases in
// separate namespaces. Provider IDs are case-sensitive. Aliases are normalized
// for compatibility, while a case-insensitive provider-ID fallback is allowed
// only when it identifies exactly one concrete provider ID.
type providerRouteIndex struct {
	Exact           map[ProviderModelID]resolvedProviderModel
	Aliases         map[string]resolvedProviderModel
	FoldedExact     map[string]ProviderModelID
	AmbiguousFolded map[string]bool
}

func newProviderRouteIndex() providerRouteIndex {
	return providerRouteIndex{
		Exact:           make(map[ProviderModelID]resolvedProviderModel),
		Aliases:         make(map[string]resolvedProviderModel),
		FoldedExact:     make(map[string]ProviderModelID),
		AmbiguousFolded: make(map[string]bool),
	}
}

func (routes providerRouteIndex) resolve(requested string) (resolvedProviderModel, bool) {
	exact := exactProviderModelID(requested)
	if exact != "" {
		if route, ok := routes.Exact[exact]; ok {
			return route, true
		}
	}
	key := requestedModelLookupKey(requested)
	if key == "" {
		return resolvedProviderModel{}, false
	}
	if route, ok := routes.Aliases[key]; ok {
		return route, true
	}
	if routes.AmbiguousFolded[key] {
		return resolvedProviderModel{}, false
	}
	providerID, ok := routes.FoldedExact[key]
	if !ok {
		return resolvedProviderModel{}, false
	}
	route, ok := routes.Exact[providerID]
	return route, ok
}

func (routes providerRouteIndex) resolveAlias(alias string) (resolvedProviderModel, bool) {
	route, ok := routes.Aliases[requestedModelLookupKey(alias)]
	return route, ok
}

func (routes providerRouteIndex) coarseModelIDs() []string {
	ids := make([]string, 0, len(routes.Exact)+len(routes.Aliases)+len(routes.FoldedExact))
	for providerID := range routes.Exact {
		ids = append(ids, string(providerID))
	}
	for alias := range routes.Aliases {
		ids = append(ids, alias)
	}
	for folded := range routes.FoldedExact {
		if !routes.AmbiguousFolded[folded] {
			ids = append(ids, folded)
		}
	}
	return ids
}

type providerAccountAvailability struct {
	LifecycleGeneration uint64
	Routes              providerRouteIndex
	CooldownUntil       time.Time
	DeniedUntil         map[string]time.Time
	RefreshedAt         time.Time
	FreshUntil          time.Time
	StaleUntil          time.Time
	Stale               bool
}

type providerPublicModel struct {
	Model             ModelInfo
	Alias             bool
	AnyThinking       bool
	AnyFreshCandidate bool
}

type providerRegistrySnapshot struct {
	Version      uint64
	PublishedAt  time.Time
	Accounts     map[string]providerAccountAvailability
	PublicModels []providerPublicModel
	NextExpiry   time.Time
}

func exactProviderModelID(value string) ProviderModelID {
	return ProviderModelID(strings.TrimSpace(value))
}

// requestedModelLookupKey is exclusively for client-controlled aliases and the
// unique folded-provider fallback. Never use it as a provider identity.
func requestedModelLookupKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// providerModelLookupKey is retained for source compatibility with existing
// callers/tests. It is a lookup key, not an exact provider identity.
func providerModelLookupKey(value string) string {
	return requestedModelLookupKey(value)
}

func exactProviderDenialKey(providerID ProviderModelID) string {
	providerID = exactProviderModelID(string(providerID))
	if providerID == "" {
		return ""
	}
	return providerExactDenialSentinel + string(providerID)
}

func aliasProviderDenialKey(alias string) string {
	key := requestedModelLookupKey(alias)
	if key == "" {
		return ""
	}
	return providerAliasDenialSentinel + key
}

func stripRequestedModelThinkingSuffix(model string, suffix string) string {
	model = strings.TrimSpace(model)
	suffix = strings.TrimSpace(suffix)
	if suffix == "" || len(model) < len(suffix) ||
		!strings.EqualFold(model[len(model)-len(suffix):], suffix) {
		return model
	}
	return strings.TrimSpace(model[:len(model)-len(suffix)])
}

func snapshotRoutable(snapshot accountModelSnapshot, now time.Time) bool {
	return !snapshot.RefreshedAt.IsZero() &&
		!snapshot.StaleUntil.IsZero() && snapshot.StaleUntil.After(now)
}

func snapshotFresh(snapshot accountModelSnapshot, now time.Time) bool {
	return snapshotRoutable(snapshot, now) && !snapshot.Stale &&
		!snapshot.FreshUntil.IsZero() && snapshot.FreshUntil.After(now)
}

type providerRouteBuilder struct {
	routes         providerRouteIndex
	ambiguousAlias map[string]bool
}

func newProviderRouteBuilder() *providerRouteBuilder {
	return &providerRouteBuilder{
		routes:         newProviderRouteIndex(),
		ambiguousAlias: make(map[string]bool),
	}
}

func providerEvidencePriority(evidence providerModelEvidence) int {
	switch evidence {
	case providerModelEvidenceDiscovered:
		return 3
	case providerModelEvidenceOperatorConfigured:
		return 2
	case providerModelEvidenceUniqueAlias:
		return 1
	default:
		return 0
	}
}

func (b *providerRouteBuilder) addExact(
	providerID ProviderModelID,
	evidence providerModelEvidence,
	capability ModelReasoningCapability,
	capabilityKnown bool,
) {
	if b == nil {
		return
	}
	providerID = exactProviderModelID(string(providerID))
	if providerID == "" {
		return
	}
	next := resolvedProviderModel{
		RequestedID:      RequestedModelID(providerID),
		ProviderID:       providerID,
		Evidence:         evidence,
		SupportsThinking: capability.SupportsThinking,
		Reasoning:        capability,
		ReasoningKnown:   capabilityKnown,
	}
	if existing, ok := b.routes.Exact[providerID]; ok {
		existing.SupportsThinking = existing.SupportsThinking || next.SupportsThinking
		if next.ReasoningKnown && (!existing.ReasoningKnown || providerEvidencePriority(evidence) >= providerEvidencePriority(existing.Evidence)) {
			existing.Reasoning = capability
			existing.ReasoningKnown = true
		}
		if providerEvidencePriority(evidence) > providerEvidencePriority(existing.Evidence) {
			existing.Evidence = evidence
		}
		b.routes.Exact[providerID] = existing
	} else {
		b.routes.Exact[providerID] = next
	}

	folded := requestedModelLookupKey(string(providerID))
	if folded == "" || b.routes.AmbiguousFolded[folded] {
		return
	}
	if previous, ok := b.routes.FoldedExact[folded]; ok && previous != providerID {
		delete(b.routes.FoldedExact, folded)
		b.routes.AmbiguousFolded[folded] = true
		return
	}
	b.routes.FoldedExact[folded] = providerID
}

func (b *providerRouteBuilder) addAlias(
	requested string,
	providerID ProviderModelID,
	evidence providerModelEvidence,
) {
	if b == nil {
		return
	}
	key := requestedModelLookupKey(requested)
	providerID = exactProviderModelID(string(providerID))
	if key == "" || providerID == "" || b.ambiguousAlias[key] {
		return
	}
	providerRoute, ok := b.routes.Exact[providerID]
	if !ok {
		return
	}
	next := providerRoute
	next.RequestedID = RequestedModelID(strings.TrimSpace(requested))
	next.Evidence = evidence
	existing, exists := b.routes.Aliases[key]
	if !exists {
		b.routes.Aliases[key] = next
		return
	}
	if existing.ProviderID == providerID {
		if providerEvidencePriority(evidence) > providerEvidencePriority(existing.Evidence) {
			existing.Evidence = evidence
			existing.RequestedID = next.RequestedID
		}
		b.routes.Aliases[key] = existing
		return
	}
	existingPriority := providerEvidencePriority(existing.Evidence)
	nextPriority := providerEvidencePriority(evidence)
	switch {
	case existingPriority > nextPriority:
		return
	case nextPriority > existingPriority:
		b.routes.Aliases[key] = next
	default:
		delete(b.routes.Aliases, key)
		b.ambiguousAlias[key] = true
	}
}

// add preserves the historical test helper while routing discovered IDs into
// the exact namespace and presentation aliases into the alias namespace.
func (b *providerRouteBuilder) add(requested string, providerID ProviderModelID, evidence providerModelEvidence, supportsThinking bool) {
	capability := ModelReasoningCapability{ModelID: string(providerID), SupportsThinking: supportsThinking}
	if exactProviderModelID(requested) == exactProviderModelID(string(providerID)) && evidence == providerModelEvidenceDiscovered {
		b.addExact(providerID, evidence, capability, supportsThinking)
		return
	}
	if _, ok := b.routes.Exact[exactProviderModelID(string(providerID))]; !ok {
		b.addExact(providerID, evidence, capability, supportsThinking)
	}
	b.addAlias(requested, providerID, evidence)
}

type publicAliasPolicy uint8

const (
	publicAliasFixedTarget publicAliasPolicy = iota
	publicAliasFirstAvailable
)

type publicAliasDefinition struct {
	ID     PublicModelID
	Policy publicAliasPolicy
	Target RequestedModelID
}

var logicalPublicModelAliases = []publicAliasDefinition{
	{ID: "gpt-4o", Policy: publicAliasFixedTarget, Target: "gpt-4o"},
	{ID: "gpt-4", Policy: publicAliasFixedTarget, Target: "gpt-4"},
	{ID: "auto", Policy: publicAliasFirstAvailable},
}

func resolvePublicAliasRoute(
	definition publicAliasDefinition,
	routes providerRouteIndex,
	snapshot accountModelSnapshot,
) (resolvedProviderModel, bool) {
	switch definition.Policy {
	case publicAliasFixedTarget:
		target := publicAliasTarget(string(definition.Target))
		return routes.resolve(target)
	case publicAliasFirstAvailable:
		// "auto" is a routing policy, not a provider model ID. Resolve it to the
		// first exact model in this account's immutable discovery snapshot.
		for _, model := range snapshot.Models {
			providerID := exactProviderModelID(model.ModelId)
			if providerID == "" {
				continue
			}
			if route, ok := routes.Exact[providerID]; ok {
				return route, true
			}
		}
	}
	return resolvedProviderModel{}, false
}

func directAnthropicMetadataForProviderID(
	metadata map[string]directAnthropicModelMetadata,
	providerID ProviderModelID,
) (directAnthropicModelMetadata, bool) {
	exact := strings.TrimSpace(string(providerID))
	if exact == "" {
		return directAnthropicModelMetadata{}, false
	}
	if value, ok := metadata[exact]; ok {
		return value, true
	}
	folded := requestedModelLookupKey(exact)
	var (
		selected directAnthropicModelMetadata
		found    bool
	)
	for candidateID, value := range metadata {
		if requestedModelLookupKey(candidateID) != folded {
			continue
		}
		if found {
			// More than one exact provider identity shares the folded spelling.
			// Do not borrow capability metadata from either deployment.
			return directAnthropicModelMetadata{}, false
		}
		selected = value
		found = true
	}
	return selected, found
}

func bedrockOperatorReasoningCapability(
	account *config.Account,
	providerID ProviderModelID,
) (ModelReasoningCapability, bool) {
	if account == nil || providerID == "" {
		return ModelReasoningCapability{}, false
	}
	switch claudeProviderProfileForAccount(account) {
	case claudeProviderBedrockConverse:
		return ModelReasoningCapability{ModelID: string(providerID)}, true
	case claudeProviderBedrockInvoke:
		capability := resolveClaudeProviderReasoningCapability(bedrockModelCore(string(providerID)))
		if strings.TrimSpace(capability.ModelID) == "" {
			return ModelReasoningCapability{}, false
		}
		// Capability comes from the canonical Claude core ID, while routing and
		// diagnostics must preserve the exact Bedrock deployment identity.
		capability.ModelID = string(providerID)
		return capability, true
	default:
		return ModelReasoningCapability{}, false
	}
}

func accountModelReasoningCapability(
	account *config.Account,
	model ModelInfo,
	snapshot accountModelSnapshot,
	fresh bool,
) (ModelReasoningCapability, bool) {
	if account == nil || !fresh {
		return ModelReasoningCapability{}, false
	}
	profile := claudeProviderProfileForAccount(account)
	providerID := exactProviderModelID(model.ModelId)
	switch profile {
	case claudeProviderBedrockConverse, claudeProviderBedrockInvoke:
		return bedrockOperatorReasoningCapability(account, providerID)
	case claudeProviderAnthropicCompatible:
		metadata, ok := directAnthropicMetadataForProviderID(snapshot.Metadata, providerID)
		if !ok || !metadata.CapabilitiesPresent || !directAnthropicMetadataHasReasoningDetail(metadata) {
			return ModelReasoningCapability{}, false
		}
		return directAnthropicReasoningCapability(string(providerID), metadata), true
	case claudeProviderAnthropicOfficial:
		if metadata, ok := directAnthropicMetadataForProviderID(snapshot.Metadata, providerID); ok && metadata.CapabilitiesPresent {
			return directAnthropicReasoningCapability(string(providerID), metadata), true
		}
		capability := resolveClaudeProviderReasoningCapability(string(providerID))
		return capability, strings.TrimSpace(capability.ModelID) != ""
	case claudeProviderKiro:
		capability := ParseModelReasoningCapability(model)
		return capability, strings.TrimSpace(capability.ModelID) != ""
	default:
		return ModelReasoningCapability{}, false
	}
}

func accountModelThinkingSupport(account *config.Account, model ModelInfo, snapshot accountModelSnapshot, fresh bool) bool {
	capability, known := accountModelReasoningCapability(account, model, snapshot, fresh)
	return known && capability.SupportsThinking
}

func buildAccountRoutes(account *config.Account, snapshot accountModelSnapshot, now time.Time) providerRouteIndex {
	builder := newProviderRouteBuilder()
	if account == nil || !snapshotRoutable(snapshot, now) {
		return builder.routes
	}
	fresh := snapshotFresh(snapshot, now)
	providerIDs := make(map[string][]ProviderModelID, len(snapshot.Models))
	for _, model := range snapshot.Models {
		providerID := exactProviderModelID(model.ModelId)
		if providerID == "" {
			continue
		}
		folded := requestedModelLookupKey(string(providerID))
		providerIDs[folded] = append(providerIDs[folded], providerID)
		capability, known := accountModelReasoningCapability(account, model, snapshot, fresh)
		builder.addExact(providerID, providerModelEvidenceDiscovered, capability, known)
		if account.UsesKiroRuntime() && fresh {
			// Capture the additional-request-field capability on this exact
			// candidate route. A different Kiro account advertising the same
			// provider model ID must not make this account look capable.
			if supported, err := modelAdvertisesKiroStructuredOutputFormat(model); err == nil {
				route := builder.routes.Exact[providerID]
				if !route.KiroStructuredOutputConflict {
					if route.KiroStructuredOutputKnown && route.KiroStructuredOutputFormat != supported {
						// Conflicting duplicate evidence for the same exact provider ID is
						// not a capability proof. Keep the conflict sticky so a third
						// duplicate cannot accidentally re-authorize the route.
						route.KiroStructuredOutputKnown = false
						route.KiroStructuredOutputFormat = false
						route.KiroStructuredOutputConflict = true
					} else if !route.KiroStructuredOutputKnown {
						route.KiroStructuredOutputFormat = supported
						route.KiroStructuredOutputKnown = true
					}
				}
				builder.routes.Exact[providerID] = route
			}
		}
	}

	uniqueProviderID := func(value string) (ProviderModelID, bool) {
		matches := providerIDs[requestedModelLookupKey(value)]
		if len(matches) != 1 {
			return "", false
		}
		return matches[0], true
	}

	if account.Kind() == config.AccountKindBedrock {
		for alias, target := range account.BedrockModelMap {
			providerID := exactProviderModelID(target)
			if providerID == "" {
				continue
			}
			if known, ok := uniqueProviderID(string(providerID)); ok {
				providerID = known
			} else {
				// Explicit per-account mappings are operator evidence even when the
				// Bedrock control plane cannot be listed by this credential. Provider
				// reasoning facts come from the canonical Claude core ID, not from a
				// possibly unavailable discovery response.
				capability, capabilityKnown := bedrockOperatorReasoningCapability(account, providerID)
				builder.addExact(providerID, providerModelEvidenceOperatorConfigured, capability, capabilityKnown)
				providerIDs[requestedModelLookupKey(string(providerID))] = append(providerIDs[requestedModelLookupKey(string(providerID))], providerID)
			}
			builder.addAlias(alias, providerID, providerModelEvidenceOperatorConfigured)
		}
		for alias, target := range defaultBedrockModelMap {
			providerID, ok := uniqueProviderID(target)
			if !ok {
				continue
			}
			builder.addAlias(alias, providerID, providerModelEvidenceUniqueAlias)
		}
	}

	// Logical aliases are registry data. They are published/routable only when
	// their concrete target exists in this same account snapshot.
	for _, definition := range logicalPublicModelAliases {
		alias := string(definition.ID)
		resolved, ok := resolvePublicAliasRoute(definition, builder.routes, snapshot)
		if !ok {
			continue
		}
		builder.addAlias(alias, resolved.ProviderID, providerModelEvidenceUniqueAlias)
	}
	return builder.routes
}

func providerRouteDenied(
	availability providerAccountAvailability,
	requested string,
	route resolvedProviderModel,
	now time.Time,
) bool {
	if len(availability.DeniedUntil) == 0 {
		return false
	}
	keys := []string{
		exactProviderDenialKey(route.ProviderID),
		aliasProviderDenialKey(requested),
		// Legacy runtime keys from earlier generations are honored until expiry.
		requestedModelLookupKey(string(route.ProviderID)),
		requestedModelLookupKey(requested),
	}
	for _, key := range keys {
		if key != "" && availability.DeniedUntil[key].After(now) {
			return true
		}
	}
	return false
}

func providerCandidateUnavailable(
	registry *providerRegistrySnapshot,
	account *config.Account,
	requested string,
	route resolvedProviderModel,
	now time.Time,
) (bool, string) {
	if account == nil {
		return true, "missing-account"
	}
	if registry == nil {
		if providerAccountRequiresRegistry(account) {
			return true, "registry-unavailable"
		}
		return false, ""
	}
	availability, ok := registry.Accounts[account.ID]
	if !ok {
		if providerAccountRequiresRegistry(account) {
			return true, "account-not-in-registry"
		}
		return false, ""
	}
	if account.DispatchGeneration != 0 && availability.LifecycleGeneration != account.DispatchGeneration {
		return true, "account-lifecycle-changed"
	}
	if availability.CooldownUntil.After(now) {
		return true, "provider-cooldown"
	}
	if availability.StaleUntil.IsZero() || !availability.StaleUntil.After(now) {
		return true, "model-snapshot-expired"
	}
	if providerRouteDenied(availability, requested, route, now) {
		return true, "model-denied"
	}
	return false, ""
}

func buildProviderRegistry(
	version uint64,
	enabled []config.Account,
	snapshots map[string]accountModelSnapshot,
	cooldowns map[string]time.Time,
	denials map[string]map[string]time.Time,
	allowOverUsage bool,
	now time.Time,
) *providerRegistrySnapshot {
	registry := &providerRegistrySnapshot{
		Version:     version,
		PublishedAt: now,
		Accounts:    make(map[string]providerAccountAvailability, len(enabled)),
	}
	type aggregate struct {
		model    ModelInfo
		thinking bool
		fresh    bool
		exact    bool
		alias    bool
	}
	models := make(map[string]aggregate)
	accountsByID := make(map[string]*config.Account, len(enabled))

	for index := range enabled {
		account := &enabled[index]
		if account.UsageLimit > 0 && account.UsageCurrent >= account.UsageLimit &&
			!strings.EqualFold(account.OverageStatus, "ENABLED") && !allowOverUsage {
			continue
		}
		snapshot, ok := snapshots[account.ID]
		if !ok {
			continue
		}
		availability := providerAccountAvailability{
			LifecycleGeneration: account.DispatchGeneration,
			Routes:              buildAccountRoutes(account, snapshot, now),
			CooldownUntil:       cooldowns[account.ID],
			DeniedUntil:         denials[account.ID],
			RefreshedAt:         snapshot.RefreshedAt,
			FreshUntil:          snapshot.FreshUntil,
			StaleUntil:          snapshot.StaleUntil,
			Stale:               snapshot.Stale,
		}
		registry.Accounts[account.ID] = availability
		accountsByID[account.ID] = account
		for _, expiry := range []time.Time{availability.CooldownUntil, availability.FreshUntil, availability.StaleUntil} {
			if expiry.After(now) && (registry.NextExpiry.IsZero() || expiry.Before(registry.NextExpiry)) {
				registry.NextExpiry = expiry
			}
		}
		for _, expiry := range availability.DeniedUntil {
			if expiry.After(now) && (registry.NextExpiry.IsZero() || expiry.Before(registry.NextExpiry)) {
				registry.NextExpiry = expiry
			}
		}

		if availability.CooldownUntil.After(now) || !snapshotRoutable(snapshot, now) {
			continue
		}
		fresh := snapshotFresh(snapshot, now)
		for _, model := range snapshot.Models {
			providerID := exactProviderModelID(model.ModelId)
			route, ok := availability.Routes.Exact[providerID]
			if providerID == "" || !ok || providerRouteDenied(availability, string(providerID), route, now) {
				continue
			}
			key := string(providerID) // exact, case-sensitive public identity
			current := models[key]
			if strings.TrimSpace(current.model.ModelId) == "" {
				current.model = model
			} else {
				current.model = mergeModelInfo(current.model, model)
			}
			current.exact = true
			current.fresh = current.fresh || fresh
			current.thinking = current.thinking || route.SupportsThinking
			models[key] = current
		}
	}

	// Publish logical aliases only when at least one live account resolves the
	// alias in the same registry generation. An exact provider ID with identical
	// spelling remains authoritative.
	for _, definition := range logicalPublicModelAliases {
		alias := string(definition.ID)
		if current, exists := models[alias]; exists && current.exact {
			continue
		}
		var aggregateAlias aggregate
		for accountID, availability := range registry.Accounts {
			if availability.CooldownUntil.After(now) {
				continue
			}
			route, ok := availability.Routes.resolveAlias(alias)
			if !ok || providerRouteDenied(availability, alias, route, now) {
				continue
			}
			account := accountsByID[accountID]
			snapshot := snapshots[accountID]
			if account == nil || !snapshotRoutable(snapshot, now) {
				continue
			}
			model := ModelInfo{ModelId: alias}
			for _, candidate := range snapshot.Models {
				if exactProviderModelID(candidate.ModelId) == route.ProviderID {
					model = candidate
					model.ModelId = alias
					break
				}
			}
			if strings.TrimSpace(aggregateAlias.model.ModelId) == "" {
				aggregateAlias.model = model
			} else {
				aggregateAlias.model = mergeModelInfo(aggregateAlias.model, model)
				aggregateAlias.model.ModelId = alias
			}
			aggregateAlias.fresh = aggregateAlias.fresh || snapshotFresh(snapshot, now)
			aggregateAlias.thinking = aggregateAlias.thinking || route.SupportsThinking
		}
		if strings.TrimSpace(aggregateAlias.model.ModelId) != "" {
			aggregateAlias.alias = true
			models[alias] = aggregateAlias
		}
	}

	keys := make([]string, 0, len(models))
	for key := range models {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := requestedModelLookupKey(keys[i]), requestedModelLookupKey(keys[j])
		if left == right {
			return keys[i] < keys[j]
		}
		return left < right
	})
	registry.PublicModels = make([]providerPublicModel, 0, len(keys))
	for _, key := range keys {
		value := models[key]
		registry.PublicModels = append(registry.PublicModels, providerPublicModel{
			Model:             value.model,
			Alias:             value.alias,
			AnyThinking:       value.thinking,
			AnyFreshCandidate: value.fresh,
		})
	}
	return registry
}

func (h *Handler) currentProviderRegistry() *providerRegistrySnapshot {
	if h == nil {
		return nil
	}
	return h.providerRegistry.Load()
}

func resolveProviderRouteForAccount(
	registry *providerRegistrySnapshot,
	accountID string,
	requested string,
) (resolvedProviderModel, uint64, bool) {
	if registry == nil {
		return resolvedProviderModel{}, 0, false
	}
	account, ok := registry.Accounts[accountID]
	if !ok {
		return resolvedProviderModel{}, registry.Version, false
	}
	resolved, ok := account.Routes.resolve(requested)
	if !ok {
		return resolvedProviderModel{}, registry.Version, false
	}
	return resolved, registry.Version, true
}

func (h *Handler) resolveProviderModelForAccount(accountID, requested string) (ProviderModelID, uint64, bool) {
	resolved, version, ok := resolveProviderRouteForAccount(h.currentProviderRegistry(), accountID, requested)
	return resolved.ProviderID, version, ok
}

func resolveProviderRouteForAccountLifecycle(
	registry *providerRegistrySnapshot,
	account *config.Account,
	requested string,
) (resolvedProviderModel, uint64, bool) {
	if account == nil || registry == nil {
		return resolvedProviderModel{}, 0, false
	}
	availability, ok := registry.Accounts[account.ID]
	if !ok {
		return resolvedProviderModel{}, registry.Version, false
	}
	if account.DispatchGeneration != 0 && availability.LifecycleGeneration != account.DispatchGeneration {
		return resolvedProviderModel{}, registry.Version, false
	}
	resolved, ok := availability.Routes.resolve(requested)
	if !ok {
		return resolvedProviderModel{}, registry.Version, false
	}
	return resolved, registry.Version, true
}

func providerAccountRequiresRegistry(account *config.Account) bool {
	if account == nil {
		return false
	}
	switch claudeTransportKindForAccount(account) {
	case claudeTransportDirectAnthropic, claudeTransportBedrockInvoke, claudeTransportBedrockConverse:
		return true
	default:
		return false
	}
}

func resolveCandidateProviderRoute(
	registry *providerRegistrySnapshot,
	account *config.Account,
	requestedModel string,
	mappedModel string,
) (resolvedProviderModel, uint64, bool) {
	if account == nil {
		return resolvedProviderModel{}, 0, false
	}
	requestedModel = strings.TrimSpace(requestedModel)
	mappedModel = strings.TrimSpace(mappedModel)
	if requestedModel == "" {
		requestedModel = mappedModel
	}
	if route, version, ok := resolveProviderRouteForAccountLifecycle(registry, account, requestedModel); ok {
		return route, version, true
	}
	if requestedModel != mappedModel {
		if route, version, ok := resolveProviderRouteForAccountLifecycle(registry, account, mappedModel); ok {
			return route, version, true
		}
	}
	version := uint64(0)
	if registry != nil {
		version = registry.Version
	}
	if providerAccountRequiresRegistry(account) {
		return resolvedProviderModel{}, version, false
	}
	fallback := mappedModel
	if account.IsCustomApi() && claudeTransportKindForAccount(account) == claudeTransportCustomRelay {
		fallback = requestedModel
	}
	providerID := exactProviderModelID(fallback)
	if providerID == "" {
		return resolvedProviderModel{}, version, false
	}
	return resolvedProviderModel{
		RequestedID: RequestedModelID(requestedModel),
		ProviderID:  providerID,
		Evidence:    providerModelEvidenceUnknown,
	}, version, true
}

func (h *Handler) resolveCandidateProviderModel(
	account *config.Account,
	requestedModel string,
	mappedModel string,
) (ProviderModelID, uint64, bool) {
	route, version, ok := resolveCandidateProviderRoute(h.currentProviderRegistry(), account, requestedModel, mappedModel)
	return route.ProviderID, version, ok
}

func publicAliasTarget(alias string) string {
	return MapModel(alias)
}
