package pipeline

import (
	"bytes"
	"strings"
	"testing"
)

func TestExactSecretRedactorSpansChunks(t *testing.T) {
	const secret = "super-secret-value"
	var output bytes.Buffer
	redactor := newExactSecretRedactor(&output, []byte(secret))
	for _, chunk := range []string{"before super-", "secret-", "value middle ", secret, " after"} {
		if _, err := redactor.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := redactor.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("plaintext secret reached output: %q", output.String())
	}
	if strings.Count(output.String(), string(targetCredentialRedaction)) != 2 {
		t.Fatalf("unexpected redacted output %q", output.String())
	}
}
