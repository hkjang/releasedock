package server

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func mustPrefixes(t *testing.T, values ...string) []netip.Prefix {
	t.Helper()
	prefixes, err := parsePrefixes(values)
	if err != nil {
		t.Fatalf("parse prefixes %v: %v", values, err)
	}
	return prefixes
}

func TestParsePrefixAcceptsBareAddressAsSingleHost(t *testing.T) {
	prefix, err := parsePrefix("10.1.2.3")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if prefix.Bits() != 32 {
		t.Fatalf("expected a /32, got %s", prefix)
	}
	if _, err := parsePrefix("not-an-ip"); err == nil {
		t.Fatal("expected an invalid address to be rejected")
	}
	if _, err := parsePrefix("10.0.0.0/99"); err == nil {
		t.Fatal("expected an invalid CIDR to be rejected")
	}
}

// Without a configured trusted proxy, a forwarded header is attacker-supplied
// and must be ignored entirely.
func TestClientIPIgnoresForwardedHeaderFromUntrustedPeer(t *testing.T) {
	request := httptest.NewRequest("GET", "/api/v1/admin/users", nil)
	request.RemoteAddr = "203.0.113.9:44321"
	request.Header.Set("X-Forwarded-For", "10.0.0.5")
	if got := clientIP(request, mustPrefixes(t, "192.168.10.0/24")); got.String() != "203.0.113.9" {
		t.Fatalf("expected the peer address, got %s", got)
	}
}

func TestClientIPUsesForwardedHeaderFromTrustedProxy(t *testing.T) {
	request := httptest.NewRequest("GET", "/api/v1/admin/users", nil)
	request.RemoteAddr = "192.168.10.4:44321"
	request.Header.Set("X-Forwarded-For", "203.0.113.9, 192.168.10.4")
	if got := clientIP(request, mustPrefixes(t, "192.168.10.0/24")); got.String() != "203.0.113.9" {
		t.Fatalf("expected the client address, got %s", got)
	}
}

// A client may append its own X-Forwarded-For entries; only the rightmost
// non-proxy hop is trustworthy.
func TestClientIPTakesRightmostUntrustedHop(t *testing.T) {
	request := httptest.NewRequest("GET", "/api/v1/admin/users", nil)
	request.RemoteAddr = "192.168.10.4:1234"
	request.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9, 192.168.10.4")
	if got := clientIP(request, mustPrefixes(t, "192.168.10.0/24")); got.String() != "203.0.113.9" {
		t.Fatalf("spoofed leftmost entry was trusted: %s", got)
	}
}

func TestAdminNetworkAllowedHonoursTheAllowlist(t *testing.T) {
	cfg := networkSettings{
		AdminAllowlistEnabled: true,
		AdminAllowlist:        []string{"192.168.10.0/24"},
		adminPrefixes:         mustPrefixes(t, "192.168.10.0/24"),
	}
	if !adminNetworkAllowed(cfg, netip.MustParseAddr("192.168.10.55")) {
		t.Fatal("expected an in-range address to be allowed")
	}
	if adminNetworkAllowed(cfg, netip.MustParseAddr("203.0.113.9")) {
		t.Fatal("expected an out-of-range address to be denied")
	}
	// Loopback is the console escape hatch and is always allowed.
	if !adminNetworkAllowed(cfg, netip.MustParseAddr("127.0.0.1")) {
		t.Fatal("expected loopback to stay allowed")
	}
	if !adminNetworkAllowed(cfg, netip.MustParseAddr("::1")) {
		t.Fatal("expected IPv6 loopback to stay allowed")
	}
	// An unparsable address must fail closed while the allowlist is on.
	if adminNetworkAllowed(cfg, netip.Addr{}) {
		t.Fatal("expected an invalid address to be denied")
	}
}

func TestAdminNetworkAllowedIsInertWhenDisabled(t *testing.T) {
	cfg := networkSettings{AdminAllowlistEnabled: false}
	if !adminNetworkAllowed(cfg, netip.MustParseAddr("203.0.113.9")) {
		t.Fatal("a disabled allowlist must not block anyone")
	}
}

// An IPv4-mapped IPv6 peer must match an IPv4 allowlist entry.
func TestAdminNetworkAllowedMatchesMappedIPv4(t *testing.T) {
	cfg := networkSettings{
		AdminAllowlistEnabled: true,
		adminPrefixes:         mustPrefixes(t, "192.168.10.0/24"),
	}
	if !adminNetworkAllowed(cfg, netip.MustParseAddr("::ffff:192.168.10.55")) {
		t.Fatal("expected an IPv4-mapped address to match the IPv4 range")
	}
}

func TestIsAdminRouteCoversPermissionAndPath(t *testing.T) {
	if !isAdminRoute("admin.users.read", "/api/v1/users") {
		t.Fatal("an admin.* permission must be gated")
	}
	if !isAdminRoute("audit.read", "/api/v1/admin/audit") {
		t.Fatal("an /api/v1/admin/ path must be gated")
	}
	if isAdminRoute("releases.read", "/api/v1/releases") {
		t.Fatal("an ordinary route must not be gated")
	}
}

func TestSplitLinesDropsBlanksAndCarriageReturns(t *testing.T) {
	values := splitLines("10.0.0.1\r\n\n  192.168.0.0/16  \n")
	if len(values) != 2 || values[0] != "10.0.0.1" || values[1] != "192.168.0.0/16" {
		t.Fatalf("unexpected parse: %q", values)
	}
}
