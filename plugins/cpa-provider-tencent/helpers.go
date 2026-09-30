package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// envAPIKey is the API key source used when the config carries none.
const envAPIKey = "CODEBUDDY_API_KEY"

// defaultAuthDirName is where this plugin keeps its own OAuth document.
const defaultAuthDirName = ".dsh-cpa"

// defaultAuthFileName is the plugin's OAuth document basename.
const defaultAuthFileName = "cpa-provider-tencent-auth.json"

// authFilePath is the plugin's own OAuth credential document.
func authFilePath() string {
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		return defaultAuthFileName
	}
	return filepath.Join(home, defaultAuthDirName, defaultAuthFileName)
}

// getEnv reads an environment variable.
func getEnv(key string) string { return os.Getenv(key) }

// firstNonEmpty returns the first non-blank value.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// containsStringFold reports whether values contains target, ignoring case.
func containsStringFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

// extraDocumentFields returns the auth document's fields beyond the ones this
// plugin owns, so user-managed settings survive the host's metadata round trip.
func extraDocumentFields(raw []byte) map[string]any {
	var document map[string]any
	if errUnmarshal := json.Unmarshal(raw, &document); errUnmarshal != nil {
		return nil
	}
	for _, owned := range []string{"type", "kind"} {
		delete(document, owned)
	}
	if len(document) == 0 {
		return nil
	}
	return document
}

// mergedFileExclusions unions the excluded_models lists saved in every kind's
// auth file, so the credential-less static path still honors the user's
// visibility choices.
func mergedFileExclusions() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, kind := range kinds {
		raw, errRead := os.ReadFile(filepath.Join(desktopAuthDir(), kind.authFilename))
		if errRead != nil {
			continue
		}
		var doc struct {
			ExcludedModels []string `json:"excluded_models"`
		}
		if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
			continue
		}
		for _, id := range doc.ExcludedModels {
			if trimmed := strings.TrimSpace(id); trimmed != "" && !seen[trimmed] {
				seen[trimmed] = true
				out = append(out, trimmed)
			}
		}
	}
	return out
}
