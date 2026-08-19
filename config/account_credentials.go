package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrAccountNotFound       = errors.New("account not found")
	ErrDuplicateAccountID    = errors.New("account ID already exists")
	ErrDuplicateRefreshToken = errors.New("account refresh token already exists")
	ErrDuplicateAPIKey       = errors.New("account API key already exists")
	ErrEmptyAPIKey           = errors.New("kiroApiKey is empty")
)

// RefreshTokenFingerprint returns a stable, non-reversible identifier for an
// opaque refresh token. Empty tokens do not receive a fingerprint.
func RefreshTokenFingerprint(refreshToken string) string {
	if refreshToken == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(refreshToken))
	return fmt.Sprintf("%x", sum[:])
}

// APIKeyFingerprint returns a stable, non-reversible identifier for a Kiro API key.
func APIKeyFingerprint(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return fmt.Sprintf("%x", sum[:])
}

// normalizedKiroRegion canonicalizes an account region for equality tests. An
// empty region is the historical us-east-1 default.
func normalizedKiroRegion(region string) string {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		return "us-east-1"
	}
	return region
}

// IsAPIKeyAccount reports whether the account authenticates against Kiro with a
// Kiro API key. Unknown/import-shaped records may carry a bare KiroApiKey before
// normalization. Non-Kiro provider kinds must never be reclassified this way.
func IsAPIKeyAccount(account *Account) bool {
	if account == nil {
		return false
	}
	switch account.Kind() {
	case AccountKindCustomRelay, AccountKindBedrock, AccountKindDirectAnthropic:
		return false
	case AccountKindKiroAPIKey:
		return true
	}
	if strings.TrimSpace(account.KiroApiKey) != "" {
		return true
	}
	method := strings.ToLower(strings.TrimSpace(account.AuthMethod))
	return method == "api_key" || method == "apikey"
}

// SplitKiroAPIKeyAndRegion parses the convenience form "key|region".
func SplitKiroAPIKeyAndRegion(raw string) (key, region string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", ErrEmptyAPIKey
	}
	parts := strings.Split(trimmed, "|")
	if len(parts) > 2 {
		return "", "", errors.New("multiple pipe separators are not allowed")
	}
	key = strings.TrimSpace(parts[0])
	if key == "" {
		return "", "", errors.New("key before pipe is empty")
	}
	if len(parts) == 2 {
		region = strings.TrimSpace(parts[1])
		if region == "" {
			return "", "", errors.New("region after pipe is empty")
		}
		if err := validateKiroRegionHostLabel(region); err != nil {
			return "", "", err
		}
	}
	return key, region, nil
}

func validateKiroRegionHostLabel(region string) error {
	region = strings.TrimSpace(region)
	if region == "" {
		return errors.New("region is empty")
	}
	for _, r := range region {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return errors.New("region contains host-unsafe characters")
	}
	if strings.Contains(region, " ") || strings.ContainsAny(region, "\n\r\t./") {
		return errors.New("region contains host-unsafe characters")
	}
	return nil
}

// MachineIdFromAPIKey derives the machine id used by Kiro CLI/API-key clients.
func MachineIdFromAPIKey(apiKey string) string {
	sum := sha256.Sum256([]byte("KiroAPIKey/" + apiKey))
	return fmt.Sprintf("%x", sum[:])
}

// NormalizeAPIKeyAccount canonicalizes an imported/headless Kiro API-key account.
func NormalizeAPIKeyAccount(account *Account) error {
	if account == nil {
		return errors.New("account is nil")
	}
	if account.IsCustomApi() || account.IsBedrock() || account.IsDirectAnthropic() {
		return fmt.Errorf("account kind %s is not a Kiro API-key credential", account.Kind())
	}
	raw := strings.TrimSpace(account.KiroApiKey)
	if raw == "" {
		raw = strings.TrimSpace(account.AccessToken)
	}
	key, region, err := SplitKiroAPIKeyAndRegion(raw)
	if err != nil {
		return err
	}
	account.KiroApiKey = key
	account.AccessToken = key
	account.AuthMethod = "api_key"
	account.RefreshToken = ""
	account.RefreshTokenFingerprint = ""
	account.ClientID = ""
	account.ClientSecret = ""
	account.TokenEndpoint = ""
	account.IssuerURL = ""
	account.Scopes = ""
	account.ProfileArn = ""
	account.ExpiresAt = 0
	if region != "" && strings.TrimSpace(account.Region) == "" {
		account.Region = region
	}
	if strings.TrimSpace(account.Region) == "" {
		account.Region = "us-east-1"
	}
	if err := validateKiroRegionHostLabel(account.Region); err != nil {
		return err
	}
	if strings.TrimSpace(account.MachineId) == "" {
		account.MachineId = MachineIdFromAPIKey(key)
	}
	if strings.TrimSpace(account.Provider) == "" {
		account.Provider = "APIKey"
	}
	if strings.TrimSpace(account.Email) == "" {
		fp := APIKeyFingerprint(key)
		if len(fp) > 12 {
			fp = fp[:12]
		}
		account.Email = "api-key-" + fp
	}
	return nil
}

// AccountCredentialExists checks both the current refresh token and the
// original credential fingerprint while holding the configuration read lock.
func AccountCredentialExists(refreshToken string) bool {
	if refreshToken == "" {
		return false
	}
	fingerprint := RefreshTokenFingerprint(refreshToken)
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	for _, account := range cfg.Accounts {
		if account.RefreshToken == refreshToken ||
			(fingerprint != "" && account.RefreshTokenFingerprint == fingerprint) {
			return true
		}
	}
	return false
}

// AccountAPIKeyExists reports whether a Kiro API key is already persisted.
func AccountAPIKeyExists(apiKey string) bool {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return false
	}
	fingerprint := APIKeyFingerprint(apiKey)
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	for i := range cfg.Accounts {
		account := &cfg.Accounts[i]
		if !IsAPIKeyAccount(account) {
			continue
		}
		existing := strings.TrimSpace(account.KiroApiKey)
		if existing != "" && (existing == apiKey || APIKeyFingerprint(existing) == fingerprint) {
			return true
		}
	}
	return false
}

// prepareAccountForAdd canonicalizes credential metadata before the account is
// admitted into the transaction and duplicate checks are performed.
func prepareAccountForAdd(account *Account) error {
	// DispatchGeneration is process-local request state and must never be admitted
	// into the canonical config image from a recycled request/admin snapshot.
	account.DispatchGeneration = 0
	if IsAPIKeyAccount(account) {
		return NormalizeAPIKeyAccount(account)
	}
	if account.RefreshTokenFingerprint == "" {
		account.RefreshTokenFingerprint = RefreshTokenFingerprint(account.RefreshToken)
	}
	return nil
}

func rejectDuplicateAccount(next *Config, account *Account) error {
	for i := range next.Accounts {
		existing := &next.Accounts[i]
		if account.ID != "" && existing.ID == account.ID {
			return ErrDuplicateAccountID
		}
		if account.RefreshToken != "" && existing.RefreshToken == account.RefreshToken {
			return ErrDuplicateRefreshToken
		}
		existingFingerprint := existing.RefreshTokenFingerprint
		if existingFingerprint == "" {
			existingFingerprint = RefreshTokenFingerprint(existing.RefreshToken)
		}
		if account.RefreshTokenFingerprint != "" && existingFingerprint == account.RefreshTokenFingerprint {
			return ErrDuplicateRefreshToken
		}
		if IsAPIKeyAccount(account) && IsAPIKeyAccount(existing) &&
			normalizedKiroRegion(existing.Region) == normalizedKiroRegion(account.Region) {
			existingKey := strings.TrimSpace(existing.KiroApiKey)
			if existingKey != "" && (existingKey == account.KiroApiKey ||
				APIKeyFingerprint(existingKey) == APIKeyFingerprint(account.KiroApiKey)) {
				return ErrDuplicateAPIKey
			}
		}
	}
	return nil
}

// preserveAuthoritativeCredentialState applies a stale administrative snapshot
// without allowing it to roll back credentials refreshed concurrently. Region is
// intentionally editable; blank means "not supplied" and falls back to storage.
func preserveAuthoritativeCredentialState(current Account, incoming *Account) {
	incoming.ID = current.ID
	incoming.DispatchGeneration = 0
	incoming.AccessToken = current.AccessToken
	incoming.RefreshToken = current.RefreshToken
	incoming.RefreshTokenFingerprint = current.RefreshTokenFingerprint
	incoming.KiroApiKey = current.KiroApiKey
	incoming.ClientID = current.ClientID
	incoming.ClientSecret = current.ClientSecret
	incoming.AuthMethod = current.AuthMethod
	incoming.Provider = current.Provider
	if strings.TrimSpace(incoming.Region) == "" {
		incoming.Region = current.Region
	}
	incoming.StartUrl = current.StartUrl
	incoming.ExpiresAt = current.ExpiresAt
	incoming.ProfileArn = current.ProfileArn
	incoming.TokenEndpoint = current.TokenEndpoint
	incoming.IssuerURL = current.IssuerURL
	incoming.Scopes = current.Scopes
	if incoming.RefreshTokenFingerprint == "" {
		incoming.RefreshTokenFingerprint = RefreshTokenFingerprint(current.RefreshToken)
	}
}

// UpdateAccountCredentials deliberately replaces credential material. Unlike
// UpdateAccount, this is an identity-changing operation and therefore advances
// the account lifecycle generation when the persisted dispatch identity changes.
func UpdateAccountCredentials(id string, account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	account = account.Clone()
	account.DispatchGeneration = 0
	account.RefreshTokenFingerprint = RefreshTokenFingerprint(account.RefreshToken)
	found, _, err := publishAccountByIDLocked(id, true, func(current *Account) error {
		account.ID = current.ID
		*current = account.Clone()
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return ErrAccountNotFound
	}
	return nil
}

// ClearAccountBanStatus marks an account active without replacing credentials
// from a potentially stale caller snapshot.
func ClearAccountBanStatus(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, _, err := publishAccountByIDLocked(id, false, func(account *Account) error {
		account.BanStatus = "ACTIVE"
		account.BanReason = ""
		account.BanTime = 0
		return nil
	})
	return err
}

// UpdateAccountRegion persists a validated runtime/API-key region. Region is a
// dispatch-identity field, so changing it advances lifecycle generation.
func UpdateAccountRegion(id, region string) error {
	if err := validateKiroRegionHostLabel(region); err != nil {
		return fmt.Errorf("invalid region %q: %w", region, err)
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	found, _, err := publishAccountByIDLocked(id, true, func(account *Account) error {
		account.Region = strings.TrimSpace(region)
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("account not found: %s", id)
	}
	return nil
}

// UpdateAccountCredentialState atomically publishes the fields produced by one
// refresh-token exchange. A normal refresh intentionally stays within the same
// logical account lifecycle.
func UpdateAccountCredentialState(id, accessToken, refreshToken string, expiresAt int64, profileArn string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	found, _, err := publishAccountByIDLocked(id, false, func(account *Account) error {
		if account.RefreshTokenFingerprint == "" {
			account.RefreshTokenFingerprint = RefreshTokenFingerprint(account.RefreshToken)
		}
		account.AccessToken = accessToken
		if refreshToken != "" {
			account.RefreshToken = refreshToken
		}
		account.ExpiresAt = expiresAt
		if profileArn != "" {
			account.ProfileArn = profileArn
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return ErrAccountNotFound
	}
	return nil
}
