package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/hkjang/releasedock/backend/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MCP 를 개인 키 없이 Keycloak 토큰으로.
//
// The authorization flow itself — PKCE, the redirect, the code exchange — is
// Keycloak's and the client's. What is this server's is the resource-server
// half of the specification, and that is what these tests hold it to: it says
// where the authorization server is, it turns a 401 into a pointer there, and
// it accepts exactly the tokens that server issued for this resource, for a
// person ReleaseDock already knows, with the powers a key would have and no
// more. Every request below goes through Handler(), the production wiring.

func TestLooksLikeJWTSeparatesTokensFromKeys(t *testing.T) {
	for value, want := range map[string]bool{
		"a.b.c": true, "rdk_abcdef": false, "a.b": false, "a..c": false, ".b.c": false, "a.b.": false, "": false,
	} {
		if got := looksLikeJWT(value); got != want {
			t.Errorf("looksLikeJWT(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestMetadataURLPrefixesTheWellKnownPath(t *testing.T) {
	cfg := mcpOAuthConfig{Resource: "https://releasedock.example.test/mcp"}
	if got := cfg.metadataURL(); got != "https://releasedock.example.test/.well-known/oauth-protected-resource/mcp" {
		t.Fatalf("metadataURL = %q", got)
	}
}

func TestAudienceBindingAcceptsResourceOrListedClient(t *testing.T) {
	resource := "https://releasedock.example.test/mcp"
	allowed := []string{"releasedock-mcp"}
	cases := []struct {
		name string
		aud  []string
		azp  string
		want bool
	}{
		{"audience mapper", []string{"account", resource}, "anything", true},
		{"listed azp", []string{"account"}, "releasedock-mcp", true},
		{"listed aud", []string{"releasedock-mcp"}, "", true},
		{"other application", []string{"account"}, "some-other-app", false},
		{"nothing bound", nil, "", false},
		{"empty azp never matches", []string{"account"}, "", false},
	}
	for _, tc := range cases {
		if got := mcpAudienceAccepted(tc.aud, tc.azp, resource, allowed); got != tc.want {
			t.Errorf("%s: accepted=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestScopeIntersectionRefusesInsteadOfGrantingNothing(t *testing.T) {
	ceiling := []string{"mcp.use", "releases.read"}
	if scopes, ok := intersectMCPScopes(ceiling, nil); !ok || strings.Join(scopes, " ") != "mcp.use releases.read" {
		t.Fatalf("a token without this application's vocabulary should get the ceiling: %v %v", scopes, ok)
	}
	if scopes, ok := intersectMCPScopes(ceiling, []string{"releases.read"}); !ok || strings.Join(scopes, " ") != "releases.read" {
		t.Fatalf("a token naming part of the ceiling should get that part: %v %v", scopes, ok)
	}
	// Principal.Has reads an empty scope list on a key-like principal as
	// "nothing", so returning one would be a silent lockout rather than a
	// privilege escalation — but it would still be silent.
	if scopes, ok := intersectMCPScopes(ceiling, []string{"releases.write"}); ok || len(scopes) != 0 {
		t.Fatalf("a token naming only permissions outside the ceiling must be refused: %v %v", scopes, ok)
	}
}

func TestJWSAlgorithmRefusesSymmetricAndNone(t *testing.T) {
	for _, alg := range []string{"HS256", "HS512", "none", "", "RS255"} {
		if _, _, ok := jwsAlgorithm(alg); ok {
			t.Errorf("%q accepted", alg)
		}
	}
	for _, alg := range []string{"RS256", "PS384", "ES512"} {
		if _, _, ok := jwsAlgorithm(alg); !ok {
			t.Errorf("%q refused", alg)
		}
	}
}

// fakeIDP is Keycloak as far as this server can tell: a discovery document, a
// key set, and a signing key. Everything an MCP client would do between the
// two (the redirect, the code exchange) happens elsewhere.
type fakeIDP struct {
	server *httptest.Server
	rsaKey *rsa.PrivateKey
	ecKey  *ecdsa.PrivateKey
	kid    string
	ecKid  string

	// Knobs for the key-cache tests: every request to the IdP is counted;
	// the JWKS handler signals entered (if set), then waits for block (if
	// set) before answering, and answers 500 while fail is set.
	mu      sync.Mutex
	hits    int
	entered chan struct{}
	block   chan struct{}
	fail    bool
}

func (idp *fakeIDP) requests() int {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.hits
}

func (idp *fakeIDP) setJWKS(entered, block chan struct{}, fail bool) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.entered, idp.block, idp.fail = entered, block, fail
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIDP{rsaKey: rsaKey, ecKey: ecKey, kid: "rsa-key", ecKid: "ec-key"}
	idp.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idp.mu.Lock()
		idp.hits++
		entered, block, fail := idp.entered, idp.block, idp.fail
		idp.mu.Unlock()
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": idp.server.URL, "authorization_endpoint": idp.server.URL + "/auth",
				"token_endpoint": idp.server.URL + "/token", "jwks_uri": idp.server.URL + "/jwks",
			})
		case "/jwks":
			if entered != nil {
				select {
				case entered <- struct{}{}:
				default:
				}
			}
			if block != nil {
				<-block
			}
			if fail {
				http.Error(w, "keycloak is having a moment", http.StatusInternalServerError)
				return
			}
			e := big.NewInt(int64(rsaKey.E)).Bytes()
			size := (ecKey.Curve.Params().BitSize + 7) / 8
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
				{"kty": "RSA", "kid": idp.kid, "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e)},
				{"kty": "EC", "kid": idp.ecKid, "use": "sig", "crv": "P-256", "x": base64.RawURLEncoding.EncodeToString(ecKey.X.FillBytes(make([]byte, size))), "y": base64.RawURLEncoding.EncodeToString(ecKey.Y.FillBytes(make([]byte, size)))},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idp.server.Close)
	return idp
}

// accessToken is what Keycloak would hand an MCP client after the person
// signed in: signed by the realm key, issued by the issuer, for an audience.
func (idp *fakeIDP) accessToken(t *testing.T, audience any, claims map[string]any) string {
	t.Helper()
	payload := map[string]any{
		"iss": idp.server.URL, "aud": audience, "sub": "subject-mcp", "azp": "releasedock-mcp",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"typ": "Bearer", "preferred_username": "ssomember",
	}
	for key, value := range claims {
		payload[key] = value
	}
	return idp.sign(t, map[string]any{"alg": "RS256", "kid": idp.kid, "typ": "JWT"}, payload)
}

func (idp *fakeIDP) sign(t *testing.T, header, payload map[string]any) string {
	t.Helper()
	encodedHeader, _ := json.Marshal(header)
	encodedPayload, _ := json.Marshal(payload)
	signingInput := base64.RawURLEncoding.EncodeToString(encodedHeader) + "." + base64.RawURLEncoding.EncodeToString(encodedPayload)
	digest := sha256.Sum256([]byte(signingInput))
	var signature []byte
	switch header["alg"] {
	case "RS256":
		var err error
		signature, err = rsa.SignPKCS1v15(rand.Reader, idp.rsaKey, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
	case "ES256":
		r, s, err := ecdsa.Sign(rand.Reader, idp.ecKey, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		signature = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	default:
		// HS256 and friends: any bytes, the server must refuse before looking.
		signature = digest[:]
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// logSink captures the operator-facing log so a test can hold the server to
// saying *which* check refused a token.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type mcpOAuthFixture struct {
	server   *Server
	handler  http.Handler
	store    *store.Store
	idp      *fakeIDP
	logs     *logSink
	memberID string
	adminID  string
	apiKey   string
	resource string
}

// newMCPOAuthFixture is a schema-isolated database with SSO configured
// against the fake IdP but MCP-over-SSO still switched off, an OIDC-provisioned
// viewer, an inactive OIDC account, and an administrator with a personal key.
func newMCPOAuthFixture(t *testing.T) *mcpOAuthFixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	adminPool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect to integration PostgreSQL: %v", err)
	}
	t.Cleanup(adminPool.Close)
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	schema := "releasedock_mcp_oauth_it_" + hex.EncodeToString(suffix)
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(t.Context(), "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create integration schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE")
	})
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	st := &store.Store{Pool: pool}
	t.Cleanup(st.Close)
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	idp := newFakeIDP(t)
	fixture := &mcpOAuthFixture{store: st, idp: idp, logs: &logSink{}, memberID: "sso-member", adminID: "sso-admin", resource: "https://releasedock.example.test/mcp"}
	random, _ := secure.RandomToken(32)
	fixture.apiKey = store.APIKeyPrefix + random
	seed := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id,username,display_name,auth_source,oidc_subject) VALUES($1,'ssomember','SSO member','oidc',$2)`, []any{fixture.memberID, idp.server.URL + "|subject-mcp"}},
		{`INSERT INTO users(id,username,display_name,auth_source,oidc_subject,active) VALUES('sso-inactive','ssoinactive','Gone','oidc',$1,FALSE)`, []any{idp.server.URL + "|subject-inactive"}},
		{`INSERT INTO users(id,username,display_name,auth_source) VALUES($1,'ssoadmin','Admin','local')`, []any{fixture.adminID}},
		{`INSERT INTO user_roles(user_id,role_id) VALUES($1,'role-viewer'),($2,'role-admin')`, []any{fixture.memberID, fixture.adminID}},
		{`INSERT INTO api_keys(id,user_id,name,prefix,secret_hash,scopes) VALUES('key-1',$1,'ci',$2,$3,ARRAY['mcp.use','releases.read'])`, []any{fixture.memberID, fixture.apiKey[:12], secure.TokenHash(fixture.apiKey)}},
		// The issuer is the fake IdP on loopback HTTP, which is exactly the
		// air-gapped Keycloak case the insecure-endpoints switch exists for.
		{`UPDATE oidc_settings SET issuer=$1,client_id='releasedock-web',allow_insecure_endpoints=TRUE,mcp_oauth_audience=ARRAY['releasedock-mcp'] WHERE id='default'`, []any{idp.server.URL}},
		{`UPDATE app_settings SET general_config='{"publicUrl":"https://releasedock.example.test"}'::jsonb WHERE id='default'`, nil},
	}
	for _, statement := range seed {
		if _, err := st.Pool.Exec(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	vault, err := secure.NewVault([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.server = New(st, vault, slog.New(slog.NewTextHandler(fixture.logs, nil)), BuildInfo{}, "")
	fixture.handler = fixture.server.Handler()
	return fixture
}

func (f *mcpOAuthFixture) enable(t *testing.T, enabled bool) {
	t.Helper()
	if _, err := f.store.Pool.Exec(t.Context(), `UPDATE oidc_settings SET mcp_oauth_enabled=$1 WHERE id='default'`, enabled); err != nil {
		t.Fatal(err)
	}
}

const listTools = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`

// mcp sends one legacy-protocol MCP call with the given bearer; extra headers
// are what a proxy in front of the server might add.
func (f *mcpOAuthFixture) mcp(bearer, body string, headers ...string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", mcpLegacyProtocolVersion)
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

// refusalLog is the log line written for the request that got response, found
// by the request ID the client was handed, so a test can hold the server to
// having said why *this* token was refused — not some earlier one.
func (f *mcpOAuthFixture) refusalLog(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	id := response.Header().Get("X-Request-ID")
	if id == "" {
		t.Fatalf("the response carries no X-Request-ID")
	}
	for _, line := range strings.Split(f.logs.String(), "\n") {
		if strings.Contains(line, "request_id="+id) {
			return line
		}
	}
	t.Errorf("no log line carries request_id=%s:\n%s", id, f.logs.String())
	return ""
}

func (f *mcpOAuthFixture) get(path, bearer string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

// guards: protectedResourceMetadata, mcpChallenge, withAuth
func TestMCPOAuthOffByDefaultChangesNothing(t *testing.T) {
	f := newMCPOAuthFixture(t)

	for _, path := range []string{mcpProtectedResourcePath, mcpProtectedResourcePath + mcpPath} {
		if response := f.get(path, ""); response.Code != http.StatusNotFound {
			t.Fatalf("%s served with SSO off: %d %s", path, response.Code, response.Body.String())
		}
	}
	refusal := f.mcp("", listTools)
	if refusal.Code != http.StatusUnauthorized || refusal.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("a 401 pointed at an authorization server that is not switched on: %d %q", refusal.Code, refusal.Header().Get("WWW-Authenticate"))
	}
	// A real token is refused in exactly the words a bad key gets: a
	// deployment that never heard of SSO must not start saying new things.
	token := f.idp.accessToken(t, f.resource, nil)
	withToken := f.mcp(token, listTools)
	if withToken.Code != http.StatusUnauthorized || withToken.Header().Get("WWW-Authenticate") != "" || withToken.Body.String() != refusal.Body.String() {
		t.Fatalf("token with SSO off: %d %q %s", withToken.Code, withToken.Header().Get("WWW-Authenticate"), withToken.Body.String())
	}
	// The log says why, under the request ID the client was handed.
	if line := f.refusalLog(t, withToken); !strings.Contains(line, "inactive: mcp.oauth.enabled is off") {
		t.Errorf("the log does not say why the token was ignored:\n%s", f.logs.String())
	}
	// And the key works as it always did.
	if opened := f.mcp(f.apiKey, listTools); opened.Code != http.StatusOK || !strings.Contains(opened.Body.String(), "releasedock_list_releases") {
		t.Fatalf("personal key refused: %d %s", opened.Code, opened.Body.String())
	}
}

// guards: protectedResourceMetadata, mcpChallenge
func TestARefusedMCPClientIsToldWhereToSignIn(t *testing.T) {
	f := newMCPOAuthFixture(t)
	f.enable(t, true)

	// RFC 9728: the resource names itself and its authorization server, as a
	// bare document — the reader is an OAuth library, not this product's SPA.
	for _, path := range []string{mcpProtectedResourcePath, mcpProtectedResourcePath + mcpPath} {
		response := f.get(path, "")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
		if response.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: metadata is not CORS-readable", path)
		}
		var metadata struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
			BearerMethods        []string `json:"bearer_methods_supported"`
			Scopes               []string `json:"scopes_supported"`
			Error                any      `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
			t.Fatalf("decode %s: %v", response.Body.String(), err)
		}
		if metadata.Error != nil {
			t.Errorf("%s: wrapped in the product envelope: %s", path, response.Body.String())
		}
		if metadata.Resource != f.resource {
			t.Errorf("resource %q, want the public URL plus /mcp", metadata.Resource)
		}
		if len(metadata.AuthorizationServers) != 1 || metadata.AuthorizationServers[0] != f.idp.server.URL {
			t.Errorf("authorization servers %v, want the configured issuer", metadata.AuthorizationServers)
		}
		if strings.Join(metadata.BearerMethods, ",") != "header" || len(metadata.Scopes) == 0 {
			t.Errorf("bearer methods %v scopes %v", metadata.BearerMethods, metadata.Scopes)
		}
	}

	// The 401 now carries the pointer. Without it the client has no way to
	// discover the document above, and the refusal is a dead end.
	refusal := f.mcp("", listTools)
	if refusal.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d", refusal.Code)
	}
	header := refusal.Header().Get("WWW-Authenticate")
	want := `Bearer realm="ReleaseDock", resource_metadata="https://releasedock.example.test/.well-known/oauth-protected-resource/mcp"`
	if header != want {
		t.Errorf("WWW-Authenticate = %q, want %q", header, want)
	}
	// A refused token adds error="invalid_token" so the client re-authorizes
	// instead of retrying the same token.
	refusedToken := f.mcp(f.idp.accessToken(t, "https://other-app.example.test", map[string]any{"azp": "other-app"}), listTools)
	if refusedToken.Code != http.StatusUnauthorized || !strings.HasSuffix(refusedToken.Header().Get("WWW-Authenticate"), `, error="invalid_token"`) {
		t.Errorf("refused token challenge: %d %q", refusedToken.Code, refusedToken.Header().Get("WWW-Authenticate"))
	}
	// But not on REST: a browser or a REST client sent to an OAuth flow it
	// cannot complete is worse off than a plain 401.
	rest := f.get("/api/v1/me", "")
	if rest.Code != http.StatusUnauthorized || rest.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("REST 401 carries the MCP challenge: %d %q", rest.Code, rest.Header().Get("WWW-Authenticate"))
	}
}

// guards: oauthPrincipal, withAuth
func TestAKeycloakTokenOpensMCPForAnAccountReleaseDockKnows(t *testing.T) {
	f := newMCPOAuthFixture(t)
	f.enable(t, true)

	// The formal path: an Audience mapper put the resource in aud.
	token := f.idp.accessToken(t, []string{"account", f.resource}, map[string]any{"azp": "unrelated-client"})
	opened := f.mcp(token, listTools)
	if opened.Code != http.StatusOK || !strings.Contains(opened.Body.String(), "releasedock_list_releases") {
		t.Fatalf("a token for this resource was refused: %d %s", opened.Code, opened.Body.String())
	}
	// The plain path, measured against a real Keycloak 26: aud is just
	// "account" and the client is in azp, which the administrator listed.
	viaClient := f.mcp(f.idp.accessToken(t, "account", map[string]any{"azp": "releasedock-mcp"}), listTools)
	if viaClient.Code != http.StatusOK {
		t.Fatalf("a token issued to the listed client was refused: %d %s", viaClient.Code, viaClient.Body.String())
	}
	// ES256 is as good as RS256; only symmetric and unsigned are out.
	esToken := f.idp.sign(t, map[string]any{"alg": "ES256", "kid": f.idp.ecKid, "typ": "JWT"}, map[string]any{
		"iss": f.idp.server.URL, "aud": "account", "azp": "releasedock-mcp", "sub": "subject-mcp", "typ": "Bearer",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	})
	if es := f.mcp(esToken, listTools); es.Code != http.StatusOK {
		t.Fatalf("an ES256 token was refused: %d %s", es.Code, es.Body.String())
	}

	// The token is the person, with a key's powers: the ceiling the
	// administrator set (viewer permissions within it work) and nothing the
	// ceiling does not name (the viewer role would allow keys.manage, the
	// ceiling does not).
	call := func(name string, arguments map[string]any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": name, "arguments": arguments}})
		return f.mcp(token, string(body))
	}
	if dashboard := call("releasedock_dashboard", map[string]any{}); dashboard.Code != http.StatusOK || strings.Contains(dashboard.Body.String(), "permission denied") {
		t.Fatalf("dashboard within the ceiling: %d %s", dashboard.Code, dashboard.Body.String())
	}
	if create := call("releasedock_create_release", map[string]any{"applicationId": "11000000-0000-4000-8000-000000000001", "environmentId": "22000000-0000-4000-8000-000000000001", "deploymentProfileId": "33000000-0000-4000-8000-000000000001", "version": "1"}); !strings.Contains(create.Body.String(), "permission denied") {
		t.Fatalf("a write outside the ceiling went through: %d %s", create.Code, create.Body.String())
	}
	if keys := f.get("/api/v1/me/api-keys", token); keys.Code != http.StatusUnauthorized || keys.Header().Get("WWW-Authenticate") != "" {
		// The token is an MCP credential only. REST gets the answer a
		// non-key bearer always got, with no challenge attached.
		t.Fatalf("an SSO token opened a REST route: %d %s", keys.Code, keys.Body.String())
	}
	// And a token issued to some other application in the same realm is not
	// ours, however real its signature — that is the passthrough RFC 8707
	// exists to stop. The refusal says what it saw and what to change.
	other := f.mcp(f.idp.accessToken(t, "account", map[string]any{"azp": "some-other-app"}), listTools)
	if other.Code != http.StatusUnauthorized {
		t.Fatalf("a token issued to another application opened MCP: %d %s", other.Code, other.Body.String())
	}
	var refusal struct {
		Error struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(other.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"aud=[account]", `azp="some-other-app"`, `허용 대상에 "some-other-app"`, `Audience 매퍼로 "` + f.resource + `"`} {
		if !strings.Contains(refusal.Error.Message, want) {
			t.Errorf("the audience refusal does not say %q: %s", want, refusal.Error.Message)
		}
	}
}

// guards: verifyAccessToken, oauthPrincipal, withAuth
func TestATokenIsRefusedForTheRightReason(t *testing.T) {
	f := newMCPOAuthFixture(t)
	f.enable(t, true)
	var usersBefore int
	if err := f.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&usersBefore); err != nil {
		t.Fatal(err)
	}
	other := newFakeIDP(t)
	past := time.Now().Add(-time.Hour).Unix()
	future := time.Now().Add(time.Hour).Unix()
	cases := []struct {
		name        string
		bearer      string
		wantMessage string
		wantLog     string
	}{
		{"expired", f.idp.accessToken(t, f.resource, map[string]any{"exp": past}), "유효하지 않습니다", "exp: token expired"},
		{"not yet valid", f.idp.accessToken(t, f.resource, map[string]any{"nbf": future}), "유효하지 않습니다", "nbf: token not yet valid"},
		{"foreign issuer", f.idp.accessToken(t, f.resource, map[string]any{"iss": "https://elsewhere.example.test"}), "유효하지 않습니다", "iss:"},
		{"foreign key", other.accessToken(t, f.resource, map[string]any{"iss": f.idp.server.URL}), "유효하지 않습니다", "signature:"},
		{"symmetric signature", f.idp.sign(t, map[string]any{"alg": "HS256", "kid": f.idp.kid}, map[string]any{"iss": f.idp.server.URL, "aud": f.resource, "sub": "subject-mcp", "exp": future}), "유효하지 않습니다", "alg: HS256"},
		{"id token", f.idp.accessToken(t, f.resource, map[string]any{"typ": "ID"}), "ID 토큰", "typ: ID token"},
		{"sender-constrained", f.idp.accessToken(t, f.resource, map[string]any{"cnf": map[string]any{"jkt": "thumbprint"}}), "소지자 증명", "cnf:"},
		{"no subject", f.idp.accessToken(t, f.resource, map[string]any{"sub": ""}), "유효하지 않습니다", "sub: empty"},
		{"unknown account", f.idp.accessToken(t, f.resource, map[string]any{"sub": "stranger", "preferred_username": "nobody-here"}), "먼저 웹으로", "account: no active account"},
		{"inactive account", f.idp.accessToken(t, f.resource, map[string]any{"sub": "subject-inactive", "preferred_username": "ssoinactive"}), "먼저 웹으로", "account: no active account"},
		{"local account by username", f.idp.accessToken(t, f.resource, map[string]any{"sub": "impostor", "preferred_username": "ssoadmin"}), "먼저 웹으로", "account: no active account"},
		{"scope outside ceiling", f.idp.accessToken(t, f.resource, map[string]any{"scope": "openid releases.write"}), "겹치지 않습니다", "scope: token carries [releases.write]"},
	}
	for _, tc := range cases {
		response := f.mcp(tc.bearer, listTools)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d %s", tc.name, response.Code, response.Body.String())
			continue
		}
		if !strings.Contains(response.Body.String(), tc.wantMessage) {
			t.Errorf("%s: the refusal does not say %q: %s", tc.name, tc.wantMessage, response.Body.String())
		}
		// The line for *this* request — found by its request ID, so a
		// refusal logged for an earlier case cannot satisfy this one — names
		// the check that failed.
		if line := f.refusalLog(t, response); !strings.Contains(line, tc.wantLog) {
			t.Errorf("%s: the log does not name the failed check %q under request_id=%s: %s", tc.name, tc.wantLog, response.Header().Get("X-Request-ID"), line)
		}
		if strings.Contains(f.logs.String(), tc.bearer) {
			t.Errorf("%s: the token itself was logged", tc.name)
		}
	}
	var usersAfter int
	if err := f.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&usersAfter); err != nil {
		t.Fatal(err)
	}
	if usersAfter != usersBefore {
		t.Errorf("a token provisioned an account: %d users, had %d", usersAfter, usersBefore)
	}
	// A token naming part of the ceiling gets exactly that part.
	narrowed := f.idp.accessToken(t, f.resource, map[string]any{"scope": "openid mcp.use"})
	if listed := f.mcp(narrowed, listTools); listed.Code != http.StatusOK {
		t.Fatalf("a token narrowed to mcp.use cannot list tools: %d %s", listed.Code, listed.Body.String())
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "releasedock_dashboard", "arguments": map[string]any{}}})
	if denied := f.mcp(narrowed, string(body)); !strings.Contains(denied.Body.String(), "permission denied") {
		t.Errorf("a token narrowed to mcp.use still read the dashboard: %s", denied.Body.String())
	}
	// A personal key is untouched by any of this.
	if opened := f.mcp(f.apiKey, listTools); opened.Code != http.StatusOK {
		t.Fatalf("personal key refused with SSO on: %d %s", opened.Code, opened.Body.String())
	}
	// Garbage that is neither key nor token gets the answer it always got.
	if garbage := f.mcp("not-a-token", listTools); garbage.Code != http.StatusUnauthorized || !strings.Contains(garbage.Body.String(), "authentication required") {
		t.Fatalf("garbage bearer: %d %s", garbage.Code, garbage.Body.String())
	}
}

// guards: mcpOAuthConfigFrom, protectedResourceMetadata
func TestEnabledWithoutAnIssuerBehavesAsOffAndSaysWhy(t *testing.T) {
	f := newMCPOAuthFixture(t)
	f.enable(t, true)
	if _, err := f.store.Pool.Exec(t.Context(), `UPDATE oidc_settings SET issuer='' WHERE id='default'`); err != nil {
		t.Fatal(err)
	}
	if response := f.get(mcpProtectedResourcePath+mcpPath, ""); response.Code != http.StatusNotFound {
		t.Fatalf("metadata served without an issuer: %d", response.Code)
	}
	if refusal := f.mcp(f.idp.accessToken(t, f.resource, nil), listTools); refusal.Code != http.StatusUnauthorized || refusal.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("token without an issuer: %d %q", refusal.Code, refusal.Header().Get("WWW-Authenticate"))
	}
	if !strings.Contains(f.logs.String(), "oidc.issuer_url is empty") {
		t.Errorf("the log does not say why SSO is inactive:\n%s", f.logs.String())
	}
}

// guards: mcpResource, mcpOAuthConfigFrom, putGeneralSettings
func TestTheResourceIdentifierNeverComesFromTheRequest(t *testing.T) {
	f := newMCPOAuthFixture(t)
	f.enable(t, true)
	// Neither mcp.oauth.resource nor a public URL: the only thing left that
	// could name a resource is the request itself, and a request is the
	// caller's to shape. Through a trusted proxy the caller even picks the
	// host — httptest requests come from 192.0.2.1, so that is the proxy.
	if _, err := f.store.Pool.Exec(t.Context(), `UPDATE app_settings SET general_config='{}'::jsonb WHERE id='default'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Pool.Exec(t.Context(), `UPDATE network_settings SET trusted_proxy_cidrs=ARRAY['192.0.2.0/24'] WHERE id='default'`); err != nil {
		t.Fatal(err)
	}
	f.server.invalidateNetworkSettings()
	forged := "http://other-releasedock.internal/mcp"
	token := f.idp.accessToken(t, forged, map[string]any{"azp": "unrelated-client"})
	response := f.mcp(token, listTools, "X-Forwarded-Host", "other-releasedock.internal", "X-Forwarded-Proto", "http")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("a token whose audience is the forwarded host opened MCP: %d %s", response.Code, response.Body.String())
	}
	// Inactive, not merely refused: the plain 401 with no challenge, the
	// metadata gone, and the log saying what is missing.
	if response.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("challenge issued without a resource identifier: %q", response.Header().Get("WWW-Authenticate"))
	}
	if line := f.refusalLog(t, response); !strings.Contains(line, "inactive: no resource identifier") {
		t.Errorf("the log does not say the switch is inactive for want of a resource: %s", line)
	}
	if metadata := f.get(mcpProtectedResourcePath+mcpPath, ""); metadata.Code != http.StatusNotFound {
		t.Errorf("metadata served without a resource identifier: %d %s", metadata.Code, metadata.Body.String())
	}
	// The same token through the same proxy is not accepted with a public
	// URL either, because the resource is then that URL, not the header.
	if _, err := f.store.Pool.Exec(t.Context(), `UPDATE app_settings SET general_config='{"publicUrl":"https://releasedock.example.test"}'::jsonb WHERE id='default'`); err != nil {
		t.Fatal(err)
	}
	if again := f.mcp(token, listTools, "X-Forwarded-Host", "other-releasedock.internal", "X-Forwarded-Proto", "http"); again.Code != http.StatusUnauthorized || !strings.Contains(f.refusalLog(t, again), "audience:") {
		t.Errorf("forwarded host accepted as audience beside a public URL: %d %s", again.Code, again.Body.String())
	}
	// And an administrator cannot clear the public URL from under the
	// switch: the save is refused with the reason, and SSO keeps working.
	cleared := f.adminRequest(t, http.MethodPut, "/api/v1/admin/settings/general", map[string]any{"serviceName": "ReleaseDock", "publicUrl": ""})
	if cleared.Code != http.StatusBadRequest || !strings.Contains(cleared.Body.String(), "mcp.oauth.resource") {
		t.Errorf("clearing publicUrl while MCP SSO depends on it: %d %s", cleared.Code, cleared.Body.String())
	}
	if opened := f.mcp(f.idp.accessToken(t, f.resource, nil), listTools); opened.Code != http.StatusOK {
		t.Errorf("SSO stopped working after the refused save: %d %s", opened.Code, opened.Body.String())
	}
}

// guards: mcpSigningKey, mcpOAuthKeyCache
func TestKeyFetchesNeitherBlockOtherTokensNorHammerTheIssuer(t *testing.T) {
	f := newMCPOAuthFixture(t)
	f.enable(t, true)
	cache := &f.server.mcpOAuthKeys
	backdate := func(by time.Duration) {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		cache.fetched, cache.attempted = cache.fetched.Add(-by), cache.attempted.Add(-by)
	}
	valid := f.idp.accessToken(t, f.resource, nil)
	if warm := f.mcp(valid, listTools); warm.Code != http.StatusOK {
		t.Fatalf("first token: %d %s", warm.Code, warm.Body.String())
	}
	unknownKid := f.idp.sign(t, map[string]any{"alg": "RS256", "kid": "rotated-key", "typ": "JWT"}, map[string]any{
		"iss": f.idp.server.URL, "aud": f.resource, "sub": "subject-mcp", "typ": "Bearer",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	})

	// (a) A kid the cache does not have sends the server back to the issuer.
	// While that round trip is in flight — Keycloak being slow — a token
	// signed with a key the cache already holds must verify without waiting.
	entered, block := make(chan struct{}, 1), make(chan struct{})
	f.idp.setJWKS(entered, block, false)
	backdate(mcpOAuthKeyRetry + time.Second)
	fetching := make(chan *httptest.ResponseRecorder, 1)
	go func() { fetching <- f.mcp(unknownKid, listTools) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the unknown kid did not send the server to the issuer")
	}
	verified := make(chan *httptest.ResponseRecorder, 1)
	go func() { verified <- f.mcp(valid, listTools) }()
	select {
	case response := <-verified:
		if response.Code != http.StatusOK {
			t.Errorf("a cached-key token during a fetch: %d %s", response.Code, response.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Error("a token with a cached kid waited on the unknown kid's key fetch")
	}
	close(block)
	rotated := <-fetching
	if rotated.Code != http.StatusUnauthorized || !strings.Contains(f.refusalLog(t, rotated), "kid: rotated-key not in the issuer's key set") {
		t.Errorf("unknown kid after the fetch: %d %s", rotated.Code, rotated.Body.String())
	}
	// (b) A second unknown kid inside the retry window is refused from the
	// cache; the issuer is not asked again.
	hits := f.idp.requests()
	if second := f.mcp(unknownKid, listTools); second.Code != http.StatusUnauthorized {
		t.Errorf("second unknown kid: %d %s", second.Code, second.Body.String())
	}
	if f.idp.requests() != hits {
		t.Errorf("a second unknown kid within %s hit the issuer again (%d requests, had %d)", mcpOAuthKeyRetry, f.idp.requests(), hits)
	}

	// (c) A failed refresh keeps the old keys, and is not retried on every
	// request either.
	f.idp.setJWKS(nil, nil, true)
	backdate(mcpOAuthKeyTTL + time.Second)
	hits = f.idp.requests()
	if kept := f.mcp(valid, listTools); kept.Code != http.StatusOK {
		t.Errorf("old keys not kept across a failed refresh: %d %s", kept.Code, kept.Body.String())
	}
	if f.idp.requests() <= hits {
		t.Fatalf("an expired key set was not refreshed")
	}
	hits = f.idp.requests()
	if again := f.mcp(valid, listTools); again.Code != http.StatusOK || f.idp.requests() != hits {
		t.Errorf("a failed refresh was retried within %s: %d, %d requests, had %d", mcpOAuthKeyRetry, again.Code, f.idp.requests(), hits)
	}
	if !strings.Contains(f.logs.String(), "keeping the previous key set") {
		t.Errorf("a failed refresh was not logged:\n%s", f.logs.String())
	}

	// (d) With nothing cached at all, a failed fetch is 503 — and the failure
	// itself is cached for the retry window, so a stream of tokens against a
	// dead Keycloak does not become a stream of discovery requests.
	cache.mu.Lock()
	cache.issuer, cache.keys, cache.fetched, cache.attempted, cache.lastErr = "", nil, time.Time{}, time.Time{}, nil
	cache.mu.Unlock()
	hits = f.idp.requests()
	cold := f.mcp(valid, listTools)
	if cold.Code != http.StatusServiceUnavailable || !strings.Contains(f.refusalLog(t, cold), "keys: JWKS returned HTTP 500") {
		t.Errorf("cold cache with the issuer down: %d %s", cold.Code, cold.Body.String())
	}
	if f.idp.requests() <= hits {
		t.Fatalf("a cold cache did not ask the issuer")
	}
	hits = f.idp.requests()
	if repeat := f.mcp(valid, listTools); repeat.Code != http.StatusServiceUnavailable || f.idp.requests() != hits {
		t.Errorf("a failed cold fetch was retried within %s: %d, %d requests, had %d", mcpOAuthKeyRetry, repeat.Code, f.idp.requests(), hits)
	}
	// Once the issuer is back and the window has passed, so is SSO.
	f.idp.setJWKS(nil, nil, false)
	backdate(mcpOAuthKeyRetry + time.Second)
	if recovered := f.mcp(valid, listTools); recovered.Code != http.StatusOK {
		t.Errorf("no recovery after the issuer came back: %d %s", recovered.Code, recovered.Body.String())
	}
}

// adminSession signs the administrator in the way the browser does, so the
// settings round trip below runs through withAuth, CSRF and the admin gate.
func (f *mcpOAuthFixture) adminRequest(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	session, _ := secure.RandomToken(32)
	csrf, _ := secure.RandomToken(24)
	if _, err := f.store.Pool.Exec(t.Context(), `INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at,ip,user_agent) VALUES($1,$2,$3,now()+interval '1 hour','127.0.0.1','test')`, secure.TokenHash(session), f.adminID, secure.TokenHash(csrf)); err != nil {
		t.Fatal(err)
	}
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, "http://127.0.0.1"+path, reader)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	request.AddCookie(&http.Cookie{Name: "releasedock_session", Value: session})
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

// guards: putOIDCSettings, validateMCPOAuthSettings, getOIDCSettings
func TestMCPOAuthSettingsAreCheckedBeforeAnythingIsSaved(t *testing.T) {
	f := newMCPOAuthFixture(t)
	base := map[string]any{"enabled": false, "issuerUrl": f.idp.server.URL, "clientId": "releasedock-web", "allowInsecureEndpoints": true, "scopes": "openid profile email", "autoProvision": false, "defaultRole": "viewer"}
	put := func(overrides map[string]any) *httptest.ResponseRecorder {
		values := map[string]any{}
		for key, value := range base {
			values[key] = value
		}
		for key, value := range overrides {
			values[key] = value
		}
		return f.adminRequest(t, http.MethodPut, "/api/v1/admin/settings/oidc", values)
	}
	stored := func() oidcSettings {
		cfg, err := f.server.loadOIDC(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	refused := []struct {
		name      string
		overrides map[string]any
		want      string
	}{
		{"unknown permission", map[string]any{"mcp.oauth.enabled": true, "mcp.oauth.scopes": "mcp.use releases.raed"}, "unknown permission: releases.raed"},
		{"ceiling without mcp.use", map[string]any{"mcp.oauth.enabled": true, "mcp.oauth.scopes": "releases.read"}, "must include mcp.use"},
		{"resource with a query", map[string]any{"mcp.oauth.enabled": true, "mcp.oauth.resource": "https://releasedock.example.test/mcp?x=1"}, "mcp.oauth.resource must be"},
		{"resource off the MCP path", map[string]any{"mcp.oauth.enabled": true, "mcp.oauth.resource": "https://releasedock.example.test/api"}, "must end with /mcp"},
		{"plaintext public resource", map[string]any{"mcp.oauth.enabled": true, "mcp.oauth.resource": "http://releasedock.example.com/mcp"}, "must use HTTPS"},
		{"no issuer", map[string]any{"mcp.oauth.enabled": true, "issuerUrl": ""}, "requires the Keycloak issuer"},
	}
	for _, tc := range refused {
		response := put(tc.overrides)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), tc.want) {
			t.Errorf("%s: %d %s", tc.name, response.Code, response.Body.String())
		}
		if after := stored(); after.MCPOAuthEnabled || strings.Join(after.MCPOAuthScopes, " ") != "mcp.use applications.read profiles.read releases.read" || after.MCPOAuthResource != "" {
			t.Errorf("%s: a refused save changed stored values: %+v", tc.name, after)
		}
	}
	// Nothing about the MCP switch in the request keeps what is stored.
	if response := put(nil); response.Code != http.StatusOK {
		t.Fatalf("plain save: %d %s", response.Code, response.Body.String())
	}
	if after := stored(); strings.Join(after.MCPOAuthAudience, " ") != "releasedock-mcp" {
		t.Fatalf("a save that did not mention the audience list dropped it: %+v", after)
	}
	// A good configuration lands whole, and the read-back shows what the
	// server will advertise.
	accepted := put(map[string]any{"mcp.oauth.enabled": true, "mcp.oauth.audience": "releasedock-mcp, claude-desktop", "mcp.oauth.scopes": "mcp.use releases.read"})
	if accepted.Code != http.StatusOK {
		t.Fatalf("valid save: %d %s", accepted.Code, accepted.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(accepted.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["mcp.oauth.enabled"] != true || view["mcp.oauth.audience"] != "releasedock-mcp claude-desktop" || view["mcp.oauth.scopes"] != "mcp.use releases.read" {
		t.Errorf("read-back: %v", view)
	}
	if view["mcpOauthActive"] != true || view["mcpOauthEffectiveResource"] != f.resource || view["mcpOauthMetadataUrl"] != "https://releasedock.example.test/.well-known/oauth-protected-resource/mcp" {
		t.Errorf("read-back derived values: %v", view)
	}
	if response := f.get(mcpProtectedResourcePath+mcpPath, ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"scopes_supported":["mcp.use","releases.read"]`) {
		t.Errorf("metadata after save: %d %s", response.Code, response.Body.String())
	}
	if config := f.get("/api/v1/auth/config", ""); !strings.Contains(config.Body.String(), `"mcpOAuth":true`) {
		t.Errorf("auth config does not announce MCP SSO: %s", config.Body.String())
	}
}
