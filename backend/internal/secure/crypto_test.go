package secure

import (
	"regexp"
	"testing"
)

func TestVaultRoundTripAndContextBinding(t *testing.T) {
	t.Parallel()
	vault, err := NewVault([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := vault.Encrypt("top-secret", "ai.api_key")
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := vault.Decrypt(ciphertext, "ai.api_key")
	if err != nil || plaintext != "top-secret" {
		t.Fatalf("round trip failed: plaintext=%q err=%v", plaintext, err)
	}
	if _, err := vault.Decrypt(ciphertext, "oidc.client_secret"); err == nil {
		t.Fatal("ciphertext must be bound to its setting context")
	}
	tampered := ciphertext[:len(ciphertext)-1] + "A"
	if _, err := vault.Decrypt(tampered, "ai.api_key"); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestTokenAndUUIDGeneration(t *testing.T) {
	t.Parallel()
	a, err := RandomToken(32)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := RandomToken(32)
	if a == b || len(a) < 40 {
		t.Fatal("tokens must be unique and retain at least 256 bits")
	}
	if !ConstantTimeHashEqual(TokenHash(a), a) || ConstantTimeHashEqual(TokenHash(a), b) {
		t.Fatal("token hash comparison failed")
	}
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("not an RFC 4122 UUIDv4: %q", id)
	}
}
