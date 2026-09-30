package pluginkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPResult is a fully buffered HTTP response.
type HTTPResult struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// httpDoRequest mirrors the host callback payload for host.http.do.
type httpDoRequest struct {
	HostCallbackID string              `json:"host_callback_id,omitempty"`
	Method         string              `json:"method,omitempty"`
	URL            string              `json:"url,omitempty"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
}

// httpDoResponse mirrors pluginapi.HTTPResponse, which carries no JSON tags.
// The wire keys are therefore the exported Go field names, not snake_case —
// requesting snake_case silently yields a zero-valued response.
type httpDoResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

// HTTPDo performs an upstream HTTP request.
//
// Plugin executors are required to route upstream calls through the host client
// so the host can apply its proxy policy and capture request logs. That client
// only exists inside an executor call, which is why HostHTTPDo is used when the
// bridge is available and a local client is the fallback for the model, quota,
// and auth paths that run outside an executor invocation.
func HTTPDo(ctx context.Context, method, url string, headers http.Header, body []byte) (*HTTPResult, error) {
	if HostAvailable() {
		req := httpDoRequest{
			HostCallbackID: HostCallbackID(),
			Method:         method,
			URL:            url,
			Headers:        map[string][]string(headers),
			Body:           body,
		}
		var resp httpDoResponse
		if errCall := callHost(MethodHostHTTPDo, req, &resp); errCall == nil {
			return &HTTPResult{StatusCode: resp.StatusCode, Header: http.Header(resp.Headers), Body: resp.Body}, nil
		}
		// Fall through to the local client so a host bridge change cannot take
		// quota and catalog reads down with it.
	}
	return directHTTPDo(ctx, method, url, headers, body)
}

func directHTTPDo(ctx context.Context, method, url string, headers http.Header, body []byte) (*HTTPResult, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, url, reader)
	if errRequest != nil {
		return nil, fmt.Errorf("build request: %w", errRequest)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 90 * time.Second}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("http %s %s: %w", method, url, errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("read body %s: %w", url, errRead)
	}
	return &HTTPResult{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: raw}, nil
}

// HTTPJSON performs a JSON request and decodes the response into out.
func HTTPJSON(ctx context.Context, method, url string, headers http.Header, requestBody any, out any) (*HTTPResult, error) {
	var payload []byte
	if requestBody != nil {
		encoded, errMarshal := json.Marshal(requestBody)
		if errMarshal != nil {
			return nil, fmt.Errorf("marshal request body: %w", errMarshal)
		}
		payload = encoded
	}
	if headers == nil {
		headers = http.Header{}
	}
	if headers.Get("Content-Type") == "" && len(payload) > 0 {
		headers.Set("Content-Type", "application/json")
	}
	result, errDo := HTTPDo(ctx, method, url, headers, payload)
	if errDo != nil {
		return nil, errDo
	}
	if out != nil && len(result.Body) > 0 {
		if errUnmarshal := json.Unmarshal(result.Body, out); errUnmarshal != nil {
			return result, fmt.Errorf("decode %s: %w (body: %s)", url, errUnmarshal, truncate(result.Body, 400))
		}
	}
	return result, nil
}

func truncate(raw []byte, limit int) string {
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "..."
}
