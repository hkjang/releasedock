package server

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hkjang/releasedock/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

// MCP 를 SSO 로 — 개인 키 없이, Keycloak 이 발급한 토큰으로.
//
// The MCP authorization specification (2025-06-18 and later) is OAuth 2.1: the
// MCP server is a *resource server* that publishes where its authorization
// server is (RFC 9728, /.well-known/oauth-protected-resource), and a client
// that is refused with 401 reads that document, sends the person through the
// authorization server with PKCE, and comes back with an access token whose
// audience (RFC 8707) is this server. Nothing about issuing tokens happens
// here — Keycloak does that — and this file only has to answer two questions:
// where is the authorization server, and is this token one it issued for us.
//
// The personal key stays. It is what an automation with no person behind it
// uses, and what a deployment without Keycloak uses. A token from SSO is a
// second door into the same room: it authenticates an *existing* ReleaseDock
// account, carries the permission ceiling the administrator chose, and passes
// every gate a key passes (ViaAPIKey). It never creates an account — signing
// in to the web once is what provisions one, and a machine presenting a token
// is not the moment to decide who somebody is. Tokens are accepted on /mcp
// only; REST, streams and administration keep taking keys and sessions.

const (
	mcpPath                  = "/mcp"
	mcpProtectedResourcePath = "/.well-known/oauth-protected-resource"
	mcpOAuthRealm            = "ReleaseDock"
	// mcpOAuthKeyTTL bounds how long a fetched key set is trusted before the
	// issuer is asked again; an unknown kid triggers an earlier refresh.
	mcpOAuthKeyTTL = 5 * time.Minute
	// mcpOAuthKeyRetry is the shortest interval between refreshes forced by an
	// unknown kid, so a stream of forged tokens cannot hammer Keycloak.
	mcpOAuthKeyRetry = 10 * time.Second
	// mcpOAuthClockSkew matches the leeway verifyIDToken already gives.
	mcpOAuthClockSkew = 60
)

func isMCPRequest(r *http.Request) bool { return r.URL.Path == mcpPath }

// mcpOAuthConfig is the resource-server view of the OIDC settings.
type mcpOAuthConfig struct {
	// Enabled is the administrator switch.
	Enabled bool
	// Active is Enabled plus everything the switch needs: an issuer and a
	// resource identifier. Enabled without Active behaves as if switched off
	// and says why in the log.
	Active        bool
	InactiveWhy   string
	Issuer        string
	Resource      string
	Audiences     []string
	Scopes        []string
	AllowInsecure bool
}

// metadataURL is where a refused client is sent to learn the above: the
// well-known path prefixed to the resource's own path (RFC 9728 §3.1).
func (cfg mcpOAuthConfig) metadataURL() string {
	parsed, err := url.Parse(cfg.Resource)
	if err != nil {
		return ""
	}
	parsed.Path = mcpProtectedResourcePath + parsed.Path
	parsed.RawPath = ""
	return parsed.String()
}

// mcpResource is the identifier this deployment claims for its MCP endpoint:
// what the metadata document advertises and what a token's aud must name. An
// explicit setting wins, then the public URL, and that is all: the request
// itself is never consulted, because the resource is what a token's audience
// is checked against, and a caller who could steer it through Host or
// X-Forwarded-Host could present a token minted for some other deployment.
// Without either setting the switch is inactive, and says so.
func (s *Server) mcpResource(ctx context.Context, cfg oidcSettings) string {
	if configured := strings.TrimSpace(cfg.MCPOAuthResource); configured != "" {
		return configured
	}
	if origin, err := s.configuredPublicOrigin(ctx); err == nil && origin != "" {
		return origin + mcpPath
	}
	return ""
}

func (s *Server) mcpOAuthConfig(ctx context.Context) (mcpOAuthConfig, error) {
	settings, err := s.loadOIDC(ctx)
	if err != nil {
		return mcpOAuthConfig{}, err
	}
	return s.mcpOAuthConfigFrom(ctx, settings), nil
}

func (s *Server) mcpOAuthConfigFrom(ctx context.Context, settings oidcSettings) mcpOAuthConfig {
	cfg := mcpOAuthConfig{
		Enabled:       settings.MCPOAuthEnabled,
		Issuer:        strings.TrimSuffix(strings.TrimSpace(settings.Issuer), "/"),
		Resource:      s.mcpResource(ctx, settings),
		Audiences:     settings.MCPOAuthAudience,
		Scopes:        settings.MCPOAuthScopes,
		AllowInsecure: settings.AllowInsecureEndpoints,
	}
	switch {
	case !cfg.Enabled:
		cfg.InactiveWhy = "mcp.oauth.enabled is off"
	case cfg.Issuer == "":
		cfg.InactiveWhy = "oidc.issuer_url is empty"
	case cfg.Resource == "":
		cfg.InactiveWhy = "no resource identifier: set mcp.oauth.resource or the general publicUrl"
	default:
		cfg.Active = true
	}
	return cfg
}

// mcpScopeQueryer is what the permission check needs; pgx.Tx and the pool
// both satisfy it.
type mcpScopeQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// validateMCPOAuthSettings refuses a configuration at save time rather than
// letting the switch sit on doing nothing. The resource must be a URL a
// client can actually reach; the ceiling must name real permissions, because
// a misspelt one would silently grant nothing; and enabling needs an issuer
// and a resource, the two things the metadata document is made of.
func (s *Server) validateMCPOAuthSettings(ctx context.Context, queryer mcpScopeQueryer, cfg oidcSettings) error {
	if resource := cfg.MCPOAuthResource; resource != "" {
		parsed, err := url.Parse(resource)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.ContainsAny(resource, " \t\r\n") {
			return errors.New("mcp.oauth.resource must be an absolute URL without userinfo, query, or fragment, e.g. https://releasedock.company.local/mcp")
		}
		switch parsed.Scheme {
		case "https":
		case "http":
			if !cfg.AllowInsecureEndpoints || !privatePlaintextHost(parsed.Host) {
				return errors.New("mcp.oauth.resource must use HTTPS (plaintext HTTP is accepted only for an internal host with insecure endpoints allowed)")
			}
		default:
			return errors.New("mcp.oauth.resource must use http or https")
		}
		if !strings.HasSuffix(parsed.Path, mcpPath) {
			return errors.New("mcp.oauth.resource must end with " + mcpPath + ", the path MCP clients connect to")
		}
	}
	if len(cfg.MCPOAuthAudience) > 50 {
		return errors.New("mcp.oauth.audience may list at most 50 values")
	}
	for _, audience := range cfg.MCPOAuthAudience {
		if len(audience) > 512 {
			return errors.New("mcp.oauth.audience contains a value longer than 512 characters")
		}
	}
	if len(cfg.MCPOAuthScopes) == 0 {
		return errors.New("mcp.oauth.scopes must name at least one permission, e.g. mcp.use releases.read")
	}
	if len(cfg.MCPOAuthScopes) > 100 {
		return errors.New("mcp.oauth.scopes may list at most 100 permissions")
	}
	rows, err := queryer.Query(ctx, `SELECT code FROM permissions WHERE code=ANY($1)`, cfg.MCPOAuthScopes)
	if err != nil {
		return fmt.Errorf("could not check mcp.oauth.scopes: %w", err)
	}
	defer rows.Close()
	known := map[string]bool{}
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return fmt.Errorf("could not check mcp.oauth.scopes: %w", err)
		}
		known[code] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("could not check mcp.oauth.scopes: %w", err)
	}
	for _, scope := range cfg.MCPOAuthScopes {
		if !known[scope] {
			return errors.New("mcp.oauth.scopes contains an unknown permission: " + scope)
		}
	}
	if !slices.Contains(cfg.MCPOAuthScopes, "mcp.use") {
		return errors.New("mcp.oauth.scopes must include mcp.use, or no SSO token can open /mcp")
	}
	if cfg.MCPOAuthEnabled {
		if strings.TrimSpace(cfg.Issuer) == "" {
			return errors.New("mcp.oauth.enabled requires the Keycloak issuer URL (oidc.issuer_url)")
		}
		if cfg.MCPOAuthResource == "" {
			if origin, err := s.configuredPublicOrigin(ctx); err != nil || origin == "" {
				return errors.New("mcp.oauth.enabled requires a resource identifier: set mcp.oauth.resource or the public URL in general settings")
			}
		}
	}
	return nil
}

// protectedResourceMetadata is RFC 9728: the document a refused MCP client
// reads to find the authorization server. Public by design — it says where to
// sign in, not who is signed in — and CORS-open because MCP clients that run
// inside a browser read it from another origin.
func (s *Server) protectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.mcpOAuthConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load MCP OAuth settings")
		return
	}
	if !cfg.Active {
		if cfg.Enabled {
			s.log.Warn("MCP OAuth is enabled but inactive", "reason", cfg.InactiveWhy)
		}
		writeError(w, http.StatusNotFound, "mcp_oauth_disabled", "이 서버의 MCP 는 SSO 토큰을 받지 않습니다. 개인 API 키(rdk_)를 사용하세요.")
		return
	}
	// The bare document, not the product's {error:…} envelope: the reader is
	// an OAuth client library following RFC 9728.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resource":                 cfg.Resource,
		"authorization_servers":    []string{cfg.Issuer},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         cfg.Scopes,
		"resource_name":            "ReleaseDock MCP",
	})
}

// mcpChallenge turns a 401 on /mcp into an invitation: the MCP client reads
// resource_metadata and starts the OAuth flow from there. Only when SSO is
// active — otherwise the refusal is the refusal it always was — and only on
// the MCP path, because a REST 401 carrying it would send browsers and other
// clients somewhere they cannot follow.
func (s *Server) mcpChallenge(w http.ResponseWriter, r *http.Request, invalidToken bool) {
	cfg, err := s.mcpOAuthConfig(r.Context())
	if err != nil || !cfg.Active {
		return
	}
	header := fmt.Sprintf(`Bearer realm=%q, resource_metadata=%q`, mcpOAuthRealm, cfg.metadataURL())
	if invalidToken {
		header += `, error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", header)
}

// looksLikeJWT is the cheap shape test that separates a key from a token, so
// a bearer that is neither gets the same "invalid key" answer as today.
func looksLikeJWT(token string) bool {
	if len(token) > 32<<10 {
		return false
	}
	parts := strings.Split(token, ".")
	return len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != ""
}

// mcpOAuthRefusal says why a token was not accepted: reason for the log (which
// check failed), message for the client (what to do about it).
type mcpOAuthRefusal struct {
	status  int
	code    string
	reason  string
	message string
}

func refuseToken(reason, message string) *mcpOAuthRefusal {
	return &mcpOAuthRefusal{status: http.StatusUnauthorized, code: "invalid_token", reason: reason, message: message}
}

type accessTokenClaims struct {
	Issuer            string          `json:"iss"`
	Subject           string          `json:"sub"`
	Audience          json.RawMessage `json:"aud"`
	AuthorizedParty   string          `json:"azp"`
	ExpiresAt         int64           `json:"exp"`
	NotBefore         int64           `json:"nbf"`
	IssuedAt          int64           `json:"iat"`
	Type              string          `json:"typ"`
	Scope             string          `json:"scope"`
	Confirmation      json.RawMessage `json:"cnf"`
	PreferredUsername string          `json:"preferred_username"`
}

// oauthPrincipal turns a bearer access token into a principal, or says
// exactly why it will not. The principal is what the same person would get
// from a key: ViaAPIKey so every key gate applies, scopes from the
// administrator ceiling rather than from any role claim in the token.
func (s *Server) oauthPrincipal(ctx context.Context, r *http.Request, token string) (store.Principal, *mcpOAuthRefusal) {
	cfg, err := s.mcpOAuthConfig(ctx)
	if err != nil {
		s.log.Error("load MCP OAuth settings", "error", err)
		return store.Principal{}, &mcpOAuthRefusal{status: http.StatusInternalServerError, code: "database_error", reason: "settings: " + err.Error(), message: "could not load authentication settings"}
	}
	if !cfg.Active {
		// Not switched on, or switched on without what it needs: the refusal
		// must read exactly like the key-only one, so a deployment that never
		// heard of SSO does not start saying new things. The log gets the why.
		return store.Principal{}, &mcpOAuthRefusal{status: http.StatusUnauthorized, code: "unauthorized", reason: "inactive: " + cfg.InactiveWhy, message: "authentication required"}
	}
	claims, refusal := s.verifyAccessToken(ctx, cfg, token)
	if refusal != nil {
		return store.Principal{}, refusal
	}
	// Whom the token was minted for. Measured against a real Keycloak 26: an
	// access token issued to a client carries that client in `azp` and
	// `aud: ["account"]` — the client id is *not* in aud, whatever an ID token
	// does. So the binding is "aud names this resource (the Audience mapper),
	// or aud/azp is a client the administrator listed". Either means the token
	// is for this deployment rather than passed through from another
	// application in the realm, which is what RFC 8707 guards against.
	audiences, _ := parseAudience(claims.Audience)
	if !mcpAudienceAccepted(audiences, claims.AuthorizedParty, cfg.Resource, cfg.Audiences) {
		return store.Principal{}, refuseToken(
			fmt.Sprintf("audience: aud=%v azp=%q not accepted for resource %q", audiences, claims.AuthorizedParty, cfg.Resource),
			fmt.Sprintf("SSO 토큰이 이 서버를 위해 발급된 것이 아닙니다(aud=%v, azp=%q). 관리자가 MCP SSO 설정의 허용 대상에 %q 를 더하거나, Keycloak 클라이언트에 Audience 매퍼로 %q 를 넣으세요.",
				audiences, claims.AuthorizedParty, firstNonEmpty(claims.AuthorizedParty, "<client id>"), cfg.Resource))
	}
	// The same account the web sign-in would find, without the provisioning
	// half: the subject first, then the username claim — but only against an
	// account that came from OIDC, so a realm user named like a local
	// administrator does not become one.
	var userID string
	err = s.store.Pool.QueryRow(ctx, `SELECT id FROM users WHERE active=TRUE AND (oidc_subject=$1 OR (auth_source='oidc' AND lower(username)=lower($2)))
		ORDER BY oidc_subject=$1 DESC LIMIT 1`, cfg.Issuer+"|"+claims.Subject, claims.PreferredUsername).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Principal{}, refuseToken("account: no active account for subject "+claims.Subject,
			"이 SSO 계정은 ReleaseDock 에 등록되지 않았거나 비활성입니다. 먼저 웹으로 한 번 로그인하세요.")
	}
	if err != nil {
		s.log.Error("look up MCP OAuth account", "error", err)
		return store.Principal{}, &mcpOAuthRefusal{status: http.StatusInternalServerError, code: "database_error", reason: "account lookup: " + err.Error(), message: "could not look up the account"}
	}
	p, err := s.storePrincipal(ctx, userID)
	if err != nil {
		s.log.Error("load MCP OAuth principal", "error", err)
		return store.Principal{}, &mcpOAuthRefusal{status: http.StatusInternalServerError, code: "database_error", reason: "principal: " + err.Error(), message: "could not load the account"}
	}
	scopes, refusal := s.mcpOAuthScopes(ctx, cfg, claims.Scope)
	if refusal != nil {
		return store.Principal{}, refusal
	}
	p.ViaAPIKey = true
	p.Scopes = scopes
	return p, nil
}

// mcpAudienceAccepted is the RFC 8707 binding described in oauthPrincipal.
func mcpAudienceAccepted(audiences []string, azp, resource string, allowed []string) bool {
	if resource != "" && slices.Contains(audiences, resource) {
		return true
	}
	for _, value := range audiences {
		if value != "" && slices.Contains(allowed, value) {
			return true
		}
	}
	return azp != "" && slices.Contains(allowed, azp)
}

// mcpOAuthScopes decides what the token may do. The administrator's list is
// the ceiling; a token does not carry ReleaseDock's permission vocabulary
// unless somebody taught Keycloak that vocabulary, and when it does the
// answer is the intersection. An empty intersection is a refusal, never an
// empty list: Principal.Has treats an empty scope list on a key-like
// principal as "nothing", but saying so here tells the person what to fix.
func (s *Server) mcpOAuthScopes(ctx context.Context, cfg mcpOAuthConfig, tokenScope string) ([]string, *mcpOAuthRefusal) {
	ceiling := slices.Clone(cfg.Scopes)
	requested := strings.Fields(tokenScope)
	if len(requested) == 0 {
		return ceiling, nil
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT code FROM permissions WHERE code=ANY($1) ORDER BY code`, requested)
	if err != nil {
		s.log.Error("look up MCP OAuth scopes", "error", err)
		return nil, &mcpOAuthRefusal{status: http.StatusInternalServerError, code: "database_error", reason: "scope lookup: " + err.Error(), message: "could not evaluate token scopes"}
	}
	defer rows.Close()
	var carried []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, &mcpOAuthRefusal{status: http.StatusInternalServerError, code: "database_error", reason: "scope lookup: " + err.Error(), message: "could not evaluate token scopes"}
		}
		carried = append(carried, code)
	}
	if err := rows.Err(); err != nil {
		return nil, &mcpOAuthRefusal{status: http.StatusInternalServerError, code: "database_error", reason: "scope lookup: " + err.Error(), message: "could not evaluate token scopes"}
	}
	scopes, ok := intersectMCPScopes(ceiling, carried)
	if !ok {
		return nil, refuseToken(fmt.Sprintf("scope: token carries %v, none within ceiling %v", carried, ceiling),
			fmt.Sprintf("SSO 토큰의 scope(%s)가 관리자가 허용한 MCP 범위(%s)와 겹치지 않습니다. Keycloak 클라이언트의 scope 를 고치거나 관리자에게 허용 범위를 문의하세요.",
				strings.Join(carried, " "), strings.Join(ceiling, " ")))
	}
	return scopes, nil
}

// intersectMCPScopes gives the ceiling when the token names none of this
// application's permissions, the intersection when it names some, and false
// when it names some but none the administrator allows.
func intersectMCPScopes(ceiling, carried []string) ([]string, bool) {
	if len(carried) == 0 {
		return slices.Clone(ceiling), true
	}
	var scopes []string
	for _, scope := range ceiling {
		if slices.Contains(carried, scope) && !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	return scopes, len(scopes) > 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// verifyAccessToken checks everything about the token that does not need the
// database: signature against the issuer's keys, issuer, time window, and the
// two shapes of token that must never pass as an API credential — an ID token
// (typ ID: proof of a login, not authority to call an API) and a
// sender-constrained token (cnf: bound to a DPoP or mTLS key this server
// cannot verify). Audience is the caller's, because more than one value is
// acceptable and the message has to say which.
func (s *Server) verifyAccessToken(ctx context.Context, cfg mcpOAuthConfig, token string) (accessTokenClaims, *mcpOAuthRefusal) {
	invalid := func(reason string) (accessTokenClaims, *mcpOAuthRefusal) {
		return accessTokenClaims{}, refuseToken(reason, "SSO 액세스 토큰이 유효하지 않습니다(서명·발급자·만료). 클라이언트에서 다시 로그인하세요.")
	}
	parts := strings.Split(token, ".")
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return invalid("header: not base64url")
	}
	var header struct{ Alg, Kid, Typ string }
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return invalid("header: not JSON")
	}
	hash, family, ok := jwsAlgorithm(header.Alg)
	if !ok {
		// HS* would let anybody who can read the public key set sign; none is
		// no signature at all. Neither is a Keycloak access token.
		return invalid("alg: " + header.Alg + " is not an asymmetric JWS algorithm")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return invalid("signature: not base64url")
	}
	publicKey, refusal := s.mcpSigningKey(ctx, cfg, header.Kid, header.Alg)
	if refusal != nil {
		return accessTokenClaims{}, refusal
	}
	if err := verifyJWS(publicKey, hash, family, []byte(parts[0]+"."+parts[1]), signature); err != nil {
		return invalid("signature: " + err.Error())
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return invalid("payload: not base64url")
	}
	var claims accessTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return invalid("payload: not JSON")
	}
	if claims.Issuer != cfg.Issuer {
		return invalid(fmt.Sprintf("iss: %q is not %q", claims.Issuer, cfg.Issuer))
	}
	now := time.Now().Unix()
	if claims.ExpiresAt == 0 || claims.ExpiresAt <= now-mcpOAuthClockSkew {
		return invalid("exp: token expired")
	}
	if claims.NotBefore > now+mcpOAuthClockSkew {
		return invalid("nbf: token not yet valid")
	}
	if claims.IssuedAt > now+mcpOAuthClockSkew {
		return invalid("iat: token issued in the future")
	}
	if strings.EqualFold(claims.Type, "ID") || strings.EqualFold(header.Typ, "ID") {
		return accessTokenClaims{}, refuseToken("typ: ID token presented as access token",
			"ID 토큰은 API 자격이 아닙니다. 클라이언트가 액세스 토큰(typ=Bearer)을 보내야 합니다.")
	}
	if len(claims.Confirmation) > 0 {
		return accessTokenClaims{}, refuseToken("cnf: sender-constrained token",
			"소지자 증명(DPoP·mTLS)이 묶인 토큰은 이 서버가 검증할 수 없습니다. 일반 Bearer 토큰을 발급받으세요.")
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return invalid("sub: empty")
	}
	return claims, nil
}

// jwsAlgorithm maps a JWS alg to its digest and key family. Only asymmetric
// algorithms are listed; anything else is refused before a key is looked up.
func jwsAlgorithm(alg string) (crypto.Hash, string, bool) {
	switch alg {
	case "RS256", "PS256", "ES256":
		return crypto.SHA256, alg[:2], true
	case "RS384", "PS384", "ES384":
		return crypto.SHA384, alg[:2], true
	case "RS512", "PS512", "ES512":
		return crypto.SHA512, alg[:2], true
	}
	return 0, "", false
}

func verifyJWS(key crypto.PublicKey, hash crypto.Hash, family string, signingInput, signature []byte) error {
	var digest []byte
	switch hash {
	case crypto.SHA256:
		sum := sha256.Sum256(signingInput)
		digest = sum[:]
	case crypto.SHA384:
		sum := sha512.Sum384(signingInput)
		digest = sum[:]
	case crypto.SHA512:
		sum := sha512.Sum512(signingInput)
		digest = sum[:]
	default:
		return errors.New("unsupported digest")
	}
	switch family {
	case "RS":
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("key is not RSA")
		}
		return rsa.VerifyPKCS1v15(rsaKey, hash, digest, signature)
	case "PS":
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("key is not RSA")
		}
		return rsa.VerifyPSS(rsaKey, hash, digest, signature, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthAuto, Hash: hash})
	case "ES":
		ecKey, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("key is not EC")
		}
		size := (ecKey.Curve.Params().BitSize + 7) / 8
		if len(signature) != 2*size {
			return errors.New("EC signature has the wrong length")
		}
		r := new(big.Int).SetBytes(signature[:size])
		s := new(big.Int).SetBytes(signature[size:])
		if !ecdsa.Verify(ecKey, digest, r, s) {
			return errors.New("EC signature is invalid")
		}
		return nil
	}
	return errors.New("unsupported key family")
}

// jsonWebKey is the subset of RFC 7517 Keycloak publishes for signing keys.
type jsonWebKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// publicKey rejects anything that is not a signing key of a strength this
// server would accept for its own ID tokens (verifyIDToken).
func (key jsonWebKey) publicKey() (crypto.PublicKey, bool) {
	if key.Use != "" && key.Use != "sig" {
		return nil, false
	}
	switch key.Kty {
	case "RSA":
		nBytes, nErr := base64.RawURLEncoding.DecodeString(key.N)
		eBytes, eErr := base64.RawURLEncoding.DecodeString(key.E)
		if nErr != nil || eErr != nil || len(eBytes) == 0 || len(eBytes) > 4 {
			return nil, false
		}
		var padded [4]byte
		copy(padded[4-len(eBytes):], eBytes)
		exponent := int(binary.BigEndian.Uint32(padded[:]))
		if exponent < 3 || exponent%2 == 0 {
			return nil, false
		}
		public := &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: exponent}
		if public.N.BitLen() < 2048 {
			return nil, false
		}
		return public, true
	case "EC":
		var curve elliptic.Curve
		switch key.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, false
		}
		xBytes, xErr := base64.RawURLEncoding.DecodeString(key.X)
		yBytes, yErr := base64.RawURLEncoding.DecodeString(key.Y)
		if xErr != nil || yErr != nil {
			return nil, false
		}
		public := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(xBytes), Y: new(big.Int).SetBytes(yBytes)}
		if !curve.IsOnCurve(public.X, public.Y) {
			return nil, false
		}
		return public, true
	}
	return nil, false
}

// mcpOAuthKeyCache holds the issuer's signing keys. Discovery and the JWKS
// behind it are network round trips to Keycloak; doing them per request would
// put Keycloak's latency in front of every MCP call. Keys are refreshed after
// mcpOAuthKeyTTL, or sooner when a token names a kid the cache does not have
// (key rotation), but not more often than mcpOAuthKeyRetry — and that
// interval counts failed attempts too, so a dead Keycloak or a stream of
// forged kids is asked about at most once per interval.
//
// The mutex guards the fields only, never the network: a fetch runs with the
// mutex released so a token whose kid is already cached verifies while the
// fetch is in flight, and requests that all need the same fetch share one
// (inflight) rather than each starting their own.
type mcpOAuthKeyCache struct {
	mu     sync.Mutex
	issuer string
	keys   map[string]jsonWebKey
	// fetched is when keys were last read successfully; attempted is the last
	// fetch, successful or not; lastErr is why the last attempt failed.
	fetched   time.Time
	attempted time.Time
	lastErr   error
	// inflight is closed when the fetch in progress has been recorded.
	inflight chan struct{}
}

// needsFetch is the refresh policy, decided under the lock.
func (cache *mcpOAuthKeyCache) needsFetch(issuer, kid string, now time.Time) bool {
	if cache.issuer != issuer {
		return true
	}
	throttled := now.Sub(cache.attempted) <= mcpOAuthKeyRetry
	if cache.keys == nil || now.Sub(cache.fetched) > mcpOAuthKeyTTL {
		return !throttled
	}
	if _, known := cache.keys[kid]; !known {
		return !throttled
	}
	return false
}

// record stores the outcome of a fetch. A failure keeps an older key set for
// the same issuer, but never one that belongs to a different issuer.
func (cache *mcpOAuthKeyCache) record(issuer string, keys map[string]jsonWebKey, err error, now time.Time) {
	cache.attempted = now
	if err == nil {
		cache.issuer, cache.keys, cache.fetched, cache.lastErr = issuer, keys, now, nil
		return
	}
	cache.lastErr = err
	if cache.issuer != issuer {
		cache.issuer, cache.keys, cache.fetched = issuer, nil, time.Time{}
	}
}

func (s *Server) mcpSigningKey(ctx context.Context, cfg mcpOAuthConfig, kid, alg string) (crypto.PublicKey, *mcpOAuthRefusal) {
	if kid == "" {
		return nil, refuseToken("kid: missing", "SSO 액세스 토큰이 유효하지 않습니다(서명·발급자·만료). 클라이언트에서 다시 로그인하세요.")
	}
	unavailable := func(err error) *mcpOAuthRefusal {
		return &mcpOAuthRefusal{status: http.StatusServiceUnavailable, code: "oidc_discovery_failed", reason: "keys: " + err.Error(),
			message: "Keycloak 발급자 정보를 읽지 못해 SSO 토큰을 확인할 수 없습니다. 잠시 후 다시 시도하거나 관리자에게 알리세요."}
	}
	cache := &s.mcpOAuthKeys
	cache.mu.Lock()
	if cache.needsFetch(cfg.Issuer, kid, time.Now()) {
		if cache.inflight == nil {
			// This request fetches; anything else that decides it needs the
			// same fetch while the lock is released waits for this one.
			done := make(chan struct{})
			cache.inflight = done
			cache.mu.Unlock()
			keys, err := s.fetchMCPSigningKeys(ctx, cfg)
			cache.mu.Lock()
			cache.record(cfg.Issuer, keys, err, time.Now())
			cache.inflight = nil
			close(done)
			if err != nil {
				if cache.keys == nil {
					s.log.Warn("MCP OAuth key fetch", "issuer", cfg.Issuer, "error", err)
				} else {
					s.log.Warn("MCP OAuth key refresh failed; keeping the previous key set", "issuer", cfg.Issuer, "error", err)
				}
			}
		} else {
			done := cache.inflight
			cache.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return nil, unavailable(fmt.Errorf("waiting for the issuer's key set: %w", ctx.Err()))
			}
			cache.mu.Lock()
		}
	}
	if cache.issuer != cfg.Issuer || cache.keys == nil {
		// Nothing to check against and nothing older to fall back on: say so
		// rather than accept or refuse on a guess. Within mcpOAuthKeyRetry
		// this is the cached failure, not a new round trip.
		err := cache.lastErr
		cache.mu.Unlock()
		if err == nil {
			err = errors.New("no key set for issuer " + cfg.Issuer)
		}
		return nil, unavailable(err)
	}
	key, known := cache.keys[kid]
	cache.mu.Unlock()
	if !known {
		return nil, refuseToken("kid: "+kid+" not in the issuer's key set", "SSO 액세스 토큰이 유효하지 않습니다(서명·발급자·만료). 클라이언트에서 다시 로그인하세요.")
	}
	if key.Alg != "" && key.Alg != alg {
		return nil, refuseToken("alg: token "+alg+" does not match key "+key.Alg, "SSO 액세스 토큰이 유효하지 않습니다(서명·발급자·만료). 클라이언트에서 다시 로그인하세요.")
	}
	public, ok := key.publicKey()
	if !ok {
		return nil, refuseToken("kid: "+kid+" is not a usable signing key", "SSO 액세스 토큰이 유효하지 않습니다(서명·발급자·만료). 클라이언트에서 다시 로그인하세요.")
	}
	return public, nil
}

func (s *Server) fetchMCPSigningKeys(ctx context.Context, cfg mcpOAuthConfig) (map[string]jsonWebKey, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	discovery, err := s.discoverOIDC(ctx, cfg.Issuer, cfg.AllowInsecure)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, discovery.JWKSURI, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("JWKS request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS returned HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []jsonWebKey `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, (2<<20)+1)).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}
	keys := make(map[string]jsonWebKey, len(set.Keys))
	for _, key := range set.Keys {
		if key.Kid != "" {
			keys[key.Kid] = key
		}
	}
	return keys, nil
}
