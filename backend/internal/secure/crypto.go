package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const envelopeVersion = "v1"

type Vault struct {
	aead cipher.AEAD
}

func NewVault(key []byte) (*Vault, error) {
	if len(key) != 32 {
		return nil, errors.New("AES-256-GCM requires a 32-byte key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: aead}, nil
}

func (v *Vault) Encrypt(plaintext, context string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := v.aead.Seal(nil, nonce, []byte(plaintext), []byte(context))
	payload := append(nonce, ciphertext...)
	return envelopeVersion + ":" + base64.RawStdEncoding.EncodeToString(payload), nil
}

func (v *Vault) Decrypt(envelope, context string) (string, error) {
	if envelope == "" {
		return "", nil
	}
	parts := strings.SplitN(envelope, ":", 2)
	if len(parts) != 2 || parts[0] != envelopeVersion {
		return "", errors.New("unsupported ciphertext envelope")
	}
	payload, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("invalid ciphertext encoding")
	}
	if len(payload) < v.aead.NonceSize()+v.aead.Overhead() {
		return "", errors.New("invalid ciphertext length")
	}
	nonce, ciphertext := payload[:v.aead.NonceSize()], payload[v.aead.NonceSize():]
	plaintext, err := v.aead.Open(nil, nonce, ciphertext, []byte(context))
	if err != nil {
		return "", errors.New("ciphertext authentication failed")
	}
	return string(plaintext), nil
}

func RandomToken(bytes int) (string, error) {
	if bytes < 16 {
		return "", errors.New("token entropy must be at least 128 bits")
	}
	b := make([]byte, bytes)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func TokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func ConstantTimeHashEqual(got []byte, token string) bool {
	want := TokenHash(token)
	return subtle.ConstantTimeCompare(got, want) == 1
}
