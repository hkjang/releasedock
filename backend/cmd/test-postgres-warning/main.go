package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	warnIfSkipped(os.Stdout)
}

func warnIfSkipped(out io.Writer) {
	// Match the PostgreSQL integration fixtures, including Unicode whitespace.
	if strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN")) == "" {
		fmt.Fprintln(out, "WARN: TEST_POSTGRES_DSN is unset or blank; PostgreSQL integration tests will be skipped (see README development section).")
	}
}
