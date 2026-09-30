package pluginkit

import (
	"context"
	"fmt"
	"time"
)

// QuotaDescribeResponse mirrors pluginapi.QuotaDescribeResponse. The host uses
// SupportedProviders to attach this quota provider to the auth records it
// manages, so the list must contain the provider keys of the credentials this
// plugin owns.
type QuotaDescribeResponse struct {
	SupportedProviders []string `json:"supported_providers,omitempty"`
	DisplayName        string   `json:"display_name,omitempty"`
	SupportsReset      bool     `json:"supports_reset,omitempty"`
}

// QuotaDescribeRequest mirrors pluginapi.QuotaDescribeRequest.
type QuotaDescribeRequest struct {
	Plugin Metadata `json:"plugin,omitempty"`
}

// QuotaSubscription mirrors pluginapi.QuotaSubscription.
type QuotaSubscription struct {
	Plan     string `json:"plan,omitempty"`
	TierName string `json:"tierName,omitempty"`
	TierID   string `json:"tierId,omitempty"`
}

// QuotaGroup mirrors pluginapi.QuotaGroup.
type QuotaGroup struct {
	DisplayName string        `json:"displayName,omitempty"`
	Buckets     []QuotaBucket `json:"buckets,omitempty"`
}

// QuotaBucket mirrors pluginapi.QuotaBucket. RemainingFraction is a 0..1
// fraction, not a percentage; the management UI renders it as a progress bar.
type QuotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

// QuotaMetric mirrors pluginapi.QuotaMetric.
type QuotaMetric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Unit     string  `json:"unit,omitempty"`
	Format   string  `json:"format,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

// QuotaFetchResponse mirrors pluginapi.QuotaFetchResponse.
type QuotaFetchResponse struct {
	Subscription       *QuotaSubscription `json:"subscription,omitempty"`
	Summary            []QuotaMetric      `json:"summary,omitempty"`
	ServerTimeOffsetMs int64              `json:"serverTimeOffsetMs,omitempty"`
	Groups             []QuotaGroup       `json:"groups,omitempty"`
}

// QuotaFetchRequest mirrors pluginapi.QuotaFetchRequest. StorageJSON carries the
// credential blob the plugin itself persisted when the auth record was created.
type QuotaFetchRequest struct {
	AuthIndex   string            `json:"auth_index"`
	AuthID      string            `json:"auth_id"`
	Provider    string            `json:"provider"`
	StorageJSON []byte            `json:"storage_json,omitempty"`
	Metadata    map[string]any    `json:"metadata,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
	Host        HostConfigSummary `json:"host,omitempty"`
}

// QuotaResetRequest mirrors pluginapi.QuotaResetRequest.
type QuotaResetRequest struct {
	AuthIndex   string            `json:"auth_index"`
	AuthID      string            `json:"auth_id"`
	Provider    string            `json:"provider"`
	StorageJSON []byte            `json:"storage_json,omitempty"`
	Metadata    map[string]any    `json:"metadata,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
	Host        HostConfigSummary `json:"host,omitempty"`
}

// QuotaResetResponse mirrors pluginapi.QuotaResetResponse.
type QuotaResetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// QuotaProvider is implemented by plugins that surface credential quota.
type QuotaProvider interface {
	// QuotaIdentifier is the provider key this quota provider serves.
	QuotaIdentifier() string
	// DescribeQuota declares the provider keys this plugin owns.
	DescribeQuota() QuotaDescribeResponse
	// FetchQuota returns normalised quota for one credential.
	FetchQuota(ctx context.Context, req QuotaFetchRequest) (QuotaFetchResponse, error)
	// ResetQuota optionally resets quota; returning ErrUnhandled is fine.
	ResetQuota(ctx context.Context, req QuotaResetRequest) (QuotaResetResponse, error)
}

// DescribeQuotaRequestKey documents that describe takes no useful input today.
var _ = QuotaDescribeRequest{}

// FractionFromPercent converts a provider percentage into the 0..1 fraction the
// host expects. Providers commonly report "used" percentages; pass usedPercent
// and this returns the remaining fraction.
func FractionFromPercent(usedPercent float64) float64 {
	remaining := 1 - usedPercent/100
	switch {
	case remaining < 0:
		return 0
	case remaining > 1:
		return 1
	}
	return remaining
}

// FractionFromUsedRatio converts a used/total pair into a remaining fraction.
func FractionFromUsedRatio(used, total float64) float64 {
	if total <= 0 {
		return 0
	}
	remaining := 1 - used/total
	switch {
	case remaining < 0:
		return 0
	case remaining > 1:
		return 1
	}
	return remaining
}

// FormatResetTime renders a reset timestamp the way the management UI expects:
// RFC3339 in UTC.
func FormatResetTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// FormatResetTimeUnix renders a reset timestamp from Unix seconds.
func FormatResetTimeUnix(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return FormatResetTime(time.Unix(seconds, 0))
}

// Metric builds a summary metric.
func Metric(key, label string, value float64, unit string) QuotaMetric {
	return QuotaMetric{Key: key, Label: label, Value: value, Unit: unit, Format: "number"}
}

// StatusError maps an upstream HTTP status onto a plugin error that preserves
// its meaning for the client instead of collapsing to a generic 500.
func StatusError(status int, context string, body []byte) *PluginError {
	code := "upstream_error"
	switch {
	case status == 401 || status == 403:
		code = "invalid_credential"
	case status == 429:
		code = "rate_limited"
	case status >= 500:
		code = "upstream_unavailable"
	case status == 404:
		code = "not_found"
	}
	return NewError(code, fmt.Sprintf("%s: upstream returned HTTP %d (%s): %s", context, status, httpStatusText(status), truncate(body, 300)), status)
}
