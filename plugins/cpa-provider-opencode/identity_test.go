package main

import (
	"strings"
	"testing"
)

// decodePrefix recovers the 48-bit value an identifier carries.
func decodePrefix(t *testing.T, id, prefix string) uint64 {
	t.Helper()
	body := strings.TrimPrefix(id, prefix+"_")
	if len(body) != 26 {
		t.Fatalf("identifier %q does not carry 26 characters after the prefix", id)
	}
	var value uint64
	for _, char := range body[:12] {
		digit := strings.IndexRune("0123456789abcdef", char)
		if digit < 0 {
			t.Fatalf("prefix %q carries a non-hex digit", body[:12])
		}
		value = value<<4 | uint64(digit)
	}
	return value
}

// TestOpencodeIDCounterStaysInRange pins the property the live endpoint
// enforces: the counter in the low twelve bits of the value before the
// complement must stay below 64. Measured against the live endpoint, counters
// 0..63 answer 200 and counters 64..4095 answer 403 FreeTierError.
func TestOpencodeIDCounterStaysInRange(t *testing.T) {
	for trial := 0; trial < 512; trial++ {
		id := opencodeID("ses", true)
		complemented := decodePrefix(t, id, "ses")
		counter := (^complemented) & 0xFFF
		if counter >= idCounterRange {
			t.Fatalf("trial %d produced counter %d in %q, want less than %d", trial, counter, id, idCounterRange)
		}
	}
}

// TestOpencodeIDCarriesTheClockReading proves the digits encode the low six
// bytes of millisecond times 4096 plus counter, most significant byte first.
// The helper is pinned against an explicit clock reading so the expectation does
// not depend on when the test runs.
func TestOpencodeIDCarriesTheClockReading(t *testing.T) {
	const millis = int64(1791975000000)
	id := opencodeIDAt("ses", true, millis)
	complemented := decodePrefix(t, id, "ses")
	restored := ^complemented & 0xFFFFFFFFFFFF
	counter := restored & 0xFFF
	if counter >= idCounterRange {
		t.Fatalf("counter %d is outside the accepted range", counter)
	}
	if want := (uint64(millis) * 4096) & 0xFFFFFFFFFFFF; restored-counter != want {
		t.Fatalf("prefix decodes to %#x, want %#x", restored-counter, want)
	}
}

// TestOpencodeIDAscendingIsNotComplemented pins the difference between the two
// identifier kinds: only the session identifier is complemented.
func TestOpencodeIDAscendingIsNotComplemented(t *testing.T) {
	const millis = int64(1791975000000)
	id := opencodeIDAt("msg", false, millis)
	value := decodePrefix(t, id, "msg")
	counter := value & 0xFFF
	if counter >= idCounterRange {
		t.Fatalf("counter %d is outside the accepted range", counter)
	}
	if want := (uint64(millis) * 4096) & 0xFFFFFFFFFFFF; value-counter != want {
		t.Fatalf("ascending prefix decodes to %#x, want %#x", value-counter, want)
	}
}

// TestOpencodeIDAlphabet pins the suffix shape: fourteen base62 characters.
func TestOpencodeIDAlphabet(t *testing.T) {
	id := opencodeID("ses", true)
	body := strings.TrimPrefix(id, "ses_")
	for index, char := range body[12:] {
		if !strings.ContainsRune(idAlphabet, char) {
			t.Fatalf("suffix character %d of %q is not base62", index, id)
		}
	}
}

// TestSessionIDIsStable pins the reuse rule: Zen meters free usage per session
// and caches by that id, so one conversation keeps one identifier.
func TestSessionIDIsStable(t *testing.T) {
	store := newSessionStore()
	first := store.id("conversation")
	if second := store.id("conversation"); first != second {
		t.Fatalf("session id changed between calls: %q then %q", first, second)
	}
	if other := store.id("other"); other == first {
		t.Fatalf("distinct conversations share the session id %q", other)
	}
}
