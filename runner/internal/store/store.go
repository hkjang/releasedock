package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hkjang/releasedock/runner/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNoJob     = errors.New("no queued release job")
	ErrLockBusy  = errors.New("release lock is busy")
	ErrLostLease = errors.New("runner no longer owns the job lease")
	ErrInactive  = errors.New("runner is disabled by an administrator")
)

// Repository is deliberately narrow so the pipeline can be tested without a
// database. PostgreSQL-specific queue and lock behavior lives in PGStore.
type Repository interface {
	LoadSettings(context.Context) (model.Settings, error)
	RecoverStaleJobs(context.Context, time.Duration) (int64, error)
	ClaimJob(context.Context, time.Duration) (*model.Job, error)
	Heartbeat(context.Context, string) error
	BeginStep(context.Context, string, model.JobStatus) (model.Step, error)
	FinishStep(context.Context, model.Step, bool, model.StepResult) error
	AppendLog(context.Context, string, int64, string, int64, []byte) error
	RecordImage(context.Context, string, model.ImageRecord) error
	FinishJob(context.Context, string, model.JobStatus, string) error
}

// RecoverStaleJobs intentionally fails abandoned work instead of re-queueing
// it. A runner can disappear after a deployment command reached its target;
// automatic replay would risk a duplicate deployment. An administrator may
// explicitly retry the failed release after inspecting its logs.
func (s *PGStore) RecoverStaleJobs(ctx context.Context, staleAfter time.Duration) (int64, error) {
	if staleAfter <= 0 {
		return 0, errors.New("stale job interval must be positive")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT id::text
		FROM release_jobs
		WHERE status NOT IN ('QUEUED', 'SUCCESS', 'FAILED', 'ROLLED_BACK')
		  AND heartbeat_at < clock_timestamp() - $1::interval
		FOR UPDATE SKIP LOCKED`, durationInterval(staleAfter))
	if err != nil {
		return 0, fmt.Errorf("select stale release jobs: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `
			UPDATE release_jobs
			SET status = 'FAILED', failure_message = 'runner lease expired; manual retry required',
			    finished_at = clock_timestamp(), updated_at = clock_timestamp(),
			    locked_by = NULL, locked_at = NULL
			WHERE id = $1::uuid`, id); err != nil {
			return 0, fmt.Errorf("fail stale release job: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM release_locks WHERE job_id = $1::uuid`, id); err != nil {
			return 0, fmt.Errorf("release stale deployment lock: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE releases r
			SET status = 'FAILED', updated_at = clock_timestamp()
			FROM release_jobs j
			WHERE j.id = $1::uuid AND r.id = j.release_id`, id); err != nil {
			return 0, fmt.Errorf("synchronize stale release state: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int64(len(ids)), nil
}

type PGStore struct {
	pool           *pgxpool.Pool
	workerID       string
	runnerIdentity string
}

func Open(ctx context.Context, dsn, workerID string) (*PGStore, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL DSN: %w", err)
	}
	config.ConnConfig.RuntimeParams["application_name"] = "releasedock-runner"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	return &PGStore{pool: pool, workerID: workerID}, nil
}

func (s *PGStore) Close() { s.pool.Close() }

// RegisterRunner makes the direct-DB worker visible in the administrator UI.
// The stable identity is derived from the host, so restarts update the same
// inventory row without requiring another environment variable or token.
func (s *PGStore) RegisterRunner(ctx context.Context, identity, name, address string) (bool, error) {
	if identity == "" || name == "" || address == "" {
		return false, errors.New("runner identity, name, and address are required")
	}
	id, err := randomUUID()
	if err != nil {
		return false, err
	}
	var active bool
	err = s.pool.QueryRow(ctx, `
		INSERT INTO runner_instances
		    (id, worker_id, name, address, labels, max_concurrent_jobs,
		     active, managed_by_runner, last_heartbeat_at)
		VALUES ($1::uuid, $2, $3, $4, '{}'::text[], 1, TRUE, TRUE, clock_timestamp())
		ON CONFLICT (worker_id) DO UPDATE SET
		    address = EXCLUDED.address,
		    managed_by_runner = TRUE,
		    last_heartbeat_at = clock_timestamp(),
		    updated_at = clock_timestamp()
		RETURNING active`, id, identity, name, address).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("register runner instance: %w", err)
	}
	s.runnerIdentity = identity
	return active, nil
}

// HeartbeatRunner updates inventory health and returns whether new work may be
// claimed. Disabling a runner pauses polling after its current job completes.
func (s *PGStore) HeartbeatRunner(ctx context.Context) (bool, error) {
	if s.runnerIdentity == "" {
		return false, errors.New("runner is not registered")
	}
	var active bool
	err := s.pool.QueryRow(ctx, `
		UPDATE runner_instances
		SET last_heartbeat_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE worker_id = $1 AND managed_by_runner = TRUE
		RETURNING active`, s.runnerIdentity).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errors.New("runner registration no longer exists")
	}
	if err != nil {
		return false, fmt.Errorf("heartbeat runner instance: %w", err)
	}
	return active, nil
}

func (s *PGStore) LoadSettings(ctx context.Context) (model.Settings, error) {
	var (
		settings                                             model.Settings
		pollMS, lockRetryMS, refreshMS, heartbeatMS, staleMS int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT poll_interval_ms, lock_retry_ms, settings_refresh_ms,
		       heartbeat_interval_ms, stale_job_after_ms, rs.workspace_root,
		       app.artifact_storage_path, rs.command_path, rs.log_chunk_bytes
		FROM runner_settings rs
		CROSS JOIN app_settings app
		WHERE rs.singleton = TRUE AND app.id = 'default'`).Scan(
		&pollMS, &lockRetryMS, &refreshMS, &heartbeatMS, &staleMS,
		&settings.WorkspaceRoot, &settings.ArtifactRoot, &settings.CommandPath, &settings.LogChunkBytes,
	)
	if err != nil {
		return model.Settings{}, fmt.Errorf("load runner settings: %w", err)
	}
	settings.PollInterval = time.Duration(pollMS) * time.Millisecond
	settings.LockRetry = time.Duration(lockRetryMS) * time.Millisecond
	settings.SettingsRefresh = time.Duration(refreshMS) * time.Millisecond
	settings.HeartbeatInterval = time.Duration(heartbeatMS) * time.Millisecond
	settings.StaleJobAfter = time.Duration(staleMS) * time.Millisecond
	if err := validateSettings(settings); err != nil {
		return model.Settings{}, fmt.Errorf("invalid runner settings: %w", err)
	}
	return settings, nil
}

func validateSettings(settings model.Settings) error {
	if settings.PollInterval <= 0 || settings.LockRetry <= 0 || settings.SettingsRefresh <= 0 || settings.HeartbeatInterval <= 0 {
		return errors.New("poll, lock retry, refresh, and heartbeat intervals must be positive")
	}
	if settings.StaleJobAfter <= settings.HeartbeatInterval*2 {
		return errors.New("stale job interval must exceed twice the heartbeat interval")
	}
	if settings.WorkspaceRoot == "" || settings.ArtifactRoot == "" || settings.CommandPath == "" {
		return errors.New("workspace_root, artifact_root, and command_path are required")
	}
	if settings.LogChunkBytes < 1024 || settings.LogChunkBytes > 1<<20 {
		return errors.New("log_chunk_bytes must be between 1024 and 1048576")
	}
	return nil
}

func (s *PGStore) ClaimJob(ctx context.Context, lockRetry time.Duration) (*model.Job, error) {
	if s.runnerIdentity == "" {
		return nil, errors.New("runner is not registered")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin claim transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var active, heartbeatFresh bool
	if err := tx.QueryRow(ctx, `
		SELECT active,
		       COALESCE(last_heartbeat_at >= clock_timestamp() - interval '60 seconds', FALSE)
		FROM runner_instances
		WHERE worker_id = $1
		FOR SHARE`, s.runnerIdentity).Scan(&active, &heartbeatFresh); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.New("runner registration no longer exists")
		}
		return nil, fmt.Errorf("check runner state: %w", err)
	}
	if !active {
		return nil, ErrInactive
	}
	if !heartbeatFresh {
		return nil, ErrNoJob
	}

	var job model.Job
	err = tx.QueryRow(ctx, `
		SELECT j.id::text, j.release_id::text,
		       COALESCE(j.rollback_source_release_id::text, ''),
		       COALESCE(j.rollback_source_job_id::text, ''),
		       COALESCE(j.target_credential_id::text, ''),
		       COALESCE(j.target_credential_version, 0),
		       j.application, j.version, j.environment,
		       j.lock_key, j.artifact_path, j.expected_sha256, j.manifest_path, j.operation,
		       j.attempts + 1, j.profile_id::text
		FROM release_jobs j
		JOIN runner_instances ri
		  ON ri.worker_id = $1
		 AND ri.active = TRUE
		 AND ri.last_heartbeat_at >= clock_timestamp() - interval '60 seconds'
		WHERE j.status = 'QUEUED' AND j.available_at <= clock_timestamp()
		  AND j.runner_labels <@ ri.labels
		ORDER BY j.priority DESC, j.created_at ASC
		FOR UPDATE OF j SKIP LOCKED
		LIMIT 1`, s.runnerIdentity).Scan(
		&job.ID, &job.ReleaseID, &job.RollbackSourceReleaseID, &job.RollbackSourceJobID,
		&job.TargetCredential.ID, &job.TargetCredential.Version, &job.Application, &job.Version,
		&job.Environment, &job.LockKey, &job.ArtifactPath,
		&job.ExpectedSHA256, &job.ManifestPath, &job.Operation, &job.Attempt, &job.Profile.ID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoJob
	}
	if err != nil {
		return nil, fmt.Errorf("select queued job: %w", err)
	}

	result, err := tx.Exec(ctx, `
		INSERT INTO release_locks (lock_key, job_id, worker_id, acquired_at, heartbeat_at)
		VALUES ($1, $2::uuid, $3, clock_timestamp(), clock_timestamp())
		ON CONFLICT (lock_key) DO NOTHING`, job.LockKey, job.ID, s.workerID)
	if err != nil {
		return nil, fmt.Errorf("acquire release lock: %w", err)
	}
	if result.RowsAffected() != 1 {
		_, err = tx.Exec(ctx, `UPDATE release_jobs SET available_at = clock_timestamp() + $2::interval WHERE id = $1::uuid`, job.ID, durationInterval(lockRetry))
		if err != nil {
			return nil, fmt.Errorf("defer lock-busy job: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit deferred job: %w", err)
		}
		return nil, ErrLockBusy
	}

	dependencyErr := s.loadProfile(ctx, tx, &job)
	if dependencyErr == nil {
		dependencyErr = s.loadTargetCredential(ctx, tx, &job)
	}
	if dependencyErr == nil && job.Operation == model.OperationRollback {
		dependencyErr = s.loadRollbackImages(ctx, tx, &job)
	}
	if dependencyErr == nil {
		dependencyErr = validateJob(&job)
	}
	if dependencyErr != nil {
		// A revoked or invalid dependency must not leave a poison job at the
		// front of the queue forever. Fail it atomically and release its lock.
		failure := truncate(dependencyErr.Error(), 4000)
		if _, updateErr := tx.Exec(ctx, `
			UPDATE release_jobs
			SET status = 'FAILED', failure_message = $2,
			    finished_at = clock_timestamp(), updated_at = clock_timestamp()
			WHERE id = $1::uuid`, job.ID, failure); updateErr != nil {
			return nil, errors.Join(dependencyErr, fmt.Errorf("fail invalid queued job: %w", updateErr))
		}
		if _, deleteErr := tx.Exec(ctx, `DELETE FROM release_locks WHERE job_id = $1::uuid`, job.ID); deleteErr != nil {
			return nil, errors.Join(dependencyErr, fmt.Errorf("release invalid job lock: %w", deleteErr))
		}
		if _, updateErr := tx.Exec(ctx, `
			UPDATE releases r
			SET status = 'FAILED', updated_at = clock_timestamp()
			FROM release_jobs j
			WHERE j.id = $1::uuid AND r.id = j.release_id`, job.ID); updateErr != nil {
			return nil, errors.Join(dependencyErr, fmt.Errorf("synchronize invalid release: %w", updateErr))
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return nil, errors.Join(dependencyErr, fmt.Errorf("commit invalid queued job: %w", commitErr))
		}
		return nil, fmt.Errorf("reject invalid queued job %s: %w", job.ID, dependencyErr)
	}
	result, err = tx.Exec(ctx, `
		UPDATE release_jobs
		SET status = 'VALIDATING', attempts = $2, locked_by = $3,
		    locked_at = clock_timestamp(), heartbeat_at = clock_timestamp(),
		    started_at = COALESCE(started_at, clock_timestamp()), failure_message = NULL
		WHERE id = $1::uuid`, job.ID, job.Attempt, s.workerID)
	if err != nil || result.RowsAffected() != 1 {
		if err == nil {
			err = ErrLostLease
		}
		return nil, fmt.Errorf("mark job claimed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit job claim: %w", err)
	}
	return &job, nil
}

func (s *PGStore) loadTargetCredential(ctx context.Context, tx pgx.Tx, job *model.Job) error {
	credential := &job.TargetCredential
	if credential.ID == "" && credential.Version == 0 {
		return nil
	}
	if credential.ID == "" || credential.Version <= 0 {
		return errors.New("target credential snapshot is incomplete")
	}
	err := tx.QueryRow(ctx, `
		SELECT credential_type,ciphertext
		FROM target_credentials
		WHERE id=$1::uuid AND version=$2 AND active=TRUE
		  AND approved_at IS NOT NULL AND revoked_at IS NULL`, credential.ID, credential.Version).
		Scan(&credential.Type, &credential.Ciphertext)
	if err != nil {
		return fmt.Errorf("load active immutable target credential: %w", err)
	}
	credential.AAD = fmt.Sprintf("target-credential:%s:v%d", credential.ID, credential.Version)
	return nil
}

func (s *PGStore) loadProfile(ctx context.Context, tx pgx.Tx, job *model.Job) error {
	var (
		healthJSON, scriptArgsJSON         []byte
		commandTimeoutSeconds, maxLogBytes int64
	)
	err := tx.QueryRow(ctx, `
		SELECT p.name, p.max_archive_bytes, p.max_extracted_bytes,
		       p.max_archive_files, p.max_images, p.allow_symlinks,
		       p.runtime_kind, p.runtime_binary_path, p.containerd_namespace,
		       p.registry_url, p.registry_host, p.registry_project,
		       p.registry_insecure, p.registry_ca_pem,
		       COALESCE('credential:' || c.id::text || ':v' || c.version::text, ''),
		       COALESCE(c.ciphertext, ''), p.health_checks,
		       p.command_timeout_seconds, p.max_log_bytes, p.auto_rollback,
		       p.cleanup_workspace, p.keep_failed_workspace
		FROM deployment_profiles p
		LEFT JOIN runner_credentials c ON c.id = p.registry_credential_id
		WHERE p.id = $1::uuid AND p.active = TRUE AND p.revoked_at IS NULL
		  AND (c.id IS NULL OR (c.active = TRUE AND c.approved_at IS NOT NULL AND c.revoked_at IS NULL))`, job.Profile.ID).Scan(
		&job.Profile.Name, &job.Profile.Extraction.MaxArchiveBytes,
		&job.Profile.Extraction.MaxExtractedBytes, &job.Profile.Extraction.MaxFiles,
		&job.Profile.Extraction.MaxImages, &job.Profile.Extraction.AllowSymlinks,
		&job.Profile.Runtime.Kind, &job.Profile.Runtime.BinaryPath,
		&job.Profile.Runtime.Namespace, &job.Profile.Runtime.RegistryURL,
		&job.Profile.Runtime.RegistryHost, &job.Profile.Runtime.RegistryProject,
		&job.Profile.Runtime.RegistryInsecure, &job.Profile.Runtime.RegistryCAPEM,
		&job.Profile.Runtime.CredentialAAD, &job.Profile.Runtime.CredentialCiphertext,
		&healthJSON, &commandTimeoutSeconds, &maxLogBytes,
		&job.Profile.AutoRollback, &job.Profile.CleanupWorkspace,
		&job.Profile.KeepFailedWorkspace,
	)
	if err != nil {
		return fmt.Errorf("load enabled immutable deployment profile: %w", err)
	}
	job.Profile.CommandTimeout = time.Duration(commandTimeoutSeconds) * time.Second
	job.Profile.MaxLogBytes = maxLogBytes
	if err := json.Unmarshal(healthJSON, &job.Profile.HealthChecks); err != nil {
		return fmt.Errorf("decode health checks: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT s.id::text, s.name, s.version, ps.phase, s.interpreter_path,
		       s.sha256, convert_to(s.content, 'UTF8'), ps.args,
		       COALESCE(ps.timeout_seconds, s.timeout_seconds, $2), s.approved_at
		FROM deployment_profile_scripts ps
		JOIN script_versions s ON s.id = ps.script_version_id
		WHERE ps.profile_id = $1::uuid
		  AND s.approved_at IS NOT NULL AND s.revoked_at IS NULL
		ORDER BY ps.execution_order ASC`, job.Profile.ID, commandTimeoutSeconds)
	if err != nil {
		return fmt.Errorf("load approved scripts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var script model.Script
		var timeoutSeconds int64
		if err := rows.Scan(&script.ID, &script.Name, &script.Version, &script.Phase,
			&script.InterpreterPath, &script.SHA256, &script.Content,
			&scriptArgsJSON, &timeoutSeconds, &script.ApprovedAt); err != nil {
			return fmt.Errorf("scan approved script: %w", err)
		}
		if err := json.Unmarshal(scriptArgsJSON, &script.Args); err != nil {
			return fmt.Errorf("decode args for script %s: %w", script.ID, err)
		}
		script.Timeout = time.Duration(timeoutSeconds) * time.Second
		job.Profile.Scripts = append(job.Profile.Scripts, script)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate approved scripts: %w", err)
	}
	return nil
}

func (s *PGStore) loadRollbackImages(ctx context.Context, tx pgx.Tx, job *model.Job) error {
	if job.RollbackSourceReleaseID == "" || job.RollbackSourceJobID == "" {
		return errors.New("manual rollback job has no immutable source release and DEPLOY basis job")
	}
	rows, err := tx.Query(ctx, `
		SELECT i.file_path, i.source_ref, i.destination_ref,
		       i.repository, i.tag, i.digest
		FROM release_jobs source_job
		JOIN release_images i ON i.job_id = source_job.id
		WHERE source_job.id = $1::uuid
		  AND source_job.release_id = $2::uuid
		  AND source_job.operation = 'DEPLOY' AND source_job.status = 'SUCCESS'
		ORDER BY i.file_path`, job.RollbackSourceJobID, job.RollbackSourceReleaseID)
	if err != nil {
		return fmt.Errorf("load rollback source images: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var image model.ImageRecord
		var digest *string
		if err := rows.Scan(&image.FilePath, &image.SourceRef, &image.DestinationRef,
			&image.Repository, &image.Tag, &digest); err != nil {
			return fmt.Errorf("scan rollback source image: %w", err)
		}
		if digest != nil {
			image.Digest = *digest
		}
		job.RollbackImages = append(job.RollbackImages, image)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rollback source images: %w", err)
	}
	if len(job.RollbackImages) == 0 {
		return errors.New("rollback source has no successful deployment image records")
	}
	return nil
}

func validateJob(job *model.Job) error {
	p := job.Profile
	if p.AutoRollback {
		return errors.New("automatic rollback is disabled in v0.1; use an explicit digest-verified manual rollback")
	}
	if job.Operation != model.OperationDeploy && job.Operation != model.OperationRollback {
		return fmt.Errorf("unsupported job operation %q", job.Operation)
	}
	if p.Extraction.MaxImages <= 0 || (job.Operation == model.OperationDeploy &&
		(p.Extraction.MaxArchiveBytes <= 0 || p.Extraction.MaxExtractedBytes <= 0 || p.Extraction.MaxFiles <= 0)) {
		return errors.New("deployment profile has invalid extraction limits")
	}
	if p.CommandTimeout <= 0 || p.MaxLogBytes <= 0 {
		return errors.New("deployment profile has invalid command limits")
	}
	if p.Runtime.Kind != "docker" && p.Runtime.Kind != "podman" && p.Runtime.Kind != "containerd" {
		return fmt.Errorf("unsupported runtime kind %q", p.Runtime.Kind)
	}
	if p.Runtime.BinaryPath == "" || p.Runtime.RegistryHost == "" || p.Runtime.RegistryProject == "" || p.Runtime.RegistryURL == "" {
		return errors.New("runtime binary and registry settings are required")
	}
	credential := job.TargetCredential
	if credential.ID == "" {
		if credential.Type != "" || credential.Version != 0 || credential.Ciphertext != "" || credential.AAD != "" {
			return errors.New("target credential snapshot is incomplete")
		}
	} else if credential.Version <= 0 || credential.Ciphertext == "" || credential.AAD == "" || !allowedTargetCredentialType(credential.Type) {
		return errors.New("target credential snapshot is invalid")
	}
	requiredPhase := "DEPLOY"
	if job.Operation == model.OperationRollback {
		requiredPhase = "ROLLBACK"
	}
	if !hasScriptPhase(p.Scripts, requiredPhase) {
		return fmt.Errorf("operation %s requires an approved %s script", job.Operation, requiredPhase)
	}
	if job.Operation == model.OperationRollback {
		if job.RollbackSourceReleaseID == "" || job.RollbackSourceJobID == "" {
			return errors.New("manual rollback requires an immutable source release and DEPLOY basis job")
		}
		if len(job.RollbackImages) == 0 || len(job.RollbackImages) > p.Extraction.MaxImages {
			return fmt.Errorf("manual rollback source image count must be between 1 and %d", p.Extraction.MaxImages)
		}
		for _, image := range job.RollbackImages {
			if image.FilePath == "" || image.DestinationRef == "" || image.Repository == "" || image.Tag == "" || image.Digest == "" {
				return errors.New("manual rollback source contains an incomplete image record")
			}
		}
	}
	return nil
}

func allowedTargetCredentialType(value string) bool {
	switch value {
	case "SSH_PRIVATE_KEY", "KUBECONFIG", "TOKEN", "OPAQUE_FILE":
		return true
	default:
		return false
	}
}

func hasScriptPhase(scripts []model.Script, phase string) bool {
	for _, script := range scripts {
		if script.Phase == phase {
			return true
		}
	}
	return false
}

func (s *PGStore) Heartbeat(ctx context.Context, jobID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `
		UPDATE release_jobs SET heartbeat_at = clock_timestamp()
		WHERE id = $1::uuid AND locked_by = $2
		  AND status NOT IN ('SUCCESS', 'FAILED', 'ROLLED_BACK')`, jobID, s.workerID)
	if err != nil {
		return fmt.Errorf("heartbeat job: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrLostLease
	}
	result, err = tx.Exec(ctx, `
		UPDATE release_locks SET heartbeat_at = clock_timestamp()
		WHERE job_id = $1::uuid AND worker_id = $2`, jobID, s.workerID)
	if err != nil {
		return fmt.Errorf("heartbeat release lock: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrLostLease
	}
	if s.runnerIdentity != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE runner_instances
			SET last_heartbeat_at = clock_timestamp(), updated_at = clock_timestamp()
			WHERE worker_id = $1 AND managed_by_runner = TRUE`, s.runnerIdentity); err != nil {
			return fmt.Errorf("heartbeat runner inventory: %w", err)
		}
	}
	return tx.Commit(ctx)
}

func (s *PGStore) BeginStep(ctx context.Context, jobID string, status model.JobStatus) (model.Step, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Step{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `
		UPDATE release_jobs SET status = $2, updated_at = clock_timestamp()
		WHERE id = $1::uuid AND locked_by = $3
		  AND status NOT IN ('SUCCESS', 'FAILED', 'ROLLED_BACK')`, jobID, status, s.workerID)
	if err != nil {
		return model.Step{}, fmt.Errorf("set job state %s: %w", status, err)
	}
	if result.RowsAffected() != 1 {
		return model.Step{}, ErrLostLease
	}
	if _, err := tx.Exec(ctx, `
		UPDATE releases r
		SET status = $2, updated_at = clock_timestamp()
		FROM release_jobs j
		WHERE j.id = $1::uuid AND r.id = j.release_id`, jobID, status); err != nil {
		return model.Step{}, fmt.Errorf("synchronize release state %s: %w", status, err)
	}
	var step model.Step
	step.JobID = jobID
	step.Name = status
	err = tx.QueryRow(ctx, `
		INSERT INTO release_job_steps (job_id, attempt, step_order, name, status, started_at)
		SELECT j.id, j.attempts,
		       COALESCE((SELECT MAX(s.step_order) + 1 FROM release_job_steps s WHERE s.job_id = j.id), 1),
		       $2, 'RUNNING', clock_timestamp()
		FROM release_jobs j WHERE j.id = $1::uuid
		RETURNING id, step_order`, jobID, status).Scan(&step.ID, &step.Number)
	if err != nil {
		return model.Step{}, fmt.Errorf("insert job step %s: %w", status, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Step{}, err
	}
	return step, nil
}

func (s *PGStore) FinishStep(ctx context.Context, step model.Step, success bool, result model.StepResult) error {
	status := "SUCCESS"
	if !success {
		status = "FAILED"
	}
	err := executeLeaseMutation(ctx, s.pool, "finish job step "+string(step.Name), finishStepSQL,
		step.ID, status, result.ExitCode, result.Message, nullableJSON(result.Metadata), step.JobID, s.workerID)
	return err
}

const finishStepSQL = `
		UPDATE release_job_steps s
		SET status = $2, exit_code = $3, message = NULLIF($4, ''),
		    metadata = COALESCE($5::jsonb, '{}'::jsonb), finished_at = clock_timestamp()
		FROM release_jobs j
		WHERE s.id = $1 AND s.job_id = $6::uuid AND j.id = s.job_id
		  AND j.locked_by = $7
		  AND j.status NOT IN ('SUCCESS', 'FAILED', 'ROLLED_BACK')
		  AND EXISTS (
		      SELECT 1 FROM release_locks l
		      WHERE l.job_id = j.id AND l.worker_id = $7
		  )`

func (s *PGStore) AppendLog(ctx context.Context, jobID string, stepID int64, stream string, sequence int64, payload []byte) error {
	return executeLeaseMutation(ctx, s.pool, "append job log", appendLogSQL,
		jobID, stepID, stream, sequence, payload, s.workerID)
}

const appendLogSQL = `
		INSERT INTO release_job_logs (job_id, step_id, stream, sequence, payload, created_at)
		SELECT j.id, $2, $3, $4, $5, clock_timestamp()
		FROM release_jobs j
		WHERE j.id = $1::uuid AND j.locked_by = $6
		  AND j.status NOT IN ('SUCCESS', 'FAILED', 'ROLLED_BACK')
		  AND EXISTS (
		      SELECT 1 FROM release_locks l
		      WHERE l.job_id = j.id AND l.worker_id = $6
		  )
		  AND EXISTS (
		      SELECT 1 FROM release_job_steps s
		      WHERE s.id = $2 AND s.job_id = j.id
		  )`

func (s *PGStore) RecordImage(ctx context.Context, jobID string, image model.ImageRecord) error {
	return executeLeaseMutation(ctx, s.pool, "record release image", recordImageSQL,
		jobID, image.FilePath, image.SourceRef, image.DestinationRef,
		image.Repository, image.Tag, image.Digest, s.workerID)
}

const recordImageSQL = `
		INSERT INTO release_images
		    (job_id, file_path, source_ref, destination_ref, repository, tag, digest, created_at)
		SELECT j.id, $2, $3, $4, $5, $6, NULLIF($7, ''), clock_timestamp()
		FROM release_jobs j
		WHERE j.id = $1::uuid AND j.locked_by = $8
		  AND j.status NOT IN ('SUCCESS', 'FAILED', 'ROLLED_BACK')
		  AND EXISTS (
		      SELECT 1 FROM release_locks l
		      WHERE l.job_id = j.id AND l.worker_id = $8
		  )
		ON CONFLICT (job_id, file_path) DO UPDATE SET
		    source_ref = EXCLUDED.source_ref,
		    destination_ref = EXCLUDED.destination_ref,
		    repository = EXCLUDED.repository, tag = EXCLUDED.tag,
		    digest = COALESCE(EXCLUDED.digest, release_images.digest)`

type leaseMutationExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func executeLeaseMutation(ctx context.Context, executor leaseMutationExecutor, action, statement string, arguments ...any) error {
	commandTag, err := executor.Exec(ctx, statement, arguments...)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if commandTag.RowsAffected() != 1 {
		return ErrLostLease
	}
	return nil
}

func (s *PGStore) FinishJob(ctx context.Context, jobID string, status model.JobStatus, failure string) error {
	if status != model.StatusSuccess && status != model.StatusFailed && status != model.StatusRolledBack {
		return fmt.Errorf("invalid terminal status %q", status)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `
		UPDATE release_jobs
		SET status = $2, failure_message = NULLIF($3, ''),
		    finished_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE id = $1::uuid AND locked_by = $4
		  AND status NOT IN ('SUCCESS', 'FAILED', 'ROLLED_BACK')
		  AND EXISTS (
		      SELECT 1 FROM release_locks l
		      WHERE l.job_id = release_jobs.id AND l.worker_id = $4
		  )`, jobID, status, failure, s.workerID)
	if err != nil {
		return fmt.Errorf("finish job: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrLostLease
	}
	if _, err := tx.Exec(ctx, `
		UPDATE releases r
		SET status = $2, updated_at = clock_timestamp()
		FROM release_jobs j
		WHERE j.id = $1::uuid AND r.id = j.release_id`, jobID, status); err != nil {
		return fmt.Errorf("synchronize release terminal state: %w", err)
	}
	if status == model.StatusSuccess || status == model.StatusRolledBack {
		if err := executeLeaseMutation(ctx, tx, "synchronize deployment head", syncDeploymentHeadSQL, jobID, status); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM release_locks WHERE job_id = $1::uuid AND worker_id = $2`, jobID, s.workerID); err != nil {
		return fmt.Errorf("release deployment lock: %w", err)
	}
	return tx.Commit(ctx)
}

// current_job_id is the successful DEPLOY job that supplied the currently
// deployed image set. A manual rollback remains independently auditable as its
// own release_jobs row, while the head returns to the source release's latest
// successful DEPLOY basis job. The release lock serializes target changes.
const syncDeploymentHeadSQL = `
		WITH terminal_job AS (
		    SELECT j.id, j.release_id, j.rollback_source_release_id, j.rollback_source_job_id,
		           j.operation, r.application_id, r.environment_id
		    FROM release_jobs j
		    JOIN releases r ON r.id = j.release_id
		    WHERE j.id = $1::uuid
		      AND (($2 = 'SUCCESS' AND j.operation = 'DEPLOY')
		        OR ($2 = 'ROLLED_BACK' AND j.operation = 'ROLLBACK'))
		), deployment_basis AS (
		    SELECT t.application_id, t.environment_id, t.release_id AS current_release_id,
		           t.id AS current_job_id
		    FROM terminal_job t
		    WHERE t.operation = 'DEPLOY'
		    UNION ALL
		    SELECT t.application_id, t.environment_id,
		           t.rollback_source_release_id AS current_release_id,
		           source_job.id AS current_job_id
		    FROM terminal_job t
		    JOIN release_jobs source_job
		      ON source_job.id = t.rollback_source_job_id
		     AND source_job.release_id = t.rollback_source_release_id
		     AND source_job.operation = 'DEPLOY' AND source_job.status = 'SUCCESS'
		    WHERE t.operation = 'ROLLBACK'
		)
		INSERT INTO deployment_heads
		    (application_id, environment_id, current_release_id, current_job_id, updated_at)
		SELECT application_id, environment_id, current_release_id, current_job_id, clock_timestamp()
		FROM deployment_basis
		ON CONFLICT (application_id, environment_id) DO UPDATE SET
		    current_release_id = EXCLUDED.current_release_id,
		    current_job_id = EXCLUDED.current_job_id,
		    updated_at = clock_timestamp()`

func durationInterval(value time.Duration) string {
	return fmt.Sprintf("%d milliseconds", value.Milliseconds())
}

func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func randomUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate runner id: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
