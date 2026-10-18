package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const transientLocalResponseIDPrefix = "resp_kiro_ephemeral_"

type responseIDClass uint8

const (
	responseIDNone responseIDClass = iota
	responseIDExternal
	responseIDTransientLocal
	responseIDNamespacedLocal
	responseIDLegacyAmbiguous
)

// classifyResponseID is deliberately pure: routing code can decide whether a
// previous_response_id requires local state without touching the filesystem or
// the fail-stop state-store latch.
func classifyResponseID(id string) responseIDClass {
	id = strings.TrimSpace(id)
	if id == "" {
		return responseIDNone
	}
	if strings.HasPrefix(id, transientLocalResponseIDPrefix) {
		return responseIDTransientLocal
	}
	if _, ok := responseIDInstanceNamespace(id); ok {
		return responseIDNamespacedLocal
	}
	if strings.HasPrefix(id, localResponseIDPrefix) || isHistoricalPrePrefixResponseID(id) {
		return responseIDLegacyAmbiguous
	}
	return responseIDExternal
}

// isHistoricalPrePrefixResponseID recognizes only the two response-ID grammars
// emitted by Kiro-Go before the resp_kiro_ namespace was introduced. The
// result is deliberately only "ambiguous": an exact local-state hit is still
// required before routing treats the ID as locally owned.
func isHistoricalPrePrefixResponseID(id string) bool {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "resp_") {
		return false
	}
	rest := strings.TrimPrefix(id, "resp_")

	// Normal legacy generator: 12 random bytes (24 lowercase hex chars) plus
	// an 8-character lowercase-hex Unix-seconds suffix.
	if len(rest) == 32 && isLowerHexResponseIDBody(rest) {
		return true
	}

	// rand.Read fallback: decimal UnixNano followed by exactly twelve zeroes.
	const zeroSuffix = "000000000000"
	if len(rest) <= len(zeroSuffix) || !strings.HasSuffix(rest, zeroSuffix) {
		return false
	}
	decimal := rest[:len(rest)-len(zeroSuffix)]
	if decimal == "" || decimal[0] == '0' {
		return false
	}
	for i := 0; i < len(decimal); i++ {
		if decimal[i] < '0' || decimal[i] > '9' {
			return false
		}
	}
	value, err := strconv.ParseInt(decimal, 10, 64)
	return err == nil && value > 0
}

func isLowerHexResponseIDBody(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return value != ""
}

func generateTransientResponseID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate transient Responses id: %w", err)
	}
	return transientLocalResponseIDPrefix + hex.EncodeToString(buf[:]), nil
}

func generateResponseIDForStore(store bool) (string, error) {
	if !store {
		return generateTransientResponseID()
	}
	return generateResponseID()
}

func resolveResponseStateOwnershipByClass(id string, class responseIDClass) (responseStateOwnership, error) {
	id = strings.TrimSpace(id)
	switch class {
	case responseIDNone:
		return responseStateNoPrevious, nil
	case responseIDExternal:
		return responseStateExternal, nil
	case responseIDTransientLocal:
		// These IDs are intentionally never persisted. Keep them locally owned so
		// they can never be forwarded to a custom relay as if they were upstream
		// state.
		return responseStateLocalMissing, errStoredResponseNotFound
	case responseIDLegacyAmbiguous:
		// Historical IDs (both pre-prefix resp_* and prefix-only resp_kiro_*)
		// cannot prove ownership syntactically. An exact local hit is authoritative;
		// a miss remains external for backward-compatible transparent relay.
		_, err := loadResponse(id)
		if err == nil {
			return responseStateLocal, nil
		}
		if errors.Is(err, errStoredResponseNotFound) {
			return responseStateExternal, nil
		}
		return responseStateUnavailable, err
	case responseIDNamespacedLocal:
		idNamespace, _ := responseIDInstanceNamespace(id)
		localNamespace, err := responseInstanceNamespace()
		if err != nil {
			return responseStateUnavailable, err
		}
		if idNamespace != localNamespace {
			return responseStateExternal, nil
		}
		_, err = loadResponse(id)
		if err == nil {
			return responseStateLocal, nil
		}
		if errors.Is(err, errStoredResponseNotFound) {
			return responseStateLocalMissing, errStoredResponseNotFound
		}
		return responseStateUnavailable, err
	default:
		return responseStateUnavailable, fmt.Errorf("unknown Responses id class %d", class)
	}
}
