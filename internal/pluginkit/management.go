package pluginkit

import (
	"context"
	"net/http"
	"net/url"
)

// ManagementRegistrationRequest mirrors pluginapi.ManagementRegistrationRequest.
type ManagementRegistrationRequest struct {
	Plugin           Metadata
	BasePath         string
	ResourceBasePath string
}

// ManagementRegistrationResponse mirrors pluginapi.ManagementRegistrationResponse.
type ManagementRegistrationResponse struct {
	Routes    []ManagementRoute
	Resources []ResourceRoute
}

// ManagementRoute mirrors pluginapi.ManagementRoute. Path is resolved under the
// plugin Management API prefix the host passes in ManagementRegistrationRequest.
type ManagementRoute struct {
	Method      string
	Path        string
	Menu        string
	Description string
}

// ResourceRoute mirrors pluginapi.ResourceRoute. Resource routes are served
// under /v0/resource/plugins/<pluginID>/ and are not management-authenticated.
type ResourceRoute struct {
	Path        string
	Menu        string
	Description string
}

// ManagementRequest mirrors pluginapi.ManagementRequest.
type ManagementRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
	Body    []byte
}

// ManagementResponse mirrors pluginapi.ManagementResponse.
type ManagementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// ManagementAPI exposes plugin-owned diagnostics over the Management API.
type ManagementAPI interface {
	RegisterManagement(ctx context.Context, req ManagementRegistrationRequest) (ManagementRegistrationResponse, error)
	HandleManagement(ctx context.Context, req ManagementRequest) (ManagementResponse, error)
}

// JSONManagementResponse is a helper for management route handlers.
func JSONManagementResponse(status int, body []byte) (ManagementResponse, error) {
	return ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}, nil
}
