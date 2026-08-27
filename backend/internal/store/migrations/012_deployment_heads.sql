-- A verified deployment head is independent from release/job creation order.
-- DEPLOY jobs snapshot the head they replace in rollback_source_release_id.
CREATE TABLE IF NOT EXISTS deployment_heads (
    application_id UUID NOT NULL REFERENCES applications(id),
    environment_id UUID NOT NULL REFERENCES environments(id),
    current_release_id UUID NOT NULL REFERENCES releases(id),
    current_job_id UUID NOT NULL UNIQUE REFERENCES release_jobs(id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(application_id,environment_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS release_jobs_active_target_idx
ON release_jobs(lock_key)
WHERE status NOT IN ('SUCCESS','FAILED','ROLLED_BACK');

-- Best-effort provenance backfill for deployments completed before this
-- migration. New jobs are checked transactionally below.
WITH successful_deploys AS (
    SELECT j.id,
           lag(j.release_id) OVER (
               PARTITION BY r.application_id,r.environment_id
               ORDER BY COALESCE(j.finished_at,j.created_at),j.created_at,j.id
           ) AS prior_release_id
    FROM release_jobs j
    JOIN releases r ON r.id=j.release_id
    WHERE j.operation='DEPLOY' AND j.status='SUCCESS'
)
UPDATE release_jobs j
SET rollback_source_release_id=s.prior_release_id
FROM successful_deploys s
WHERE j.id=s.id AND j.rollback_source_release_id IS NULL;

WITH terminal_events AS (
    SELECT r.application_id,r.environment_id,j.id AS event_job_id,j.operation,
           CASE WHEN j.operation='ROLLBACK' THEN j.rollback_source_release_id ELSE j.release_id END AS deployed_release_id,
           COALESCE(j.finished_at,j.created_at) AS event_at,j.created_at
    FROM release_jobs j
    JOIN releases r ON r.id=j.release_id
    WHERE (j.operation='DEPLOY' AND j.status='SUCCESS')
       OR (j.operation='ROLLBACK' AND j.status='ROLLED_BACK')
), latest_events AS (
    SELECT DISTINCT ON (application_id,environment_id)
           application_id,environment_id,event_job_id,operation,deployed_release_id
    FROM terminal_events
    WHERE deployed_release_id IS NOT NULL
    ORDER BY application_id,environment_id,event_at DESC,created_at DESC,event_job_id DESC
), resolved AS (
    SELECT e.application_id,e.environment_id,e.deployed_release_id,
           CASE WHEN e.operation='DEPLOY' THEN e.event_job_id ELSE basis.id END AS basis_job_id
    FROM latest_events e
    LEFT JOIN LATERAL (
        SELECT j.id
        FROM release_jobs j
        WHERE j.release_id=e.deployed_release_id AND j.operation='DEPLOY' AND j.status='SUCCESS'
        ORDER BY j.finished_at DESC NULLS LAST,j.created_at DESC,j.id DESC
        LIMIT 1
    ) basis ON TRUE
)
INSERT INTO deployment_heads(application_id,environment_id,current_release_id,current_job_id)
SELECT application_id,environment_id,deployed_release_id,basis_job_id
FROM resolved
WHERE basis_job_id IS NOT NULL
ON CONFLICT(application_id,environment_id) DO NOTHING;

-- Migration 006 reserved rollback_source_release_id for ROLLBACK jobs. It is
-- now also the immutable prior-head snapshot for DEPLOY jobs, while all other
-- dependency checks remain unchanged.
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

CREATE OR REPLACE FUNCTION validate_release_job_active_target() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target_application UUID;
    target_environment UUID;
    expected_lock_key TEXT;
    head_release UUID;
    head_job UUID;
    expected_source UUID;
    latest_job RECORD;
BEGIN
    SELECT r.application_id,r.environment_id,r.application_id::text||':'||r.environment_id::text
    INTO target_application,target_environment,expected_lock_key
    FROM releases r
    JOIN applications a ON a.id=r.application_id
    JOIN environments e ON e.id=r.environment_id
    JOIN deployment_profiles p ON p.id=NEW.profile_id
    WHERE r.id=NEW.release_id
      AND p.application_id=r.application_id AND p.environment_id=r.environment_id
      AND a.active AND e.active AND p.active AND p.enabled AND p.revoked_at IS NULL
    FOR SHARE OF a,e,p;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'release application, environment, or deployment profile is inactive';
    END IF;
    IF lower(NEW.lock_key)<>lower(expected_lock_key) THEN
        RAISE EXCEPTION 'release job lock key does not match immutable target identifiers';
    END IF;

    SELECT current_release_id,current_job_id INTO head_release,head_job
    FROM deployment_heads
    WHERE application_id=target_application AND environment_id=target_environment
    FOR SHARE;

    IF NEW.operation='DEPLOY' THEN
        IF NEW.rollback_source_release_id IS DISTINCT FROM head_release THEN
            RAISE EXCEPTION 'deploy job prior-head snapshot does not match the verified deployment head';
        END IF;
        RETURN NEW;
    END IF;

    IF head_release IS NULL OR NEW.rollback_source_release_id IS NULL THEN
        RAISE EXCEPTION 'rollback requires an existing verified deployment head and source';
    END IF;

    SELECT j.id,j.release_id,j.operation,j.status,j.rollback_source_release_id
    INTO latest_job
    FROM release_jobs j
    JOIN releases candidate ON candidate.id=j.release_id
    WHERE candidate.application_id=target_application AND candidate.environment_id=target_environment
    ORDER BY j.created_at DESC,j.id DESC
    LIMIT 1;

    IF NEW.release_id=head_release THEN
        SELECT rollback_source_release_id INTO expected_source
        FROM release_jobs
        WHERE id=head_job AND release_id=head_release AND operation='DEPLOY' AND status='SUCCESS';
        IF expected_source IS NULL THEN
            RAISE EXCEPTION 'current deployment head has no previous verified deployment';
        END IF;
        IF NOT (
            latest_job.id=head_job
            OR (latest_job.operation='ROLLBACK' AND latest_job.status='ROLLED_BACK' AND latest_job.rollback_source_release_id=head_release)
            OR (latest_job.release_id=NEW.release_id AND latest_job.operation='ROLLBACK' AND latest_job.status='FAILED' AND latest_job.rollback_source_release_id=expected_source)
        ) THEN
            RAISE EXCEPTION 'current deployment head is shadowed by a newer deployment attempt';
        END IF;
    ELSE
        expected_source:=head_release;
        IF NOT (
            latest_job.release_id=NEW.release_id
            AND (
                (latest_job.operation='DEPLOY' AND latest_job.status='FAILED')
                OR (latest_job.operation='ROLLBACK' AND latest_job.status='FAILED' AND latest_job.rollback_source_release_id=expected_source)
            )
        ) THEN
            RAISE EXCEPTION 'rollback target is not the latest failed deployment attempt';
        END IF;
    END IF;

    IF NEW.rollback_source_release_id IS DISTINCT FROM expected_source THEN
        RAISE EXCEPTION 'rollback source does not match the verified deployment history';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS release_job_active_target_check ON release_jobs;
CREATE TRIGGER release_job_active_target_check
BEFORE INSERT ON release_jobs
FOR EACH ROW EXECUTE FUNCTION validate_release_job_active_target();

CREATE OR REPLACE FUNCTION sync_verified_deployment_head() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target_application UUID;
    target_environment UUID;
    basis_job UUID;
BEGIN
    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;
    IF NOT ((NEW.operation='DEPLOY' AND NEW.status='SUCCESS') OR (NEW.operation='ROLLBACK' AND NEW.status='ROLLED_BACK')) THEN
        RETURN NEW;
    END IF;

    SELECT application_id,environment_id INTO target_application,target_environment
    FROM releases WHERE id=NEW.release_id;

    IF NEW.operation='DEPLOY' THEN
        INSERT INTO deployment_heads(application_id,environment_id,current_release_id,current_job_id,updated_at)
        VALUES(target_application,target_environment,NEW.release_id,NEW.id,clock_timestamp())
        ON CONFLICT(application_id,environment_id) DO UPDATE
        SET current_release_id=EXCLUDED.current_release_id,current_job_id=EXCLUDED.current_job_id,updated_at=EXCLUDED.updated_at;
        RETURN NEW;
    END IF;

    SELECT id INTO basis_job
    FROM release_jobs
    WHERE release_id=NEW.rollback_source_release_id AND operation='DEPLOY' AND status='SUCCESS'
    ORDER BY finished_at DESC NULLS LAST,created_at DESC,id DESC
    LIMIT 1;
    IF basis_job IS NULL THEN
        RAISE EXCEPTION 'rollback source has no verified successful deploy job';
    END IF;

    UPDATE deployment_heads
    SET current_release_id=NEW.rollback_source_release_id,current_job_id=basis_job,updated_at=clock_timestamp()
    WHERE application_id=target_application AND environment_id=target_environment
      AND current_release_id IN (NEW.release_id,NEW.rollback_source_release_id);
    IF NOT FOUND THEN
        RAISE EXCEPTION 'deployment head changed before rollback completed';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS verified_deployment_head_status_sync ON release_jobs;
CREATE TRIGGER verified_deployment_head_status_sync
AFTER UPDATE OF status ON release_jobs
FOR EACH ROW EXECUTE FUNCTION sync_verified_deployment_head();
