package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// oauthEnvelope is the {code,msg,data} wrapper every plugin-auth endpoint uses.
type oauthEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// authStateData is the device-flow state response.
type authStateData struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

// oauthTokenData is the token payload issued by the poll and refresh endpoints.
// RefreshExpiresIn is a duration in seconds; the gateway has no
// refreshExpiresAt field, so a client that reads one never learns the refresh
// window.
type oauthTokenData struct {
	AccessToken      string `json:"accessToken"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshToken     string `json:"refreshToken"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
	TokenType        string `json:"tokenType"`
	Scope            string `json:"scope"`
}

// loginAccountData is the best-effort account lookup that follows a successful
// poll.
type loginAccountData struct {
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	EnterpriseID   string `json:"enterpriseId"`
	EnterpriseName string `json:"enterpriseName"`
}

// createAuthState opens a device-flow login. The platform parameter is required
// but its value is not validated by the gateway, so the CLI identity is used.
func createAuthState(ctx context.Context, baseURL string) (*authStateData, error) {
	endpoint := strings.TrimRight(baseURL, "/") + pathAuthState + "?platform=CLI"
	headers := http.Header{}
	headers.Set("Accept", "application/json")

	var envelope oauthEnvelope
	result, errCall := pluginkit.HTTPJSON(ctx, http.MethodPost, endpoint, headers, nil, &envelope)
	if errCall != nil {
		return nil, pluginkit.NewError("upstream_unavailable", "create CodeBuddy login state: "+errCall.Error(), http.StatusBadGateway)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return nil, pluginkit.StatusError(result.StatusCode, "create CodeBuddy login state", result.Body)
	}
	if envelope.Code != 0 {
		return nil, pluginkit.NewError("upstream_error",
			fmt.Sprintf("create CodeBuddy login state returned code %d: %s", envelope.Code, envelope.Msg),
			http.StatusBadGateway)
	}
	var data authStateData
	if errDecode := json.Unmarshal(envelope.Data, &data); errDecode != nil {
		return nil, pluginkit.NewError("upstream_error", "decode login state: "+errDecode.Error(), http.StatusBadGateway)
	}
	if strings.TrimSpace(data.State) == "" {
		return nil, pluginkit.NewError("upstream_error", "the login state response carried no state token", http.StatusBadGateway)
	}
	return &data, nil
}

// pollAuthToken performs one token poll.
//
// pending=true means the user has not finished authorising. The gateway answers
// authPendingCode for a pending, a fabricated, and an expired state alike, so
// the caller owns the timeout: this function never distinguishes them.
func pollAuthToken(ctx context.Context, baseURL, state string) (*oauthTokenData, bool, error) {
	endpoint := strings.TrimRight(baseURL, "/") + pathAuthToken + "?state=" + url.QueryEscape(state)
	headers := http.Header{}
	headers.Set("Accept", "application/json")

	var envelope oauthEnvelope
	result, errCall := pluginkit.HTTPJSON(ctx, http.MethodGet, endpoint, headers, nil, &envelope)
	if errCall != nil {
		return nil, false, pluginkit.NewError("upstream_unavailable", "poll CodeBuddy login: "+errCall.Error(), http.StatusBadGateway)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return nil, false, pluginkit.StatusError(result.StatusCode, "poll CodeBuddy login", result.Body)
	}
	if envelope.Code == authPendingCode {
		return nil, true, nil
	}
	if envelope.Code != 0 {
		return nil, false, pluginkit.NewError("upstream_error",
			fmt.Sprintf("login poll returned code %d: %s", envelope.Code, envelope.Msg),
			http.StatusBadGateway)
	}
	var data oauthTokenData
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if errDecode := json.Unmarshal(envelope.Data, &data); errDecode != nil {
			return nil, false, pluginkit.NewError("upstream_error", "decode login token: "+errDecode.Error(), http.StatusBadGateway)
		}
	}
	if strings.TrimSpace(data.AccessToken) == "" {
		// A success code without a token is not a usable login; keep the caller
		// polling rather than reporting a credential it cannot use.
		return nil, true, nil
	}
	return &data, false, nil
}

// fetchLoginAccount reads the uid and enterprise identity of the account that
// just authorised. It is best effort: an unfinished state is answered with a
// bare nginx 401 page, and a missing nickname must not fail a login.
func fetchLoginAccount(ctx context.Context, baseURL, state, accessToken, domain string) *loginAccountData {
	endpoint := strings.TrimRight(baseURL, "/") + pathLoginAccount + "?state=" + url.QueryEscape(state)
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers.Set("Authorization", "Bearer "+accessToken)
	if domain != "" {
		headers.Set("X-Domain", domain)
	}
	headers.Set("X-No-User-Id", "true")
	headers.Set("X-No-Enterprise-Id", "true")

	var envelope oauthEnvelope
	result, errCall := pluginkit.HTTPJSON(ctx, http.MethodGet, endpoint, headers, nil, &envelope)
	if errCall != nil || result.StatusCode < 200 || result.StatusCode >= 300 || envelope.Code != 0 {
		return nil
	}
	var data loginAccountData
	if errDecode := json.Unmarshal(envelope.Data, &data); errDecode != nil {
		return nil
	}
	return &data
}

// refreshOAuthToken renews an access token.
//
// The gateway authenticates the refresh with the old access token, the refresh
// token, and the account identity headers; a bogus refresh token is answered
// with HTTP 401 and code 12153, which is not retryable because only a new
// device login can fix it.
func refreshOAuthToken(ctx context.Context, baseURL string, stored *storedAuth) (*oauthTokenData, error) {
	endpoint := strings.TrimRight(baseURL, "/") + pathAuthRefresh
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	if stored.AccessToken != "" {
		headers.Set("Authorization", "Bearer "+stored.AccessToken)
	}
	if stored.RefreshToken != "" {
		headers.Set("X-Refresh-Token", stored.RefreshToken)
	}
	if stored.Domain != "" {
		headers.Set("X-Domain", stored.Domain)
	}
	if stored.UID != "" {
		headers.Set("X-User-Id", stored.UID)
	}
	if stored.EnterpriseID != "" {
		headers.Set("X-Enterprise-Id", stored.EnterpriseID)
	}

	var envelope oauthEnvelope
	result, errCall := pluginkit.HTTPJSON(ctx, http.MethodPost, endpoint, headers, map[string]any{}, &envelope)
	if errCall != nil {
		return nil, pluginkit.NewError("upstream_unavailable", "refresh CodeBuddy token: "+errCall.Error(), http.StatusBadGateway)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return nil, pluginkit.NewError("invalid_credential",
			"CodeBuddy rejected the OAuth refresh; start the device login again: "+truncateBody(result.Body),
			http.StatusUnauthorized)
	}
	if envelope.Code != 0 {
		return nil, pluginkit.NewError("invalid_credential",
			fmt.Sprintf("CodeBuddy refresh returned code %d: %s; start the device login again", envelope.Code, envelope.Msg),
			http.StatusUnauthorized)
	}
	var data oauthTokenData
	if errDecode := json.Unmarshal(envelope.Data, &data); errDecode != nil {
		return nil, pluginkit.NewError("upstream_error", "decode refreshed token: "+errDecode.Error(), http.StatusBadGateway)
	}
	if strings.TrimSpace(data.AccessToken) == "" {
		return nil, pluginkit.NewError("invalid_credential", "CodeBuddy refresh returned no access token", http.StatusUnauthorized)
	}
	return &data, nil
}
