package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// upstreamCall issues an authenticated request against one product's API.
//
// The desktop kinds carry the WorkBuddy identity headers the upstream gateway
// validates; a missing user/enterprise/domain header is not omitted but
// replaced by its explicit "absent" marker, because the gateway rejects the
// request outright when the header is simply missing. The CodeBuddy kind
// instead uses its own CLI header group, which is what its endpoints resolve
// the active key and account from.
func upstreamCall(ctx context.Context, v *credentialKind, creds *credentials, method, url string, headers http.Header, body any, out any) (*pluginkit.HTTPResult, error) {
	if headers == nil {
		headers = http.Header{}
	}
	if creds == nil {
		return nil, pluginkit.NewError("invalid_credential", "no credential was resolved for this request", 401)
	}
	if v != nil && v.id == kindCodebuddyID {
		for key, values := range apiHeaders(creds) {
			for _, value := range values {
				headers.Set(key, value)
			}
		}
		return pluginkit.HTTPJSON(ctx, method, url, headers, body, out)
	}
	headers.Set("Accept", "application/json, text/plain, */*")
	bearer := creds.AccessToken
	if creds.APIKey != "" {
		bearer = creds.APIKey
	}
	headers.Set("Authorization", "Bearer "+bearer)
	if creds.UID != "" {
		headers.Set("X-User-Id", creds.UID)
	} else {
		headers.Set("X-No-User-Id", "1")
	}
	if creds.isEnterprise() {
		headers.Set("X-Enterprise-Id", creds.EnterpriseID)
		headers.Set("X-Tenant-Id", creds.EnterpriseID)
	} else {
		headers.Set("X-No-Enterprise-Id", "1")
	}
	if creds.Domain != "" {
		headers.Set("X-Domain", creds.Domain)
	} else {
		headers.Set("X-No-Department-Info", "1")
	}
	return pluginkit.HTTPJSON(ctx, method, url, headers, body, out)
}

// desktopChatHeaders builds the identity header set for a desktop-product chat
// completion call.
func desktopChatHeaders(kind *credentialKind, creds *credentials, appVersion, cliVersion string) http.Header {
	headers := http.Header{}
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("X-Requested-With", "XMLHttpRequest")
	headers.Set("Origin", kind.origin)
	headers.Set("Referer", kind.origin+"/")
	headers.Set("Content-Type", "application/json")
	headers.Set("X-IDE-Type", "WorkBuddy")
	headers.Set("X-IDE-Name", "WorkBuddy")
	headers.Set("X-IDE-Version", appVersion)
	headers.Set("X-Product", "SaaS")
	headers.Set("User-Agent", desktopUA(kind.productToken, appVersion, cliVersion))
	bearer := creds.bearer()
	headers.Set("Authorization", "Bearer "+bearer)
	if creds.UID != "" {
		headers.Set("X-User-Id", creds.UID)
	} else {
		headers.Set("X-No-User-Id", "1")
	}
	if creds.isEnterprise() {
		headers.Set("X-Enterprise-Id", creds.EnterpriseID)
		headers.Set("X-Tenant-Id", creds.EnterpriseID)
	} else {
		headers.Set("X-No-Enterprise-Id", "1")
	}
	if creds.Domain != "" {
		headers.Set("X-Domain", creds.Domain)
	} else {
		headers.Set("X-No-Department-Info", "1")
	}
	return headers
}

// desktopUA renders the desktop user agent. The CLI token is dropped entirely
// when the CLI version is unknown rather than guessed.
func desktopUA(product, appVersion, cliVersion string) string {
	ua := fmt.Sprintf("WorkBuddy/%s %s/%s", appVersion, product, appVersion)
	if cliVersion != "" {
		ua += " CLI/" + cliVersion
	}
	return ua
}

// catalogHeaders builds the header set for a model catalog read. The CN catalog
// is served to the CLI identity while the international one requires the app
// identity; the caller passes the user agent that selects the right roster.
func (v *credentialKind) catalogHeaders(appVersion string) http.Header {
	headers := http.Header{}
	headers.Set("X-Requested-With", "XMLHttpRequest")
	headers.Set("Origin", v.origin)
	headers.Set("Referer", v.origin+"/")
	headers.Set("X-Product", "SaaS")
	if v.region == "cn" {
		headers.Set("User-Agent", v.catalogUA)
	} else {
		// The international gateway rejects any user agent containing a space
		// in this position (error 12403), so the product token must be joined
		// without one.
		headers.Set("User-Agent", "WorkBuddyAI/"+appVersion)
	}
	return headers
}

// billingHeaders builds the header set for the quota endpoints.
func (v *credentialKind) billingHeaders() http.Header {
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	return headers
}

// configEnvelope is the outer shape of a /v3/config response. The endpoint is
// inconsistent across regions and deployments: CN wraps the document in
// {code,msg,data}, while some international deployments return the document
// bare at the top level.
type configEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// catalogDocument is the model roster document.
type catalogDocument struct {
	Models          []modelRow     `json:"models"`
	Agents          []agentRow     `json:"agents"`
	ModelPromotions []promotionRow `json:"modelPromotions"`
	EnterpriseID    string         `json:"enterpriseId"`
}

type modelRow struct {
	ID                 string        `json:"id"`
	Name               string        `json:"name"`
	Credits            string        `json:"credits"`
	MaxInputTokens     int64         `json:"maxInputTokens"`
	MaxOutputTokens    int64         `json:"maxOutputTokens"`
	SupportsImages     bool          `json:"supportsImages"`
	SupportsToolCall   bool          `json:"supportsToolCall"`
	SupportsReasoning  bool          `json:"supportsReasoning"`
	OnlyReasoning      bool          `json:"onlyReasoning"`
	Reasoning          *reasoningRow `json:"reasoning"`
	ContextWindow      *contextRow   `json:"contextWindow"`
	Tags               []string      `json:"tags"`
	Disabled           bool          `json:"disabled"`
	DisabledMultimodal bool          `json:"disabledMultimodal"`
	// RelatedModels is an object upstream (model id -> relation), so it is kept
	// raw: the plugin does not consume it and a typed slice breaks decoding.
	RelatedModels json.RawMessage `json:"relatedModels"`
	Vendor        string          `json:"vendor"`
	DescriptionZh string          `json:"descriptionZh"`
	DescriptionEn string          `json:"descriptionEn"`
}

type contextRow struct {
	DefaultLength    int64   `json:"defaultLength"`
	SupportedLengths []int64 `json:"supportedLengths"`
}

type reasoningRow struct {
	SupportedEfforts   []string `json:"supportedEfforts"`
	DefaultEffort      string   `json:"defaultEffort"`
	Effort             string   `json:"effort"`
	CanDisableThinking bool     `json:"canDisableThinking"`
	Summary            string   `json:"summary"`
}

type agentRow struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

type promotionRow struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Enabled  bool            `json:"enabled"`
	Priority int             `json:"priority"`
	ModelIDs []string        `json:"modelIds"`
	Badge    *promotionBadge `json:"badge"`
	Discount *promotionDisc  `json:"discount"`
	Schedule *promotionSched `json:"schedule"`
}

type promotionBadge struct {
	Color   string `json:"color"`
	Display string `json:"display"`
	Label   string `json:"label"`
}

type promotionDisc struct {
	DiscountedCredits string   `json:"discountedCredits"`
	DisplayMode       string   `json:"displayMode"`
	Factor            *float64 `json:"factor"`
}

type promotionSched struct {
	Timezone   string `json:"timezone"`
	ValidFrom  string `json:"validFrom"`
	ValidUntil string `json:"validUntil"`
}

// readCatalog fetches and normalises the model roster for one credential.
// The header group is per kind: the CN desktop catalog is served to the CLI
// identity with the catalogUA user agent, the international one requires the
// joined WorkBuddyAI spelling, and the CodeBuddy kind uses the CLI group its
// gateway validates on this endpoint.
func readCatalog(ctx context.Context, v *credentialKind, creds *credentials, appVersion string) (*catalogDocument, error) {
	url := v.chatBase + pathConfig
	headers := v.catalogHeaders(appVersion)
	if v.id == kindCodebuddyID {
		headers = apiHeaders(creds)
	}
	var envelope configEnvelope
	result, errCall := upstreamCall(ctx, v, creds, http.MethodGet, url, headers, nil, &envelope)
	if errCall != nil {
		return nil, errCall
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return nil, pluginkit.StatusError(result.StatusCode, "model catalog", result.Body)
	}
	doc, errDecode := decodeCatalog(result.Body, envelope)
	if errDecode != nil {
		return nil, errDecode
	}
	return doc, nil
}

// decodeCatalog accepts both the enveloped and the bare document shapes.
func decodeCatalog(raw []byte, envelope configEnvelope) (*catalogDocument, error) {
	var doc catalogDocument
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if errUnmarshal := json.Unmarshal(envelope.Data, &doc); errUnmarshal != nil {
			return nil, fmt.Errorf("decode catalog data: %w", errUnmarshal)
		}
		if len(doc.Models) > 0 || len(doc.Agents) > 0 {
			if envelope.Code != 0 {
				return nil, pluginkit.NewError("upstream_error", fmt.Sprintf("catalog returned code %d: %s", envelope.Code, envelope.Msg), http.StatusBadGateway)
			}
			return &doc, nil
		}
	}
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		return nil, fmt.Errorf("decode catalog document: %w", errUnmarshal)
	}
	if len(doc.Models) == 0 && len(doc.Agents) == 0 {
		return nil, pluginkit.NewError("upstream_error", "model catalog carried no models", http.StatusBadGateway)
	}
	return &doc, nil
}

// normalizeCredits strips the trailing unit from the credit multiplier the
// catalog reports, so "x0.79 credits" renders as "x0.79".
func normalizeCredits(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	if index := strings.Index(strings.ToLower(trimmed), " credits"); index > 0 {
		trimmed = strings.TrimSpace(trimmed[:index])
	}
	free := trimmed == "x0.00" || trimmed == "0x" || trimmed == "x0"
	return trimmed, free
}

// activePromotion returns the highest priority promotion that applies to a
// model at time now. Promotions are re-evaluated on every read so an expired
// campaign stops being advertised without a restart.
func activePromotion(doc *catalogDocument, modelID string, now time.Time) *promotionRow {
	var chosen *promotionRow
	for index := range doc.ModelPromotions {
		promo := &doc.ModelPromotions[index]
		if !promo.Enabled || promo.Discount == nil || promo.Discount.DisplayMode != "replace" {
			continue
		}
		if promo.Discount.Factor == nil || *promo.Discount.Factor < 0 {
			continue
		}
		if !slices.Contains(promo.ModelIDs, modelID) {
			continue
		}
		if promo.Schedule != nil {
			if !withinSchedule(promo.Schedule, now) {
				continue
			}
		}
		if chosen == nil || promo.Priority > chosen.Priority {
			chosen = promo
		}
	}
	return chosen
}

func withinSchedule(schedule *promotionSched, now time.Time) bool {
	location := time.UTC
	if schedule.Timezone != "" {
		if parsed, errLoad := time.LoadLocation(schedule.Timezone); errLoad == nil {
			location = parsed
		}
	}
	if schedule.ValidFrom != "" {
		if from, errParse := time.Parse(time.RFC3339, schedule.ValidFrom); errParse == nil && now.Before(from) {
			return false
		}
	}
	if schedule.ValidUntil != "" {
		if until, errParse := time.Parse(time.RFC3339, schedule.ValidUntil); errParse == nil && now.After(until) {
			return false
		}
	}
	_ = location
	return true
}

// formatCredits renders a promotion factor as a multiplier.
func formatCredits(factor float64) string {
	if factor == 0 {
		return "x0.00"
	}
	return "x" + strconv.FormatFloat(factor, 'f', -1, 64)
}

// Gateway identity and endpoint constants shared with the OAuth machinery.
const (
	defaultBaseURL = "https://copilot.tencent.com"

	// The identity of the official CodeBuddy CLI. The gateway validates this
	// shape on /v3/config (error 12403).
	userAgent      = "CLI/unknown CodeBuddy/2.136.0"
	ideVersion     = "2.133.1"
	productVersion = "2.133.1"

	pathChat   = "/v2/chat/completions"
	pathConfig = "/v3/config"

	// modeOAuth marks a stored credential that came from the device login.
	// modeAPIKey marks a stored credential backed by a static API key.
	_ = modeOAuth // redeclared below as authMode-typed values

	// refreshMargin is how long before expiry a token is renewed.
	refreshMargin = 60 * time.Second

	pathAuthState    = "/v2/plugin/auth/state"
	pathAuthToken    = "/v2/plugin/auth/token"
	pathAuthRefresh  = "/v2/plugin/auth/token/refresh"
	pathLoginAccount = "/v2/plugin/login/account"

	// authPendingCode is the gateway's "login still in progress" code, answered
	// for a pending, a fabricated, and an expired state alike.
	authPendingCode = 11217

	// oauthTimeout bounds one device login; the gateway offers no earlier
	// signal that a state went stale.
	oauthTimeout = 10 * time.Minute

	// defaultKeyCooldown is how long a failed API key sits out before it is
	// retried by the rotation.
	defaultKeyCooldown = 60 * time.Second

	// defaultCacheTTL is how long a catalog snapshot is reused.
	defaultCacheTTL = 5 * time.Minute
)

// authMode selects where a request's credential comes from.
type authMode string

const (
	modeAuto   authMode = "auto"
	modeAPIKey authMode = "api-key"
	modeOAuth  authMode = "oauth"
)

// appVersion resolves the desktop app version used in the user agent.
func (p *pluginState) appVersion(kind *credentialKind) string {
	if version := appVersionFromBundle(kind); version != "" {
		return version
	}
	return kind.fallbackAppVersion
}

// chatHeaders builds the identity header set for a chat completion call. The
// missing user/enterprise/domain headers are replaced by their explicit
// "absent" markers rather than omitted, because the gateway rejects a request
// where they are simply missing.
func chatHeaders(kind *credentialKind, creds *credentials, appVersion, cliVersion string) http.Header {
	headers := http.Header{}
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("X-Requested-With", "XMLHttpRequest")
	headers.Set("Origin", kind.origin)
	headers.Set("Referer", kind.origin+"/")
	headers.Set("Content-Type", "application/json")
	headers.Set("X-IDE-Type", "WorkBuddy")
	headers.Set("X-IDE-Name", "WorkBuddy")
	headers.Set("X-IDE-Version", appVersion)
	headers.Set("X-Product", "SaaS")
	headers.Set("User-Agent", desktopUA(kind.productToken, appVersion, cliVersion))

	bearer := creds.AccessToken
	if creds.APIKey != "" {
		bearer = creds.APIKey
	}
	headers.Set("Authorization", "Bearer "+bearer)

	if creds.UID != "" {
		headers.Set("X-User-Id", creds.UID)
	} else {
		headers.Set("X-No-User-Id", "1")
	}
	if creds.isEnterprise() {
		headers.Set("X-Enterprise-Id", creds.EnterpriseID)
		headers.Set("X-Tenant-Id", creds.EnterpriseID)
	} else {
		headers.Set("X-No-Enterprise-Id", "1")
	}
	if creds.Domain != "" {
		headers.Set("X-Domain", creds.Domain)
	} else {
		headers.Set("X-No-Department-Info", "1")
	}
	return headers
}
