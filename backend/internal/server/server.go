package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/hkjang/releasedock/backend/internal/store"
)

const maxJSONBody = 2 << 20

var errOutboundRedirect = errors.New("outbound redirects are disabled")

type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"builtAt"`
}

type Server struct {
	store       *store.Store
	vault       *secure.Vault
	log         *slog.Logger
	build       BuildInfo
	httpClient  *http.Client
	webRoot     string
	limiter     *loginLimiter
	streamMu    sync.Mutex
	streams     map[string]int
	streamTotal int
	aiMu        sync.Mutex
	aiActive    map[string]int
	aiTotal     int
}

func New(st *store.Store, vault *secure.Vault, logger *slog.Logger, build BuildInfo, webRoot string) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		store: st,
		vault: vault,
		log:   logger,
		build: build,
		httpClient: &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errOutboundRedirect
		}},
		webRoot:  webRoot,
		limiter:  newLoginLimiter(),
		streams:  make(map[string]int),
		aiActive: make(map[string]int),
	}
}

func (s *Server) acquireAIRequest(actorID string) bool {
	s.aiMu.Lock()
	defer s.aiMu.Unlock()
	if actorID == "" || s.aiActive[actorID] >= 2 || s.aiTotal >= 32 {
		return false
	}
	s.aiActive[actorID]++
	s.aiTotal++
	return true
}

func (s *Server) releaseAIRequest(actorID string) {
	s.aiMu.Lock()
	defer s.aiMu.Unlock()
	if s.aiActive[actorID] <= 0 {
		return
	}
	s.aiActive[actorID]--
	s.aiTotal--
	if s.aiActive[actorID] == 0 {
		delete(s.aiActive, actorID)
	}
}

func (s *Server) acquireLogStream(actorID string) bool {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if actorID == "" || s.streams[actorID] >= 3 || s.streamTotal >= 64 {
		return false
	}
	s.streams[actorID]++
	s.streamTotal++
	return true
}

func (s *Server) releaseLogStream(actorID string) {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if s.streams[actorID] <= 0 {
		return
	}
	s.streams[actorID]--
	s.streamTotal--
	if s.streams[actorID] == 0 {
		delete(s.streams, actorID)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/v1/version", s.version)
	mux.HandleFunc("POST /api/v1/auth/login", s.localLogin)
	mux.HandleFunc("GET /api/v1/auth/config", s.authConfig)
	mux.HandleFunc("POST /api/v1/auth/logout", s.withAuth(s.logout))
	mux.HandleFunc("GET /api/v1/auth/oidc/login", s.oidcLogin)
	mux.HandleFunc("GET /api/v1/auth/oidc/callback", s.oidcCallback)
	mux.HandleFunc("GET /api/v1/me", s.withAuth(s.me))
	mux.HandleFunc("PUT /api/v1/me/profile", s.withAuth(s.updateMyProfile))
	mux.HandleFunc("PUT /api/v1/me/password", s.withAuth(s.changeMyPassword))
	mux.HandleFunc("PATCH /api/v1/me/preferences", s.withAuth(s.updatePreferences))
	mux.HandleFunc("GET /api/v1/me/api-keys", s.withPermission("keys.manage", s.listAPIKeys))
	mux.HandleFunc("POST /api/v1/me/api-keys", s.withSessionPermission("keys.manage", s.createAPIKey))
	mux.HandleFunc("POST /api/v1/me/api-keys/{id}/rotate", s.withSessionPermission("keys.manage", s.rotateAPIKey))
	mux.HandleFunc("PATCH /api/v1/me/api-keys/{id}", s.withSessionPermission("keys.manage", s.updateAPIKey))
	mux.HandleFunc("PUT /api/v1/me/api-keys/{id}", s.withSessionPermission("keys.manage", s.updateAPIKey))
	mux.HandleFunc("DELETE /api/v1/me/api-keys/{id}", s.withSessionPermission("keys.manage", s.revokeAPIKey))

	mux.HandleFunc("GET /api/v1/admin/settings", s.withPermission("admin.settings.read", s.getSettings))
	mux.HandleFunc("PATCH /api/v1/admin/settings", s.withPermission("admin.settings.write", s.updateSettings))
	mux.HandleFunc("GET /api/v1/admin/settings/general", s.withPermission("admin.settings.read", s.getGeneralSettings))
	mux.HandleFunc("PUT /api/v1/admin/settings/general", s.withPermission("admin.settings.write", s.putGeneralSettings))
	mux.HandleFunc("GET /api/v1/admin/settings/approval", s.withPermission("admin.settings.read", s.getApprovalSettings))
	mux.HandleFunc("PUT /api/v1/admin/settings/approval", s.withPermission("admin.settings.write", s.putApprovalSettings))
	mux.HandleFunc("GET /api/v1/admin/settings/storage", s.withPermission("admin.settings.read", s.getStorageSettings))
	mux.HandleFunc("PUT /api/v1/admin/settings/storage", s.withPermission("admin.settings.write", s.putStorageSettings))
	mux.HandleFunc("GET /api/v1/admin/settings/runner", s.withPermission("admin.settings.read", s.getRunnerSettings))
	mux.HandleFunc("PUT /api/v1/admin/settings/runner", s.withPermission("admin.settings.write", s.putRunnerSettings))
	mux.HandleFunc("GET /api/v1/admin/oidc", s.withPermission("admin.settings.read", s.getOIDCSettings))
	mux.HandleFunc("PATCH /api/v1/admin/oidc", s.withPermission("admin.settings.write", s.updateOIDCSettings))
	mux.HandleFunc("GET /api/v1/admin/settings/oidc", s.withPermission("admin.settings.read", s.getOIDCSettings))
	mux.HandleFunc("PUT /api/v1/admin/settings/oidc", s.withPermission("admin.settings.write", s.putOIDCSettings))
	mux.HandleFunc("GET /api/v1/admin/ai", s.withPermission("admin.settings.read", s.getAISettings))
	mux.HandleFunc("PATCH /api/v1/admin/ai", s.withPermission("admin.settings.write", s.updateAISettings))
	mux.HandleFunc("GET /api/v1/admin/settings/ai", s.withPermission("admin.settings.read", s.getAISettings))
	mux.HandleFunc("PUT /api/v1/admin/settings/ai", s.withPermission("admin.settings.write", s.putAISettings))
	mux.HandleFunc("GET /api/v1/admin/permissions", s.withPermission("admin.rbac.read", s.listPermissions))
	mux.HandleFunc("GET /api/v1/admin/roles", s.withPermission("admin.rbac.read", s.listRoles))
	mux.HandleFunc("POST /api/v1/admin/roles", s.withPermission("admin.rbac.write", s.createRoleCompat))
	mux.HandleFunc("PATCH /api/v1/admin/roles/{id}", s.withPermission("admin.rbac.write", s.updateRole))
	mux.HandleFunc("PUT /api/v1/admin/roles/{id}", s.withPermission("admin.rbac.write", s.putRole))
	mux.HandleFunc("PUT /api/v1/admin/roles/{id}/permissions", s.withPermission("admin.rbac.write", s.replaceRolePermissions))
	mux.HandleFunc("DELETE /api/v1/admin/roles/{id}", s.withPermission("admin.rbac.write", s.deleteRole))
	mux.HandleFunc("GET /api/v1/admin/users", s.withPermission("admin.users.read", s.listUsers))
	mux.HandleFunc("POST /api/v1/admin/users", s.withPermission("admin.users.write", s.createUser))
	mux.HandleFunc("PATCH /api/v1/admin/users/{id}", s.withPermission("admin.users.write", s.updateUser))
	mux.HandleFunc("PUT /api/v1/admin/users/{id}", s.withPermission("admin.users.write", s.putUser))
	mux.HandleFunc("PUT /api/v1/admin/users/{id}/roles", s.withPermission("admin.users.write", s.replaceUserRoles))
	mux.HandleFunc("GET /api/v1/admin/audit", s.withPermission("audit.read", s.listAudit))
	mux.HandleFunc("GET /api/v1/admin/scripts", s.withPermission("admin.scripts.read", s.listScripts))
	mux.HandleFunc("POST /api/v1/admin/scripts", s.withPermission("admin.scripts.write", s.createScript))
	mux.HandleFunc("PUT /api/v1/admin/scripts/{id}", s.withPermission("admin.scripts.write", s.updateScript))
	mux.HandleFunc("POST /api/v1/admin/scripts/{id}/approve", s.withPermission("admin.scripts.write", s.approveScript))
	mux.HandleFunc("DELETE /api/v1/admin/scripts/{id}", s.withPermission("admin.scripts.write", s.revokeScript))
	mux.HandleFunc("GET /api/v1/admin/registries", s.withPermission("admin.registries.read", s.listRegistries))
	mux.HandleFunc("POST /api/v1/admin/registries", s.withPermission("admin.registries.write", s.createRegistry))
	mux.HandleFunc("PUT /api/v1/admin/registries/{id}", s.withPermission("admin.registries.write", s.updateRegistry))
	mux.HandleFunc("DELETE /api/v1/admin/registries/{id}", s.withPermission("admin.registries.write", s.revokeRegistry))
	mux.HandleFunc("GET /api/v1/admin/target-credentials", s.withPermission("admin.credentials.read", s.listTargetCredentials))
	mux.HandleFunc("POST /api/v1/admin/target-credentials", s.withSessionPermission("admin.credentials.write", s.createTargetCredential))
	mux.HandleFunc("PUT /api/v1/admin/target-credentials/{id}", s.withSessionPermission("admin.credentials.write", s.updateTargetCredential))
	mux.HandleFunc("POST /api/v1/admin/target-credentials/{id}/rotate", s.withSessionPermission("admin.credentials.write", s.rotateTargetCredential))
	mux.HandleFunc("DELETE /api/v1/admin/target-credentials/{id}", s.withSessionPermission("admin.credentials.write", s.revokeTargetCredential))
	mux.HandleFunc("GET /api/v1/admin/runners", s.withPermission("admin.runners.read", s.listRunners))
	mux.HandleFunc("POST /api/v1/admin/runners", s.withPermission("admin.runners.write", s.createRunner))
	mux.HandleFunc("PUT /api/v1/admin/runners/{id}", s.withPermission("admin.runners.write", s.updateRunner))
	mux.HandleFunc("DELETE /api/v1/admin/runners/{id}", s.withPermission("admin.runners.write", s.deleteRunner))
	mux.HandleFunc("GET /api/v1/admin/deployment-presets", s.withPermission("admin.presets.read", s.listDeploymentPresets))
	mux.HandleFunc("POST /api/v1/admin/deployment-presets", s.withPermission("admin.presets.write", s.createDeploymentPreset))
	mux.HandleFunc("GET /api/v1/admin/deployment-presets/{id}", s.withPermission("admin.presets.read", s.getDeploymentPreset))
	mux.HandleFunc("PATCH /api/v1/admin/deployment-presets/{id}", s.withPermission("admin.presets.write", s.updateDeploymentPreset))
	mux.HandleFunc("PUT /api/v1/admin/deployment-presets/{id}", s.withPermission("admin.presets.write", s.updateDeploymentPreset))
	mux.HandleFunc("DELETE /api/v1/admin/deployment-presets/{id}", s.withPermission("admin.presets.write", s.deleteDeploymentPreset))
	mux.HandleFunc("POST /api/v1/runner/heartbeat", s.runnerHeartbeat)

	mux.HandleFunc("GET /api/v1/applications", s.withPermission("applications.read", s.listApplications))
	mux.HandleFunc("POST /api/v1/applications", s.withPermission("applications.write", s.createApplication))
	mux.HandleFunc("GET /api/v1/applications/{id}", s.withPermission("applications.read", s.getApplication))
	mux.HandleFunc("PATCH /api/v1/applications/{id}", s.withPermission("applications.write", s.updateApplication))
	mux.HandleFunc("PUT /api/v1/applications/{id}", s.withPermission("applications.write", s.updateApplication))
	mux.HandleFunc("DELETE /api/v1/applications/{id}", s.withPermission("applications.write", s.deleteApplication))
	mux.HandleFunc("GET /api/v1/applications/{id}/environments", s.withPermission("applications.read", s.listEnvironments))
	mux.HandleFunc("POST /api/v1/applications/{id}/environments", s.withPermission("applications.write", s.createEnvironment))
	mux.HandleFunc("PATCH /api/v1/environments/{id}", s.withPermission("applications.write", s.updateEnvironment))
	mux.HandleFunc("PUT /api/v1/environments/{id}", s.withPermission("applications.write", s.updateEnvironment))
	mux.HandleFunc("DELETE /api/v1/environments/{id}", s.withPermission("applications.write", s.deleteEnvironment))
	mux.HandleFunc("GET /api/v1/environments", s.withPermission("applications.read", s.listAllEnvironments))
	mux.HandleFunc("POST /api/v1/environments", s.withPermission("applications.write", s.createEnvironment))
	mux.HandleFunc("GET /api/v1/profiles", s.withPermission("profiles.read", s.listProfiles))
	mux.HandleFunc("POST /api/v1/profiles", s.withPermission("profiles.write", s.createProfile))
	mux.HandleFunc("GET /api/v1/profiles/{id}", s.withPermission("profiles.read", s.getProfile))
	mux.HandleFunc("PATCH /api/v1/profiles/{id}", s.withPermission("profiles.write", s.updateProfile))
	mux.HandleFunc("PUT /api/v1/profiles/{id}", s.withPermission("profiles.write", s.updateProfile))
	mux.HandleFunc("DELETE /api/v1/profiles/{id}", s.withPermission("profiles.write", s.deleteProfile))
	mux.HandleFunc("GET /api/v1/deployment-profiles", s.withPermission("profiles.read", s.listProfiles))
	mux.HandleFunc("POST /api/v1/deployment-profiles", s.withPermission("profiles.write", s.createProfile))
	mux.HandleFunc("GET /api/v1/deployment-profiles/{id}", s.withPermission("profiles.read", s.getProfile))
	mux.HandleFunc("PATCH /api/v1/deployment-profiles/{id}", s.withPermission("profiles.write", s.updateProfile))
	mux.HandleFunc("PUT /api/v1/deployment-profiles/{id}", s.withPermission("profiles.write", s.updateProfile))
	mux.HandleFunc("DELETE /api/v1/deployment-profiles/{id}", s.withPermission("profiles.write", s.deleteProfile))
	mux.HandleFunc("GET /api/v1/dashboard", s.withPermission("releases.read", s.dashboard))

	mux.HandleFunc("GET /api/v1/releases", s.withPermission("releases.read", s.listReleases))
	mux.HandleFunc("POST /api/v1/releases", s.withPermission("releases.create", s.createRelease))
	mux.HandleFunc("POST /api/v1/releases/preflight", s.withPermission("releases.create", s.preflightQuickRelease))
	mux.HandleFunc("POST /api/v1/releases/quick", s.withPermission("releases.create", s.quickRelease))
	mux.HandleFunc("GET /api/v1/releases/{id}", s.withPermission("releases.read", s.getRelease))
	mux.HandleFunc("PATCH /api/v1/releases/{id}", s.withPermission("releases.write", s.updateRelease))
	mux.HandleFunc("DELETE /api/v1/releases/{id}", s.withPermission("releases.write", s.deleteRelease))
	mux.HandleFunc("GET /api/v1/releases/{id}/artifacts", s.withPermission("releases.read", s.listArtifacts))
	mux.HandleFunc("POST /api/v1/releases/{id}/artifacts", s.withPermission("releases.create", s.createArtifactMetadata))
	mux.HandleFunc("POST /api/v1/releases/{id}/artifacts/upload", s.withPermission("releases.create", s.uploadArtifact))
	mux.HandleFunc("POST /api/v1/releases/{id}/enqueue", s.withPermission("releases.submit", s.enqueueRelease))
	mux.HandleFunc("POST /api/v1/releases/{id}/submit-review", s.withPermission("releases.submit", s.enqueueRelease))
	mux.HandleFunc("POST /api/v1/releases/{id}/deploy", s.withPermission("releases.submit", s.enqueueRelease))
	mux.HandleFunc("POST /api/v1/releases/{id}/review", s.withPermission("releases.review", s.reviewRelease))
	mux.HandleFunc("POST /api/v1/releases/{id}/approve", s.withPermission("releases.approve", s.approveRelease))
	mux.HandleFunc("POST /api/v1/releases/{id}/reject", s.withPermission("releases.reject", s.rejectRelease))
	mux.HandleFunc("POST /api/v1/releases/{id}/rollback", s.withPermission("releases.submit", s.rollbackRelease))
	mux.HandleFunc("POST /api/v1/releases/{id}/retry", s.withPermission("releases.submit", s.retryRelease))
	mux.HandleFunc("GET /api/v1/releases/{id}/logs/stream", s.withPermission("releases.read", s.streamReleaseLogs))
	mux.HandleFunc("GET /api/v1/jobs", s.withPermission("releases.read", s.listJobs))
	mux.HandleFunc("POST /api/v1/ai/chat/completions", s.withPermission("ai.use", s.proxyAI))

	mux.HandleFunc("GET /mcp", s.withPermission("mcp.use", s.mcpGET))
	mux.HandleFunc("POST /mcp", s.withPermission("mcp.use", s.mcpPOST))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "API route not found")
	})

	if s.webRoot != "" {
		mux.Handle("/", s.spaHandler(s.webRoot))
	}
	return s.recover(s.securityHeaders(s.requestID(s.accessLog(mux))))
}

type contextKey int

const principalKey contextKey = 1

func principalFrom(r *http.Request) (store.Principal, bool) {
	p, ok := r.Context().Value(principalKey).(store.Principal)
	return p, ok
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p store.Principal
		var err error
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			bearer := strings.TrimSpace(auth[7:])
			p, err = s.store.AuthenticateAPIKey(r.Context(), bearer)
		} else if cookie, cookieErr := r.Cookie("releasedock_session"); cookieErr == nil {
			p, err = s.store.AuthenticateSession(r.Context(), cookie.Value)
			if err == nil && isUnsafeMethod(r.Method) {
				csrf := r.Header.Get("X-CSRF-Token")
				if csrf == "" || !secure.ConstantTimeHashEqual(p.CSRFHash, csrf) {
					writeError(w, http.StatusForbidden, "csrf_failed", "missing or invalid CSRF token")
					return
				}
			}
		} else {
			err = errors.New("credentials not provided")
		}
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	}
}

func (s *Server) withPermission(permission string, next http.HandlerFunc) http.HandlerFunc {
	return s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFrom(r)
		if !p.Has(permission) {
			writeError(w, http.StatusForbidden, "forbidden", "permission required: "+permission)
			return
		}
		next(w, r)
	})
}

func (s *Server) withSessionPermission(permission string, next http.HandlerFunc) http.HandlerFunc {
	return s.withPermission(permission, func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFrom(r)
		if !sessionCanMutatePersonalKeys(p) {
			writeError(w, http.StatusForbidden, "session_required", "this security-sensitive operation requires a browser session")
			return
		}
		next(w, r)
	})
}

func sessionCanMutatePersonalKeys(p store.Principal) bool { return !p.ViaAPIKey }

func isUnsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON value")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Pool.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.build)
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := secure.RandomToken(16)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		s.log.Info("http request", "method", r.Method, "path", r.URL.Path, "status", recorder.status, "duration_ms", time.Since(started).Milliseconds())
	})
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("request panic", "error", recovered, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func FindWebRoot() string {
	candidates := []string{filepath.Join("web", "dist"), "web"}
	if executable, err := os.Executable(); err == nil {
		directory := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(directory, "web", "dist"),
			filepath.Join(directory, "web"),
			filepath.Join(directory, "..", "web", "dist"),
			filepath.Join(directory, "..", "web"),
		)
	}
	return findWebRoot(candidates)
}

func findWebRoot(candidates []string) string {
	for _, candidate := range candidates {
		index, indexErr := os.ReadFile(filepath.Join(candidate, "index.html"))
		assets, assetsErr := os.Stat(filepath.Join(candidate, "assets"))
		builtIndex := bytes.Contains(index, []byte("/assets/")) || bytes.Contains(index, []byte("./assets/"))
		if indexErr != nil || assetsErr != nil || !assets.IsDir() || !builtIndex {
			continue
		}
		absolute, _ := filepath.Abs(candidate)
		return absolute
	}
	return ""
}

func (s *Server) spaHandler(root string) http.Handler {
	fileSystem := os.DirFS(root)
	fileServer := http.FileServer(http.FS(fileSystem))
	index, indexErr := fs.ReadFile(fileSystem, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusNotFound, "not_found", "route not found")
			return
		}
		clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(r.URL.Path)), "/")
		if clean == "." || clean == "" {
			clean = "index.html"
		}
		if info, err := fs.Stat(fileSystem, clean); err == nil && !info.IsDir() {
			if contentType := mime.TypeByExtension(filepath.Ext(clean)); contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		if indexErr != nil {
			writeError(w, http.StatusNotFound, "not_found", "web application entry point not found")
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(index)
		}
	})
}

type loginAttempt struct {
	count       int
	until       time.Time
	windowStart time.Time
	lastSeen    time.Time
}

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{attempts: make(map[string]loginAttempt)} }

const (
	limiterEntryTTL = 15 * time.Minute
	limiterMaxKeys  = 4096
)

func (l *loginLimiter) maintain(now time.Time) {
	for key, attempt := range l.attempts {
		if now.Sub(attempt.lastSeen) > limiterEntryTTL && (attempt.until.IsZero() || now.After(attempt.until)) {
			delete(l.attempts, key)
		}
	}
	for len(l.attempts) >= limiterMaxKeys {
		var oldestKey string
		var oldest time.Time
		for key, attempt := range l.attempts {
			if oldestKey == "" || attempt.lastSeen.Before(oldest) {
				oldestKey, oldest = key, attempt.lastSeen
			}
		}
		if oldestKey == "" {
			break
		}
		delete(l.attempts, oldestKey)
	}
}

func (l *loginLimiter) allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.maintain(now)
	a := l.attempts[key]
	if !a.until.IsZero() && now.Before(a.until) {
		a.lastSeen = now
		l.attempts[key] = a
		return false
	}
	if !a.until.IsZero() {
		delete(l.attempts, key)
	}
	return true
}

func (l *loginLimiter) failure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.maintain(now)
	a := l.attempts[key]
	a.count++
	a.lastSeen = now
	if a.count >= 5 {
		a.until = now.Add(5 * time.Minute)
		a.count = 0
	}
	l.attempts[key] = a
}

func (l *loginLimiter) success(key string) { l.mu.Lock(); delete(l.attempts, key); l.mu.Unlock() }

func (l *loginLimiter) consume(key string, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.maintain(now)
	a := l.attempts[key]
	if a.windowStart.IsZero() || now.Sub(a.windowStart) >= window {
		a.windowStart = now
		a.count = 0
	}
	a.lastSeen = now
	a.count++
	l.attempts[key] = a
	return a.count <= limit
}
