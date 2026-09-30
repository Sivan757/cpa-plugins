package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cosyPublicKeyPEM is the RSA public key the Qoder client encrypts its
// per-request AES key with. It is a protocol constant, not a secret.
const cosyPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var (
	cosyKeyOnce sync.Once
	cosyKeyVal  *rsa.PublicKey
	cosyKeyErr  error
)

// cosyPublicKey parses the embedded key once.
func cosyPublicKey() (*rsa.PublicKey, error) {
	cosyKeyOnce.Do(func() {
		block, _ := pem.Decode([]byte(cosyPublicKeyPEM))
		if block == nil {
			cosyKeyErr = fmt.Errorf("qoder COSY public key is not PEM encoded")
			return
		}
		parsed, errParse := x509.ParsePKIXPublicKey(block.Bytes)
		if errParse != nil {
			cosyKeyErr = fmt.Errorf("parse qoder COSY public key: %w", errParse)
			return
		}
		typed, okTyped := parsed.(*rsa.PublicKey)
		if !okTyped {
			cosyKeyErr = fmt.Errorf("qoder COSY public key is %T, want RSA", parsed)
			return
		}
		cosyKeyVal = typed
	})
	return cosyKeyVal, cosyKeyErr
}

// cosyIdentity is the subscriber identity the COSY headers carry. AuthToken is
// the exchanged job token, never the personal access token itself.
type cosyIdentity struct {
	UserID    string
	AuthToken string
	Name      string
	Email     string
	MachineID string
}

// cosyUserInfo is the inner document that travels AES-encrypted in the
// Authorization header. Field order is irrelevant to the server, but the shape
// must match because the server parses it as JSON.
type cosyUserInfo struct {
	UID                string `json:"uid"`
	SecurityOAuthToken string `json:"security_oauth_token"`
	Name               string `json:"name"`
	AID                string `json:"aid"`
	Email              string `json:"email"`
}

// cosyPayload is the outer document that is base64-encoded and then signed.
type cosyPayload struct {
	Version     string `json:"version"`
	RequestID   string `json:"requestId"`
	Info        string `json:"info"`
	CosyVersion string `json:"cosyVersion"`
	IDEVersion  string `json:"ideVersion"`
}

// cosySigPath is the path the signature covers: the request path with the
// transport's "/algo" prefix removed. Signing the prefixed path is rejected.
func cosySigPath(rawURL string) (string, error) {
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil {
		return "", fmt.Errorf("parse qoder url %q: %w", rawURL, errParse)
	}
	path := parsed.Path
	if strings.HasPrefix(path, "/algo") {
		path = strings.TrimPrefix(path, "/algo")
	}
	return path, nil
}

// randomAESKey returns the 16-character AES-128 key the client generates per
// request. It is hex, mirroring the reference client, so it is always 16 bytes.
func randomAESKey() (string, error) {
	raw := make([]byte, 8)
	if _, errRead := rand.Read(raw); errRead != nil {
		return "", fmt.Errorf("read randomness: %w", errRead)
	}
	return hex.EncodeToString(raw), nil
}

// aesCBCEncryptBase64 encrypts with AES-128-CBC using the key as its own IV,
// which is what the client does: the payload is short-lived and session-scoped.
func aesCBCEncryptBase64(plaintext, key []byte) (string, error) {
	block, errCipher := aes.NewCipher(key)
	if errCipher != nil {
		return "", fmt.Errorf("init aes: %w", errCipher)
	}
	padded := pkcs7Pad(plaintext, aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	return append(append([]byte{}, data...), strings.Repeat(string(rune(padding)), padding)...)
}

// buildCosyHeaders signs one request and returns the full COSY header set.
//
// body must be the exact byte string that goes on the wire (already WAF
// encoded for the chat face), because both the signature and Cosy-Bodyhash
// cover it.
func buildCosyHeaders(rawURL string, body []byte, identity cosyIdentity) (http.Header, error) {
	if strings.TrimSpace(identity.UserID) == "" {
		return nil, fmt.Errorf("qoder: subscriber id is empty")
	}
	if strings.TrimSpace(identity.AuthToken) == "" {
		return nil, fmt.Errorf("qoder: job token is empty")
	}
	publicKey, errKey := cosyPublicKey()
	if errKey != nil {
		return nil, errKey
	}
	aesKey, errAESKey := randomAESKey()
	if errAESKey != nil {
		return nil, errAESKey
	}
	infoJSON, errInfo := json.Marshal(cosyUserInfo{
		UID:                identity.UserID,
		SecurityOAuthToken: identity.AuthToken,
		Name:               identity.Name,
		AID:                "",
		Email:              identity.Email,
	})
	if errInfo != nil {
		return nil, fmt.Errorf("marshal qoder user info: %w", errInfo)
	}
	infoB64, errInfoEnc := aesCBCEncryptBase64(infoJSON, []byte(aesKey))
	if errInfoEnc != nil {
		return nil, errInfoEnc
	}
	encryptedKey, errEncrypt := rsa.EncryptPKCS1v15(rand.Reader, publicKey, []byte(aesKey))
	if errEncrypt != nil {
		return nil, fmt.Errorf("encrypt qoder session key: %w", errEncrypt)
	}
	cosyKey := base64.StdEncoding.EncodeToString(encryptedKey)

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	payloadJSON, errPayload := json.Marshal(cosyPayload{
		Version:     "v1",
		RequestID:   newUUID(),
		Info:        infoB64,
		CosyVersion: ideVersion,
		IDEVersion:  "",
	})
	if errPayload != nil {
		return nil, fmt.Errorf("marshal qoder auth payload: %w", errPayload)
	}
	payloadB64 := base64.StdEncoding.EncodeToString(payloadJSON)

	sigPath, errSigPath := cosySigPath(rawURL)
	if errSigPath != nil {
		return nil, errSigPath
	}
	sigInput := payloadB64 + "\n" + cosyKey + "\n" + timestamp + "\n" + string(body) + "\n" + sigPath
	signature := md5.Sum([]byte(sigInput))
	bodyHash := md5.Sum(body)

	machineID := identity.MachineID
	if strings.TrimSpace(machineID) == "" {
		machineID = defaultMachineID()
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer COSY."+payloadB64+"."+hex.EncodeToString(signature[:]))
	headers.Set("Cosy-Key", cosyKey)
	headers.Set("Cosy-User", identity.UserID)
	headers.Set("Cosy-Date", timestamp)
	headers.Set("Cosy-Version", ideVersion)
	headers.Set("Cosy-Machineid", machineID)
	headers.Set("Cosy-Machinetoken", machineID)
	headers.Set("Cosy-Machinetype", "5")
	headers.Set("Cosy-Machineos", machineOS())
	headers.Set("Cosy-Clienttype", clientType)
	headers.Set("Cosy-Clientip", "127.0.0.1")
	headers.Set("Cosy-Bodyhash", hex.EncodeToString(bodyHash[:]))
	headers.Set("Cosy-Bodylength", strconv.Itoa(len(body)))
	headers.Set("Cosy-Sigpath", sigPath)
	headers.Set("Cosy-Data-Policy", "disagree")
	headers.Set("Cosy-Organization-Id", "")
	headers.Set("Cosy-Organization-Tags", "")
	headers.Set("Login-Version", "v2")
	headers.Set("X-Request-Id", newUUID())
	// The official client also advertises its business context here. Matching it
	// keeps this plugin's traffic indistinguishable from the first-party client.
	headers.Set("Cosy-Business-Product", "cli")
	headers.Set("Cosy-Business-Type", "agent")
	headers.Set("Cosy-Scene", "assistant")
	return headers, nil
}

var (
	machineIDOnce sync.Once
	machineIDVal  string
)

// defaultMachineID returns a stable per-install identifier.
//
// The upstream does not validate the format, so a generated UUID that survives
// for the life of the process is enough; it is never used as a credential.
func defaultMachineID() string {
	machineIDOnce.Do(func() { machineIDVal = newUUID() })
	return machineIDVal
}
