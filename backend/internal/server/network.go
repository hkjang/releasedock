package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// networkSettingsTTL keeps the allowlist off the hot path without making a
// change take noticeably long to apply. A write invalidates the cache outright,
// so the TTL only bounds staleness from another process.
const networkSettingsTTL = 5 * time.Second

type networkSettings struct {
	AdminAllowlistEnabled bool      `json:"adminIpAllowlistEnabled"`
	AdminAllowlist        []string  `json:"adminIpAllowlist"`
	TrustedProxies        []string  `json:"trustedProxyCidrs"`
	UpdatedAt             time.Time `json:"updatedAt"`

	adminPrefixes []netip.Prefix
	proxyPrefixes []netip.Prefix
}

// parsePrefix accepts either a CIDR block or a bare address, which is treated
// as a single host.
func parsePrefix(value string) (netip.Prefix, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return netip.Prefix{}, errors.New("빈 값은 사용할 수 없습니다")
	}
	if strings.Contains(value, "/") {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q는 올바른 CIDR이 아닙니다", value)
		}
		return prefix.Masked(), nil
	}
	address, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q는 올바른 IP 주소가 아닙니다", value)
	}
	return netip.PrefixFrom(address, address.BitLen()), nil
}

func parsePrefixes(values []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := parsePrefix(value)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func prefixesContain(prefixes []netip.Prefix, address netip.Addr) bool {
	// Compare IPv4 and IPv4-mapped IPv6 on the same footing.
	address = address.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func (s *Server) loadNetworkSettings(ctx context.Context) (networkSettings, error) {
	s.networkMu.Lock()
	if s.networkCache != nil && time.Now().Before(s.networkExpiry) {
		cached := *s.networkCache
		s.networkMu.Unlock()
		return cached, nil
	}
	s.networkMu.Unlock()

	// Returning an error rather than dereferencing a nil pool keeps every
	// caller on its documented failure path instead of panicking.
	if s.store == nil || s.store.Pool == nil {
		return networkSettings{}, errors.New("network settings store is unavailable")
	}
	var cfg networkSettings
	err := s.store.Pool.QueryRow(ctx, `SELECT admin_ip_allowlist_enabled,admin_ip_allowlist,trusted_proxy_cidrs,updated_at
		FROM network_settings WHERE id='default'`).
		Scan(&cfg.AdminAllowlistEnabled, &cfg.AdminAllowlist, &cfg.TrustedProxies, &cfg.UpdatedAt)
	if err != nil {
		return networkSettings{}, err
	}
	if cfg.AdminAllowlist == nil {
		cfg.AdminAllowlist = []string{}
	}
	if cfg.TrustedProxies == nil {
		cfg.TrustedProxies = []string{}
	}
	// Values were validated on write; a parse failure here must not silently
	// widen access, so an unparsable entry is simply dropped.
	cfg.adminPrefixes, _ = parsePrefixes(cfg.AdminAllowlist)
	cfg.proxyPrefixes, _ = parsePrefixes(cfg.TrustedProxies)

	s.networkMu.Lock()
	s.networkCache = &cfg
	s.networkExpiry = time.Now().Add(networkSettingsTTL)
	s.networkMu.Unlock()
	return cfg, nil
}

func (s *Server) invalidateNetworkSettings() {
	s.networkMu.Lock()
	s.networkCache = nil
	s.networkMu.Unlock()
}

// clientIP resolves the caller's address. X-Forwarded-For is consulted only
// when the immediate peer is a configured trusted proxy, and then only from
// the right, stopping at the first address that is not itself a trusted proxy.
// Anything further left is attacker-controlled and must never be trusted.
func clientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	host := remoteIP(r)
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	if len(trusted) == 0 || !prefixesContain(trusted, peer) {
		return peer
	}
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded == "" {
		return peer
	}
	parts := strings.Split(forwarded, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			continue
		}
		candidate = candidate.Unmap()
		if !prefixesContain(trusted, candidate) {
			return candidate
		}
	}
	return peer
}

func (s *Server) requestClientIP(r *http.Request) netip.Addr {
	cfg, err := s.loadNetworkSettings(r.Context())
	if err != nil {
		address, _ := netip.ParseAddr(remoteIP(r))
		return address.Unmap()
	}
	return clientIP(r, cfg.proxyPrefixes)
}

// adminNetworkAllowed reports whether an administration request may proceed.
// Loopback is always permitted so an operator on the server console can undo a
// mistaken allowlist.
func adminNetworkAllowed(cfg networkSettings, address netip.Addr) bool {
	if !cfg.AdminAllowlistEnabled {
		return true
	}
	if !address.IsValid() {
		return false
	}
	if address.IsLoopback() {
		return true
	}
	return prefixesContain(cfg.adminPrefixes, address)
}

// withAdminNetwork gates administration endpoints on the source address. It
// runs after authentication so a rejection can be attributed in the audit log.
func (s *Server) withAdminNetwork(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg, err := s.loadNetworkSettings(r.Context())
		if err != nil {
			// Failing closed here would make a database blip lock out every
			// administrator, including the one who could fix it.
			s.log.Warn("could not load network settings; admin IP allowlist not enforced", "error", err)
			next(w, r)
			return
		}
		if adminNetworkAllowed(cfg, clientIP(r, cfg.proxyPrefixes)) {
			next(w, r)
			return
		}
		p, _ := principalFrom(r)
		s.store.Audit(r.Context(), p.UserID, "admin.network.denied", "settings", "network", "denied",
			clientIP(r, cfg.proxyPrefixes).String(), r.UserAgent(), nil)
		writeError(w, http.StatusForbidden, "admin_network_denied", "이 IP 주소에서는 관리 기능을 사용할 수 없습니다")
	}
}

// addrString renders an address for display, falling back to the raw peer
// string so the screen never shows a blank or "invalid IP" where an operator
// expects to read their own address.
func addrString(address netip.Addr, raw string) string {
	if address.IsValid() {
		return address.String()
	}
	return strings.TrimSpace(raw)
}

func (s *Server) getNetworkSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadNetworkSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load network settings")
		return
	}
	peer := remoteIP(r)
	forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	// A forwarded header from an unregistered peer is deliberately ignored for
	// access control, but reporting it is what lets an administrator discover
	// they are behind a proxy and which address to actually allow. Without
	// this the screen shows only the proxy address and the allowlist cannot be
	// configured correctly.
	proxySuspected := forwarded != "" && !prefixesContain(cfg.proxyPrefixes, s.peerAddr(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"adminIpAllowlistEnabled": cfg.AdminAllowlistEnabled,
		"adminIpAllowlist":        strings.Join(cfg.AdminAllowlist, "\n"),
		"trustedProxyCidrs":       strings.Join(cfg.TrustedProxies, "\n"),
		// The address the allowlist is actually compared against.
		"callerIp": addrString(clientIP(r, cfg.proxyPrefixes), peer),
		// The direct TCP peer, which is the proxy when one is in front.
		"peerIp": peer,
		// Informational only, never used for a decision.
		"forwardedFor":   forwarded,
		"proxySuspected": proxySuspected,
		"updatedAt":      cfg.UpdatedAt,
	})
}

// peerAddr parses the direct TCP peer address.
func (s *Server) peerAddr(r *http.Request) netip.Addr {
	address, _ := netip.ParseAddr(remoteIP(r))
	return address.Unmap()
}

type networkSettingsInput struct {
	AdminIPAllowlistEnabled *bool   `json:"adminIpAllowlistEnabled"`
	AdminIPAllowlist        *string `json:"adminIpAllowlist"`
	TrustedProxyCIDRs       *string `json:"trustedProxyCidrs"`
}

func splitLines(raw string) []string {
	values := []string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line != "" {
			values = append(values, line)
		}
	}
	return values
}

func (s *Server) putNetworkSettings(w http.ResponseWriter, r *http.Request) {
	var input networkSettingsInput
	if !decodeJSON(w, r, &input) {
		return
	}
	current, err := s.loadNetworkSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load network settings")
		return
	}
	next := current
	if input.AdminIPAllowlistEnabled != nil {
		next.AdminAllowlistEnabled = *input.AdminIPAllowlistEnabled
	}
	if input.AdminIPAllowlist != nil {
		next.AdminAllowlist = splitLines(*input.AdminIPAllowlist)
	}
	if input.TrustedProxyCIDRs != nil {
		next.TrustedProxies = splitLines(*input.TrustedProxyCIDRs)
	}
	if len(next.AdminAllowlist) > 256 || len(next.TrustedProxies) > 64 {
		writeError(w, http.StatusBadRequest, "invalid_network_settings", "목록이 너무 깁니다")
		return
	}
	adminPrefixes, err := parsePrefixes(next.AdminAllowlist)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_network_settings", "허용 IP: "+err.Error())
		return
	}
	proxyPrefixes, err := parsePrefixes(next.TrustedProxies)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_network_settings", "신뢰 프록시: "+err.Error())
		return
	}
	next.adminPrefixes = adminPrefixes
	next.proxyPrefixes = proxyPrefixes

	if next.AdminAllowlistEnabled && len(next.AdminAllowlist) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_network_settings", "허용 목록이 비어 있으면 활성화할 수 없습니다")
		return
	}
	// Refuse a change that would lock the requester out of the screen they are
	// standing on. Loopback keeps its permanent exemption.
	caller := clientIP(r, next.proxyPrefixes)
	if !adminNetworkAllowed(next, caller) {
		writeError(w, http.StatusConflict, "would_lock_out_admin",
			fmt.Sprintf("현재 접속 IP(%s)가 허용 목록에 없어 저장할 수 없습니다", caller))
		return
	}

	p, _ := principalFrom(r)
	_, err = s.store.Pool.Exec(r.Context(), `UPDATE network_settings SET
		admin_ip_allowlist_enabled=$1,admin_ip_allowlist=$2,trusted_proxy_cidrs=$3,
		updated_by=$4,updated_at=now() WHERE id='default'`,
		next.AdminAllowlistEnabled, next.AdminAllowlist, next.TrustedProxies, p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not update network settings")
		return
	}
	s.invalidateNetworkSettings()
	details, _ := json.Marshal(map[string]any{
		"enabled": next.AdminAllowlistEnabled, "entries": len(next.AdminAllowlist),
		"trustedProxies": len(next.TrustedProxies),
	})
	s.store.Audit(r.Context(), p.UserID, "network_settings.update", "settings", "network", "success", caller.String(), r.UserAgent(), details)
	s.getNetworkSettings(w, r)
}
