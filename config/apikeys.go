package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// ListApiKeys returns a snapshot of all configured API key entries.
func ListApiKeys() []ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]ApiKeyEntry, len(cfg.ApiKeys))
	copy(out, cfg.ApiKeys)
	return out
}

// GetApiKeyEntry returns a copy of the entry with the given ID, or nil if not found.
func GetApiKeyEntry(id string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cp := cfg.ApiKeys[i]
			return &cp
		}
	}
	return nil
}

// AddApiKey appends a new API key entry. Generates ID and CreatedAt if missing,
// rejects empty Key values, and refuses duplicates of an existing Key.
func AddApiKey(entry ApiKeyEntry) (ApiKeyEntry, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return ApiKeyEntry{}, errors.New("config not initialized")
	}
	entry.Key = strings.TrimSpace(entry.Key)
	if entry.Key == "" {
		return ApiKeyEntry{}, errors.New("api key value must not be empty")
	}
	if entry.ID == "" {
		entry.ID = newUUID()
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = time.Now().Unix()
	}
	_, err := publishConfigFieldsLocked(func(next *Config) error {
		for _, existing := range next.ApiKeys {
			if existing.Key == entry.Key {
				return errors.New("api key already exists")
			}
		}
		next.ApiKeys = append(next.ApiKeys, entry)
		return nil
	})
	if err != nil {
		return ApiKeyEntry{}, err
	}
	return entry, nil
}

func UpdateApiKey(id string, patch ApiKeyEntry) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	_, err := publishConfigFieldsLocked(func(next *Config) error {
		idx := -1
		for i := range next.ApiKeys {
			if next.ApiKeys[i].ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return errors.New("api key not found")
		}
		if patch.Name != "" {
			next.ApiKeys[idx].Name = patch.Name
		}
		if patch.Key != "" {
			newKey := strings.TrimSpace(patch.Key)
			for j := range next.ApiKeys {
				if j != idx && next.ApiKeys[j].Key == newKey {
					return errors.New("api key value collides with existing entry")
				}
			}
			next.ApiKeys[idx].Key = newKey
		}
		next.ApiKeys[idx].Enabled = patch.Enabled
		next.ApiKeys[idx].TokenLimit = patch.TokenLimit
		next.ApiKeys[idx].CreditLimit = patch.CreditLimit
		if patch.Migrated {
			next.ApiKeys[idx].Migrated = true
		}
		return nil
	})
	return err
}

func DeleteApiKey(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	found := false
	for _, entry := range cfg.ApiKeys {
		if entry.ID == id {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	_, err := publishConfigFieldsLocked(func(next *Config) error {
		for i, entry := range next.ApiKeys {
			if entry.ID == id {
				next.ApiKeys = append(next.ApiKeys[:i], next.ApiKeys[i+1:]...)
				return nil
			}
		}
		return errors.New("api key changed while staging delete")
	})
	return err
}

func FindApiKeyByValue(key string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || key == "" {
		return nil
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].Key == key {
			cp := cfg.ApiKeys[i]
			return &cp
		}
	}
	return nil
}

// HasApiKeys returns true when at least one API key entry is configured.
func HasApiKeys() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return len(cfg.ApiKeys) > 0
}

// RecordApiKeyUsage atomically adds tokens and credits to the entry's counters,
// updates LastUsedAt, increments RequestsCount, and persists.
//
// Quota enforcement: when the updated counters reach a configured limit
// (token or credit), the key is deactivated (Enabled=false) in the same write.
// This is the "sold quota" contract used by the Telegram bot flow — a key sold
// with N credits stops working permanently once N credits are consumed, and
// stays off until an admin re-enables it (e.g. after a top-up).
func RecordApiKeyUsage(id string, tokens int64, credits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	_, err := publishConfigFieldsLocked(func(next *Config) error {
		for i := range next.ApiKeys {
			if next.ApiKeys[i].ID != id {
				continue
			}
			if tokens > 0 {
				next.ApiKeys[i].TokensUsed += tokens
			}
			if credits > 0 {
				next.ApiKeys[i].CreditsUsed += credits
			}
			next.ApiKeys[i].RequestsCount++
			next.ApiKeys[i].LastUsedAt = time.Now().Unix()
			if overToken, overCredit := ApiKeyOverLimit(next.ApiKeys[i]); overToken || overCredit {
				next.ApiKeys[i].Enabled = false
			}
			return nil
		}
		return errors.New("api key not found")
	})
	return err
}

func ResetApiKeyUsage(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	_, err := publishConfigFieldsLocked(func(next *Config) error {
		for i := range next.ApiKeys {
			if next.ApiKeys[i].ID == id {
				next.ApiKeys[i].TokensUsed = 0
				next.ApiKeys[i].CreditsUsed = 0
				next.ApiKeys[i].RequestsCount = 0
				return nil
			}
		}
		return errors.New("api key not found")
	})
	return err
}

func RechargeApiKey(id string, addCredits float64, addTokens int64) (ApiKeyEntry, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return ApiKeyEntry{}, errors.New("config not initialized")
	}
	var updated ApiKeyEntry
	_, err := publishConfigFieldsLocked(func(next *Config) error {
		for i := range next.ApiKeys {
			if next.ApiKeys[i].ID != id {
				continue
			}
			if addCredits > 0 {
				next.ApiKeys[i].CreditLimit += addCredits
			}
			if addTokens > 0 {
				next.ApiKeys[i].TokenLimit += addTokens
			}
			if overToken, overCredit := ApiKeyOverLimit(next.ApiKeys[i]); !overToken && !overCredit {
				next.ApiKeys[i].Enabled = true
			}
			updated = next.ApiKeys[i]
			return nil
		}
		return errors.New("api key not found")
	})
	if err != nil {
		return ApiKeyEntry{}, err
	}
	return updated, nil
}

func GenerateApiKeyValue() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return "sk-" + hex.EncodeToString(buf)
}

// MaskApiKey produces a display-friendly masked version: keeps first 6 and last 4
// characters, replaces the middle with "****". Returns "" for empty input and
// the original string if it's too short to mask meaningfully.
func MaskApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 10 {
		return key
	}
	return key[:6] + "****" + key[len(key)-4:]
}

// ApiKeyOverLimit returns (overToken, overCredit) for the entry. Limits with value 0
// are ignored. The function does not lock; callers should pass a copied entry.
func ApiKeyOverLimit(e ApiKeyEntry) (overToken bool, overCredit bool) {
	if e.TokenLimit > 0 && e.TokensUsed >= e.TokenLimit {
		overToken = true
	}
	if e.CreditLimit > 0 && e.CreditsUsed >= e.CreditLimit {
		overCredit = true
	}
	return
}
