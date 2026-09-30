package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// ParseAuth implements pluginkit.AuthProvider. Every auth file whose type is
// the provider key is claimed; the kind inside decides which upstream it drives.
func (g *gateway) ParseAuth(_ context.Context, req pluginkit.AuthParseRequest) (pluginkit.AuthParseResponse, error) {
	if !strings.EqualFold(strings.TrimSpace(req.Provider), providerKey) {
		return pluginkit.AuthParseResponse{Handled: false}, nil
	}
	doc := parseStorage(req.RawJSON)
	kind := kindFor(doc.Kind)
	if doc.Kind != "" && kinds[doc.Kind] == nil {
		return pluginkit.AuthParseResponse{Handled: false}, nil
	}
	// A codebuddy record may carry its own PAT-less marker only; its token lives
	// in the plugin's OAuth document or an API key.
	storage := storageFor(kind)
	return pluginkit.AuthParseResponse{
		Handled: true,
		Auth: pluginkit.AuthData{
			Provider:    providerKey,
			ID:          providerKey + "-" + kind.id,
			FileName:    req.FileName,
			Label:       kind.displayName,
			StorageJSON: storage,
			Attributes:  map[string]string{"kind": kind.id, "region": kind.region},
		},
	}, nil
}

// StartLogin implements pluginkit.AuthProvider. Only the codebuddy kind has a
// browser flow: the desktop kinds are authenticated by their own apps.
func (g *gateway) StartLogin(ctx context.Context, req pluginkit.AuthLoginStartRequest) (pluginkit.AuthLoginStartResponse, error) {
	if !strings.EqualFold(strings.TrimSpace(req.Provider), providerKey) {
		return pluginkit.AuthLoginStartResponse{}, pluginkit.ErrUnhandled
	}
	kindID, _ := req.Metadata["kind"].(string)
	if kindID == "" || kindID == "codebuddy" {
		return g.startCodeBuddyLogin(ctx, req)
	}
	kind := kindFor(kindID)
	return pluginkit.AuthLoginStartResponse{
		Provider: providerKey,
		State:    "desktop-" + kind.id,
		Metadata: map[string]any{
			"message": "Sign in to the " + kind.displayName + " desktop app; this plugin reads that sign-in.",
		},
	}, nil
}

// PollLogin implements pluginkit.AuthProvider.
func (g *gateway) PollLogin(ctx context.Context, req pluginkit.AuthLoginPollRequest) (pluginkit.AuthLoginPollResponse, error) {
	if !strings.EqualFold(strings.TrimSpace(req.Provider), providerKey) {
		return pluginkit.AuthLoginPollResponse{}, pluginkit.ErrUnhandled
	}
	if strings.HasPrefix(req.State, "desktop-") {
		kindID := strings.TrimPrefix(req.State, "desktop-")
		kind := kindFor(kindID)
		creds, errCreds := g.impl.resolve(ctx, kind)
		if errCreds != nil {
			return pluginkit.AuthLoginPollResponse{Status: pluginkit.LoginPending, Message: errCreds.Error()}, nil
		}
		return pluginkit.AuthLoginPollResponse{
			Status: pluginkit.LoginSuccess,
			Auth: pluginkit.AuthData{
				Provider:    providerKey,
				ID:          providerKey + "-" + kind.id,
				FileName:    providerKey + "-" + kind.id + ".json",
				Label:       kind.displayName,
				StorageJSON: storageFor(kind),
				Metadata: map[string]any{
					"uid":       creds.UID,
					"expiresAt": creds.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
				},
			},
		}, nil
	}
	return g.pollCodeBuddyLogin(ctx, req)
}

// RefreshAuth implements pluginkit.AuthProvider.
func (g *gateway) RefreshAuth(ctx context.Context, req pluginkit.AuthRefreshRequest) (pluginkit.AuthRefreshResponse, error) {
	doc := parseStorage(req.StorageJSON)
	kind := kindFor(doc.Kind)
	creds, errCreds := g.impl.resolve(ctx, kind)
	if errCreds != nil {
		return pluginkit.AuthRefreshResponse{}, errCreds
	}
	return pluginkit.AuthRefreshResponse{
		Auth: pluginkit.AuthData{
			Provider:    providerKey,
			ID:          firstNonEmpty(req.AuthID, providerKey+"-"+kind.id),
			Label:       kind.displayName,
			StorageJSON: storageFor(kind),
		},
		NextRefreshAfter: creds.ExpiresAt,
	}, nil
}

// authDataFor is unused today but keeps the storage shape in one place.
func authDataFor(kind *credentialKind) ([]byte, error) { return storageFor(kind), nil }

// marshalKindJSON is a tiny indirection so tests can assert on the payload.
func marshalKindJSON(kind *credentialKind) string {
	encoded, _ := json.Marshal(map[string]string{"type": providerKey, "kind": kind.id})
	return string(encoded)
}
