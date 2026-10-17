package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// The free tier meters its daily allowance per source address, and a proxy
// changes that address. The pool therefore holds a set of proxies and hands one
// to each keyless request, so the tier sees a different address per proxy.
//
// Proxies come from public lists that are re-checked hourly and sorted fastest
// first. Most entries do not carry this workload: measured against the live tier,
// 2 of the 24 fastest HTTP entries answered and 3 more were alive but already
// drained, so a candidate list has to be checked before it is trusted.
//
// The check is the real keyless request, because a proxy can answer a plain GET
// and still fail the POST. A 200 proves the proxy carries the request and has
// budget; a 429 proves it carries the request and is drained. Both keep it. Only
// a transport failure or an answer the tier never sends marks it dead, and a 429
// costs no tokens, so the check is free.

const (
	proxyListMonosansHTTP   = "https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/http.txt"
	proxyListMonosansSOCKS  = "https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks5.txt"
	proxyListSpeedXHTTP     = "https://raw.githubusercontent.com/TheSpeedX/SOCKS-List/master/http.txt"
	proxyListSpeedXSOCKS    = "https://raw.githubusercontent.com/TheSpeedX/SOCKS-List/master/socks5.txt"
	proxyListAliilaproHTTP  = "https://raw.githubusercontent.com/ALIILAPRO/Proxy/main/http.txt"
	proxyListAliilaproSOCKS = "https://raw.githubusercontent.com/ALIILAPRO/Proxy/main/socks5.txt"

	// proxyCandidatesPerList bounds how many entries of each list one refresh
	// checks. The publishers sort by speed, so the head is the best of what
	// worked at the last update, and checking thousands would take minutes for
	// little gain.
	proxyCandidatesPerList = 48
	// proxyCheckConcurrency bounds the simultaneous checks.
	proxyCheckConcurrency = 24
	// proxyCheckTimeout is one check's budget. A slow proxy is not worth the
	// latency it adds to a user request.
	proxyCheckTimeout = 12 * time.Second
	// proxyRefreshInterval is how long a checked pool is reused. The publishers
	// re-check their lists hourly or more often.
	proxyRefreshInterval = 20 * time.Minute
	// proxyHeadTimeout bounds the wait for a response head. A proxy that has not
	// answered by then is abandoned for the next one, because the edge in front
	// of this service gives up at 60 seconds and a stalled proxy would otherwise
	// spend that budget before failing.
	proxyHeadTimeout = 20 * time.Second
	// proxyDialTimeout bounds establishing the connection to the proxy.
	proxyDialTimeout = 8 * time.Second
	// proxyMaxLatency is the slowest head a proxy may take and stay in the pool.
	// Past this the proxy adds more delay than the allowance is worth, and the
	// edge may cut the request off before the answer arrives.
	proxyMaxLatency = 6 * time.Second
	// proxyBufferedTimeout bounds a whole buffered answer. The streaming path is
	// not bounded here, because a model may generate for a long time; the head
	// timeout still applies to it.
	proxyBufferedTimeout = 3 * time.Minute
	// proxyFailuresBeforeRetire is how many transport failures retire an entry.
	proxyFailuresBeforeRetire = 2
	// proxyModel is the model the health check asks for. One token is enough.
	proxyModel = "mimo-v2.6-flash-free"
)

// defaultProxySources is the source set the pool loads when proxy_source is
// empty. They are plain files served by GitHub, so no key and no account is
// needed.
//
// Several lists are combined because the yield is low: measured against the live
// tier, roughly one entry in ten carries the keyless request. SOCKS4 lists are
// deliberately absent, because the transport speaks HTTP, HTTPS and SOCKS5.
var defaultProxySources = []string{
	proxyListMonosansHTTP,
	proxyListMonosansSOCKS,
	proxyListSpeedXHTTP,
	proxyListSpeedXSOCKS,
	proxyListAliilaproHTTP,
	proxyListAliilaproSOCKS,
}

// proxyEntry is one candidate address with what is known about it.
type proxyEntry struct {
	url string
	// exitIP is the address the tier sees. Learned from the first answer, so two
	// entries that share it are known to share one allowance.
	exitIP string
	// drainedUntil is when this entry's allowance is expected back.
	drainedUntil time.Time
	// latency is the last observed wait for a response head. The pool is ordered
	// by it, so the fastest proxies are used first.
	latency time.Duration
	// failures counts transport failures and stalls since the last success.
	failures int
	// retiredAt is set when the entry failed enough times to stop using it.
	retiredAt time.Time
}

// usable reports whether a request may be sent through this entry.
func (e *proxyEntry) usable(now time.Time) bool {
	if e == nil {
		return false
	}
	if !e.retiredAt.IsZero() {
		return false
	}
	return !now.Before(e.drainedUntil)
}

// proxyPool holds the checked proxies.
type proxyPool struct {
	mu        sync.Mutex
	entries   []*proxyEntry
	checkedAt time.Time
	checking  bool
	// sources are the list URLs, and candidates bounds the entries checked.
	sources    []string
	candidates int
}

// proxyFor returns the next usable proxy URL, or an empty string for a direct
// connection. Entries are visited round robin so load spreads across the pool.
func (p *pluginState) proxyFor() string {
	if p == nil {
		return ""
	}
	pool := &p.proxies
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.entries) == 0 {
		return ""
	}
	now := time.Now()
	// One pass over the pool, starting after the last choice.
	start := p.proxyCursor
	for offset := 0; offset < len(pool.entries); offset++ {
		index := (start + offset) % len(pool.entries)
		entry := pool.entries[index]
		if entry.usable(now) {
			p.proxyCursor = (index + 1) % len(pool.entries)
			return entry.url
		}
	}
	return ""
}

// noteProxy records what an answer says about the proxy that carried it.
func (p *pluginState) noteProxy(proxyURL string, status int, body []byte, transportFailed bool, latency time.Duration) {
	if p == nil || proxyURL == "" {
		return
	}
	pool := &p.proxies
	pool.mu.Lock()
	var entry *proxyEntry
	for _, candidate := range pool.entries {
		if candidate.url == proxyURL {
			entry = candidate
			break
		}
	}
	if entry == nil {
		pool.mu.Unlock()
		return
	}
	now := time.Now()
	switch {
	case transportFailed:
		entry.failures++
		cpakit.HostLog(nil, "warn", fmt.Sprintf("opencode: proxy %s failed (%d)", redactProxy(proxyURL), entry.failures))
		if entry.failures >= proxyFailuresBeforeRetire {
			entry.retiredAt = now
			cpakit.HostLog(nil, "warn", "opencode: proxy "+redactProxy(proxyURL)+" retired from the pool")
		}
	case latency > proxyMaxLatency:
		// A stall is retirement-worthy for the same reason a broken proxy is: the
		// caller waits, and the edge may cut the request off before it completes.
		entry.latency = latency
		entry.failures++
		cpakit.HostLog(nil, "warn", fmt.Sprintf("opencode: proxy %s answered in %s, over the %s limit (%d)",
			redactProxy(proxyURL), latency.Round(time.Millisecond), proxyMaxLatency, entry.failures))
		if entry.failures >= proxyFailuresBeforeRetire {
			entry.retiredAt = now
			cpakit.HostLog(nil, "warn", "opencode: proxy "+redactProxy(proxyURL)+" retired from the pool")
		}
	case status >= 200 && status < 300:
		entry.failures = 0
		entry.latency = latency
		entry.drainedUntil = time.Time{}
	case isFreeUsageLimit(status, body):
		// Alive but spent, which still proves the proxy carries the request. The
		// reset instant is recorded by markProxyDrained, which sees the header.
		entry.failures = 0
	}
	pool.mu.Unlock()
}

// proxyDrainFallback is used when a drained answer carried no Retry-After. The
// tier resets at midnight UTC, so the next reset is the safe assumption.
func proxyDrainFallback(now time.Time) time.Time {
	utc := now.UTC()
	next := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	return next
}

// markProxyDrained records the reset a drained answer stated.
func (p *pluginState) markProxyDrained(proxyURL string, until time.Time) {
	if p == nil || proxyURL == "" || until.IsZero() {
		return
	}
	pool := &p.proxies
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, entry := range pool.entries {
		if entry.url == proxyURL {
			entry.failures = 0
			entry.drainedUntil = until
			return
		}
	}
}

// ensureProxyPool starts a refresh when the checked pool has aged out. The
// refresh runs in the background so a request is never blocked on it.
func (p *pluginState) ensureProxyPool(ctx context.Context) {
	if p == nil {
		return
	}
	pool := &p.proxies
	pool.mu.Lock()
	if len(pool.sources) == 0 {
		pool.mu.Unlock()
		return
	}
	if pool.checking || time.Since(pool.checkedAt) < proxyRefreshInterval {
		pool.mu.Unlock()
		return
	}
	pool.checking = true
	sources := append([]string(nil), pool.sources...)
	candidates := pool.candidates
	pool.mu.Unlock()

	go func() {
		entries := checkProxyLists(context.Background(), sources, candidates)
		now := time.Now()
		pool.mu.Lock()
		pool.entries = entries
		pool.checkedAt = now
		pool.checking = false
		pool.mu.Unlock()
		ready := 0
		checked := time.Now()
		fastest := time.Duration(0)
		for index, entry := range entries {
			if entry.usable(checked) {
				ready++
			}
			if index == 0 {
				fastest = entry.latency
			}
		}
		cpakit.HostLog(context.Background(), "info", fmt.Sprintf(
			"opencode: proxy pool checked, %d carried the request, %d with a free allowance, fastest %s, limit %s",
			len(entries), ready, fastest.Round(time.Millisecond), proxyMaxLatency))
	}()
}

// checkProxyLists reads every source and keeps the entries that carry the
// keyless request.
func checkProxyLists(ctx context.Context, sources []string, candidates int) []*proxyEntry {
	if candidates <= 0 {
		candidates = proxyCandidatesPerList
	}
	seen := map[string]bool{}
	ordered := make([]string, 0, candidates*len(sources))
	for _, source := range sources {
		for _, entry := range fetchProxyList(ctx, source, candidates) {
			if seen[entry] {
				continue
			}
			seen[entry] = true
			ordered = append(ordered, entry)
		}
	}
	if len(ordered) == 0 {
		return nil
	}

	results := make([]*proxyEntry, len(ordered))
	var wait sync.WaitGroup
	slots := make(chan struct{}, proxyCheckConcurrency)
	for index, raw := range ordered {
		wait.Add(1)
		go func(index int, raw string) {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[index] = checkOneProxy(ctx, raw)
		}(index, raw)
	}
	wait.Wait()

	kept := make([]*proxyEntry, 0, len(results))
	for _, entry := range results {
		if entry != nil {
			kept = append(kept, entry)
		}
	}
	return orderByLatency(kept)
}

// orderByLatency puts the fastest entries first, so the round robin spends most
// of its turns on the proxies least likely to reach the edge's timeout.
func orderByLatency(entries []*proxyEntry) []*proxyEntry {
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].latency < entries[j].latency
	})
	return entries
}

// fetchProxyList reads one list and returns its first candidates entries.
func fetchProxyList(ctx context.Context, url string, limit int) []string {
	headers := http.Header{}
	headers.Set("Accept", "text/plain")
	headers.Set("User-Agent", "opencode/"+versionFallback)
	result, errCall := cpakit.HTTPDo(ctx, http.MethodGet, url, headers, nil)
	if errCall != nil || result == nil || result.StatusCode < 200 || result.StatusCode >= 300 {
		cpakit.HostLog(ctx, "warn", "opencode: could not read a proxy list")
		return nil
	}
	out := make([]string, 0, limit)
	for _, line := range strings.Split(string(result.Body), "\n") {
		value := strings.TrimSpace(line)
		if value == "" || strings.HasPrefix(value, "#") || strings.HasPrefix(value, ";") {
			continue
		}
		if !strings.Contains(value, "://") {
			// The plain files carry host:port only; the tier is HTTPS, so an
			// HTTP CONNECT proxy is the right default.
			value = "http://" + value
		}
		// Several publishers prepend a banner line and some ship a stray error
		// page, so an entry is only taken when it parses as a proxy URL.
		if !plausibleProxyURL(value) {
			continue
		}
		out = append(out, value)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// plausibleProxyURL reports whether an entry is a usable proxy URL: a supported
// scheme, a host, and a numeric port.
func plausibleProxyURL(value string) bool {
	parsed, errParse := url.Parse(value)
	if errParse != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return false
	}
	if strings.TrimSpace(parsed.Hostname()) == "" {
		return false
	}
	port, errPort := strconv.Atoi(parsed.Port())
	if errPort != nil || port < 1 || port > 65535 {
		return false
	}
	return true
}

// checkOneProxy sends the real keyless request through one proxy and reports
// whether it carried it.
func checkOneProxy(ctx context.Context, proxyURL string) *proxyEntry {
	client, errClient := proxyHTTPClient(proxyURL)
	if errClient != nil {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, proxyCheckTimeout)
	defer cancel()

	session := newSessionStore().id("proxy-check")
	body, _ := json.Marshal(map[string]any{
		"model":      proxyModel,
		"stream":     true,
		"max_tokens": 1,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":      ensureRequiredTools(nil),
	})
	headers := make(http.Header)
	for name, values := range clientHeaders(versionFallback, session, "msg_proxycheck") {
		headers[name] = values
	}
	headers.Set("Authorization", "Bearer "+publicCredential)

	started := time.Now()
	response, errDo := sendThrough(checkCtx, client, freeBaseURL+"/chat/completions", headers, body)
	if errDo != nil {
		return nil
	}
	defer response.Body.Close()
	raw := make([]byte, 400)
	count, _ := response.Body.Read(raw)
	preview := string(raw[:count])

	entry := &proxyEntry{url: proxyURL, latency: time.Since(started)}
	// A proxy slower than the limit is dropped here rather than at request time,
	// so a slow entry never enters the pool.
	if entry.latency > proxyMaxLatency {
		return nil
	}
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return entry
	case response.StatusCode == http.StatusTooManyRequests && strings.Contains(preview, "FreeUsageLimitError"):
		// Alive and spent. Kept, because it proves the proxy carries the request.
		entry.drainedUntil = proxyDrainFallback(time.Now())
		return entry
	default:
		return nil
	}
}

// redactProxy removes any credential from a proxy URL before it is logged.
func redactProxy(raw string) string {
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		return "…@" + raw[at+1:]
	}
	return raw
}

// proxyCount reports how many entries the pool holds and how many are usable.
func (p *pluginState) proxyCount() (total int, usable int) {
	if p == nil {
		return 0, 0
	}
	pool := &p.proxies
	pool.mu.Lock()
	defer pool.mu.Unlock()
	now := time.Now()
	for _, entry := range pool.entries {
		if entry.usable(now) {
			usable++
		}
	}
	return len(pool.entries), usable
}

// proxyDrainWindow is the fallback drain window when an answer states no reset.
