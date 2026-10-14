package main

import (
	"context"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// versionURL reports the newest published OpenCode CLI version. Zen refuses
// every free-tier request below 1.18.0 with HTTP 426, so an unreachable
// registry must fall back to a version that is known to qualify.
const (
	versionURL      = "https://unpkg.com/opencode-ai@latest/package.json"
	versionFallback = "1.19.0"
	minVersionMajor = 1
	minVersionMinor = 18
)

// idAlphabet is the base62 alphabet OpenCode uses for identifier suffixes.
const idAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// idCounterRange bounds the identifier's counter field.
//
// The counter occupies the low twelve bits of the value before the complement,
// and the free tier rejects any identifier whose counter exceeds 63: measured
// against the live endpoint, counters 0..63 answer 200 and 64..4095 answer 403
// with FreeTierError. Drawing a wider range makes most identifiers unusable.
const idCounterRange = 64

// sessionKeep bounds the session-id map. Zen meters free usage per session and
// caches its prompt prefix by that id, so one client conversation must reuse
// the id it was first given.
const sessionKeep = 256

type sessionStore struct {
	mu    sync.Mutex
	ids   map[string]string
	order []string
}

func newSessionStore() *sessionStore {
	return &sessionStore{ids: map[string]string{}}
}

// id returns the identifier minted for seed, minting one on first use.
func (s *sessionStore) id(seed string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if hit, ok := s.ids[seed]; ok {
		return hit
	}
	minted := opencodeID("ses", true)
	s.ids[seed] = minted
	s.order = append(s.order, seed)
	if len(s.order) > sessionKeep {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.ids, oldest)
	}
	return minted
}

// requestID mints a fresh per-turn identifier.
func (s *sessionStore) requestID() string { return opencodeID("msg", false) }

// opencodeID reproduces the OpenCode CLI identifier format: twelve hex digits
// carrying the six most significant bytes of ms times 4096 plus a counter, then
// fourteen base62 characters. Zen decodes the timestamp prefix, so a
// digest-shaped value is refused even when every header is otherwise correct.
func opencodeID(prefix string, descending bool) string {
	return opencodeIDAt(prefix, descending, time.Now().UnixMilli())
}

// opencodeIDAt mints an identifier for an explicit millisecond clock reading, so
// the format can be pinned against an independent implementation in a test.
//
// The twelve leading hex digits are the LOW six bytes of the 64-bit value
// (millisecond * 4096 + counter), most significant byte first, bit-inverted for
// a descending identifier. Extracting the high bytes instead is a live 403.
func opencodeIDAt(prefix string, descending bool, millis int64) string {
	value := uint64(millis)*4096 + uint64(rand.Intn(idCounterRange))
	if descending {
		value = ^value
	}
	var builder strings.Builder
	builder.WriteString(prefix)
	builder.WriteByte('_')
	for index := 5; index >= 0; index-- {
		builder.WriteString(padHex(byte(value >> uint(8*index))))
	}
	for index := 0; index < 14; index++ {
		builder.WriteByte(idAlphabet[rand.Intn(len(idAlphabet))])
	}
	return builder.String()
}

// padHex renders one byte as two lowercase hex digits.
func padHex(value byte) string {
	rendered := strconv.FormatUint(uint64(value), 16)
	if len(rendered) < 2 {
		return "0" + rendered
	}
	return rendered
}

// parseVersion reads major and minor out of a version string.
func parseVersion(raw string) (int, int) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	parts := strings.Split(trimmed, ".")
	if len(parts) < 2 {
		return 0, 0
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if errMajor != nil || errMinor != nil {
		return 0, 0
	}
	return major, minor
}

// qualifies reports whether a version clears the free-tier floor.
func qualifies(version string) bool {
	major, minor := parseVersion(version)
	if major != minVersionMajor {
		return major > minVersionMajor
	}
	return minor >= minVersionMinor
}

// resolveClientVersion reads the newest OpenCode CLI version, falling back to a
// version that clears the free-tier floor.
func resolveClientVersion(ctx context.Context) string {
	var payload map[string]any
	result, errCall := cpakit.HTTPJSON(ctx, http.MethodGet, versionURL,
		http.Header{"Accept": []string{"application/json"}}, nil, &payload)
	if errCall != nil || result == nil || result.StatusCode != http.StatusOK {
		return versionFallback
	}
	if version, ok := payload["version"].(string); ok && qualifies(version) {
		return version
	}
	return versionFallback
}

// clientHeaders builds the identity every Zen request carries.
//
// The free tier gates on this shape: the CLI user agent, a session identifier
// in the OpenCode format, and a per-turn request identifier. The paid tier
// accepts the same headers and adds its API key.
func clientHeaders(version, session, request string) http.Header {
	headers := http.Header{}
	headers.Set("User-Agent", "opencode/"+version)
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	headers.Set("HTTP-Referer", "https://opencode.ai")
	headers.Set("x-opencode-client", "cli")
	headers.Set("x-opencode-project", "global")
	headers.Set("x-opencode-session", session)
	headers.Set("x-opencode-session-id", session)
	headers.Set("x-session-affinity", session)
	headers.Set("X-Session-Id", session)
	headers.Set("x-opencode-request", request)
	return headers
}

// requiredTools names the tools whose presence unlocks the free tier. Zen reads
// the names only; the schemas are never inspected.
var requiredTools = []string{"bash", "read"}

// placeholderTool builds the argument-free stand-in for a required tool.
func placeholderTool(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": "Placeholder tool declaration required by the OpenCode free tier.",
			"parameters": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"required":             []any{},
				"additionalProperties": false,
			},
		},
	}
}

// toolName reads the declared name of one tool entry.
func toolName(entry any) string {
	object, okObject := entry.(map[string]any)
	if !okObject {
		return ""
	}
	if name, _ := object["name"].(string); name != "" {
		return name
	}
	function, okFunction := object["function"].(map[string]any)
	if !okFunction {
		return ""
	}
	name, _ := function["name"].(string)
	return name
}

// ensureRequiredTools appends the placeholder declarations a free-tier request
// is missing, leaving an already-qualifying list untouched so the upstream
// prompt cache is not invalidated.
func ensureRequiredTools(tools []any) []any {
	present := map[string]bool{}
	for _, entry := range tools {
		if name := toolName(entry); name != "" {
			present[name] = true
		}
	}
	filled := tools
	for _, name := range requiredTools {
		if !present[name] {
			filled = append(filled, placeholderTool(name))
		}
	}
	return filled
}

// isPlaceholderTool reports whether a tool call belongs to the padding this
// plugin adds, so its invocation can be dropped from the reply.
func isPlaceholderTool(name string) bool {
	for _, required := range requiredTools {
		if name == required {
			return true
		}
	}
	return false
}
