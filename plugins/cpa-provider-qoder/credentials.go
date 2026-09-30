package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// credentialDocument is the credential shape this plugin claims and accepts.
//
// The same document is read from three places: the host's auth record
// (StorageJSON), the plugin's own config block, and the environment. The
// upstream calls the secret a "personal token"; both spellings are accepted so
// a hand-written auth file works either way.
type credentialDocument struct {
	Type          string `json:"type"`
	PAT           string `json:"pat"`
	PersonalToken string `json:"personal_token"`
	Region        string `json:"region"`
	Area          string `json:"area"`
}

// parsedCredential is the subset this plugin needs.
type parsedCredential struct {
	PAT      string
	RegionID string
	Type     string
}

func parseCredentialDocument(raw []byte) (parsedCredential, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return parsedCredential{}, nil
	}
	var doc credentialDocument
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		return parsedCredential{}, pluginkit.BadRequestError("decode qoder credential: " + errUnmarshal.Error())
	}
	return parsedCredential{
		PAT:      firstNonEmpty(doc.PAT, doc.PersonalToken),
		RegionID: firstNonEmpty(doc.Region, doc.Area),
		Type:     strings.TrimSpace(doc.Type),
	}, nil
}

// credentialSource records where a PAT came from, for diagnostics.
type credentialSource string

const (
	sourceAuthFile credentialSource = "auth"
	sourceConfig   credentialSource = "config"
	sourceEnv      credentialSource = "env"
	sourceMissing  credentialSource = "missing"
)

// resolvedCredential is a PAT plus the deployment it belongs to.
type resolvedCredential struct {
	PAT    string
	Region *region
	Source credentialSource
}

// envPAT and envRegion are the fallbacks for deployments that inject secrets
// through the environment instead of the plugin config.
const (
	envPAT    = "QODER_PAT"
	envRegion = "QODER_REGION"
)

// resolveCredential applies the credential precedence the plugin documents:
// the host's auth record first, then the plugin config, then the environment.
//
// A region named by the credential wins over the configured one so that a
// global account and a mainland account can live side by side under the single
// provider key.
func resolveCredential(storageJSON []byte, cfg *configState) (resolvedCredential, error) {
	pat := ""
	regionID := ""
	source := sourceMissing

	if len(storageJSON) > 0 {
		parsed, errParse := parseCredentialDocument(storageJSON)
		if errParse != nil {
			return resolvedCredential{}, errParse
		}
		if parsed.PAT != "" {
			pat = parsed.PAT
			regionID = parsed.RegionID
			source = sourceAuthFile
		}
	}
	if pat == "" && cfg != nil {
		if fromConfig := strings.TrimSpace(cfg.pat); fromConfig != "" {
			pat = fromConfig
			regionID = cfg.region
			source = sourceConfig
		}
	}
	if pat == "" {
		if fromEnv := strings.TrimSpace(os.Getenv(envPAT)); fromEnv != "" {
			pat = fromEnv
			regionID = os.Getenv(envRegion)
			source = sourceEnv
		}
	}
	if pat == "" {
		return resolvedCredential{}, pluginkit.NewError(
			"missing_credential",
			"no Qoder personal access token: set pat or region in the plugin config, or write a qoder auth file, or export "+envPAT,
			http.StatusUnauthorized,
		)
	}
	// An unset region on an env-sourced credential still honours the config.
	if strings.TrimSpace(regionID) == "" && cfg != nil {
		regionID = cfg.region
	}
	if strings.TrimSpace(regionID) == "" {
		regionID = os.Getenv(envRegion)
	}
	return resolvedCredential{PAT: pat, Region: regionFor(regionID), Source: source}, nil
}

// jobToken is one exchanged credential bundle. Qoder issues a short-lived
// bearer for the API faces from the long-lived PAT.
type jobToken struct {
	Token     string
	UserID    string
	Name      string
	Email     string
	ExpiresAt time.Time
}

// credentials is a fully resolved credential set for one call.
type credentials struct {
	PAT       string
	Region    *region
	Source    credentialSource
	Identity  cosyIdentity
	ExpiresAt time.Time
}

const (
	// jobTokenExpiryMargin re-exchanges slightly before the bearer lapses, so a
	// long request never starts on an already-expired token.
	jobTokenExpiryMargin = 5 * time.Minute
	// jobTokenDefaultTTL is the fallback when the exchange omits both expiry
	// fields. It matches the reference client.
	jobTokenDefaultTTL = 24 * time.Hour
)

// jobTokenExchangeResponse is the PAT exchange payload.
type jobTokenExchangeResponse struct {
	Token     string          `json:"token"`
	ExpiresAt json.RawMessage `json:"expires_at"`
	ExpiresIn json.RawMessage `json:"expires_in"`
}

// userInfoResponse is the identity the exchanged bearer unlocks.
type userInfoResponse struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

// jobTokenCache memoises exchanged bearers.
//
// One lock per credential keeps two concurrent calls from exchanging twice
// while leaving unrelated credentials independent.
type jobTokenCache struct {
	mu      sync.Mutex
	entries map[string]jobToken
	locks   map[string]*sync.Mutex
	now     func() time.Time
}

func newJobTokenCache() *jobTokenCache {
	return &jobTokenCache{
		entries: map[string]jobToken{},
		locks:   map[string]*sync.Mutex{},
		now:     time.Now,
	}
}

func (c *jobTokenCache) key(pat string, reg *region) string {
	sum := sha256.Sum256([]byte(pat))
	return reg.id + ":" + hex.EncodeToString(sum[:])
}

func (c *jobTokenCache) lockFor(key string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	lock, okLock := c.locks[key]
	if !okLock {
		lock = &sync.Mutex{}
		c.locks[key] = lock
	}
	return lock
}

// invalidate drops a cached bearer, used when the upstream rejects it (401/403)
// so the next call re-exchanges instead of replaying the same failure.
func (c *jobTokenCache) invalidate(pat string, reg *region) {
	key := c.key(pat, reg)
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// get returns a usable bearer, exchanging one when the cache is cold or the
// current token is inside its expiry margin.
func (c *jobTokenCache) get(ctx context.Context, pat string, reg *region) (jobToken, error) {
	key := c.key(pat, reg)
	lock := c.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	c.mu.Lock()
	cached, okCached := c.entries[key]
	c.mu.Unlock()
	if okCached && c.now().Add(jobTokenExpiryMargin).Before(cached.ExpiresAt) {
		return cached, nil
	}

	fresh, errExchange := exchangeJobToken(ctx, pat, reg)
	if errExchange != nil {
		return jobToken{}, errExchange
	}
	c.mu.Lock()
	c.entries[key] = fresh
	c.mu.Unlock()
	return fresh, nil
}

// exchangeJobToken trades a PAT for a job token and resolves the subscriber.
//
// The identity lookup is not optional: the COSY signature needs the user id, and
// a bearer that cannot read the profile cannot drive a chat either.
func exchangeJobToken(ctx context.Context, pat string, reg *region) (jobToken, error) {
	body := map[string]string{"personal_token": pat}
	var exchanged jobTokenExchangeResponse
	result, errCall := openAPIJSON(ctx, reg, http.MethodPost, "/api/v1/jobToken/exchange", "", body, &exchanged)
	if errCall != nil {
		return jobToken{}, pluginkit.NewError("upstream_unavailable", "Qoder token exchange failed: "+errCall.Error(), http.StatusBadGateway)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return jobToken{}, classifyOpenAPIError(result.StatusCode, "token exchange", result.Body)
	}
	if strings.TrimSpace(exchanged.Token) == "" {
		return jobToken{}, pluginkit.NewError("invalid_credential", "Qoder token exchange returned no job token", http.StatusUnauthorized)
	}

	expiresAt := time.Now().Add(jobTokenDefaultTTL)
	if parsed, okExpiry := parseLooseTimestamp(exchanged.ExpiresAt); okExpiry {
		expiresAt = parsed
	} else if seconds, okSeconds := parseLooseSeconds(exchanged.ExpiresIn); okSeconds && seconds > 0 {
		expiresAt = time.Now().Add(time.Duration(seconds) * time.Second)
	}

	var info userInfoResponse
	resultInfo, errInfo := openAPIJSON(ctx, reg, http.MethodGet, "/api/v1/userinfo", exchanged.Token, nil, &info)
	if errInfo != nil {
		return jobToken{}, pluginkit.NewError("upstream_unavailable", "Qoder identity lookup failed: "+errInfo.Error(), http.StatusBadGateway)
	}
	if resultInfo.StatusCode < 200 || resultInfo.StatusCode >= 300 {
		return jobToken{}, classifyOpenAPIError(resultInfo.StatusCode, "identity lookup", resultInfo.Body)
	}
	if strings.TrimSpace(info.ID) == "" {
		return jobToken{}, pluginkit.NewError("invalid_credential", "Qoder identity lookup returned no user id", http.StatusUnauthorized)
	}

	return jobToken{
		Token:     exchanged.Token,
		UserID:    info.ID,
		Name:      firstNonEmpty(info.Name, info.Username),
		Email:     info.Email,
		ExpiresAt: expiresAt,
	}, nil
}

// resolveCredentials is the single entry point every capability uses.
func (p *pluginState) resolveCredentials(ctx context.Context, storageJSON []byte) (*credentials, error) {
	resolved, errResolve := resolveCredential(storageJSON, p.cfg)
	if errResolve != nil {
		return nil, errResolve
	}
	bearer, errBearer := p.tokens.get(ctx, resolved.PAT, resolved.Region)
	if errBearer != nil {
		return nil, errBearer
	}
	return &credentials{
		PAT:    resolved.PAT,
		Region: resolved.Region,
		Source: resolved.Source,
		Identity: cosyIdentity{
			UserID:    bearer.UserID,
			AuthToken: bearer.Token,
			Name:      bearer.Name,
			Email:     bearer.Email,
			MachineID: defaultMachineID(),
		},
		ExpiresAt: bearer.ExpiresAt,
	}, nil
}

// parseLooseTimestamp accepts the epoch-milliseconds integer, the epoch-seconds
// integer and the ISO-8601 string spellings the upstream has been seen using for
// the same field.
func parseLooseTimestamp(raw json.RawMessage) (time.Time, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return time.Time{}, false
	}
	if trimmed[0] == '"' {
		var text string
		if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal != nil {
			return time.Time{}, false
		}
		parsed, errParse := time.Parse(time.RFC3339, strings.TrimSpace(text))
		if errParse != nil {
			return time.Time{}, false
		}
		return parsed, true
	}
	var numeric float64
	if errUnmarshal := json.Unmarshal(raw, &numeric); errUnmarshal != nil {
		return time.Time{}, false
	}
	if numeric <= 0 {
		return time.Time{}, false
	}
	// Values this large can only be milliseconds: seconds would land in year
	// 33658.
	if numeric > 1e11 {
		return time.UnixMilli(int64(numeric)), true
	}
	return time.Unix(int64(numeric), 0), true
}

func parseLooseSeconds(raw json.RawMessage) (float64, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0, false
	}
	if trimmed[0] == '"' {
		var text string
		if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal != nil {
			return 0, false
		}
		var numeric float64
		if _, errScan := fmt.Sscanf(strings.TrimSpace(text), "%g", &numeric); errScan != nil {
			return 0, false
		}
		return numeric, true
	}
	var numeric float64
	if errUnmarshal := json.Unmarshal(raw, &numeric); errUnmarshal != nil {
		return 0, false
	}
	return numeric, true
}
