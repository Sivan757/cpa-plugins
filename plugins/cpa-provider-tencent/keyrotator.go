package main

import (
	"strings"
	"sync"
	"time"
)

// splitAPIKeys splits a config value into distinct keys. Multiple keys are
// accepted because the gateway rate-limits per key, not per account.
// (Copied from the verified CodeBuddy plugin.)
func splitAPIKeys(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	out := make([]string, 0, len(fields))
	seen := map[string]struct{}{}
	for _, field := range fields {
		key := strings.TrimSpace(field)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

// keyRotator tracks per-key cooldowns and a round-robin cursor.
//
// A cooled-down key is not removed from the rotation: it is moved to the back
// and used when every other candidate is cooling down too, because a refused
// request is still better than no request at all. (Copied from the verified
// CodeBuddy plugin, retargeted from the credential type to credentials.)
type keyRotator struct {
	mu          sync.Mutex
	cursor      int
	cooldownFor time.Duration
	until       map[string]time.Time
}

func newKeyRotator(cooldown time.Duration) *keyRotator {
	return &keyRotator{cooldownFor: cooldown, until: map[string]time.Time{}}
}

func (r *keyRotator) setCooldown(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cooldownFor = d
}

// ordered reorders the candidates for one request: fresh keys first in
// round-robin order, cooled-down keys last as a fallback.
func (r *keyRotator) ordered(creds []*credentials) []*credentials {
	if len(creds) <= 1 {
		return creds
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	start := r.cursor % len(creds)
	r.cursor++
	now := time.Now()
	fresh := make([]*credentials, 0, len(creds))
	cooled := make([]*credentials, 0, len(creds))
	for offset := 0; offset < len(creds); offset++ {
		cred := creds[(start+offset)%len(creds)]
		if until, okUntil := r.until[cred.rotationKey()]; okUntil && now.Before(until) {
			cooled = append(cooled, cred)
			continue
		}
		fresh = append(fresh, cred)
	}
	return append(fresh, cooled...)
}

// penalize cools one credential down after a failure that suggests the key
// itself is the problem.
func (r *keyRotator) penalize(cred *credentials) {
	if r.cooldownFor <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.until == nil {
		r.until = map[string]time.Time{}
	}
	r.until[cred.rotationKey()] = time.Now().Add(r.cooldownFor)
}

// shouldRotateCredential reports the statuses that say "this key cannot serve
// the request" rather than "this request is invalid". (Copied from the
// verified CodeBuddy plugin.)
func shouldRotateCredential(status int) bool {
	switch {
	case status == 401, status == 403, status == 429:
		return true
	case status >= 500:
		return true
	default:
		return false
	}
}
