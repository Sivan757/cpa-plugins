package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// rawQuota is one quota block. Every numeric field is optional: the upstream
// omits whichever one it considers implied, and the same block has been seen
// carrying "total", "cap" or only used+remaining.
type rawQuota struct {
	Total      *float64 `json:"total"`
	Cap        *float64 `json:"cap"`
	Used       *float64 `json:"used"`
	Remaining  *float64 `json:"remaining"`
	Percentage *float64 `json:"percentage"`
	Unit       string   `json:"unit"`
	Available  *bool    `json:"available"`
}

type rawDisplayLabel struct {
	Dimension string            `json:"dimension"`
	Value     string            `json:"value"`
	ValueI18n map[string]string `json:"valueI18n"`
	ValueAlt  map[string]string `json:"value_i18n"`
}

type rawResourcePackage struct {
	rawQuota
	ID            string            `json:"id"`
	DisplayLabels []rawDisplayLabel `json:"displayLabels"`
	DisplayAlt    []rawDisplayLabel `json:"display_labels"`
	ExpiresAt     json.RawMessage   `json:"expiresAt"`
	Status        string            `json:"status"`
}

// rawUsage is the quota document.
//
// addOnQuota is the field that actually moves for a personal subscription: the
// plan block stays at zero, so a reader that only looks at userQuota reports
// full headroom for an account that is nearly exhausted.
type rawUsage struct {
	UserID               string               `json:"userId"`
	UserType             string               `json:"userType"`
	UsageType            string               `json:"usageType"`
	TotalUsagePercentage *float64             `json:"totalUsagePercentage"`
	IsQuotaExceeded      *bool                `json:"isQuotaExceeded"`
	ExpiresAt            json.RawMessage      `json:"expiresAt"`
	UserQuota            *rawQuota            `json:"userQuota"`
	AddOnQuota           *rawQuota            `json:"addOnQuota"`
	OrgResourcePackage   *rawQuota            `json:"orgResourcePackage"`
	DedicatedPackages    []rawResourcePackage `json:"dedicatedResourcePackages"`
}

type rawPlan struct {
	UserType       string          `json:"user_type"`
	UserTypeCamel  string          `json:"userType"`
	PlanTierName   string          `json:"plan_tier_name"`
	PlanTierCamel  string          `json:"planTierName"`
	PlanName       string          `json:"plan_name"`
	PlanTier       string          `json:"plan_tier"`
	IsPersonal     *bool           `json:"is_personal_version"`
	EndDate        json.RawMessage `json:"end_date"`
	StartDate      json.RawMessage `json:"start_date"`
	FeatureAllowed json.RawMessage `json:"feature_allowed"`
}

type rawStatus struct {
	UserType    string           `json:"userType"`
	Plan        string           `json:"plan"`
	UserTag     string           `json:"userTag"`
	Quota       *float64         `json:"quota"`
	NextResetAt json.RawMessage  `json:"nextResetAt"`
	FeatureSw   rawFeatureSwitch `json:"featureSwitches"`
}

type rawFeatureSwitch struct {
	AllowByok json.RawMessage `json:"allow_byok"`
}

// normalizedQuota is one quota block reduced to the four numbers that matter.
type normalizedQuota struct {
	Total     float64
	Used      float64
	Remaining float64
	Unit      string
}

// normalizeQuota applies the reader's arithmetic: the upstream may state the
// capacity, the cap, or only the two halves, and every spelling must produce the
// same three numbers.
func normalizeQuota(raw *rawQuota) (normalizedQuota, bool) {
	if raw == nil {
		return normalizedQuota{}, false
	}
	used := 0.0
	if raw.Used != nil {
		used = *raw.Used
	}
	remaining := 0.0
	hasRemaining := raw.Remaining != nil
	if hasRemaining {
		remaining = *raw.Remaining
	}
	total := 0.0
	switch {
	case raw.Total != nil:
		total = *raw.Total
	case raw.Cap != nil:
		total = *raw.Cap
	case raw.Remaining != nil && raw.Used != nil:
		total = used + remaining
	}
	if !hasRemaining {
		remaining = total - used
		if remaining < 0 {
			remaining = 0
		}
	}
	// A block with nothing in it would render an empty progress bar, which reads
	// as a failure rather than as "no such package".
	if total <= 0 && used <= 0 && remaining <= 0 {
		return normalizedQuota{}, false
	}
	return normalizedQuota{
		Total:     total,
		Used:      used,
		Remaining: remaining,
		Unit:      firstNonEmpty(raw.Unit, "credits"),
	}, true
}

// remainingFraction clamps the share the management UI renders as a bar.
func remainingFraction(quota normalizedQuota) float64 {
	if quota.Total <= 0 {
		return 0
	}
	return clampFraction(quota.Remaining / quota.Total)
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

// fetchQuota reads the account quota, the plan, and (best effort) the account
// status.
//
// Only the quota read is fatal. The plan and status calls enrich the answer, so
// a failure there degrades the panel instead of failing the request: the
// upstream has served each of them with a different field vocabulary per region.
func (p *pluginState) fetchQuota(ctx context.Context, storageJSON []byte) (pluginkit.QuotaFetchResponse, error) {
	creds, errCreds := p.resolveCredentials(ctx, storageJSON)
	if errCreds != nil {
		return pluginkit.QuotaFetchResponse{}, errCreds
	}
	token := creds.Identity.AuthToken

	var usage rawUsage
	result, errCall := openAPIJSON(ctx, creds.Region, http.MethodGet, "/api/v2/quota/usage", token, nil, &usage)
	if errCall != nil {
		return pluginkit.QuotaFetchResponse{}, errCall
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return pluginkit.QuotaFetchResponse{}, classifyOpenAPIError(result.StatusCode, "quota query", result.Body)
	}

	plan, planOK := p.readPlan(ctx, creds)
	status, statusOK := p.readStatus(ctx, creds)

	groups := buildQuotaGroups(creds.Region, &usage)
	summary := buildQuotaSummary(&usage)
	response := pluginkit.QuotaFetchResponse{
		Summary: summary,
		Groups:  groups,
	}
	if planOK || statusOK {
		response.Subscription = &pluginkit.QuotaSubscription{
			Plan: planLabel(plan, status, planOK, statusOK),
			TierName: firstNonEmpty(planValue(plan, planOK, func(value rawPlan) string {
				return firstNonEmpty(value.PlanTierName, value.PlanTierCamel, value.PlanName)
			}), planValue(plan, planOK, func(value rawPlan) string { return value.PlanTier })),
			TierID: planValue(plan, planOK, func(value rawPlan) string { return firstNonEmpty(value.PlanTier, value.PlanTierName) }),
		}
	}
	if reset := quotaResetTime(&usage, plan, planOK, status, statusOK); reset != "" {
		attachResetTime(response.Groups, reset)
	}
	return response, nil
}

func planValue(plan rawPlan, okPlan bool, pick func(rawPlan) string) string {
	if !okPlan {
		return ""
	}
	return strings.TrimSpace(pick(plan))
}

// planLabel prefers the plan document and falls back to the status tag, because
// the two disagree about which field carries the human-readable tier.
func planLabel(plan rawPlan, status rawStatus, okPlan, okStatus bool) string {
	if okPlan {
		if tier := firstNonEmpty(plan.PlanTierName, plan.PlanTierCamel, plan.PlanName); tier != "" {
			if userType := firstNonEmpty(plan.UserType, plan.UserTypeCamel); userType != "" && !strings.EqualFold(userType, tier) {
				return tier + " (" + userType + ")"
			}
			return tier
		}
	}
	if okStatus {
		return firstNonEmpty(status.UserTag, status.Plan, status.UserType)
	}
	return ""
}

// buildQuotaGroups renders one bucket per quota block the account exposes.
func buildQuotaGroups(reg *region, usage *rawUsage) []pluginkit.QuotaGroup {
	buckets := make([]pluginkit.QuotaBucket, 0, 4)
	appendBucket := func(label string, raw *rawQuota) {
		normalized, okQuota := normalizeQuota(raw)
		if !okQuota {
			return
		}
		buckets = append(buckets, pluginkit.QuotaBucket{
			Window:            label,
			RemainingFraction: remainingFraction(normalized),
			Description:       fmt.Sprintf("剩余 %g / %g %s", normalized.Remaining, normalized.Total, normalized.Unit),
		})
	}
	appendBucket("附加额度", usage.AddOnQuota)
	appendBucket("套餐额度", usage.UserQuota)
	appendBucket("组织资源包", usage.OrgResourcePackage)
	for index := range usage.DedicatedPackages {
		pkg := &usage.DedicatedPackages[index]
		label := resourcePackageLabel(pkg)
		normalized, okQuota := normalizeQuota(&pkg.rawQuota)
		if !okQuota {
			continue
		}
		bucket := pluginkit.QuotaBucket{
			Window:            label,
			RemainingFraction: remainingFraction(normalized),
			Description:       fmt.Sprintf("剩余 %g / %g %s", normalized.Remaining, normalized.Total, normalized.Unit),
		}
		if expiry, okExpiry := timestampValue(pkg.ExpiresAt); okExpiry {
			bucket.ResetTime = pluginkit.FormatResetTime(expiry)
		}
		buckets = append(buckets, bucket)
	}
	if len(buckets) == 0 {
		return nil
	}
	return []pluginkit.QuotaGroup{{DisplayName: reg.displayName + " 额度", Buckets: buckets}}
}

// resourcePackageLabel reads the subscriber-facing copy.
//
// The package's own name and description carry internal campaign identifiers, so
// only displayLabels is ever surfaced.
func resourcePackageLabel(pkg *rawResourcePackage) string {
	labels := pkg.DisplayLabels
	if len(labels) == 0 {
		labels = pkg.DisplayAlt
	}
	for _, label := range labels {
		if !strings.EqualFold(strings.TrimSpace(label.Dimension), "title") {
			continue
		}
		if text := localizedLabel(label); text != "" {
			return text
		}
	}
	return firstNonEmpty(pkg.ID, "资源包")
}

func localizedLabel(label rawDisplayLabel) string {
	for _, source := range []map[string]string{label.ValueI18n, label.ValueAlt} {
		if len(source) == 0 {
			continue
		}
		locales := make([]string, 0, len(source))
		for locale := range source {
			locales = append(locales, locale)
		}
		sort.Strings(locales)
		for _, locale := range locales {
			if text := strings.TrimSpace(source[locale]); text != "" {
				return text
			}
		}
	}
	return strings.TrimSpace(label.Value)
}

// buildQuotaSummary reports the totals a user checks first, summing the blocks
// so that addOnQuota is included rather than shadowed by the empty plan block.
func buildQuotaSummary(usage *rawUsage) []pluginkit.QuotaMetric {
	remaining := 0.0
	used := 0.0
	total := 0.0
	unit := "credits"
	blocks := []*rawQuota{usage.AddOnQuota, usage.UserQuota, usage.OrgResourcePackage}
	for index := range usage.DedicatedPackages {
		blocks = append(blocks, &usage.DedicatedPackages[index].rawQuota)
	}
	anyBlock := false
	for _, block := range blocks {
		normalized, okQuota := normalizeQuota(block)
		if !okQuota {
			continue
		}
		anyBlock = true
		remaining += normalized.Remaining
		used += normalized.Used
		total += normalized.Total
		unit = normalized.Unit
	}
	if !anyBlock {
		return nil
	}
	metrics := []pluginkit.QuotaMetric{
		pluginkit.Metric("remaining_credits", "剩余额度", remaining, unit),
		pluginkit.Metric("used_credits", "已用额度", used, unit),
		pluginkit.Metric("total_credits", "总额度", total, unit),
	}
	if usage.IsQuotaExceeded != nil {
		exceeded := 0.0
		if *usage.IsQuotaExceeded {
			exceeded = 1
		}
		metrics = append(metrics, pluginkit.Metric("quota_exceeded", "额度已用尽", exceeded, ""))
	}
	return metrics
}

// quotaResetTime picks the reset timestamp the panel shows.
//
// The usage document's expiresAt is a "never expires" sentinel, and the status
// document's nextResetAt is the only real cycle boundary, so the order matters.
func quotaResetTime(usage *rawUsage, plan rawPlan, okPlan bool, status rawStatus, okStatus bool) string {
	if okStatus {
		if reset, okReset := timestampValue(status.NextResetAt); okReset {
			return pluginkit.FormatResetTime(reset)
		}
	}
	if okPlan {
		if end, okEnd := timestampValue(plan.EndDate); okEnd {
			return pluginkit.FormatResetTime(end)
		}
	}
	return ""
}

func attachResetTime(groups []pluginkit.QuotaGroup, reset string) {
	for groupIndex := range groups {
		for bucketIndex := range groups[groupIndex].Buckets {
			if groups[groupIndex].Buckets[bucketIndex].ResetTime == "" {
				groups[groupIndex].Buckets[bucketIndex].ResetTime = reset
			}
		}
	}
}

// readPlan reads the subscription document, tolerating failure.
func (p *pluginState) readPlan(ctx context.Context, creds *credentials) (rawPlan, bool) {
	var plan rawPlan
	result, errCall := openAPIJSON(ctx, creds.Region, http.MethodGet, "/api/v2/user/plan", creds.Identity.AuthToken, nil, &plan)
	if errCall != nil || result.StatusCode < 200 || result.StatusCode >= 300 {
		return rawPlan{}, false
	}
	return plan, true
}

// readStatus reads the account status document, tolerating failure.
//
// This endpoint additionally expects the machine token, which is how the
// upstream binds the call to one installation.
func (p *pluginState) readStatus(ctx context.Context, creds *credentials) (rawStatus, bool) {
	headers := openAPIHeaders(creds.Identity.AuthToken)
	headers.Set("Cosy-MachineToken", creds.Identity.MachineID)
	headers.Set("Cosy-MachineType", "host")
	url := strings.TrimSuffix(creds.Region.openAPI, "/") + "/api/v3/user/status"
	var status rawStatus
	result, errCall := pluginkit.HTTPJSON(ctx, http.MethodGet, url, headers, nil, &status)
	if errCall != nil || result.StatusCode < 200 || result.StatusCode >= 300 {
		return rawStatus{}, false
	}
	return status, true
}

// timestampValue parses a timestamp that may be epoch milliseconds, epoch
// seconds or an ISO string, and rejects the "never expires" sentinel.
func timestampValue(raw json.RawMessage) (time.Time, bool) {
	parsed, okParse := parseLooseTimestamp(raw)
	if !okParse || !plausibleTimestamp(parsed) {
		return time.Time{}, false
	}
	return parsed, true
}
