// Package config also validates the at-rest encryption key for secret-bearing
// columns: 32 raw bytes (AES-256) as hex; malformed values fail at startup.
package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const SecretKeySize = 32 // AES-256

// SecretKey is an AES-256 key ready to construct an AEAD.
type SecretKey [SecretKeySize]byte

// SecretKeyFromHex decodes a 64-char hex string into a SecretKey; wrong
// length or non-hex input is caught at boot, not on the first encrypt.
func SecretKeyFromHex(s string) (SecretKey, error) {
	var k SecretKey
	raw, err := hex.DecodeString(s)
	if err != nil {
		return k, fmt.Errorf("secret key: not hex: %w", err)
	}
	if len(raw) != SecretKeySize {
		return k, fmt.Errorf("secret key: want %d bytes, got %d", SecretKeySize, len(raw))
	}
	copy(k[:], raw)
	return k, nil
}

// Seal encrypts a secret for an at-rest column: AES-256-GCM, random nonce
// prepended. The stored bytes are useless without UC_SECRET_KEY_HEX.
func (k SecretKey) Seal(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts what Seal produced. A truncated row or a value sealed with a
// different key errors instead of answering garbage.
func (k SecretKey) Open(sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, fmt.Errorf("sealed value shorter than a nonce")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
}
