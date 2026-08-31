package server

import (
	"net/http/httptest"
	"testing"
)

func TestForwardedFirstTakesTheOriginalClientValue(t *testing.T) {
	if got := forwardedFirst("https, http"); got != "https" {
		t.Fatalf("expected https, got %q", got)
	}
	if got := forwardedFirst("  HTTPS  "); got != "https" {
		t.Fatalf("expected lower-cased https, got %q", got)
	}
	if got := forwardedFirst(""); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestValidOriginHostRejectsSmuggledValues(t *testing.T) {
	for _, host := range []string{"releasedock.company.local", "releasedock.local:8443", "[::1]:8080"} {
		if !validOriginHost(host) {
			t.Fatalf("expected %q to be accepted", host)
		}
	}
	for _, host := range []string{"", "host/path", "user@host", "host?a=b", "host#frag", "ho st", "host\nx"} {
		if validOriginHost(host) {
			t.Fatalf("expected %q to be rejected", host)
		}
	}
}

// A blank configuration must still produce a usable redirect URI, which is the
// whole point of making the setting optional.
func TestRequestOriginDerivesHTTPSForNonLoopback(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest("GET", "/api/v1/auth/oidc/login", nil)
	request.Host = "releasedock.company.local"
	request.RemoteAddr = "10.0.0.9:1234"
	// No trusted proxies are configured, so no forwarded header is consulted;
	// a non-loopback host is still assumed to be served over TLS.
	if got := server.requestOrigin(request.Context(), request); got != "https://releasedock.company.local" {
		t.Fatalf("unexpected origin %q", got)
	}
}

func TestRequestOriginKeepsHTTPForLoopback(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest("GET", "/api/v1/auth/oidc/login", nil)
	request.Host = "localhost:8080"
	request.RemoteAddr = "127.0.0.1:5555"
	if got := server.requestOrigin(request.Context(), request); got != "http://localhost:8080" {
		t.Fatalf("unexpected origin %q", got)
	}
}

func TestRequestOriginIgnoresForwardedHostFromUntrustedPeer(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest("GET", "/api/v1/auth/oidc/login", nil)
	request.Host = "releasedock.company.local"
	request.RemoteAddr = "203.0.113.9:1234"
	request.Header.Set("X-Forwarded-Host", "evil.example.com")
	request.Header.Set("X-Forwarded-Proto", "http")
	if got := server.requestOrigin(request.Context(), request); got != "https://releasedock.company.local" {
		t.Fatalf("a forwarded header from an untrusted peer was honoured: %q", got)
	}
}

func TestRequestOriginRejectsUnusableHost(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest("GET", "/api/v1/auth/oidc/login", nil)
	request.Host = "bad host/with-path"
	request.RemoteAddr = "10.0.0.9:1234"
	if got := server.requestOrigin(request.Context(), request); got != "" {
		t.Fatalf("expected an empty origin, got %q", got)
	}
}

func TestOidcCallbackPathIsStable(t *testing.T) {
	// The route table and the derived redirect URI must not drift apart.
	if oidcCallbackPath != "/api/v1/auth/oidc/callback" {
		t.Fatalf("unexpected callback path %q", oidcCallbackPath)
	}
}
