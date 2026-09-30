package main

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// qoderCustomAlphabet is the substitution table the upstream WAF expects in
// place of the standard base64 alphabet.
const qoderCustomAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

const qoderStdAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// qoderEncodeBody applies the upstream body transform: standard base64, a
// three-way rotation of the result, then substitution into the custom alphabet.
//
// This is a wire codec, not encryption: it exists so the upstream's WAF sees the
// shape it expects, and both the COSY signature and Cosy-Bodyhash cover the
// encoded bytes rather than the JSON.
func qoderEncodeBody(plaintext []byte) string {
	return qoderSubstitute(qoderRotate(base64.StdEncoding.EncodeToString(plaintext)))
}

// qoderDecodeBody inverts qoderEncodeBody. It exists so the plugin can prove the
// codec round-trips (management diagnostics) and so tests can assert on the
// request body that actually went on the wire.
func qoderDecodeBody(encoded string) ([]byte, error) {
	n := len(encoded)
	if n == 0 {
		return nil, nil
	}
	var mapped strings.Builder
	mapped.Grow(n)
	for index := 0; index < n; index++ {
		char := encoded[index]
		if char == '$' {
			mapped.WriteByte('=')
			continue
		}
		if position := strings.IndexByte(qoderCustomAlphabet, char); position >= 0 {
			mapped.WriteByte(qoderStdAlphabet[position])
			continue
		}
		mapped.WriteByte(char)
	}
	rotated := mapped.String()
	a := n / 3
	if a == 0 {
		return base64.StdEncoding.DecodeString(rotated)
	}
	// qoderRotate produced rotated = std[n-a:] + std[a:n-a] + std[0:a], so the
	// original is the three pieces in reverse order.
	std := rotated[n-a:] + rotated[a:n-a] + rotated[0:a]
	decoded, errDecode := base64.StdEncoding.DecodeString(std)
	if errDecode != nil {
		return nil, fmt.Errorf("decode qoder body: %w", errDecode)
	}
	return decoded, nil
}

// qoderRotate moves the trailing third to the front and the leading third to the
// back, leaving the middle in place. For inputs shorter than three characters
// the rotation is the identity.
func qoderRotate(std string) string {
	n := len(std)
	a := n / 3
	return std[n-a:] + std[a:n-a] + std[0:a]
}

// qoderSubstitute maps standard base64 characters onto the custom alphabet and
// encodes padding as '$'.
func qoderSubstitute(rotated string) string {
	var out strings.Builder
	out.Grow(len(rotated))
	for index := 0; index < len(rotated); index++ {
		char := rotated[index]
		if char == '=' {
			out.WriteByte('$')
			continue
		}
		if position := strings.IndexByte(qoderStdAlphabet, char); position >= 0 {
			out.WriteByte(qoderCustomAlphabet[position])
			continue
		}
		out.WriteByte(char)
	}
	return out.String()
}
