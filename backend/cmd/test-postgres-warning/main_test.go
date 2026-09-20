package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestWarnIfSkipped(t *testing.T) {
	for _, tc := range []struct {
		name string
		dsn  string
		warn bool
	}{
		{"empty", "", true},
		{"ASCII whitespace", " \t\r\n\v\f", true},
		{"NBSP", "\u00a0", true},
		{"Unicode whitespace", "\u0085\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000", true},
		{"nonempty", "postgres://test_user@localhost/test", false},
		{"invalid", "invalid-dsn", false},
		{"padded nonempty", "\u00a0invalid-dsn\u00a0", false},
		{"zero width space is not whitespace", "\u200b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_POSTGRES_DSN", tc.dsn)
			var out bytes.Buffer
			warnIfSkipped(&out)
			if !tc.warn {
				if out.Len() != 0 {
					t.Fatal("nonblank DSN must not produce output")
				}
				return
			}
			warning := out.String()
			if strings.Count(warning, "\n") != 1 || !strings.HasPrefix(warning, "WARN: TEST_POSTGRES_DSN") || !strings.Contains(warning, "PostgreSQL integration tests will be skipped") {
				t.Fatalf("expected one integration-test skip warning, got %q", warning)
			}
		})
	}
}
