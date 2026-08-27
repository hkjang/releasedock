package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/hkjang/releasedock/backend/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPublicVersionEndpoint(t *testing.T) {
	vault, _ := secure.NewVault([]byte("0123456789abcdef0123456789abcdef"))
	s := New(nil, vault, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{Version: "1.2.3", Commit: "abc", BuildTime: "now"}, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"version":"1.2.3"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers were not applied")
	}
}

func TestSPAFallbackDoesNotRedirect(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<!doctype html><title>ReleaseDock</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	vault, _ := secure.NewVault([]byte("0123456789abcdef0123456789abcdef"))
	s := New(nil, vault, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{}, root)
	request := httptest.NewRequest(http.MethodGet, "/releases/deep-link", nil)
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Location") != "" || !strings.Contains(recorder.Body.String(), "ReleaseDock") {
		t.Fatalf("SPA fallback status=%d location=%q body=%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
}

func TestWebRootPrefersBuiltDistribution(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	development := filepath.Join(root, "web")
	distribution := filepath.Join(development, "dist")
	if err := os.MkdirAll(filepath.Join(distribution, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(development, "index.html"), []byte(`<script type="module" src="/src/main.tsx"></script>`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(distribution, "index.html"), []byte(`<script type="module" src="/assets/index.js"></script>`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := findWebRoot([]string{distribution, development}); got != distribution {
		t.Fatalf("web root=%q, want built distribution %q", got, distribution)
	}
	if got := findWebRoot([]string{development}); got != "" {
		t.Fatalf("development source index was accepted as deployable web root: %q", got)
	}
}

func TestSafetyValidators(t *testing.T) {
	t.Parallel()
	if safeReturnTo("https://evil.example/") || safeReturnTo("//evil.example/") || !safeReturnTo("/releases/123?tab=log") {
		t.Fatal("return_to validation is incorrect")
	}
	for _, bad := range []string{"../release.tar", "dir/release.tar.gz", "release.zip", "release.tar\n"} {
		if safeArtifactName(bad) {
			t.Fatalf("unsafe artifact accepted: %q", bad)
		}
	}
	if !safeArtifactName("release-1.2.3.tar.gz") || !validSHA256(strings.Repeat("a", 64)) {
		t.Fatal("valid artifact metadata was rejected")
	}
	relative := "data/artifacts"
	if validateAppSettings(appSettingsInput{ArtifactStoragePath: &relative}) == nil {
		t.Fatal("relative artifact path was accepted")
	}
	absolute := filepath.Join(string(filepath.Separator), "var", "lib", "releasedock", "artifacts")
	if err := validateAppSettings(appSettingsInput{ArtifactStoragePath: &absolute}); err != nil {
		t.Fatalf("absolute artifact path rejected: %v", err)
	}
	outsideManagedRoot := "/srv/releasedock/artifacts"
	if err := validateAppSettings(appSettingsInput{ArtifactStoragePath: &outsideManagedRoot}); err == nil {
		t.Fatal("artifact path outside the systemd writable root was accepted")
	}
	managedRoot := managedDataRoot
	if err := validateAppSettings(appSettingsInput{ArtifactStoragePath: &managedRoot}); err == nil {
		t.Fatal("the broad managed data root was accepted as artifact storage")
	}
	principal := store.Principal{Permissions: []string{"releases.read", "mcp.use"}}
	if err := validateKeyInput(principal, apiKeyInput{Name: "read key", Scopes: []string{"releases.read"}}); err != nil {
		t.Fatal(err)
	}
	if err := validateKeyInput(principal, apiKeyInput{Name: "escalated", Scopes: []string{"admin.settings.write"}}); err == nil {
		t.Fatal("API key scope escalation accepted")
	}
}

func TestAIAndJSONValidation(t *testing.T) {
	t.Parallel()
	if got, ok := integerValue(float64(262144)); !ok || got != 262144 {
		t.Fatal("valid max token value rejected")
	}
	if _, ok := integerValue(float64(1.5)); ok {
		t.Fatal("fractional token count accepted")
	}
	var value struct {
		Name string `json:"name"`
	}
	if strictUnmarshal([]byte(`{"name":"ok","unknown":true}`), &value) == nil {
		t.Fatal("unknown JSON field accepted")
	}
	if strictUnmarshal([]byte(`{"name":"ok"} {}`), &value) == nil {
		t.Fatal("trailing JSON accepted")
	}
	if mcpProtocolVersion != "2026-07-28" || mcpLegacyProtocolVersion != "2025-11-25" || !supportedMCPProtocolVersion("2025-11-25") || len(mcpTools()) != 15 {
		t.Fatal("MCP contract is incomplete")
	}
	seenTools := make(map[string]bool, len(mcpTools()))
	for _, tool := range mcpTools() {
		name, _ := tool["name"].(string)
		if name == "" || seenTools[name] {
			t.Fatalf("MCP tool name is empty or duplicated: %q", name)
		}
		seenTools[name] = true
	}
	if !validMCPPostAccept("application/json, text/event-stream") || validMCPPostAccept("application/json") {
		t.Fatal("MCP POST Accept negotiation is incorrect")
	}
	mcpRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	mcpRequest.Header.Set("Mcp-Method", "tools/list")
	if !validModernMCPRequestHeaders(mcpRequest, "tools/list", "") || validModernMCPRequestHeaders(mcpRequest, "tools/call", "") {
		t.Fatal("modern MCP routing header validation is incorrect")
	}
	mcpRequest.Header.Set("Mcp-Method", "tools/call")
	mcpRequest.Header.Set("Mcp-Name", "releasedock_dashboard")
	if !validModernMCPRequestHeaders(mcpRequest, "tools/call", "releasedock_dashboard") || validModernMCPRequestHeaders(mcpRequest, "tools/call", "releasedock_list_jobs") {
		t.Fatal("modern MCP named source routing is incorrect")
	}
	mcpRequest.Header.Set("Mcp-Name", "=?base64?"+base64.StdEncoding.EncodeToString([]byte("releasedock_dashboard"))+"?=")
	if !validModernMCPRequestHeaders(mcpRequest, "tools/call", "releasedock_dashboard") {
		t.Fatal("modern MCP base64 name sentinel was not decoded")
	}
	modernParams := json.RawMessage(`{"name":"releasedock_dashboard","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`)
	if name, err := modernMCPRequestMetadata(modernParams, "tools/call", mcpProtocolVersion); err != nil || name != "releasedock_dashboard" {
		t.Fatalf("valid modern MCP metadata rejected: name=%q err=%v", name, err)
	}
	if _, err := modernMCPRequestMetadata(modernParams, "tools/call", mcpLegacyProtocolVersion); err == nil {
		t.Fatal("modern MCP protocol metadata/header mismatch was accepted")
	}
	payload := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	metadata, err := applyAIRequestPolicy(payload, aiSettings{Model: "approved-model", MaxTokens: 262144})
	if err != nil || payload["model"] != "approved-model" || payload["stream"] != true || metadata.MaxTokens == nil || *metadata.MaxTokens != 262144 {
		t.Fatalf("configured AI defaults were not enforced: payload=%v metadata=%+v err=%v", payload, metadata, err)
	}
	if _, err := applyAIRequestPolicy(map[string]any{"model": "attacker-model"}, aiSettings{Model: "approved-model", MaxTokens: 4096}); err == nil {
		t.Fatal("AI model override was accepted")
	}
}

func TestEveryAdvertisedMCPToolHasDispatch(t *testing.T) {
	t.Parallel()
	vault, _ := secure.NewVault([]byte("0123456789abcdef0123456789abcdef"))
	s := New(nil, vault, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{}, "")
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(context.WithValue(request.Context(), principalKey, store.Principal{}))
	for _, advertised := range mcpTools() {
		name, _ := advertised["name"].(string)
		params, _ := json.Marshal(map[string]any{"name": name, "arguments": map[string]any{}})
		result, rpcErr := s.callMCPTool(request, params)
		if rpcErr != nil {
			t.Fatalf("advertised tool %q has no dispatch: %+v", name, rpcErr)
		}
		object, ok := result.(map[string]any)
		if !ok || object["isError"] != true || object["resultType"] != "complete" {
			t.Fatalf("permission-denied dispatch for %q returned %#v", name, result)
		}
	}
}

func TestOutboundRedirectsAreRejected(t *testing.T) {
	t.Parallel()
	destinationCalled := false
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationCalled = true
	}))
	defer destination.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, httptest.NewRequest(http.MethodGet, destination.URL, nil), destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	vault, _ := secure.NewVault([]byte("0123456789abcdef0123456789abcdef"))
	s := New(nil, vault, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{}, "")
	req, _ := http.NewRequest(http.MethodPost, redirector.URL, strings.NewReader(`{"secret":true}`))
	req.Header.Set("Authorization", "Bearer must-not-leak")
	_, err := s.httpClient.Do(req)
	if !errors.Is(err, errOutboundRedirect) {
		t.Fatalf("redirect was not rejected: %v", err)
	}
	if destinationCalled {
		t.Fatal("redirect destination received the credential-bearing request")
	}
}

func TestAIAuditSnapshotSurvivesClientCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/ai/chat/completions", nil).WithContext(ctx)
	snapshot := snapshotAIRequest(request)
	cancel()
	if snapshot.Context.Err() != nil {
		t.Fatal("AI audit context was canceled with the client request")
	}
}

func TestAIConcurrencyLimit(t *testing.T) {
	t.Parallel()
	vault, _ := secure.NewVault([]byte("0123456789abcdef0123456789abcdef"))
	s := New(nil, vault, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{}, "")
	if !s.acquireAIRequest("user-1") || !s.acquireAIRequest("user-1") {
		t.Fatal("requests below the per-user limit were rejected")
	}
	if s.acquireAIRequest("user-1") {
		t.Fatal("third concurrent request for one user was accepted")
	}
	s.releaseAIRequest("user-1")
	if !s.acquireAIRequest("user-1") {
		t.Fatal("released capacity was not reusable")
	}
	s.releaseAIRequest("user-1")
	s.releaseAIRequest("user-1")
	if s.aiTotal != 0 || len(s.aiActive) != 0 {
		t.Fatalf("AI concurrency accounting leaked: total=%d active=%v", s.aiTotal, s.aiActive)
	}
}

func TestAPIKeyDelegationAuthorityIsScopeBound(t *testing.T) {
	t.Parallel()
	databasePermissions := []string{"admin.users.write", "admin.rbac.write", "admin.settings.write", "releases.read"}
	apiKey := store.Principal{
		UserID:      "administrator",
		Permissions: databasePermissions,
		Scopes:      []string{"admin.users.write"},
		ViaAPIKey:   true,
	}
	effective, protectedAdmin := attenuateDelegatedAuthority(databasePermissions, true, apiKey, true)
	if protectedAdmin {
		t.Fatal("API key retained protected Administrator delegation authority")
	}
	if len(effective) != 1 || effective[0] != "admin.users.write" {
		t.Fatalf("API key authority was not intersected with scopes: %v", effective)
	}
	if permissionSubset(effective, []string{"admin.rbac.write"}) {
		t.Fatal("API key delegated a permission outside its scope")
	}
	sessionEffective, sessionAdmin := attenuateDelegatedAuthority(databasePermissions, true, store.Principal{UserID: "administrator"}, true)
	if !sessionAdmin || len(sessionEffective) != len(databasePermissions) {
		t.Fatal("browser session authority was unexpectedly attenuated")
	}
}

func TestArtifactStorageContractAndExpiryDate(t *testing.T) {
	t.Parallel()
	relative, err := artifactRelativePath("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", ".tar.gz")
	if err != nil || filepath.IsAbs(relative) || relative != "11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222.tar.gz" {
		t.Fatalf("invalid relative artifact contract: %q %v", relative, err)
	}
	if _, err := artifactRelativePath("..", "22222222-2222-4222-8222-222222222222", ".tar"); err == nil {
		t.Fatal("traversal release ID accepted")
	}
	expires, err := parseAPIKeyExpiry(stringPointer("2026-08-28"))
	if err != nil || expires == nil || expires.Hour() != 23 || expires.Minute() != 59 {
		t.Fatalf("date expiry not normalized to inclusive day: %v %v", expires, err)
	}
	if _, err := parseAPIKeyExpiry(stringPointer("28/08/2026")); err == nil {
		t.Fatal("invalid expiry date accepted")
	}
}
func stringPointer(value string) *string { return &value }

func TestApprovalQueueAndOperationContracts(t *testing.T) {
	t.Parallel()
	autoRollback := true
	if err := validateProfile(profileInput{Name: "prod", ApplicationID: "app", EnvironmentID: "env", AutoRollback: &autoRollback}); err == nil {
		t.Fatal("unsafe automatic rollback was accepted")
	}
	for _, status := range []string{"PENDING_REVIEW", "UNDER_REVIEW"} {
		if releaseCanQueue(status, true) {
			t.Fatalf("approval queue status %s bypassed an enabled workflow", status)
		}
		if !releaseCanQueue(status, false) {
			t.Fatalf("approval queue status %s was stranded after workflow disable", status)
		}
	}
	if !releaseCanQueue("APPROVED", true) || !releaseCanQueue("REJECTED", false) || releaseCanQueue("REJECTED", true) {
		t.Fatal("release queue state contract is incorrect")
	}
	phase, scriptType, ok := requiredScriptForOperation("ROLLBACK")
	if !ok || phase != "ROLLBACK" || scriptType != "ROLLBACK" {
		t.Fatalf("rollback dependency mapping is incorrect: %q %q %v", phase, scriptType, ok)
	}
	if _, _, ok := requiredScriptForOperation("UNKNOWN"); ok {
		t.Fatal("unknown operation received a dependency mapping")
	}
	if !artifactStoragePathChanged("/var/lib/releasedock/artifacts", "/srv/releasedock/artifacts") {
		t.Fatal("storage root change was not detected")
	}
	if artifactStoragePathChanged("/var/lib/releasedock/artifacts/", "/var/lib/releasedock/artifacts") {
		t.Fatal("equivalent storage roots were treated as different")
	}
	applicationID := "11111111-1111-4111-8111-111111111111"
	environmentID := "22222222-2222-4222-8222-222222222222"
	lockKey, err := releaseTargetLockKey(applicationID, environmentID)
	if err != nil || lockKey != applicationID+":"+environmentID {
		t.Fatalf("stable release target lock key is invalid: %q %v", lockKey, err)
	}
	if _, err := releaseTargetLockKey("mutable-app-code", "prod"); err == nil {
		t.Fatal("mutable target codes were accepted as lock identifiers")
	}
	if sessionCanMutatePersonalKeys(store.Principal{ViaAPIKey: true}) {
		t.Fatal("an API key was allowed to mint or mutate another API key")
	}
	if !sessionCanMutatePersonalKeys(store.Principal{}) {
		t.Fatal("a browser session was denied personal key management")
	}
	if !permissionSubset([]string{"admin.users.write", "releases.read"}, []string{"releases.read"}) || permissionSubset([]string{"releases.read"}, []string{"admin.users.write"}) {
		t.Fatal("RBAC delegation ceiling is incorrect")
	}
	labels, err := profileRunnerLabels(json.RawMessage(`["zone=prod","runtime=docker"]`))
	if err != nil || len(labels) != 2 {
		t.Fatalf("valid runner labels were rejected: %v %v", labels, err)
	}
	if _, err := profileRunnerLabels(json.RawMessage(`["prod","prod"]`)); err == nil {
		t.Fatal("duplicate runner labels were accepted")
	}
	if _, provided, err := profileTargetCredential(nil); err != nil || provided {
		t.Fatal("omitted targetCredentialId did not preserve the existing binding")
	}
	if id, provided, err := profileTargetCredential(json.RawMessage(`null`)); err != nil || !provided || id != "" {
		t.Fatal("null targetCredentialId did not explicitly clear the binding")
	}
	credentialID := "33333333-3333-4333-8333-333333333333"
	if id, provided, err := profileTargetCredential(json.RawMessage(`"` + credentialID + `"`)); err != nil || !provided || id != credentialID {
		t.Fatal("valid targetCredentialId was rejected")
	}
	credentialAPIKey := store.Principal{Permissions: []string{"admin.credentials.write"}, Scopes: []string{"admin.credentials.write"}, ViaAPIKey: true}
	if canManageTargetCredential(credentialAPIKey) {
		t.Fatal("API key was allowed to bind a deployment target credential")
	}
	if !canManageTargetCredential(store.Principal{Permissions: []string{"admin.credentials.write"}}) {
		t.Fatal("authorized browser session could not bind a deployment target credential")
	}
	if targetCredentialAAD(credentialID, 2) != "target-credential:"+credentialID+":v2" {
		t.Fatal("target credential AAD is not version-bound")
	}
	if _, err := normalizeOrigin("https://portal.example.test/path", true); err == nil {
		t.Fatal("public URL path was accepted")
	}
	if origin, err := normalizeOrigin("https://PORTAL.example.test/", true); err != nil || origin != "https://portal.example.test" {
		t.Fatalf("valid public origin was not normalized: %q %v", origin, err)
	}
	for _, valid := range []struct{ kind, path string }{{"docker", "/usr/bin/docker"}, {"podman", "/usr/local/bin/podman"}, {"containerd", "/usr/sbin/ctr"}} {
		if !validRuntimeBinaryPath(valid.kind, valid.path) {
			t.Fatalf("approved runtime path rejected: %s %s", valid.kind, valid.path)
		}
	}
	for _, invalid := range []struct{ kind, path string }{{"docker", "/tmp/docker"}, {"docker", "/usr/bin/podman"}, {"docker", "/usr/bin/sh"}, {"containerd", "/usr/bin/containerd"}, {"docker", "/usr/bin/../bin/docker"}} {
		if validRuntimeBinaryPath(invalid.kind, invalid.path) {
			t.Fatalf("unsafe runtime path accepted: %s %s", invalid.kind, invalid.path)
		}
	}
}

func TestRunnerSettingsValidationAndPatch(t *testing.T) {
	t.Parallel()
	current := runnerSettingsResponse{
		PollIntervalMS:      2_000,
		LockRetryMS:         5_000,
		SettingsRefreshMS:   30_000,
		HeartbeatIntervalMS: 5_000,
		StaleJobAfterMS:     60_000,
		LogChunkBytes:       16_384,
	}
	if err := validateRunnerSettings(current); err != nil {
		t.Fatalf("default Runner settings were rejected: %v", err)
	}
	poll := 3_000
	updated := applyRunnerSettingsInput(current, runnerSettingsInput{PollIntervalMS: &poll})
	if updated.PollIntervalMS != poll || updated.HeartbeatIntervalMS != current.HeartbeatIntervalMS {
		t.Fatalf("partial Runner settings update did not preserve omitted values: %+v", updated)
	}
	invalidRelation := current
	invalidRelation.StaleJobAfterMS = 10_000
	if err := validateRunnerSettings(invalidRelation); err == nil {
		t.Fatal("stale timeout equal to twice the heartbeat interval was accepted")
	}
	invalidChunk := current
	invalidChunk.LogChunkBytes = 1_023
	if err := validateRunnerSettings(invalidChunk); err == nil {
		t.Fatal("undersized Runner log chunk was accepted")
	}
}

type rollbackRetryFixture struct {
	server       *Server
	store        *store.Store
	creatorID    string
	approverID   string
	application  string
	environment  string
	profile      string
	releaseA     string
	releaseB     string
	artifactA    string
	jobA         string
	jobB         string
	failedJob    string
	runner       string
	registry     string
	deployScript string
	rollback     string
}

func newRollbackRetryFixture(t *testing.T) rollbackRetryFixture {
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
		t.Fatalf("generate schema suffix: %v", err)
	}
	schema := "releasedock_server_it_" + hex.EncodeToString(suffix)
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
		t.Fatalf("parse integration DSN: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatalf("open schema-isolated pool: %v", err)
	}
	st := &store.Store{Pool: pool}
	t.Cleanup(st.Close)
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate rollback retry fixture: %v", err)
	}

	fixture := rollbackRetryFixture{
		store:        st,
		creatorID:    "rollback-retry-creator",
		approverID:   "rollback-retry-approver",
		application:  "11000000-0000-4000-8000-000000000001",
		environment:  "22000000-0000-4000-8000-000000000001",
		profile:      "33000000-0000-4000-8000-000000000001",
		releaseA:     "44000000-0000-4000-8000-000000000001",
		releaseB:     "44000000-0000-4000-8000-000000000002",
		artifactA:    "55000000-0000-4000-8000-000000000001",
		jobA:         "66000000-0000-4000-8000-000000000001",
		jobB:         "66000000-0000-4000-8000-000000000002",
		failedJob:    "66000000-0000-4000-8000-000000000003",
		runner:       "77000000-0000-4000-8000-000000000001",
		registry:     "88000000-0000-4000-8000-000000000001",
		deployScript: "99000000-0000-4000-8000-000000000001",
		rollback:     "99000000-0000-4000-8000-000000000002",
	}
	seed := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id,username,display_name) VALUES($1,'retry-creator','Retry creator'),($2,'retry-approver','Retry approver')`, []any{fixture.creatorID, fixture.approverID}},
		{`UPDATE app_settings SET approval_enabled=TRUE,approval_config='{"allowSelfApproval":false}'::jsonb`, nil},
		{`INSERT INTO applications(id,code,name,created_by) VALUES($1,'retry-app','Retry app',$2)`, []any{fixture.application, fixture.creatorID}},
		{`INSERT INTO environments(id,application_id,code,name,created_by) VALUES($1,$2,'prod','Production',$3)`, []any{fixture.environment, fixture.application, fixture.creatorID}},
		{`INSERT INTO runner_credentials(id,name,endpoint,project,username,ciphertext,approved_at,approved_by,created_by) VALUES($1,'retry-registry','https://registry.example.test','releasedock','robot','encrypted',now(),$2,$2)`, []any{fixture.registry, fixture.creatorID}},
		{`INSERT INTO deployment_profiles(id,application_id,environment_id,name,approval_required,registry_url,registry_host,registry_project,registry_credential_id,created_by) VALUES($1,$2,$3,'retry-profile',TRUE,'https://registry.example.test','registry.example.test','releasedock',$4,$5)`, []any{fixture.profile, fixture.application, fixture.environment, fixture.registry, fixture.creatorID}},
		{`INSERT INTO script_versions(id,name,script_type,version,interpreter_path,content,sha256,approved_at,approved_by,created_by) VALUES($1,'retry-deploy','DEPLOY',1,'/bin/sh','exit 0',repeat('a',64),now(),$3,$3),($2,'retry-rollback','ROLLBACK',1,'/bin/sh','exit 0',repeat('b',64),now(),$3,$3)`, []any{fixture.deployScript, fixture.rollback, fixture.creatorID}},
		{`INSERT INTO deployment_profile_scripts(profile_id,script_version_id,phase,execution_order) VALUES($1,$2,'DEPLOY',1),($1,$3,'ROLLBACK',2)`, []any{fixture.profile, fixture.deployScript, fixture.rollback}},
		{`INSERT INTO runner_instances(id,worker_id,name,address,managed_by_runner,last_heartbeat_at,created_by) VALUES($1,'retry-worker','retry-worker','direct-db',TRUE,clock_timestamp(),$2)`, []any{fixture.runner, fixture.creatorID}},
	}
	for _, statement := range seed {
		if _, err := st.Pool.Exec(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatalf("seed rollback retry fixture: %v", err)
		}
	}
	artifactB := "55000000-0000-4000-8000-000000000002"
	for _, release := range []struct{ id, version, artifact string }{{fixture.releaseA, "A", fixture.artifactA}, {fixture.releaseB, "B", artifactB}} {
		if _, err := st.Pool.Exec(t.Context(), `INSERT INTO releases(id,application_id,environment_id,profile_id,version,created_by) VALUES($1,$2,$3,$4,$5,$6)`, release.id, fixture.application, fixture.environment, fixture.profile, release.version, fixture.creatorID); err != nil {
			t.Fatalf("insert release %s: %v", release.version, err)
		}
		if _, err := st.Pool.Exec(t.Context(), `INSERT INTO release_artifacts(id,release_id,original_filename,storage_path,size_bytes,sha256,uploaded_by) VALUES($1,$2,$3,$4,1,repeat('c',64),$5)`, release.artifact, release.id, release.version+".tar", release.id+"/"+release.artifact+".tar", fixture.creatorID); err != nil {
			t.Fatalf("insert artifact %s: %v", release.version, err)
		}
	}
	lockKey := fixture.application + ":" + fixture.environment
	insertDeploy := func(jobID, releaseID, artifactID string, sourceRelease, sourceJob any) {
		t.Helper()
		if _, err := st.Pool.Exec(t.Context(), `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,rollback_source_release_id,rollback_source_job_id,created_by)
			SELECT $1,$2,$3,'retry-app',r.version,'prod',$4,$5,a.storage_path,a.sha256,$6,$7,$8 FROM releases r JOIN release_artifacts a ON a.id=$5 WHERE r.id=$2`, jobID, releaseID, fixture.profile, lockKey, artifactID, sourceRelease, sourceJob, fixture.creatorID); err != nil {
			t.Fatalf("insert deploy job: %v", err)
		}
		if _, err := st.Pool.Exec(t.Context(), `UPDATE release_jobs SET status='SUCCESS',finished_at=clock_timestamp() WHERE id=$1`, jobID); err != nil {
			t.Fatalf("finish deploy job: %v", err)
		}
	}
	insertDeploy(fixture.jobA, fixture.releaseA, fixture.artifactA, nil, nil)
	insertDeploy(fixture.jobB, fixture.releaseB, artifactB, fixture.releaseA, fixture.jobA)
	if _, err := st.Pool.Exec(t.Context(), `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,operation,rollback_source_release_id,rollback_source_job_id,created_by)
		SELECT $1,$2,$3,'retry-app','A','prod',$4,$5,a.storage_path,a.sha256,'ROLLBACK',$6,$7,$8 FROM release_artifacts a WHERE a.id=$5`, fixture.failedJob, fixture.releaseB, fixture.profile, lockKey, fixture.artifactA, fixture.releaseA, fixture.jobA, fixture.creatorID); err != nil {
		t.Fatalf("insert failed rollback: %v", err)
	}
	if _, err := st.Pool.Exec(t.Context(), `UPDATE release_jobs SET status='FAILED',finished_at=clock_timestamp() WHERE id=$1`, fixture.failedJob); err != nil {
		t.Fatalf("finish failed rollback: %v", err)
	}
	if _, err := st.Pool.Exec(t.Context(), `UPDATE releases SET status='FAILED',requested_operation='ROLLBACK',rollback_source_release_id=$2,rollback_source_job_id=$3,operation_base_status='SUCCESS' WHERE id=$1`, fixture.releaseB, fixture.releaseA, fixture.jobA); err != nil {
		t.Fatalf("mark rollback failure: %v", err)
	}
	vault, err := secure.NewVault([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.server = New(st, vault, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{}, "")
	return fixture
}

func releaseIntegrationRequest(t *testing.T, fixture rollbackRetryFixture, releaseID, actorID, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, body)
	request.SetPathValue("id", releaseID)
	request = request.WithContext(context.WithValue(request.Context(), principalKey, store.Principal{UserID: actorID}))
	recorder := httptest.NewRecorder()
	returnRecorder := recorder
	switch {
	case strings.HasSuffix(path, "/retry"):
		fixture.server.retryRelease(recorder, request)
	case strings.HasSuffix(path, "/approve"):
		fixture.server.approveRelease(recorder, request)
	case strings.HasSuffix(path, "/reject"):
		fixture.server.rejectRelease(recorder, request)
	default:
		fixture.server.enqueueRelease(recorder, request)
	}
	return returnRecorder
}

func rollbackRetryRequest(t *testing.T, fixture rollbackRetryFixture, actorID, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	return releaseIntegrationRequest(t, fixture, fixture.releaseB, actorID, path, body)
}

func TestRollbackRetryApprovalIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	retry := rollbackRetryRequest(t, fixture, fixture.creatorID, "/api/v1/releases/"+fixture.releaseB+"/retry", nil)
	if retry.Code != http.StatusOK {
		t.Fatalf("submit rollback retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	approve := rollbackRetryRequest(t, fixture, fixture.approverID, "/api/v1/releases/"+fixture.releaseB+"/approve", strings.NewReader(`{"comment":"approved"}`))
	if approve.Code != http.StatusOK {
		t.Fatalf("approve rollback retry status=%d body=%s", approve.Code, approve.Body.String())
	}
	enqueue := rollbackRetryRequest(t, fixture, fixture.creatorID, "/api/v1/releases/"+fixture.releaseB+"/deploy", nil)
	if enqueue.Code != http.StatusOK {
		t.Fatalf("enqueue approved rollback retry status=%d body=%s", enqueue.Code, enqueue.Body.String())
	}
	var operation, status, sourceRelease, sourceJob string
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT operation,status,rollback_source_release_id::text,rollback_source_job_id::text FROM release_jobs WHERE release_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, fixture.releaseB).Scan(&operation, &status, &sourceRelease, &sourceJob); err != nil {
		t.Fatal(err)
	}
	if operation != "ROLLBACK" || status != "QUEUED" || sourceRelease != fixture.releaseA || sourceJob != fixture.jobA {
		t.Fatalf("queued rollback retry lost provenance: %s %s %s %s", operation, status, sourceRelease, sourceJob)
	}
}

func TestRollbackRetryRejectionRestoresBaseIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	retry := rollbackRetryRequest(t, fixture, fixture.creatorID, "/api/v1/releases/"+fixture.releaseB+"/retry", nil)
	if retry.Code != http.StatusOK {
		t.Fatalf("submit rollback retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	reject := rollbackRetryRequest(t, fixture, fixture.approverID, "/api/v1/releases/"+fixture.releaseB+"/reject", strings.NewReader(`{"comment":"not approved"}`))
	if reject.Code != http.StatusOK {
		t.Fatalf("reject rollback retry status=%d body=%s", reject.Code, reject.Body.String())
	}
	var status, operation string
	var sourceRelease, sourceJob *string
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT status,requested_operation,rollback_source_release_id::text,rollback_source_job_id::text FROM releases WHERE id=$1`, fixture.releaseB).Scan(&status, &operation, &sourceRelease, &sourceJob); err != nil {
		t.Fatal(err)
	}
	if status != "SUCCESS" || operation != "DEPLOY" || sourceRelease != nil || sourceJob != nil {
		t.Fatalf("rejected rollback retry was not restored: %s %s %v %v", status, operation, sourceRelease, sourceJob)
	}
}

func TestRollbackRetryApprovalRejectsNewerDeploymentIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	retry := rollbackRetryRequest(t, fixture, fixture.creatorID, "/api/v1/releases/"+fixture.releaseB+"/retry", nil)
	if retry.Code != http.StatusOK {
		t.Fatalf("submit rollback retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	const (
		releaseC  = "44000000-0000-4000-8000-000000000003"
		artifactC = "55000000-0000-4000-8000-000000000003"
		jobC      = "66000000-0000-4000-8000-000000000004"
	)
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO releases(id,application_id,environment_id,profile_id,version,created_by) VALUES($1,$2,$3,$4,'C',$5)`, releaseC, fixture.application, fixture.environment, fixture.profile, fixture.creatorID); err != nil {
		t.Fatalf("insert newer release: %v", err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO release_artifacts(id,release_id,original_filename,storage_path,size_bytes,sha256,uploaded_by) VALUES($1,$2,'C.tar',$3,1,repeat('c',64),$4)`, artifactC, releaseC, releaseC+"/"+artifactC+".tar", fixture.creatorID); err != nil {
		t.Fatalf("insert newer artifact: %v", err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,rollback_source_release_id,rollback_source_job_id,created_by)
		SELECT $1,$2,$3,'retry-app','C','prod',$4,$5,a.storage_path,a.sha256,$6,$7,$8 FROM release_artifacts a WHERE a.id=$5`, jobC, releaseC, fixture.profile, fixture.application+":"+fixture.environment, artifactC, fixture.releaseB, fixture.jobB, fixture.creatorID); err != nil {
		t.Fatalf("insert newer deployment: %v", err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `UPDATE release_jobs SET status='SUCCESS',finished_at=clock_timestamp() WHERE id=$1`, jobC); err != nil {
		t.Fatalf("finish newer deployment: %v", err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `UPDATE releases SET status='SUCCESS' WHERE id=$1`, releaseC); err != nil {
		t.Fatalf("mark newer release successful: %v", err)
	}
	approve := rollbackRetryRequest(t, fixture, fixture.approverID, "/api/v1/releases/"+fixture.releaseB+"/approve", strings.NewReader(`{"comment":"stale"}`))
	if approve.Code != http.StatusConflict || !strings.Contains(approve.Body.String(), "rollback_target_stale") {
		t.Fatalf("stale rollback retry approval status=%d body=%s", approve.Code, approve.Body.String())
	}
	var status string
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT status FROM releases WHERE id=$1`, fixture.releaseB).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "PENDING_REVIEW" {
		t.Fatalf("stale approval mutated the pending request to %s", status)
	}
}

func TestDeployRetryRejectionRestoresSelfApprovalIdentityIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	const (
		releaseC  = "44000000-0000-4000-8000-000000000004"
		artifactC = "55000000-0000-4000-8000-000000000004"
		jobC      = "66000000-0000-4000-8000-000000000005"
	)
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO releases(id,application_id,environment_id,profile_id,version,created_by) VALUES($1,$2,$3,$4,'retry-sod',$5)`, releaseC, fixture.application, fixture.environment, fixture.profile, fixture.creatorID); err != nil {
		t.Fatalf("insert failed deployment release: %v", err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO release_artifacts(id,release_id,original_filename,storage_path,size_bytes,sha256,uploaded_by) VALUES($1,$2,'retry-sod.tar',$3,1,repeat('d',64),$4)`, artifactC, releaseC, releaseC+"/"+artifactC+".tar", fixture.creatorID); err != nil {
		t.Fatalf("insert failed deployment artifact: %v", err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,rollback_source_release_id,rollback_source_job_id,created_by)
		SELECT $1,$2,$3,'retry-app','retry-sod','prod',$4,$5,a.storage_path,a.sha256,$6,$7,$8 FROM release_artifacts a WHERE a.id=$5`, jobC, releaseC, fixture.profile, fixture.application+":"+fixture.environment, artifactC, fixture.releaseB, fixture.jobB, fixture.creatorID); err != nil {
		t.Fatalf("insert failed deployment job: %v", err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `UPDATE release_jobs SET status='FAILED',finished_at=clock_timestamp() WHERE id=$1`, jobC); err != nil {
		t.Fatalf("finish failed deployment job: %v", err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `UPDATE releases SET status='FAILED' WHERE id=$1`, releaseC); err != nil {
		t.Fatalf("mark deployment failed: %v", err)
	}
	retry := releaseIntegrationRequest(t, fixture, releaseC, fixture.approverID, "/api/v1/releases/"+releaseC+"/retry", nil)
	if retry.Code != http.StatusOK {
		t.Fatalf("submit deploy retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	reject := releaseIntegrationRequest(t, fixture, releaseC, fixture.creatorID, "/api/v1/releases/"+releaseC+"/reject", strings.NewReader(`{"comment":"retry rejected"}`))
	if reject.Code != http.StatusOK {
		t.Fatalf("reject deploy retry status=%d body=%s", reject.Code, reject.Body.String())
	}
	var status string
	var operationRequester *string
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT status,operation_requested_by FROM releases WHERE id=$1`, releaseC).Scan(&status, &operationRequester); err != nil {
		t.Fatal(err)
	}
	if status != "REJECTED" || operationRequester != nil {
		t.Fatalf("retry rejection retained delegated requester: status=%s requester=%v", status, operationRequester)
	}
	resubmit := releaseIntegrationRequest(t, fixture, releaseC, fixture.creatorID, "/api/v1/releases/"+releaseC+"/deploy", nil)
	if resubmit.Code != http.StatusOK {
		t.Fatalf("resubmit rejected release status=%d body=%s", resubmit.Code, resubmit.Body.String())
	}
	selfApprove := releaseIntegrationRequest(t, fixture, releaseC, fixture.creatorID, "/api/v1/releases/"+releaseC+"/approve", strings.NewReader(`{"comment":"self"}`))
	if selfApprove.Code != http.StatusForbidden || !strings.Contains(selfApprove.Body.String(), "self_approval_forbidden") {
		t.Fatalf("creator self-approved after retry rejection: status=%d body=%s", selfApprove.Code, selfApprove.Body.String())
	}
}

func TestProfileTargetCredentialBindPreserveAndClearIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	const credentialID = "aa000000-0000-4000-8000-000000000001"
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO target_credentials(id,name,credential_type,ciphertext,approved_by,created_by) VALUES($1,'profile-target','TOKEN','encrypted',$2,$2)`, credentialID, fixture.creatorID); err != nil {
		t.Fatalf("create profile target credential: %v", err)
	}
	apply := func(raw json.RawMessage) *string {
		t.Helper()
		tx, err := fixture.store.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context()) //nolint:errcheck
		input := profileInput{
			RegistryID:         fixture.registry,
			TargetCredentialID: raw,
			DeployScriptID:     fixture.deployScript,
			RollbackScriptID:   fixture.rollback,
		}
		if err := fixture.server.updateProfileLinks(t.Context(), tx, fixture.profile, input); err != nil {
			t.Fatalf("update profile target credential: %v", err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		var got *string
		if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT target_credential_id::text FROM deployment_profiles WHERE id=$1`, fixture.profile).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := apply(json.RawMessage(`"` + credentialID + `"`)); got == nil || *got != credentialID {
		t.Fatalf("profile target credential was not bound: %v", got)
	}
	if got := apply(nil); got == nil || *got != credentialID {
		t.Fatalf("omitted targetCredentialId did not preserve binding: %v", got)
	}
	if got := apply(json.RawMessage(`null`)); got != nil {
		t.Fatalf("null targetCredentialId did not clear binding: %v", *got)
	}
	if got := apply(json.RawMessage(`"` + credentialID + `"`)); got == nil {
		t.Fatal("could not restore target credential before empty-string clear")
	}
	if got := apply(json.RawMessage(`""`)); got != nil {
		t.Fatalf("empty targetCredentialId did not clear binding: %v", *got)
	}
}

func TestProfileWriteWithoutCredentialPermissionIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	const credentialID = "aa000000-0000-4000-8000-000000000002"
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO target_credentials(id,name,credential_type,ciphertext,approved_by,created_by) VALUES($1,'least-privilege-target','TOKEN','encrypted',$2,$2)`, credentialID, fixture.creatorID); err != nil {
		t.Fatalf("create profile target credential: %v", err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `UPDATE deployment_profiles SET target_credential_id=$2 WHERE id=$1`, fixture.profile, credentialID); err != nil {
		t.Fatalf("bind profile target credential: %v", err)
	}

	const roleID = "role-profile-write-only"
	grants := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO roles(id,name,description) VALUES($1,'Profile writer','Profile writer without credential metadata access')`, []any{roleID}},
		{`INSERT INTO role_permissions(role_id,permission_code) VALUES($1,'profiles.write')`, []any{roleID}},
		{`INSERT INTO user_roles(user_id,role_id) VALUES($1,$2)`, []any{fixture.creatorID, roleID}},
	}
	for _, grant := range grants {
		if _, err := fixture.store.Pool.Exec(t.Context(), grant.query, grant.args...); err != nil {
			t.Fatalf("grant least-privilege profile role: %v", err)
		}
	}
	sessionToken, err := secure.RandomToken(32)
	if err != nil {
		t.Fatal(err)
	}
	csrfToken, err := secure.RandomToken(24)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, secure.TokenHash(sessionToken), fixture.creatorID, secure.TokenHash(csrfToken)); err != nil {
		t.Fatalf("create least-privilege session: %v", err)
	}

	request := func(method, path string, payload map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrfToken)
		req.AddCookie(&http.Cookie{Name: "releasedock_session", Value: sessionToken})
		recorder := httptest.NewRecorder()
		fixture.server.Handler().ServeHTTP(recorder, req)
		return recorder
	}

	create := request(http.MethodPost, "/api/v1/deployment-profiles", map[string]any{
		"applicationId": fixture.application,
		"environmentId": fixture.environment,
		"name":          "least-privilege-create",
		"active":        true,
	})
	if create.Code != http.StatusCreated {
		t.Fatalf("profiles.write-only create status=%d body=%s", create.Code, create.Body.String())
	}

	update := request(http.MethodPut, "/api/v1/deployment-profiles/"+fixture.profile, map[string]any{
		"applicationId":    fixture.application,
		"environmentId":    fixture.environment,
		"name":             "least-privilege-update",
		"registryId":       fixture.registry,
		"deployScriptId":   fixture.deployScript,
		"rollbackScriptId": fixture.rollback,
	})
	if update.Code != http.StatusOK {
		t.Fatalf("profiles.write-only update status=%d body=%s", update.Code, update.Body.String())
	}
	var boundCredentialID *string
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT target_credential_id::text FROM deployment_profiles WHERE id=$1`, fixture.profile).Scan(&boundCredentialID); err != nil {
		t.Fatal(err)
	}
	if boundCredentialID == nil || *boundCredentialID != credentialID {
		t.Fatalf("omitted targetCredentialId did not preserve the existing binding: %v", boundCredentialID)
	}
}

func mcpHTTPIntegrationRequest(t *testing.T, fixture rollbackRetryFixture, method, version, name string, params any, permissions ...string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if version != "" {
		request.Header.Set("MCP-Protocol-Version", version)
	}
	if method != "initialize" {
		request.Header.Set("Mcp-Method", method)
	}
	if name != "" {
		request.Header.Set("Mcp-Name", name)
	}
	request = request.WithContext(context.WithValue(request.Context(), principalKey, store.Principal{UserID: fixture.creatorID, Permissions: permissions}))
	recorder := httptest.NewRecorder()
	fixture.server.mcpPOST(recorder, request)
	return recorder
}

func modernMCPMeta(clientInfo bool) map[string]any {
	metadata := map[string]any{
		"io.modelcontextprotocol/protocolVersion":    mcpProtocolVersion,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	if clientInfo {
		metadata["io.modelcontextprotocol/clientInfo"] = map[string]any{"name": "releasedock-test", "version": "1.0.0"}
	}
	return metadata
}

func TestModernAndLegacyMCPHTTPIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)

	discover := mcpHTTPIntegrationRequest(t, fixture, "server/discover", mcpProtocolVersion, "", map[string]any{"_meta": modernMCPMeta(false)})
	if discover.Code != http.StatusOK || !strings.Contains(discover.Body.String(), `"io.modelcontextprotocol/serverInfo"`) || !strings.Contains(discover.Body.String(), `"supportedVersions":["2026-07-28"]`) || strings.Contains(discover.Body.String(), `"protocolVersions"`) || !strings.Contains(discover.Body.String(), `"resultType":"complete"`) || !strings.Contains(discover.Body.String(), `"ttlMs":300000`) || !strings.Contains(discover.Body.String(), `"cacheScope":"public"`) {
		t.Fatalf("modern server/discover status=%d body=%s", discover.Code, discover.Body.String())
	}
	ping := mcpHTTPIntegrationRequest(t, fixture, "ping", mcpProtocolVersion, "", map[string]any{"_meta": modernMCPMeta(false)})
	if ping.Code != http.StatusOK || !strings.Contains(ping.Body.String(), `"resultType":"complete"`) || !strings.Contains(ping.Body.String(), `"io.modelcontextprotocol/serverInfo"`) {
		t.Fatalf("modern ping status=%d body=%s", ping.Code, ping.Body.String())
	}
	list := mcpHTTPIntegrationRequest(t, fixture, "tools/list", mcpProtocolVersion, "", map[string]any{"_meta": modernMCPMeta(false)})
	if list.Code != http.StatusOK || list.Header().Get("MCP-Protocol-Version") != mcpProtocolVersion || !strings.Contains(list.Body.String(), `"ttlMs":300000`) || !strings.Contains(list.Body.String(), `"cacheScope":"public"`) || !strings.Contains(list.Body.String(), `"io.modelcontextprotocol/serverInfo"`) {
		t.Fatalf("modern tools/list status=%d headers=%v body=%s", list.Code, list.Header(), list.Body.String())
	}
	callParams := map[string]any{"name": "releasedock_dashboard", "arguments": map[string]any{}, "_meta": modernMCPMeta(false)}
	encodedToolName := "=?base64?" + base64.StdEncoding.EncodeToString([]byte("releasedock_dashboard")) + "?="
	call := mcpHTTPIntegrationRequest(t, fixture, "tools/call", mcpProtocolVersion, encodedToolName, callParams, "releases.read")
	if call.Code != http.StatusOK || strings.Contains(call.Body.String(), "Invalid params") || !strings.Contains(call.Body.String(), `"resultType":"complete"`) {
		t.Fatalf("modern tools/call status=%d body=%s", call.Code, call.Body.String())
	}
	mismatch := mcpHTTPIntegrationRequest(t, fixture, "tools/call", mcpProtocolVersion, "releasedock_list_jobs", callParams, "releases.read")
	if mismatch.Code != http.StatusBadRequest || !strings.Contains(mismatch.Body.String(), `"code":-32020`) {
		t.Fatalf("modern tool name mismatch status=%d body=%s", mismatch.Code, mismatch.Body.String())
	}
	modernInitialize := mcpHTTPIntegrationRequest(t, fixture, "initialize", "", "", map[string]any{"protocolVersion": mcpProtocolVersion})
	if modernInitialize.Code != http.StatusOK || !strings.Contains(modernInitialize.Body.String(), `"code":-32601`) {
		t.Fatalf("modern initialize was not rejected: status=%d body=%s", modernInitialize.Code, modernInitialize.Body.String())
	}
	legacyInitialize := mcpHTTPIntegrationRequest(t, fixture, "initialize", "", "", map[string]any{"protocolVersion": mcpLegacyProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "legacy-test", "version": "1"}})
	if legacyInitialize.Code != http.StatusOK || !strings.Contains(legacyInitialize.Body.String(), `"protocolVersion":"2025-11-25"`) || strings.Contains(legacyInitialize.Body.String(), `"error"`) {
		t.Fatalf("legacy initialize failed: status=%d body=%s", legacyInitialize.Code, legacyInitialize.Body.String())
	}
}

func TestVerifyOIDCIDToken(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kid := "test-key"
	var jwksURL string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jwks" {
			http.NotFound(w, r)
			return
		}
		n := base64.RawURLEncoding.EncodeToString(privateKey.N.Bytes())
		e := big.NewInt(int64(privateKey.E)).Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256", "n": n, "e": base64.RawURLEncoding.EncodeToString(e)}}})
	}))
	defer provider.Close()
	jwksURL = provider.URL + "/jwks"
	issuer := "https://keycloak.internal/realms/releasedock"
	nonce := "nonce-value"
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{"iss": issuer, "sub": "subject", "aud": "releasedock", "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(), "nonce": nonce, "preferred_username": "alice"})
	encodedHeader := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(encodedHeader + "." + encodedPayload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	token := encodedHeader + "." + encodedPayload + "." + base64.RawURLEncoding.EncodeToString(signature)
	vault, _ := secure.NewVault([]byte("0123456789abcdef0123456789abcdef"))
	s := New(nil, vault, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{}, "")
	claims, err := s.verifyIDToken(t.Context(), token, oidcDiscovery{Issuer: issuer, JWKSURI: jwksURL}, "releasedock", nonce)
	if err != nil || claims.Subject != "subject" {
		t.Fatalf("valid token rejected: claims=%+v err=%v", claims, err)
	}
	if _, err := s.verifyIDToken(t.Context(), token, oidcDiscovery{Issuer: issuer, JWKSURI: jwksURL}, "releasedock", "wrong"); err == nil {
		t.Fatal("wrong nonce accepted")
	}
}
