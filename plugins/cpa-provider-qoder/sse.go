package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// sseEnvelope is one outer frame of the upstream event stream.
//
// The model payload travels as a JSON *string* inside body, and control frames
// (the closing timing frame) carry no body at all. statusCodeValue is the only
// error channel that is always present when something went wrong.
type sseEnvelope struct {
	StatusCodeValue json.RawMessage `json:"statusCodeValue"`
	StatusCode      json.RawMessage `json:"statusCode"`
	Message         json.RawMessage `json:"message"`
	Body            json.RawMessage `json:"body"`
}

// innerProbe is the shallow view used to tell a model chunk apart from the
// in-band failure frame the upstream sends with HTTP 200.
type innerProbe struct {
	Choices []json.RawMessage `json:"choices"`
	Usage   json.RawMessage   `json:"usage"`
	Code    json.RawMessage   `json:"code"`
	Message string            `json:"message"`
}

// doneMarker is the stream sentinel. It is not reliable: the upstream often ends
// the stream at a terminal finish_reason without one.
const doneMarker = "[DONE]"

// parseQoderStream unwraps the outer SSE envelope and returns the inner model
// chunks verbatim.
//
// The chunks are returned as bare JSON on purpose. The host frames every chunk
// as "data: <chunk>" and appends its own [DONE], so adding either here would
// double the framing and terminate the client's stream twice.
func parseQoderStream(body []byte) ([]string, error) {
	normalised := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	chunks := make([]string, 0, 64)
	event := ""
	for _, line := range strings.Split(string(normalised), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			continue
		}
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		event = ""
		if payload == "" {
			continue
		}
		if payload == doneMarker {
			return chunks, nil
		}
		var envelope sseEnvelope
		if errUnmarshal := json.Unmarshal([]byte(payload), &envelope); errUnmarshal != nil {
			return nil, pluginkit.NewError("upstream_error",
				"Qoder sent a malformed stream frame: "+truncateText([]byte(payload), 200), http.StatusBadGateway)
		}
		if strings.EqualFold(event, "error") {
			return nil, frameError(&envelope, payload)
		}
		if status, okStatus := envelopeStatus(&envelope); okStatus && status != 0 && status != http.StatusOK {
			return nil, statusError(status, &envelope)
		}
		bodyText, okBody := envelopeBody(&envelope)
		if !okBody {
			continue
		}
		if strings.TrimSpace(bodyText) == doneMarker {
			return chunks, nil
		}
		if errInBand := detectInBandError(bodyText); errInBand != nil {
			return nil, errInBand
		}
		chunks = append(chunks, bodyText)
	}
	return chunks, nil
}

// envelopeStatus reads the upstream status. An absent value means success: the
// field is omitted on the ordinary frames.
func envelopeStatus(envelope *sseEnvelope) (int, bool) {
	raw := bytes.TrimSpace(envelope.StatusCodeValue)
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var numeric int
	if errUnmarshal := json.Unmarshal(raw, &numeric); errUnmarshal == nil {
		return numeric, true
	}
	var text string
	if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal == nil {
		var parsed int
		if _, errScan := fmt.Sscanf(strings.TrimSpace(text), "%d", &parsed); errScan == nil {
			return parsed, true
		}
	}
	return 0, false
}

// envelopeBody returns the inner payload text. A frame without one is a control
// frame and carries nothing to forward.
func envelopeBody(envelope *sseEnvelope) (string, bool) {
	raw := bytes.TrimSpace(envelope.Body)
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	if raw[0] == '"' {
		var text string
		if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal != nil {
			return "", false
		}
		return text, true
	}
	return string(raw), true
}

// detectInBandError recognises the upstream's failure frame: an HTTP 200
// envelope whose model body is an error object with no choices and no usage.
// Without this check the failure would be forwarded as an empty response.
func detectInBandError(bodyText string) *pluginkit.PluginError {
	var probe innerProbe
	if errUnmarshal := json.Unmarshal([]byte(bodyText), &probe); errUnmarshal != nil {
		return nil
	}
	if len(probe.Choices) > 0 || len(probe.Usage) > 0 {
		return nil
	}
	code := scalarText(probe.Code)
	if code == "" && strings.TrimSpace(probe.Message) == "" {
		return nil
	}
	detail := strings.TrimSpace(firstNonEmpty(code, probe.Message))
	if code != "" && strings.TrimSpace(probe.Message) != "" && !strings.Contains(detail, probe.Message) {
		detail = code + ": " + strings.TrimSpace(probe.Message)
	}
	return pluginkit.NewError("upstream_error",
		"Qoder reported an error inside the stream: "+truncateText([]byte(detail), 300), http.StatusBadGateway)
}

// scalarText renders a JSON scalar that may be a string or a number.
func scalarText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var text string
	if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal == nil {
		return strings.TrimSpace(text)
	}
	var numeric float64
	if errUnmarshal := json.Unmarshal(raw, &numeric); errUnmarshal == nil && numeric != 0 {
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%g", numeric), "0"), ".")
	}
	return ""
}

// statusError renders a non-200 envelope as a plugin error, extracting whatever
// detail the upstream attached.
func statusError(status int, envelope *sseEnvelope) *pluginkit.PluginError {
	detail := ""
	if bodyText, okBody := envelopeBody(envelope); okBody {
		detail = extractErrorDetail(bodyText)
	}
	if detail == "" {
		detail = scalarText(envelope.Message)
	}
	message := fmt.Sprintf("Qoder returned upstream status %d", status)
	if detail != "" {
		message += ": " + detail
	}
	return pluginkit.NewError("upstream_error", message, http.StatusBadGateway)
}

// frameError renders an event:error frame.
func frameError(envelope *sseEnvelope, payload string) *pluginkit.PluginError {
	detail := scalarText(envelope.Message)
	if bodyText, okBody := envelopeBody(envelope); okBody {
		detail = firstNonEmpty(extractErrorDetail(bodyText), detail)
	}
	if detail == "" {
		detail = truncateText([]byte(payload), 300)
	}
	return pluginkit.NewError("upstream_error", "Qoder stream failed: "+detail, http.StatusBadGateway)
}

// extractErrorDetail digs the human-readable message out of an error payload.
//
// The upstream nests it one level further in a "details" field that is itself a
// JSON string, so a single unmarshal is not enough.
func extractErrorDetail(bodyText string) string {
	trimmed := strings.TrimSpace(bodyText)
	if trimmed == "" {
		return ""
	}
	var parsed struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
		Details json.RawMessage `json:"details"`
	}
	if errUnmarshal := json.Unmarshal([]byte(trimmed), &parsed); errUnmarshal != nil {
		return truncateText([]byte(trimmed), 300)
	}
	detail := ""
	if len(parsed.Details) > 0 && string(parsed.Details) != "null" {
		var detailsText string
		if errDetails := json.Unmarshal(parsed.Details, &detailsText); errDetails == nil && strings.TrimSpace(detailsText) != "" {
			var inner struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
				Message string `json:"message"`
			}
			if errInner := json.Unmarshal([]byte(detailsText), &inner); errInner == nil {
				detail = firstNonEmpty(inner.Error.Message, inner.Message)
			}
		}
	}
	code := scalarText(parsed.Code)
	message := strings.TrimSpace(parsed.Message)
	if detail == "" {
		detail = firstNonEmpty(message, code)
	} else if code != "" {
		detail = code + ": " + detail
	}
	if detail == "" {
		detail = truncateText([]byte(trimmed), 300)
	}
	return detail
}
