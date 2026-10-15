package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// Upstream roots, taken from the published Console API documentation.
//
// The free tier and the Go plan are separate gateways. Each keeps its own chat
// base, and the free tier reads its model list from a different path than the one
// it posts chat to. The older /zen/... roots still answer, but they are not the
// documented contract, so the plugin targets the documented ones.
const (
	// freeBaseURL serves free-tier chat completions.
	freeBaseURL = "https://opencode.ai/inference/openai/v1"
	// freeModelsURL lists the models the free tier can reach.
	freeModelsURL = "https://opencode.ai/inference/v1/models"
	// goBaseURL serves the paid Go plan: chat completions and its model list.
	goBaseURL = "https://opencode.ai/inference/go/v1"
	// responsesBaseURL serves the models the tier publishes only on the Responses
	// API. Sending one to /chat/completions is refused upstream.
	responsesBaseURL = "https://opencode.ai/inference/openai/v1/responses"
)

// catalogTTL keeps a fetched catalogue briefly so the model-list request the
// host makes immediately before a chat does not hit the upstream twice.
const catalogTTL = 10 * time.Minute

// Tier names as they appear in an auth record.
const (
	tierFree = "free"
	tierGo   = "go"
)

// publicCredential is the literal bearer the anonymous free tier expects.
const publicCredential = "public"

// modelLimits carries the context window and output cap for a model family.
// Values the upstream catalogue does not publish come from live measurement.
type modelLimit struct {
	context int64
	output  int64
}

var modelLimits = map[string]modelLimit{
	"kimi-k3":              {context: 1048576, output: 131072},
	"mimo-v2.6-flash-free": {context: 1048576, output: 32768},
	"deepseek-v4.1-flash":  {context: 200000, output: 65536},
	// The free variant caps output lower than the paid one, and its context is
	// the same 200k. models.dev publishes 200000/32000.
	"deepseek-v4.1-flash-free":    {context: 200000, output: 32000},
	"deepseek-flash":              {context: 200000, output: 65536},
	"deepseek-v4-pro":             {context: 200000, output: 65536},
	"deepseek-v4-flash":           {context: 200000, output: 65536},
	"space-bunny-free":            {context: 200000, output: 32768},
	"nemotron-3.5-lightning-free": {context: 200000, output: 32768},
}

// defaultLimit applies to a model with no measured entry.
var defaultLimit = modelLimit{context: 200000, output: 32768}

// limitFor resolves the declared limits for one upstream model id.
func limitFor(id string) modelLimit {
	if hit, ok := modelLimits[id]; ok {
		return hit
	}
	return defaultLimit
}

// tierOf reads the tier marker out of an auth record. An unknown or absent
// value is the free tier, which is the only one that needs no credential.
func tierOf(attributes map[string]string, storage []byte) string {
	if attributes != nil {
		if tier := strings.TrimSpace(attributes["tier"]); tier != "" {
			return strings.ToLower(tier)
		}
	}
	var document map[string]any
	if len(storage) > 0 && json.Unmarshal(storage, &document) == nil {
		if tier, ok := document["tier"].(string); ok && strings.TrimSpace(tier) != "" {
			return strings.ToLower(tier)
		}
	}
	return tierFree
}

// keyOf reads the Go-plan API key out of an auth record.
func keyOf(attributes map[string]string, storage []byte) string {
	if attributes != nil {
		if key := strings.TrimSpace(attributes["api_key"]); key != "" {
			return key
		}
	}
	var document map[string]any
	if len(storage) > 0 && json.Unmarshal(storage, &document) == nil {
		if key, ok := document["api_key"].(string); ok {
			return strings.TrimSpace(key)
		}
	}
	return ""
}

// pluginState is the runtime state shared by every capability.
type pluginState struct {
	mu       sync.Mutex
	version  string
	sessions *sessionStore

	// cache holds the last catalogue fetch, keyed by tier.
	cacheMu sync.Mutex
	cache   map[string]cachedCatalog

	// config values, refreshed on every plugin.reconfigure.
	apiKeys     []string
	freeEnabled bool
	// versionResolved records that the published CLI version was discovered, so
	// the lookup runs once rather than on every reconfigure.
	versionResolved bool
	// expose narrows the advertised roster to matching ids. Empty means no
	// restriction.
	expose []string

	// materialized guards the one-shot auth-record creation.
	materialized bool

	// account holds the console session token the free tier now requires, plus
	// the workspace it is metered against.
	account *accountAuth

	// credentialDir overrides where the rotating account credential is
	// persisted. A container needs a mounted path so a recreation keeps it.
	credentialDir string

	// baseURL overrides the free-tier upstream root, for testing against a
	// local capture endpoint.
	baseURL string
	// goURL overrides the paid-tier upstream root.
	goURL string
}

type cachedCatalog struct {
	fetchedAt time.Time
	models    []cpakit.ModelInfo
}

func newPluginState() *pluginState {
	return &pluginState{
		version:     versionFallback,
		sessions:    newSessionStore(),
		cache:       map[string]cachedCatalog{},
		freeEnabled: true,
		account:     &accountAuth{},
	}
}
