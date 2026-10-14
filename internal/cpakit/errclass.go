package cpakit

import (
	"net/http"
	"regexp"
	"strings"
)

// Class is a stable error category used as the envelope error code. The host
// maps unknown codes onto its own taxonomy, so these names are descriptive
// rather than load-bearing.
type Class string

const (
	ClassAuth         Class = "auth_failure"
	ClassInvalidModel Class = "invalid_model"
	ClassUnsupported  Class = "unsupported_protocol_or_parameter"
	ClassRateLimit    Class = "rate_limit"
	ClassQuota        Class = "quota_exhaustion"
	ClassBilling      Class = "billing"
	ClassUpstream     Class = "upstream_server_failure"
	ClassNetwork      Class = "timeout_or_network_failure"
	ClassTranslation  Class = "translation_failure"
	ClassInternal     Class = "internal_error"
)

// Error carries a classified failure across the ABI.
type Error struct {
	Class      Class
	Message    string
	StatusCode int
	Retryable  bool
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return string(e.Class) + ": " + e.Message
}

// ClassOf returns the class of an error, or ClassInternal for an unclassified one.
func ClassOf(err error) Class {
	if classified, ok := err.(*Error); ok && classified != nil {
		return classified.Class
	}
	return ClassInternal
}

// FromStatus classifies an upstream HTTP failure.
func FromStatus(status int, message string) *Error {
	redacted := Redact(message)
	lower := strings.ToLower(redacted)
	quota := strings.Contains(lower, "quota") || strings.Contains(lower, "credit")
	withQuota := func(base Class) Class {
		if quota {
			return ClassQuota
		}
		return base
	}
	switch {
	case status == http.StatusUnauthorized:
		return &Error{Class: ClassAuth, Message: redacted, StatusCode: status, Retryable: true}
	case status == http.StatusForbidden:
		return &Error{Class: withQuota(ClassAuth), Message: redacted, StatusCode: status, Retryable: true}
	case status == http.StatusPaymentRequired:
		return &Error{Class: withQuota(ClassBilling), Message: redacted, StatusCode: status, Retryable: true}
	case status == http.StatusNotFound:
		return &Error{Class: ClassInvalidModel, Message: redacted, StatusCode: status}
	case status == http.StatusTooManyRequests:
		return &Error{Class: withQuota(ClassRateLimit), Message: redacted, StatusCode: status, Retryable: true}
	case status >= 500:
		return &Error{Class: ClassUpstream, Message: redacted, StatusCode: status, Retryable: true}
	default:
		return &Error{Class: ClassUnsupported, Message: redacted, StatusCode: status}
	}
}

// FromNetwork classifies a transport failure, which carries no status.
func FromNetwork(err error) *Error {
	if err == nil {
		return nil
	}
	return &Error{Class: ClassNetwork, Message: Redact(err.Error()), Retryable: true}
}

// Translation builds a translation failure.
func Translation(message string) *Error {
	return &Error{Class: ClassTranslation, Message: Redact(message)}
}

// Unsupported builds an unsupported-parameter failure.
func Unsupported(message string) *Error {
	return &Error{Class: ClassUnsupported, Message: Redact(message), StatusCode: http.StatusBadRequest}
}

// Internal builds an internal failure.
func Internal(message string) *Error {
	return &Error{Class: ClassInternal, Message: Redact(message)}
}

// Auth builds a credential failure.
func Auth(message string) *Error {
	return &Error{Class: ClassAuth, Message: Redact(message), StatusCode: http.StatusUnauthorized, Retryable: true}
}

// Quota builds a quota failure.
func Quota(message string) *Error {
	return &Error{Class: ClassQuota, Message: Redact(message), StatusCode: http.StatusForbidden, Retryable: true}
}

// RateLimited builds a rate-limit failure.
func RateLimited(message string) *Error {
	return &Error{Class: ClassRateLimit, Message: Redact(message), StatusCode: http.StatusTooManyRequests, Retryable: true}
}

// Upstream builds an upstream failure at the given status.
func Upstream(status int, message string) *Error {
	return &Error{Class: ClassUpstream, Message: Redact(message), StatusCode: status, Retryable: status >= 500}
}

// Redact removes anything that looks like a credential from text that will be
// returned to a client or written to a log. Error bodies echo request material,
// so every path that surfaces an upstream message runs through here.
func Redact(message string) string {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) > maxReported {
		trimmed = trimmed[:maxReported] + "..."
	}
	return secretPattern.ReplaceAllString(trimmed, "$1<redacted>")
}

// maxReported bounds one reported upstream message.
const maxReported = 600

// secretPattern matches the credential shapes this project handles: bearer
// tokens, long API keys, and the personal tokens of the subscription vendors.
var secretPattern = regexp.MustCompile(
	`((?i)(?:authorization|api[-_]?key|access[-_]?token|refresh[-_]?token|personal[-_]?token|pat|secret|cookie)["']?\s*[:=]\s*["']?)([A-Za-z0-9._-]{8,})`)

// ClassEnvelope encodes a classified failure as an ABI envelope.
func ClassEnvelope(e *Error) []byte {
	if e == nil {
		return Fail("internal_error", "unspecified failure")
	}
	return Fail(string(e.Class), e.Message, e.StatusCode)
}

// EnvelopeFor encodes any error: a classified one keeps its class and status,
// anything else becomes an internal failure.
func EnvelopeFor(err error) []byte {
	if err == nil {
		return Fail("internal_error", "unspecified failure")
	}
	if classified, ok := err.(*Error); ok {
		return ClassEnvelope(classified)
	}
	return Fail("internal_error", Redact(err.Error()))
}
