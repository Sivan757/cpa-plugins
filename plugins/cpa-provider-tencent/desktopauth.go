package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// desktopAuthDocument is the subset of the desktop app's auth file this plugin
// reads. The file is the single source of truth for who is signed in; the
// plugin never writes it back.
type desktopAuthDocument struct {
	Account desktopAccount `json:"account"`
	Auth    desktopAuth    `json:"auth"`
}

type desktopAccount struct {
	UID          string       `json:"uid"`
	Uin          json.Number  `json:"uin"`
	Type         string       `json:"type"`
	Nickname     wrappedField `json:"nickname"`
	EnterpriseID string       `json:"enterpriseId"`
	Domain       string       `json:"domain"`
	SSO          struct {
		Domain string `json:"domain"`
	} `json:"sso"`
}

type desktopAuth struct {
	AccessToken  wrappedField `json:"accessToken"`
	RefreshToken wrappedField `json:"refreshToken"`
	TokenType    string       `json:"tokenType"`
	Domain       string       `json:"domain"`
	ExpiresAt    int64        `json:"expiresAt"`
}

// desktopAuthDir is the shared directory both WorkBuddy products write to.
func desktopAuthDir() string {
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "CodeBuddyExtension", "Data", "Public", "auth")
}

// loadDesktopCredentials reads and decrypts one product's desktop sign-in.
func loadDesktopCredentials(kind *credentialKind, keys *keyProvider) (*credentials, error) {
	path := filepath.Join(desktopAuthDir(), kind.authFilename)
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, fmt.Errorf("read %s: %w", path, errRead)
	}
	var doc desktopAuthDocument
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		return nil, fmt.Errorf("decode %s: %w", path, errUnmarshal)
	}
	accessToken, errAccess := keys.open(doc.Auth.AccessToken)
	if errAccess != nil {
		return nil, fmt.Errorf("open access token in %s: %w", path, errAccess)
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("%s carries no access token; sign in to %s first", path, kind.displayName)
	}
	refreshToken, errRefresh := keys.open(doc.Auth.RefreshToken)
	if errRefresh != nil {
		// A missing refresh token is not fatal: the cached access token still
		// works until it expires.
		refreshToken = ""
	}
	return &credentials{
		Kind:         kind.id,
		Region:       kind.region,
		UID:          doc.Account.UID,
		EnterpriseID: firstNonEmpty(doc.Account.EnterpriseID, ""),
		Domain:       firstNonEmpty(doc.Auth.Domain, doc.Account.Domain, doc.Account.SSO.Domain),
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    time.UnixMilli(doc.Auth.ExpiresAt),
		SourcePath:   path,
		Mode:         "desktop",
	}, nil
}
