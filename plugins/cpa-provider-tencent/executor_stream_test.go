package main

import (
	"strings"
	"testing"
)

func payloadStrings(payloads [][]byte) []string {
	out := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		out = append(out, string(payload))
	}
	return out
}

// An event split across reads must not be emitted until it is complete, or the
// client receives a truncated — and therefore invalid — JSON chunk.
func TestSSEForwarderHoldsPartialEvent(t *testing.T) {
	forwarder := &sseForwarder{}

	if got := forwarder.feed([]byte(`data: {"a":`)); len(got) != 0 {
		t.Fatalf("partial event emitted early: %v", payloadStrings(got))
	}
	got := forwarder.feed([]byte("1}\n\n"))
	if len(got) != 1 || string(got[0]) != `{"a":1}` {
		t.Fatalf("completed event = %v, want one payload", payloadStrings(got))
	}
}

// Several events in one read must all be emitted, in order.
func TestSSEForwarderEmitsEveryBufferedEvent(t *testing.T) {
	forwarder := &sseForwarder{}
	got := forwarder.feed([]byte("data: one\n\ndata: two\n\ndata: three\n\n"))
	want := []string{"one", "two", "three"}
	if strings.Join(payloadStrings(got), ",") != strings.Join(want, ",") {
		t.Fatalf("payloads = %v, want %v", payloadStrings(got), want)
	}
}

// The host appends its own terminator, so forwarding the upstream's would end
// the client's stream twice.
func TestSSEForwarderDropsDone(t *testing.T) {
	forwarder := &sseForwarder{}
	got := forwarder.feed([]byte("data: {\"x\":1}\n\ndata: [DONE]\n\n"))
	if len(got) != 1 || string(got[0]) != `{"x":1}` {
		t.Fatalf("payloads = %v, want only the data event", payloadStrings(got))
	}
}

// CRLF is a legal SSE dialect and appears on some gateways.
func TestSSEForwarderHandlesCRLF(t *testing.T) {
	forwarder := &sseForwarder{}
	got := forwarder.feed([]byte("data: {\"a\":1}\r\n\r\ndata: [DONE]\r\n\r\n"))
	if len(got) != 1 || string(got[0]) != `{"a":1}` {
		t.Fatalf("payloads = %v", payloadStrings(got))
	}
}

// An upstream that closes right after its last event leaves it unterminated;
// dropping it would silently lose the final token.
func TestSSEForwarderFlushEmitsTrailingEvent(t *testing.T) {
	forwarder := &sseForwarder{}
	if got := forwarder.feed([]byte(`data: {"last":true}`)); len(got) != 0 {
		t.Fatalf("unterminated event emitted by feed: %v", payloadStrings(got))
	}
	got := forwarder.flush()
	if len(got) != 1 || string(got[0]) != `{"last":true}` {
		t.Fatalf("flush = %v, want the trailing event", payloadStrings(got))
	}
}

// A multi-line data field is one JSON payload, not several.
func TestSSEForwarderJoinsMultilineData(t *testing.T) {
	forwarder := &sseForwarder{}
	got := forwarder.feed([]byte("data: {\"a\":\ndata: 1}\n\n"))
	if len(got) != 1 || string(got[0]) != "{\"a\":\n1}" {
		t.Fatalf("payloads = %v", payloadStrings(got))
	}
}

// Byte-at-a-time delivery is the worst case for the framing buffer.
func TestSSEForwarderHandlesByteAtATime(t *testing.T) {
	forwarder := &sseForwarder{}
	full := []byte("data: {\"a\":1}\n\ndata: {\"b\":2}\n\n")
	var collected [][]byte
	for _, b := range full {
		collected = append(collected, forwarder.feed([]byte{b})...)
	}
	got := payloadStrings(collected)
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Fatalf("payloads = %v", got)
	}
}

// Comment lines and blank padding carry no payload.
func TestSSEForwarderIgnoresNonDataLines(t *testing.T) {
	forwarder := &sseForwarder{}
	got := forwarder.feed([]byte(": keepalive\n\nevent: message\ndata: {\"a\":1}\n\n"))
	if len(got) != 1 || string(got[0]) != `{"a":1}` {
		t.Fatalf("payloads = %v", payloadStrings(got))
	}
}
