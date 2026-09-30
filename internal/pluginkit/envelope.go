package pluginkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Envelope is the JSON message exchanged across the C ABI in both directions.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *PluginError    `json:"error,omitempty"`
}

// PluginError mirrors pluginabi.Error. HTTPStatus is required for upstream
// failures: without it the host downgrades the error to HTTP 500 and clients
// treat a credential or quota problem as a gateway fault.
type PluginError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// Error implements the error interface so PluginError can flow through normal
// Go error handling.
func (e *PluginError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// NewError builds a PluginError with an optional HTTP status code.
func NewError(code, message string, httpStatus ...int) *PluginError {
	status := 0
	if len(httpStatus) > 0 {
		status = httpStatus[0]
	}
	return &PluginError{Code: code, Message: message, HTTPStatus: status}
}

// StatusCode lets the dispatcher recover the HTTP status from a wrapped error.
func (e *PluginError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}

// ErrUnhandled reports that this plugin does not implement the requested method.
// The dispatcher turns it into an "unknown_method" envelope.
var ErrUnhandled = errors.New("method not handled by plugin")

// OK marshals a successful envelope whose result is v.
func OK(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal result: %w", errMarshal)
	}
	return json.Marshal(Envelope{OK: true, Result: raw})
}

// OKRaw marshals a successful envelope from an already-encoded result.
func OKRaw(result []byte) ([]byte, error) {
	return json.Marshal(Envelope{OK: true, Result: json.RawMessage(result)})
}

// Fail marshals a failed envelope.
func Fail(code, message string, httpStatus ...int) []byte {
	raw, errMarshal := json.Marshal(Envelope{OK: false, Error: NewError(code, message, httpStatus...)})
	if errMarshal != nil {
		// Fall back to a literal envelope; this cannot realistically fail.
		return []byte(`{"ok":false,"error":{"code":"internal_error","message":"failed to encode error envelope"}}`)
	}
	return raw
}

// statusOf extracts an HTTP status carried by an error, if any.
func statusOf(err error) int {
	var sc interface{ StatusCode() int }
	if errors.As(err, &sc) && sc != nil {
		return sc.StatusCode()
	}
	return 0
}

// codeOf extracts the error code from a PluginError, defaulting sensibly.
func codeOf(err error) string {
	var pe *PluginError
	if errors.As(err, &pe) && pe != nil && pe.Code != "" {
		return pe.Code
	}
	return "plugin_error"
}

// httpStatusText is used in a few diagnostics.
func httpStatusText(code int) string {
	if text := http.StatusText(code); text != "" {
		return text
	}
	return "unknown status"
}
