DROP INDEX IF EXISTS release_jobs_active_idx;
CREATE UNIQUE INDEX release_jobs_active_idx ON release_jobs(release_id)
WHERE status NOT IN ('SUCCESS','FAILED','ROLLED_BACK');

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

CREATE OR REPLACE FUNCTION guard_artifact_storage_path_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.artifact_storage_path IS DISTINCT FROM OLD.artifact_storage_path
       AND EXISTS(SELECT 1 FROM release_artifacts WHERE storage_path<>'') THEN
        RAISE EXCEPTION 'artifact storage path is locked while uploaded artifacts exist';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS artifact_storage_path_guard ON app_settings;
CREATE TRIGGER artifact_storage_path_guard
BEFORE UPDATE OF artifact_storage_path ON app_settings
FOR EACH ROW EXECUTE FUNCTION guard_artifact_storage_path_change();
