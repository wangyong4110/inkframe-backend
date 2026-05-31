package model

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"io"
	"os"
	"strings"
)

// encryptionKey is loaded once from DB_ENCRYPTION_KEY env var.
// AES-256-GCM requires exactly 32 bytes; shorter keys are zero-padded.
var encryptionKey []byte

const modelEncPrefix = "enc:"

func init() {
	key := os.Getenv("DB_ENCRYPTION_KEY")
	if key != "" {
		k := make([]byte, 32)
		copy(k, []byte(key))
		encryptionKey = k
	}
}

// EncryptField encrypts plaintext with AES-256-GCM.
// Returns plaintext unchanged when the encryption key is not configured or plaintext is empty.
// Output format: "enc:" + base64(nonce + ciphertext).
func EncryptField(plaintext string) (string, error) {
	if len(encryptionKey) == 0 || plaintext == "" {
		return plaintext, nil
	}
	// Already encrypted — don't double-encrypt
	if strings.HasPrefix(plaintext, modelEncPrefix) {
		return plaintext, nil
	}
	block, err := aes.NewCipher(encryptionKey)
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
	return modelEncPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptField decrypts a value produced by EncryptField.
// Values without the "enc:" prefix are returned as-is (backward compatibility / no key).
func DecryptField(ciphertext string) (string, error) {
	if !strings.HasPrefix(ciphertext, modelEncPrefix) {
		return ciphertext, nil // not encrypted — pass through
	}
	if len(encryptionKey) == 0 {
		// Key not configured but data is encrypted — return ciphertext unchanged
		// to avoid losing data; caller should configure the key.
		return ciphertext, nil
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ciphertext, modelEncPrefix))
	if err != nil {
		return ciphertext, nil // malformed — treat as plaintext
	}
	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return ciphertext, nil // too short — treat as plaintext
	}
	plain, err := gcm.Open(nil, data[:ns], data[ns:], nil)
	if err != nil {
		return ciphertext, nil // decryption failed — treat as plaintext (backward compat)
	}
	return string(plain), nil
}
