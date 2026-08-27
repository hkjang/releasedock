package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type scriptInput struct {
	Name            string          `json:"name"`
	Type            string          `json:"type"`
	Version         json.RawMessage `json:"version"`
	Content         string          `json:"content"`
	InterpreterPath string          `json:"interpreterPath"`
	TimeoutSeconds  int             `json:"timeoutSeconds"`
	Active          *bool           `json:"active"`
}

func scriptVersion(raw json.RawMessage) (int, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	var number int
	if json.Unmarshal(raw, &number) == nil {
		return number, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strconv.Atoi(text)
	}
	return 0, errors.New("version must be an integer")
}
func validateScript(input *scriptInput) (int, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Type = strings.ToUpper(strings.TrimSpace(input.Type))
	if input.Name == "" || len(input.Name) > 200 {
		return 0, errors.New("name is required")
	}
	allowed := map[string]bool{"PRE_CHECK": true, "DEPLOY": true, "HEALTH_CHECK": true, "ROLLBACK": true}
	if !allowed[input.Type] {
		return 0, errors.New("type must be PRE_CHECK, DEPLOY, HEALTH_CHECK, or ROLLBACK")
	}
	if input.Content == "" || len(input.Content) > 1<<20 {
		return 0, errors.New("content is required and must not exceed 1 MiB")
	}
	if input.InterpreterPath == "" {
		input.InterpreterPath = "/bin/bash"
	}
	if !filepath.IsAbs(input.InterpreterPath) {
		return 0, errors.New("interpreterPath must be absolute")
	}
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = 600
	}
	if input.TimeoutSeconds < 1 || input.TimeoutSeconds > 86400 {
		return 0, errors.New("timeoutSeconds must be between 1 and 86400")
	}
	version, err := scriptVersion(input.Version)
	if err != nil {
		return 0, err
	}
	return version, nil
}

func (s *Server) listScripts(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	scriptType := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("type")))
	if status != "" && status != "active" && status != "inactive" && status != "pending" && status != "revoked" {
		writeError(w, http.StatusBadRequest, "invalid_filter", "status must be active, inactive, pending, or revoked")
		return
	}
	filter := `($1='' OR name ILIKE '%'||$1||'%') AND ($2='' OR script_type=$2) AND
		($3='' OR ($3='active' AND active AND approved_at IS NOT NULL AND revoked_at IS NULL) OR ($3='inactive' AND NOT active AND revoked_at IS NULL) OR ($3='pending' AND approved_at IS NULL AND revoked_at IS NULL) OR ($3='revoked' AND revoked_at IS NOT NULL))`
	var total int
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM script_versions WHERE `+filter, search, scriptType, status).Scan(&total); err != nil {
		writeError(w, 500, "database_error", "could not count scripts")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT id::text,name,script_type,version,interpreter_path,sha256,timeout_seconds,active,approved_at,revoked_at,created_at FROM script_versions WHERE `+filter+` ORDER BY name,version DESC LIMIT $4 OFFSET $5`, search, scriptType, status, limit, offset)
	if err != nil {
		writeError(w, 500, "database_error", "could not list scripts")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, name, kind, interpreter, checksum string
		var version, timeout int
		var active bool
		var approved, revoked *time.Time
		var created time.Time
		if rows.Scan(&id, &name, &kind, &version, &interpreter, &checksum, &timeout, &active, &approved, &revoked, &created) != nil {
			writeError(w, 500, "database_error", "could not list scripts")
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "type": kind, "version": version, "interpreterPath": interpreter, "sha256": checksum, "timeoutSeconds": timeout, "active": active && revoked == nil, "approved": approved != nil, "createdAt": created})
	}
	writeJSON(w, 200, page(items, total, limit, offset))
}

func (s *Server) createScript(w http.ResponseWriter, r *http.Request) {
	var input scriptInput
	if !decodeJSON(w, r, &input) {
		return
	}
	version, err := validateScript(&input)
	if err != nil {
		writeError(w, 400, "invalid_script", err.Error())
		return
	}
	if version == 0 {
		_ = s.store.Pool.QueryRow(r.Context(), `SELECT COALESCE(max(version),0)+1 FROM script_versions WHERE name=$1`, input.Name).Scan(&version)
	}
	id, _ := secure.NewID()
	sum := sha256.Sum256([]byte(input.Content))
	p, _ := principalFrom(r)
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	_, err = s.store.Pool.Exec(r.Context(), `INSERT INTO script_versions(id,name,script_type,version,interpreter_path,content,sha256,timeout_seconds,active,approved_at,approved_by,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,CASE WHEN $9 THEN now() END,CASE WHEN $9 THEN $10 END,$10)`, id, input.Name, input.Type, version, input.InterpreterPath, input.Content, hex.EncodeToString(sum[:]), input.TimeoutSeconds, active, p.UserID)
	if err != nil {
		writeError(w, 409, "script_conflict", "script name and version already exist")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "script.create", "script", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 201, map[string]any{"id": id, "name": input.Name, "type": input.Type, "version": version, "sha256": hex.EncodeToString(sum[:]), "timeoutSeconds": input.TimeoutSeconds, "active": active, "approved": active})
}

func (s *Server) updateScript(w http.ResponseWriter, r *http.Request) {
	var input scriptInput
	if !decodeJSON(w, r, &input) {
		return
	}
	version, err := validateScript(&input)
	if err != nil {
		writeError(w, 400, "invalid_script", err.Error())
		return
	}
	oldID := r.PathValue("id")
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not version script")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	var oldName, oldType string
	var currentVersion int
	if err = tx.QueryRow(r.Context(), `SELECT name,script_type,version FROM script_versions WHERE id=$1 AND revoked_at IS NULL FOR UPDATE`, oldID).Scan(&oldName, &oldType, &currentVersion); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "script not found")
		return
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not load script")
		return
	}
	if input.Name != oldName || input.Type != oldType {
		writeError(w, 400, "invalid_script", "immutable script versions must retain their name and type")
		return
	}
	if version <= currentVersion {
		version = currentVersion + 1
	}
	newID, _ := secure.NewID()
	sum := sha256.Sum256([]byte(input.Content))
	p, _ := principalFrom(r)
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO script_versions(id,name,script_type,version,interpreter_path,content,sha256,timeout_seconds,active,approved_at,approved_by,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,CASE WHEN $9 THEN now() END,CASE WHEN $9 THEN $10 END,$10)`, newID, input.Name, input.Type, version, input.InterpreterPath, input.Content, hex.EncodeToString(sum[:]), input.TimeoutSeconds, active, p.UserID)
	if err == nil && active {
		err = promoteScriptVersion(r.Context(), tx, newID, p.UserID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 409, "script_conflict", "could not create immutable script version")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "script.version", "script", newID, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 200, map[string]any{"id": newID, "name": input.Name, "type": input.Type, "version": version, "sha256": hex.EncodeToString(sum[:]), "timeoutSeconds": input.TimeoutSeconds, "active": active, "approved": active})
}

// promoteScriptVersion approves one immutable version without changing profile
// bindings. Binding a newly approved version is an explicit profile update, so
// an approval cannot silently change a reviewed release's execution inputs.
func promoteScriptVersion(ctx context.Context, tx pgx.Tx, id, approverID string) error {
	tag, err := tx.Exec(ctx, `UPDATE script_versions SET approved_at=now(),approved_by=$2,active=TRUE WHERE id=$1 AND revoked_at IS NULL`, id, approverID)
	if err == nil && tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}

func (s *Server) approveScript(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	id := r.PathValue("id")
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not approve script")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	err = promoteScriptVersion(r.Context(), tx, id, p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "script not found")
		return
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 409, "script_in_use", "script cannot be promoted while an active job uses the current version")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "script.approve", "script", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 200, map[string]any{"id": id, "approved": true, "active": true})
}
func (s *Server) revokeScript(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	id := r.PathValue("id")
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE script_versions SET active=FALSE,revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	if err != nil || tag.RowsAffected() == 0 {
		writeError(w, 404, "not_found", "script not found")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "script.revoke", "script", id, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(204)
}

type registryInput struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Project  string `json:"project"`
	Username string `json:"username"`
	Password string `json:"password"`
	Insecure bool   `json:"insecureSkipVerify"`
	Active   *bool  `json:"active"`
}

func validateRegistry(input *registryInput) error {
	input.Name = strings.TrimSpace(input.Name)
	input.Endpoint = strings.TrimRight(strings.TrimSpace(input.Endpoint), "/")
	input.Project = strings.Trim(strings.TrimSpace(input.Project), "/")
	input.Username = strings.TrimSpace(input.Username)
	u, err := url.Parse(input.Endpoint)
	if input.Name == "" || input.Project == "" || input.Username == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("name, project, username and an absolute http(s) endpoint are required")
	}
	return nil
}
func registryAAD(id string, version int) string { return fmt.Sprintf("credential:%s:v%d", id, version) }
func (s *Server) listRegistries(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	if status != "" && status != "active" && status != "inactive" && status != "revoked" {
		writeError(w, http.StatusBadRequest, "invalid_filter", "status must be active, inactive, or revoked")
		return
	}
	filter := `($1='' OR name ILIKE '%'||$1||'%' OR endpoint ILIKE '%'||$1||'%' OR project ILIKE '%'||$1||'%') AND ($2='' OR ($2='active' AND active AND revoked_at IS NULL) OR ($2='inactive' AND NOT active AND revoked_at IS NULL) OR ($2='revoked' AND revoked_at IS NOT NULL))`
	var total int
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM runner_credentials WHERE `+filter, search, status).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not count registries")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT id::text,name,endpoint,project,username,insecure_skip_verify,active,version,ciphertext<>'',approved_at,revoked_at,created_at FROM runner_credentials WHERE `+filter+` ORDER BY name LIMIT $3 OFFSET $4`, search, status, limit, offset)
	if err != nil {
		writeError(w, 500, "database_error", "could not list registries")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, name, endpoint, project, username string
		var insecure, active, configured bool
		var version int
		var approved, revoked *time.Time
		var created time.Time
		if rows.Scan(&id, &name, &endpoint, &project, &username, &insecure, &active, &version, &configured, &approved, &revoked, &created) != nil {
			writeError(w, 500, "database_error", "could not list registries")
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "endpoint": endpoint, "project": project, "username": username, "insecureSkipVerify": insecure, "active": active && revoked == nil, "version": version, "secretConfigured": configured, "approved": approved != nil, "createdAt": created})
	}
	writeJSON(w, 200, page(items, total, limit, offset))
}
func (s *Server) createRegistry(w http.ResponseWriter, r *http.Request) {
	var input registryInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := validateRegistry(&input); err != nil || input.Password == "" {
		if err == nil {
			err = errors.New("password is required")
		}
		writeError(w, 400, "invalid_registry", err.Error())
		return
	}
	id, _ := secure.NewID()
	version := 1
	plaintext, _ := json.Marshal(map[string]string{"username": input.Username, "password": input.Password})
	ciphertext, err := s.vault.Encrypt(string(plaintext), registryAAD(id, version))
	if err != nil {
		writeError(w, 500, "secret_error", "could not encrypt registry credential")
		return
	}
	p, _ := principalFrom(r)
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	_, err = s.store.Pool.Exec(r.Context(), `INSERT INTO runner_credentials(id,name,endpoint,project,username,insecure_skip_verify,active,version,ciphertext,approved_at,approved_by,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,now(),$10,$10)`, id, input.Name, input.Endpoint, input.Project, input.Username, input.Insecure, active, version, ciphertext, p.UserID)
	if err != nil {
		writeError(w, 409, "registry_conflict", "registry name already exists")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "registry.create", "registry", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 201, map[string]any{"id": id, "name": input.Name, "endpoint": input.Endpoint, "project": input.Project, "username": input.Username, "insecureSkipVerify": input.Insecure, "active": active, "version": version, "secretConfigured": true})
}
func (s *Server) updateRegistry(w http.ResponseWriter, r *http.Request) {
	var input registryInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := validateRegistry(&input); err != nil {
		writeError(w, 400, "invalid_registry", err.Error())
		return
	}
	id := r.PathValue("id")
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not rotate registry credential")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	var oldCipher string
	var version int
	if err := tx.QueryRow(r.Context(), `SELECT ciphertext,version FROM runner_credentials WHERE id=$1 AND revoked_at IS NULL FOR UPDATE`, id).Scan(&oldCipher, &version); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "registry not found")
		return
	} else if err != nil {
		writeError(w, 500, "database_error", "could not load registry")
		return
	}
	if input.Password == "" {
		plain, err := s.vault.Decrypt(oldCipher, registryAAD(id, version))
		if err != nil {
			writeError(w, 500, "secret_error", "could not rotate registry credential")
			return
		}
		var credential map[string]string
		if json.Unmarshal([]byte(plain), &credential) != nil {
			writeError(w, 500, "secret_error", "stored registry credential is invalid")
			return
		}
		input.Password = credential["password"]
	}
	version++
	plaintext, _ := json.Marshal(map[string]string{"username": input.Username, "password": input.Password})
	ciphertext, err := s.vault.Encrypt(string(plaintext), registryAAD(id, version))
	if err != nil {
		writeError(w, 500, "secret_error", "could not encrypt registry credential")
		return
	}
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	p, _ := principalFrom(r)
	tag, err := tx.Exec(r.Context(), `UPDATE runner_credentials SET name=$2,endpoint=$3,project=$4,username=$5,insecure_skip_verify=$6,active=$7,version=$8,ciphertext=$9,approved_at=now(),approved_by=$10 WHERE id=$1 AND revoked_at IS NULL`, id, input.Name, input.Endpoint, input.Project, input.Username, input.Insecure, active, version, ciphertext, p.UserID)
	if err != nil || tag.RowsAffected() != 1 {
		writeError(w, 409, "registry_conflict", "registry name already exists")
		return
	}
	var profileCount int64
	if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM deployment_profiles WHERE registry_credential_id=$1`, id).Scan(&profileCount); err == nil {
		tag, err = tx.Exec(r.Context(), `UPDATE deployment_profiles SET registry_url=$2,registry_host=$3,registry_project=$4,registry_insecure=$5,updated_at=now() WHERE registry_credential_id=$1`, id, input.Endpoint, registryHost(input.Endpoint), input.Project, input.Insecure)
	}
	if err != nil || tag.RowsAffected() != profileCount {
		writeError(w, 409, "registry_in_use", "dependent deployment profiles could not be updated atomically")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 409, "registry_conflict", "registry credential rotation could not be committed")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "registry.rotate", "registry", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 200, map[string]any{"id": id, "name": input.Name, "endpoint": input.Endpoint, "project": input.Project, "username": input.Username, "insecureSkipVerify": input.Insecure, "active": active, "version": version, "secretConfigured": true})
}
func registryHost(endpoint string) string { u, _ := url.Parse(endpoint); return u.Host }
func (s *Server) revokeRegistry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE runner_credentials SET active=FALSE,revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	if err != nil || tag.RowsAffected() == 0 {
		writeError(w, 404, "not_found", "registry not found")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "registry.revoke", "registry", id, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(204)
}

type targetCredentialInput struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Secret string `json:"secret"`
	Active *bool  `json:"active"`
}

func validateTargetCredential(input *targetCredentialInput, requireSecret bool) error {
	input.Name = strings.TrimSpace(input.Name)
	input.Type = strings.ToUpper(strings.TrimSpace(input.Type))
	if input.Name == "" || len(input.Name) > 200 {
		return errors.New("name is required and must not exceed 200 characters")
	}
	allowed := map[string]bool{"SSH_PRIVATE_KEY": true, "KUBECONFIG": true, "TOKEN": true, "OPAQUE_FILE": true}
	if !allowed[input.Type] {
		return errors.New("type must be SSH_PRIVATE_KEY, KUBECONFIG, TOKEN, or OPAQUE_FILE")
	}
	if requireSecret && input.Secret == "" {
		return errors.New("secret is required")
	}
	if len(input.Secret) > 1<<20 || strings.ContainsRune(input.Secret, '\x00') {
		return errors.New("secret must not exceed 1 MiB or contain NUL bytes")
	}
	return nil
}

func targetCredentialAAD(id string, version int) string {
	return fmt.Sprintf("target-credential:%s:v%d", id, version)
}

func (s *Server) listTargetCredentials(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	if status != "" && status != "active" && status != "inactive" && status != "revoked" {
		writeError(w, http.StatusBadRequest, "invalid_filter", "status must be active, inactive, or revoked")
		return
	}
	filter := `($1='' OR name ILIKE '%'||$1||'%' OR credential_type ILIKE '%'||$1||'%') AND
		($2='' OR ($2='active' AND active AND revoked_at IS NULL) OR ($2='inactive' AND NOT active AND revoked_at IS NULL) OR ($2='revoked' AND revoked_at IS NOT NULL))`
	var total int
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM target_credentials WHERE `+filter, search, status).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not count target credentials")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT id::text,name,credential_type,version,ciphertext<>'',active,approved_at,revoked_at,created_at,updated_at FROM target_credentials WHERE `+filter+` ORDER BY name LIMIT $3 OFFSET $4`, search, status, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not list target credentials")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, name, credentialType string
		var version int
		var configured, active bool
		var approved, revoked *time.Time
		var created, updated time.Time
		if err := rows.Scan(&id, &name, &credentialType, &version, &configured, &active, &approved, &revoked, &created, &updated); err != nil {
			writeError(w, http.StatusInternalServerError, "database_error", "could not list target credentials")
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "type": credentialType, "version": version, "secretConfigured": configured, "active": active && revoked == nil, "approved": approved != nil, "revokedAt": revoked, "createdAt": created, "updatedAt": updated})
	}
	writeJSON(w, http.StatusOK, page(items, total, limit, offset))
}

func (s *Server) createTargetCredential(w http.ResponseWriter, r *http.Request) {
	var input targetCredentialInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := validateTargetCredential(&input, true); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_target_credential", err.Error())
		return
	}
	id, _ := secure.NewID()
	const version = 1
	ciphertext, err := s.vault.Encrypt(input.Secret, targetCredentialAAD(id, version))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "secret_error", "could not encrypt target credential")
		return
	}
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	p, _ := principalFrom(r)
	_, err = s.store.Pool.Exec(r.Context(), `INSERT INTO target_credentials(id,name,credential_type,version,ciphertext,active,approved_at,approved_by,created_by) VALUES($1,$2,$3,$4,$5,$6,now(),$7,$7)`, id, input.Name, input.Type, version, ciphertext, active, p.UserID)
	if err != nil {
		writeError(w, http.StatusConflict, "target_credential_conflict", "target credential name already exists")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "target_credential.create", "target_credential", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": input.Name, "type": input.Type, "version": version, "active": active, "approved": true, "secretConfigured": true})
}

func (s *Server) updateTargetCredential(w http.ResponseWriter, r *http.Request) {
	var input targetCredentialInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Secret != "" {
		writeError(w, http.StatusBadRequest, "invalid_target_credential", "use the rotate endpoint to replace the secret")
		return
	}
	if err := validateTargetCredential(&input, false); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_target_credential", err.Error())
		return
	}
	id := r.PathValue("id")
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE target_credentials SET name=$2,active=COALESCE($3,active),updated_at=now() WHERE id=$1 AND credential_type=$4 AND revoked_at IS NULL`, id, input.Name, input.Active, input.Type)
	if err != nil {
		writeError(w, http.StatusConflict, "target_credential_in_use", "target credential is in use or its name conflicts")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "target credential not found or type is immutable")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "target_credential.update", "target_credential", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": input.Name, "type": input.Type, "active": input.Active, "secretConfigured": true})
}

func (s *Server) rotateTargetCredential(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Secret string `json:"secret"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Secret == "" || len(input.Secret) > 1<<20 || strings.ContainsRune(input.Secret, '\x00') {
		writeError(w, http.StatusBadRequest, "invalid_target_credential", "secret is required, must not exceed 1 MiB, and cannot contain NUL bytes")
		return
	}
	id := r.PathValue("id")
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not rotate target credential")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	var version int
	if err := tx.QueryRow(r.Context(), `SELECT version FROM target_credentials WHERE id=$1 AND revoked_at IS NULL FOR UPDATE`, id).Scan(&version); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "target credential not found")
		return
	} else if err != nil {
		writeError(w, http.StatusConflict, "target_credential_in_use", "target credential cannot be rotated while in use")
		return
	}
	version++
	ciphertext, err := s.vault.Encrypt(input.Secret, targetCredentialAAD(id, version))
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE target_credentials SET version=$2,ciphertext=$3,approved_at=now(),approved_by=$4,updated_at=now() WHERE id=$1`, id, version, ciphertext, principalUserID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusConflict, "target_credential_in_use", "target credential cannot be rotated while in use")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "target_credential.rotate", "target_credential", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "version": version, "secretConfigured": true})
}

func principalUserID(r *http.Request) string {
	p, _ := principalFrom(r)
	return p.UserID
}

func (s *Server) revokeTargetCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE target_credentials SET active=FALSE,revoked_at=now(),updated_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	if err != nil {
		writeError(w, http.StatusConflict, "target_credential_in_use", "target credential cannot be revoked while in use")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "target credential not found")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "target_credential.revoke", "target_credential", id, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(http.StatusNoContent)
}

type runnerInput struct {
	Name              string          `json:"name"`
	Address           string          `json:"address"`
	Token             string          `json:"token"`
	Labels            json.RawMessage `json:"labels"`
	MaxConcurrentJobs int             `json:"maxConcurrentJobs"`
	Active            *bool           `json:"active"`
}

func parseLabels(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	var values []string
	if json.Unmarshal(raw, &values) == nil {
		return values, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		for _, item := range strings.Split(text, ",") {
			if item = strings.TrimSpace(item); item != "" {
				values = append(values, item)
			}
		}
		return values, nil
	}
	return nil, errors.New("labels must be a comma-separated string or string array")
}
func validateRunner(input *runnerInput) ([]string, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Address = strings.TrimSpace(input.Address)
	if input.Name == "" || input.Address == "" {
		return nil, errors.New("name and address are required")
	}
	if input.MaxConcurrentJobs == 0 {
		input.MaxConcurrentJobs = 1
	}
	if input.MaxConcurrentJobs != 1 {
		return nil, errors.New("this Runner version processes one job at a time; maxConcurrentJobs must be 1")
	}
	return parseLabels(input.Labels)
}
func (s *Server) listRunners(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	if status != "" && status != "online" && status != "offline" && status != "inactive" {
		writeError(w, http.StatusBadRequest, "invalid_filter", "status must be online, offline, or inactive")
		return
	}
	filter := `($1='' OR name ILIKE '%'||$1||'%' OR address ILIKE '%'||$1||'%') AND
		($2='' OR ($2='online' AND active AND managed_by_runner AND worker_id IS NOT NULL AND last_heartbeat_at>=clock_timestamp()-interval '60 seconds') OR ($2='offline' AND active AND (last_heartbeat_at IS NULL OR last_heartbeat_at<clock_timestamp()-interval '60 seconds')) OR ($2='inactive' AND NOT active))`
	var total int
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM runner_instances WHERE `+filter, search, status).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not count runners")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT id::text,name,address,token_prefix,labels,max_concurrent_jobs,active,last_heartbeat_at,created_at FROM runner_instances WHERE `+filter+` ORDER BY name LIMIT $3 OFFSET $4`, search, status, limit, offset)
	if err != nil {
		writeError(w, 500, "database_error", "could not list runners")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, name, address string
		var prefix *string
		var labels []string
		var max int
		var active bool
		var heartbeat *time.Time
		var created time.Time
		if rows.Scan(&id, &name, &address, &prefix, &labels, &max, &active, &heartbeat, &created) != nil {
			writeError(w, 500, "database_error", "could not list runners")
			return
		}
		status := "REGISTERED"
		if !active {
			status = "INACTIVE"
		} else if heartbeat != nil && time.Since(*heartbeat) < time.Minute {
			status = "ONLINE"
		} else if heartbeat != nil {
			status = "OFFLINE"
		}
		items = append(items, map[string]any{"id": id, "name": name, "address": address, "tokenPrefix": prefix, "labels": labels, "maxConcurrentJobs": max, "active": active, "status": status, "lastHeartbeatAt": heartbeat, "createdAt": created})
	}
	writeJSON(w, 200, page(items, total, limit, offset))
}
func (s *Server) createRunner(w http.ResponseWriter, r *http.Request) {
	var input runnerInput
	if !decodeJSON(w, r, &input) {
		return
	}
	labels, err := validateRunner(&input)
	if err != nil {
		writeError(w, 400, "invalid_runner", err.Error())
		return
	}
	token := input.Token
	if token == "" {
		random, _ := secure.RandomToken(32)
		token = "rdr_" + random
	}
	if len(token) < 24 {
		writeError(w, 400, "invalid_runner", "token must contain at least 24 characters")
		return
	}
	id, _ := secure.NewID()
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	p, _ := principalFrom(r)
	_, err = s.store.Pool.Exec(r.Context(), `INSERT INTO runner_instances(id,name,address,token_prefix,token_hash,labels,max_concurrent_jobs,active,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, input.Name, input.Address, token[:12], secure.TokenHash(token), labels, input.MaxConcurrentJobs, active, p.UserID)
	if err != nil {
		writeError(w, 409, "runner_conflict", "runner name or token already exists")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "runner.create", "runner", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 201, map[string]any{"id": id, "name": input.Name, "address": input.Address, "labels": labels, "maxConcurrentJobs": input.MaxConcurrentJobs, "active": active, "token": token, "tokenVisibleOnce": true})
}
func (s *Server) updateRunner(w http.ResponseWriter, r *http.Request) {
	var input runnerInput
	if !decodeJSON(w, r, &input) {
		return
	}
	labels, err := validateRunner(&input)
	if err != nil {
		writeError(w, 400, "invalid_runner", err.Error())
		return
	}
	id := r.PathValue("id")
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	var tag pgconn.CommandTag
	if input.Token != "" {
		if len(input.Token) < 24 {
			writeError(w, 400, "invalid_runner", "token must contain at least 24 characters")
			return
		}
		tag, err = s.store.Pool.Exec(r.Context(), `UPDATE runner_instances SET name=$2,address=$3,labels=$4,max_concurrent_jobs=$5,active=$6,token_prefix=$7,token_hash=$8,updated_at=now() WHERE id=$1`, id, input.Name, input.Address, labels, input.MaxConcurrentJobs, active, input.Token[:12], secure.TokenHash(input.Token))
	} else {
		tag, err = s.store.Pool.Exec(r.Context(), `UPDATE runner_instances SET name=$2,address=$3,labels=$4,max_concurrent_jobs=$5,active=$6,updated_at=now() WHERE id=$1`, id, input.Name, input.Address, labels, input.MaxConcurrentJobs, active)
	}
	if err != nil {
		writeError(w, 409, "runner_conflict", "runner name or token already exists")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 404, "not_found", "runner not found")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "runner.update", "runner", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 200, map[string]any{"id": id, "name": input.Name, "address": input.Address, "labels": labels, "maxConcurrentJobs": input.MaxConcurrentJobs, "active": active})
}
func (s *Server) deleteRunner(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tag, err := s.store.Pool.Exec(r.Context(), `DELETE FROM runner_instances WHERE id=$1`, id)
	if err != nil || tag.RowsAffected() == 0 {
		writeError(w, 404, "not_found", "runner not found")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "runner.delete", "runner", id, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(204)
}
func (s *Server) runnerHeartbeat(w http.ResponseWriter, r *http.Request) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		writeError(w, 401, "unauthorized", "runner token required")
		return
	}
	token := strings.TrimSpace(auth[7:])
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE runner_instances SET last_heartbeat_at=now(),updated_at=now() WHERE token_hash=$1 AND active=TRUE`, secure.TokenHash(token))
	if err != nil || tag.RowsAffected() == 0 {
		writeError(w, 401, "unauthorized", "invalid runner token")
		return
	}
	w.WriteHeader(204)
}
