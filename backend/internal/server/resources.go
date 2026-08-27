package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/hkjang/releasedock/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

type applicationInput struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Active      *bool  `json:"active"`
}

func validateCode(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for i, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' && i > 0) {
			return false
		}
	}
	return true
}

func (s *Server) listApplications(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	search := "%" + strings.TrimSpace(r.URL.Query().Get("search")) + "%"
	activeOnly := strings.EqualFold(r.URL.Query().Get("activeOnly"), "true")
	var total int
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM applications WHERE (code ILIKE $1 OR name ILIKE $1) AND (NOT $2 OR active)`, search, activeOnly).Scan(&total); err != nil {
		writeError(w, 500, "database_error", "could not list applications")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT id::text,code,name,description,active,created_at,updated_at FROM applications WHERE (code ILIKE $1 OR name ILIKE $1) AND (NOT $2 OR active) ORDER BY name LIMIT $3 OFFSET $4`, search, activeOnly, limit, offset)
	if err != nil {
		writeError(w, 500, "database_error", "could not list applications")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		item, err := scanApplication(rows)
		if err != nil {
			writeError(w, 500, "database_error", "could not list applications")
			return
		}
		items = append(items, item)
	}
	writeJSON(w, 200, page(items, total, limit, offset))
}

type rowScanner interface{ Scan(...any) error }

func scanApplication(row rowScanner) (map[string]any, error) {
	var id, code, name, description string
	var active bool
	var created, updated time.Time
	if err := row.Scan(&id, &code, &name, &description, &active, &created, &updated); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "code": code, "name": name, "description": description, "active": active, "createdAt": created, "updatedAt": updated}, nil
}

func (s *Server) createApplication(w http.ResponseWriter, r *http.Request) {
	var input applicationInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Code = strings.ToLower(strings.TrimSpace(input.Code))
	input.Name = strings.TrimSpace(input.Name)
	if !validateCode(input.Code) || input.Name == "" || len(input.Name) > 200 {
		writeError(w, 400, "invalid_application", "code must use lowercase letters, digits, hyphen or underscore; name is required")
		return
	}
	id, _ := secure.NewID()
	p, _ := principalFrom(r)
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	_, err := s.store.Pool.Exec(r.Context(), `INSERT INTO applications(id,code,name,description,active,created_by) VALUES($1,$2,$3,$4,$5,$6)`, id, input.Code, input.Name, input.Description, active, p.UserID)
	if err != nil {
		writeError(w, 409, "application_conflict", "application code already exists")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "application.create", "application", id, "success", remoteIP(r), r.UserAgent(), nil)
	s.getApplicationByID(w, r, id, http.StatusCreated)
}

func (s *Server) getApplication(w http.ResponseWriter, r *http.Request) {
	s.getApplicationByID(w, r, r.PathValue("id"), 200)
}
func (s *Server) getApplicationByID(w http.ResponseWriter, r *http.Request, id string, status int) {
	row := s.store.Pool.QueryRow(r.Context(), `SELECT id::text,code,name,description,active,created_at,updated_at FROM applications WHERE id=$1`, id)
	item, err := scanApplication(row)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "application not found")
		return
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not load application")
		return
	}
	writeJSON(w, status, item)
}

func (s *Server) updateApplication(w http.ResponseWriter, r *http.Request) {
	var input applicationInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Code = strings.ToLower(strings.TrimSpace(input.Code))
	input.Name = strings.TrimSpace(input.Name)
	if !validateCode(input.Code) || input.Name == "" {
		writeError(w, 400, "invalid_application", "valid code and name are required")
		return
	}
	id := r.PathValue("id")
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE applications SET code=$2,name=$3,description=$4,active=COALESCE($5,active),updated_at=now() WHERE id=$1`, id, input.Code, input.Name, input.Description, input.Active)
	if err != nil {
		writeError(w, 409, "application_conflict", "application code already exists")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 404, "not_found", "application not found")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "application.update", "application", id, "success", remoteIP(r), r.UserAgent(), nil)
	s.getApplicationByID(w, r, id, 200)
}

func (s *Server) deleteApplication(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tag, err := s.store.Pool.Exec(r.Context(), `DELETE FROM applications WHERE id=$1`, id)
	if err != nil {
		writeError(w, 409, "application_in_use", "application has release history and cannot be deleted")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 404, "not_found", "application not found")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "application.delete", "application", id, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(204)
}

type environmentInput struct {
	ApplicationID string `json:"applicationId"`
	Code          string `json:"code"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Protected     *bool  `json:"protected"`
	Description   string `json:"description"`
	Active        *bool  `json:"active"`
}

func scanEnvironment(row rowScanner) (map[string]any, error) {
	var id, appID, code, name, kind, description string
	var protected, active bool
	var created, updated time.Time
	if err := row.Scan(&id, &appID, &code, &name, &kind, &description, &protected, &active, &created, &updated); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "applicationId": appID, "code": code, "kind": kind, "name": name, "description": description, "protected": protected, "active": active, "createdAt": created, "updatedAt": updated}, nil
}
func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request) {
	s.queryEnvironments(w, r, r.PathValue("id"))
}
func (s *Server) listAllEnvironments(w http.ResponseWriter, r *http.Request) {
	s.queryEnvironments(w, r, "")
}
func (s *Server) queryEnvironments(w http.ResponseWriter, r *http.Request, appID string) {
	limit, offset := pagination(r)
	activeOnly := strings.EqualFold(r.URL.Query().Get("activeOnly"), "true")
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	if activeOnly && status == "" {
		status = "active"
	}
	if status != "" && status != "active" && status != "inactive" {
		writeError(w, http.StatusBadRequest, "invalid_filter", "status must be active or inactive")
		return
	}
	var total int
	err := s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM environments WHERE ($1='' OR application_id=$1::uuid) AND ($2='' OR code ILIKE '%'||$2||'%' OR name ILIKE '%'||$2||'%') AND ($3='' OR ($3='active' AND active) OR ($3='inactive' AND NOT active))`, appID, search, status).Scan(&total)
	if err != nil {
		writeError(w, 400, "invalid_application", "invalid application id")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT id::text,application_id::text,code,name,kind,description,protected,active,created_at,updated_at FROM environments WHERE ($1='' OR application_id=$1::uuid) AND ($2='' OR code ILIKE '%'||$2||'%' OR name ILIKE '%'||$2||'%') AND ($3='' OR ($3='active' AND active) OR ($3='inactive' AND NOT active)) ORDER BY application_id,name LIMIT $4 OFFSET $5`, appID, search, status, limit, offset)
	if err != nil {
		writeError(w, 500, "database_error", "could not list environments")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		item, err := scanEnvironment(rows)
		if err != nil {
			writeError(w, 500, "database_error", "could not list environments")
			return
		}
		items = append(items, item)
	}
	writeJSON(w, 200, page(items, total, limit, offset))
}
func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	var input environmentInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Code = strings.ToLower(strings.TrimSpace(input.Code))
	input.Name = strings.TrimSpace(input.Name)
	input.Kind = strings.ToUpper(strings.TrimSpace(input.Kind))
	if input.Kind == "" {
		input.Kind = strings.ToUpper(input.Code)
	}
	if !validateCode(input.Code) || input.Name == "" {
		writeError(w, 400, "invalid_environment", "valid code and name are required")
		return
	}
	id, _ := secure.NewID()
	p, _ := principalFrom(r)
	applicationID := r.PathValue("id")
	if applicationID == "" {
		applicationID = input.ApplicationID
	}
	if applicationID == "" {
		_ = s.store.Pool.QueryRow(r.Context(), `SELECT id::text FROM applications WHERE active=TRUE ORDER BY created_at LIMIT 1`).Scan(&applicationID)
	}
	if applicationID == "" {
		writeError(w, 400, "application_required", "applicationId is required")
		return
	}
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	protected := false
	if input.Protected != nil {
		protected = *input.Protected
	}
	_, err := s.store.Pool.Exec(r.Context(), `INSERT INTO environments(id,application_id,code,name,kind,description,protected,active,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, applicationID, input.Code, input.Name, input.Kind, input.Description, protected, active, p.UserID)
	if err != nil {
		writeError(w, 409, "environment_conflict", "application is invalid or environment code exists")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "environment.create", "environment", id, "success", remoteIP(r), r.UserAgent(), nil)
	s.getEnvironmentByID(w, r, id, 201)
}
func (s *Server) getEnvironmentByID(w http.ResponseWriter, r *http.Request, id string, status int) {
	item, err := scanEnvironment(s.store.Pool.QueryRow(r.Context(), `SELECT id::text,application_id::text,code,name,kind,description,protected,active,created_at,updated_at FROM environments WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "environment not found")
		return
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not load environment")
		return
	}
	writeJSON(w, status, item)
}
func (s *Server) updateEnvironment(w http.ResponseWriter, r *http.Request) {
	var input environmentInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Code = strings.ToLower(strings.TrimSpace(input.Code))
	input.Name = strings.TrimSpace(input.Name)
	input.Kind = strings.ToUpper(strings.TrimSpace(input.Kind))
	if input.Kind == "" {
		input.Kind = strings.ToUpper(input.Code)
	}
	if !validateCode(input.Code) || input.Name == "" {
		writeError(w, 400, "invalid_environment", "valid code and name are required")
		return
	}
	id := r.PathValue("id")
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE environments SET code=$2,name=$3,kind=$4,description=$5,protected=COALESCE($6,protected),active=COALESCE($7,active),updated_at=now() WHERE id=$1`, id, input.Code, input.Name, input.Kind, input.Description, input.Protected, input.Active)
	if err != nil {
		writeError(w, 409, "environment_conflict", "environment code already exists")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 404, "not_found", "environment not found")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "environment.update", "environment", id, "success", remoteIP(r), r.UserAgent(), nil)
	s.getEnvironmentByID(w, r, id, 200)
}
func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tag, err := s.store.Pool.Exec(r.Context(), `DELETE FROM environments WHERE id=$1`, id)
	if err != nil {
		writeError(w, 409, "environment_in_use", "environment is in use")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 404, "not_found", "environment not found")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "environment.delete", "environment", id, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(204)
}

type profileInput struct {
	ApplicationID         string          `json:"applicationId"`
	EnvironmentID         string          `json:"environmentId"`
	Name                  string          `json:"name"`
	Description           string          `json:"description"`
	ApprovalRequired      *bool           `json:"approvalRequired"`
	Active                *bool           `json:"active"`
	Config                json.RawMessage `json:"config"`
	RegistryID            string          `json:"registryId"`
	TargetCredentialID    json.RawMessage `json:"targetCredentialId"`
	PreScriptID           string          `json:"preScriptId"`
	DeployScriptID        string          `json:"deployScriptId"`
	HealthScriptID        string          `json:"healthScriptId"`
	RollbackScriptID      string          `json:"rollbackScriptId"`
	MaxArchiveBytes       *int64          `json:"maxArchiveBytes"`
	MaxExtractedBytes     *int64          `json:"maxExtractedBytes"`
	MaxArchiveFiles       *int            `json:"maxArchiveFiles"`
	MaxImages             *int            `json:"maxImages"`
	AllowSymlinks         *bool           `json:"allowSymlinks"`
	RuntimeKind           *string         `json:"runtimeKind"`
	RuntimeBinaryPath     *string         `json:"runtimeBinaryPath"`
	ContainerdNamespace   *string         `json:"containerdNamespace"`
	RegistryURL           *string         `json:"registryUrl"`
	RegistryHost          *string         `json:"registryHost"`
	RegistryProject       *string         `json:"registryProject"`
	RegistryInsecure      *bool           `json:"registryInsecure"`
	RegistryCAPEM         *string         `json:"registryCaPem"`
	HealthChecks          json.RawMessage `json:"healthChecks"`
	CommandTimeoutSeconds *int            `json:"timeoutSeconds"`
	MaxLogBytes           *int64          `json:"maxLogBytes"`
	AutoRollback          *bool           `json:"autoRollback"`
	CleanupWorkspace      *bool           `json:"cleanupWorkspace"`
	KeepFailedWorkspace   *bool           `json:"keepFailedWorkspace"`
	RunnerLabels          json.RawMessage `json:"runnerLabels"`
}

func profileRunnerLabels(raw json.RawMessage) ([]string, error) {
	labels, err := parseLabels(raw)
	if err != nil {
		return nil, err
	}
	if len(labels) > 20 {
		return nil, errors.New("runnerLabels must not contain more than 20 labels")
	}
	seen := map[string]bool{}
	for index, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" || len(label) > 64 || strings.ContainsAny(label, "\x00\r\n") {
			return nil, errors.New("runnerLabels entries must contain 1 to 64 characters")
		}
		if seen[label] {
			return nil, errors.New("runnerLabels must not contain duplicates")
		}
		seen[label] = true
		labels[index] = label
	}
	return labels, nil
}

func profileTargetCredential(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	if string(raw) == "null" {
		return "", true, nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", true, errors.New("targetCredentialId must be a UUID string, empty string, or null")
	}
	id = strings.TrimSpace(id)
	if id != "" && !validUUID(id) {
		return "", true, errors.New("targetCredentialId must be a UUID string, empty string, or null")
	}
	return id, true, nil
}

func canManageTargetCredential(principal store.Principal) bool {
	return !principal.ViaAPIKey && principal.Has("admin.credentials.write")
}

func validRuntimeBinaryPath(kind, binaryPath string) bool {
	wantedName := map[string]string{"docker": "docker", "podman": "podman", "containerd": "ctr"}[kind]
	if wantedName == "" || !filepath.IsAbs(binaryPath) || filepath.Clean(binaryPath) != binaryPath || filepath.Base(binaryPath) != wantedName {
		return false
	}
	switch filepath.Dir(binaryPath) {
	case "/usr/bin", "/usr/local/bin", "/usr/sbin", "/usr/local/sbin":
		return true
	default:
		return false
	}
}

func validRuntimeBinaryForAnyKind(binaryPath string) bool {
	for _, kind := range []string{"docker", "podman", "containerd"} {
		if validRuntimeBinaryPath(kind, binaryPath) {
			return true
		}
	}
	return false
}

func validateProfile(input profileInput) error {
	if strings.TrimSpace(input.Name) == "" || len(input.Name) > 200 {
		return errors.New("name is required")
	}
	if input.ApplicationID == "" || input.EnvironmentID == "" {
		return errors.New("applicationId and environmentId are required")
	}
	if len(input.Config) > 0 {
		var object map[string]any
		if json.Unmarshal(input.Config, &object) != nil {
			return errors.New("config must be a JSON object")
		}
	}
	if len(input.HealthChecks) > 0 {
		var checks []map[string]any
		if json.Unmarshal(input.HealthChecks, &checks) != nil {
			return errors.New("healthChecks must be a JSON array")
		}
	}
	for _, v := range []*int64{input.MaxArchiveBytes, input.MaxExtractedBytes, input.MaxLogBytes} {
		if v != nil && *v <= 0 {
			return errors.New("size limits must be positive")
		}
	}
	if input.MaxArchiveFiles != nil && *input.MaxArchiveFiles <= 0 || input.MaxImages != nil && *input.MaxImages <= 0 || input.CommandTimeoutSeconds != nil && *input.CommandTimeoutSeconds <= 0 {
		return errors.New("numeric limits must be positive")
	}
	if input.RuntimeKind != nil && *input.RuntimeKind != "docker" && *input.RuntimeKind != "podman" && *input.RuntimeKind != "containerd" {
		return errors.New("runtimeKind must be docker, podman, or containerd")
	}
	if input.RuntimeBinaryPath != nil {
		valid := validRuntimeBinaryForAnyKind(*input.RuntimeBinaryPath)
		if input.RuntimeKind != nil {
			valid = validRuntimeBinaryPath(*input.RuntimeKind, *input.RuntimeBinaryPath)
		}
		if !valid {
			return errors.New("runtimeBinaryPath must be the matching docker, podman, or ctr binary in an approved system directory")
		}
	}
	if input.AutoRollback != nil && *input.AutoRollback {
		return errors.New("autoRollback is not supported; use an explicit audited rollback request")
	}
	if _, _, err := profileTargetCredential(input.TargetCredentialID); err != nil {
		return err
	}
	if _, err := profileRunnerLabels(input.RunnerLabels); err != nil {
		return err
	}
	return nil
}

func scanProfile(row rowScanner) (map[string]any, error) {
	var id, appID, envID, name, description, runtimeKind, runtimePath, namespace, registryURL, registryHost, registryProject string
	var approval, active, enabled, allowSymlinks, registryInsecure, autoRollback, cleanup, keepFailed bool
	var config, health json.RawMessage
	var maxArchive, maxExtracted, maxLog int64
	var maxFiles, maxImages, timeout int
	var created, updated time.Time
	var registryCAPEM string
	var runnerLabels []string
	var registryID, targetCredentialID, preScriptID, deployScriptID, healthScriptID, rollbackScriptID *string
	if err := row.Scan(&id, &appID, &envID, &name, &description, &approval, &config, &active, &maxArchive, &maxExtracted, &maxFiles, &maxImages, &allowSymlinks, &runtimeKind, &runtimePath, &namespace, &registryURL, &registryHost, &registryProject, &registryInsecure, &registryCAPEM, &health, &timeout, &maxLog, &autoRollback, &cleanup, &keepFailed, &enabled, &runnerLabels, &registryID, &targetCredentialID, &preScriptID, &deployScriptID, &healthScriptID, &rollbackScriptID, &created, &updated); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "applicationId": appID, "environmentId": envID, "name": name, "description": description, "approvalRequired": approval, "config": config, "active": active && enabled, "maxArchiveBytes": maxArchive, "maxExtractedBytes": maxExtracted, "maxArchiveFiles": maxFiles, "maxImages": maxImages, "allowSymlinks": allowSymlinks, "runtimeKind": runtimeKind, "runtimeBinaryPath": runtimePath, "containerdNamespace": namespace, "registryUrl": registryURL, "registryHost": registryHost, "registryProject": registryProject, "registryInsecure": registryInsecure, "registryCaPem": registryCAPEM, "registryId": registryID, "targetCredentialId": targetCredentialID, "preScriptId": preScriptID, "deployScriptId": deployScriptID, "healthScriptId": healthScriptID, "rollbackScriptId": rollbackScriptID, "runnerLabels": runnerLabels, "healthChecks": health, "timeoutSeconds": timeout, "maxLogBytes": maxLog, "autoRollback": autoRollback, "cleanupWorkspace": cleanup, "keepFailedWorkspace": keepFailed, "createdAt": created, "updatedAt": updated}, nil
}

const profileSelect = `SELECT id::text,application_id::text,environment_id::text,name,description,approval_required,config,active,max_archive_bytes,max_extracted_bytes,max_archive_files,max_images,allow_symlinks,runtime_kind,runtime_binary_path,containerd_namespace,registry_url,registry_host,registry_project,registry_insecure,registry_ca_pem,health_checks,command_timeout_seconds,max_log_bytes,auto_rollback,cleanup_workspace,keep_failed_workspace,enabled,runner_labels,registry_credential_id::text,
	target_credential_id::text,
 (SELECT script_version_id::text FROM deployment_profile_scripts WHERE profile_id=deployment_profiles.id AND phase='PRE_DEPLOY' ORDER BY execution_order LIMIT 1),
 (SELECT script_version_id::text FROM deployment_profile_scripts WHERE profile_id=deployment_profiles.id AND phase='DEPLOY' ORDER BY execution_order LIMIT 1),
 (SELECT script_version_id::text FROM deployment_profile_scripts WHERE profile_id=deployment_profiles.id AND phase='POST_DEPLOY' ORDER BY execution_order LIMIT 1),
 (SELECT script_version_id::text FROM deployment_profile_scripts WHERE profile_id=deployment_profiles.id AND phase='ROLLBACK' ORDER BY execution_order LIMIT 1),created_at,updated_at FROM deployment_profiles`

func (s *Server) listProfiles(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	appID := r.URL.Query().Get("applicationId")
	envID := r.URL.Query().Get("environmentId")
	activeOnly := strings.EqualFold(r.URL.Query().Get("activeOnly"), "true")
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	if activeOnly && status == "" {
		status = "active"
	}
	if status != "" && status != "active" && status != "inactive" {
		writeError(w, http.StatusBadRequest, "invalid_filter", "status must be active or inactive")
		return
	}
	var total int
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM deployment_profiles WHERE revoked_at IS NULL AND ($1='' OR application_id=$1::uuid) AND ($2='' OR environment_id=$2::uuid) AND ($3='' OR name ILIKE '%'||$3||'%') AND ($4='' OR ($4='active' AND active AND enabled) OR ($4='inactive' AND (NOT active OR NOT enabled)))`, appID, envID, search, status).Scan(&total); err != nil {
		writeError(w, 400, "invalid_filter", "invalid profile filter")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), profileSelect+` WHERE revoked_at IS NULL AND ($1='' OR application_id=$1::uuid) AND ($2='' OR environment_id=$2::uuid) AND ($3='' OR name ILIKE '%'||$3||'%') AND ($4='' OR ($4='active' AND active AND enabled) OR ($4='inactive' AND (NOT active OR NOT enabled))) ORDER BY name LIMIT $5 OFFSET $6`, appID, envID, search, status, limit, offset)
	if err != nil {
		writeError(w, 500, "database_error", "could not list profiles")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		item, err := scanProfile(rows)
		if err != nil {
			writeError(w, 500, "database_error", "could not list profiles")
			return
		}
		items = append(items, item)
	}
	writeJSON(w, 200, page(items, total, limit, offset))
}
func (s *Server) createProfile(w http.ResponseWriter, r *http.Request) {
	var input profileInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := validateProfile(input); err != nil {
		writeError(w, 400, "invalid_profile", err.Error())
		return
	}
	p, _ := principalFrom(r)
	targetCredentialID, targetCredentialProvided, _ := profileTargetCredential(input.TargetCredentialID)
	if targetCredentialProvided && targetCredentialID != "" && !canManageTargetCredential(p) {
		writeError(w, http.StatusForbidden, "permission_denied", "a browser session with admin.credentials.write is required to bind a target credential")
		return
	}
	id, _ := secure.NewID()
	approval := false
	if input.ApprovalRequired != nil {
		approval = *input.ApprovalRequired
	}
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	config := input.Config
	if len(config) == 0 {
		config = []byte(`{}`)
	}
	runnerLabels, _ := profileRunnerLabels(input.RunnerLabels)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{})
	if err != nil {
		writeError(w, 500, "database_error", "could not create profile")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	tag, err := tx.Exec(r.Context(), `INSERT INTO deployment_profiles(id,application_id,environment_id,name,description,approval_required,config,active,enabled,runner_labels,created_by) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$8,$9,$10 WHERE EXISTS(SELECT 1 FROM environments WHERE id=$3 AND application_id=$2)`, id, input.ApplicationID, input.EnvironmentID, strings.TrimSpace(input.Name), input.Description, approval, config, active, runnerLabels, p.UserID)
	if err != nil {
		writeError(w, 409, "profile_conflict", "profile is invalid or already exists")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusBadRequest, "invalid_profile", "environment does not belong to application")
		return
	}
	if err := s.updateProfileRuntime(r.Context(), tx, id, input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_profile", "runtime profile settings could not be saved")
		return
	}
	if err := s.updateProfileLinks(r.Context(), tx, id, input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_profile", err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 409, "profile_conflict", "profile could not be committed")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "profile.create", "profile", id, "success", remoteIP(r), r.UserAgent(), nil)
	s.getProfileByID(w, r, id, 201)
}
func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	s.getProfileByID(w, r, r.PathValue("id"), 200)
}
func (s *Server) getProfileByID(w http.ResponseWriter, r *http.Request, id string, status int) {
	item, err := scanProfile(s.store.Pool.QueryRow(r.Context(), profileSelect+` WHERE id=$1 AND revoked_at IS NULL`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "profile not found")
		return
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not load profile")
		return
	}
	writeJSON(w, status, item)
}
func (s *Server) updateProfile(w http.ResponseWriter, r *http.Request) {
	var input profileInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := validateProfile(input); err != nil {
		writeError(w, 400, "invalid_profile", err.Error())
		return
	}
	id := r.PathValue("id")
	p, _ := principalFrom(r)
	config := input.Config
	if len(config) == 0 {
		config = nil
	}
	runnerLabels, _ := profileRunnerLabels(input.RunnerLabels)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not update profile")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	var existingTargetCredentialID *string
	if err := tx.QueryRow(r.Context(), `SELECT target_credential_id::text FROM deployment_profiles WHERE id=$1 AND revoked_at IS NULL FOR UPDATE`, id).Scan(&existingTargetCredentialID); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "profile not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load profile credential binding")
		return
	}
	_, targetCredentialProvided, _ := profileTargetCredential(input.TargetCredentialID)
	if targetCredentialProvided && !canManageTargetCredential(p) {
		writeError(w, http.StatusForbidden, "permission_denied", "a browser session with admin.credentials.write is required to change a target credential binding")
		return
	}
	tag, err := tx.Exec(r.Context(), `UPDATE deployment_profiles SET application_id=$2,environment_id=$3,name=$4,description=$5,approval_required=COALESCE($6,approval_required),active=COALESCE($7,active),enabled=COALESCE($7,enabled),config=COALESCE($8,config),runner_labels=$9,updated_at=now() WHERE id=$1 AND revoked_at IS NULL AND EXISTS(SELECT 1 FROM environments WHERE id=$3 AND application_id=$2)`, id, input.ApplicationID, input.EnvironmentID, strings.TrimSpace(input.Name), input.Description, input.ApprovalRequired, input.Active, config, runnerLabels)
	if err != nil {
		writeError(w, 409, "profile_conflict", "profile is invalid or already exists")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 404, "not_found", "profile not found or environment does not belong to application")
		return
	}
	if err := s.updateProfileRuntime(r.Context(), tx, id, input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_profile", "runtime profile settings could not be saved")
		return
	}
	if err := s.updateProfileLinks(r.Context(), tx, id, input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_profile", err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 409, "profile_conflict", "profile could not be committed")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "profile.update", "profile", id, "success", remoteIP(r), r.UserAgent(), nil)
	s.getProfileByID(w, r, id, 200)
}

func (s *Server) updateProfileRuntime(ctx context.Context, tx pgx.Tx, id string, input profileInput) error {
	var currentKind, currentBinaryPath string
	if err := tx.QueryRow(ctx, `SELECT runtime_kind,runtime_binary_path FROM deployment_profiles WHERE id=$1 FOR UPDATE`, id).Scan(&currentKind, &currentBinaryPath); err != nil {
		return err
	}
	effectiveKind, effectiveBinaryPath := currentKind, currentBinaryPath
	if input.RuntimeKind != nil {
		effectiveKind = *input.RuntimeKind
	}
	if input.RuntimeBinaryPath != nil {
		effectiveBinaryPath = *input.RuntimeBinaryPath
	}
	if !validRuntimeBinaryPath(effectiveKind, effectiveBinaryPath) {
		return errors.New("runtimeBinaryPath does not match runtimeKind or an approved system binary path")
	}
	health := input.HealthChecks
	if len(health) == 0 {
		health = nil
	}
	_, err := tx.Exec(ctx, `UPDATE deployment_profiles SET
		max_archive_bytes=COALESCE($2,max_archive_bytes),max_extracted_bytes=COALESCE($3,max_extracted_bytes),
		max_archive_files=COALESCE($4,max_archive_files),max_images=COALESCE($5,max_images),allow_symlinks=COALESCE($6,allow_symlinks),
		runtime_kind=COALESCE($7,runtime_kind),runtime_binary_path=COALESCE($8,runtime_binary_path),containerd_namespace=COALESCE($9,containerd_namespace),
		registry_url=COALESCE($10,registry_url),registry_host=COALESCE($11,registry_host),registry_project=COALESCE($12,registry_project),
		registry_insecure=COALESCE($13,registry_insecure),registry_ca_pem=COALESCE($14,registry_ca_pem),health_checks=COALESCE($15,health_checks),
		command_timeout_seconds=COALESCE($16,command_timeout_seconds),max_log_bytes=COALESCE($17,max_log_bytes),
		auto_rollback=COALESCE($18,auto_rollback),cleanup_workspace=COALESCE($19,cleanup_workspace),keep_failed_workspace=COALESCE($20,keep_failed_workspace),updated_at=now()
		WHERE id=$1`, id, input.MaxArchiveBytes, input.MaxExtractedBytes, input.MaxArchiveFiles, input.MaxImages, input.AllowSymlinks, input.RuntimeKind, input.RuntimeBinaryPath, input.ContainerdNamespace, input.RegistryURL, input.RegistryHost, input.RegistryProject, input.RegistryInsecure, input.RegistryCAPEM, health, input.CommandTimeoutSeconds, input.MaxLogBytes, input.AutoRollback, input.CleanupWorkspace, input.KeepFailedWorkspace)
	return err
}

func (s *Server) updateProfileLinks(ctx context.Context, tx pgx.Tx, id string, input profileInput) error {
	links := []struct{ ID, Phase, ScriptType string }{{input.PreScriptID, "PRE_DEPLOY", "PRE_CHECK"}, {input.DeployScriptID, "DEPLOY", "DEPLOY"}, {input.HealthScriptID, "POST_DEPLOY", "HEALTH_CHECK"}, {input.RollbackScriptID, "ROLLBACK", "ROLLBACK"}}
	if input.RegistryID != "" {
		var endpoint, project string
		var insecure bool
		if err := tx.QueryRow(ctx, `SELECT endpoint,project,insecure_skip_verify FROM runner_credentials WHERE id=$1 AND active=TRUE AND approved_at IS NOT NULL AND revoked_at IS NULL`, input.RegistryID).Scan(&endpoint, &project, &insecure); err != nil {
			return errors.New("registryId is invalid or revoked")
		}
		host := registryHost(endpoint)
		if host == "" {
			return errors.New("registry endpoint is invalid")
		}
		if _, err := tx.Exec(ctx, `UPDATE deployment_profiles SET registry_credential_id=$2,registry_url=$3,registry_host=$4,registry_project=$5,registry_insecure=$6 WHERE id=$1`, id, input.RegistryID, endpoint, host, project, insecure); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE deployment_profiles SET registry_credential_id=NULL,registry_url='',registry_host='',registry_project='',registry_insecure=FALSE WHERE id=$1`, id); err != nil {
			return err
		}
	}
	targetCredentialID, targetCredentialProvided, err := profileTargetCredential(input.TargetCredentialID)
	if err != nil {
		return err
	}
	if targetCredentialProvided && targetCredentialID != "" {
		tag, err := tx.Exec(ctx, `UPDATE deployment_profiles SET target_credential_id=$2 WHERE id=$1 AND EXISTS(SELECT 1 FROM target_credentials WHERE id=$2 AND active AND approved_at IS NOT NULL AND revoked_at IS NULL)`, id, targetCredentialID)
		if err != nil || tag.RowsAffected() == 0 {
			return errors.New("targetCredentialId is invalid, inactive, or revoked")
		}
	} else if targetCredentialProvided {
		if _, err := tx.Exec(ctx, `UPDATE deployment_profiles SET target_credential_id=NULL WHERE id=$1`, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM deployment_profile_scripts WHERE profile_id=$1`, id); err != nil {
		return err
	}
	order := 0
	for _, link := range links {
		if link.ID == "" {
			continue
		}
		order++
		tag, insertErr := tx.Exec(ctx, `INSERT INTO deployment_profile_scripts(profile_id,script_version_id,phase,execution_order) SELECT $1,id,$3,$4 FROM script_versions WHERE id=$2 AND script_type=$5 AND active=TRUE AND approved_at IS NOT NULL AND revoked_at IS NULL`, id, link.ID, link.Phase, order, link.ScriptType)
		if insertErr != nil || tag.RowsAffected() == 0 {
			return errors.New("one or more script IDs are invalid or not approved")
		}
	}
	return nil
}
func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE deployment_profiles SET active=FALSE,enabled=FALSE,revoked_at=now(),updated_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	if err != nil {
		writeError(w, 500, "database_error", "could not revoke profile")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 404, "not_found", "profile not found")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "profile.revoke", "profile", id, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(204)
}

func page(items any, total, limit, offset int) map[string]any {
	pageNumber := offset/limit + 1
	return map[string]any{"items": items, "total": total, "limit": limit, "offset": offset, "page": pageNumber, "pageSize": limit}
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	var total, active, pending, success, recentTotal int
	s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM releases`).Scan(&total)
	s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM releases WHERE status IN ('QUEUED','VALIDATING','PRE_CHECK','EXTRACTING','IMAGE_INSPECT','IMAGE_LOAD','IMAGE_TAG','IMAGE_PUSH','DEPLOYING','VERIFYING','ROLLBACK')`).Scan(&active)
	s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM releases WHERE status IN ('PENDING_REVIEW','UNDER_REVIEW')`).Scan(&pending)
	s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FILTER(WHERE status='SUCCESS'),count(*) FROM (SELECT status FROM releases ORDER BY created_at DESC LIMIT 100) x`).Scan(&success, &recentTotal)
	successRate := 0
	if recentTotal > 0 {
		successRate = success * 100 / recentTotal
	}
	items, err := s.releaseRows(r, 10, 0, "", "")
	if err != nil {
		writeError(w, 500, "database_error", "could not load dashboard")
		return
	}
	writeJSON(w, 200, map[string]any{"totalReleases": total, "activeDeployments": active, "pendingApprovals": pending, "successRate": successRate, "recentReleases": items})
}
