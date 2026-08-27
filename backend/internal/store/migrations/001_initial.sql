CREATE TABLE IF NOT EXISTS schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL,
    password_hash TEXT,
    display_name TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT '',
    auth_source TEXT NOT NULL DEFAULT 'local' CHECK (auth_source IN ('local','oidc')),
    oidc_subject TEXT UNIQUE,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    preferences JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (username)
);

CREATE TABLE IF NOT EXISTS roles (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    system BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS permissions (
    code TEXT PRIMARY KEY,
    description TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS role_permissions (
    role_id TEXT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    permission_code TEXT NOT NULL REFERENCES permissions(code) ON DELETE CASCADE,
    PRIMARY KEY (role_id, permission_code)
);

CREATE TABLE IF NOT EXISTS user_roles (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_id TEXT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, role_id)
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash BYTEA PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_hash BYTEA NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ip TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions(user_id);
CREATE INDEX IF NOT EXISTS sessions_expiry_idx ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS oidc_states (
    state_hash BYTEA PRIMARY KEY,
    nonce TEXT NOT NULL,
    code_verifier TEXT NOT NULL,
    return_to TEXT NOT NULL DEFAULT '/',
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS oidc_states_expiry_idx ON oidc_states(expires_at);

CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    prefix TEXT NOT NULL UNIQUE,
    secret_hash BYTEA NOT NULL UNIQUE,
    scopes TEXT[] NOT NULL DEFAULT '{}',
    expires_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS api_keys_user_idx ON api_keys(user_id);

CREATE TABLE IF NOT EXISTS app_settings (
    id TEXT PRIMARY KEY DEFAULT 'default',
    service_name TEXT NOT NULL DEFAULT 'ReleaseDock',
    approval_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    artifact_storage_path TEXT NOT NULL DEFAULT '/var/lib/releasedock/artifacts',
    artifact_max_bytes BIGINT NOT NULL DEFAULT 10737418240,
    allowed_origins TEXT[] NOT NULL DEFAULT '{}',
    general_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    approval_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    storage_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (artifact_max_bytes > 0)
);
INSERT INTO app_settings(id) VALUES ('default') ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS oidc_settings (
    id TEXT PRIMARY KEY DEFAULT 'default',
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    issuer TEXT NOT NULL DEFAULT '',
    client_id TEXT NOT NULL DEFAULT '',
    client_secret_enc TEXT NOT NULL DEFAULT '',
    redirect_url TEXT NOT NULL DEFAULT '',
    scopes TEXT[] NOT NULL DEFAULT ARRAY['openid','profile','email'],
    auto_create_user BOOLEAN NOT NULL DEFAULT FALSE,
    default_role_id TEXT REFERENCES roles(id) ON DELETE SET NULL,
    config JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO oidc_settings(id) VALUES ('default') ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS ai_settings (
    id TEXT PRIMARY KEY DEFAULT 'default',
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    endpoint TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    api_key_enc TEXT NOT NULL DEFAULT '',
    max_tokens INTEGER NOT NULL DEFAULT 4096 CHECK (max_tokens > 0 AND max_tokens <= 262144),
    config JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO ai_settings(id) VALUES ('default') ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS applications (
    id UUID PRIMARY KEY,
    code TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_by TEXT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS environments (
    id UUID PRIMARY KEY,
    application_id UUID NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    code TEXT NOT NULL,
    name TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT 'DEV',
    protected BOOLEAN NOT NULL DEFAULT FALSE,
    description TEXT NOT NULL DEFAULT '',
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_by TEXT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(application_id, code)
);

CREATE TABLE IF NOT EXISTS deployment_profiles (
    id UUID PRIMARY KEY,
    application_id UUID NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    environment_id UUID NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    approval_required BOOLEAN NOT NULL DEFAULT FALSE,
    config JSONB NOT NULL DEFAULT '{}'::jsonb,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    max_archive_bytes BIGINT NOT NULL DEFAULT 10737418240,
    max_extracted_bytes BIGINT NOT NULL DEFAULT 21474836480,
    max_archive_files INTEGER NOT NULL DEFAULT 10000,
    max_images INTEGER NOT NULL DEFAULT 100,
    allow_symlinks BOOLEAN NOT NULL DEFAULT FALSE,
    runtime_kind TEXT NOT NULL DEFAULT 'docker',
    runtime_binary_path TEXT NOT NULL DEFAULT '/usr/bin/docker',
    containerd_namespace TEXT NOT NULL DEFAULT 'default',
    registry_url TEXT NOT NULL DEFAULT '',
    registry_host TEXT NOT NULL DEFAULT '',
    registry_project TEXT NOT NULL DEFAULT '',
    registry_insecure BOOLEAN NOT NULL DEFAULT FALSE,
    registry_ca_pem TEXT NOT NULL DEFAULT '',
    registry_credential_id UUID,
    target_credential_id UUID,
    health_checks JSONB NOT NULL DEFAULT '[]'::jsonb,
    command_timeout_seconds INTEGER NOT NULL DEFAULT 600,
    max_log_bytes BIGINT NOT NULL DEFAULT 52428800,
    auto_rollback BOOLEAN NOT NULL DEFAULT FALSE CONSTRAINT deployment_profiles_auto_rollback_disabled CHECK (auto_rollback=FALSE),
    cleanup_workspace BOOLEAN NOT NULL DEFAULT TRUE,
    keep_failed_workspace BOOLEAN NOT NULL DEFAULT FALSE,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    runner_labels TEXT[] NOT NULL DEFAULT '{}',
    revoked_at TIMESTAMPTZ,
    created_by TEXT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(application_id, environment_id, name)
);

CREATE TABLE IF NOT EXISTS releases (
    id UUID PRIMARY KEY,
    application_id UUID NOT NULL REFERENCES applications(id),
    environment_id UUID NOT NULL REFERENCES environments(id),
    profile_id UUID NOT NULL REFERENCES deployment_profiles(id),
    version TEXT NOT NULL,
    notes TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'DRAFT',
    requested_operation TEXT NOT NULL DEFAULT 'DEPLOY' CHECK(requested_operation IN ('DEPLOY','ROLLBACK')),
    rollback_source_release_id UUID REFERENCES releases(id),
    rollback_source_job_id UUID,
    retry_source_job_id UUID,
    operation_requested_by TEXT REFERENCES users(id),
    operation_base_status TEXT,
    created_by TEXT NOT NULL REFERENCES users(id),
    reviewed_by TEXT REFERENCES users(id),
    approved_by TEXT REFERENCES users(id),
    rejected_by TEXT REFERENCES users(id),
    decision_note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(application_id, environment_id, version)
);
CREATE INDEX IF NOT EXISTS releases_status_idx ON releases(status, created_at DESC);

CREATE TABLE IF NOT EXISTS release_artifacts (
    id UUID PRIMARY KEY,
    release_id UUID NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    original_filename TEXT NOT NULL,
    storage_path TEXT NOT NULL DEFAULT '',
    media_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    sha256 TEXT NOT NULL,
    uploaded_by TEXT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS release_jobs (
    id UUID PRIMARY KEY,
    release_id UUID NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    profile_id UUID NOT NULL REFERENCES deployment_profiles(id),
    application TEXT NOT NULL,
    version TEXT NOT NULL,
    environment TEXT NOT NULL,
    lock_key TEXT NOT NULL,
    artifact_id UUID NOT NULL REFERENCES release_artifacts(id),
    artifact_path TEXT NOT NULL,
    expected_sha256 TEXT NOT NULL,
    manifest_path TEXT NOT NULL DEFAULT 'manifest.yaml',
    operation TEXT NOT NULL DEFAULT 'DEPLOY' CHECK(operation IN ('DEPLOY','ROLLBACK')),
    rollback_source_release_id UUID REFERENCES releases(id),
    rollback_source_job_id UUID,
    retry_of_job_id UUID,
    target_credential_id UUID,
    target_credential_version INTEGER,
    runner_labels TEXT[] NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'QUEUED',
    attempts INTEGER NOT NULL DEFAULT 0,
    priority INTEGER NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_by TEXT,
    locked_at TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    failure_message TEXT,
    created_by TEXT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS release_jobs_active_idx ON release_jobs(release_id)
WHERE status NOT IN ('SUCCESS','FAILED','ROLLED_BACK');
CREATE UNIQUE INDEX IF NOT EXISTS release_jobs_active_target_idx ON release_jobs(lock_key)
WHERE status NOT IN ('SUCCESS','FAILED','ROLLED_BACK');
ALTER TABLE releases ADD CONSTRAINT releases_rollback_source_job_fk
    FOREIGN KEY(rollback_source_job_id) REFERENCES release_jobs(id);
ALTER TABLE release_jobs ADD CONSTRAINT release_jobs_rollback_source_job_fk
    FOREIGN KEY(rollback_source_job_id) REFERENCES release_jobs(id);
ALTER TABLE releases ADD CONSTRAINT releases_retry_source_job_fk
    FOREIGN KEY(retry_source_job_id) REFERENCES release_jobs(id);
ALTER TABLE release_jobs ADD CONSTRAINT release_jobs_retry_of_job_fk
    FOREIGN KEY(retry_of_job_id) REFERENCES release_jobs(id);

CREATE TABLE IF NOT EXISTS deployment_heads (
    application_id UUID NOT NULL REFERENCES applications(id),
    environment_id UUID NOT NULL REFERENCES environments(id),
    current_release_id UUID NOT NULL REFERENCES releases(id),
    current_job_id UUID NOT NULL UNIQUE REFERENCES release_jobs(id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(application_id,environment_id)
);

CREATE TABLE IF NOT EXISTS runner_settings (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    poll_interval_ms INTEGER NOT NULL DEFAULT 2000,
    lock_retry_ms INTEGER NOT NULL DEFAULT 5000,
    settings_refresh_ms INTEGER NOT NULL DEFAULT 30000,
    heartbeat_interval_ms INTEGER NOT NULL DEFAULT 5000,
    stale_job_after_ms INTEGER NOT NULL DEFAULT 60000,
    workspace_root TEXT NOT NULL DEFAULT '/var/lib/releasedock/workspaces',
    command_path TEXT NOT NULL DEFAULT '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
    log_chunk_bytes INTEGER NOT NULL DEFAULT 16384,
    shutdown_grace_seconds INTEGER NOT NULL DEFAULT 30,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO runner_settings(singleton) VALUES(TRUE) ON CONFLICT(singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS runner_credentials (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    endpoint TEXT NOT NULL,
    project TEXT NOT NULL,
    username TEXT NOT NULL,
    insecure_skip_verify BOOLEAN NOT NULL DEFAULT FALSE,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    version INTEGER NOT NULL DEFAULT 1,
    ciphertext TEXT NOT NULL,
    approved_at TIMESTAMPTZ,
    approved_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    revoked_at TIMESTAMPTZ,
    created_by TEXT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS target_credentials (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    credential_type TEXT NOT NULL CHECK(credential_type IN ('SSH_PRIVATE_KEY','KUBECONFIG','TOKEN','OPAQUE_FILE')),
    version INTEGER NOT NULL DEFAULT 1 CHECK(version > 0),
    ciphertext TEXT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    approved_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    approved_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    revoked_at TIMESTAMPTZ,
    created_by TEXT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE deployment_profiles
    ADD CONSTRAINT deployment_profiles_registry_credential_fk
    FOREIGN KEY (registry_credential_id) REFERENCES runner_credentials(id) ON DELETE SET NULL;
ALTER TABLE deployment_profiles
    ADD CONSTRAINT deployment_profiles_target_credential_fk
    FOREIGN KEY (target_credential_id) REFERENCES target_credentials(id) ON DELETE SET NULL;
ALTER TABLE release_jobs
    ADD CONSTRAINT release_jobs_target_credential_fk
    FOREIGN KEY (target_credential_id) REFERENCES target_credentials(id);

CREATE TABLE IF NOT EXISTS script_versions (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    script_type TEXT NOT NULL DEFAULT 'DEPLOY',
    version INTEGER NOT NULL,
    interpreter_path TEXT NOT NULL,
    content TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    timeout_seconds INTEGER NOT NULL DEFAULT 600,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    approved_at TIMESTAMPTZ,
    approved_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    revoked_at TIMESTAMPTZ,
    created_by TEXT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(name, version)
);

CREATE TABLE IF NOT EXISTS runner_instances (
    id UUID PRIMARY KEY,
    worker_id TEXT UNIQUE,
    name TEXT NOT NULL UNIQUE,
    address TEXT NOT NULL,
    token_prefix TEXT,
    token_hash BYTEA UNIQUE,
    labels TEXT[] NOT NULL DEFAULT '{}',
    max_concurrent_jobs INTEGER NOT NULL DEFAULT 1 CHECK(max_concurrent_jobs = 1),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    managed_by_runner BOOLEAN NOT NULL DEFAULT FALSE,
    last_heartbeat_at TIMESTAMPTZ,
    created_by TEXT REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS deployment_profile_scripts (
    profile_id UUID NOT NULL REFERENCES deployment_profiles(id) ON DELETE CASCADE,
    script_version_id UUID NOT NULL REFERENCES script_versions(id),
    phase TEXT NOT NULL,
    execution_order INTEGER NOT NULL DEFAULT 0,
    args JSONB NOT NULL DEFAULT '[]'::jsonb,
    timeout_seconds INTEGER,
    PRIMARY KEY(profile_id, script_version_id, phase)
);

CREATE TABLE IF NOT EXISTS release_locks (
    lock_key TEXT PRIMARY KEY,
    job_id UUID NOT NULL UNIQUE REFERENCES release_jobs(id) ON DELETE CASCADE,
    worker_id TEXT NOT NULL,
    acquired_at TIMESTAMPTZ NOT NULL,
    heartbeat_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS release_job_steps (
    id BIGSERIAL PRIMARY KEY,
    job_id UUID NOT NULL REFERENCES release_jobs(id) ON DELETE CASCADE,
    attempt INTEGER NOT NULL,
    step_order INTEGER NOT NULL,
    name TEXT NOT NULL,
    status TEXT NOT NULL,
    exit_code INTEGER,
    message TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    UNIQUE(job_id, attempt, step_order)
);

CREATE TABLE IF NOT EXISTS release_job_logs (
    id BIGSERIAL PRIMARY KEY,
    job_id UUID NOT NULL REFERENCES release_jobs(id) ON DELETE CASCADE,
    step_id BIGINT NOT NULL REFERENCES release_job_steps(id) ON DELETE CASCADE,
    stream TEXT NOT NULL CHECK(stream IN ('stdout','stderr','system')),
    sequence BIGINT NOT NULL,
    payload BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(job_id, step_id, stream, sequence)
);
CREATE INDEX IF NOT EXISTS release_job_logs_stream_idx ON release_job_logs(job_id,id);

CREATE TABLE IF NOT EXISTS release_images (
    id BIGSERIAL PRIMARY KEY,
    job_id UUID NOT NULL REFERENCES release_jobs(id) ON DELETE CASCADE,
    file_path TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    destination_ref TEXT NOT NULL,
    repository TEXT NOT NULL,
    tag TEXT NOT NULL,
    digest TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(job_id,file_path)
);

CREATE OR REPLACE FUNCTION validate_release_job_dependencies() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    credential UUID;
    required_phase TEXT;
    required_script_type TEXT;
BEGIN
	IF NEW.operation='ROLLBACK' THEN
		required_phase:='ROLLBACK';
		required_script_type:='ROLLBACK';
	ELSE
		required_phase:='DEPLOY';
		required_script_type:='DEPLOY';
	END IF;
    SELECT p.registry_credential_id INTO credential
    FROM deployment_profiles p
    JOIN runner_credentials c ON c.id=p.registry_credential_id
    WHERE p.id=NEW.profile_id AND p.active AND p.enabled AND p.revoked_at IS NULL
      AND p.runtime_kind IN ('docker','podman','containerd')
      AND p.runtime_binary_path LIKE '/%'
      AND p.registry_url<>'' AND p.registry_host<>'' AND p.registry_project<>''
      AND c.active AND c.approved_at IS NOT NULL AND c.revoked_at IS NULL
    FOR SHARE OF p,c;
    IF credential IS NULL THEN
        RAISE EXCEPTION 'deployment profile or registry credential is not ready';
    END IF;
    PERFORM 1
    FROM deployment_profile_scripts ps
    JOIN script_versions s ON s.id=ps.script_version_id
    WHERE ps.profile_id=NEW.profile_id AND ps.phase=required_phase
      AND s.script_type=required_script_type AND s.active AND s.approved_at IS NOT NULL AND s.revoked_at IS NULL
    FOR SHARE OF ps,s;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'an approved active % script is required', required_script_type;
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS release_job_dependency_check ON release_jobs;
CREATE TRIGGER release_job_dependency_check BEFORE INSERT ON release_jobs
FOR EACH ROW EXECUTE FUNCTION validate_release_job_dependencies();

CREATE OR REPLACE FUNCTION guard_active_job_dependency_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE dependency_id UUID;
BEGIN
    IF TG_TABLE_NAME='deployment_profiles' THEN dependency_id:=OLD.id;
    ELSIF TG_TABLE_NAME='runner_credentials' THEN
        IF EXISTS(SELECT 1 FROM release_jobs j JOIN deployment_profiles p ON p.id=j.profile_id WHERE p.registry_credential_id=OLD.id AND j.status NOT IN ('SUCCESS','FAILED','ROLLED_BACK')) THEN
            RAISE EXCEPTION 'registry credential is locked by an active release job';
        END IF;
        IF TG_OP='DELETE' THEN RETURN OLD; END IF;
        RETURN NEW;
    ELSIF TG_TABLE_NAME='script_versions' THEN
        IF EXISTS(SELECT 1 FROM release_jobs j JOIN deployment_profile_scripts ps ON ps.profile_id=j.profile_id WHERE ps.script_version_id=OLD.id AND j.status NOT IN ('SUCCESS','FAILED','ROLLED_BACK')) THEN
            RAISE EXCEPTION 'script is locked by an active release job';
        END IF;
        IF TG_OP='DELETE' THEN RETURN OLD; END IF;
        RETURN NEW;
    ELSE dependency_id:=OLD.profile_id;
    END IF;
    IF EXISTS(SELECT 1 FROM release_jobs WHERE profile_id=dependency_id AND status NOT IN ('SUCCESS','FAILED','ROLLED_BACK')) THEN
        RAISE EXCEPTION 'deployment profile is locked by an active release job';
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS active_job_profile_guard ON deployment_profiles;
CREATE TRIGGER active_job_profile_guard BEFORE UPDATE OR DELETE ON deployment_profiles FOR EACH ROW EXECUTE FUNCTION guard_active_job_dependency_change();
DROP TRIGGER IF EXISTS active_job_registry_guard ON runner_credentials;
CREATE TRIGGER active_job_registry_guard BEFORE UPDATE OR DELETE ON runner_credentials FOR EACH ROW EXECUTE FUNCTION guard_active_job_dependency_change();
DROP TRIGGER IF EXISTS active_job_script_guard ON script_versions;
CREATE TRIGGER active_job_script_guard BEFORE UPDATE OR DELETE ON script_versions FOR EACH ROW EXECUTE FUNCTION guard_active_job_dependency_change();
DROP TRIGGER IF EXISTS active_job_profile_script_guard ON deployment_profile_scripts;
CREATE TRIGGER active_job_profile_script_guard BEFORE UPDATE OR DELETE ON deployment_profile_scripts FOR EACH ROW EXECUTE FUNCTION guard_active_job_dependency_change();

CREATE OR REPLACE FUNCTION sync_release_job_status() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE releases SET status=NEW.status,updated_at=clock_timestamp() WHERE id=NEW.release_id;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS release_job_status_sync ON release_jobs;
CREATE TRIGGER release_job_status_sync AFTER INSERT OR UPDATE OF status ON release_jobs
FOR EACH ROW EXECUTE FUNCTION sync_release_job_status();

CREATE TABLE IF NOT EXISTS audit_logs (
    id BIGSERIAL PRIMARY KEY,
    actor_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL DEFAULT 'success',
    ip TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_logs_created_idx ON audit_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS audit_logs_resource_idx ON audit_logs(resource_type, resource_id);

INSERT INTO roles(id, name, description, system) VALUES
    ('role-admin', 'Administrator', 'Full service administration', TRUE),
    ('role-operator', 'Operator', 'Release operator', TRUE),
    ('role-developer', 'Developer', 'Release package developer', TRUE),
    ('role-viewer', 'Viewer', 'Read-only access', TRUE)
ON CONFLICT (id) DO NOTHING;

INSERT INTO permissions(code, description) VALUES
    ('admin.settings.read', 'Read service settings'),
    ('admin.settings.write', 'Change service settings'),
    ('admin.users.read', 'Read users'),
    ('admin.users.write', 'Change users and role membership'),
    ('admin.rbac.read', 'Read roles and permissions'),
    ('admin.rbac.write', 'Change role permissions'),
    ('admin.scripts.read', 'Read approved deployment scripts'),
    ('admin.scripts.write', 'Create, approve, and revoke deployment scripts'),
    ('admin.registries.read', 'Read registry configuration'),
    ('admin.registries.write', 'Change encrypted registry credentials'),
    ('admin.credentials.read', 'Read deployment target credential metadata'),
    ('admin.credentials.write', 'Create, rotate, bind, and revoke encrypted deployment target credentials'),
    ('admin.runners.read', 'Read runner registrations'),
    ('admin.runners.write', 'Change runner registrations'),
    ('applications.read', 'Read applications and environments'),
    ('applications.write', 'Change applications and environments'),
    ('profiles.read', 'Read deployment profiles'),
    ('profiles.write', 'Change deployment profiles'),
    ('releases.read', 'Read releases'),
    ('releases.create', 'Create releases and artifacts'),
    ('releases.write', 'Change releases'),
    ('releases.submit', 'Submit releases for deployment'),
    ('releases.review', 'Review pending releases'),
    ('releases.approve', 'Approve releases'),
    ('releases.reject', 'Reject releases'),
    ('keys.manage', 'Manage personal API keys'),
    ('ai.use', 'Use configured AI gateway'),
    ('mcp.use', 'Use the MCP endpoint'),
    ('audit.read', 'Read audit events')
ON CONFLICT (code) DO UPDATE SET description = EXCLUDED.description;

INSERT INTO role_permissions(role_id, permission_code)
SELECT 'role-admin', code FROM permissions ON CONFLICT DO NOTHING;

INSERT INTO role_permissions(role_id, permission_code)
SELECT 'role-operator', code FROM permissions
WHERE code IN ('applications.read','profiles.read','releases.read','releases.create','releases.write','releases.submit','keys.manage','ai.use','mcp.use')
ON CONFLICT DO NOTHING;

INSERT INTO role_permissions(role_id, permission_code)
SELECT 'role-developer', code FROM permissions
WHERE code IN ('applications.read','profiles.read','releases.read','releases.create','releases.write','releases.submit','keys.manage','ai.use','mcp.use')
ON CONFLICT DO NOTHING;

INSERT INTO role_permissions(role_id, permission_code)
SELECT 'role-viewer', code FROM permissions
WHERE code IN ('applications.read','profiles.read','releases.read','keys.manage','mcp.use')
ON CONFLICT DO NOTHING;
