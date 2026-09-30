package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// ParseAuth claims an auth file that belongs to this provider.
//
// The file carries the personal access token itself, because a PAT is a static
// credential with no browser handshake to replay. It is stored back verbatim so
// the quota, catalog and executor calls can read it from the auth record.
func (g *gateway) ParseAuth(_ context.Context, req pluginkit.AuthParseRequest) (pluginkit.AuthParseResponse, error) {
	if !strings.EqualFold(strings.TrimSpace(req.Provider), providerKey) {
		return pluginkit.AuthParseResponse{Handled: false}, nil
	}
	parsed, errParse := parseCredentialDocument(req.RawJSON)
	if errParse != nil {
		return pluginkit.AuthParseResponse{}, errParse
	}
	if parsed.Type != "" && !strings.EqualFold(parsed.Type, providerKey) {
		// The file names a different provider than this binary serves.
		return pluginkit.AuthParseResponse{Handled: false}, nil
	}
	resolved, errResolve := resolveCredential(req.RawJSON, g.impl.cfg)
	if errResolve != nil {
		return pluginkit.AuthParseResponse{}, pluginkit.BadRequestError(
			"this qoder auth file carries no personal access token: " + errResolve.Error())
	}
	storage, errStorage := json.Marshal(credentialDocument{
		Type:   providerKey,
		PAT:    resolved.PAT,
		Region: resolved.Region.id,
	})
	if errStorage != nil {
		return pluginkit.AuthParseResponse{}, pluginkit.NewError("internal_error", errStorage.Error())
	}
	return pluginkit.AuthParseResponse{
		Handled: true,
		Auth: pluginkit.AuthData{
			Provider:    providerKey,
			ID:          providerKey,
			FileName:    req.FileName,
			Label:       resolved.Region.displayName,
			StorageJSON: storage,
			Attributes: map[string]string{
				"region": resolved.Region.id,
				"source": string(resolved.Source),
			},
		},
	}, nil
}

// StartLogin explains where to create the personal access token.
//
// There is no browser flow to start: a PAT is minted in the Qoder web console,
// which is what the returned URL points at.
func (g *gateway) StartLogin(_ context.Context, req pluginkit.AuthLoginStartRequest) (pluginkit.AuthLoginStartResponse, error) {
	if !strings.EqualFold(strings.TrimSpace(req.Provider), providerKey) && strings.TrimSpace(req.Provider) != "" {
		return pluginkit.AuthLoginStartResponse{}, pluginkit.ErrUnhandled
	}
	regionID := ""
	if named, okNamed := req.Metadata["region"].(string); okNamed {
		regionID = named
	}
	active := regionFor(firstNonEmpty(regionID, g.impl.cfg.region))
	return pluginkit.AuthLoginStartResponse{
		Provider: providerKey,
		URL:      active.portalURL,
		State:    providerKey,
		Metadata: map[string]any{
			"message": "Create a Qoder personal access token in the web console, then store it as the pat setting of plugins.configs.cpa-provider-qoder (or write it into a qoder auth file).",
			"region":  active.id,
		},
	}, nil
}

// PollLogin reports whether the configured credential now exchanges successfully.
func (g *gateway) PollLogin(ctx context.Context, req pluginkit.AuthLoginPollRequest) (pluginkit.AuthLoginPollResponse, error) {
	if !strings.EqualFold(strings.TrimSpace(req.Provider), providerKey) && strings.TrimSpace(req.Provider) != "" {
		return pluginkit.AuthLoginPollResponse{}, pluginkit.ErrUnhandled
	}
	resolved, errResolve := resolveCredential(nil, g.impl.cfg)
	if errResolve != nil {
		return pluginkit.AuthLoginPollResponse{Status: pluginkit.LoginPending, Message: errResolve.Error()}, nil
	}
	if _, errBearer := g.impl.tokens.get(ctx, resolved.PAT, resolved.Region); errBearer != nil {
		return pluginkit.AuthLoginPollResponse{Status: pluginkit.LoginPending, Message: errBearer.Error()}, nil
	}
	storage, errStorage := json.Marshal(credentialDocument{Type: providerKey, PAT: resolved.PAT, Region: resolved.Region.id})
	if errStorage != nil {
		return pluginkit.AuthLoginPollResponse{}, pluginkit.NewError("internal_error", errStorage.Error())
	}
	return pluginkit.AuthLoginPollResponse{
		Status: pluginkit.LoginSuccess,
		Auth: pluginkit.AuthData{
			Provider:    providerKey,
			ID:          providerKey,
			FileName:    providerKey + ".json",
			Label:       resolved.Region.displayName,
			StorageJSON: storage,
			Attributes:  map[string]string{"region": resolved.Region.id},
		},
	}, nil
}

// RefreshAuth re-exchanges the PAT and reports the new bearer's expiry.
//
// Qoder does not issue a refresh token for a PAT exchange, so the PAT itself is
// the durable credential and every refresh is a fresh exchange.
func (g *gateway) RefreshAuth(ctx context.Context, req pluginkit.AuthRefreshRequest) (pluginkit.AuthRefreshResponse, error) {
	resolved, errResolve := resolveCredential(req.StorageJSON, g.impl.cfg)
	if errResolve != nil {
		return pluginkit.AuthRefreshResponse{}, errResolve
	}
	g.impl.tokens.invalidate(resolved.PAT, resolved.Region)
	bearer, errBearer := g.impl.tokens.get(ctx, resolved.PAT, resolved.Region)
	if errBearer != nil {
		return pluginkit.AuthRefreshResponse{}, errBearer
	}
	storage, errStorage := json.Marshal(credentialDocument{Type: providerKey, PAT: resolved.PAT, Region: resolved.Region.id})
	if errStorage != nil {
		return pluginkit.AuthRefreshResponse{}, pluginkit.NewError("internal_error", errStorage.Error())
	}
	return pluginkit.AuthRefreshResponse{
		Auth: pluginkit.AuthData{
			Provider:    providerKey,
			ID:          firstNonEmpty(req.AuthID, providerKey),
			Label:       resolved.Region.displayName,
			StorageJSON: storage,
			Attributes:  map[string]string{"region": resolved.Region.id},
		},
		NextRefreshAfter: bearer.ExpiresAt,
	}, nil
}
