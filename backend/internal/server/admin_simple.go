package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/jackc/pgx/v5"
)

type simpleTarget struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	UploadDir      string   `json:"uploadDir"`
	CommandPath    string   `json:"commandPath"`
	CommandArgs    []string `json:"commandArgs"`
	WorkingDir     string   `json:"workingDir"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
	MaxUploadBytes int64    `json:"maxUploadBytes"`
	Active         bool     `json:"active"`
}

const simpleTargetColumns = `id::text,name,description,upload_dir,COALESCE(command_path,''),command_args,
	COALESCE(working_dir,''),COALESCE(timeout_seconds,0),max_upload_bytes,active AND revoked_at IS NULL`

func scanSimpleTarget(row pgx.Row) (simpleTarget, error) {
	var t simpleTarget
	err := row.Scan(&t.ID, &t.Name, &t.Description, &t.UploadDir, &t.CommandPath, &t.CommandArgs,
		&t.WorkingDir, &t.TimeoutSeconds, &t.MaxUploadBytes, &t.Active)
	if t.CommandArgs == nil {
		t.CommandArgs = []string{}
	}
	return t, err
}

type simpleTargetInput struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	UploadDir      string `json:"uploadDir"`
	CommandPath    string `json:"commandPath"`
	CommandArgs    string `json:"commandArgs"`
	WorkingDir     string `json:"workingDir"`
	TimeoutSeconds *int   `json:"timeoutSeconds"`
	MaxUploadBytes *int64 `json:"maxUploadBytes"`
	Active         *bool  `json:"active"`
}

// splitCommandArgs accepts the newline-separated form the admin UI renders.
// One argument per line, never split on spaces: an argument that legitimately
// contains a space must survive intact.
func splitCommandArgs(raw string) []string {
	args := []string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		args = append(args, line)
	}
	return args
}

func joinCommandArgs(args []string) string { return strings.Join(args, "\n") }

// normalizeSimpleTarget validates administrator input. A command is optional
// here because SHARED mode does not need one; putSimpleSettings enforces that
// every active target has a command before PER_TARGET mode can be selected.
func normalizeSimpleTarget(cfg simpleSettings, input *simpleTargetInput) (simpleTarget, error) {
	target := simpleTarget{
		Name:        strings.TrimSpace(input.Name),
		Description: strings.TrimSpace(input.Description),
		CommandPath: strings.TrimSpace(input.CommandPath),
		WorkingDir:  strings.TrimSpace(input.WorkingDir),
		CommandArgs: splitCommandArgs(input.CommandArgs),
		Active:      true,
	}
	if target.Name == "" || len(target.Name) > 200 {
		return target, errors.New("이름은 1~200자여야 합니다")
	}
	if len(target.Description) > 1000 {
		return target, errors.New("설명은 1000자를 넘을 수 없습니다")
	}
	dir, err := normalizeUploadDir(cfg.UploadRoot, input.UploadDir)
	if err != nil {
		return target, err
	}
	target.UploadDir = dir

	target.TimeoutSeconds = 600
	if input.TimeoutSeconds != nil {
		target.TimeoutSeconds = *input.TimeoutSeconds
	}
	target.MaxUploadBytes = 10 << 30
	if input.MaxUploadBytes != nil {
		target.MaxUploadBytes = *input.MaxUploadBytes
	}
	if target.MaxUploadBytes < 1<<20 || target.MaxUploadBytes > 1<<40 {
		return target, errors.New("최대 업로드 크기는 1 MiB 이상 1 TiB 이하여야 합니다")
	}
	if input.Active != nil {
		target.Active = *input.Active
	}
	if target.CommandPath != "" {
		if err := validateCommandFields(target.CommandPath, target.CommandArgs, target.WorkingDir, target.TimeoutSeconds); err != nil {
			return target, err
		}
	} else if cfg.CommandMode == commandModePerTarget && target.Active {
		return target, errors.New("서비스별 명령 모드에서는 활성 대상에 실행 명령이 필요합니다")
	}
	if err := ensureUploadDir(target.UploadDir); err != nil {
		return target, err
	}
	return target, nil
}

func (s *Server) listSimpleTargets(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	filter := `($1='' OR name ILIKE '%'||$1||'%' OR upload_dir ILIKE '%'||$1||'%')`
	var total int
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM simple_targets WHERE revoked_at IS NULL AND `+filter, search).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not count simple targets")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT `+simpleTargetColumns+` FROM simple_targets
		WHERE revoked_at IS NULL AND `+filter+` ORDER BY name LIMIT $2 OFFSET $3`, search, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not list simple targets")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		target, err := scanSimpleTarget(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database_error", "could not list simple targets")
			return
		}
		items = append(items, map[string]any{
			"id": target.ID, "name": target.Name, "description": target.Description,
			"uploadDir": target.UploadDir, "commandPath": target.CommandPath,
			"commandArgs": joinCommandArgs(target.CommandArgs), "workingDir": target.WorkingDir,
			"timeoutSeconds": target.TimeoutSeconds, "maxUploadBytes": target.MaxUploadBytes,
			"active": target.Active,
		})
	}
	writeJSON(w, http.StatusOK, page(items, total, limit, offset))
}

func (s *Server) createSimpleTarget(w http.ResponseWriter, r *http.Request) {
	var input simpleTargetInput
	if !decodeJSON(w, r, &input) {
		return
	}
	cfg, err := loadSimpleSettings(r.Context(), s.store.Pool)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load simple settings")
		return
	}
	target, err := normalizeSimpleTarget(cfg, &input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_simple_target", err.Error())
		return
	}
	id, err := secure.NewID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "id_error", "could not allocate an identifier")
		return
	}
	p, _ := principalFrom(r)
	_, err = s.store.Pool.Exec(r.Context(), `INSERT INTO simple_targets
		(id,name,description,upload_dir,command_path,command_args,working_dir,timeout_seconds,max_upload_bytes,active,created_by)
		VALUES($1,$2,$3,$4,NULLIF($5,''),$6,NULLIF($7,''),$8,$9,$10,$11)`,
		id, target.Name, target.Description, target.UploadDir, target.CommandPath, target.CommandArgs,
		target.WorkingDir, target.TimeoutSeconds, target.MaxUploadBytes, target.Active, p.UserID)
	if err != nil {
		writeError(w, http.StatusConflict, "simple_target_conflict", "이름 또는 업로드 경로가 이미 사용 중입니다")
		return
	}
	target.ID = id
	s.store.Audit(r.Context(), p.UserID, "simple_target.create", "simple_target", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, http.StatusCreated, target)
}

func (s *Server) updateSimpleTarget(w http.ResponseWriter, r *http.Request) {
	var input simpleTargetInput
	if !decodeJSON(w, r, &input) {
		return
	}
	cfg, err := loadSimpleSettings(r.Context(), s.store.Pool)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load simple settings")
		return
	}
	target, err := normalizeSimpleTarget(cfg, &input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_simple_target", err.Error())
		return
	}
	id := r.PathValue("id")
	p, _ := principalFrom(r)
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE simple_targets SET
		name=$2,description=$3,upload_dir=$4,command_path=NULLIF($5,''),command_args=$6,
		working_dir=NULLIF($7,''),timeout_seconds=$8,max_upload_bytes=$9,active=$10,
		updated_by=$11,updated_at=now()
		WHERE id=$1 AND revoked_at IS NULL`,
		id, target.Name, target.Description, target.UploadDir, target.CommandPath, target.CommandArgs,
		target.WorkingDir, target.TimeoutSeconds, target.MaxUploadBytes, target.Active, p.UserID)
	if err != nil {
		writeError(w, http.StatusConflict, "simple_target_conflict", "이름 또는 업로드 경로가 이미 사용 중입니다")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "simple target not found")
		return
	}
	target.ID = id
	s.store.Audit(r.Context(), p.UserID, "simple_target.update", "simple_target", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, http.StatusOK, target)
}

func (s *Server) revokeSimpleTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// A target with work in flight must not disappear from under the runner.
	var running bool
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM simple_runs WHERE target_id=$1 AND status IN ('PENDING','RUNNING'))`, id).Scan(&running); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not inspect simple target")
		return
	}
	if running {
		writeError(w, http.StatusConflict, "simple_target_busy", "실행 중인 작업이 끝난 뒤에 삭제할 수 있습니다")
		return
	}
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE simple_targets SET active=FALSE,revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	if err != nil || tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "simple target not found")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "simple_target.revoke", "simple_target", id, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(http.StatusNoContent)
}

type simpleSettingsInput struct {
	DefaultUIMode        *string `json:"defaultUiMode"`
	CommandMode          *string `json:"commandMode"`
	SharedCommandPath    *string `json:"sharedCommandPath"`
	SharedCommandArgs    *string `json:"sharedCommandArgs"`
	SharedWorkingDir     *string `json:"sharedWorkingDir"`
	SharedTimeoutSeconds *int    `json:"sharedTimeoutSeconds"`
	ReplicationEnabled   *bool   `json:"replicationEnabled"`
	ReplicationRegistry  *string `json:"replicationRegistryId"`
	ReplicationPolicyID  *int64  `json:"replicationPolicyId"`
	ReplicationPolicy    *string `json:"replicationPolicyName"`
	ReplicationTimeout   *int    `json:"replicationTimeoutSeconds"`
	ReplicationScope     *string `json:"replicationScope"`
	AppDeployEnabled     *bool   `json:"appDeployEnabled"`
	AppDeployScope       *string `json:"appDeployScope"`
	AppDeployPath        *string `json:"appDeployCommandPath"`
	AppDeployArgs        *string `json:"appDeployCommandArgs"`
	AppDeployDir         *string `json:"appDeployWorkingDir"`
	AppDeployTimeout     *int    `json:"appDeployTimeoutSeconds"`
	UploadRoot           *string `json:"uploadRoot"`
}

// normalizeStageScope keeps the two post-deployment scopes to the values the
// storage constraint allows, so a typo cannot reach the database.
func normalizeStageScope(value string) (string, error) {
	scope := strings.ToUpper(strings.TrimSpace(value))
	if scope != stageScopeEach && scope != stageScopeOnce {
		return "", errors.New("실행 범위는 EACH 또는 ONCE여야 합니다")
	}
	return scope, nil
}

func (s *Server) getSimpleSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadSimpleSettings(r.Context(), s.store.Pool)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load simple settings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"defaultUiMode": cfg.DefaultUIMode, "commandMode": cfg.CommandMode,
		"sharedCommandPath": cfg.SharedCommandPath, "sharedCommandArgs": joinCommandArgs(cfg.SharedCommandArgs),
		"sharedWorkingDir": cfg.SharedWorkingDir, "sharedTimeoutSeconds": cfg.SharedTimeoutSeconds,
		"uploadRoot": cfg.UploadRoot, "updatedAt": cfg.UpdatedAt,
		"replicationEnabled":        cfg.ReplicationEnabled,
		"replicationRegistryId":     cfg.ReplicationRegistry,
		"replicationPolicyId":       cfg.ReplicationPolicyID,
		"replicationPolicyName":     cfg.ReplicationPolicy,
		"replicationTimeoutSeconds": cfg.ReplicationTimeout,
		"replicationScope":          cfg.ReplicationScope,
		"appDeployEnabled":          cfg.AppDeployEnabled,
		"appDeployScope":            cfg.AppDeployScope,
		"appDeployCommandPath":      cfg.AppDeployPath,
		"appDeployCommandArgs":      joinCommandArgs(cfg.AppDeployArgs),
		"appDeployWorkingDir":       cfg.AppDeployDir,
		"appDeployTimeoutSeconds":   cfg.AppDeployTimeout,
	})
}

func (s *Server) putSimpleSettings(w http.ResponseWriter, r *http.Request) {
	var input simpleSettingsInput
	if !decodeJSON(w, r, &input) {
		return
	}
	p, _ := principalFrom(r)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not update simple settings")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck

	if _, err := tx.Exec(r.Context(), `SELECT 1 FROM simple_settings WHERE id='default' FOR UPDATE`); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not lock simple settings")
		return
	}
	cfg, err := loadSimpleSettings(r.Context(), tx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load simple settings")
		return
	}

	if input.DefaultUIMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*input.DefaultUIMode))
		if mode != "simple" && mode != "full" {
			writeError(w, http.StatusBadRequest, "invalid_simple_settings", "기본 모드는 simple 또는 full이어야 합니다")
			return
		}
		cfg.DefaultUIMode = mode
	}
	if input.UploadRoot != nil {
		root, err := normalizeUploadRoot(*input.UploadRoot)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_simple_settings", err.Error())
			return
		}
		cfg.UploadRoot = root
	}
	if input.SharedCommandPath != nil {
		cfg.SharedCommandPath = strings.TrimSpace(*input.SharedCommandPath)
	}
	if input.SharedCommandArgs != nil {
		cfg.SharedCommandArgs = splitCommandArgs(*input.SharedCommandArgs)
	}
	if input.SharedWorkingDir != nil {
		cfg.SharedWorkingDir = strings.TrimSpace(*input.SharedWorkingDir)
	}
	if input.SharedTimeoutSeconds != nil {
		cfg.SharedTimeoutSeconds = *input.SharedTimeoutSeconds
	}
	if input.ReplicationEnabled != nil {
		cfg.ReplicationEnabled = *input.ReplicationEnabled
	}
	if input.ReplicationRegistry != nil {
		cfg.ReplicationRegistry = strings.TrimSpace(*input.ReplicationRegistry)
	}
	if input.ReplicationPolicyID != nil {
		cfg.ReplicationPolicyID = *input.ReplicationPolicyID
	}
	if input.ReplicationPolicy != nil {
		cfg.ReplicationPolicy = strings.TrimSpace(*input.ReplicationPolicy)
	}
	if input.ReplicationTimeout != nil {
		cfg.ReplicationTimeout = *input.ReplicationTimeout
	}
	if input.ReplicationScope != nil {
		scope, err := normalizeStageScope(*input.ReplicationScope)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_simple_settings", err.Error())
			return
		}
		cfg.ReplicationScope = scope
	}
	if input.AppDeployEnabled != nil {
		cfg.AppDeployEnabled = *input.AppDeployEnabled
	}
	if input.AppDeployScope != nil {
		scope, err := normalizeStageScope(*input.AppDeployScope)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_simple_settings", err.Error())
			return
		}
		cfg.AppDeployScope = scope
	}
	if input.AppDeployPath != nil {
		cfg.AppDeployPath = strings.TrimSpace(*input.AppDeployPath)
	}
	if input.AppDeployArgs != nil {
		cfg.AppDeployArgs = splitCommandArgs(*input.AppDeployArgs)
	}
	if input.AppDeployDir != nil {
		cfg.AppDeployDir = strings.TrimSpace(*input.AppDeployDir)
	}
	if input.AppDeployTimeout != nil {
		cfg.AppDeployTimeout = *input.AppDeployTimeout
	}
	if cfg.ReplicationTimeout < 1 || cfg.ReplicationTimeout > 86400 {
		writeError(w, http.StatusBadRequest, "invalid_simple_settings", "복제 제한 시간은 1초 이상 86400초 이하여야 합니다")
		return
	}
	// Enabling without a rule would fail on every deployment, so the pair
	// is required up front rather than discovered at run time.
	if cfg.ReplicationEnabled {
		if cfg.ReplicationRegistry == "" || cfg.ReplicationPolicyID <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_simple_settings", "복제를 사용하려면 레지스트리와 복제 규칙을 함께 선택해야 합니다")
			return
		}
		var exists bool
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM runner_credentials WHERE id=$1 AND active AND revoked_at IS NULL)`, cfg.ReplicationRegistry).Scan(&exists); err != nil || !exists {
			writeError(w, http.StatusBadRequest, "invalid_simple_settings", "선택한 Harbor Registry 를 사용할 수 없습니다")
			return
		}
	}
	// The app deployment command is validated whenever it is present, not only
	// while the stage is on, so a stored value can never become active while
	// invalid. Enabling without one would fail on every deployment.
	if cfg.AppDeployPath != "" {
		if err := validateCommandFields(cfg.AppDeployPath, cfg.AppDeployArgs, cfg.AppDeployDir, cfg.AppDeployTimeout); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_app_deploy_command", err.Error())
			return
		}
	} else if cfg.AppDeployEnabled {
		writeError(w, http.StatusBadRequest, "invalid_simple_settings", "앱 배포를 사용하려면 실행할 명령을 함께 입력해야 합니다")
		return
	}

	if input.CommandMode != nil {
		mode := strings.ToUpper(strings.TrimSpace(*input.CommandMode))
		if mode != commandModePerTarget && mode != commandModeShared {
			writeError(w, http.StatusBadRequest, "invalid_simple_settings", "명령 방식은 PER_TARGET 또는 SHARED여야 합니다")
			return
		}
		cfg.CommandMode = mode
	}

	// The shared command is validated whenever it is present, not only in
	// SHARED mode, so a stored value can never become active while invalid.
	if cfg.SharedCommandPath != "" {
		if err := validateCommandFields(cfg.SharedCommandPath, cfg.SharedCommandArgs, cfg.SharedWorkingDir, cfg.SharedTimeoutSeconds); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_shared_command", err.Error())
			return
		}
	}
	if err := s.checkCommandModeSwitch(r.Context(), tx, cfg); err != nil {
		writeError(w, http.StatusConflict, "command_mode_unavailable", err.Error())
		return
	}

	_, err = tx.Exec(r.Context(), `UPDATE simple_settings SET default_ui_mode=$1,command_mode=$2,
		shared_command_path=$3,shared_command_args=$4,shared_working_dir=$5,shared_timeout_seconds=$6,
		upload_root=$7,replication_enabled=$8,replication_registry_id=NULLIF($9,'')::uuid,
		replication_policy_id=NULLIF($10,0),replication_policy_name=$11,
		replication_timeout_seconds=$12,replication_scope=$13,app_deploy_enabled=$14,
		app_deploy_scope=$15,app_deploy_command_path=$16,app_deploy_command_args=$17,
		app_deploy_working_dir=$18,app_deploy_timeout_seconds=$19,
		updated_by=$20,updated_at=now() WHERE id='default'`,
		cfg.DefaultUIMode, cfg.CommandMode, cfg.SharedCommandPath, cfg.SharedCommandArgs,
		cfg.SharedWorkingDir, cfg.SharedTimeoutSeconds, cfg.UploadRoot,
		cfg.ReplicationEnabled, cfg.ReplicationRegistry, cfg.ReplicationPolicyID,
		cfg.ReplicationPolicy, cfg.ReplicationTimeout, cfg.ReplicationScope,
		cfg.AppDeployEnabled, cfg.AppDeployScope, cfg.AppDeployPath, cfg.AppDeployArgs,
		cfg.AppDeployDir, cfg.AppDeployTimeout, p.UserID)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not update simple settings")
		return
	}
	details, _ := json.Marshal(map[string]any{"commandMode": cfg.CommandMode, "defaultUiMode": cfg.DefaultUIMode})
	s.store.Audit(r.Context(), p.UserID, "simple_settings.update", "settings", "simple", "success", remoteIP(r), r.UserAgent(), details)
	s.getSimpleSettings(w, r)
}

// checkCommandModeSwitch refuses a mode that would leave simple mode unable to
// run: SHARED without a shared command, or PER_TARGET while an active target
// has no command of its own. Per-target commands are never cleared on a switch
// to SHARED, so switching back stays lossless.
func (s *Server) checkCommandModeSwitch(ctx context.Context, tx pgx.Tx, cfg simpleSettings) error {
	if cfg.CommandMode == commandModeShared {
		if cfg.SharedCommandPath == "" {
			return errors.New("공통 명령 모드로 바꾸려면 공통 명령을 먼저 저장해야 합니다")
		}
		return nil
	}
	var missing int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM simple_targets
		WHERE revoked_at IS NULL AND active AND command_path IS NULL`).Scan(&missing); err != nil {
		return errors.New("대상 명령 설정을 확인할 수 없습니다")
	}
	if missing > 0 {
		return errors.New("서비스별 명령 모드로 바꾸려면 활성 대상 모두에 실행 명령을 먼저 설정해야 합니다")
	}
	return nil
}

// listRegistryReplicationPolicies fetches the replication rules configured on a
// Harbor so the simple-mode settings screen can offer them by name. Reading
// them requires registry access because the call authenticates with that
// registry's stored robot credential.
func (s *Server) listRegistryReplicationPolicies(w http.ResponseWriter, r *http.Request) {
	registry, err := s.loadHarborRegistry(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Harbor Registry 를 찾을 수 없습니다")
		return
	}
	ctx, cancel := contextWithTimeout(r, 20*time.Second)
	defer cancel()
	policies, err := s.listReplicationPolicies(ctx, registry)
	if err != nil {
		// A reachability or credential problem is the administrator's to fix,
		// so the message is passed through rather than flattened.
		writeError(w, http.StatusBadGateway, "harbor_unavailable", err.Error())
		return
	}
	items := make([]map[string]any, 0, len(policies))
	for _, policy := range policies {
		items = append(items, map[string]any{
			"id": policy.ID, "name": policy.Name, "description": policy.Description,
			"enabled": policy.Enabled, "destination": policy.Destination,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "registryName": registry.Name})
}

// checkRegistryHarbor runs a connection test against a registry and returns the
// individual probe results. This exists because a bare 404 from the replication
// API has several unrelated causes and an operator cannot tell them apart from
// the failure alone.
func (s *Server) checkRegistryHarbor(w http.ResponseWriter, r *http.Request) {
	registry, err := s.loadHarborRegistry(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Harbor Registry 를 찾을 수 없습니다")
		return
	}
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.diagnoseHarbor(ctx, registry))
}
