package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

const envelopePrefix = "v1:"

type SecretBox struct {
	aead cipher.AEAD
}

func NewSecretBox(encodedKey string) (*SecretBox, error) {
	key, err := decodeKey(encodedKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create AES-GCM: %w", err)
	}
	return &SecretBox{aead: aead}, nil
}

func decodeKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	var candidates = []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		hex.DecodeString,
	}
	for _, decode := range candidates {
		if key, err := decode(value); err == nil && len(key) == 32 {
			return key, nil
		}
	}
	return nil, errors.New("ENCRYPTION_KEY must encode exactly 32 bytes (base64 or hex)")
}

func (s *SecretBox) Decrypt(envelope, aad string) ([]byte, error) {
	if !strings.HasPrefix(envelope, envelopePrefix) {
		return nil, errors.New("unsupported encrypted value format")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(envelope, envelopePrefix))
	if err != nil {
		return nil, fmt.Errorf("decode encrypted value: %w", err)
	}
	if len(raw) < s.aead.NonceSize()+s.aead.Overhead() {
		return nil, errors.New("encrypted value is too short")
	}
	nonce, ciphertext := raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():]
	plaintext, err := s.aead.Open(nil, nonce, ciphertext, []byte(aad))
	if err != nil {
		return nil, errors.New("decrypt encrypted value: authentication failed")
	}
	return plaintext, nil
}

// Encrypt is provided for the administrator/API implementation and tests. The
// runner itself only decrypts approved credentials read from PostgreSQL.
func (s *SecretBox) Encrypt(plaintext []byte, aad string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := s.aead.Seal(nil, nonce, plaintext, []byte(aad))
	raw := append(nonce, sealed...)
	return envelopePrefix + base64.RawStdEncoding.EncodeToString(raw), nil
}
