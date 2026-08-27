ALTER TABLE app_settings ADD COLUMN IF NOT EXISTS general_config JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE app_settings ADD COLUMN IF NOT EXISTS approval_config JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE app_settings ADD COLUMN IF NOT EXISTS storage_config JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE oidc_settings ADD COLUMN IF NOT EXISTS config JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE ai_settings ADD COLUMN IF NOT EXISTS config JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE oidc_states ADD COLUMN IF NOT EXISTS code_verifier TEXT NOT NULL DEFAULT '';
ALTER TABLE environments ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'DEV';
ALTER TABLE environments ADD COLUMN IF NOT EXISTS protected BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE release_jobs ADD COLUMN IF NOT EXISTS operation TEXT NOT NULL DEFAULT 'DEPLOY';

DO $$ BEGIN
    ALTER TABLE release_jobs ADD CONSTRAINT release_jobs_operation_check CHECK(operation IN ('DEPLOY','ROLLBACK'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

ALTER TABLE runner_credentials ADD COLUMN IF NOT EXISTS endpoint TEXT NOT NULL DEFAULT '';
ALTER TABLE runner_credentials ADD COLUMN IF NOT EXISTS project TEXT NOT NULL DEFAULT '';
ALTER TABLE runner_credentials ADD COLUMN IF NOT EXISTS username TEXT NOT NULL DEFAULT '';
ALTER TABLE runner_credentials ADD COLUMN IF NOT EXISTS insecure_skip_verify BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE runner_credentials ADD COLUMN IF NOT EXISTS active BOOLEAN NOT NULL DEFAULT TRUE;

ALTER TABLE script_versions ADD COLUMN IF NOT EXISTS script_type TEXT NOT NULL DEFAULT 'DEPLOY';
ALTER TABLE script_versions ADD COLUMN IF NOT EXISTS timeout_seconds INTEGER NOT NULL DEFAULT 600;
ALTER TABLE script_versions ADD COLUMN IF NOT EXISTS active BOOLEAN NOT NULL DEFAULT TRUE;

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
    last_heartbeat_at TIMESTAMPTZ,
    managed_by_runner BOOLEAN NOT NULL DEFAULT FALSE,
    created_by TEXT REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE runner_instances ADD COLUMN IF NOT EXISTS worker_id TEXT UNIQUE;
ALTER TABLE runner_instances ADD COLUMN IF NOT EXISTS managed_by_runner BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE runner_instances ALTER COLUMN token_prefix DROP NOT NULL;
ALTER TABLE runner_instances ALTER COLUMN token_hash DROP NOT NULL;
ALTER TABLE runner_instances ALTER COLUMN created_by DROP NOT NULL;
ALTER TABLE runner_instances DROP CONSTRAINT IF EXISTS runner_instances_max_concurrent_jobs_check;
ALTER TABLE runner_instances ADD CONSTRAINT runner_instances_max_concurrent_jobs_check CHECK(max_concurrent_jobs=1);

INSERT INTO roles(id,name,description,system) VALUES('role-developer','Developer','Release package developer',TRUE)
ON CONFLICT(id) DO NOTHING;

INSERT INTO permissions(code,description) VALUES
 ('admin.scripts.read','Read approved deployment scripts'),
 ('admin.scripts.write','Create, approve, and revoke deployment scripts'),
 ('admin.registries.read','Read registry configuration'),
 ('admin.registries.write','Change encrypted registry credentials'),
 ('admin.runners.read','Read runner registrations'),
 ('admin.runners.write','Change runner registrations')
ON CONFLICT(code) DO UPDATE SET description=EXCLUDED.description;

INSERT INTO role_permissions(role_id,permission_code)
SELECT 'role-admin',code FROM permissions ON CONFLICT DO NOTHING;
INSERT INTO role_permissions(role_id,permission_code)
SELECT 'role-developer',code FROM permissions
WHERE code IN ('applications.read','profiles.read','releases.read','releases.create','releases.write','releases.submit','keys.manage','ai.use','mcp.use')
ON CONFLICT DO NOTHING;

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
      AND p.runtime_kind IN ('docker','podman','containerd') AND p.runtime_binary_path LIKE '/%'
      AND p.registry_url<>'' AND p.registry_host<>'' AND p.registry_project<>''
      AND c.active AND c.approved_at IS NOT NULL AND c.revoked_at IS NULL
    FOR SHARE OF p,c;
    IF credential IS NULL THEN RAISE EXCEPTION 'deployment profile or registry credential is not ready'; END IF;
    PERFORM 1 FROM deployment_profile_scripts ps JOIN script_versions s ON s.id=ps.script_version_id
    WHERE ps.profile_id=NEW.profile_id AND ps.phase=required_phase AND s.script_type=required_script_type
      AND s.active AND s.approved_at IS NOT NULL AND s.revoked_at IS NULL
    FOR SHARE OF ps,s;
    IF NOT FOUND THEN RAISE EXCEPTION 'an approved active % script is required', required_script_type; END IF;
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
        IF EXISTS(SELECT 1 FROM release_jobs j JOIN deployment_profiles p ON p.id=j.profile_id WHERE p.registry_credential_id=OLD.id AND j.status NOT IN ('SUCCESS','FAILED','ROLLED_BACK')) THEN RAISE EXCEPTION 'registry credential is locked by an active release job'; END IF;
        IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
    ELSIF TG_TABLE_NAME='script_versions' THEN
        IF EXISTS(SELECT 1 FROM release_jobs j JOIN deployment_profile_scripts ps ON ps.profile_id=j.profile_id WHERE ps.script_version_id=OLD.id AND j.status NOT IN ('SUCCESS','FAILED','ROLLED_BACK')) THEN RAISE EXCEPTION 'script is locked by an active release job'; END IF;
        IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
    ELSE dependency_id:=OLD.profile_id;
    END IF;
    IF EXISTS(SELECT 1 FROM release_jobs WHERE profile_id=dependency_id AND status NOT IN ('SUCCESS','FAILED','ROLLED_BACK')) THEN RAISE EXCEPTION 'deployment profile is locked by an active release job'; END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
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
