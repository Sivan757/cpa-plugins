package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// billingRequest is the quota query body.
type billingRequest struct {
	PageNumber               int    `json:"PageNumber"`
	PageSize                 int    `json:"PageSize"`
	ProductCode              string `json:"ProductCode"`
	Status                   []int  `json:"Status"`
	PackageEndTimeRangeBegin string `json:"PackageEndTimeRangeBegin"`
	PackageEndTimeRangeEnd   string `json:"PackageEndTimeRangeEnd"`
}

// billingEnvelope is the nested envelope the quota endpoint returns.
type billingEnvelope struct {
	Code int           `json:"code"`
	Msg  string        `json:"msg"`
	Data *billingOuter `json:"data"`
}

type billingOuter struct {
	Response *billingResponse `json:"Response"`
}

type billingResponse struct {
	Data *billingData `json:"Data"`
}

type billingData struct {
	TotalCount  int              `json:"TotalCount"`
	TotalDosage float64          `json:"TotalDosage"`
	Accounts    []billingAccount `json:"Accounts"`
}

type billingAccount struct {
	PackageName         string  `json:"PackageName"`
	PackageCode         string  `json:"PackageCode"`
	ProductName         string  `json:"ProductName"`
	CycleStartTime      string  `json:"CycleStartTime"`
	CycleEndTime        string  `json:"CycleEndTime"`
	ExpiredTime         string  `json:"ExpiredTime"`
	CycleCapacitySize   float64 `json:"CycleCapacitySize"`
	CycleCapacityRemain float64 `json:"CycleCapacityRemain"`
	CycleCapacityUsed   float64 `json:"CycleCapacityUsed"`
	CapacitySize        float64 `json:"CapacitySize"`
	CapacityRemain      float64 `json:"CapacityRemain"`
	CapacityUsed        float64 `json:"CapacityUsed"`
	CapacityUnit        string  `json:"CapacityUnit"`
	Status              int     `json:"Status"`
}

// remaining reports the package's remaining credits using the app's rule:
// prefer the cycle counters, fall back to the lifetime counters.
func (a billingAccount) remaining() (float64, float64) {
	size := a.CycleCapacitySize
	remain := a.CycleCapacityRemain
	if size <= 0 {
		if a.CycleCapacityRemain <= 0 && a.CycleCapacityUsed <= 0 {
			remain = a.CapacityRemain
			size = a.CapacitySize
		}
	}
	if remain < 0 {
		remain = 0
	}
	return remain, size
}

// enterpriseUsage is the enterprise quota response. The two spellings are both
// accepted because the endpoint has been observed emitting each.
type enterpriseUsage struct {
	LimitNum       *float64 `json:"limitNum"`
	LimitNumAlt    *float64 `json:"limit_num"`
	Credit         *float64 `json:"credit"`
	UsedNum        *float64 `json:"used_num"`
	CycleResetTime string   `json:"cycleResetTime"`
}

// quotaFetch implements the plugin's quota capability for one credential.
func fetchQuota(ctx context.Context, v *credentialKind, creds *credentials) (pluginkit.QuotaFetchResponse, error) {
	if v == nil {
		return pluginkit.QuotaFetchResponse{}, pluginkit.NewError("invalid_credential", "unknown credential kind", 401)
	}
	if creds == nil {
		return pluginkit.QuotaFetchResponse{}, pluginkit.NewError("invalid_credential", "no credential was resolved", 401)
	}
	if v.region == "cn" && creds.isEnterprise() {
		return fetchEnterpriseQuota(ctx, v, creds)
	}
	return fetchPersonalQuota(ctx, v, creds)
}

func fetchPersonalQuota(ctx context.Context, v *credentialKind, creds *credentials) (pluginkit.QuotaFetchResponse, error) {
	now := time.Now()
	body := billingRequest{
		PageNumber:               1,
		PageSize:                 100,
		ProductCode:              "p_tcaca",
		Status:                   []int{0, 3},
		PackageEndTimeRangeBegin: now.Format("2006-01-02 15:04:05"),
		PackageEndTimeRangeEnd:   now.AddDate(0, 0, 365*101).Format("2006-01-02 15:04:05"),
	}
	headers := v.billingHeaders()
	var envelope billingEnvelope
	result, errCall := upstreamCall(ctx, v, creds, http.MethodPost, v.billingURL, headers, body, &envelope)
	if errCall != nil {
		return pluginkit.QuotaFetchResponse{}, errCall
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return pluginkit.QuotaFetchResponse{}, pluginkit.StatusError(result.StatusCode, "quota query", result.Body)
	}
	if envelope.Code != 0 {
		return pluginkit.QuotaFetchResponse{}, pluginkit.NewError("upstream_error", fmt.Sprintf("quota query returned code %d: %s", envelope.Code, envelope.Msg), http.StatusBadGateway)
	}
	var accounts []billingAccount
	if envelope.Data != nil && envelope.Data.Response != nil && envelope.Data.Response.Data != nil {
		accounts = envelope.Data.Response.Data.Accounts
	}

	var total float64
	buckets := make([]pluginkit.QuotaBucket, 0, len(accounts))
	for _, account := range accounts {
		remain, size := account.remaining()
		total += remain
		name := account.PackageName
		if name == "" {
			name = account.ProductName
		}
		bucket := pluginkit.QuotaBucket{
			Window:      name,
			ResetTime:   formatCycleEnd(account.CycleEndTime),
			Description: fmt.Sprintf("剩余 %.2f / %.2f credits", remain, size),
		}
		if size > 0 {
			bucket.RemainingFraction = clampFraction(remain / size)
		}
		buckets = append(buckets, bucket)
	}

	response := pluginkit.QuotaFetchResponse{
		Subscription: &pluginkit.QuotaSubscription{Plan: planLabel(accounts)},
		Summary: []pluginkit.QuotaMetric{
			pluginkit.Metric("remaining_credits", "剩余积分", total, "credits"),
			pluginkit.Metric("packages", "套餐数", float64(len(accounts)), ""),
		},
		Groups: []pluginkit.QuotaGroup{{
			DisplayName: v.displayName + " 积分",
			Buckets:     buckets,
		}},
	}
	return response, nil
}

func fetchEnterpriseQuota(ctx context.Context, v *credentialKind, creds *credentials) (pluginkit.QuotaFetchResponse, error) {
	url := "https://www.codebuddy.cn/v2/billing/meter/get-enterprise-user-usage"
	headers := v.billingHeaders()
	var raw json.RawMessage
	result, errCall := upstreamCall(ctx, v, creds, http.MethodPost, url, headers, map[string]any{}, &raw)
	if errCall != nil {
		return pluginkit.QuotaFetchResponse{}, errCall
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return pluginkit.QuotaFetchResponse{}, pluginkit.StatusError(result.StatusCode, "enterprise quota query", result.Body)
	}
	usage, errDecode := decodeEnterpriseUsage(result.Body)
	if errDecode != nil {
		return pluginkit.QuotaFetchResponse{}, errDecode
	}
	limit := firstPtr(usage.LimitNum, usage.LimitNumAlt)
	used := firstPtr(usage.Credit, usage.UsedNum)
	if limit == nil {
		return pluginkit.QuotaFetchResponse{}, pluginkit.NewError("upstream_error", "enterprise quota response carried no limit", http.StatusBadGateway)
	}
	if *limit < 0 {
		return pluginkit.QuotaFetchResponse{
			Subscription: &pluginkit.QuotaSubscription{Plan: "企业额度（不限量）"},
			Summary: []pluginkit.QuotaMetric{
				{Key: "unlimited", Label: "额度", Value: 1, Unit: "unlimited", Format: "number"},
			},
			Groups: []pluginkit.QuotaGroup{{
				DisplayName: "企业额度",
				Buckets: []pluginkit.QuotaBucket{{
					Window:            "enterprise",
					RemainingFraction: 1,
					ResetTime:         usage.CycleResetTime,
					Description:       "不限量",
				}},
			}},
		}, nil
	}
	if used == nil {
		// Reporting a full bar would claim headroom we cannot verify.
		return pluginkit.QuotaFetchResponse{}, pluginkit.NewError("upstream_error", "enterprise quota response carried a limit but no usage", http.StatusBadGateway)
	}
	remain := *limit - *used
	if remain < 0 {
		remain = 0
	}
	fraction := 0.0
	if *limit > 0 {
		fraction = clampFraction(remain / *limit)
	}
	return pluginkit.QuotaFetchResponse{
		Subscription: &pluginkit.QuotaSubscription{Plan: "企业额度"},
		Summary: []pluginkit.QuotaMetric{
			pluginkit.Metric("remaining_credits", "剩余额度", remain, "credits"),
			pluginkit.Metric("total_credits", "总额度", *limit, "credits"),
		},
		Groups: []pluginkit.QuotaGroup{{
			DisplayName: "企业额度",
			Buckets: []pluginkit.QuotaBucket{{
				Window:            "enterprise",
				RemainingFraction: fraction,
				ResetTime:         usage.CycleResetTime,
				Description:       fmt.Sprintf("剩余 %.2f / %.2f", remain, *limit),
			}},
		}},
	}, nil
}

// decodeEnterpriseUsage locates the usage fields across the three response
// shapes this endpoint has been observed to use.
func decodeEnterpriseUsage(raw []byte) (enterpriseUsage, error) {
	candidates := [][]byte{raw}
	var outer struct {
		Data json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &outer); errUnmarshal == nil && len(outer.Data) > 0 {
		candidates = append(candidates, outer.Data)
		var inner struct {
			Data json.RawMessage `json:"data"`
		}
		if errInner := json.Unmarshal(outer.Data, &inner); errInner == nil && len(inner.Data) > 0 {
			candidates = append(candidates, inner.Data)
		}
	}
	for _, candidate := range candidates {
		var usage enterpriseUsage
		if errUnmarshal := json.Unmarshal(candidate, &usage); errUnmarshal != nil {
			continue
		}
		if usage.LimitNum != nil || usage.LimitNumAlt != nil || usage.Credit != nil || usage.UsedNum != nil {
			return usage, nil
		}
	}
	return enterpriseUsage{}, pluginkit.NewError("upstream_error", "could not locate usage fields in the enterprise quota response", http.StatusBadGateway)
}

func planLabel(accounts []billingAccount) string {
	if len(accounts) == 0 {
		return ""
	}
	names := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if account.PackageName != "" && !containsStringFold(names, account.PackageName) {
			names = append(names, account.PackageName)
		}
	}
	if len(names) == 0 {
		return ""
	}
	label := names[0]
	if len(names) > 1 {
		label = fmt.Sprintf("%s 等 %d 个套餐", label, len(names))
	}
	return label
}

func firstPtr(values ...*float64) *float64 {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func clampFraction(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	}
	return value
}

// formatCycleEnd converts the upstream "YYYY-MM-DD HH:mm:ss" local timestamp
// into the RFC3339 UTC string the management UI expects.
func formatCycleEnd(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, errParse := time.ParseInLocation("2006-01-02 15:04:05", raw, time.Local)
	if errParse != nil {
		return ""
	}
	return pluginkit.FormatResetTime(parsed)
}
