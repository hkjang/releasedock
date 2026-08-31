package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/jackc/pgx/v5"
)

type appSettingsResponse struct {
	ServiceName         string    `json:"service_name"`
	ApprovalEnabled     bool      `json:"approval_enabled"`
	ArtifactStoragePath string    `json:"artifact_storage_path"`
	ArtifactMaxBytes    int64     `json:"artifact_max_bytes"`
	AllowedOrigins      []string  `json:"allowed_origins"`
	UpdatedAt           time.Time `json:"updated_at"`
}

func (s *Server) loadAppSettings(r *http.Request) (appSettingsResponse, error) {
	var cfg appSettingsResponse
	err := s.store.Pool.QueryRow(r.Context(), `SELECT service_name,approval_enabled,artifact_storage_path,artifact_max_bytes,allowed_origins,updated_at FROM app_settings WHERE id='default'`).
		Scan(&cfg.ServiceName, &cfg.ApprovalEnabled, &cfg.ArtifactStoragePath, &cfg.ArtifactMaxBytes, &cfg.AllowedOrigins, &cfg.UpdatedAt)
	return cfg, err
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadAppSettings(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load settings")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

type appSettingsInput struct {
	ServiceName         *string   `json:"service_name"`
	ApprovalEnabled     *bool     `json:"approval_enabled"`
	ArtifactStoragePath *string   `json:"artifact_storage_path"`
	ArtifactMaxBytes    *int64    `json:"artifact_max_bytes"`
	AllowedOrigins      *[]string `json:"allowed_origins"`
}

var errArtifactStorageInUse = errors.New("artifact storage path is locked while uploaded artifacts exist")

const managedDataRoot = "/var/lib/releasedock"

func normalizeArtifactStoragePath(value string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(value))
	if !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return "", errors.New("artifact storage path must be an absolute, non-root directory")
	}
	relative, err := filepath.Rel(managedDataRoot, clean)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("artifact storage path must be below /var/lib/releasedock")
	}
	return clean, nil
}

func artifactStoragePathChanged(current, next string) bool {
	return filepath.Clean(current) != filepath.Clean(next)
}

func ensureArtifactStorageChange(ctx context.Context, tx pgx.Tx, next string) error {
	var current string
	if err := tx.QueryRow(ctx, `SELECT artifact_storage_path FROM app_settings WHERE id='default' FOR UPDATE`).Scan(&current); err != nil {
		return err
	}
	if !artifactStoragePathChanged(current, next) {
		return nil
	}
	var uploaded bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM release_artifacts WHERE storage_path<>'')`).Scan(&uploaded); err != nil {
		return err
	}
	if uploaded {
		return errArtifactStorageInUse
	}
	return nil
}

func validateAppSettings(input appSettingsInput) error {
	if input.ServiceName != nil && (strings.TrimSpace(*input.ServiceName) == "" || len(*input.ServiceName) > 100) {
		return errors.New("service_name is required and must not exceed 100 characters")
	}
	if input.ArtifactStoragePath != nil {
		if _, err := normalizeArtifactStoragePath(*input.ArtifactStoragePath); err != nil {
			return errors.New(strings.NewReplacer("artifact storage path", "artifact_storage_path").Replace(err.Error()))
		}
	}
	if input.ArtifactMaxBytes != nil && (*input.ArtifactMaxBytes < 1<<20 || *input.ArtifactMaxBytes > 1<<40) {
		return errors.New("artifact_max_bytes must be between 1 MiB and 1 TiB")
	}
	if input.AllowedOrigins != nil {
		seen := map[string]bool{}
		for _, origin := range *input.AllowedOrigins {
			u, err := url.Parse(origin)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
				return errors.New("each allowed origin must contain only an http(s) scheme and host")
			}
			if seen[origin] {
				return errors.New("allowed_origins must not contain duplicates")
			}
			seen[origin] = true
		}
	}
	return nil
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var input appSettingsInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := validateAppSettings(input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_settings", err.Error())
		return
	}
	if input.ArtifactStoragePath != nil {
		clean, _ := normalizeArtifactStoragePath(*input.ArtifactStoragePath)
		input.ArtifactStoragePath = &clean
	}
	p, _ := principalFrom(r)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not update settings")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if input.ArtifactStoragePath != nil {
		if err := ensureArtifactStorageChange(r.Context(), tx, *input.ArtifactStoragePath); err != nil {
			if errors.Is(err, errArtifactStorageInUse) {
				writeError(w, http.StatusConflict, "artifact_storage_in_use", err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, "database_error", "could not validate artifact storage")
			}
			return
		}
	}
	_, err = tx.Exec(r.Context(), `UPDATE app_settings SET
		service_name=COALESCE($1,service_name), approval_enabled=COALESCE($2,approval_enabled),
		artifact_storage_path=COALESCE($3,artifact_storage_path), artifact_max_bytes=COALESCE($4,artifact_max_bytes),
		allowed_origins=COALESCE($5,allowed_origins),updated_by=$6,updated_at=now() WHERE id='default'`,
		input.ServiceName, input.ApprovalEnabled, input.ArtifactStoragePath, input.ArtifactMaxBytes, input.AllowedOrigins, p.UserID)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not update settings")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "settings.update", "settings", "default", "success", remoteIP(r), r.UserAgent(), nil)
	s.getSettings(w, r)
}

func (s *Server) getGeneralSettings(w http.ResponseWriter, r *http.Request) {
	var name string
	var maxBytes int64
	var allowedOrigins []string
	var raw json.RawMessage
	err := s.store.Pool.QueryRow(r.Context(), `SELECT service_name,artifact_max_bytes,allowed_origins,general_config FROM app_settings WHERE id='default'`).Scan(&name, &maxBytes, &allowedOrigins, &raw)
	if err != nil {
		writeError(w, 500, "database_error", "could not load settings")
		return
	}
	values := map[string]any{}
	_ = json.Unmarshal(raw, &values)
	values["serviceName"] = name
	values["artifactMaxSizeGb"] = maxBytes >> 30
	values["allowedOrigins"] = allowedOrigins
	if _, ok := values["publicUrl"]; !ok {
		values["publicUrl"] = ""
	}
	if _, ok := values["secureCookies"]; !ok {
		values["secureCookies"] = false
	}
	writeJSON(w, 200, values)
}

func (s *Server) updateGeneralSettings(w http.ResponseWriter, r *http.Request) {
	s.updateSettings(w, r)
}

func (s *Server) getApprovalSettings(w http.ResponseWriter, r *http.Request) {
	var enabled bool
	var raw json.RawMessage
	err := s.store.Pool.QueryRow(r.Context(), `SELECT approval_enabled,approval_config FROM app_settings WHERE id='default'`).Scan(&enabled, &raw)
	if err != nil {
		writeError(w, 500, "database_error", "could not load settings")
		return
	}
	values := map[string]any{}
	_ = json.Unmarshal(raw, &values)
	values["enabled"] = enabled
	if _, ok := values["minimumApprovers"]; !ok {
		values["minimumApprovers"] = 1
	}
	writeJSON(w, 200, values)
}

func (s *Server) updateApprovalSettings(w http.ResponseWriter, r *http.Request) {
	s.updateSettings(w, r)
}

func (s *Server) getStorageSettings(w http.ResponseWriter, r *http.Request) {
	var path string
	var raw json.RawMessage
	err := s.store.Pool.QueryRow(r.Context(), `SELECT artifact_storage_path,storage_config FROM app_settings WHERE id='default'`).Scan(&path, &raw)
	if err != nil {
		writeError(w, 500, "database_error", "could not load settings")
		return
	}
	values := map[string]any{}
	_ = json.Unmarshal(raw, &values)
	values["driver"] = "local"
	values["localPath"] = path
	writeJSON(w, 200, values)
}

func (s *Server) updateStorageSettings(w http.ResponseWriter, r *http.Request) {
	s.updateSettings(w, r)
}

func (s *Server) getOIDCSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadOIDC(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load OIDC settings")
		return
	}
	// Missing or unset is not an error here; it simply means the redirect URI
	// falls back to the incoming request.
	publicURL, _ := s.configuredPublicOrigin(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": cfg.Enabled, "issuerUrl": cfg.Issuer, "clientId": cfg.ClientID,
		"secretConfigured": cfg.ClientSecretEnc != "", "redirectUrl": cfg.RedirectURL,
		"allowInsecureEndpoints": cfg.AllowInsecureEndpoints,
		// Read-only context: the public URL lives in general settings but is what
		// the redirect URI is derived from, so it is shown where SSO is configured.
		"publicUrl": publicURL,
		// What the server will actually send, so an administrator can register
		// it in Keycloak without configuring it here.
		"effectiveRedirectUri": s.resolveOIDCRedirectURI(r.Context(), r, cfg),
		"scopes":               strings.Join(cfg.Scopes, " "), "autoProvision": cfg.AutoCreateUser, "defaultRoleId": cfg.DefaultRoleID,
		"verifyTls": true, "usernameClaim": "preferred_username", "groupsClaim": "groups",
	})
}

type oidcSettingsInput struct {
	Enabled        *bool     `json:"enabled"`
	Issuer         *string   `json:"issuer"`
	ClientID       *string   `json:"client_id"`
	ClientSecret   *string   `json:"client_secret"`
	RedirectURL    *string   `json:"redirect_url"`
	AllowInsecure  *bool     `json:"allow_insecure_endpoints"`
	Scopes         *[]string `json:"scopes"`
	AutoCreateUser *bool     `json:"auto_create_user"`
	DefaultRoleID  *string   `json:"default_role_id"`
}

func (s *Server) updateOIDCSettings(w http.ResponseWriter, r *http.Request) {
	var input oidcSettingsInput
	if !decodeJSON(w, r, &input) {
		return
	}
	p, _ := principalFrom(r)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not update OIDC settings")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err = lockAdminDelegation(r.Context(), tx); err != nil {
		writeError(w, 500, "database_error", "could not lock role delegation")
		return
	}
	current, err := loadOIDCWithQueryer(r.Context(), tx, true)
	if err != nil {
		writeError(w, 500, "database_error", "could not load OIDC settings")
		return
	}
	if input.Enabled != nil {
		current.Enabled = *input.Enabled
	}
	if input.Issuer != nil {
		current.Issuer = strings.TrimSuffix(strings.TrimSpace(*input.Issuer), "/")
	}
	if input.ClientID != nil {
		current.ClientID = strings.TrimSpace(*input.ClientID)
	}
	if input.RedirectURL != nil {
		current.RedirectURL = strings.TrimSpace(*input.RedirectURL)
	}
	if input.AllowInsecure != nil {
		current.AllowInsecureEndpoints = *input.AllowInsecure
	}
	if input.Scopes != nil {
		current.Scopes = *input.Scopes
	}
	if input.AutoCreateUser != nil {
		current.AutoCreateUser = *input.AutoCreateUser
	}
	if input.DefaultRoleID != nil {
		if *input.DefaultRoleID == "" {
			current.DefaultRoleID = nil
		} else {
			value := *input.DefaultRoleID
			current.DefaultRoleID = &value
		}
	}
	secretEnc := current.ClientSecretEnc
	if input.ClientSecret != nil {
		if *input.ClientSecret == "" {
			secretEnc = ""
		} else if len(*input.ClientSecret) > 4096 {
			writeError(w, 400, "invalid_oidc_settings", "client_secret is too long")
			return
		} else if secretEnc, err = s.vault.Encrypt(*input.ClientSecret, "oidc.client_secret"); err != nil {
			writeError(w, 500, "secret_error", "could not encrypt client secret")
			return
		}
	}
	if current.Enabled {
		if current.ClientID == "" || secretEnc == "" || !contains(current.Scopes, "openid") {
			writeError(w, 400, "invalid_oidc_settings", "enabled OIDC requires issuer, client_id, client_secret, and openid scope")
			return
		}
		// The redirect URI is optional: when it is blank the server derives it
		// from the public URL or the incoming request. Only an explicit value
		// is format-checked.
		if current.RedirectURL != "" {
			if err := validateOIDCEndpoint("redirect_url", current.RedirectURL, current.AllowInsecureEndpoints); err != nil {
				writeError(w, 400, "invalid_oidc_settings", err.Error())
				return
			}
			redirect, parseErr := url.Parse(current.RedirectURL)
			if parseErr != nil || redirect.RawQuery != "" {
				writeError(w, 400, "invalid_oidc_settings", "redirect_url must not contain a query string")
				return
			}
		}
		ctx, cancel := contextWithTimeout(r, 10*time.Second)
		_, discoveryErr := s.discoverOIDC(ctx, current.Issuer, current.AllowInsecureEndpoints)
		cancel()
		if discoveryErr != nil {
			writeError(w, 400, "oidc_discovery_failed", discoveryErr.Error())
			return
		}
	}
	if current.DefaultRoleID != nil {
		if err = validateDelegatedRoles(r.Context(), tx, p.UserID, []string{*current.DefaultRoleID}); err != nil {
			writeError(w, http.StatusForbidden, "delegation_forbidden", err.Error())
			return
		}
	}
	_, err = tx.Exec(r.Context(), `UPDATE oidc_settings SET enabled=$1,issuer=$2,client_id=$3,client_secret_enc=$4,redirect_url=$5,scopes=$6,auto_create_user=$7,allow_insecure_endpoints=$8,default_role_id=$9,updated_by=$10,updated_at=now() WHERE id='default'`, current.Enabled, current.Issuer, current.ClientID, secretEnc, current.RedirectURL, current.Scopes, current.AutoCreateUser, current.AllowInsecureEndpoints, current.DefaultRoleID, p.UserID)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 400, "invalid_oidc_settings", "could not update OIDC settings")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "settings.oidc.update", "settings", "oidc", "success", remoteIP(r), r.UserAgent(), nil)
	s.getOIDCSettings(w, r)
}

func contextWithTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), timeout)
}

type aiSettings struct {
	Enabled   bool
	Endpoint  string
	Model     string
	APIKeyEnc string
	MaxTokens int
}

func (s *Server) loadAI(ctx context.Context) (aiSettings, error) {
	var cfg aiSettings
	err := s.store.Pool.QueryRow(ctx, `SELECT enabled,endpoint,model,api_key_enc,max_tokens FROM ai_settings WHERE id='default'`).Scan(&cfg.Enabled, &cfg.Endpoint, &cfg.Model, &cfg.APIKeyEnc, &cfg.MaxTokens)
	return cfg, err
}

func (s *Server) getAISettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadAI(r.Context())
	if err != nil {
		writeError(w, 500, "database_error", "could not load AI settings")
		return
	}
	writeJSON(w, 200, map[string]any{"enabled": cfg.Enabled, "baseUrl": cfg.Endpoint, "model": cfg.Model, "keyConfigured": cfg.APIKeyEnc != "", "maxTokens": cfg.MaxTokens, "provider": "openai-compatible", "streamingDefault": true})
}

type aiSettingsInput struct {
	Enabled   *bool   `json:"enabled"`
	Endpoint  *string `json:"endpoint"`
	Model     *string `json:"model"`
	APIKey    *string `json:"api_key"`
	MaxTokens *int    `json:"max_tokens"`
}

func (s *Server) updateAISettings(w http.ResponseWriter, r *http.Request) {
	var input aiSettingsInput
	if !decodeJSON(w, r, &input) {
		return
	}
	cfg, err := s.loadAI(r.Context())
	if err != nil {
		writeError(w, 500, "database_error", "could not load AI settings")
		return
	}
	if input.Enabled != nil {
		cfg.Enabled = *input.Enabled
	}
	if input.Endpoint != nil {
		cfg.Endpoint = strings.TrimRight(strings.TrimSpace(*input.Endpoint), "/")
	}
	if input.Model != nil {
		cfg.Model = strings.TrimSpace(*input.Model)
	}
	if input.MaxTokens != nil {
		cfg.MaxTokens = *input.MaxTokens
	}
	apiKeyEnc := cfg.APIKeyEnc
	if input.APIKey != nil {
		if *input.APIKey == "" {
			apiKeyEnc = ""
		} else if len(*input.APIKey) > 8192 {
			writeError(w, 400, "invalid_ai_settings", "api_key is too long")
			return
		} else if apiKeyEnc, err = s.vault.Encrypt(*input.APIKey, "ai.api_key"); err != nil {
			writeError(w, 500, "secret_error", "could not encrypt API key")
			return
		}
	}
	endpoint, parseErr := url.Parse(cfg.Endpoint)
	endpointInvalid := parseErr != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != ""
	if cfg.MaxTokens < 1 || cfg.MaxTokens > 262144 || (cfg.Enabled && (endpointInvalid || cfg.Model == "")) {
		writeError(w, 400, "invalid_ai_settings", "enabled AI requires an http(s) endpoint, model, and max_tokens between 1 and 262144")
		return
	}
	p, _ := principalFrom(r)
	_, err = s.store.Pool.Exec(r.Context(), `UPDATE ai_settings SET enabled=$1,endpoint=$2,model=$3,api_key_enc=$4,max_tokens=$5,updated_by=$6,updated_at=now() WHERE id='default'`, cfg.Enabled, cfg.Endpoint, cfg.Model, apiKeyEnc, cfg.MaxTokens, p.UserID)
	if err != nil {
		writeError(w, 500, "database_error", "could not update AI settings")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "settings.ai.update", "settings", "ai", "success", remoteIP(r), r.UserAgent(), nil)
	s.getAISettings(w, r)
}

func (s *Server) listPermissions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Pool.Query(r.Context(), `SELECT code,description FROM permissions ORDER BY code`)
	if err != nil {
		writeError(w, 500, "database_error", "could not list permissions")
		return
	}
	defer rows.Close()
	items := []map[string]string{}
	for rows.Next() {
		var code, description string
		if rows.Scan(&code, &description) != nil {
			writeError(w, 500, "database_error", "could not list permissions")
			return
		}
		items = append(items, map[string]string{"code": code, "description": description})
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) listRoles(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	var total int
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM roles WHERE $1='' OR name ILIKE '%'||$1||'%' OR description ILIKE '%'||$1||'%'`, search).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not count roles")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT r.id,r.name,r.description,r.system,r.created_at,r.updated_at,COALESCE(array_agg(rp.permission_code ORDER BY rp.permission_code) FILTER(WHERE rp.permission_code IS NOT NULL),'{}') FROM roles r LEFT JOIN role_permissions rp ON rp.role_id=r.id WHERE $1='' OR r.name ILIKE '%'||$1||'%' OR r.description ILIKE '%'||$1||'%' GROUP BY r.id ORDER BY r.name LIMIT $2 OFFSET $3`, search, limit, offset)
	if err != nil {
		writeError(w, 500, "database_error", "could not list roles")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, name, description string
		var system bool
		var created, updated time.Time
		var permissions []string
		if rows.Scan(&id, &name, &description, &system, &created, &updated, &permissions) != nil {
			writeError(w, 500, "database_error", "could not list roles")
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "description": description, "system": system, "permissions": permissions, "created_at": created, "updated_at": updated})
	}
	writeJSON(w, 200, page(items, total, limit, offset))
}

type roleInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (s *Server) createRole(w http.ResponseWriter, r *http.Request) {
	var input roleInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 100 {
		writeError(w, 400, "invalid_role", "name is required and must not exceed 100 characters")
		return
	}
	id, _ := secure.NewID()
	_, err := s.store.Pool.Exec(r.Context(), `INSERT INTO roles(id,name,description) VALUES($1,$2,$3)`, id, input.Name, input.Description)
	if err != nil {
		writeError(w, 409, "role_conflict", "role name already exists")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "role.create", "role", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 201, map[string]any{"id": id, "name": input.Name, "description": input.Description, "permissions": []string{}})
}

func (s *Server) updateRole(w http.ResponseWriter, r *http.Request) {
	var input roleInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 100 {
		writeError(w, 400, "invalid_role", "valid name is required")
		return
	}
	id := r.PathValue("id")
	actor, _ := principalFrom(r)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not update role")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err = lockAdminDelegation(r.Context(), tx); err != nil {
		writeError(w, 500, "database_error", "could not lock role delegation")
		return
	}
	if _, err = validateRoleMutationTarget(r.Context(), tx, actor.UserID, id); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "role not found")
		return
	} else if err != nil {
		writeError(w, http.StatusForbidden, "delegation_forbidden", err.Error())
		return
	}
	tag, err := tx.Exec(r.Context(), `UPDATE roles SET name=$2,description=$3,updated_at=now() WHERE id=$1 AND system=FALSE`, id, input.Name, input.Description)
	if err != nil {
		writeError(w, 409, "role_conflict", "role name already exists")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 400, "system_role", "system roles cannot be renamed")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 409, "role_conflict", "role update could not be committed")
		return
	}
	s.store.Audit(r.Context(), actor.UserID, "role.update", "role", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 200, map[string]any{"id": id, "name": input.Name, "description": input.Description})
}

func (s *Server) replaceRolePermissions(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Permissions []string `json:"permissions"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if id == "role-admin" {
		writeError(w, 400, "protected_role", "Administrator permissions are protected to prevent service lockout")
		return
	}
	actor, _ := principalFrom(r)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not update permissions")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err = lockAdminDelegation(r.Context(), tx); err != nil {
		writeError(w, 500, "database_error", "could not lock role delegation")
		return
	}
	if err = validateRolePermissionMutation(r.Context(), tx, actor.UserID, id, input.Permissions); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "role not found")
		return
	} else if err != nil {
		writeError(w, http.StatusForbidden, "delegation_forbidden", err.Error())
		return
	}
	if _, err = tx.Exec(r.Context(), `DELETE FROM role_permissions WHERE role_id=$1`, id); err == nil {
		for _, permission := range input.Permissions {
			if _, err = tx.Exec(r.Context(), `INSERT INTO role_permissions(role_id,permission_code) VALUES($1,$2)`, id, permission); err != nil {
				break
			}
		}
	}
	if err != nil {
		writeError(w, 400, "invalid_permission", "one or more permissions do not exist")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "database_error", "could not update permissions")
		return
	}
	details, _ := json.Marshal(map[string]any{"permissions": input.Permissions})
	s.store.Audit(r.Context(), actor.UserID, "role.permissions.replace", "role", id, "success", remoteIP(r), r.UserAgent(), details)
	writeJSON(w, 200, map[string]any{"id": id, "permissions": input.Permissions})
}

func (s *Server) deleteRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	actor, _ := principalFrom(r)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not delete role")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err = lockAdminDelegation(r.Context(), tx); err != nil {
		writeError(w, 500, "database_error", "could not lock role delegation")
		return
	}
	system, err := validateRoleMutationTarget(r.Context(), tx, actor.UserID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "role not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusForbidden, "delegation_forbidden", err.Error())
		return
	}
	if system {
		writeError(w, 400, "system_role", "system role cannot be deleted")
		return
	}
	tag, err := tx.Exec(r.Context(), `DELETE FROM roles WHERE id=$1 AND system=FALSE`, id)
	if err != nil {
		writeError(w, 409, "role_in_use", "role is in use")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 400, "system_role", "system role cannot be deleted")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 409, "role_conflict", "role deletion could not be committed")
		return
	}
	s.store.Audit(r.Context(), actor.UserID, "role.delete", "role", id, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(204)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	if status != "" && status != "active" && status != "inactive" {
		writeError(w, http.StatusBadRequest, "invalid_filter", "status must be active or inactive")
		return
	}
	if source != "" && source != "local" && source != "oidc" {
		writeError(w, http.StatusBadRequest, "invalid_filter", "source must be local or oidc")
		return
	}
	filter := `($1='' OR username ILIKE '%'||$1||'%' OR display_name ILIKE '%'||$1||'%' OR email ILIKE '%'||$1||'%') AND ($2='' OR ($2='active' AND active) OR ($2='inactive' AND NOT active)) AND ($3='' OR auth_source=$3)`
	var total int
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM users WHERE `+filter, search, status, source).Scan(&total); err != nil {
		writeError(w, 500, "database_error", "could not count users")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT u.id,u.username,u.display_name,u.email,u.auth_source,u.active,u.created_at,u.updated_at,COALESCE(array_agg(ur.role_id ORDER BY ur.role_id) FILTER(WHERE ur.role_id IS NOT NULL),'{}') FROM users u LEFT JOIN user_roles ur ON ur.user_id=u.id WHERE ($1='' OR u.username ILIKE '%'||$1||'%' OR u.display_name ILIKE '%'||$1||'%' OR u.email ILIKE '%'||$1||'%') AND ($2='' OR ($2='active' AND u.active) OR ($2='inactive' AND NOT u.active)) AND ($3='' OR u.auth_source=$3) GROUP BY u.id ORDER BY u.username LIMIT $4 OFFSET $5`, search, status, source, limit, offset)
	if err != nil {
		writeError(w, 500, "database_error", "could not list users")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, username, display, email, source string
		var active bool
		var created, updated time.Time
		var roles []string
		if rows.Scan(&id, &username, &display, &email, &source, &active, &created, &updated, &roles) != nil {
			writeError(w, 500, "database_error", "could not list users")
			return
		}
		items = append(items, map[string]any{"id": id, "username": username, "displayName": display, "email": email, "source": source, "active": active, "roles": roles, "createdAt": created, "updatedAt": updated})
	}
	writeJSON(w, 200, page(items, total, limit, offset))
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	var input struct {
		DisplayName *string `json:"display_name"`
		Email       *string `json:"email"`
		Active      *bool   `json:"active"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	actor, _ := principalFrom(r)
	id := r.PathValue("id")
	if id == actor.UserID && input.Active != nil && !*input.Active {
		writeError(w, 400, "self_deactivation", "administrators cannot deactivate their own current session")
		return
	}
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not update user")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err = lockAdminDelegation(r.Context(), tx); err != nil {
		writeError(w, 500, "database_error", "could not lock Administrator membership")
		return
	}
	var currentActive bool
	var currentRoles []string
	err = tx.QueryRow(r.Context(), `SELECT active FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&currentActive)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "user not found")
		return
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not load user")
		return
	}
	if err = validateTargetUserAuthority(r.Context(), tx, actor.UserID, id); err != nil {
		writeError(w, http.StatusForbidden, "delegation_forbidden", err.Error())
		return
	}
	if err = tx.QueryRow(r.Context(), `SELECT COALESCE(array_agg(role_id ORDER BY role_id),'{}') FROM user_roles WHERE user_id=$1`, id).Scan(&currentRoles); err != nil {
		writeError(w, 500, "database_error", "could not load user roles")
		return
	}
	nextActive := currentActive
	if input.Active != nil {
		nextActive = *input.Active
	}
	if err = protectLastActiveAdmin(r.Context(), tx, id, nextActive, currentRoles); err != nil {
		writeError(w, 409, "last_active_admin", err.Error())
		return
	}
	tag, err := tx.Exec(r.Context(), `UPDATE users SET display_name=COALESCE($2,display_name),email=COALESCE($3,email),active=COALESCE($4,active),updated_at=now() WHERE id=$1`, id, input.DisplayName, input.Email, input.Active)
	if err != nil {
		writeError(w, 500, "database_error", "could not update user")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 404, "not_found", "user not found")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 409, "user_conflict", "user update could not be committed")
		return
	}
	s.store.Audit(r.Context(), actor.UserID, "user.update", "user", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) replaceUserRoles(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Roles []string `json:"roles"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	actor, _ := principalFrom(r)
	id := r.PathValue("id")
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not update roles")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err = lockAdminDelegation(r.Context(), tx); err != nil {
		writeError(w, 500, "database_error", "could not lock role delegation")
		return
	}
	var active, currentlyAdmin bool
	err = tx.QueryRow(r.Context(), `SELECT active FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "user not found")
		return
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not load user")
		return
	}
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM user_roles WHERE user_id=$1 AND role_id='role-admin')`, id).Scan(&currentlyAdmin); err != nil {
		writeError(w, 500, "database_error", "could not load user roles")
		return
	}
	if err = validateTargetUserAuthority(r.Context(), tx, actor.UserID, id); err != nil {
		writeError(w, http.StatusForbidden, "delegation_forbidden", err.Error())
		return
	}
	if err = validateDelegatedRoles(r.Context(), tx, actor.UserID, input.Roles); err != nil {
		writeError(w, http.StatusForbidden, "delegation_forbidden", err.Error())
		return
	}
	if id == actor.UserID && currentlyAdmin && !contains(input.Roles, "role-admin") {
		writeError(w, 400, "self_lockout", "cannot remove your own Administrator role")
		return
	}
	if err = protectLastActiveAdmin(r.Context(), tx, id, active, input.Roles); err != nil {
		writeError(w, 409, "last_active_admin", err.Error())
		return
	}
	if _, err = tx.Exec(r.Context(), `DELETE FROM user_roles WHERE user_id=$1`, id); err == nil {
		for _, role := range input.Roles {
			if _, err = tx.Exec(r.Context(), `INSERT INTO user_roles(user_id,role_id) VALUES($1,$2)`, id, role); err != nil {
				break
			}
		}
	}
	if err != nil {
		writeError(w, 400, "invalid_role", "user or role does not exist")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "database_error", "could not update roles")
		return
	}
	details, _ := json.Marshal(map[string]any{"roles": input.Roles})
	s.store.Audit(r.Context(), actor.UserID, "user.roles.replace", "user", id, "success", remoteIP(r), r.UserAgent(), details)
	writeJSON(w, 200, map[string]any{"id": id, "roles": input.Roles})
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	outcome := strings.TrimSpace(r.URL.Query().Get("outcome"))
	if outcome == "" {
		outcome = strings.TrimSpace(r.URL.Query().Get("status"))
	}
	resourceType := strings.TrimSpace(r.URL.Query().Get("resourceType"))
	filter := `($1='' OR action ILIKE '%'||$1||'%' OR resource_type ILIKE '%'||$1||'%' OR resource_id ILIKE '%'||$1||'%' OR COALESCE(actor_id,'') ILIKE '%'||$1||'%' OR ip ILIKE '%'||$1||'%') AND ($2='' OR outcome=$2) AND ($3='' OR resource_type=$3)`
	var total int
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM audit_logs WHERE `+filter, search, outcome, resourceType).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not count audit events")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT id,actor_id,action,resource_type,resource_id,outcome,ip,user_agent,details,created_at FROM audit_logs WHERE `+filter+` ORDER BY created_at DESC LIMIT $4 OFFSET $5`, search, outcome, resourceType, limit, offset)
	if err != nil {
		writeError(w, 500, "database_error", "could not list audit events")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var actor *string
		var action, resourceType, resourceID, outcome, ip, userAgent string
		var details json.RawMessage
		var created time.Time
		if rows.Scan(&id, &actor, &action, &resourceType, &resourceID, &outcome, &ip, &userAgent, &details, &created) != nil {
			writeError(w, 500, "database_error", "could not list audit events")
			return
		}
		items = append(items, map[string]any{"id": id, "actor_id": actor, "action": action, "resource_type": resourceType, "resource_id": resourceID, "outcome": outcome, "ip": ip, "user_agent": userAgent, "details": details, "created_at": created})
	}
	writeJSON(w, 200, page(items, total, limit, offset))
}

func pagination(r *http.Request) (int, int) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

var _ = pgx.ErrNoRows
