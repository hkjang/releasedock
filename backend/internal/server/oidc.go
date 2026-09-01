package server

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/hkjang/releasedock/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

type oidcSettings struct {
	Enabled         bool
	Issuer          string
	ClientID        string
	ClientSecretEnc string
	RedirectURL     string
	Scopes          []string
	AutoCreateUser  bool
	// AllowInsecureEndpoints permits plaintext HTTP OIDC endpoints, but only to
	// non-public hosts. Needed for air-gapped Keycloak without TLS.
	AllowInsecureEndpoints bool
	// AutoLogin signs a visitor in silently when the identity provider session
	// is still valid, using prompt=none.
	AutoLogin     bool
	DefaultRoleID *string
}

type oidcDiscovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	Algorithms            []string `json:"id_token_signing_alg_values_supported"`
}

func (s *Server) loadOIDC(ctx context.Context) (oidcSettings, error) {
	return loadOIDCWithQueryer(ctx, s.store.Pool, false)
}

func loadOIDCWithQueryer(ctx context.Context, queryer dependencyQueryer, forUpdate bool) (oidcSettings, error) {
	var cfg oidcSettings
	query := `SELECT enabled,issuer,client_id,client_secret_enc,redirect_url,scopes,auto_create_user,allow_insecure_endpoints,auto_login,default_role_id FROM oidc_settings WHERE id='default'`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	err := queryer.QueryRow(ctx, query).
		Scan(&cfg.Enabled, &cfg.Issuer, &cfg.ClientID, &cfg.ClientSecretEnc, &cfg.RedirectURL, &cfg.Scopes, &cfg.AutoCreateUser, &cfg.AllowInsecureEndpoints, &cfg.AutoLogin, &cfg.DefaultRoleID)
	return cfg, err
}

// oidcCallbackPath is fixed by the route table, so the redirect URI only ever
// varies by origin.
const oidcCallbackPath = "/api/v1/auth/oidc/callback"

// requestOrigin reconstructs the origin the browser actually used. Forwarded
// headers are honoured only when the immediate peer is a configured trusted
// proxy; otherwise a client could choose its own redirect URI.
func (s *Server) requestOrigin(ctx context.Context, r *http.Request, allowInsecure bool) string {
	host := r.Host
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if cfg, err := s.loadNetworkSettings(ctx); err == nil && len(cfg.proxyPrefixes) > 0 {
		if peer, parseErr := netip.ParseAddr(remoteIP(r)); parseErr == nil && prefixesContain(cfg.proxyPrefixes, peer.Unmap()) {
			if forwarded := forwardedFirst(r.Header.Get("X-Forwarded-Proto")); forwarded == "http" || forwarded == "https" {
				scheme = forwarded
			}
			if forwarded := forwardedFirst(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
				host = forwarded
			}
		}
	}
	// A TLS-terminating proxy that forwards neither X-Forwarded-Proto nor a
	// trusted-proxy entry would otherwise yield http, which every identity
	// provider rejects for a non-loopback redirect URI.
	if scheme == "http" && !loopbackHost(host) && !allowInsecure {
		scheme = "https"
	}
	if !validOriginHost(host) {
		return ""
	}
	return scheme + "://" + host
}

// forwardedFirst returns the left-most entry of a comma-separated forwarded
// header, which is the value describing the original client request.
func forwardedFirst(value string) string {
	if value == "" {
		return ""
	}
	first := value
	if index := strings.IndexByte(value, ','); index >= 0 {
		first = value[:index]
	}
	return strings.ToLower(strings.TrimSpace(first))
}

// validOriginHost rejects anything that is not a bare host[:port], so a header
// cannot smuggle a path, credentials, or a second URL into the redirect URI.
func validOriginHost(host string) bool {
	if host == "" || len(host) > 255 {
		return false
	}
	if strings.ContainsAny(host, "/\\?#@ \t\r\n") {
		return false
	}
	parsed, err := url.Parse("https://" + host)
	return err == nil && parsed.Host == host && parsed.Path == ""
}

// resolveOIDCRedirectURI decides which redirect URI to present to the identity
// provider. An explicitly configured value always wins; otherwise the public
// URL is used, and failing that the value is derived from the request. This is
// why the setting can be left blank entirely.
func (s *Server) resolveOIDCRedirectURI(ctx context.Context, r *http.Request, cfg oidcSettings) string {
	if configured := strings.TrimSpace(cfg.RedirectURL); configured != "" {
		return configured
	}
	if origin, err := s.configuredPublicOrigin(ctx); err == nil && origin != "" {
		return origin + oidcCallbackPath
	}
	if origin := s.requestOrigin(ctx, r, cfg.AllowInsecureEndpoints); origin != "" {
		return origin + oidcCallbackPath
	}
	return ""
}

func (s *Server) authConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadOIDC(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load authentication configuration")
		return
	}
	// autoLogin is published so the browser knows whether to attempt a silent
	// sign-in before rendering the login screen.
	writeJSON(w, http.StatusOK, map[string]any{"local_enabled": true, "oidc": map[string]any{
		"enabled": cfg.Enabled, "issuer": cfg.Issuer, "autoLogin": cfg.Enabled && cfg.AutoLogin,
	}})
}

// privatePlaintextHost reports whether a plaintext endpoint may target this
// host. Loopback, RFC1918/RFC4193 private ranges, link-local and dotless
// internal hostnames are permitted; a publicly routable host is not, so an
// opt-in for an air-gapped Keycloak cannot leak a client secret to the
// internet in cleartext.
func privatePlaintextHost(host string) bool {
	name := host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		name = parsedHost
	}
	name = strings.Trim(name, "[]")
	if name == "" {
		return false
	}
	if address, err := netip.ParseAddr(name); err == nil {
		address = address.Unmap()
		return address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() ||
			address.IsLinkLocalMulticast() || address.IsUnspecified()
	}
	if strings.EqualFold(name, "localhost") {
		return true
	}
	// A dotless name can only resolve inside a search domain, so it is
	// necessarily internal. ".local" and ".internal" are likewise not routable.
	lower := strings.ToLower(name)
	return !strings.Contains(lower, ".") ||
		strings.HasSuffix(lower, ".local") ||
		strings.HasSuffix(lower, ".internal") ||
		strings.HasSuffix(lower, ".intranet")
}

// validateOIDCEndpoint checks one discovery URL. The error names the offending
// value and the reason, because "token_endpoint is invalid" on its own gives an
// administrator nothing to act on.
func validateOIDCEndpoint(name, raw string, allowInsecure bool) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("OIDC discovery %s is missing", name)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("OIDC discovery %s is not a valid URL: %q", name, raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("OIDC discovery %s has no host: %q", name, raw)
	}
	if parsed.User != nil {
		return fmt.Errorf("OIDC discovery %s must not contain userinfo: %q", name, raw)
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("OIDC discovery %s must not contain a fragment: %q", name, raw)
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if !allowInsecure {
			return fmt.Errorf(
				"OIDC discovery %s uses plaintext HTTP (%q). Keycloak이 이 endpoint를 HTTPS로 내려주도록 고치거나, 폐쇄망이라면 OIDC 설정에서 '내부 평문 HTTP endpoint 허용'을 켜십시오",
				name, raw)
		}
		if !privatePlaintextHost(parsed.Host) {
			return fmt.Errorf(
				"OIDC discovery %s uses plaintext HTTP to a publicly routable host (%q); this is refused even with the insecure option enabled",
				name, raw)
		}
		return nil
	default:
		return fmt.Errorf("OIDC discovery %s must use http or https: %q", name, raw)
	}
}

func (s *Server) discoverOIDC(ctx context.Context, issuer string, allowInsecure bool) (oidcDiscovery, error) {
	issuer = strings.TrimSuffix(strings.TrimSpace(issuer), "/")
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return oidcDiscovery{}, errors.New("OIDC issuer must be an absolute URL without userinfo, query, or fragment")
	}
	if err := validateOIDCEndpoint("issuer", issuer, allowInsecure); err != nil {
		return oidcDiscovery{}, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return oidcDiscovery{}, fmt.Errorf("OIDC discovery request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return oidcDiscovery{}, fmt.Errorf("OIDC discovery returned HTTP %d", resp.StatusCode)
	}
	var discovery oidcDiscovery
	dec := json.NewDecoder(io.LimitReader(resp.Body, (1<<20)+1))
	if err := dec.Decode(&discovery); err != nil {
		return oidcDiscovery{}, fmt.Errorf("decode OIDC discovery: %w", err)
	}
	if discovery.Issuer != issuer {
		return oidcDiscovery{}, fmt.Errorf(
			"OIDC discovery issuer %q does not exactly match the configured issuer %q",
			discovery.Issuer, issuer)
	}
	// Ordered rather than map iteration so a document with several problems
	// always reports the same one, which keeps the message reproducible.
	endpoints := []struct{ name, value string }{
		{"authorization_endpoint", discovery.AuthorizationEndpoint},
		{"token_endpoint", discovery.TokenEndpoint},
		{"jwks_uri", discovery.JWKSURI},
	}
	for _, endpoint := range endpoints {
		if err := validateOIDCEndpoint(endpoint.name, endpoint.value, allowInsecure); err != nil {
			return oidcDiscovery{}, err
		}
	}
	return discovery, nil
}

func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.consume("oidc-ip:"+remoteIP(r), 120, time.Minute) {
		writeError(w, http.StatusTooManyRequests, "oidc_rate_limited", "too many OIDC login attempts; try again later")
		return
	}
	cfg, err := s.loadOIDC(r.Context())
	if err != nil || !cfg.Enabled {
		writeError(w, http.StatusNotFound, "oidc_disabled", "OIDC login is not enabled")
		return
	}
	discovery, err := s.discoverOIDC(r.Context(), cfg.Issuer, cfg.AllowInsecureEndpoints)
	if err != nil {
		s.log.Error("OIDC discovery", "error", err)
		writeError(w, http.StatusBadGateway, "oidc_discovery_failed", "identity provider discovery failed")
		return
	}
	redirectURI := s.resolveOIDCRedirectURI(r.Context(), r, cfg)
	if redirectURI == "" {
		writeError(w, http.StatusInternalServerError, "oidc_redirect_unresolved", "could not determine the OIDC redirect URI for this request")
		return
	}
	// prompt=none asks the provider to answer from an existing session only.
	// It never renders UI: either a code comes straight back, or an error
	// such as login_required does.
	silent := r.URL.Query().Get("prompt") == "none"
	if silent && !cfg.AutoLogin {
		// Refusing an unrequested silent attempt keeps the redirect surface
		// tied to the administrator setting.
		silent = false
	}
	state, _ := secure.RandomToken(32)
	nonce, _ := secure.RandomToken(24)
	codeVerifier, _ := secure.RandomToken(32)
	codeChallengeDigest := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(codeChallengeDigest[:])
	returnTo := r.URL.Query().Get("return_to")
	if !safeReturnTo(returnTo) {
		returnTo = "/"
	}
	_, err = s.store.Pool.Exec(r.Context(), `WITH expired AS (DELETE FROM oidc_states WHERE expires_at<now()) INSERT INTO oidc_states(state_hash,nonce,code_verifier,return_to,redirect_uri,silent,expires_at) VALUES($1,$2,$3,$4,$5,$6,now()+interval '10 minutes')`, secure.TokenHash(state), nonce, codeVerifier, returnTo, redirectURI, silent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not initiate OIDC login")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "releasedock_oidc_state", Value: state, Path: "/api/v1/auth/oidc/callback", HttpOnly: true, Secure: s.useSecureCookies(r.Context(), r), SameSite: http.SameSiteLaxMode, MaxAge: 600})
	params := url.Values{
		"response_type": {"code"}, "client_id": {cfg.ClientID}, "redirect_uri": {redirectURI},
		"scope": {strings.Join(cfg.Scopes, " ")}, "state": {state}, "nonce": {nonce},
		"code_challenge": {codeChallenge}, "code_challenge_method": {"S256"},
	}
	if silent {
		params.Set("prompt", "none")
	}
	http.Redirect(w, r, discovery.AuthorizationEndpoint+"?"+params.Encode(), http.StatusFound)
}

func safeReturnTo(value string) bool {
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "\r\n") {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && !parsed.IsAbs() && parsed.Host == ""
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	// The provider reports a refusal as an error parameter rather than a code.
	// prompt=none produces login_required here whenever no session exists,
	// which is an ordinary outcome and must not look like a failure.
	if providerError := r.URL.Query().Get("error"); providerError != "" {
		silent := false
		if state != "" {
			_ = s.store.Pool.QueryRow(r.Context(),
				`DELETE FROM oidc_states WHERE state_hash=$1 RETURNING silent`,
				secure.TokenHash(state)).Scan(&silent)
		}
		if silent {
			// Land on the login page with the marker the browser uses to stop
			// retrying, so a signed-out visitor is not bounced in a loop.
			http.Redirect(w, r, "/login?sso=none", http.StatusFound)
			return
		}
		s.log.Warn("OIDC provider returned an error", "error", providerError)
		http.Redirect(w, r, "/login?sso=error", http.StatusFound)
		return
	}
	stateCookie, cookieErr := r.Cookie("releasedock_oidc_state")
	if state == "" || code == "" || cookieErr != nil || subtle.ConstantTimeCompare([]byte(state), []byte(stateCookie.Value)) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_oidc_callback", "missing or invalid OIDC callback parameters")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "releasedock_oidc_state", Value: "", Path: "/api/v1/auth/oidc/callback", HttpOnly: true, Secure: s.useSecureCookies(r.Context(), r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	var nonce, codeVerifier, returnTo, redirectURI string
	var silent bool
	err := s.store.Pool.QueryRow(r.Context(), `DELETE FROM oidc_states WHERE state_hash=$1 AND expires_at>now() RETURNING nonce,code_verifier,return_to,redirect_uri,silent`, secure.TokenHash(state)).Scan(&nonce, &codeVerifier, &returnTo, &redirectURI, &silent)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_oidc_state", "OIDC state expired or was already used")
		return
	}
	cfg, err := s.loadOIDC(r.Context())
	if err != nil || !cfg.Enabled {
		writeError(w, http.StatusBadRequest, "oidc_disabled", "OIDC login is not enabled")
		return
	}
	// A state row created before the redirect URI was pinned carries an empty
	// value; resolve it exactly as the login leg would have.
	if redirectURI == "" {
		redirectURI = s.resolveOIDCRedirectURI(r.Context(), r, cfg)
	}
	discovery, err := s.discoverOIDC(r.Context(), cfg.Issuer, cfg.AllowInsecureEndpoints)
	if err != nil {
		writeError(w, http.StatusBadGateway, "oidc_discovery_failed", "identity provider discovery failed")
		return
	}
	secret, err := s.vault.Decrypt(cfg.ClientSecretEnc, "oidc.client_secret")
	if err != nil {
		s.log.Error("decrypt OIDC client secret", "error", err)
		writeError(w, http.StatusInternalServerError, "secret_error", "OIDC secret could not be decrypted")
		return
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {codeVerifier}}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, discovery.TokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(cfg.ClientID, secret)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "oidc_token_failed", "OIDC token exchange failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeError(w, http.StatusBadGateway, "oidc_token_failed", "OIDC token exchange was rejected")
		return
	}
	var tokenResponse struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, (2<<20)+1)).Decode(&tokenResponse); err != nil || tokenResponse.IDToken == "" {
		writeError(w, http.StatusBadGateway, "oidc_token_failed", "OIDC response did not contain an ID token")
		return
	}
	claims, err := s.verifyIDToken(r.Context(), tokenResponse.IDToken, discovery, cfg.ClientID, nonce)
	if err != nil {
		s.log.Warn("OIDC ID token rejected", "error", err)
		writeError(w, http.StatusUnauthorized, "invalid_id_token", "OIDC ID token validation failed")
		return
	}
	userID, err := s.resolveOIDCUser(r.Context(), cfg, claims)
	if err != nil {
		if errors.Is(err, errOIDCUserNotProvisioned) {
			writeError(w, http.StatusForbidden, "oidc_user_not_provisioned", err.Error())
		} else {
			s.log.Error("resolve OIDC user", "error", err)
			writeError(w, http.StatusInternalServerError, "database_error", "could not provision OIDC user")
		}
		return
	}
	if !safeReturnTo(returnTo) {
		returnTo = "/"
	}
	// Browser clients use the session response directly. return_to is included so
	// a frontend can perform a same-origin redirect after storing the CSRF token.
	r.Header.Set("X-ReleaseDock-OIDC-Return-To", returnTo)
	s.createSession(w, r, userID, "oidc")
}

type idTokenClaims struct {
	Issuer            string          `json:"iss"`
	Subject           string          `json:"sub"`
	Audience          json.RawMessage `json:"aud"`
	AuthorizedParty   string          `json:"azp"`
	ExpiresAt         int64           `json:"exp"`
	IssuedAt          int64           `json:"iat"`
	Nonce             string          `json:"nonce"`
	PreferredUsername string          `json:"preferred_username"`
	Name              string          `json:"name"`
	Email             string          `json:"email"`
	EmailVerified     bool            `json:"email_verified"`
}

type jwkSet struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Use string `json:"use"`
		Alg string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

func (s *Server) verifyIDToken(ctx context.Context, raw string, discovery oidcDiscovery, clientID, nonce string) (idTokenClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return idTokenClaims{}, errors.New("ID token must be a compact JWT")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return idTokenClaims{}, err
	}
	var header struct{ Alg, Kid, Typ string }
	if err := json.Unmarshal(headerBytes, &header); err != nil || header.Alg != "RS256" || header.Kid == "" {
		return idTokenClaims{}, errors.New("only RS256 ID tokens with kid are accepted")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, discovery.JWKSURI, nil)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return idTokenClaims{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return idTokenClaims{}, fmt.Errorf("JWKS returned HTTP %d", resp.StatusCode)
	}
	var set jwkSet
	if err := json.NewDecoder(io.LimitReader(resp.Body, (2<<20)+1)).Decode(&set); err != nil {
		return idTokenClaims{}, err
	}
	var publicKey *rsa.PublicKey
	for _, key := range set.Keys {
		if key.Kid != header.Kid || key.Kty != "RSA" || (key.Use != "" && key.Use != "sig") || (key.Alg != "" && key.Alg != "RS256") {
			continue
		}
		nBytes, nErr := base64.RawURLEncoding.DecodeString(key.N)
		eBytes, eErr := base64.RawURLEncoding.DecodeString(key.E)
		if nErr != nil || eErr != nil || len(eBytes) == 0 || len(eBytes) > 4 {
			continue
		}
		var padded [4]byte
		copy(padded[4-len(eBytes):], eBytes)
		exponent := int(binary.BigEndian.Uint32(padded[:]))
		if exponent < 3 || exponent%2 == 0 {
			continue
		}
		publicKey = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: exponent}
		break
	}
	if publicKey == nil || publicKey.N.BitLen() < 2048 {
		return idTokenClaims{}, errors.New("matching secure RSA signing key not found")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return idTokenClaims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return idTokenClaims{}, errors.New("ID token signature is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return idTokenClaims{}, err
	}
	var claims idTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return idTokenClaims{}, err
	}
	if claims.Issuer != discovery.Issuer || claims.Subject == "" || claims.Nonce != nonce {
		return idTokenClaims{}, errors.New("ID token issuer, subject, or nonce is invalid")
	}
	now := time.Now().Unix()
	if claims.ExpiresAt <= now-60 || claims.IssuedAt > now+60 {
		return idTokenClaims{}, errors.New("ID token is expired or issued in the future")
	}
	audiences, err := parseAudience(claims.Audience)
	if err != nil || !contains(audiences, clientID) {
		return idTokenClaims{}, errors.New("ID token audience is invalid")
	}
	if len(audiences) > 1 && claims.AuthorizedParty != clientID {
		return idTokenClaims{}, errors.New("ID token azp is invalid")
	}
	return claims, nil
}

func parseAudience(raw json.RawMessage) ([]string, error) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err != nil {
		return nil, err
	}
	return multiple, nil
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

var errOIDCUserNotProvisioned = errors.New("OIDC user is not provisioned and automatic provisioning is disabled")

func (s *Server) resolveOIDCUser(ctx context.Context, cfg oidcSettings, claims idTokenClaims) (string, error) {
	var id string
	err := s.store.Pool.QueryRow(ctx, `SELECT id FROM users WHERE oidc_subject=$1 AND active=TRUE`, claims.Issuer+"|"+claims.Subject).Scan(&id)
	if err == nil {
		_, _ = s.store.Pool.Exec(ctx, `UPDATE users SET display_name=$2,email=$3,updated_at=now() WHERE id=$1`, id, claims.Name, claims.Email)
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if !cfg.AutoCreateUser {
		return "", errOIDCUserNotProvisioned
	}
	username := store.NormalizeUsername(claims.PreferredUsername)
	if username == "" {
		username = store.NormalizeUsername(claims.Email)
	}
	if username == "" || len(username) > 200 {
		return "", errors.New("OIDC token has no usable username")
	}
	id, err = secure.NewID()
	if err != nil {
		return "", err
	}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `INSERT INTO users(id,username,display_name,email,auth_source,oidc_subject) VALUES($1,$2,$3,$4,'oidc',$5)`, id, username, claims.Name, claims.Email, claims.Issuer+"|"+claims.Subject)
	if err != nil {
		return "", fmt.Errorf("create OIDC user (username may already exist): %w", err)
	}
	if cfg.DefaultRoleID != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO user_roles(user_id,role_id) VALUES($1,$2)`, id, *cfg.DefaultRoleID); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

var _ = store.NormalizeUsername
