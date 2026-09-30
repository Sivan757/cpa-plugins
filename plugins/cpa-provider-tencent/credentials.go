package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// credentials is one usable credential regardless of kind. Every capability
// works on this shape, so the rest of the plugin is credential-agnostic.
type credentials struct {
	Kind           string
	Region         string
	UID            string
	Uin            string
	Scope          string
	Nickname       string
	EnterpriseID   string
	EnterpriseName string
	Domain         string
	AccessToken    string
	RefreshToken   string
	ExpiresAt      time.Time
	SourcePath     string
	AppVersion     string
	CLIVersion     string
	// APIKey is set for the codebuddy key mode instead of AccessToken.
	APIKey string
	// Mode records how this credential was resolved, for diagnostics.
	Mode string
}

// isEnterprise reports an enterprise identity, which selects the enterprise
// credit endpoint on the CN gateway.
func (c *credentials) isEnterprise() bool {
	return strings.TrimSpace(c.EnterpriseID) != ""
}

// chatURL is this credential's chat endpoint.
func (c *credentials) chatURL(kind *credentialKind) string { return kind.chatBase + pathChat }

// catalogURL is this credential's model catalog endpoint.
func (c *credentials) catalogURL(kind *credentialKind) string { return kind.chatBase + pathConfig }

// expired reports whether the access token is within the renewal margin.
func (c *credentials) expired(now time.Time) bool {
	if c.ExpiresAt.IsZero() {
		return false
	}
	return now.Add(5 * time.Minute).After(c.ExpiresAt)
}

// bearer is the token sent in the Authorization header.
func (c *credentials) bearer() string {
	if c.APIKey != "" {
		return c.APIKey
	}
	return c.AccessToken
}

// storageDocument is the auth-record payload. kind selects which upstream this
// record drives.
type storageDocument struct {
	Type string `json:"type"`
	Kind string `json:"kind,omitempty"`
}

// storageFor serialises one auth record.
func storageFor(kind *credentialKind) []byte {
	encoded, _ := json.Marshal(storageDocument{Type: providerKey, Kind: kind.id})
	return encoded
}

// parseStorage decodes an auth record's storage payload.
func parseStorage(raw []byte) storageDocument {
	var doc storageDocument
	_ = json.Unmarshal(raw, &doc)
	if doc.Kind == "" {
		doc.Kind = doc.Type
	}
	return doc
}

// kindForRecord resolves the credential kind of one host auth record from its
// storage blob and its attributes. The attributes win when both are present:
// the plugin writes "kind" there on every auth.parse answer, and attributes
// survive even when a host passes the storage blob through lossily.
func kindForRecord(storageJSON []byte, attributes map[string]string) *credentialKind {
	if id := attributes["kind"]; id != "" {
		if k, ok := kinds[id]; ok {
			return k
		}
	}
	return kindFor(parseStorage(storageJSON).Kind)
}

// resolve loads a usable credential for one kind.
//
// Desktop kinds read the app's own sign-in so a sign-out or account switch
// takes effect immediately; the codebuddy kind reads this plugin's OAuth
// document, or every API key from configuration or the environment.
func (p *pluginState) resolve(ctx context.Context, kind *credentialKind) (*credentials, error) {
	creds, _, errCreds := p.resolveAll(ctx, kind)
	return creds, errCreds
}

// resolveAll returns every candidate credential for one kind. Only the
// CodeBuddy kind can carry more than one: each configured API key is a
// candidate the executor rotates. Catalog and quota callers use resolve and
// get the first candidate, because those endpoints resolve the active key
// from the x-api-key header and a mixed answer would be misleading.
func (p *pluginState) resolveAll(ctx context.Context, kind *credentialKind) (*credentials, []*credentials, error) {
	if kind == nil {
		return nil, nil, pluginkit.NewError("invalid_credential", "unknown credential kind", http.StatusUnauthorized)
	}
	if kind.desktop {
		creds, errLoad := loadDesktopCredentials(kind, p.keysFor(kind))
		if errLoad != nil {
			return nil, nil, pluginkit.NewError("invalid_credential", errLoad.Error(), http.StatusUnauthorized)
		}
		creds.AppVersion = p.appVersion(kind)
		creds.CLIVersion = cliVersionFromBundle(kind)
		return creds, []*credentials{creds}, nil
	}
	return p.resolveCodeBuddyAll(ctx, kind)
}

// resolveCodeBuddyAll returns the plugin-managed candidates: a stored OAuth
// token first (refreshing it at the renewal margin), then every API key from
// configuration or the environment.
func (p *pluginState) resolveCodeBuddyAll(ctx context.Context, kind *credentialKind) (*credentials, []*credentials, error) {
	if stored, errRead := p.loadStoredAuth(); errRead == nil && strings.TrimSpace(stored.AccessToken) != "" {
		if stored.expiredAt(time.Now()) {
			// The token is at or past its refresh margin. Renewal is best
			// effort: a failed refresh still serves the stale token, because
			// the gateway, not the timestamp, is the authority.
			if errRefresh := p.refreshTokens(ctx, stored); errRefresh == nil {
				pluginkit.HostLog(ctx, "info", "tencent: refreshed the CodeBuddy access token")
			}
		}
		creds := &credentials{
			Kind:           kind.id,
			Region:         kind.region,
			UID:            stored.UID,
			Nickname:       stored.Nickname,
			EnterpriseID:   stored.EnterpriseID,
			EnterpriseName: stored.EnterpriseName,
			Domain:         stored.Domain,
			AccessToken:    stored.AccessToken,
			RefreshToken:   stored.RefreshToken,
			ExpiresAt:      stored.accessExpiry(),
			Mode:           string(modeOAuth),
		}
		return creds, []*credentials{creds}, nil
	}
	keys := splitAPIKeys(p.apiKey)
	if len(keys) == 0 {
		keys = splitAPIKeys(getEnv(envAPIKey))
	}
	if len(keys) == 0 {
		return nil, nil, pluginkit.NewError("invalid_credential",
			fmt.Sprintf("no %s credential: complete the browser login or set an API key (config api_key or %s)", kind.displayName, envAPIKey), http.StatusUnauthorized)
	}
	out := make([]*credentials, 0, len(keys))
	for _, key := range keys {
		out = append(out, &credentials{Kind: kind.id, Region: kind.region, APIKey: key, Mode: string(modeAPIKey)})
	}
	return out[0], out, nil
}

// loadStoredAuth reads the plugin's own OAuth document.
func (p *pluginState) loadStoredAuth() (*storedAuth, error) {
	path := authFilePath()
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, fmt.Errorf("read %s: %w", path, errRead)
	}
	var stored storedAuth
	if errUnmarshal := json.Unmarshal(raw, &stored); errUnmarshal != nil {
		return nil, fmt.Errorf("decode %s: %w", path, errUnmarshal)
	}
	return &stored, nil
}

// saveStoredAuth writes the OAuth document with owner-only permissions.
func (p *pluginState) saveStoredAuth(stored *storedAuth) error {
	path := authFilePath()
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), errMkdir)
	}
	encoded, errMarshal := json.MarshalIndent(stored, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("encode credential: %w", errMarshal)
	}
	if errWrite := os.WriteFile(path, append(encoded, '\n'), 0o600); errWrite != nil {
		return fmt.Errorf("write %s: %w", path, errWrite)
	}
	if errChmod := os.Chmod(path, 0o600); errChmod != nil {
		return fmt.Errorf("chmod %s: %w", path, errChmod)
	}
	return nil
}
