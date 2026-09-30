package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// wrappedField is one credential field sealed at rest by the desktop app.
//
// WorkBuddy 5.6+ replaces plaintext token fields with this envelope. The
// envelope name and the AAD builder below are transcribed from the app's own
// buildAuthenticatedContextAad; the framing is suite 1 under "WBEV1".
type wrappedField struct {
	Encrypted byte // always 1; distinguishes an envelope from a plain string
	Envelope  string
}

// UnmarshalJSON accepts either a plain JSON string or the sealed object, so a
// single struct handles both disk formats.
func (w *wrappedField) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var plain string
		if errUnmarshal := json.Unmarshal(trimmed, &plain); errUnmarshal != nil {
			return errUnmarshal
		}
		*w = wrappedField{Envelope: plain}
		return nil
	}
	var shape struct {
		Encrypted *int   `json:"$wbEncrypted"`
		Envelope  string `json:"envelope"`
	}
	if errUnmarshal := json.Unmarshal(trimmed, &shape); errUnmarshal != nil {
		return errUnmarshal
	}
	flag := byte(0)
	if shape.Encrypted != nil && *shape.Encrypted == 1 {
		flag = 1
	}
	*w = wrappedField{Encrypted: flag, Envelope: shape.Envelope}
	return nil
}

// envelope JSON payload inside wrappedField.Envelope.
type sealedEnvelope struct {
	Suite      int    `json:"suite"`
	KeyID      string `json:"keyId"`
	Nonce      string `json:"nonce"`
	AuthTag    string `json:"authTag"`
	Ciphertext string `json:"ciphertext"`
}

// atRestPayload is what the app's private storage binding returns.
type atRestPayload struct {
	Version         int    `json:"version"`
	AtRestSecretKey string `json:"atRestSecretKey"`
}

// keyProvider resolves the app's at-rest protector key by running the app's own
// Electron binary with ELECTRON_RUN_AS_NODE, which is the only way to reach the
// private binding that owns the secret. The key is cached in memory only and
// never persisted or logged.
type keyProvider struct {
	binary string

	mu     sync.Mutex
	key    []byte
	keyID  string
	loaded bool
}

func newKeyProvider(binary string) *keyProvider {
	return &keyProvider{binary: binary}
}

// protectorKey returns the AES key and its id, resolving them on first use.
func (p *keyProvider) protectorKey() ([]byte, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded {
		return p.key, p.keyID, nil
	}
	if strings.TrimSpace(p.binary) == "" {
		return nil, "", fmt.Errorf("WorkBuddy Electron binary path is not configured")
	}
	ctx, cancel := contextWithTimeout(10 * time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.binary, "-e", helperScript)
	cmd.Env = append(commandEnviron(), "ELECTRON_RUN_AS_NODE=1")
	out, errRun := cmd.Output()
	if errRun != nil {
		return nil, "", fmt.Errorf("run WorkBuddy key helper %s: %w", p.binary, errRun)
	}
	var payload atRestPayload
	if errUnmarshal := json.Unmarshal(bytes.TrimSpace(out), &payload); errUnmarshal != nil {
		return nil, "", fmt.Errorf("decode at-rest payload: %w", errUnmarshal)
	}
	if payload.Version != 1 || payload.AtRestSecretKey == "" {
		return nil, "", fmt.Errorf("unsupported at-rest payload (version %d)", payload.Version)
	}
	if decoded, errDecode := base64.StdEncoding.DecodeString(payload.AtRestSecretKey); errDecode != nil || len(decoded) != 32 {
		return nil, "", fmt.Errorf("at-rest secret is not a canonical 32-byte base64 value")
	}
	key := sha256.Sum256([]byte(payload.AtRestSecretKey))
	keyID := sha256.Sum256(key[:])
	p.key = key[:]
	p.keyID = hex.EncodeToString(keyID[:])[:16]
	p.loaded = true
	return p.key, p.keyID, nil
}

// helperScript asks the app's modified Electron for its at-rest secret.
const helperScript = `process.stdout.write(String(process._linkedBinding("electron_browser_workbuddy_storage").loggerGet()))`

// open unwraps one credential field. A plaintext field is returned as-is.
func (p *keyProvider) open(field wrappedField) (string, error) {
	if field.Encrypted != 1 {
		return field.Envelope, nil
	}
	key, keyID, errKey := p.protectorKey()
	if errKey != nil {
		return "", errKey
	}
	// The envelope is base64-encoded JSON, not JSON text.
	envelopeJSON, errEnvelope := base64.StdEncoding.DecodeString(field.Envelope)
	if errEnvelope != nil {
		return "", fmt.Errorf("decode credential envelope base64: %w", errEnvelope)
	}
	var env sealedEnvelope
	if errUnmarshal := json.Unmarshal(envelopeJSON, &env); errUnmarshal != nil {
		return "", fmt.Errorf("decode credential envelope: %w", errUnmarshal)
	}
	if env.Suite != 1 {
		return "", fmt.Errorf("unsupported credential envelope suite %d", env.Suite)
	}
	if env.KeyID != keyID {
		return "", fmt.Errorf("credential envelope key id %s does not match the installed WorkBuddy key %s", env.KeyID, keyID)
	}
	nonce, errNonce := base64.StdEncoding.DecodeString(env.Nonce)
	if errNonce != nil {
		return "", fmt.Errorf("decode envelope nonce: %w", errNonce)
	}
	tag, errTag := base64.StdEncoding.DecodeString(env.AuthTag)
	if errTag != nil {
		return "", fmt.Errorf("decode envelope auth tag: %w", errTag)
	}
	ciphertext, errCipher := base64.StdEncoding.DecodeString(env.Ciphertext)
	if errCipher != nil {
		return "", fmt.Errorf("decode envelope ciphertext: %w", errCipher)
	}
	block, errBlock := aes.NewCipher(key)
	if errBlock != nil {
		return "", fmt.Errorf("init aes: %w", errBlock)
	}
	gcm, errGCM := cipher.NewGCM(block)
	if errGCM != nil {
		return "", fmt.Errorf("init gcm: %w", errGCM)
	}
	plaintext, errOpen := gcm.Open(nil, nonce, append(ciphertext, tag...), buildAAD(env.KeyID, env.Suite))
	if errOpen != nil {
		return "", fmt.Errorf("open credential envelope: %w", errOpen)
	}
	return string(plaintext), nil
}

// buildAAD reproduces the app's authenticated-context builder byte for byte.
func buildAAD(keyID string, suite int) []byte {
	var buf bytes.Buffer
	buf.WriteString("WB-AAD\x00")
	buf.WriteByte(0x01)
	writeLengthPrefixed(&buf, "WBEV1")
	writeLengthPrefixed(&buf, "sym-v1")
	var suiteBytes [4]byte
	binary.BigEndian.PutUint32(suiteBytes[:], uint32(suite))
	buf.Write(suiteBytes[:])
	writeLengthPrefixed(&buf, keyID)
	buf.Write([]byte{0x02, 0x00, 0x00})
	return buf.Bytes()
}

func writeLengthPrefixed(buf *bytes.Buffer, value string) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	buf.Write(length[:])
	buf.WriteString(value)
}
