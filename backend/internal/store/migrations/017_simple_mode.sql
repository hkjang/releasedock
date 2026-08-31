-- Simple mode: upload a package to an administrator-registered directory and
-- run one administrator-registered command. Deliberately independent of
-- releases/release_jobs/deployment_profiles: no image load, tag, registry
-- push, approval, health check, or version comparison happens on this path.

-- Singleton settings, following the oidc_settings/ai_settings/runner_settings
-- precedent. Deliberately not app_settings.general_config: the general
-- settings endpoint replaces that JSONB wholesale and would drop these keys.
CREATE TABLE IF NOT EXISTS simple_settings (
    id TEXT PRIMARY KEY DEFAULT 'default',
    -- Which UI users land in by default. Administrators can still switch.
    default_ui_mode TEXT NOT NULL DEFAULT 'full' CHECK (default_ui_mode IN ('simple','full')),
    -- PER_TARGET: every simple target carries its own command.
    -- SHARED: one common command runs for every target.
    command_mode TEXT NOT NULL DEFAULT 'PER_TARGET' CHECK (command_mode IN ('PER_TARGET','SHARED')),
    shared_command_path TEXT NOT NULL DEFAULT '',
    shared_command_args TEXT[] NOT NULL DEFAULT '{}',
    shared_working_dir TEXT NOT NULL DEFAULT '',
    shared_timeout_seconds INTEGER NOT NULL DEFAULT 600
        CHECK (shared_timeout_seconds BETWEEN 1 AND 86400),
    upload_root TEXT NOT NULL DEFAULT '/var/lib/releasedock/simple'
        CHECK (upload_root LIKE '/%'),
    updated_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- SHARED mode is unusable without a command; the API enforces the
    -- converse (every active target has a command) for PER_TARGET.
    CHECK (command_mode <> 'SHARED' OR shared_command_path LIKE '/%')
);
INSERT INTO simple_settings(id) VALUES ('default') ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS simple_targets (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    description TEXT NOT NULL DEFAULT '',
    upload_dir TEXT NOT NULL CHECK (upload_dir LIKE '/%'),
    -- The four command columns apply only to PER_TARGET command mode. They are
    -- kept (not cleared) while SHARED mode is active so switching back is
    -- lossless, which is why they are nullable.
    command_path TEXT CHECK (command_path IS NULL OR command_path LIKE '/%'),
    command_args TEXT[] NOT NULL DEFAULT '{}',
    working_dir TEXT CHECK (working_dir IS NULL OR working_dir LIKE '/%'),
    timeout_seconds INTEGER CHECK (timeout_seconds IS NULL OR timeout_seconds BETWEEN 1 AND 86400),
    max_upload_bytes BIGINT NOT NULL DEFAULT 10737418240 CHECK (max_upload_bytes > 0),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_by TEXT NOT NULL REFERENCES users(id),
    updated_by TEXT REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS simple_targets_active_name_idx
    ON simple_targets(lower(btrim(name))) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS simple_targets_active_dir_idx
    ON simple_targets(upload_dir) WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS simple_runs (
    id UUID PRIMARY KEY,
    target_id UUID NOT NULL REFERENCES simple_targets(id),
    actor_id TEXT NOT NULL REFERENCES users(id),
    original_filename TEXT NOT NULL,
    stored_path TEXT NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    sha256 TEXT NOT NULL,
    -- Snapshot of the command actually used, so later settings changes never
    -- rewrite history.
    command_source TEXT NOT NULL CHECK (command_source IN ('PER_TARGET','SHARED')),
    resolved_command_path TEXT NOT NULL,
    resolved_command_args TEXT[] NOT NULL DEFAULT '{}',
    resolved_timeout_seconds INTEGER NOT NULL CHECK (resolved_timeout_seconds BETWEEN 1 AND 86400),
    status TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING','RUNNING','SUCCESS','FAILED','TIMEOUT')),
    exit_code INTEGER,
    error TEXT NOT NULL DEFAULT '',
    log_bytes BIGINT NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One in-flight run per target. Mirrors the release_jobs active-job pattern.
CREATE UNIQUE INDEX IF NOT EXISTS simple_runs_active_target_idx
    ON simple_runs(target_id) WHERE status IN ('PENDING','RUNNING');
CREATE INDEX IF NOT EXISTS simple_runs_created_idx ON simple_runs(created_at DESC);
CREATE INDEX IF NOT EXISTS simple_runs_actor_idx ON simple_runs(actor_id, created_at DESC);

CREATE TABLE IF NOT EXISTS simple_run_logs (
    id BIGSERIAL PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES simple_runs(id) ON DELETE CASCADE,
    stream TEXT NOT NULL CHECK (stream IN ('stdout','stderr','system')),
    payload BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS simple_run_logs_run_idx ON simple_run_logs(run_id, id);

-- A terminal run is immutable: the executor writes the outcome exactly once.
CREATE OR REPLACE FUNCTION guard_simple_run_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status IN ('SUCCESS','FAILED','TIMEOUT') THEN
        RAISE EXCEPTION 'simple run % is already terminal', OLD.id;
    END IF;
    IF NEW.target_id IS DISTINCT FROM OLD.target_id
       OR NEW.actor_id IS DISTINCT FROM OLD.actor_id
       OR NEW.stored_path IS DISTINCT FROM OLD.stored_path
       OR NEW.sha256 IS DISTINCT FROM OLD.sha256
       OR NEW.command_source IS DISTINCT FROM OLD.command_source
       OR NEW.resolved_command_path IS DISTINCT FROM OLD.resolved_command_path
       OR NEW.resolved_command_args IS DISTINCT FROM OLD.resolved_command_args THEN
        RAISE EXCEPTION 'simple run provenance is immutable';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS simple_run_transition_guard ON simple_runs;
CREATE TRIGGER simple_run_transition_guard
BEFORE UPDATE ON simple_runs
FOR EACH ROW EXECUTE FUNCTION guard_simple_run_transition();

INSERT INTO permissions(code,description) VALUES
    ('simple.deploy', 'Upload a package and run the configured simple-mode command'),
    ('simple.read', 'Read simple-mode targets and run history'),
    ('admin.simple.read', 'Read simple-mode targets and command settings'),
    ('admin.simple.write', 'Create, change, and revoke simple-mode targets and command settings')
ON CONFLICT (code) DO UPDATE SET description=EXCLUDED.description;

INSERT INTO role_permissions(role_id,permission_code)
SELECT 'role-admin',code FROM permissions
WHERE code IN ('simple.deploy','simple.read','admin.simple.read','admin.simple.write')
ON CONFLICT DO NOTHING;

INSERT INTO role_permissions(role_id,permission_code)
SELECT role_id,code FROM (VALUES ('role-operator'),('role-developer')) AS r(role_id)
CROSS JOIN (VALUES ('simple.deploy'),('simple.read')) AS p(code)
ON CONFLICT DO NOTHING;

INSERT INTO role_permissions(role_id,permission_code)
VALUES ('role-viewer','simple.read')
ON CONFLICT DO NOTHING;
