package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Dialing through a proxy needs the plugin's own transport: the host bridge
// carries no proxy setting, so a proxied request cannot go through it. A request
// with no proxy keeps using the host bridge, which keeps the host's request log
// and proxy policy in the path.

// proxyClients caches one client per proxy URL. A client pools connections, so
// rebuilding it per request would reconnect every time.
type proxyClientCache struct {
	mu      sync.Mutex
	clients map[string]*http.Client
}

var upstreamClients = proxyClientCache{clients: map[string]*http.Client{}}

// proxyHTTPClient returns a client that egresses through proxyURL.
func proxyHTTPClient(proxyURL string) (*http.Client, error) {
	upstreamClients.mu.Lock()
	defer upstreamClients.mu.Unlock()
	if client, ok := upstreamClients.clients[proxyURL]; ok {
		return client, nil
	}
	parsed, errParse := url.Parse(proxyURL)
	if errParse != nil {
		return nil, errParse
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("proxy URL %q has no host", proxyURL)
	}
	transport := &http.Transport{
		Proxy:               http.ProxyURL(parsed),
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   proxyDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// A proxy that stalls before the response head is abandoned. The body is
		// left unbounded for the streaming path, which a long generation needs;
		// the buffered path bounds itself separately.
		ResponseHeaderTimeout: proxyHeadTimeout,
	}
	client := &http.Client{Transport: transport}
	upstreamClients.clients[proxyURL] = client
	return client, nil
}

// bufferedTimeoutContext bounds one whole buffered answer.
func bufferedTimeoutContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, proxyBufferedTimeout)
}

// sendThrough performs one request on the given client.
func sendThrough(ctx context.Context, client *http.Client, endpoint string, headers http.Header, body []byte) (*http.Response, error) {
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errRequest != nil {
		return nil, errRequest
	}
	request.Header = headers.Clone()
	if request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return client.Do(request)
}

// directStream is a live upstream body from the plugin's own transport.
type directStream struct {
	response *http.Response
	// StatusCode and Header describe the response head.
	StatusCode int
	Header     http.Header
	closed     sync.Once
}

// Read returns the next body fragment. The boolean reports end of stream.
func (s *directStream) Read() ([]byte, bool, error) {
	if s == nil || s.response == nil || s.response.Body == nil {
		return nil, true, nil
	}
	buffer := make([]byte, 32*1024)
	count, errRead := s.response.Body.Read(buffer)
	if count > 0 {
		return buffer[:count], false, nil
	}
	if errRead == io.EOF {
		return nil, true, nil
	}
	if errRead != nil {
		return nil, true, errRead
	}
	return nil, false, nil
}

// Close releases the connection. It is safe to call more than once.
func (s *directStream) Close() {
	if s == nil || s.response == nil || s.response.Body == nil {
		return
	}
	s.closed.Do(func() { _ = s.response.Body.Close() })
}

// openThrough starts a request on the given client and returns its live body. A
// non-2xx head is read into the buffer, because a finite error page has to be
// classified before the caller decides to try the next proxy.
func openThrough(ctx context.Context, client *http.Client, endpoint string, headers http.Header, body []byte) (*directStream, []byte, error) {
	response, errDo := sendThrough(ctx, client, endpoint, headers, body)
	if errDo != nil {
		return nil, nil, errDo
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return &directStream{response: response, StatusCode: response.StatusCode, Header: response.Header}, nil, nil
	}
	raw, errRead := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	status := response.StatusCode
	header := response.Header
	_ = response.Body.Close()
	if errRead != nil {
		return nil, nil, errRead
	}
	return &directStream{StatusCode: status, Header: header}, raw, nil
}

// drainThrough reads a whole body from the client's transport.
func drainThrough(ctx context.Context, client *http.Client, endpoint string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	ctx, cancel := bufferedTimeoutContext(ctx)
	defer cancel()
	response, errDo := sendThrough(ctx, client, endpoint, headers, body)
	if errDo != nil {
		return 0, nil, nil, errDo
	}
	defer response.Body.Close()
	raw, errRead := io.ReadAll(io.LimitReader(response.Body, 16*1024*1024))
	if errRead != nil {
		return response.StatusCode, response.Header, raw, errRead
	}
	return response.StatusCode, response.Header, raw, nil
}

// proxyIsDirect reports whether a lane has no proxy to dial through.
func proxyIsDirect(proxyURL string) bool {
	return strings.TrimSpace(proxyURL) == ""
}
