package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	"kiro-go/internal/privatefile"
	"kiro-go/logger"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	responsesDirName                        = "responses"
	responsesDefaultTTL                     = 30 * 24 * time.Hour
	localResponseIDPrefix                   = "resp_kiro_"
	responseStateFilePrefix                 = "response_"
	responseInstanceNamespaceFile           = ".instance-id"
	responseInstanceNamespaceSize           = 16
	storedResponseStateVersion              = 2
	responsesStatePersistenceFailureMessage = "Failed to persist local Responses state"
	responsesTempCleanupGrace               = 24 * time.Hour
)

var (
	errStoredResponseNotFound           = errors.New("stored response not found")
	errStoredResponseIdentityMismatch   = errors.New("stored response identity mismatch")
	errStoredResponseVersionUnsupported = errors.New("stored response version is unsupported")
	errResponseInstanceNamespaceCorrupt = errors.New("Responses instance namespace is corrupt")
	errResponseStateUnavailable         = errors.New("Responses state store is unavailable until process restart")

	publishResponseStateNoReplaceForWrite  = publishResponseStateNoReplace
	syncResponseStateDirectoryForWrite     = syncResponseStateDirectory
	saveResponseForCompletion              = saveResponse
	ensureResponseStateOwnerPrivateForRead = func(path string) error {
		return privatefile.EnsureOwnerPrivate(path, 0o600)
	}

	responseStateHealthMu   sync.RWMutex
	responseStateHealthErr  error
	responseStateSessionMu  sync.Mutex
	responseStateSessionDir string
)

type responseStateDurabilityError struct{ err error }

func (e *responseStateDurabilityError) Error() string {
	return fmt.Sprintf("Responses state was published but durability sync failed: %v", e.err)
}
func (e *responseStateDurabilityError) Unwrap() error { return e.err }

type responseStateCommitState uint8

const (
	responseStateNotPublished responseStateCommitState = iota
	responseStatePublishedDurable
	responseStateDurabilityUnknown
)

type responseStateUnavailableError struct{ cause error }

func (e *responseStateUnavailableError) Error() string {
	if e == nil || e.cause == nil {
		return errResponseStateUnavailable.Error()
	}
	return fmt.Sprintf("%s: %v", errResponseStateUnavailable, e.cause)
}
func (e *responseStateUnavailableError) Unwrap() error { return e.cause }
func (e *responseStateUnavailableError) Is(target error) bool {
	return target == errResponseStateUnavailable
}

func responseStateHealthError() error {
	responseStateHealthMu.RLock()
	defer responseStateHealthMu.RUnlock()
	return responseStateHealthErr
}

func poisonResponseState(cause error) error {
	if cause == nil {
		cause = errors.New("unknown Responses state durability failure")
	}
	responseStateHealthMu.Lock()
	defer responseStateHealthMu.Unlock()
	if responseStateHealthErr == nil {
		responseStateHealthErr = &responseStateUnavailableError{cause: cause}
	}
	return responseStateHealthErr
}

func resetResponseStateHealthForTest() {
	responseStateHealthMu.Lock()
	responseStateHealthErr = nil
	responseStateHealthMu.Unlock()
	responseStateSessionMu.Lock()
	responseStateSessionDir = ""
	responseStateSessionMu.Unlock()
}

func establishResponseStateSession(dir string) error {
	if err := responseStateHealthError(); err != nil {
		return err
	}
	dir = filepath.Clean(dir)
	responseStateSessionMu.Lock()
	defer responseStateSessionMu.Unlock()
	if err := responseStateHealthError(); err != nil {
		return err
	}
	if responseStateSessionDir == dir {
		return nil
	}
	if err := syncResponseStateDirectoryForWrite(dir); err != nil {
		durabilityErr := &responseStateDurabilityError{err: fmt.Errorf("establish Responses state session for %s: %w", dir, err)}
		return poisonResponseState(durabilityErr)
	}
	responseStateSessionDir = dir
	return nil
}

func responsesDir() string {
	return filepath.Join(config.GetConfigDir(), responsesDirName)
}

func responseInstanceNamespacePath() string {
	return filepath.Join(responsesDir(), responseInstanceNamespaceFile)
}

func validateResponseInstanceNamespace(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != responseInstanceNamespaceSize*2 {
		return "", fmt.Errorf("invalid Responses instance namespace length")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != responseInstanceNamespaceSize {
		return "", fmt.Errorf("invalid Responses instance namespace")
	}
	return value, nil
}

func readResponseInstanceNamespace(path string) (string, error) {
	if err := ensureResponseStateOwnerPrivateForRead(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "", poisonResponseState(fmt.Errorf("secure Responses instance namespace: %w", err))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value, validateErr := validateResponseInstanceNamespace(string(data))
	if validateErr != nil {
		return "", fmt.Errorf("%w at %s: %v", errResponseInstanceNamespaceCorrupt, path, validateErr)
	}
	return value, nil
}

func writeImmutableResponseState(path string, data []byte) (responseStateCommitState, error) {
	if err := responseStateHealthError(); err != nil {
		return responseStateNotPublished, err
	}

	dir, base := filepath.Dir(path), filepath.Base(path)
	tmp, err := privatefile.CreateTemp(dir, base, 0o600)
	if err != nil {
		return responseStateNotPublished, fmt.Errorf("create Responses state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	removeTempOnReturn := true
	defer func() {
		_ = tmp.Close()
		if removeTempOnReturn {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return responseStateNotPublished, fmt.Errorf("write Responses state temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return responseStateNotPublished, fmt.Errorf("sync Responses state temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return responseStateNotPublished, fmt.Errorf("close Responses state temp file: %w", err)
	}
	// Another concurrent state operation may have discovered an indeterminate
	// durability failure while this temporary payload was being prepared. Never
	// publish new state after the fail-stop latch is raised.
	if err := responseStateHealthError(); err != nil {
		return responseStateNotPublished, err
	}

	published, publishErr := publishResponseStateNoReplaceForWrite(tmpPath, path)
	if !published {
		if publishErr == nil {
			publishErr = errors.New("Responses state publication returned without publishing destination")
		}
		return responseStateNotPublished, publishErr
	}
	// A successful Unix link either removed the temporary name or returned a
	// cleanup-only error. Do not retry removal after the durability barrier: that
	// would create a new unsynced directory mutation. Stale sources are cleaned
	// later by the scoped startup sweeper.
	removeTempOnReturn = false

	if err := syncResponseStateDirectoryForWrite(dir); err != nil {
		durabilityErr := &responseStateDurabilityError{err: err}
		return responseStateDurabilityUnknown, poisonResponseState(durabilityErr)
	}
	if publishErr != nil {
		logger.Warnf("[Responses] published %s but could not remove its temporary source: %v", filepath.Base(path), publishErr)
	}
	// A different concurrent commit can poison the store after our own directory
	// barrier succeeds. The payload is durable, but this process must still fail
	// closed rather than acknowledge completion while the shared state subsystem
	// is unhealthy.
	if err := responseStateHealthError(); err != nil {
		return responseStatePublishedDurable, err
	}
	return responseStatePublishedDurable, nil
}

func ensureResponsesDir() error {
	if err := responseStateHealthError(); err != nil {
		return err
	}
	dir := responsesDir()
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("responses state path is not a directory: %s", dir)
		}
		return establishResponseStateSession(dir)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat responses dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create responses dir: %w", err)
	}
	if err := syncResponseStateDirectoryForWrite(filepath.Dir(dir)); err != nil {
		durabilityErr := &responseStateDurabilityError{err: fmt.Errorf("sync responses parent directory: %w", err)}
		return poisonResponseState(durabilityErr)
	}
	return establishResponseStateSession(dir)
}

func responseInstanceNamespace() (string, error) {
	if err := ensureResponsesDir(); err != nil {
		return "", err
	}
	return responseInstanceNamespaceAt(responseInstanceNamespacePath())
}

func responseInstanceNamespaceAt(path string) (string, error) {
	if err := responseStateHealthError(); err != nil {
		return "", err
	}
	if err := establishResponseStateSession(filepath.Dir(path)); err != nil {
		return "", err
	}
	if value, err := readResponseInstanceNamespace(path); err == nil {
		if healthErr := responseStateHealthError(); healthErr != nil {
			return "", healthErr
		}
		return value, nil
	} else if errors.Is(err, errResponseInstanceNamespaceCorrupt) {
		// Final-path publication happens only after the complete payload is
		// synced, so malformed final metadata is stable corruption, not an
		// in-progress writer. Never regenerate it because that can split ID
		// ownership from already persisted response documents.
		return "", err
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	buf := make([]byte, responseInstanceNamespaceSize)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate Responses instance namespace: %w", err)
	}
	value := hex.EncodeToString(buf)
	commitState, err := writeImmutableResponseState(path, []byte(value+"\n"))
	if err == nil && commitState == responseStatePublishedDurable {
		return value, nil
	}
	if commitState == responseStateNotPublished && errors.Is(err, os.ErrExist) {
		// Another process atomically published a complete namespace first.
		if healthErr := responseStateHealthError(); healthErr != nil {
			return "", healthErr
		}
		return readResponseInstanceNamespace(path)
	}
	return "", fmt.Errorf("persist Responses instance namespace: %w", err)
}

func generateResponseID() (string, error) {
	namespace, err := responseInstanceNamespace()
	if err != nil {
		return "", err
	}
	return generateResponseIDForNamespace(namespace), nil
}

func generateResponseIDForNamespace(namespace string) string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s%s_%d%012x", localResponseIDPrefix, namespace, time.Now().UnixNano(), 0)
	}
	return localResponseIDPrefix + namespace + "_" + hex.EncodeToString(buf) + fmt.Sprintf("%08x", time.Now().Unix()&0xffffffff)
}

func responseIDInstanceNamespace(id string) (string, bool) {
	if !strings.HasPrefix(id, localResponseIDPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(id, localResponseIDPrefix)
	separator := strings.IndexByte(rest, '_')
	if separator <= 0 || separator == len(rest)-1 {
		return "", false
	}
	namespace, err := validateResponseInstanceNamespace(rest[:separator])
	if err != nil {
		return "", false
	}
	return namespace, true
}

func generateOutputItemID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

// responseStatePath hashes the exact response ID instead of normalizing it.
// The response document still stores the exact ID and load verifies it, so the
// filesystem representation can never become the logical identity.
func responseStatePath(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(responsesDir(), responseStateFilePrefix+hex.EncodeToString(sum[:])+".json")
}

func legacyResponseStatePath(id string) string {
	// Legacy local IDs generated by Kiro-Go are short. Do not let an arbitrary
	// external previous_response_id turn a best-effort migration lookup into an
	// OS path-length error that would be misclassified as local-store failure.
	const maxLegacyResponseIDFilenameLen = 160
	name := sanitizeResponseID(id)
	if len(name) == 0 || len(name) > maxLegacyResponseIDFilenameLen {
		return ""
	}
	return filepath.Join(responsesDir(), name+".json")
}

func storedResponseDocument(resp *ResponsesObject) storedResponseDoc {
	return storedResponseDoc{
		StateVersion:       storedResponseStateVersion,
		ID:                 resp.ID,
		Object:             resp.Object,
		CreatedAt:          resp.CreatedAt,
		Status:             resp.Status,
		Model:              resp.Model,
		Output:             resp.Output,
		Usage:              resp.Usage,
		ToolChoice:         append(json.RawMessage(nil), resp.ToolChoice...),
		Tools:              append(json.RawMessage(nil), resp.Tools...),
		ToolCatalog:        append(json.RawMessage(nil), resp.StoredToolCatalog...),
		ParallelToolCalls:  resp.ParallelToolCalls,
		PreviousResponseID: resp.PreviousResponseID,
		Metadata:           resp.Metadata,
		Instructions:       resp.Instructions,
		StoredInput:        resp.StoredInput,
		StoredAt:           resp.StoredAt,
	}
}

func writeStoredResponseDoc(path string, doc storedResponseDoc) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal stored response: %w", err)
	}
	commitState, err := writeImmutableResponseState(path, data)
	if err != nil {
		return fmt.Errorf("commit stored response: %w", err)
	}
	if commitState != responseStatePublishedDurable {
		return fmt.Errorf("commit stored response returned non-durable state %d", commitState)
	}
	return nil
}

func saveResponse(resp *ResponsesObject) error {
	if resp == nil || resp.ID == "" {
		return fmt.Errorf("response missing id")
	}
	if err := ensureResponsesDir(); err != nil {
		return err
	}
	if resp.StoredAt == 0 {
		resp.StoredAt = time.Now().Unix()
	}
	return writeStoredResponseDoc(responseStatePath(resp.ID), storedResponseDocument(resp))
}

func validateStoredResponseDoc(doc *storedResponseDoc, expectedID string) error {
	if doc == nil {
		return fmt.Errorf("stored response document is nil")
	}
	if doc.StateVersion < 0 || doc.StateVersion > storedResponseStateVersion {
		return fmt.Errorf("%w: got=%d max=%d", errStoredResponseVersionUnsupported, doc.StateVersion, storedResponseStateVersion)
	}
	if doc.ID != expectedID {
		return fmt.Errorf("%w: requested=%q stored=%q", errStoredResponseIdentityMismatch, expectedID, doc.ID)
	}
	if doc.StateVersion == 0 {
		return nil
	}
	if doc.StateVersion == 1 {
		return validateStoredResponseDocV1(doc)
	}
	return validateStoredResponseDocV2(doc)
}

func validateStoredResponseDocV1(doc *storedResponseDoc) error {
	if strings.TrimSpace(doc.ID) == "" {
		return fmt.Errorf("version 1 state has an empty response id")
	}
	if doc.Object != "response" {
		return fmt.Errorf("version 1 state object=%q, want response", doc.Object)
	}
	if doc.Status != "completed" {
		return fmt.Errorf("version 1 state status=%q, want completed", doc.Status)
	}
	if strings.TrimSpace(doc.Model) == "" {
		return fmt.Errorf("version 1 state has an empty model")
	}
	if doc.CreatedAt <= 0 {
		return fmt.Errorf("version 1 state has invalid created_at=%d", doc.CreatedAt)
	}
	if doc.StoredAt <= 0 {
		return fmt.Errorf("version 1 state has invalid stored_at=%d", doc.StoredAt)
	}
	if len(doc.Output) == 0 {
		return fmt.Errorf("version 1 state has no output items")
	}
	if _, err := parseStoredResponsesInput(doc.StoredInput); err != nil {
		return fmt.Errorf("version 1 state has invalid stored input: %w", err)
	}
	catalog, err := responsesToolCatalogFromStoredRaw(doc.Tools, doc.Output, 1)
	if err != nil {
		return fmt.Errorf("version 1 state has invalid stored tools: %w", err)
	}

	itemIDs := make(map[string]struct{}, len(doc.Output))
	callIDs := make(map[string]struct{})
	for index, item := range doc.Output {
		itemPath := fmt.Sprintf("output[%d]", index)
		id := strings.TrimSpace(item.ID)
		if id == "" {
			return fmt.Errorf("version 1 %s has an empty id", itemPath)
		}
		if _, duplicate := itemIDs[id]; duplicate {
			return fmt.Errorf("version 1 state has duplicate output id %q", id)
		}
		itemIDs[id] = struct{}{}
		if item.Status != "completed" {
			return fmt.Errorf("version 1 %s status=%q, want completed", itemPath, item.Status)
		}

		switch item.Type {
		case "message":
			if item.Role != "assistant" {
				return fmt.Errorf("version 1 %s role=%q, want assistant", itemPath, item.Role)
			}
			if len(item.Content) == 0 {
				return fmt.Errorf("version 1 %s has no content", itemPath)
			}
			if len(item.Summary) != 0 || item.CallID != "" || item.Name != "" || item.Arguments != "" {
				return fmt.Errorf("version 1 %s contains fields not emitted for message output", itemPath)
			}
			for contentIndex, part := range item.Content {
				if part.Type != "output_text" {
					return fmt.Errorf("version 1 %s.content[%d] type=%q, want output_text", itemPath, contentIndex, part.Type)
				}
			}

		case "reasoning":
			if item.Role != "" || item.CallID != "" || item.Name != "" || item.Arguments != "" {
				return fmt.Errorf("version 1 %s contains fields not emitted for reasoning output", itemPath)
			}
			for contentIndex, part := range item.Content {
				if part.Type != "reasoning_text" {
					return fmt.Errorf("version 1 %s.content[%d] type=%q, want reasoning_text", itemPath, contentIndex, part.Type)
				}
			}
			for summaryIndex, part := range item.Summary {
				if part.Type != "summary_text" {
					return fmt.Errorf("version 1 %s.summary[%d] type=%q, want summary_text", itemPath, summaryIndex, part.Type)
				}
			}

		case "function_call":
			if item.Role != "" || len(item.Content) != 0 || len(item.Summary) != 0 {
				return fmt.Errorf("version 1 %s contains fields not emitted for function_call output", itemPath)
			}
			callID := strings.TrimSpace(item.CallID)
			if callID == "" {
				return fmt.Errorf("version 1 %s has an empty call_id", itemPath)
			}
			if _, duplicate := callIDs[callID]; duplicate {
				return fmt.Errorf("version 1 state has duplicate call_id %q", callID)
			}
			callIDs[callID] = struct{}{}
			name := strings.TrimSpace(item.Name)
			if name == "" {
				return fmt.Errorf("version 1 %s has an empty function name", itemPath)
			}
			if _, ok := catalog.OriginalToKiro[name]; !ok {
				if _, alreadyCanonical := catalog.KiroToOriginal[name]; !alreadyCanonical {
					return fmt.Errorf("version 1 %s function name %q has no matching persisted tool declaration", itemPath, name)
				}
			}
			var args interface{}
			if strings.TrimSpace(item.Arguments) == "" || json.Unmarshal([]byte(item.Arguments), &args) != nil {
				return fmt.Errorf("version 1 %s arguments must be valid JSON", itemPath)
			}
			if _, ok := args.(map[string]interface{}); !ok {
				return fmt.Errorf("version 1 %s arguments must encode a JSON object", itemPath)
			}

		default:
			return fmt.Errorf("version 1 %s has unsupported output type %q", itemPath, item.Type)
		}
	}
	return nil
}

func validateStoredResponseDocV2(doc *storedResponseDoc) error {
	if strings.TrimSpace(doc.ID) == "" {
		return fmt.Errorf("version 2 state has an empty response id")
	}
	if doc.Object != "response" || doc.Status != "completed" || strings.TrimSpace(doc.Model) == "" || doc.CreatedAt <= 0 || doc.StoredAt <= 0 {
		return fmt.Errorf("version 2 state has invalid response metadata")
	}
	if len(doc.Output) == 0 {
		return fmt.Errorf("version 2 state has no output items")
	}
	if _, err := parseStoredResponsesInput(doc.StoredInput); err != nil {
		return fmt.Errorf("version 2 state has invalid stored input: %w", err)
	}
	catalog, err := responsesToolCatalogFromStoredRaw(doc.ToolCatalog, doc.Output, 2)
	if err != nil {
		return fmt.Errorf("version 2 state has invalid unified tool catalog: %w", err)
	}
	itemIDs := make(map[string]struct{}, len(doc.Output))
	callIDs := make(map[string]struct{})
	for index, item := range doc.Output {
		path := fmt.Sprintf("output[%d]", index)
		if strings.TrimSpace(item.ID) == "" || item.Status != "completed" {
			return fmt.Errorf("version 2 %s has invalid id/status", path)
		}
		if _, duplicate := itemIDs[item.ID]; duplicate {
			return fmt.Errorf("version 2 state has duplicate output id %q", item.ID)
		}
		itemIDs[item.ID] = struct{}{}
		if item.Type == "function_call" || item.Type == "custom_tool_call" {
			if strings.TrimSpace(item.CallID) == "" || strings.TrimSpace(item.Name) == "" {
				return fmt.Errorf("version 2 %s requires call_id and name", path)
			}
			if _, duplicate := callIDs[item.CallID]; duplicate {
				return fmt.Errorf("version 2 state has duplicate call_id %q", item.CallID)
			}
			callIDs[item.CallID] = struct{}{}
		}
	}
	if _, err := projectStoredResponsesOutputTurnVersion(doc.Output, catalog, nil, "$.output", 2); err != nil {
		return fmt.Errorf("version 2 state has invalid stored output: %w", err)
	}
	return nil
}

func readStoredResponseDoc(path, expectedID string) (*storedResponseDoc, error) {
	if err := responseStateHealthError(); err != nil {
		return nil, err
	}
	if err := ensureResponseStateOwnerPrivateForRead(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errStoredResponseNotFound
		}
		return nil, poisonResponseState(fmt.Errorf("secure stored response %s: %w", filepath.Base(path), err))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errStoredResponseNotFound
		}
		return nil, fmt.Errorf("read stored response: %w", err)
	}
	if err := establishResponseStateSession(filepath.Dir(path)); err != nil {
		return nil, err
	}
	var doc storedResponseDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode stored response: %w", err)
	}
	if err := validateStoredResponseDoc(&doc, expectedID); err != nil {
		return nil, err
	}
	if doc.StoredAt > 0 && time.Since(time.Unix(doc.StoredAt, 0)) > responsesDefaultTTL {
		if removeErr := os.Remove(path); removeErr == nil {
			if syncErr := syncResponseStateDirectoryForWrite(filepath.Dir(path)); syncErr != nil {
				durabilityErr := &responseStateDurabilityError{err: fmt.Errorf("sync Responses directory after expiring %s: %w", filepath.Base(path), syncErr)}
				return nil, poisonResponseState(durabilityErr)
			}
		}
		return nil, errStoredResponseNotFound
	}
	if err := responseStateHealthError(); err != nil {
		return nil, err
	}
	return &doc, nil
}

func responseObjectFromStoredDoc(doc *storedResponseDoc) *ResponsesObject {
	return &ResponsesObject{
		ID:                 doc.ID,
		Object:             doc.Object,
		CreatedAt:          doc.CreatedAt,
		Status:             doc.Status,
		Model:              doc.Model,
		Output:             doc.Output,
		Usage:              doc.Usage,
		ToolChoice:         append(json.RawMessage(nil), doc.ToolChoice...),
		Tools:              append(json.RawMessage(nil), doc.Tools...),
		StoredToolCatalog:  append(json.RawMessage(nil), doc.ToolCatalog...),
		ParallelToolCalls:  doc.ParallelToolCalls,
		PreviousResponseID: doc.PreviousResponseID,
		Metadata:           doc.Metadata,
		Instructions:       doc.Instructions,
		StoredInput:        doc.StoredInput,
		StoredAt:           doc.StoredAt,
		StoredStateVersion: doc.StateVersion,
	}
}

func loadResponse(id string) (*ResponsesObject, error) {
	if id == "" {
		return nil, fmt.Errorf("empty response id")
	}

	doc, err := readStoredResponseDoc(responseStatePath(id), id)
	if err == nil {
		return responseObjectFromStoredDoc(doc), nil
	}
	if !errors.Is(err, errStoredResponseNotFound) {
		return nil, err
	}

	// Backward compatibility for response files written before exact-ID hashing.
	// A sanitized-path hit is accepted only when the stored document contains the
	// exact requested ID. This closes legacy filename collisions while allowing
	// old local conversations to continue and migrate on first successful read.
	legacyPath := legacyResponseStatePath(id)
	if legacyPath == "" {
		return nil, errStoredResponseNotFound
	}
	legacyDoc, legacyErr := readStoredResponseDoc(legacyPath, id)
	if legacyErr != nil {
		if errors.Is(legacyErr, errStoredResponseIdentityMismatch) {
			return nil, errStoredResponseNotFound
		}
		return nil, legacyErr
	}

	if mkErr := ensureResponsesDir(); mkErr == nil {
		if writeErr := writeStoredResponseDoc(responseStatePath(id), *legacyDoc); writeErr != nil && !errors.Is(writeErr, os.ErrExist) {
			if errors.Is(writeErr, errResponseStateUnavailable) {
				return nil, writeErr
			}
			logger.Warnf("[Responses] migrate legacy state %s failed: %v", id, writeErr)
		}
	} else if errors.Is(mkErr, errResponseStateUnavailable) {
		return nil, mkErr
	}
	if healthErr := responseStateHealthError(); healthErr != nil {
		return nil, healthErr
	}
	return responseObjectFromStoredDoc(legacyDoc), nil
}

type responseStateOwnership uint8

const (
	responseStateNoPrevious responseStateOwnership = iota
	responseStateLocal
	responseStateExternal
	responseStateLocalMissing
	responseStateUnavailable
)

func resolveResponseStateOwnership(id string) (responseStateOwnership, error) {
	return resolveResponseStateOwnershipByClass(id, classifyResponseID(id))
}

func isResponseStateTempName(name string) bool {
	namespacePrefix := "." + responseInstanceNamespaceFile + ".tmp-"
	if strings.HasPrefix(name, namespacePrefix) && len(name) > len(namespacePrefix) {
		return true
	}

	const responseHashHexLen = sha256.Size * 2
	prefix := "." + responseStateFilePrefix
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	rest := strings.TrimPrefix(name, prefix)
	separator := strings.Index(rest, ".json.tmp-")
	if separator != responseHashHexLen || len(rest) <= separator+len(".json.tmp-") {
		return false
	}
	_, err := hex.DecodeString(rest[:separator])
	return err == nil
}

func purgeExpiredResponses(ttl time.Duration) {
	if err := responseStateHealthError(); err != nil {
		return
	}
	if ttl <= 0 {
		ttl = responsesDefaultTTL
	}
	dir := responsesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	if err := establishResponseStateSession(dir); err != nil {
		return
	}
	now := time.Now()
	responseCutoff := now.Add(-ttl)
	tempCutoff := now.Add(-responsesTempCleanupGrace)
	removed := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		info, err := entry.Info()
		if err != nil {
			continue
		}

		remove := false
		switch {
		case isResponseStateTempName(name):
			remove = info.ModTime().Before(tempCutoff)
		case strings.HasSuffix(name, ".json"):
			remove = info.ModTime().Before(responseCutoff)
		default:
			continue
		}
		if !remove {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			logger.Warnf("[Responses] purge %s failed: %v", name, err)
			continue
		}
		removed = true
	}
	if removed {
		if err := syncResponseStateDirectoryForWrite(dir); err != nil {
			durabilityErr := &responseStateDurabilityError{err: fmt.Errorf("sync Responses directory after purge: %w", err)}
			logger.Warnf("[Responses] state store entered fail-stop mode after purge durability failure: %v", poisonResponseState(durabilityErr))
		}
	}
}

func logResponsesPersistFailure(id string, err error) {
	logger.Warnf("[Responses] persist %s failed: %v", id, err)
}

func persistResponsesCompletion(resp *ResponsesObject, store bool) error {
	if !store {
		return nil
	}
	if err := saveResponseForCompletion(resp); err != nil {
		id := ""
		if resp != nil {
			id = resp.ID
		}
		logResponsesPersistFailure(id, err)
		return err
	}
	return nil
}

// sanitizeResponseID remains only for exact-verified legacy migration. New
// writes and primary reads use responseStatePath and therefore never use this
// normalization as identity.
func sanitizeResponseID(id string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '_' || r == '-':
			return r
		default:
			return -1
		}
	}, id)
	if cleaned == "" {
		return "invalid"
	}
	return cleaned
}

type storedResponseDoc struct {
	StateVersion       int                  `json:"state_version,omitempty"`
	ID                 string               `json:"id"`
	Object             string               `json:"object"`
	CreatedAt          int64                `json:"created_at"`
	Status             string               `json:"status"`
	Model              string               `json:"model"`
	Output             []ResponseOutputItem `json:"output"`
	Usage              ResponsesUsage       `json:"usage"`
	ToolChoice         json.RawMessage      `json:"tool_choice,omitempty"`
	Tools              json.RawMessage      `json:"tools,omitempty"`
	ToolCatalog        json.RawMessage      `json:"tool_catalog,omitempty"`
	ParallelToolCalls  bool                 `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID string               `json:"previous_response_id,omitempty"`
	Metadata           map[string]string    `json:"metadata,omitempty"`
	Instructions       string               `json:"instructions,omitempty"`
	StoredInput        json.RawMessage      `json:"stored_input,omitempty"`
	StoredAt           int64                `json:"stored_at"`
}
