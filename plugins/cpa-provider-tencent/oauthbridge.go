package main

import (
	"context"
	"strings"
	"time"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// startCodeBuddyLogin begins the CodeBuddy device login and hands the
// authorization URL back to the host, which is the only step a human must
// complete. (Over the verified CodeBuddy oauth.go helpers.)
func (g *gateway) startCodeBuddyLogin(ctx context.Context, req pluginkit.AuthLoginStartRequest) (pluginkit.AuthLoginStartResponse, error) {
	state, errState := createAuthState(ctx, defaultBaseURL)
	if errState != nil {
		return pluginkit.AuthLoginStartResponse{}, errState
	}
	return pluginkit.AuthLoginStartResponse{
		Provider:  providerKey,
		URL:       state.AuthURL,
		State:     state.State,
		ExpiresAt: time.Now().Add(oauthTimeout),
		Metadata: map[string]any{
			"kind":      "codebuddy",
			"loginHint": "Open the URL, authorise, then poll for the token.",
		},
	}, nil
}

// pollCodeBuddyLogin performs one token poll. A transient gateway failure
// keeps the login pending so the host can poll again within the local
// timeout; only a saved token ends the login successfully.
func (g *gateway) pollCodeBuddyLogin(ctx context.Context, req pluginkit.AuthLoginPollRequest) (pluginkit.AuthLoginPollResponse, error) {
	data, done, errPoll := pollAuthToken(ctx, defaultBaseURL, req.State)
	if errPoll != nil {
		return pluginkit.AuthLoginPollResponse{Status: pluginkit.LoginPending, Message: errPoll.Error()}, nil
	}
	if !done {
		return pluginkit.AuthLoginPollResponse{
			Status:  pluginkit.LoginPending,
			Message: "waiting for the CodeBuddy authorization page to be approved",
		}, nil
	}
	kind := kinds["codebuddy"]
	doc := storedAuth{Mode: string(modeOAuth)}
	doc.apply(data, time.Now())

	// The account lookup is best effort: a pending state is answered by the
	// front proxy with a bare page, and that must not fail the login.
	if info := fetchLoginAccount(ctx, defaultBaseURL, req.State, doc.AccessToken, doc.Domain); info != nil {
		doc.UID = info.UID
		doc.Nickname = info.Nickname
		doc.EnterpriseID = info.EnterpriseID
		doc.EnterpriseName = info.EnterpriseName
	}
	if errSave := g.impl.saveStoredAuth(&doc); errSave != nil {
		return pluginkit.AuthLoginPollResponse{}, pluginkit.NewError("internal_error", "persist the CodeBuddy credential: "+errSave.Error())
	}
	return pluginkit.AuthLoginPollResponse{
		Status:  pluginkit.LoginSuccess,
		Message: "CodeBuddy credential saved",
		Auth: pluginkit.AuthData{
			Provider:    providerKey,
			ID:          providerKey + "-" + kind.id,
			FileName:    providerKey + "-" + kind.id + ".json",
			Label:       kind.displayName,
			StorageJSON: storageFor(kind),
			Attributes:  map[string]string{"kind": kind.id, "region": kind.region, "mode": string(modeOAuth)},
			Metadata: map[string]any{
				"uid":      doc.UID,
				"nickname": doc.Nickname,
				"mode":     doc.Mode,
			},
		},
	}, nil
}

// refreshTokens renews the stored CodeBuddy token. The gateway authenticates
// the refresh with the old access token, the refresh token, and the account
// identity headers; a rejected refresh surfaces as invalid_credential because
// only a new device login can fix it.
func (p *pluginState) refreshTokens(ctx context.Context, doc *storedAuth) error {
	if strings.TrimSpace(doc.RefreshToken) == "" {
		return pluginkit.NewError("invalid_credential", "no CodeBuddy refresh token; run the browser login again", 401)
	}
	data, errRefresh := refreshOAuthToken(ctx, defaultBaseURL, doc)
	if errRefresh != nil {
		return errRefresh
	}
	doc.apply(data, time.Now())
	return p.saveStoredAuth(doc)
}
