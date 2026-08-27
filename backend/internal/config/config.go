package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Runtime contains the only five environment-backed settings used by ReleaseDock.
// All operational settings are stored in PostgreSQL and managed through the admin API.
type Runtime struct {
	PostgresDSN            string
	BootstrapAdmin         string
	BootstrapAdminPassword string
	EncryptionKey          []byte
	Port                   int
}

func Load() (Runtime, error) {
	cfg := Runtime{
		PostgresDSN:            strings.TrimSpace(os.Getenv("POSTGRES_DSN")),
		BootstrapAdmin:         strings.TrimSpace(os.Getenv("BOOTSTRAP_ADMIN")),
		BootstrapAdminPassword: os.Getenv("BOOTSTRAP_ADMIN_PASSWORD"),
		Port:                   8080,
	}
	if rawPort := strings.TrimSpace(os.Getenv("PORT")); rawPort != "" {
		port, err := strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			return Runtime{}, errors.New("PORT must be an integer between 1 and 65535")
		}
		cfg.Port = port
	}
	if cfg.PostgresDSN == "" {
		return Runtime{}, errors.New("POSTGRES_DSN is required")
	}
	if cfg.BootstrapAdmin == "" {
		return Runtime{}, errors.New("BOOTSTRAP_ADMIN is required")
	}
	if len(cfg.BootstrapAdminPassword) < 12 {
		return Runtime{}, errors.New("BOOTSTRAP_ADMIN_PASSWORD must contain at least 12 characters")
	}
	key, err := ParseEncryptionKey(os.Getenv("ENCRYPTION_KEY"))
	if err != nil {
		return Runtime{}, err
	}
	cfg.EncryptionKey = key
	return cfg, nil
}

// ParseEncryptionKey accepts a 32-byte key encoded as base64 (preferred) or hex.
func ParseEncryptionKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("ENCRYPTION_KEY is required")
	}
	if key, err := base64.StdEncoding.DecodeString(value); err == nil && len(key) == 32 {
		return key, nil
	}
	if key, err := base64.RawStdEncoding.DecodeString(value); err == nil && len(key) == 32 {
		return key, nil
	}
	if key, err := hex.DecodeString(value); err == nil && len(key) == 32 {
		return key, nil
	}
	return nil, fmt.Errorf("ENCRYPTION_KEY must be exactly 32 bytes encoded as base64 or 64 hex characters")
}
