package main

import (
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
