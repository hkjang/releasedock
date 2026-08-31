package server

import (
	"net/http/httptest"
	"strings"
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
	if got := server.requestOrigin(request.Context(), request, false); got != "https://releasedock.company.local" {
		t.Fatalf("unexpected origin %q", got)
	}
}

func TestRequestOriginKeepsHTTPForLoopback(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest("GET", "/api/v1/auth/oidc/login", nil)
	request.Host = "localhost:8080"
	request.RemoteAddr = "127.0.0.1:5555"
	if got := server.requestOrigin(request.Context(), request, false); got != "http://localhost:8080" {
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
	if got := server.requestOrigin(request.Context(), request, false); got != "https://releasedock.company.local" {
		t.Fatalf("a forwarded header from an untrusted peer was honoured: %q", got)
	}
}

func TestRequestOriginRejectsUnusableHost(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest("GET", "/api/v1/auth/oidc/login", nil)
	request.Host = "bad host/with-path"
	request.RemoteAddr = "10.0.0.9:1234"
	if got := server.requestOrigin(request.Context(), request, false); got != "" {
		t.Fatalf("expected an empty origin, got %q", got)
	}
}

func TestOidcCallbackPathIsStable(t *testing.T) {
	// The route table and the derived redirect URI must not drift apart.
	if oidcCallbackPath != "/api/v1/auth/oidc/callback" {
		t.Fatalf("unexpected callback path %q", oidcCallbackPath)
	}
}

func TestRequestOriginKeepsHTTPWhenInsecureEndpointsAllowed(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest("GET", "/api/v1/auth/oidc/login", nil)
	request.Host = "releasedock.internal"
	request.RemoteAddr = "10.0.0.9:1234"
	// An air-gapped deployment without TLS must not have its redirect URI
	// rewritten to a scheme the identity provider is not listening on.
	if got := server.requestOrigin(request.Context(), request, true); got != "http://releasedock.internal" {
		t.Fatalf("unexpected origin %q", got)
	}
}

func TestPrivatePlaintextHostAcceptsOnlyInternalTargets(t *testing.T) {
	for _, host := range []string{
		"127.0.0.1:8080", "localhost", "10.1.2.3", "172.16.0.5:8443", "192.168.1.9",
		"keycloak", "keycloak.internal", "sso.company.local", "[::1]:8080", "[fd00::1]:8080",
	} {
		if !privatePlaintextHost(host) {
			t.Fatalf("expected %q to be treated as internal", host)
		}
	}
	for _, host := range []string{"keycloak.example.com", "8.8.8.8", "login.microsoftonline.com", ""} {
		if privatePlaintextHost(host) {
			t.Fatalf("expected %q to be treated as public", host)
		}
	}
}

func TestValidateOIDCEndpointRejectsPlaintextUnlessAllowed(t *testing.T) {
	if err := validateOIDCEndpoint("token_endpoint", "https://sso.company.local/token", false); err != nil {
		t.Fatalf("https must be accepted: %v", err)
	}
	// The default refusal must name the endpoint and the offending value so an
	// administrator can act on it.
	err := validateOIDCEndpoint("token_endpoint", "http://keycloak.internal:8080/token", false)
	if err == nil {
		t.Fatal("expected plaintext to be refused by default")
	}
	if !strings.Contains(err.Error(), "token_endpoint") || !strings.Contains(err.Error(), "keycloak.internal:8080") {
		t.Fatalf("error lacks actionable detail: %v", err)
	}
	if err := validateOIDCEndpoint("token_endpoint", "http://keycloak.internal:8080/token", true); err != nil {
		t.Fatalf("plaintext to an internal host must be accepted when allowed: %v", err)
	}
	// Even with the opt-in, a public plaintext target stays refused.
	if err := validateOIDCEndpoint("token_endpoint", "http://sso.example.com/token", true); err == nil {
		t.Fatal("expected plaintext to a public host to stay refused")
	}
	for _, value := range []string{"", "ftp://host/token", "https://user:pw@host/token", "https://host/token#frag", "https:///token"} {
		if err := validateOIDCEndpoint("token_endpoint", value, true); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}
