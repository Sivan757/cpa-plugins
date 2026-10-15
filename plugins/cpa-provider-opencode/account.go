package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// The free tier is not anonymous. Measured against the live endpoint: a request
// carrying "Authorization: Bearer public" is refused with FreeTierError even
// when every other header matches what the CLI sends. The accepted credential is
// the account session token minted by the console, sent with the active
// workspace id.
//
// Both values come from a signed-in OpenCode install, in
// ~/.local/share/opencode/opencode.db:
//
//	account.access_token         the session token            (st_...)
//	account.refresh_token        the renewal token            (rt_...)
//	account_state.active_org_id  the workspace to meter against (wrk_...)
const (
	// sessionTokenPrefix is the prefix a console session token carries.
	sessionTokenPrefix = "st_"
	// refreshURL exchanges a refresh token for a new session token.
	refreshURL = "https://opencode.ai/console/auth/device/token"
	// orgsURL lists the workspaces a session token may act in.
	//
	// A device login returns no workspace id, and every console call needs one in
	// x-org-id, so the id is discovered here instead of asking the operator for a
	// value only the console knows.
	orgsURL = "https://opencode.ai/console/api/orgs"
	// refreshClientID is the client identifier the CLI sends.
	refreshClientID = "opencode-cli"
	// refreshMargin renews this long before the stated expiry, so a request
	// cannot land on a token that expires while it is in flight.
	refreshMargin = time.Hour
	// orgHeader carries the workspace the free tier is metered against.
	orgHeader = "x-opencode-org-id"
)

// accountHTTPJSON performs the outbound JSON call a renewal needs. It is a
// variable because the host bridge it normally routes through exists only inside
// CPA, so a unit test substitutes a plain HTTP client to drive the flow offline.
var accountHTTPJSON = cpakit.HTTPJSON

// accountOrgsURL lists the workspaces. It is a variable so a unit test can point
// the lookup at a local server instead of the live console.
var accountOrgsURL = orgsURL

// accountDocument is the persisted account credential. The upstream rotates both
// tokens on every renewal, so this file is the plugin's own copy: the host auth
// record carries the bootstrap values, this file carries the current ones.
type accountDocument struct {
	Type         string `json:"type"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	OrgID        string `json:"org_id,omitempty"`
	// ExpiresAt is when AccessToken stops working, in Unix milliseconds. A zero
	// value means unknown, in which case a renewal is attempted whenever a
	// refresh token exists.
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// Source records where the credential came from, for diagnostics only.
	Source string `json:"source,omitempty"`
}

// accountAuth holds the live account credential and serialises renewals.
type accountAuth struct {
	mu      sync.Mutex
	access  string
	refresh string
	orgID   string
	expires time.Time
	dir     string
	// loadWarning records an unreadable persisted credential, so the status
	// route reports it instead of silently falling back.
	loadWarning string
}

// configured reports whether any account credential is available.
func (a *accountAuth) configured() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.TrimSpace(a.access) != "" || strings.TrimSpace(a.refresh) != ""
}

// hasRefresh reports whether a renewal token is available.
func (a *accountAuth) hasRefresh() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.TrimSpace(a.refresh) != ""
}

// org returns the configured workspace id.
func (a *accountAuth) org() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.orgID
}

// documentPath is where the rotating credential is persisted.
func (a *accountAuth) documentPath() string {
	dir := strings.TrimSpace(a.dir)
	if dir == "" {
		home, errHome := os.UserHomeDir()
		if errHome != nil {
			return "opencode-account.json"
		}
		dir = filepath.Join(home, ".dsh-cpa")
	}
	return filepath.Join(dir, "opencode-account.json")
}

// load reads the persisted credential, if any, and reports whether it supplied
// one.
//
// The return value decides precedence: the upstream rotates the refresh token on
// every renewal, so once a rotation has been persisted the file is the only
// current copy. A configuration value that still holds the pre-rotation token
// would otherwise replace a working credential on the next reload.
func (a *accountAuth) load() bool {
	a.mu.Lock()
	path := a.documentPath()
	a.mu.Unlock()

	rawBytes, errRead := os.ReadFile(path)
	if errRead != nil {
		if !os.IsNotExist(errRead) {
			a.mu.Lock()
			a.loadWarning = fmt.Sprintf("read %s: %v", path, errRead)
			a.mu.Unlock()
		}
		return false
	}
	var doc accountDocument
	if errUnmarshal := json.Unmarshal(rawBytes, &doc); errUnmarshal != nil {
		a.mu.Lock()
		a.loadWarning = fmt.Sprintf("decode %s: %v", path, errUnmarshal)
		a.mu.Unlock()
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(doc.AccessToken) != "" {
		a.access = strings.TrimSpace(doc.AccessToken)
	}
	if strings.TrimSpace(doc.RefreshToken) != "" {
		a.refresh = strings.TrimSpace(doc.RefreshToken)
	}
	if strings.TrimSpace(doc.OrgID) != "" {
		a.orgID = strings.TrimSpace(doc.OrgID)
	}
	if doc.ExpiresAt > 0 {
		a.expires = time.UnixMilli(doc.ExpiresAt)
	}
	return strings.TrimSpace(doc.AccessToken) != "" || strings.TrimSpace(doc.RefreshToken) != ""
}

// apply merges explicitly configured values.
//
// override selects the precedence. On a first run the configuration is the only
// source, so it wins. Once a rotation has been persisted the file holds the
// current pair, so the configuration only fills values the file lacks. An
// operator who needs to replace a credential deletes the document.
// clear drops every credential this object holds.
//
// Configure reuses one accountAuth across reconfigurations, and load and apply
// only ever set a value. Without this, a credential that an operator removes
// from the configuration keeps serving requests until the process restarts, so
// no reload can retire an account.
func (a *accountAuth) clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.access = ""
	a.refresh = ""
	a.orgID = ""
	a.expires = time.Time{}
	a.loadWarning = ""
}

func (a *accountAuth) apply(access, refresh, orgID string, override bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if value := strings.TrimSpace(access); value != "" && (override || a.access == "") {
		a.access = value
		// A token supplied by hand carries no stated expiry, so clear it: the
		// next request renews when a refresh token is available.
		a.expires = time.Time{}
	}
	if value := strings.TrimSpace(refresh); value != "" && (override || a.refresh == "") {
		a.refresh = value
	}
	if value := strings.TrimSpace(orgID); value != "" && (override || a.orgID == "") {
		a.orgID = value
	}
}

// credentials returns the current session token and workspace id, renewing the
// token first when it is missing or close to expiry.
func (a *accountAuth) credentials(ctx context.Context) (string, string, error) {
	if a == nil {
		return "", "", nil
	}
	a.mu.Lock()
	haveAccess := strings.TrimSpace(a.access) != ""
	hasRefresh := strings.TrimSpace(a.refresh) != ""
	expiryKnown := !a.expires.IsZero()
	fresh := haveAccess && expiryKnown && time.Now().Add(refreshMargin).Before(a.expires)
	a.mu.Unlock()

	if hasRefresh && !fresh {
		if errRefresh := a.renew(ctx); errRefresh != nil {
			// A failed renewal is not fatal while a token is still on hand: the
			// gateway, not the clock, decides whether that token works.
			if !haveAccess {
				return "", "", errRefresh
			}
			cpakit.HostLog(ctx, "warn", "opencode: token renewal failed, using the current token: "+errRefresh.Error())
		}
	}

	a.mu.Lock()
	access := a.access
	orgID := a.orgID
	a.mu.Unlock()

	// Every console call needs a workspace, and a device login does not return
	// one, so resolve it from the token rather than sending a request without it.
	if strings.TrimSpace(orgID) == "" && strings.TrimSpace(access) != "" {
		if discovered := a.discoverOrg(ctx, access); discovered != "" {
			a.mu.Lock()
			a.orgID = discovered
			doc := accountDocument{
				Type:         providerKey,
				AccessToken:  a.access,
				RefreshToken: a.refresh,
				OrgID:        a.orgID,
				ExpiresAt:    a.expires.UnixMilli(),
				Source:       "org-discovery",
			}
			path := a.documentPath()
			a.mu.Unlock()
			cpakit.HostLog(ctx, "info", "opencode: discovered workspace "+discovered)
			a.persist(path, doc)
			orgID = discovered
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	return a.access, orgID, nil
}

// discoverOrg lists the workspaces the session token may act in and returns the
// first id. A device login carries none, and a console call without one is
// refused with OrgRequired.
func (a *accountAuth) discoverOrg(ctx context.Context, access string) string {
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers.Set("Authorization", "Bearer "+access)
	headers.Set("User-Agent", "opencode/"+versionFallback)

	var orgs []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	result, errCall := accountHTTPJSON(ctx, http.MethodGet, accountOrgsURL, headers, nil, &orgs)
	if errCall != nil {
		cpakit.HostLog(ctx, "warn", "opencode: could not list workspaces: "+errCall.Error())
		return ""
	}
	if result != nil && result.StatusCode != http.StatusOK {
		cpakit.HostLog(ctx, "warn", fmt.Sprintf("opencode: workspace lookup answered HTTP %d", result.StatusCode))
		return ""
	}
	for _, org := range orgs {
		if id := strings.TrimSpace(org.ID); id != "" {
			return id
		}
	}
	return ""
}

// renew exchanges the refresh token for a new session token and persists the
// result. The upstream rotates both tokens, so the new pair replaces the old.
func (a *accountAuth) renew(ctx context.Context) error {
	return a.renewAgainst(ctx, refreshURL)
}

// renewAgainst performs one renewal against an explicit endpoint, which is what
// makes the flow testable without reaching the live console.
func (a *accountAuth) renewAgainst(ctx context.Context, url string) error {
	a.mu.Lock()
	refresh := strings.TrimSpace(a.refresh)
	a.mu.Unlock()
	if refresh == "" {
		return fmt.Errorf("no refresh token is configured")
	}

	request := map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": refresh,
		"client_id":     refreshClientID,
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "opencode/"+versionFallback)

	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		// OrgID is the workspace the renewed token is metered against. The
		// console returns it, so a renewal also repairs a missing or stale id.
		OrgID string `json:"org_id"`
	}
	result, errCall := accountHTTPJSON(ctx, http.MethodPost, url, headers, request, &response)
	if errCall != nil {
		return cpakit.NewError("upstream_unavailable", "token renewal failed: "+errCall.Error(), http.StatusBadGateway)
	}
	if result != nil && result.StatusCode != http.StatusOK {
		return cpakit.NewError("invalid_credential",
			fmt.Sprintf("token renewal was refused with HTTP %d: %s", result.StatusCode, cpakit.Redact(string(result.Body))),
			http.StatusUnauthorized)
	}
	if strings.TrimSpace(response.AccessToken) == "" {
		return cpakit.NewError("invalid_credential", "token renewal returned no access token", http.StatusUnauthorized)
	}

	a.mu.Lock()
	a.access = strings.TrimSpace(response.AccessToken)
	if strings.TrimSpace(response.RefreshToken) != "" {
		a.refresh = strings.TrimSpace(response.RefreshToken)
	}
	if value := strings.TrimSpace(response.OrgID); value != "" {
		a.orgID = value
	}
	expiresIn := response.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	a.expires = time.Now().Add(time.Duration(expiresIn) * time.Second)
	doc := accountDocument{
		Type:         providerKey,
		AccessToken:  a.access,
		RefreshToken: a.refresh,
		OrgID:        a.orgID,
		ExpiresAt:    a.expires.UnixMilli(),
		Source:       "refresh",
	}
	path := a.documentPath()
	a.mu.Unlock()

	cpakit.HostLog(ctx, "info", "opencode: renewed the account session token")
	a.persist(path, doc)
	return nil
}

// persist writes the rotating credential with owner-only permissions. A write
// failure is reported but does not fail the request: the token in memory keeps
// working until the process restarts.
func (a *accountAuth) persist(path string, doc accountDocument) {
	encoded, errMarshal := json.MarshalIndent(doc, "", "  ")
	if errMarshal != nil {
		return
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return
	}
	_ = os.WriteFile(path, append(encoded, '\n'), 0o600)
}

// report describes the credential for the management status route.
func (a *accountAuth) report() map[string]any {
	if a == nil {
		return map[string]any{"configured": false}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]any{
		"configured":      strings.TrimSpace(a.access) != "" || strings.TrimSpace(a.refresh) != "",
		"hasRefreshToken": strings.TrimSpace(a.refresh) != "",
		"orgID":           a.orgID,
		"document":        a.documentPath(),
	}
	if !a.expires.IsZero() {
		out["expiresAt"] = a.expires.Format(time.RFC3339)
		out["expiresIn"] = time.Until(a.expires).Round(time.Minute).String()
	}
	if a.loadWarning != "" {
		out["loadWarning"] = a.loadWarning
	}
	return out
}
