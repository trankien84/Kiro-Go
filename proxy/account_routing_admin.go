package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"strings"
)

type accountRoutingConfigResponse struct {
	Success             bool     `json:"success"`
	Strategy            string   `json:"strategy"`
	SupportedStrategies []string `json:"supportedStrategies"`
	DefaultPriority     int      `json:"defaultPriority"`
	Published           bool     `json:"published,omitempty"`
	Error               string   `json:"error,omitempty"`
}

func currentAccountRoutingConfigResponse() accountRoutingConfigResponse {
	return accountRoutingConfigResponse{
		Success:         true,
		Strategy:        config.GetAccountSelectionStrategy(),
		DefaultPriority: config.DefaultAccountPriority,
		SupportedStrategies: []string{
			config.AccountSelectionStrategyRoundRobin,
			config.AccountSelectionStrategyPriority,
			config.AccountSelectionStrategyFillFirst,
		},
	}
}

const maxAccountRoutingRequestBodyBytes int64 = 1 << 20

func decodeAccountRoutingJSON(w http.ResponseWriter, r *http.Request, target interface{}) error {
	if r == nil || r.Body == nil {
		return fmt.Errorf("request body is required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAccountRoutingRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

func writeAccountRoutingJSONDecodeError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		writeAccountRoutingError(w, http.StatusRequestEntityTooLarge, "Request body exceeds 1 MiB limit", false)
		return
	}
	writeAccountRoutingError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error(), false)
}

func writeAccountRoutingError(w http.ResponseWriter, status int, message string, published bool) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   false,
		"published": published,
		"error":     message,
	})
}

func (h *Handler) apiGetAccountRouting(w http.ResponseWriter, _ *http.Request) {
	json.NewEncoder(w).Encode(currentAccountRoutingConfigResponse())
}

func (h *Handler) apiUpdateAccountRouting(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ObservedStrategy *string `json:"observedStrategy"`
		Strategy         *string `json:"strategy"`
	}
	if err := decodeAccountRoutingJSON(w, r, &request); err != nil {
		writeAccountRoutingJSONDecodeError(w, err)
		return
	}
	if request.ObservedStrategy == nil || strings.TrimSpace(*request.ObservedStrategy) == "" {
		writeAccountRoutingError(w, http.StatusBadRequest, "observedStrategy is required", false)
		return
	}
	if request.Strategy == nil || strings.TrimSpace(*request.Strategy) == "" {
		writeAccountRoutingError(w, http.StatusBadRequest, "strategy is required", false)
		return
	}
	if _, err := config.NormalizeAccountSelectionStrategy(*request.ObservedStrategy); err != nil {
		writeAccountRoutingError(w, http.StatusBadRequest, "invalid observed strategy: "+err.Error(), false)
		return
	}
	if _, err := config.NormalizeAccountSelectionStrategy(*request.Strategy); err != nil {
		writeAccountRoutingError(w, http.StatusBadRequest, err.Error(), false)
		return
	}

	published, superseded, err := config.UpdateAccountSelectionStrategyIfObserved(*request.ObservedStrategy, *request.Strategy)
	if superseded {
		writeAccountRoutingError(w, http.StatusConflict, "Account routing strategy changed in another session; reload and retry", false)
		return
	}
	if err != nil {
		writeAccountRoutingError(w, http.StatusInternalServerError, err.Error(), published)
		return
	}
	response := currentAccountRoutingConfigResponse()
	response.Published = published
	json.NewEncoder(w).Encode(response)
}

func (h *Handler) apiUpdateAccountRoutingPriority(w http.ResponseWriter, r *http.Request, id string) {
	if strings.TrimSpace(id) == "" {
		writeAccountRoutingError(w, http.StatusBadRequest, "Account id is required", false)
		return
	}
	var request struct {
		Generation       uint64 `json:"generation"`
		ObservedPriority *int   `json:"observedPriority"`
		Priority         *int   `json:"priority"`
	}
	if err := decodeAccountRoutingJSON(w, r, &request); err != nil {
		writeAccountRoutingJSONDecodeError(w, err)
		return
	}
	if request.Generation == 0 {
		writeAccountRoutingError(w, http.StatusBadRequest, "generation must be greater than zero", false)
		return
	}
	if request.ObservedPriority == nil {
		writeAccountRoutingError(w, http.StatusBadRequest, "observedPriority is required", false)
		return
	}
	if request.Priority == nil {
		writeAccountRoutingError(w, http.StatusBadRequest, "priority is required", false)
		return
	}
	if *request.Priority < 0 {
		writeAccountRoutingError(w, http.StatusBadRequest, "priority must be zero or a positive integer", false)
		return
	}

	after, published, superseded, err := config.UpdateAccountPriorityIfObserved(
		id,
		request.Generation,
		*request.ObservedPriority,
		*request.Priority,
	)
	if published && h.pool != nil {
		h.pool.Reload()
	}
	if superseded {
		writeAccountRoutingError(w, http.StatusConflict, "Account routing priority changed while this edit was pending; reload and retry", false)
		return
	}
	if err != nil {
		writeAccountRoutingError(w, http.StatusInternalServerError, err.Error(), published)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"published":         published,
		"id":                after.ID,
		"generation":        after.DispatchGeneration,
		"priority":          after.Priority,
		"effectivePriority": config.EffectiveAccountPriority(after.Priority),
	})
}

func validateAccountRoutingObservationPayload(items []config.AccountRoutingObservation) error {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.ID) == "" || item.Generation == 0 {
			return fmt.Errorf("each account needs id and generation")
		}
		if _, exists := seen[item.ID]; exists {
			return fmt.Errorf("account order contains duplicate id %s", item.ID)
		}
		seen[item.ID] = struct{}{}
	}
	return nil
}

func (h *Handler) apiReorderAccounts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ObservedStrategy *string                            `json:"observedStrategy"`
		Observed         []config.AccountRoutingObservation `json:"observed"`
		Accounts         []config.AccountRoutingObservation `json:"accounts"`
	}
	if err := decodeAccountRoutingJSON(w, r, &request); err != nil {
		writeAccountRoutingJSONDecodeError(w, err)
		return
	}
	if request.ObservedStrategy == nil || strings.TrimSpace(*request.ObservedStrategy) == "" {
		writeAccountRoutingError(w, http.StatusBadRequest, "observedStrategy is required", false)
		return
	}
	strategy, err := config.NormalizeAccountSelectionStrategy(*request.ObservedStrategy)
	if err != nil {
		writeAccountRoutingError(w, http.StatusBadRequest, "invalid observed strategy: "+err.Error(), false)
		return
	}
	if strategy != config.AccountSelectionStrategyFillFirst {
		writeAccountRoutingError(w, http.StatusBadRequest, "account order can only be changed while using fill-first", false)
		return
	}
	if request.Observed == nil || request.Accounts == nil {
		writeAccountRoutingError(w, http.StatusBadRequest, "observed and accounts are required", false)
		return
	}
	if err := validateAccountRoutingObservationPayload(request.Observed); err != nil {
		writeAccountRoutingError(w, http.StatusBadRequest, err.Error(), false)
		return
	}
	if err := validateAccountRoutingObservationPayload(request.Accounts); err != nil {
		writeAccountRoutingError(w, http.StatusBadRequest, err.Error(), false)
		return
	}
	if len(request.Observed) != len(request.Accounts) {
		writeAccountRoutingError(w, http.StatusBadRequest, "observed and desired account orders must have the same length", false)
		return
	}
	desiredByID := make(map[string]config.AccountRoutingObservation, len(request.Accounts))
	for _, item := range request.Accounts {
		desiredByID[item.ID] = item
	}
	for _, item := range request.Observed {
		candidate, ok := desiredByID[item.ID]
		if !ok || candidate.Generation != item.Generation || candidate.Priority != item.Priority {
			writeAccountRoutingError(w, http.StatusBadRequest, "desired account order must contain the same observed routing state", false)
			return
		}
	}

	published, superseded, err := config.ReorderAccountsIfObserved(*request.ObservedStrategy, request.Observed, request.Accounts)
	if published && h.pool != nil {
		h.pool.Reload()
	}
	if superseded {
		writeAccountRoutingError(w, http.StatusConflict, "Account routing order, strategy, or priority changed while this edit was pending; reload and retry", false)
		return
	}
	if err != nil {
		writeAccountRoutingError(w, http.StatusInternalServerError, err.Error(), published)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"published": published,
	})
}
