package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// storedAuth is this plugin's own credential document for the codebuddy kind.
//
// The field names mirror the gateway's token payload (accessToken,
// refreshToken, domain) so the file stays readable next to a captured response.
type storedAuth struct {
	Mode             string `json:"mode,omitempty"`
	AccessToken      string `json:"accessToken,omitempty"`
	ExpiresAt        string `json:"expiresAt,omitempty"`
	RefreshToken     string `json:"refreshToken,omitempty"`
	RefreshExpiresAt string `json:"refreshExpiresAt,omitempty"`
	Domain           string `json:"domain,omitempty"`
	UID              string `json:"uid,omitempty"`
	Nickname         string `json:"nickname,omitempty"`
	EnterpriseID     string `json:"enterpriseId,omitempty"`
	EnterpriseName   string `json:"enterpriseName,omitempty"`
	UpdatedAt        string `json:"updatedAt,omitempty"`
}

// apply folds one token response into the document. The gateway names the
// lifetimes expiresIn / refreshExpiresIn, in seconds: there is no
// refreshExpiresAt field on the wire, and reading one silently loses the
// refresh window.
func (s *storedAuth) apply(data *oauthTokenData, now time.Time) {
	if data == nil {
		return
	}
	if data.AccessToken != "" {
		s.AccessToken = data.AccessToken
	}
	if data.ExpiresIn > 0 {
		s.ExpiresAt = formatTimestamp(now.Add(time.Duration(data.ExpiresIn) * time.Second))
	}
	if data.RefreshToken != "" {
		s.RefreshToken = data.RefreshToken
	}
	if data.RefreshExpiresIn > 0 {
		s.RefreshExpiresAt = formatTimestamp(now.Add(time.Duration(data.RefreshExpiresIn) * time.Second))
	}
	if data.Domain != "" {
		s.Domain = data.Domain
	}
	s.Mode = string(modeOAuth)
	s.UpdatedAt = formatTimestamp(now)
}

// readStoredAuth loads the plugin's own OAuth document.
func readStoredAuth() (storedAuth, error) {
	var doc storedAuth
	raw, errRead := os.ReadFile(authFilePath())
	if errRead != nil {
		return doc, errRead
	}
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		return doc, errUnmarshal
	}
	return doc, nil
}

// saveStoredAuth persists the OAuth document with owner-only permissions.
func saveStoredAuth(doc storedAuth) error {
	path := authFilePath()
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return errMkdir
	}
	encoded, errMarshal := json.MarshalIndent(doc, "", "  ")
	if errMarshal != nil {
		return errMarshal
	}
	if errWrite := os.WriteFile(path, append(encoded, '\n'), 0o600); errWrite != nil {
		return errWrite
	}
	return os.Chmod(path, 0o600)
}

// credential is one candidate credential for a codebuddy request.
type credential struct {
	Mode         authMode
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Domain       string
	UID          string
	Nickname     string
	EnterpriseID string
	APIKey       string
	Source       string
}

// secret is the bearer token sent to the gateway.
func (c credential) secret() string {
	if c.Mode == modeAPIKey {
		return c.APIKey
	}
	return c.AccessToken
}

// formatTimestamp renders a timestamp for the stored document.
func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseTimestamp reads a stored timestamp; an unparseable value is reported as
// the zero time, which callers treat as "unknown".
func parseTimestamp(raw string) time.Time {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}
	}
	parsed, errParse := time.Parse(time.RFC3339, trimmed)
	if errParse != nil {
		return time.Time{}
	}
	return parsed
}

// expiredAt reports whether the access token is at or past its renewal margin.
func (s *storedAuth) expiredAt(now time.Time) bool {
	if strings.TrimSpace(s.AccessToken) == "" {
		return true
	}
	expiry := parseTimestamp(s.ExpiresAt)
	if expiry.IsZero() {
		return false
	}
	return now.Add(refreshMargin).After(expiry)
}

// accessExpiry parses the access-token expiry; the zero time means "unknown".
func (s *storedAuth) accessExpiry() time.Time { return parseTimestamp(s.ExpiresAt) }

// truncateBody bounds an upstream body used inside an error message.
func truncateBody(body []byte) string {
	const limit = 300
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "..."
}
