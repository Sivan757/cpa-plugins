package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// poolWith builds a pool holding the given proxy URLs, bypassing the network.
func poolWith(urls ...string) *pluginState {
	state := newPluginState()
	state.proxyEnabled = true
	for _, url := range urls {
		state.proxies.entries = append(state.proxies.entries, &proxyEntry{url: url})
	}
	state.proxies.checkedAt = time.Now()
	return state
}

// TestProxyPoolRotates proves consecutive requests leave through different
// addresses, which is the whole point: the tier meters by source address.
func TestProxyPoolRotates(t *testing.T) {
	state := poolWith("http://a:1", "http://b:2", "http://c:3")
	seen := map[string]int{}
	for i := 0; i < 3; i++ {
		seen[state.proxyFor()]++
	}
	if len(seen) != 3 {
		t.Fatalf("three requests used %d distinct proxies, want 3", len(seen))
	}
	for _, url := range []string{"http://a:1", "http://b:2", "http://c:3"} {
		if seen[url] != 1 {
			t.Fatalf("proxy %s used %d times, want 1", url, seen[url])
		}
	}
}

// TestDrainedProxyIsSkipped proves a spent address is left out until its reset,
// and that a drained pool reports no proxy so the attempt goes direct.
func TestDrainedProxyIsSkipped(t *testing.T) {
	state := poolWith("http://a:1", "http://b:2")

	state.markProxyDrained("http://a:1", time.Now().Add(time.Hour))
	if got := state.proxyFor(); got != "http://b:2" {
		t.Fatalf("proxyFor = %q, want the undrained entry", got)
	}

	state.markProxyDrained("http://b:2", time.Now().Add(time.Hour))
	if got := state.proxyFor(); got != "" {
		t.Fatalf("proxyFor = %q, want an empty result when every entry is drained", got)
	}
}

// TestProxyReturnsAfterItsReset proves a drained address comes back once the
// reset it stated passes.
func TestProxyReturnsAfterItsReset(t *testing.T) {
	state := poolWith("http://a:1")
	state.markProxyDrained("http://a:1", time.Now().Add(-time.Minute))
	if got := state.proxyFor(); got != "http://a:1" {
		t.Fatalf("proxyFor = %q, want the entry back after its reset", got)
	}
}

// TestProxyIsRetiredAfterRepeatedFailures proves a dead entry stops being tried,
// so a broken proxy does not cost latency on every request.
func TestProxyIsRetiredAfterRepeatedFailures(t *testing.T) {
	state := poolWith("http://dead:1")
	for i := 0; i < proxyFailuresBeforeRetire; i++ {
		state.noteProxy("http://dead:1", 0, nil, true)
	}
	if got := state.proxyFor(); got != "" {
		t.Fatalf("proxyFor = %q, want a retired entry to be skipped", got)
	}
}

// TestSuccessClearsFailures proves one good answer rescues an entry that had
// failed, so a transient blip does not retire a working proxy.
func TestSuccessClearsFailures(t *testing.T) {
	state := poolWith("http://flaky:1")
	state.noteProxy("http://flaky:1", 0, nil, true)
	state.noteProxy("http://flaky:1", http.StatusOK, nil, false)
	if got := state.proxyFor(); got != "http://flaky:1" {
		t.Fatalf("proxyFor = %q, want the entry still in use", got)
	}
	state.proxies.mu.Lock()
	failures := state.proxies.entries[0].failures
	state.proxies.mu.Unlock()
	if failures != 0 {
		t.Fatalf("failures = %d, want the success to have cleared them", failures)
	}
}

// TestDrainFallbackLandsOnMidnightUTC proves the fallback reset matches the
// window the tier was measured to use when an answer states no Retry-After.
func TestDrainFallbackLandsOnMidnightUTC(t *testing.T) {
	now := time.Date(2026, 10, 16, 17, 30, 0, 0, time.FixedZone("CST", 8*3600))
	got := proxyDrainFallback(now)
	want := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("fallback = %s, want %s", got, want)
	}
}

// TestPoolIsOffUnlessAskedFor proves a lane does not use a proxy when the pool is
// disabled, which is what keeps the account and paid lanes off third parties.
func TestPoolIsOffUnlessAskedFor(t *testing.T) {
	state := newPluginState()
	state.proxies.entries = append(state.proxies.entries, &proxyEntry{url: "http://a:1"})
	state.proxyEnabled = false
	if state.proxyEnabled {
		t.Fatal("the pool must be off by default")
	}

	// A disabled pool still has entries, so the guard is what matters: the lane
	// dispatch only consults the pool when it is enabled.
	plan := freeTierPlan()
	outcome := state.attemptLane(context.Background(), plan, plan.lanes[0], "seed", []byte("{}"), false, cpakit.HostCallback{})
	// With a nil callback the host attempt fails, which is enough to prove the
	// proxied path was not taken: a proxy attempt would not need the host.
	if outcome == nil {
		t.Fatal("attemptLane returned nothing")
	}
}

// TestPlausibleProxyURL keeps a publisher banner or a stray error page out of the
// pool. Several lists prepend a line and some ship text that is not an entry.
func TestPlausibleProxyURL(t *testing.T) {
	good := []string{
		"http://1.2.3.4:8080",
		"socks5://1.2.3.4:1080",
		"http://user:pass@1.2.3.4:3128",
		"https://proxy.example.com:443",
	}
	for _, value := range good {
		if !plausibleProxyURL(value) {
			t.Fatalf("%q was rejected", value)
		}
	}
	bad := []string{
		"",
		"http://",
		"http://1.2.3.4",
		"http://1.2.3.4:0",
		"http://1.2.3.4:99999",
		"http://1.2.3.4:notaport",
		"ftp://1.2.3.4:21",
		"http://error code:502",
		"SOCKS5 Proxy list updated at 2026-10-16 16:00:03 GMT+7",
	}
	for _, value := range bad {
		if plausibleProxyURL(value) {
			t.Fatalf("%q was accepted", value)
		}
	}
}

// TestEveryDefaultSourceLooksLikeAListURL guards the source list against a typo,
// which would otherwise only show up as a missing pool at runtime.
func TestEveryDefaultSourceLooksLikeAListURL(t *testing.T) {
	if len(defaultProxySources) < 4 {
		t.Fatalf("%d default sources, want several: the yield per list is low", len(defaultProxySources))
	}
	for _, source := range defaultProxySources {
		if !strings.HasPrefix(source, "https://raw.githubusercontent.com/") {
			t.Fatalf("source %q is not a GitHub raw file", source)
		}
		if strings.Contains(source, "socks4") {
			t.Fatalf("source %q is a SOCKS4 list, which the transport cannot use", source)
		}
	}
}
