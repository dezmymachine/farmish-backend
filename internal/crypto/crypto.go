// Package crypto encrypts small secrets (ID numbers, account numbers) at
// rest with AES-256-GCM. Ciphertexts carry a version prefix so keys can
// rotate later.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// versionPrefix tags every ciphertext with its format version.
const versionPrefix = "v1:"

// nonceSize is the GCM standard nonce size.
const nonceSize = 12

// ErrDecrypt means a ciphertext failed to decode or authenticate. The input
// may be tampered, encrypted under another key, or in an unknown format.
var ErrDecrypt = errors.New("decrypt failed")

// Crypter encrypts and decrypts with one 32-byte key.
type Crypter struct {
	aead cipher.AEAD
}

// New returns a Crypter for a 32-byte key (e.g. decoded DATA_ENCRYPTION_KEY).
func New(key []byte) (*Crypter, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &Crypter{aead: aead}, nil
}

// Encrypt seals plaintext with a fresh random nonce and returns
// `v1:` + base64(nonce‖ciphertext).
func (c *Crypter) Encrypt(plaintext []byte) (string, error) {
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, plaintext, nil)
	return versionPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens a ciphertext produced by Encrypt.
func (c *Crypter) Decrypt(ciphertext string) ([]byte, error) {
	raw, ok := strings.CutPrefix(ciphertext, versionPrefix)
	if !ok {
		return nil, fmt.Errorf("%w: unknown version", ErrDecrypt)
	}
	sealed, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(sealed) < nonceSize {
		return nil, fmt.Errorf("%w: bad encoding", ErrDecrypt)
	}
	plain, err := c.aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], nil)
	if err != nil {
		return nil, fmt.Errorf("%w: authentication failed", ErrDecrypt)
	}
	return plain, nil
}
