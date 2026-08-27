package config

import (
	"errors"
	"os"
)

type Bootstrap struct {
	PostgresDSN   string
	EncryptionKey string
}

// Load reads the only two process-level settings supported by the runner.
// Every operational setting is loaded from PostgreSQL.
func Load() (Bootstrap, error) {
	dsn, ok := os.LookupEnv("POSTGRES_DSN")
	if !ok || dsn == "" {
		return Bootstrap{}, errors.New("POSTGRES_DSN is required")
	}
	key, ok := os.LookupEnv("ENCRYPTION_KEY")
	if !ok || key == "" {
		return Bootstrap{}, errors.New("ENCRYPTION_KEY is required")
	}
	return Bootstrap{PostgresDSN: dsn, EncryptionKey: key}, nil
}
