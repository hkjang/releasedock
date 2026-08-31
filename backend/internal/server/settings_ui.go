package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type runnerSettingsResponse struct {
	PollIntervalMS      int       `json:"pollIntervalMs"`
	LockRetryMS         int       `json:"lockRetryMs"`
	SettingsRefreshMS   int       `json:"settingsRefreshMs"`
	HeartbeatIntervalMS int       `json:"heartbeatIntervalMs"`
	StaleJobAfterMS     int       `json:"staleJobAfterMs"`
	LogChunkBytes       int       `json:"logChunkBytes"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

type runnerSettingsInput struct {
	PollIntervalMS      *int `json:"pollIntervalMs"`
	LockRetryMS         *int `json:"lockRetryMs"`
	SettingsRefreshMS   *int `json:"settingsRefreshMs"`
	HeartbeatIntervalMS *int `json:"heartbeatIntervalMs"`
	StaleJobAfterMS     *int `json:"staleJobAfterMs"`
	LogChunkBytes       *int `json:"logChunkBytes"`
}

func validateRunnerSettings(settings runnerSettingsResponse) error {
	ranges := []struct {
		name       string
		value, min int
		max        int
	}{
		{"pollIntervalMs", settings.PollIntervalMS, 100, 60_000},
		{"lockRetryMs", settings.LockRetryMS, 100, 300_000},
		{"settingsRefreshMs", settings.SettingsRefreshMS, 1_000, 3_600_000},
		{"heartbeatIntervalMs", settings.HeartbeatIntervalMS, 1_000, 300_000},
		{"staleJobAfterMs", settings.StaleJobAfterMS, 5_000, 3_600_000},
		{"logChunkBytes", settings.LogChunkBytes, 1_024, 1_048_576},
	}
	for _, value := range ranges {
		if value.value < value.min || value.value > value.max {
			return fmt.Errorf("%s must be between %d and %d", value.name, value.min, value.max)
		}
	}
	if settings.StaleJobAfterMS <= 2*settings.HeartbeatIntervalMS {
		return errors.New("staleJobAfterMs must be greater than twice heartbeatIntervalMs")
	}
	return nil
}

func applyRunnerSettingsInput(current runnerSettingsResponse, input runnerSettingsInput) runnerSettingsResponse {
	if input.PollIntervalMS != nil {
		current.PollIntervalMS = *input.PollIntervalMS
	}
	if input.LockRetryMS != nil {
		current.LockRetryMS = *input.LockRetryMS
	}
	if input.SettingsRefreshMS != nil {
		current.SettingsRefreshMS = *input.SettingsRefreshMS
	}
	if input.HeartbeatIntervalMS != nil {
		current.HeartbeatIntervalMS = *input.HeartbeatIntervalMS
	}
	if input.StaleJobAfterMS != nil {
		current.StaleJobAfterMS = *input.StaleJobAfterMS
	}
	if input.LogChunkBytes != nil {
		current.LogChunkBytes = *input.LogChunkBytes
	}
	return current
}

func scanRunnerSettings(row pgx.Row, settings *runnerSettingsResponse) error {
	return row.Scan(
		&settings.PollIntervalMS,
		&settings.LockRetryMS,
		&settings.SettingsRefreshMS,
		&settings.HeartbeatIntervalMS,
		&settings.StaleJobAfterMS,
		&settings.LogChunkBytes,
		&settings.UpdatedAt,
	)
}

func (s *Server) getRunnerSettings(w http.ResponseWriter, r *http.Request) {
	var settings runnerSettingsResponse
	err := scanRunnerSettings(s.store.Pool.QueryRow(r.Context(), `
		SELECT poll_interval_ms,lock_retry_ms,settings_refresh_ms,heartbeat_interval_ms,
		       stale_job_after_ms,log_chunk_bytes,updated_at
		FROM runner_settings WHERE singleton=TRUE`), &settings)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load Runner settings")
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) putRunnerSettings(w http.ResponseWriter, r *http.Request) {
	var input runnerSettingsInput
	if !decodeJSON(w, r, &input) {
		return
	}
	p, _ := principalFrom(r)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not update Runner settings")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck

	var current runnerSettingsResponse
	err = scanRunnerSettings(tx.QueryRow(r.Context(), `
		SELECT poll_interval_ms,lock_retry_ms,settings_refresh_ms,heartbeat_interval_ms,
		       stale_job_after_ms,log_chunk_bytes,updated_at
		FROM runner_settings WHERE singleton=TRUE FOR UPDATE`), &current)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load Runner settings")
		return
	}
	updated := applyRunnerSettingsInput(current, input)
	if err = validateRunnerSettings(updated); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_runner_settings", err.Error())
		return
	}
	err = tx.QueryRow(r.Context(), `
		UPDATE runner_settings SET
			poll_interval_ms=$1,
			lock_retry_ms=$2,
			settings_refresh_ms=$3,
			heartbeat_interval_ms=$4,
			stale_job_after_ms=$5,
			log_chunk_bytes=$6,
			updated_at=clock_timestamp()
		WHERE singleton=TRUE
		RETURNING updated_at`, updated.PollIntervalMS, updated.LockRetryMS, updated.SettingsRefreshMS,
		updated.HeartbeatIntervalMS, updated.StaleJobAfterMS, updated.LogChunkBytes).Scan(&updated.UpdatedAt)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not update Runner settings")
		return
	}
	details, _ := json.Marshal(updated)
	s.store.Audit(r.Context(), p.UserID, "settings.runner.update", "settings", "runner", "success", remoteIP(r), r.UserAgent(), details)
	writeJSON(w, http.StatusOK, updated)
}

func settingString(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}
func settingBool(values map[string]any, key string, fallback bool) bool {
	value, ok := values[key].(bool)
	if !ok {
		return fallback
	}
	return value
}
func settingInt(values map[string]any, key string, fallback int) (int, error) {
	value, ok := values[key]
	if !ok {
		return fallback, nil
	}
	number, ok := value.(float64)
	if !ok || number != float64(int(number)) {
		return 0, errors.New(key + " must be an integer")
	}
	return int(number), nil
}

func settingStrings(values map[string]any, key string) ([]string, error) {
	raw, ok := values[key]
	if !ok || raw == nil {
		return []string{}, nil
	}
	var items []string
	switch value := raw.(type) {
	case string:
		items = strings.FieldsFunc(value, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' })
	case []any:
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				return nil, errors.New(key + " must contain only strings")
			}
			items = append(items, text)
		}
	default:
		return nil, errors.New(key + " must be a string array or newline-separated string")
	}
	normalized := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		origin, err := normalizeOrigin(item, false)
		if err != nil {
			return nil, errors.New(key + " contains an invalid origin")
		}
		if seen[origin] {
			return nil, errors.New(key + " must not contain duplicates")
		}
		seen[origin] = true
		normalized = append(normalized, origin)
	}
	return normalized, nil
}

func decodeSettingsMap(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var values map[string]any
	if !decodeJSON(w, r, &values) {
		return nil, false
	}
	return values, true
}

func (s *Server) putGeneralSettings(w http.ResponseWriter, r *http.Request) {
	values, ok := decodeSettingsMap(w, r)
	if !ok {
		return
	}
	name := settingString(values, "serviceName")
	if name == "" || len(name) > 100 {
		writeError(w, 400, "invalid_settings", "serviceName is required and must not exceed 100 characters")
		return
	}
	sizeGB, err := settingInt(values, "artifactMaxSizeGb", 20)
	if err != nil || sizeGB < 1 || sizeGB > 1024 {
		writeError(w, 400, "invalid_settings", "artifactMaxSizeGb must be between 1 and 1024")
		return
	}
	publicURL := settingString(values, "publicUrl")
	if publicURL != "" {
		publicURL, err = normalizeOrigin(publicURL, true)
		if err != nil {
			writeError(w, 400, "invalid_settings", "publicUrl must be an HTTPS origin without path, query, fragment, or userinfo")
			return
		}
	}
	allowedOrigins, err := settingStrings(values, "allowedOrigins")
	if err != nil {
		writeError(w, 400, "invalid_settings", err.Error())
		return
	}
	values["publicUrl"] = publicURL
	values["secureCookies"] = settingBool(values, "secureCookies", false)
	values["allowedOrigins"] = allowedOrigins
	encoded, _ := json.Marshal(values)
	p, _ := principalFrom(r)
	_, err = s.store.Pool.Exec(r.Context(), `UPDATE app_settings SET service_name=$1,artifact_max_bytes=$2,allowed_origins=$3,general_config=$4,updated_by=$5,updated_at=now() WHERE id='default'`, name, int64(sizeGB)<<30, allowedOrigins, encoded, p.UserID)
	if err != nil {
		writeError(w, 500, "database_error", "could not update general settings")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "settings.general.update", "settings", "general", "success", remoteIP(r), r.UserAgent(), nil)
	s.getGeneralSettings(w, r)
}

func (s *Server) putApprovalSettings(w http.ResponseWriter, r *http.Request) {
	values, ok := decodeSettingsMap(w, r)
	if !ok {
		return
	}
	enabled := settingBool(values, "enabled", false)
	minimum, err := settingInt(values, "minimumApprovers", 1)
	if err != nil || minimum != 1 {
		writeError(w, 400, "invalid_settings", "this version supports exactly one approver")
		return
	}
	encoded, _ := json.Marshal(values)
	p, _ := principalFrom(r)
	_, err = s.store.Pool.Exec(r.Context(), `UPDATE app_settings SET approval_enabled=$1,approval_config=$2,updated_by=$3,updated_at=now() WHERE id='default'`, enabled, encoded, p.UserID)
	if err != nil {
		writeError(w, 500, "database_error", "could not update approval settings")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "settings.approval.update", "settings", "approval", "success", remoteIP(r), r.UserAgent(), nil)
	s.getApprovalSettings(w, r)
}

func (s *Server) putStorageSettings(w http.ResponseWriter, r *http.Request) {
	values, ok := decodeSettingsMap(w, r)
	if !ok {
		return
	}
	driver := settingString(values, "driver")
	if driver == "" {
		driver = "local"
	}
	if driver != "local" {
		writeError(w, 400, "unsupported_storage", "this release supports local or mounted NFS storage through the local driver")
		return
	}
	path, pathErr := normalizeArtifactStoragePath(settingString(values, "localPath"))
	if pathErr != nil {
		writeError(w, 400, "invalid_settings", "localPath must be below /var/lib/releasedock and shared with the Runner")
		return
	}
	delete(values, "accessKey")
	delete(values, "secretKey")
	encoded, _ := json.Marshal(values)
	p, _ := principalFrom(r)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{})
	if err != nil {
		writeError(w, 500, "database_error", "could not update storage settings")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err = ensureArtifactStorageChange(r.Context(), tx, path); err != nil {
		if errors.Is(err, errArtifactStorageInUse) {
			writeError(w, http.StatusConflict, "artifact_storage_in_use", err.Error())
		} else {
			writeError(w, 500, "database_error", "could not validate artifact storage")
		}
		return
	}
	_, err = tx.Exec(r.Context(), `UPDATE app_settings SET artifact_storage_path=$1,storage_config=$2,updated_by=$3,updated_at=now() WHERE id='default'`, path, encoded, p.UserID)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not update storage settings")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "settings.storage.update", "settings", "storage", "success", remoteIP(r), r.UserAgent(), nil)
	s.getStorageSettings(w, r)
}

func (s *Server) putOIDCSettings(w http.ResponseWriter, r *http.Request) {
	values, ok := decodeSettingsMap(w, r)
	if !ok {
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
	enabled := settingBool(values, "enabled", current.Enabled)
	issuer := strings.TrimRight(settingString(values, "issuerUrl"), "/")
	clientID := settingString(values, "clientId")
	redirectURL := settingString(values, "redirectUrl")
	// Blank is fully supported: oidcLogin derives the redirect URI from the
	// public URL, or from the incoming request when that is unset too, and
	// pins the exact value to the login state so the token exchange matches.
	scopes := []string{"openid", "profile", "email"}
	if raw := values["scopes"]; raw != nil {
		switch typed := raw.(type) {
		case string:
			scopes = strings.Fields(typed)
		case []any:
			scopes = nil
			for _, v := range typed {
				if text, ok := v.(string); ok {
					scopes = append(scopes, text)
				}
			}
		}
	}
	autoCreate := settingBool(values, "autoProvision", false)
	if verify, exists := values["verifyTls"].(bool); exists && !verify && enabled {
		writeError(w, 400, "invalid_oidc_settings", "TLS verification cannot be disabled; install the internal CA in the service trust store")
		return
	}
	roleName := settingString(values, "defaultRole")
	if roleName == "" {
		roleName = "viewer"
	}
	var roleID *string
	var found string
	if err := tx.QueryRow(r.Context(), `SELECT id FROM roles WHERE lower(name)=lower($1) OR id=$2`, roleName, "role-"+strings.ToLower(roleName)).Scan(&found); err == nil {
		roleID = &found
	} else if autoCreate {
		writeError(w, 400, "invalid_oidc_settings", "defaultRole does not exist")
		return
	}
	secretEnc := current.ClientSecretEnc
	if secret := settingString(values, "clientSecret"); secret != "" {
		secretEnc, err = s.vault.Encrypt(secret, "oidc.client_secret")
		if err != nil {
			writeError(w, 500, "secret_error", "could not encrypt OIDC secret")
			return
		}
	}
	if enabled {
		if issuer == "" || clientID == "" || secretEnc == "" || !contains(scopes, "openid") {
			writeError(w, 400, "invalid_oidc_settings", "enabled OIDC requires issuerUrl, clientId, clientSecret, and openid scope")
			return
		}
		// Only an explicitly supplied redirect URI is format-checked; a blank
		// value is resolved per request at login time.
		if redirectURL != "" {
			redirect, parseErr := url.Parse(redirectURL)
			if parseErr != nil || redirect.Scheme != "https" || redirect.Host == "" || redirect.User != nil || redirect.RawQuery != "" || redirect.Fragment != "" {
				writeError(w, 400, "invalid_oidc_settings", "redirectUrl must be an absolute HTTPS URL without query, fragment, or userinfo")
				return
			}
		}
		ctx, cancel := contextWithTimeout(r, 10*time.Second)
		_, err = s.discoverOIDC(ctx, issuer)
		cancel()
		if err != nil {
			writeError(w, 400, "oidc_discovery_failed", err.Error())
			return
		}
	}
	if roleID != nil {
		if err = validateDelegatedRoles(r.Context(), tx, p.UserID, []string{*roleID}); err != nil {
			writeError(w, http.StatusForbidden, "delegation_forbidden", err.Error())
			return
		}
	}
	delete(values, "clientSecret")
	encoded, _ := json.Marshal(values)
	_, err = tx.Exec(r.Context(), `UPDATE oidc_settings SET enabled=$1,issuer=$2,client_id=$3,client_secret_enc=$4,redirect_url=$5,scopes=$6,auto_create_user=$7,default_role_id=$8,config=$9,updated_by=$10,updated_at=now() WHERE id='default'`, enabled, issuer, clientID, secretEnc, redirectURL, scopes, autoCreate, roleID, encoded, p.UserID)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not update OIDC settings")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "settings.oidc.update", "settings", "oidc", "success", remoteIP(r), r.UserAgent(), nil)
	s.getOIDCSettings(w, r)
}

func (s *Server) putAISettings(w http.ResponseWriter, r *http.Request) {
	values, ok := decodeSettingsMap(w, r)
	if !ok {
		return
	}
	current, err := s.loadAI(r.Context())
	if err != nil {
		writeError(w, 500, "database_error", "could not load AI settings")
		return
	}
	enabled := settingBool(values, "enabled", current.Enabled)
	endpoint := strings.TrimRight(settingString(values, "baseUrl"), "/")
	model := settingString(values, "model")
	maxTokens, err := settingInt(values, "maxTokens", current.MaxTokens)
	if err != nil || maxTokens < 1 || maxTokens > absoluteMaxTokens {
		writeError(w, 400, "invalid_ai_settings", "maxTokens must be between 1 and 262144")
		return
	}
	parsed, parseErr := url.Parse(endpoint)
	if enabled && (parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || model == "") {
		writeError(w, 400, "invalid_ai_settings", "enabled AI requires a safe http(s) baseUrl and model")
		return
	}
	secretEnc := current.APIKeyEnc
	if secret := settingString(values, "apiKey"); secret != "" {
		secretEnc, err = s.vault.Encrypt(secret, "ai.api_key")
		if err != nil {
			writeError(w, 500, "secret_error", "could not encrypt AI API key")
			return
		}
	}
	delete(values, "apiKey")
	encoded, _ := json.Marshal(values)
	p, _ := principalFrom(r)
	_, err = s.store.Pool.Exec(r.Context(), `UPDATE ai_settings SET enabled=$1,endpoint=$2,model=$3,api_key_enc=$4,max_tokens=$5,config=$6,updated_by=$7,updated_at=now() WHERE id='default'`, enabled, endpoint, model, secretEnc, maxTokens, encoded, p.UserID)
	if err != nil {
		writeError(w, 500, "database_error", "could not update AI settings")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "settings.ai.update", "settings", "ai", "success", remoteIP(r), r.UserAgent(), nil)
	s.getAISettings(w, r)
}
