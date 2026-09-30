package pluginkit

import "strings"

// ExcludedModelsFor returns the exclusion list a provider should apply, taken
// from the host configuration summary. The host keys the list by provider;
// callers pass the same key they registered under.
func ExcludedModelsFor(host HostConfigSummary, provider string) []string {
	if len(host.ExcludedModels) == 0 {
		return nil
	}
	return host.ExcludedModels[strings.ToLower(strings.TrimSpace(provider))]
}

// RequestExcludedModels reads the per-account exclusion list the synthesizer
// pre-merges into the auth record's attributes.
func RequestExcludedModels(attributes map[string]string) []string {
	if value, ok := attributes["excluded_models"]; ok && strings.TrimSpace(value) != "" {
		return strings.Split(value, ",")
	}
	return nil
}

// FilterModelsForRequest applies every exclusion source relevant to one
// request: the global provider list from the host summary plus the per-account
// list from the auth attributes. Matching is case-insensitive; "*" hides
// everything and may appear as a wildcard inside a pattern.
func FilterModelsForRequest(models []ModelInfo, host HostConfigSummary, provider string, attributes map[string]string) []ModelInfo {
	excluded := ExcludedModelsFor(host, provider)
	excluded = append(excluded, RequestExcludedModels(attributes)...)
	return FilterModelsByExclusion(models, excluded)
}

// FilterModelsByExclusion drops models whose ID matches any exclusion entry.
func FilterModelsByExclusion(models []ModelInfo, excluded []string) []ModelInfo {
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
	out := make([]ModelInfo, 0, len(models))
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
