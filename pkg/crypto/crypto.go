// Package crypto provides symmetric encryption for secrets at rest (GitHub tokens,
// Slack bot tokens) using AES-256-GCM with a key derived from ENCRYPTION_KEY. The
// scheme is identical to the original implementation in internal/github so values
// encrypted by either are interchangeable: "v1:<base64(nonce + ciphertext+tag)>".
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

const version = "v1"

// deriveKey returns a 32-byte AES key by SHA-256 hashing the raw key string.
func deriveKey(encKey string) []byte {
	h := sha256.Sum256([]byte(encKey))
	return h[:]
}

// Encrypt encrypts plaintext with AES-256-GCM and version-tags the result so key
// rotation can be handled transparently.
func Encrypt(plaintext, encKey string) (string, error) {
	gcm, err := newGCM(encKey)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return version + ":" + base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt reverses Encrypt. It handles the "v1:<base64>" format (current) and bare
// base64 (legacy, no prefix) — trying currentKey then prevKey for rotation.
func Decrypt(encrypted, currentKey, prevKey string) (string, error) {
	if strings.HasPrefix(encrypted, version+":") {
		return decryptPayload(strings.TrimPrefix(encrypted, version+":"), currentKey)
	}
	if plain, err := decryptPayload(encrypted, currentKey); err == nil {
		return plain, nil
	}
	if prevKey == "" {
		return "", fmt.Errorf("failed to decrypt legacy value (no previous key set)")
	}
	return decryptPayload(encrypted, prevKey)
}

func newGCM(encKey string) (cipher.AEAD, error) {
	block, err := aes.NewCipher(deriveKey(encKey))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func decryptPayload(payload, encKey string) (string, error) {
	gcm, err := newGCM(encKey)
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("failed to decode value: %w", err)
	}
	if len(data) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt: %w", err)
	}
	return string(plain), nil
}
