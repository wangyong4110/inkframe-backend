package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"strings"
)

// encPrefix marks ciphertext produced by Encrypt to distinguish it from legacy plaintext keys.
const encPrefix = "enc:"

// Encrypt encrypts plaintext using AES-256-GCM.
// Returns plaintext unchanged when key is empty (encryption disabled).
// The output format is: "enc:" + base64(nonce + ciphertext).
func Encrypt(plaintext, key string) (string, error) {
	if key == "" || plaintext == "" {
		return plaintext, nil
	}
	block, err := aes.NewCipher(normalizeKey(key))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt decrypts a value produced by Encrypt.
// Values without the "enc:" prefix are returned as-is for backward compatibility.
func Decrypt(ciphertext, key string) (string, error) {
	if !strings.HasPrefix(ciphertext, encPrefix) {
		return ciphertext, nil // legacy plaintext — pass through
	}
	if key == "" {
		return "", errors.New("encryption key not configured but stored value is encrypted")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ciphertext, encPrefix))
	if err != nil {
		return ciphertext, nil // malformed — treat as plaintext
	}
	block, err := aes.NewCipher(normalizeKey(key))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return "", errors.New("ciphertext too short")
	}
	plain, err := gcm.Open(nil, data[:ns], data[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// normalizeKey pads or truncates to exactly 32 bytes for AES-256.
func normalizeKey(key string) []byte {
	k := make([]byte, 32)
	copy(k, []byte(key))
	return k
}
