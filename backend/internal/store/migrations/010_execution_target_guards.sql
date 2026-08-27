CREATE OR REPLACE FUNCTION validate_release_job_active_target() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target_job_at TIMESTAMPTZ;
BEGIN
    PERFORM 1
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

    IF NEW.operation='ROLLBACK' THEN
        SELECT max(created_at) INTO target_job_at FROM release_jobs WHERE release_id=NEW.release_id;
        IF target_job_at IS NULL THEN
            RAISE EXCEPTION 'rollback target has no deployment job history';
        END IF;
        IF EXISTS(
            SELECT 1 FROM release_jobs newer
            JOIN releases candidate ON candidate.id=newer.release_id
            JOIN releases target ON target.id=NEW.release_id
            WHERE candidate.application_id=target.application_id AND candidate.environment_id=target.environment_id
              AND newer.created_at>target_job_at
        ) THEN
            RAISE EXCEPTION 'rollback target is stale because a newer deployment job exists';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS release_job_active_target_check ON release_jobs;
CREATE TRIGGER release_job_active_target_check
BEFORE INSERT ON release_jobs
FOR EACH ROW EXECUTE FUNCTION validate_release_job_active_target();

CREATE OR REPLACE FUNCTION validate_release_approval_active_target() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status<>'APPROVED' OR OLD.status='APPROVED' THEN
        RETURN NEW;
    END IF;
    PERFORM 1
    FROM applications a JOIN environments e ON e.application_id=a.id
    WHERE a.id=NEW.application_id AND e.id=NEW.environment_id AND a.active AND e.active
    FOR SHARE OF a,e;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'inactive application or environment cannot be approved';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS release_approval_active_target_check ON releases;
CREATE TRIGGER release_approval_active_target_check
BEFORE UPDATE OF status ON releases
FOR EACH ROW EXECUTE FUNCTION validate_release_approval_active_target();
