package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"strings"
	"time"
)

// ensureReconcileProfileArn resolves the profile needed by background account
// info without ever invoking the OAuth refresh fallback in ResolveProfileArn.
// That keeps quota freshness and credential rotation as independent lanes while
// still allowing cancellable cross-region profile discovery.
func ensureReconcileProfileArn(ctx context.Context, account *config.Account) error {
	if account == nil || strings.TrimSpace(account.ProfileArn) != "" {
		return nil
	}
	if config.IsAPIKeyAccount(account) {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if isProfileArnResolutionSuppressed(account) {
		return nil
	}

	var probeErrors []error
	for _, region := range kiroProfileRegionCandidates(account) {
		profiles, err := listKiroProfilesWithRetryInRegionContext(ctx, account, region)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isBuilderIDProfileUnsupportedError(account, err) {
				// Match ensureRestProfileArn's soft-error behavior, but do not use
				// ResolveProfileArn's refresh-token fallback to obtain a profile ARN.
				suppressProfileArnResolution(account)
				return nil
			}
			probeErrors = append(probeErrors, fmt.Errorf("%s: %w", region, err))
			continue
		}
		if len(profiles) == 0 {
			continue
		}
		profileArn := profiles[0].ARN
		persistAccountProfileArnForRequest(account, profileArn)
		account.ProfileArn = profileArn
		return nil
	}
	if len(probeErrors) != 0 {
		return errors.Join(probeErrors...)
	}
	return fmt.Errorf("no available Kiro profile")
}

const (
	maxAccountInfoErrorBodyBytes   int64 = 64 << 10
	maxAccountInfoSuccessBodyBytes int64 = 1 << 20
)

func readBoundedAccountInfoErrorBody(body io.Reader) string {
	if body == nil {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(body, maxAccountInfoErrorBodyBytes+1))
	if int64(len(data)) <= maxAccountInfoErrorBodyBytes {
		return string(data)
	}
	return string(data[:maxAccountInfoErrorBodyBytes]) + " ... [truncated]"
}

func readBoundedAccountInfoSuccessBody(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, errors.New("account-info response body is nil")
	}
	data, err := io.ReadAll(io.LimitReader(body, maxAccountInfoSuccessBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxAccountInfoSuccessBodyBytes {
		return nil, fmt.Errorf("account-info response exceeds %d bytes", maxAccountInfoSuccessBodyBytes)
	}
	return data, nil
}

func getUsageLimitsForReconcileContext(ctx context.Context, account *config.Account) (*UsageLimitsResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ensureReconcileProfileArn(ctx, account); err != nil {
		return nil, fmt.Errorf("resolve profileArn: %w", err)
	}

	url := fmt.Sprintf("%s/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST&isEmailRequired=true", kiroRestAPIBase)
	url = regionalizeURL(url, account)
	url = withProfileArnQuery(url, account)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	setKiroHeaders(req, account)

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, readBoundedAccountInfoErrorBody(resp.Body))
	}

	body, err := readBoundedAccountInfoSuccessBody(resp.Body)
	if err != nil {
		return nil, err
	}
	var result UsageLimitsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// fetchAccountInfoForReconcileContext is intentionally side-effect-free with
// respect to account quota/health/credential state. The reconciler publishes the
// result only after checking that its credential/profile snapshot and monotonic
// account-info publication revision are still current. Profile-ARN caching is the
// one allowed auxiliary side effect;
// it is already lifecycle-fenced and does not rotate OAuth credentials.
func fetchAccountInfoForReconcileContext(ctx context.Context, account *config.Account) (*config.AccountInfo, error) {
	usage, err := getUsageLimitsForReconcileContext(ctx, account)
	if err != nil {
		return nil, err
	}

	info := &config.AccountInfo{LastRefresh: time.Now().Unix()}
	if usage.UserInfo != nil {
		info.Email = usage.UserInfo.Email
		info.UserId = usage.UserInfo.UserId
	}
	if usage.SubscriptionInfo != nil {
		titleOrName := usage.SubscriptionInfo.SubscriptionTitle
		if titleOrName == "" {
			titleOrName = usage.SubscriptionInfo.SubscriptionName
		}
		if titleOrName == "" {
			titleOrName = usage.SubscriptionInfo.SubscriptionType
		}
		info.SubscriptionType = parseSubscriptionType(titleOrName)
		info.SubscriptionTitle = usage.SubscriptionInfo.SubscriptionTitle
		if info.SubscriptionTitle == "" {
			info.SubscriptionTitle = usage.SubscriptionInfo.SubscriptionName
		}
	}
	if len(usage.UsageBreakdownList) > 0 {
		breakdown := usage.UsageBreakdownList[0]
		info.UsageCurrent = breakdown.CurrentUsage
		info.UsageLimit = breakdown.UsageLimit
		if info.UsageLimit > 0 {
			info.UsagePercent = info.UsageCurrent / info.UsageLimit
		}
		if breakdown.FreeTrialInfo != nil {
			info.TrialUsageCurrent = breakdown.FreeTrialInfo.CurrentUsage
			info.TrialUsageLimit = breakdown.FreeTrialInfo.UsageLimit
			if info.TrialUsageLimit > 0 {
				info.TrialUsagePercent = info.TrialUsageCurrent / info.TrialUsageLimit
			}
			info.TrialStatus = breakdown.FreeTrialInfo.FreeTrialStatus
			if breakdown.FreeTrialInfo.FreeTrialExpiry != "" {
				if ts, parseErr := breakdown.FreeTrialInfo.FreeTrialExpiry.Int64(); parseErr == nil && ts > 0 {
					info.TrialExpiresAt = ts
				} else if f, parseErr := breakdown.FreeTrialInfo.FreeTrialExpiry.Float64(); parseErr == nil && f > 0 {
					info.TrialExpiresAt = int64(f)
				}
			}
		}
	}
	if usage.NextDateReset != "" {
		if ts, parseErr := usage.NextDateReset.Int64(); parseErr == nil && ts > 0 {
			info.NextResetDate = time.Unix(ts, 0).Format("2006-01-02")
		} else if f, parseErr := usage.NextDateReset.Float64(); parseErr == nil && f > 0 {
			info.NextResetDate = time.Unix(int64(f), 0).Format("2006-01-02")
		}
	}
	return info, nil
}
