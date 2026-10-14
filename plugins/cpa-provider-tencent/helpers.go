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

// authFileExclusions groups the excluded_models lists of this plugin's gateway
// auth files by credential kind. The outer slice holds one entry per auth file.
//
// Grouping by kind is load-bearing. The gateway advertises a single roster for
// the whole plugin, so a model may only disappear when no kind still serves it.
// Merging the lists across kinds would instead drop glm-5.3-flash, which the
// international file excludes while the CN file serves it.
//
// The previous implementation read the desktop products' credential files
// (workbuddy-desktop*.info), which carry no excluded_models at all, so it
// always returned nothing and the static roster ignored every exclusion.
func authFileExclusions(authDir string) map[string][][]string {
	byKind := map[string][][]string{}
	dir := strings.TrimSpace(authDir)
	if dir == "" {
		return byKind
	}
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		return byKind
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		raw, errFile := os.ReadFile(filepath.Join(dir, entry.Name()))
		if errFile != nil {
			continue
		}
		var doc struct {
			Type           string   `json:"type"`
			Kind           string   `json:"kind"`
			ExcludedModels []string `json:"excluded_models"`
		}
		if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(doc.Type), providerKey) {
			continue
		}
		kindID := strings.TrimSpace(doc.Kind)
		if kindID == "" {
			continue
		}
		byKind[kindID] = append(byKind[kindID], doc.ExcludedModels)
	}
	return byKind
}
