package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// appVersionFromBundle reads the desktop app's bundle version. The user agent
// must carry a real version; a fabricated one is rejected by the gateway.
func appVersionFromBundle(v *credentialKind) string {
	binary := v.electronBinary()
	// .../WorkBuddy.app/Contents/MacOS/Electron -> .../WorkBuddy.app
	appDir := filepath.Dir(filepath.Dir(filepath.Dir(binary)))
	plist := filepath.Join(appDir, "Contents", "Info.plist")
	raw, errRead := os.ReadFile(plist)
	if errRead != nil {
		return ""
	}
	// A minimal plist scan avoids a dependency: find the short version key and
	// take the following <string> value.
	text := string(raw)
	marker := "<key>CFBundleShortVersionString</key>"
	index := strings.Index(text, marker)
	if index < 0 {
		return ""
	}
	rest := text[index+len(marker):]
	open := strings.Index(rest, "<string>")
	if open < 0 {
		return ""
	}
	rest = rest[open+len("<string>"):]
	close := strings.Index(rest, "</string>")
	if close < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:close])
}

// cliVersionFromBundle reads the bundled CLI package version, which the desktop
// user agent appends when present.
func cliVersionFromBundle(v *credentialKind) string {
	binary := v.electronBinary()
	appDir := filepath.Dir(filepath.Dir(filepath.Dir(binary)))
	candidates := []string{
		filepath.Join(appDir, "Contents", "Resources", "app.asar.unpacked", "cli", "package.json"),
		filepath.Join(appDir, "Contents", "Resources", "app", "cli", "package.json"),
	}
	for _, candidate := range candidates {
		raw, errRead := os.ReadFile(candidate)
		if errRead != nil {
			continue
		}
		var pkg struct {
			Version       string `json:"version"`
			PublishConfig struct {
				CustomPackage struct {
					Version string `json:"version"`
				} `json:"customPackage"`
			} `json:"publishConfig"`
		}
		if errUnmarshal := json.Unmarshal(raw, &pkg); errUnmarshal != nil {
			continue
		}
		if version := strings.TrimSpace(pkg.Version); version != "" && version != "0.0.0" {
			return version
		}
		if version := strings.TrimSpace(pkg.PublishConfig.CustomPackage.Version); version != "" {
			return version
		}
	}
	return ""
}
