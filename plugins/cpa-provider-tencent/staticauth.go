package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staticAuthDocument is a desktop sign-in exported from a machine that runs the
// WorkBuddy app, for use on a machine that does not.
//
// The desktop kinds normally read the app's own sealed sign-in file, which
// exists only where the app is installed. A Linux server has no such file, so it
// uses this document instead. The token is stored in the clear, so the file must
// be owner-readable only.
type staticAuthDocument struct {
	Kind         string `json:"kind"`
	Mode         string `json:"mode,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	UID          string `json:"uid,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Domain       string `json:"domain,omitempty"`
	// ExpiresAt is the access-token expiry in Unix milliseconds.
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// AppVersion and CLIVersion are the claims the desktop app sends in its
	// request headers. A server cannot read them from an installed bundle, so the
	// export carries them.
	AppVersion string `json:"app_version,omitempty"`
	CLIVersion string `json:"cli_version,omitempty"`
	// SourceMachine records where the export came from, for diagnostics only.
	SourceMachine string `json:"source_machine,omitempty"`
}

// credentialDir is the directory that holds this plugin's own credential
// documents. Configuration wins, so a server can keep them on a mounted volume:
// only mounted paths survive a container recreation.
func (p *pluginState) credentialDir() string {
	if trimmed := strings.TrimSpace(p.credentialDirPath); trimmed != "" {
		return trimmed
	}
	return filepath.Dir(authFilePath())
}

// credentialFilePath resolves one document inside credentialDir.
func (p *pluginState) credentialFilePath(name string) string {
	return filepath.Join(p.credentialDir(), name)
}

// staticAuthPath is where one kind's exported credential lives.
func (p *pluginState) staticAuthPath(kind *credentialKind) string {
	return p.credentialFilePath("static-" + kind.id + ".json")
}

// loadStaticCredentials reads an exported desktop credential for one kind.
func (p *pluginState) loadStaticCredentials(kind *credentialKind) (*credentials, error) {
	path := p.staticAuthPath(kind)
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, fmt.Errorf("read %s: %w", path, errRead)
	}
	var doc staticAuthDocument
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		return nil, fmt.Errorf("decode %s: %w", path, errUnmarshal)
	}
	if strings.TrimSpace(doc.AccessToken) == "" {
		return nil, fmt.Errorf("%s carries no access token", path)
	}
	if doc.Kind != "" && doc.Kind != kind.id {
		return nil, fmt.Errorf("%s holds a %s credential but %s was requested", path, doc.Kind, kind.displayName)
	}
	expires := time.Time{}
	if doc.ExpiresAt > 0 {
		expires = time.UnixMilli(doc.ExpiresAt)
	}
	return &credentials{
		Kind:         kind.id,
		Region:       kind.region,
		UID:          doc.UID,
		EnterpriseID: doc.EnterpriseID,
		Domain:       doc.Domain,
		AccessToken:  doc.AccessToken,
		RefreshToken: doc.RefreshToken,
		ExpiresAt:    expires,
		SourcePath:   path,
		AppVersion:   doc.AppVersion,
		CLIVersion:   doc.CLIVersion,
		Mode:         "static",
	}, nil
}
