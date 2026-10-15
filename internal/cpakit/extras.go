package cpakit

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// Lifecycle states a plugin-reported login can be in.
const (
	LoginPending = "pending"
	LoginSuccess = "success"
	LoginError   = "error"
)

// PluginError is the classified failure type that crosses the ABI.
type PluginError = Error

// BadRequestError builds a 400 failure.
func BadRequestError(message string) *Error {
	return &Error{Code: string(ClassUnsupported), Message: message, HTTPStatus: http.StatusBadRequest}
}

// NotFoundError builds a 404 failure.
func NotFoundError(what string) *Error {
	return &Error{Code: string(ClassInvalidModel), Message: what + " not found", HTTPStatus: http.StatusNotFound}
}

// StatusError maps an upstream HTTP status onto a plugin error that preserves
// its meaning for the client instead of collapsing to a generic 500.
func StatusError(status int, context string, body []byte) *Error {
	code := ClassUpstream
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		code = ClassAuth
	case status == http.StatusTooManyRequests:
		code = ClassRateLimit
	case status == http.StatusNotFound:
		code = ClassInvalidModel
	case status >= 500:
		code = ClassUpstream
	default:
		code = ClassUnsupported
	}
	message := fmt.Sprintf("%s: upstream returned HTTP %d: %s", context, status, TruncateBody(body, 300))
	return &Error{Code: string(code), Message: Redact(message), HTTPStatus: status, Retryable: status == 429 || status >= 500}
}

// JSONManagementResponse is a helper for management route handlers.
func JSONManagementResponse(status int, body []byte) (pluginapi.ManagementResponse, error) {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}, nil
}

// FractionFromPercent converts a provider "used" percentage into the 0..1
// remaining fraction the host expects.
func FractionFromPercent(usedPercent float64) float64 {
	return clampFraction(1 - usedPercent/100)
}

// FractionFromUsedRatio converts a used/total pair into a remaining fraction.
func FractionFromUsedRatio(used, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return clampFraction(1 - used/total)
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
func Metric(key, label string, value float64, unit string) pluginapi.QuotaMetric {
	return pluginapi.QuotaMetric{Key: key, Label: label, Value: value, Unit: unit, Format: "number"}
}

// Config is the plugin's own configuration block, decoded from the YAML the host
// sends on register and reconfigure.
//
// Every setting this project defines is a scalar or a list of scalars, so the
// decoded map is queried through the accessors below rather than bound to a
// struct per plugin.
type Config struct {
	values map[string]any
}

// ParseConfig decodes the plugin config block.
func ParseConfig(raw []byte) (*Config, error) {
	cfg := &Config{values: map[string]any{}}
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	if errUnmarshal := yaml.Unmarshal(raw, &cfg.values); errUnmarshal != nil {
		return nil, BadRequestError("decode plugin config: " + errUnmarshal.Error())
	}
	return cfg, nil
}

// NewConfigFromValues wraps an already-decoded map, for tests.
func NewConfigFromValues(values map[string]any) *Config {
	if values == nil {
		values = map[string]any{}
	}
	return &Config{values: values}
}

// String returns a scalar setting, or the fallback when unset or empty.
func (c *Config) String(key, fallback string) string {
	if c == nil {
		return fallback
	}
	switch typed := c.values[key].(type) {
	case string:
		if trimmed := strings.TrimSpace(typed); trimmed != "" {
			return trimmed
		}
	case nil:
	default:
		return fmt.Sprintf("%v", typed)
	}
	return fallback
}

// Int returns an integer setting, or the fallback when unset or unparsable.
func (c *Config) Int(key string, fallback int) int {
	if c == nil {
		return fallback
	}
	switch typed := c.values[key].(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		if parsed, errParse := strconv.Atoi(strings.TrimSpace(typed)); errParse == nil {
			return parsed
		}
	}
	return fallback
}

// Bool returns a boolean setting, or the fallback when unset or unparsable.
func (c *Config) Bool(key string, fallback bool) bool {
	if c == nil {
		return fallback
	}
	switch typed := c.values[key].(type) {
	case bool:
		return typed
	case string:
		if parsed, errParse := strconv.ParseBool(strings.TrimSpace(typed)); errParse == nil {
			return parsed
		}
	}
	return fallback
}

// Strings returns a list setting, splitting a comma-separated scalar as well so
// both YAML shapes work.
func (c *Config) Strings(key string) []string {
	if c == nil {
		return nil
	}
	switch typed := c.values[key].(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			if text := strings.TrimSpace(fmt.Sprintf("%v", entry)); text != "" {
				out = append(out, text)
			}
		}
		return out
	case string:
		var out []string
		for _, part := range strings.Split(typed, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	}
	return nil
}

// Value returns the raw decoded value for a key.
func (c *Config) Value(key string) any {
	if c == nil {
		return nil
	}
	return c.values[key]
}

// ---- host configuration helpers ----

// ExcludedModelsFor returns the exclusion list a provider should apply, keyed by
// provider in the host configuration summary.
func ExcludedModelsFor(host pluginapi.HostConfigSummary, provider string) []string {
	if len(host.ExcludedModels) == 0 {
		return nil
	}
	return host.ExcludedModels[strings.ToLower(strings.TrimSpace(provider))]
}

// RequestExcludedModels reads the per-account exclusion list the host
// synthesizer pre-merges into an auth record's attributes.
func RequestExcludedModels(attributes map[string]string) []string {
	if value, okValue := attributes["excluded_models"]; okValue && strings.TrimSpace(value) != "" {
		return strings.Split(value, ",")
	}
	return nil
}

// FilterModelsForRequest applies every exclusion source relevant to one request:
// the provider list from the host summary plus the per-account list.
func FilterModelsForRequest(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary, provider string, attributes map[string]string) []pluginapi.ModelInfo {
	excluded := ExcludedModelsFor(host, provider)
	excluded = append(excluded, RequestExcludedModels(attributes)...)
	return FilterModelsByExclusion(models, excluded)
}

// FilterModelsByExclusion drops models whose ID matches any exclusion entry.
func FilterModelsByExclusion(models []pluginapi.ModelInfo, excluded []string) []pluginapi.ModelInfo {
	if len(models) == 0 || len(excluded) == 0 {
		return models
	}
	patterns := make([]string, 0, len(excluded))
	for _, item := range excluded {
		for _, piece := range strings.Split(item, ",") {
			trimmed := strings.ToLower(strings.TrimSpace(piece))
			if trimmed != "" {
				patterns = append(patterns, trimmed)
			}
		}
	}
	if len(patterns) == 0 {
		return models
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		id := strings.ToLower(strings.TrimSpace(model.ID))
		blocked := false
		for _, pattern := range patterns {
			if pattern == "*" || pattern == id || matchesGlob(pattern, id) {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, model)
		}
	}
	return out
}

// matchesGlob supports "*" anywhere in the pattern.
func matchesGlob(pattern, value string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	cursor := 0
	for index, part := range parts {
		if part == "" {
			continue
		}
		if index == 0 {
			if !strings.HasPrefix(value[cursor:], part) {
				return false
			}
			cursor += len(part)
			continue
		}
		found := strings.Index(value[cursor:], part)
		if found < 0 {
			return false
		}
		cursor += found + len(part)
	}
	return cursor <= len(value)
}

// JoinURL joins an upstream base with a route path, tolerating a trailing slash
// on either side.
func JoinURL(base, path string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}

// ParseQuery decodes a query string for diagnostics.
func ParseQuery(raw string) url.Values {
	values, errParse := url.ParseQuery(raw)
	if errParse != nil {
		return url.Values{}
	}
	return values
}

// ExposeOnly narrows a model list to the ids matching any pattern.
//
// An empty pattern list means "no restriction" rather than "expose nothing",
// because an unset allowlist must not blank a provider's model picker. Matching
// reuses the exclusion glob so "*" and embedded wildcards behave identically.
func ExposeOnly(models []ModelInfo, patterns []string) []ModelInfo {
	if len(patterns) == 0 || len(models) == 0 {
		return models
	}
	wanted := make([]string, 0, len(patterns))
	for _, item := range patterns {
		for _, piece := range strings.Split(item, ",") {
			trimmed := strings.ToLower(strings.TrimSpace(piece))
			if trimmed != "" {
				wanted = append(wanted, trimmed)
			}
		}
	}
	if len(wanted) == 0 {
		return models
	}
	out := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		id := strings.ToLower(strings.TrimSpace(model.ID))
		for _, pattern := range wanted {
			if pattern == id || matchesGlob(pattern, id) {
				out = append(out, model)
				break
			}
		}
	}
	return out
}
