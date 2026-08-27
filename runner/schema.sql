-- ReleaseDock Release Runner schema contract.
-- Apply after the API's base schema. Domain and foreign-key IDs are UUID;
-- user/role IDs and runner worker identifiers remain TEXT in the base schema.

CREATE TABLE IF NOT EXISTS runner_settings (
    singleton                 BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    poll_interval_ms          INTEGER NOT NULL DEFAULT 2000 CHECK (poll_interval_ms > 0),
    lock_retry_ms             INTEGER NOT NULL DEFAULT 5000 CHECK (lock_retry_ms > 0),
    settings_refresh_ms       INTEGER NOT NULL DEFAULT 30000 CHECK (settings_refresh_ms > 0),
    heartbeat_interval_ms     INTEGER NOT NULL DEFAULT 5000 CHECK (heartbeat_interval_ms > 0),
    stale_job_after_ms        INTEGER NOT NULL DEFAULT 60000 CHECK (stale_job_after_ms > 0),
    workspace_root            TEXT NOT NULL DEFAULT '/var/lib/releasedock/workspaces',
    command_path              TEXT NOT NULL DEFAULT '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
    log_chunk_bytes           INTEGER NOT NULL DEFAULT 16384 CHECK (log_chunk_bytes BETWEEN 1024 AND 1048576),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

INSERT INTO runner_settings (singleton) VALUES (TRUE)
ON CONFLICT (singleton) DO NOTHING;

ALTER TABLE deployment_profiles
    ADD COLUMN IF NOT EXISTS max_archive_bytes BIGINT NOT NULL DEFAULT 10737418240,
    ADD COLUMN IF NOT EXISTS max_extracted_bytes BIGINT NOT NULL DEFAULT 21474836480,
    ADD COLUMN IF NOT EXISTS max_archive_files INTEGER NOT NULL DEFAULT 10000,
    ADD COLUMN IF NOT EXISTS max_images INTEGER NOT NULL DEFAULT 100,
    ADD COLUMN IF NOT EXISTS allow_symlinks BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS runtime_kind TEXT NOT NULL DEFAULT 'docker',
    ADD COLUMN IF NOT EXISTS runtime_binary_path TEXT NOT NULL DEFAULT '/usr/bin/docker',
    ADD COLUMN IF NOT EXISTS containerd_namespace TEXT NOT NULL DEFAULT 'default',
    ADD COLUMN IF NOT EXISTS registry_url TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS registry_host TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS registry_project TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS registry_insecure BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS registry_ca_pem TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS registry_credential_id UUID,
    ADD COLUMN IF NOT EXISTS target_credential_id UUID,
    ADD COLUMN IF NOT EXISTS health_checks JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS command_timeout_seconds BIGINT NOT NULL DEFAULT 600,
    ADD COLUMN IF NOT EXISTS max_log_bytes BIGINT NOT NULL DEFAULT 52428800,
    ADD COLUMN IF NOT EXISTS auto_rollback BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS cleanup_workspace BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS keep_failed_workspace BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS runner_labels TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;

-- v0.1 supports only the explicit rollback Job, which freezes and verifies a
-- previous successful deployment's image digests. Reusing the failed DEPLOY
-- Job context for automatic rollback is intentionally forbidden.
UPDATE deployment_profiles SET auto_rollback = FALSE WHERE auto_rollback = TRUE;
ALTER TABLE deployment_profiles
    DROP CONSTRAINT IF EXISTS deployment_profiles_auto_rollback_disabled;
ALTER TABLE deployment_profiles
    ADD CONSTRAINT deployment_profiles_auto_rollback_disabled
    CHECK (auto_rollback = FALSE);

ALTER TABLE release_jobs
    ADD COLUMN IF NOT EXISTS application TEXT,
    ADD COLUMN IF NOT EXISTS version TEXT,
    ADD COLUMN IF NOT EXISTS environment TEXT,
    ADD COLUMN IF NOT EXISTS lock_key TEXT,
    ADD COLUMN IF NOT EXISTS artifact_path TEXT,
    ADD COLUMN IF NOT EXISTS expected_sha256 TEXT,
    ADD COLUMN IF NOT EXISTS manifest_path TEXT NOT NULL DEFAULT 'manifest.yaml',
    ADD COLUMN IF NOT EXISTS operation TEXT NOT NULL DEFAULT 'DEPLOY' CHECK (operation IN ('DEPLOY', 'ROLLBACK')),
    ADD COLUMN IF NOT EXISTS rollback_source_release_id UUID,
    ADD COLUMN IF NOT EXISTS rollback_source_job_id UUID,
    ADD COLUMN IF NOT EXISTS retry_of_job_id UUID,
    ADD COLUMN IF NOT EXISTS target_credential_id UUID,
    ADD COLUMN IF NOT EXISTS target_credential_version INTEGER,
    ADD COLUMN IF NOT EXISTS profile_id UUID,
    ADD COLUMN IF NOT EXISTS priority INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS heartbeat_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS runner_labels TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS failure_message TEXT;

ALTER TABLE releases
    ADD COLUMN IF NOT EXISTS rollback_source_job_id UUID,
    ADD COLUMN IF NOT EXISTS retry_source_job_id UUID;

-- DEPLOY jobs use rollback_source_release_id/rollback_source_job_id as the
-- immutable prior verified head snapshot (NULL only for the first deployment).
-- ROLLBACK jobs use both as their explicit target. The API migrations enforce the corresponding insert
-- invariants and synchronize this head on verified terminal transitions.
CREATE TABLE IF NOT EXISTS deployment_heads (
    application_id     UUID NOT NULL REFERENCES applications(id),
    environment_id     UUID NOT NULL REFERENCES environments(id),
    current_release_id UUID NOT NULL REFERENCES releases(id),
    current_job_id     UUID NOT NULL UNIQUE REFERENCES release_jobs(id),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (application_id, environment_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS release_jobs_active_target_idx
ON release_jobs (lock_key)
WHERE status NOT IN ('SUCCESS', 'FAILED', 'ROLLED_BACK');

CREATE TABLE IF NOT EXISTS target_credentials (
    id              UUID PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    credential_type TEXT NOT NULL CHECK (credential_type IN ('SSH_PRIVATE_KEY', 'KUBECONFIG', 'TOKEN', 'OPAQUE_FILE')),
    version         INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    ciphertext      TEXT NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    approved_at     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    approved_by     TEXT REFERENCES users(id) ON DELETE SET NULL,
    revoked_at      TIMESTAMPTZ,
    created_by      TEXT NOT NULL REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS runner_credentials (
    id              UUID PRIMARY KEY,
    name            TEXT NOT NULL,
    version         INTEGER NOT NULL CHECK (version > 0),
    ciphertext      TEXT NOT NULL,
    approved_at     TIMESTAMPTZ,
    approved_by     TEXT,
    revoked_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (name, version)
);

CREATE TABLE IF NOT EXISTS script_versions (
    id                  UUID PRIMARY KEY,
    name                TEXT NOT NULL,
    version             INTEGER NOT NULL CHECK (version > 0),
    interpreter_path    TEXT NOT NULL,
    sha256              CHAR(64) NOT NULL,
    content             TEXT NOT NULL,
    approved_at         TIMESTAMPTZ,
    approved_by         TEXT,
    revoked_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (name, version)
);

CREATE TABLE IF NOT EXISTS deployment_profile_scripts (
    profile_id          UUID NOT NULL REFERENCES deployment_profiles(id) ON DELETE CASCADE,
    phase               TEXT NOT NULL CHECK (phase IN ('PRE_DEPLOY', 'DEPLOY', 'POST_DEPLOY', 'ROLLBACK')),
    execution_order     INTEGER NOT NULL CHECK (execution_order >= 0),
    script_version_id   UUID NOT NULL REFERENCES script_versions(id),
    args                JSONB NOT NULL DEFAULT '[]'::jsonb,
    timeout_seconds     BIGINT CHECK (timeout_seconds > 0),
    PRIMARY KEY (profile_id, phase, execution_order)
);

CREATE TABLE IF NOT EXISTS release_locks (
    lock_key        TEXT PRIMARY KEY,
    job_id          UUID NOT NULL UNIQUE REFERENCES release_jobs(id) ON DELETE CASCADE,
    worker_id       TEXT NOT NULL,
    acquired_at     TIMESTAMPTZ NOT NULL,
    heartbeat_at    TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS release_job_steps (
    id              BIGSERIAL PRIMARY KEY,
    job_id          UUID NOT NULL REFERENCES release_jobs(id) ON DELETE CASCADE,
    attempt         INTEGER NOT NULL,
    step_order      INTEGER NOT NULL,
    name            TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('RUNNING', 'SUCCESS', 'FAILED')),
    exit_code       INTEGER,
    message         TEXT,
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    started_at      TIMESTAMPTZ NOT NULL,
    finished_at     TIMESTAMPTZ,
    UNIQUE (job_id, step_order)
);

CREATE TABLE IF NOT EXISTS release_job_logs (
    id              BIGSERIAL PRIMARY KEY,
    job_id          UUID NOT NULL REFERENCES release_jobs(id) ON DELETE CASCADE,
    step_id         BIGINT NOT NULL REFERENCES release_job_steps(id) ON DELETE CASCADE,
    stream          TEXT NOT NULL CHECK (stream IN ('stdout', 'stderr', 'system')),
    sequence        BIGINT NOT NULL,
    payload         BYTEA NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    UNIQUE (step_id, stream, sequence)
);

CREATE TABLE IF NOT EXISTS release_images (
    id                  BIGSERIAL PRIMARY KEY,
    job_id              UUID NOT NULL REFERENCES release_jobs(id) ON DELETE CASCADE,
    file_path           TEXT NOT NULL,
    source_ref          TEXT NOT NULL,
    destination_ref     TEXT NOT NULL,
    repository          TEXT NOT NULL,
    tag                 TEXT NOT NULL,
    digest              TEXT,
    created_at          TIMESTAMPTZ NOT NULL,
    UNIQUE (job_id, file_path)
);

CREATE INDEX IF NOT EXISTS release_jobs_runner_queue_idx
    ON release_jobs (priority DESC, created_at ASC)
    WHERE status = 'QUEUED';
CREATE INDEX IF NOT EXISTS release_job_logs_stream_idx
    ON release_job_logs (job_id, id);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'deployment_profiles_runner_credential_fk'
    ) THEN
        ALTER TABLE deployment_profiles
            ADD CONSTRAINT deployment_profiles_runner_credential_fk
            FOREIGN KEY (registry_credential_id) REFERENCES runner_credentials(id);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'deployment_profiles_target_credential_fk'
    ) THEN
        ALTER TABLE deployment_profiles
            ADD CONSTRAINT deployment_profiles_target_credential_fk
            FOREIGN KEY (target_credential_id) REFERENCES target_credentials(id) ON DELETE SET NULL;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'release_jobs_target_credential_fk'
    ) THEN
        ALTER TABLE release_jobs
            ADD CONSTRAINT release_jobs_target_credential_fk
            FOREIGN KEY (target_credential_id) REFERENCES target_credentials(id);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'release_jobs_rollback_source_job_fk'
    ) THEN
        ALTER TABLE release_jobs
            ADD CONSTRAINT release_jobs_rollback_source_job_fk
            FOREIGN KEY (rollback_source_job_id) REFERENCES release_jobs(id);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'release_jobs_retry_of_job_fk'
    ) THEN
        ALTER TABLE release_jobs
            ADD CONSTRAINT release_jobs_retry_of_job_fk
            FOREIGN KEY (retry_of_job_id) REFERENCES release_jobs(id);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'releases_retry_source_job_fk'
    ) THEN
        ALTER TABLE releases
            ADD CONSTRAINT releases_retry_source_job_fk
            FOREIGN KEY (retry_source_job_id) REFERENCES release_jobs(id);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'release_jobs_runner_profile_fk'
    ) THEN
        ALTER TABLE release_jobs
            ADD CONSTRAINT release_jobs_runner_profile_fk
            FOREIGN KEY (profile_id) REFERENCES deployment_profiles(id);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'deployment_profiles_runner_limits_check'
    ) THEN
        ALTER TABLE deployment_profiles
            ADD CONSTRAINT deployment_profiles_runner_limits_check CHECK (
                max_archive_bytes > 0 AND max_extracted_bytes > 0
                AND max_archive_files > 0 AND max_images > 0
                AND command_timeout_seconds > 0 AND max_log_bytes > 0
                AND runtime_kind IN ('docker', 'podman', 'containerd')
            );
    END IF;
END
$$;

-- The API/base schema must allow these release_jobs.status values:
-- QUEUED, VALIDATING, PRE_CHECK, EXTRACTING, IMAGE_INSPECT, IMAGE_LOAD, IMAGE_TAG,
-- IMAGE_PUSH, DEPLOYING, VERIFYING, ROLLBACK, ROLLED_BACK, SUCCESS, FAILED.
-- app_settings(id='default').artifact_storage_path is the single artifact-root
-- setting used by both the API upload service and this runner.
