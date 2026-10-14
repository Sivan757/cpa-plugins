package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// openAPIHeaders builds the header set for the openapi face.
//
// This face authenticates with a plain bearer and does not use the COSY
// signature. Its Cosy-Version carries the protocol version, which is a
// different constant from the IDE version the signed face advertises.
func openAPIHeaders(token string) http.Header {
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", userAgent)
	headers.Set("Cosy-Version", cosyProtocolVersion)
	headers.Set("Cosy-Clienttype", clientType)
	if strings.TrimSpace(token) != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	return headers
}

// openAPIJSON performs one request against the plain-bearer openapi face.
func openAPIJSON(ctx context.Context, reg *region, method, path, token string, body any, out any) (*cpakit.HTTPResult, error) {
	url := strings.TrimSuffix(reg.openAPI, "/") + path
	return cpakit.HTTPJSON(ctx, method, url, openAPIHeaders(token), body, out)
}

// signedRequest performs one COSY-signed request.
//
// body must already be in its wire form, because the signature and Cosy-Bodyhash
// cover exactly these bytes.
func signedRequest(ctx context.Context, reg *region, method, rawURL string, identity cosyIdentity, body []byte, extra http.Header) (*cpakit.HTTPResult, error) {
	headers, errHeaders := buildCosyHeaders(rawURL, body, identity)
	if errHeaders != nil {
		return nil, cpakit.NewError("invalid_credential", errHeaders.Error(), http.StatusUnauthorized)
	}
	for name, values := range extra {
		for _, value := range values {
			headers.Set(name, value)
		}
	}
	result, errDo := cpakit.HTTPDo(ctx, method, rawURL, headers, body)
	if errDo != nil {
		return nil, cpakit.NewError("upstream_unavailable", "Qoder request failed: "+errDo.Error(), http.StatusBadGateway)
	}
	return result, nil
}

// classifyOpenAPIError maps a plain-bearer failure onto a plugin error.
//
// Authentication and quota problems must keep their HTTP status: collapsing them
// to 500 makes the host report a credential fault as a gateway fault.
func classifyOpenAPIError(status int, operation string, body []byte) *cpakit.PluginError {
	detail := truncateText(body, 300)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return cpakit.NewError("invalid_credential",
			fmt.Sprintf("Qoder rejected the credential during %s (HTTP %d): %s", operation, status, detail), status)
	case status == http.StatusTooManyRequests:
		return cpakit.NewError("rate_limited",
			fmt.Sprintf("Qoder rate limited %s: %s", operation, detail), status)
	case status >= 500:
		return cpakit.NewError("upstream_unavailable",
			fmt.Sprintf("Qoder %s failed upstream (HTTP %d): %s", operation, status, detail), http.StatusBadGateway)
	default:
		return cpakit.NewError("upstream_error",
			fmt.Sprintf("Qoder %s returned HTTP %d: %s", operation, status, detail), status)
	}
}
