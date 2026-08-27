package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseEncryptionKey(t *testing.T) {
	t.Parallel()
	key := []byte("0123456789abcdef0123456789abcdef")
	for _, value := range []string{base64.StdEncoding.EncodeToString(key), "3031323334353637383961626364656630313233343536373839616263646566"} {
		parsed, err := ParseEncryptionKey(value)
		if err != nil {
			t.Fatalf("ParseEncryptionKey(%q): %v", value, err)
		}
		if string(parsed) != string(key) {
			t.Fatalf("parsed key mismatch")
		}
	}
	if _, err := ParseEncryptionKey("too-short"); err == nil {
		t.Fatal("expected invalid key error")
	}
}

func TestLoadAndPortValidation(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://db/releasedock")
	t.Setenv("BOOTSTRAP_ADMIN", "admin")
	t.Setenv("BOOTSTRAP_ADMIN_PASSWORD", "a-long-test-password")
	t.Setenv("ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	t.Setenv("PORT", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8080 {
		t.Fatalf("default port=%d", cfg.Port)
	}

	t.Setenv("PORT", "18443")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 18443 {
		t.Fatalf("port=%d", cfg.Port)
	}
	for _, invalid := range []string{"0", "65536", "not-a-port"} {
		t.Setenv("PORT", invalid)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PORT") {
			t.Fatalf("PORT=%q: expected validation error, got %v", invalid, err)
		}
	}
}
