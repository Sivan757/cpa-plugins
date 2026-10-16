package main

import (
	"context"
	"net/http"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// maxProxyAttempts bounds how many proxies one request may burn through before
// it gives up on the pool. Each attempt is a fresh source address, so this is
// also how many allowances one request may reach for.
const maxProxyAttempts = 4

// laneOutcome is one lane attempt's result: either a buffered body or a live
// stream, plus the transport failure when the attempt never got that far.
type laneOutcome struct {
	status int
	header http.Header
	body   []byte
	stream cpakit.StreamSource
	// proxy is the source address the attempt egressed through, empty for a
	// direct connection.
	proxy string
	err   error
}

// ok reports whether the attempt produced a usable answer.
func (o *laneOutcome) ok() bool {
	return o != nil && o.err == nil && o.status >= 200 && o.status < 300
}

// attemptHost sends one attempt through the host bridge.
//
// The bridge applies the host's transport policy and records the request, so a
// lane with no proxy keeps using it. A streaming attempt reads a non-2xx head
// fully, because a finite error page has to be classified before the caller
// decides whether to retry.
func (p *pluginState) attemptHost(ctx context.Context, plan *executionPlan, lane executionLane, seed string, body []byte, streaming bool, callback cpakit.HostCallback) *laneOutcome {
	headers := p.requestHeaders(plan, lane, seed)
	if !streaming {
		result, errDo := cpakit.HTTPDo(ctx, http.MethodPost, plan.endpoint, headers, body)
		if errDo != nil {
			return &laneOutcome{err: errDo}
		}
		return &laneOutcome{status: result.StatusCode, header: result.Header, body: result.Body}
	}
	stream, errOpen := callback.OpenHTTPStream(ctx, http.MethodPost, plan.endpoint, headers, body)
	if errOpen != nil {
		return &laneOutcome{err: errOpen}
	}
	if stream.StatusCode >= 200 && stream.StatusCode < 300 {
		return &laneOutcome{status: stream.StatusCode, header: stream.Header, stream: stream}
	}
	raw, errRead := cpakit.ReadStreamBody(stream, 0)
	status := stream.StatusCode
	header := stream.Header
	stream.Close()
	if errRead != nil {
		return &laneOutcome{err: errRead}
	}
	return &laneOutcome{status: status, header: header, body: raw}
}

// attemptKeyless tries the keyless lane, rotating through the proxy pool.
//
// Every proxy is a different source address and the tier meters its allowance by
// address, so a spent proxy is left for the next one instead of ending the
// request. A proxy that answers 200 is used; one that fails in transport is
// retired after enough failures; one that reports a drained allowance is set
// aside until the reset it states.
//
// The pool is consulted only when it is enabled and holds a usable entry. With
// no usable entry the attempt goes direct through the host bridge, so an empty
// or exhausted pool degrades to the previous behaviour instead of failing.
func (p *pluginState) attemptKeyless(ctx context.Context, plan *executionPlan, lane executionLane, seed string, body []byte, streaming bool, callback cpakit.HostCallback) *laneOutcome {
	if !p.proxyEnabled {
		return p.attemptHost(ctx, plan, lane, seed, body, streaming, callback)
	}
	p.ensureProxyPool(ctx)

	tried := map[string]bool{}
	var last *laneOutcome
	for attempt := 0; attempt < maxProxyAttempts; attempt++ {
		proxyURL := p.proxyFor()
		if proxyURL == "" || tried[proxyURL] {
			break
		}
		tried[proxyURL] = true
		client, errClient := proxyHTTPClient(proxyURL)
		if errClient != nil {
			continue
		}
		headers := p.requestHeaders(plan, lane, seed)
		if streaming {
			stream, raw, errOpen := openThrough(ctx, client, plan.endpoint, headers, body)
			if errOpen != nil {
				p.noteProxy(proxyURL, 0, nil, true)
				last = &laneOutcome{err: errOpen, proxy: proxyURL}
				continue
			}
			if stream.StatusCode >= 200 && stream.StatusCode < 300 {
				return &laneOutcome{status: stream.StatusCode, header: stream.Header, stream: stream, proxy: proxyURL}
			}
			p.noteProxy(proxyURL, stream.StatusCode, raw, false)
			if isFreeUsageLimit(stream.StatusCode, raw) {
				p.markProxyDrained(proxyURL, freeUsageLimitUntil(stream.Header, time.Now()))
			}
			last = &laneOutcome{status: stream.StatusCode, header: stream.Header, body: raw, proxy: proxyURL}
			continue
		}
		status, header, raw, errDo := drainThrough(ctx, client, plan.endpoint, headers, body)
		if errDo != nil {
			p.noteProxy(proxyURL, 0, nil, true)
			last = &laneOutcome{err: errDo, proxy: proxyURL}
			continue
		}
		if status >= 200 && status < 300 {
			return &laneOutcome{status: status, header: header, body: raw, proxy: proxyURL}
		}
		p.noteProxy(proxyURL, status, raw, false)
		if isFreeUsageLimit(status, raw) {
			p.markProxyDrained(proxyURL, freeUsageLimitUntil(header, time.Now()))
		}
		last = &laneOutcome{status: status, header: header, body: raw, proxy: proxyURL}
	}

	if last != nil {
		return last
	}
	return p.attemptHost(ctx, plan, lane, seed, body, streaming, callback)
}

// attemptLane dispatches one lane to the right transport.
func (p *pluginState) attemptLane(ctx context.Context, plan *executionPlan, lane executionLane, seed string, body []byte, streaming bool, callback cpakit.HostCallback) *laneOutcome {
	if lane.anonymous {
		return p.attemptKeyless(ctx, plan, lane, seed, body, streaming, callback)
	}
	return p.attemptHost(ctx, plan, lane, seed, body, streaming, callback)
}
