package main

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// neverExpiresCutoff ignores timestamps far enough in the future to be a
// sentinel rather than a date. The upstream reports "no expiry" as the int64
// maximum (9999-12-31), which would otherwise render as a bogus reset time.
var neverExpiresCutoff = time.Date(2500, 1, 1, 0, 0, 0, 0, time.UTC)

func trimLower(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

// newUUID renders a random version-4 UUID, the identifier shape the upstream
// request envelope uses for its request and session ids.
func newUUID() string {
	var raw [16]byte
	if _, errRead := rand.Read(raw[:]); errRead != nil {
		// A time-derived value is still unique enough for a request id and this
		// path is unreachable on any supported platform.
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// truncateText bounds an upstream body before it is embedded in an error
// message, so a large HTML error page cannot flood the host log.
func truncateText(raw []byte, limit int) string {
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "..."
}

// plausibleTimestamp reports whether a parsed value can be shown to a user as a
// real date rather than an "expires at the end of time" placeholder.
func plausibleTimestamp(value time.Time) bool {
	if value.IsZero() {
		return false
	}
	return value.Before(neverExpiresCutoff)
}

// firstPositive returns the first strictly positive value, or zero when every
// candidate is missing: the host treats a zero limit as "unknown".
func firstPositive(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
