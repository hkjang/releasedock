package crypto

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestSecretBoxRoundTripAndAAD(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	box, err := NewSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := box.Encrypt([]byte(`{"username":"robot","password":"secret"}`), "credential:7")
	if err != nil {
		t.Fatal(err)
	}
	got, err := box.Decrypt(encrypted, "credential:7")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"username":"robot","password":"secret"}` {
		t.Fatalf("unexpected plaintext: %s", got)
	}
	if _, err := box.Decrypt(encrypted, "credential:8"); err == nil {
		t.Fatal("expected AAD authentication failure")
	}
}

func TestSecretBoxRejectsShortKey(t *testing.T) {
	if _, err := NewSecretBox(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("expected invalid key error")
	}
}
