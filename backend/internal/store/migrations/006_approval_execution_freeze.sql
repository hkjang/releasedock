ALTER TABLE releases ADD COLUMN IF NOT EXISTS requested_operation TEXT NOT NULL DEFAULT 'DEPLOY';
ALTER TABLE releases ADD COLUMN IF NOT EXISTS rollback_source_release_id UUID REFERENCES releases(id);
ALTER TABLE releases ADD COLUMN IF NOT EXISTS operation_requested_by TEXT REFERENCES users(id);
ALTER TABLE releases ADD COLUMN IF NOT EXISTS operation_base_status TEXT;
CREATE INDEX IF NOT EXISTS oidc_states_expiry_idx ON oidc_states(expires_at);
DO $$ BEGIN
    ALTER TABLE releases ADD CONSTRAINT releases_requested_operation_check CHECK(requested_operation IN ('DEPLOY','ROLLBACK'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE OR REPLACE FUNCTION profile_has_frozen_execution(candidate UUID) RETURNS BOOLEAN
LANGUAGE sql STABLE AS $$
    SELECT EXISTS(
        SELECT 1 FROM releases r
        WHERE r.profile_id=candidate AND r.status IN ('PENDING_REVIEW','UNDER_REVIEW','APPROVED')
    ) OR EXISTS(
        SELECT 1 FROM releases request
        JOIN releases source ON source.id=request.rollback_source_release_id
        WHERE request.requested_operation='ROLLBACK' AND source.profile_id=candidate
          AND request.status IN ('PENDING_REVIEW','UNDER_REVIEW','APPROVED')
    ) OR EXISTS(
        SELECT 1 FROM release_jobs j
        WHERE j.profile_id=candidate AND j.status NOT IN ('SUCCESS','FAILED','ROLLED_BACK')
    );
$$;

CREATE OR REPLACE FUNCTION guard_active_job_dependency_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE dependency_id UUID;
BEGIN
    IF TG_TABLE_NAME='deployment_profiles' THEN
        dependency_id:=OLD.id;
    ELSIF TG_TABLE_NAME='runner_credentials' THEN
        IF EXISTS(
            SELECT 1 FROM deployment_profiles p
            WHERE p.registry_credential_id=OLD.id AND profile_has_frozen_execution(p.id)
        ) THEN
            RAISE EXCEPTION 'registry credential is locked by a reviewed or active release';
        END IF;
        IF TG_OP='DELETE' THEN RETURN OLD; END IF;
        RETURN NEW;
    ELSIF TG_TABLE_NAME='script_versions' THEN
        IF EXISTS(
            SELECT 1 FROM deployment_profile_scripts ps
            WHERE ps.script_version_id=OLD.id AND profile_has_frozen_execution(ps.profile_id)
        ) THEN
            RAISE EXCEPTION 'script is locked by a reviewed or active release';
        END IF;
        IF TG_OP='DELETE' THEN RETURN OLD; END IF;
        RETURN NEW;
    ELSIF TG_OP='INSERT' THEN
        dependency_id:=NEW.profile_id;
    ELSE
        dependency_id:=OLD.profile_id;
    END IF;
    IF profile_has_frozen_execution(dependency_id) THEN
        RAISE EXCEPTION 'deployment profile is locked by a reviewed or active release';
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS active_job_profile_guard ON deployment_profiles;
CREATE TRIGGER active_job_profile_guard BEFORE UPDATE OR DELETE ON deployment_profiles
FOR EACH ROW EXECUTE FUNCTION guard_active_job_dependency_change();
DROP TRIGGER IF EXISTS active_job_registry_guard ON runner_credentials;
CREATE TRIGGER active_job_registry_guard BEFORE UPDATE OR DELETE ON runner_credentials
FOR EACH ROW EXECUTE FUNCTION guard_active_job_dependency_change();
DROP TRIGGER IF EXISTS active_job_script_guard ON script_versions;
CREATE TRIGGER active_job_script_guard BEFORE UPDATE OR DELETE ON script_versions
FOR EACH ROW EXECUTE FUNCTION guard_active_job_dependency_change();
DROP TRIGGER IF EXISTS active_job_profile_script_guard ON deployment_profile_scripts;
CREATE TRIGGER active_job_profile_script_guard BEFORE INSERT OR UPDATE OR DELETE ON deployment_profile_scripts
FOR EACH ROW EXECUTE FUNCTION guard_active_job_dependency_change();

CREATE OR REPLACE FUNCTION validate_release_job_dependencies() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    credential UUID;
    required_phase TEXT;
    required_script_type TEXT;
BEGIN
    IF NEW.operation='ROLLBACK' THEN
        required_phase:='ROLLBACK';
        required_script_type:='ROLLBACK';
        IF NEW.rollback_source_release_id IS NULL THEN
            RAISE EXCEPTION 'rollback source release is required';
        END IF;
        PERFORM 1
        FROM releases source
        JOIN releases target ON target.id=NEW.release_id
        WHERE source.id=NEW.rollback_source_release_id AND source.status='SUCCESS'
          AND source.application_id=target.application_id AND source.environment_id=target.environment_id
          AND source.profile_id=NEW.profile_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'rollback source release is invalid';
        END IF;
    ELSIF NEW.operation='DEPLOY' THEN
        required_phase:='DEPLOY';
        required_script_type:='DEPLOY';
    ELSE
        RAISE EXCEPTION 'unsupported release operation';
    END IF;

    SELECT p.registry_credential_id INTO credential
    FROM releases r
    JOIN deployment_profiles p ON p.id=NEW.profile_id
    JOIN runner_credentials c ON c.id=p.registry_credential_id
    WHERE r.id=NEW.release_id
      AND p.application_id=r.application_id AND p.environment_id=r.environment_id
      AND (NEW.operation='ROLLBACK' OR r.profile_id=NEW.profile_id)
      AND p.active AND p.enabled AND p.revoked_at IS NULL
      AND p.runtime_kind IN ('docker','podman','containerd') AND p.runtime_binary_path LIKE '/%'
      AND p.registry_url<>'' AND p.registry_host<>'' AND p.registry_project<>''
      AND c.active AND c.approved_at IS NOT NULL AND c.revoked_at IS NULL
    FOR SHARE OF p,c;
    IF credential IS NULL THEN
        RAISE EXCEPTION 'release target, deployment profile, or registry credential is not ready';
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

CREATE OR REPLACE FUNCTION validate_release_approval_dependencies() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    effective_profile UUID;
    required_phase TEXT;
    required_script_type TEXT;
BEGIN
    IF NEW.status<>'APPROVED' OR OLD.status='APPROVED' THEN
        RETURN NEW;
    END IF;
    IF NEW.requested_operation='ROLLBACK' THEN
        required_phase:='ROLLBACK';
        required_script_type:='ROLLBACK';
        IF NEW.rollback_source_release_id IS NULL THEN
            RAISE EXCEPTION 'rollback source release is required for approval';
        END IF;
        SELECT source.profile_id INTO effective_profile
        FROM releases source
        WHERE source.id=NEW.rollback_source_release_id AND source.status='SUCCESS'
          AND source.application_id=NEW.application_id AND source.environment_id=NEW.environment_id;
    ELSE
        required_phase:='DEPLOY';
        required_script_type:='DEPLOY';
        effective_profile:=NEW.profile_id;
    END IF;
    IF effective_profile IS NULL THEN
        RAISE EXCEPTION 'approved release source is invalid';
    END IF;

    PERFORM 1
    FROM deployment_profiles p
    JOIN runner_credentials c ON c.id=p.registry_credential_id
    WHERE p.id=effective_profile AND p.application_id=NEW.application_id AND p.environment_id=NEW.environment_id
      AND p.active AND p.enabled AND p.revoked_at IS NULL
      AND p.runtime_kind IN ('docker','podman','containerd') AND p.runtime_binary_path LIKE '/%'
      AND p.registry_url<>'' AND p.registry_host<>'' AND p.registry_project<>''
      AND c.active AND c.approved_at IS NOT NULL AND c.revoked_at IS NULL
    FOR SHARE OF p,c;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'approved release profile or registry credential is not ready';
    END IF;
    PERFORM 1
    FROM deployment_profile_scripts ps
    JOIN script_versions s ON s.id=ps.script_version_id
    WHERE ps.profile_id=effective_profile AND ps.phase=required_phase
      AND s.script_type=required_script_type AND s.active AND s.approved_at IS NOT NULL AND s.revoked_at IS NULL
    FOR SHARE OF ps,s;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'approved release requires an active % script', required_script_type;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS release_approval_dependency_check ON releases;
CREATE TRIGGER release_approval_dependency_check
BEFORE UPDATE OF status ON releases
FOR EACH ROW EXECUTE FUNCTION validate_release_approval_dependencies();

CREATE OR REPLACE FUNCTION guard_active_job_target_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE target_id UUID;
BEGIN
    target_id:=OLD.id;
    IF TG_TABLE_NAME='applications' THEN
        IF TG_OP<>'DELETE' AND NEW.code IS NOT DISTINCT FROM OLD.code AND NEW.name IS NOT DISTINCT FROM OLD.name THEN RETURN NEW; END IF;
        IF EXISTS(
            SELECT 1 FROM releases r
            WHERE r.application_id=target_id AND r.status IN ('PENDING_REVIEW','UNDER_REVIEW','APPROVED')
        ) OR EXISTS(
            SELECT 1 FROM release_jobs j JOIN releases r ON r.id=j.release_id
            WHERE r.application_id=target_id AND j.status NOT IN ('SUCCESS','FAILED','ROLLED_BACK')
        ) THEN RAISE EXCEPTION 'application identity is locked by a reviewed or active release'; END IF;
    ELSE
        IF TG_OP<>'DELETE' AND NEW.code IS NOT DISTINCT FROM OLD.code AND NEW.name IS NOT DISTINCT FROM OLD.name THEN RETURN NEW; END IF;
        IF EXISTS(
            SELECT 1 FROM releases r
            WHERE r.environment_id=target_id AND r.status IN ('PENDING_REVIEW','UNDER_REVIEW','APPROVED')
        ) OR EXISTS(
            SELECT 1 FROM release_jobs j JOIN releases r ON r.id=j.release_id
            WHERE r.environment_id=target_id AND j.status NOT IN ('SUCCESS','FAILED','ROLLED_BACK')
        ) THEN RAISE EXCEPTION 'environment identity is locked by a reviewed or active release'; END IF;
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;
