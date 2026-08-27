ALTER TABLE releases ADD COLUMN IF NOT EXISTS rollback_source_job_id UUID;
ALTER TABLE release_jobs ADD COLUMN IF NOT EXISTS rollback_source_job_id UUID;

DO $$ BEGIN
    ALTER TABLE releases ADD CONSTRAINT releases_rollback_source_job_fk
        FOREIGN KEY(rollback_source_job_id) REFERENCES release_jobs(id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE release_jobs ADD CONSTRAINT release_jobs_rollback_source_job_fk
        FOREIGN KEY(rollback_source_job_id) REFERENCES release_jobs(id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- Reconstruct the verified head immediately preceding each historical DEPLOY
-- from terminal events. This includes intervening successful rollbacks.
WITH deploy_jobs AS (
    SELECT j.id,r.application_id,r.environment_id,j.created_at
    FROM release_jobs j JOIN releases r ON r.id=j.release_id
    WHERE j.operation='DEPLOY'
), prior_heads AS (
    SELECT d.id,
           CASE WHEN event.operation='DEPLOY' THEN event.release_id ELSE event.rollback_source_release_id END AS prior_release_id,
           CASE WHEN event.operation='DEPLOY' THEN event.id ELSE source_basis.id END AS prior_job_id
    FROM deploy_jobs d
    LEFT JOIN LATERAL (
        SELECT candidate.*
        FROM release_jobs candidate JOIN releases candidate_release ON candidate_release.id=candidate.release_id
        WHERE candidate_release.application_id=d.application_id AND candidate_release.environment_id=d.environment_id
          AND ((candidate.operation='DEPLOY' AND candidate.status='SUCCESS') OR (candidate.operation='ROLLBACK' AND candidate.status='ROLLED_BACK'))
          AND COALESCE(candidate.finished_at,candidate.created_at)<d.created_at
        ORDER BY COALESCE(candidate.finished_at,candidate.created_at) DESC,candidate.created_at DESC,candidate.id DESC
        LIMIT 1
    ) event ON TRUE
    LEFT JOIN LATERAL (
        SELECT source.id
        FROM release_jobs source
        WHERE source.release_id=event.rollback_source_release_id AND source.operation='DEPLOY' AND source.status='SUCCESS'
          AND COALESCE(source.finished_at,source.created_at)<=COALESCE(event.finished_at,event.created_at)
        ORDER BY source.finished_at DESC NULLS LAST,source.created_at DESC,source.id DESC
        LIMIT 1
    ) source_basis ON event.operation='ROLLBACK'
)
UPDATE release_jobs job
SET rollback_source_release_id=prior.prior_release_id,
    rollback_source_job_id=prior.prior_job_id
FROM prior_heads prior
WHERE job.id=prior.id;

UPDATE release_jobs rollback_job
SET rollback_source_job_id=(
    SELECT deploy.id
    FROM release_jobs deploy
    WHERE deploy.release_id=rollback_job.rollback_source_release_id AND deploy.operation='DEPLOY' AND deploy.status='SUCCESS'
    ORDER BY deploy.finished_at DESC NULLS LAST,deploy.created_at DESC,deploy.id DESC
    LIMIT 1
)
WHERE rollback_job.operation='ROLLBACK' AND rollback_job.rollback_source_job_id IS NULL;

-- Correct deployment_heads reconstructed by 012 with the exact latest terminal
-- event and its immutable successful DEPLOY basis.
WITH terminal_events AS (
    SELECT r.application_id,r.environment_id,j.id,j.operation,j.release_id,j.rollback_source_release_id,j.rollback_source_job_id,
           COALESCE(j.finished_at,j.created_at) AS event_at,j.created_at
    FROM release_jobs j JOIN releases r ON r.id=j.release_id
    WHERE (j.operation='DEPLOY' AND j.status='SUCCESS') OR (j.operation='ROLLBACK' AND j.status='ROLLED_BACK')
), latest AS (
    SELECT DISTINCT ON (application_id,environment_id) *
    FROM terminal_events
    ORDER BY application_id,environment_id,event_at DESC,created_at DESC,id DESC
)
INSERT INTO deployment_heads(application_id,environment_id,current_release_id,current_job_id,updated_at)
SELECT application_id,environment_id,
       CASE WHEN operation='DEPLOY' THEN release_id ELSE rollback_source_release_id END,
       CASE WHEN operation='DEPLOY' THEN id ELSE rollback_source_job_id END,
       clock_timestamp()
FROM latest
WHERE CASE WHEN operation='DEPLOY' THEN id ELSE rollback_source_job_id END IS NOT NULL
ON CONFLICT(application_id,environment_id) DO UPDATE
SET current_release_id=EXCLUDED.current_release_id,current_job_id=EXCLUDED.current_job_id,updated_at=EXCLUDED.updated_at;

CREATE OR REPLACE FUNCTION validate_release_job_dependencies() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    credential UUID;
    required_phase TEXT;
    required_script_type TEXT;
BEGIN
    IF NEW.operation='ROLLBACK' THEN
        required_phase:='ROLLBACK';
        required_script_type:='ROLLBACK';
        IF NEW.rollback_source_release_id IS NULL OR NEW.rollback_source_job_id IS NULL THEN
            RAISE EXCEPTION 'rollback source release and successful deploy basis job are required';
        END IF;
        PERFORM 1
        FROM releases source
        JOIN releases target ON target.id=NEW.release_id
        JOIN release_jobs source_job ON source_job.id=NEW.rollback_source_job_id
        WHERE source.id=NEW.rollback_source_release_id
          AND source_job.release_id=source.id AND source_job.operation='DEPLOY' AND source_job.status='SUCCESS'
          AND source.application_id=target.application_id AND source.environment_id=target.environment_id
          AND source.profile_id=NEW.profile_id;
        IF NOT FOUND THEN RAISE EXCEPTION 'rollback source or immutable basis job is invalid'; END IF;
    ELSIF NEW.operation='DEPLOY' THEN
        required_phase:='DEPLOY';
        required_script_type:='DEPLOY';
    ELSE
        RAISE EXCEPTION 'unsupported release operation';
    END IF;

    SELECT p.registry_credential_id INTO credential
    FROM releases r JOIN deployment_profiles p ON p.id=NEW.profile_id
    JOIN runner_credentials c ON c.id=p.registry_credential_id
    WHERE r.id=NEW.release_id AND p.application_id=r.application_id AND p.environment_id=r.environment_id
      AND (NEW.operation='ROLLBACK' OR r.profile_id=NEW.profile_id)
      AND p.active AND p.enabled AND p.revoked_at IS NULL
      AND p.runtime_kind IN ('docker','podman','containerd') AND p.runtime_binary_path LIKE '/%'
      AND p.registry_url<>'' AND p.registry_host<>'' AND p.registry_project<>''
      AND c.active AND c.approved_at IS NOT NULL AND c.revoked_at IS NULL
    FOR SHARE OF p,c;
    IF credential IS NULL THEN RAISE EXCEPTION 'release target, deployment profile, or registry credential is not ready'; END IF;

    PERFORM 1 FROM deployment_profile_scripts ps JOIN script_versions s ON s.id=ps.script_version_id
    WHERE ps.profile_id=NEW.profile_id AND ps.phase=required_phase AND s.script_type=required_script_type
      AND s.active AND s.approved_at IS NOT NULL AND s.revoked_at IS NULL FOR SHARE OF ps,s;
    IF NOT FOUND THEN RAISE EXCEPTION 'an approved active % script is required',required_script_type; END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION validate_release_job_active_target() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    target_application UUID;
    target_environment UUID;
    expected_lock_key TEXT;
    head_release UUID;
    head_job UUID;
    expected_source UUID;
    expected_source_job UUID;
    latest_job RECORD;
BEGIN
    SELECT r.application_id,r.environment_id,r.application_id::text||':'||r.environment_id::text
    INTO target_application,target_environment,expected_lock_key
    FROM releases r JOIN applications a ON a.id=r.application_id JOIN environments e ON e.id=r.environment_id
    JOIN deployment_profiles p ON p.id=NEW.profile_id
    WHERE r.id=NEW.release_id AND p.application_id=r.application_id AND p.environment_id=r.environment_id
      AND a.active AND e.active AND p.active AND p.enabled AND p.revoked_at IS NULL FOR SHARE OF a,e,p;
    IF NOT FOUND THEN RAISE EXCEPTION 'release application, environment, or deployment profile is inactive'; END IF;
    IF lower(NEW.lock_key)<>lower(expected_lock_key) THEN RAISE EXCEPTION 'release job lock key does not match immutable target identifiers'; END IF;

    SELECT current_release_id,current_job_id INTO head_release,head_job FROM deployment_heads
    WHERE application_id=target_application AND environment_id=target_environment FOR SHARE;
    IF NEW.operation='DEPLOY' THEN
        IF NEW.rollback_source_release_id IS DISTINCT FROM head_release OR NEW.rollback_source_job_id IS DISTINCT FROM head_job THEN
            RAISE EXCEPTION 'deploy job prior-head snapshot does not match the verified deployment head';
        END IF;
        RETURN NEW;
    END IF;
    IF head_release IS NULL OR NEW.rollback_source_release_id IS NULL OR NEW.rollback_source_job_id IS NULL THEN
        RAISE EXCEPTION 'rollback requires an existing verified deployment head and basis job';
    END IF;

    SELECT j.id,j.release_id,j.operation,j.status,j.rollback_source_release_id,j.rollback_source_job_id INTO latest_job
    FROM release_jobs j JOIN releases candidate ON candidate.id=j.release_id
    WHERE candidate.application_id=target_application AND candidate.environment_id=target_environment
    ORDER BY j.created_at DESC,j.id DESC LIMIT 1;

    IF NEW.release_id=head_release THEN
        SELECT rollback_source_release_id,rollback_source_job_id INTO expected_source,expected_source_job
        FROM release_jobs WHERE id=head_job AND release_id=head_release AND operation='DEPLOY' AND status='SUCCESS';
        IF expected_source IS NULL OR expected_source_job IS NULL THEN RAISE EXCEPTION 'current deployment head has no previous verified release'; END IF;
        IF NOT (latest_job.id=head_job
            OR (latest_job.operation='ROLLBACK' AND latest_job.status='ROLLED_BACK' AND latest_job.rollback_source_release_id=head_release AND latest_job.rollback_source_job_id=head_job)
            OR (latest_job.release_id=NEW.release_id AND latest_job.operation='ROLLBACK' AND latest_job.status='FAILED' AND latest_job.rollback_source_release_id=expected_source AND latest_job.rollback_source_job_id=expected_source_job))
        THEN RAISE EXCEPTION 'current deployment head is shadowed by a newer deployment attempt'; END IF;
    ELSE
        expected_source:=head_release;
        expected_source_job:=head_job;
        IF NOT (latest_job.release_id=NEW.release_id AND (
            (latest_job.operation='DEPLOY' AND latest_job.status='FAILED')
            OR (latest_job.operation='ROLLBACK' AND latest_job.status='FAILED' AND latest_job.rollback_source_release_id=expected_source AND latest_job.rollback_source_job_id=expected_source_job)))
        THEN RAISE EXCEPTION 'rollback target is not the latest failed deployment attempt'; END IF;
    END IF;
    IF NEW.rollback_source_release_id IS DISTINCT FROM expected_source OR NEW.rollback_source_job_id IS DISTINCT FROM expected_source_job THEN
        RAISE EXCEPTION 'rollback source does not match verified deployment history';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION sync_verified_deployment_head() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE target_application UUID; target_environment UUID;
BEGIN
    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN RETURN NEW; END IF;
    IF NOT ((NEW.operation='DEPLOY' AND NEW.status='SUCCESS') OR (NEW.operation='ROLLBACK' AND NEW.status='ROLLED_BACK')) THEN RETURN NEW; END IF;
    SELECT application_id,environment_id INTO target_application,target_environment FROM releases WHERE id=NEW.release_id;
    IF NEW.operation='DEPLOY' THEN
        INSERT INTO deployment_heads(application_id,environment_id,current_release_id,current_job_id,updated_at)
        VALUES(target_application,target_environment,NEW.release_id,NEW.id,clock_timestamp())
        ON CONFLICT(application_id,environment_id) DO UPDATE SET current_release_id=EXCLUDED.current_release_id,current_job_id=EXCLUDED.current_job_id,updated_at=EXCLUDED.updated_at;
        RETURN NEW;
    END IF;
    PERFORM 1 FROM release_jobs basis WHERE basis.id=NEW.rollback_source_job_id AND basis.release_id=NEW.rollback_source_release_id AND basis.operation='DEPLOY' AND basis.status='SUCCESS';
    IF NOT FOUND THEN RAISE EXCEPTION 'rollback source has no verified successful deploy basis job'; END IF;
    UPDATE deployment_heads SET current_release_id=NEW.rollback_source_release_id,current_job_id=NEW.rollback_source_job_id,updated_at=clock_timestamp()
    WHERE application_id=target_application AND environment_id=target_environment AND current_release_id IN (NEW.release_id,NEW.rollback_source_release_id);
    IF NOT FOUND THEN RAISE EXCEPTION 'deployment head changed before rollback completed'; END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION validate_release_approval_dependencies() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE effective_profile UUID; required_phase TEXT; required_script_type TEXT;
BEGIN
    IF NEW.status<>'APPROVED' OR OLD.status='APPROVED' THEN RETURN NEW; END IF;
    IF NEW.requested_operation='ROLLBACK' THEN
        required_phase:='ROLLBACK'; required_script_type:='ROLLBACK';
        IF NEW.rollback_source_release_id IS NULL OR NEW.rollback_source_job_id IS NULL THEN RAISE EXCEPTION 'rollback source release and basis job are required for approval'; END IF;
        SELECT source.profile_id INTO effective_profile FROM releases source JOIN release_jobs basis ON basis.id=NEW.rollback_source_job_id
        WHERE source.id=NEW.rollback_source_release_id AND basis.release_id=source.id AND basis.operation='DEPLOY' AND basis.status='SUCCESS'
          AND source.application_id=NEW.application_id AND source.environment_id=NEW.environment_id;
    ELSE
        required_phase:='DEPLOY'; required_script_type:='DEPLOY'; effective_profile:=NEW.profile_id;
    END IF;
    IF effective_profile IS NULL THEN RAISE EXCEPTION 'approved release source is invalid'; END IF;
    PERFORM 1 FROM deployment_profiles p JOIN runner_credentials c ON c.id=p.registry_credential_id
    WHERE p.id=effective_profile AND p.application_id=NEW.application_id AND p.environment_id=NEW.environment_id
      AND p.active AND p.enabled AND p.revoked_at IS NULL AND p.runtime_kind IN ('docker','podman','containerd') AND p.runtime_binary_path LIKE '/%'
      AND p.registry_url<>'' AND p.registry_host<>'' AND p.registry_project<>'' AND c.active AND c.approved_at IS NOT NULL AND c.revoked_at IS NULL FOR SHARE OF p,c;
    IF NOT FOUND THEN RAISE EXCEPTION 'approved release profile or registry credential is not ready'; END IF;
    PERFORM 1 FROM deployment_profile_scripts ps JOIN script_versions s ON s.id=ps.script_version_id
    WHERE ps.profile_id=effective_profile AND ps.phase=required_phase AND s.script_type=required_script_type AND s.active AND s.approved_at IS NOT NULL AND s.revoked_at IS NULL FOR SHARE OF ps,s;
    IF NOT FOUND THEN RAISE EXCEPTION 'approved release requires an active % script',required_script_type; END IF;
    RETURN NEW;
END;
$$;
