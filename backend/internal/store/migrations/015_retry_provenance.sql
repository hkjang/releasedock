ALTER TABLE releases ADD COLUMN IF NOT EXISTS retry_source_job_id UUID;
ALTER TABLE release_jobs ADD COLUMN IF NOT EXISTS retry_of_job_id UUID;

DO $$ BEGIN
    ALTER TABLE releases ADD CONSTRAINT releases_retry_source_job_fk
        FOREIGN KEY(retry_source_job_id) REFERENCES release_jobs(id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE release_jobs ADD CONSTRAINT release_jobs_retry_of_job_fk
        FOREIGN KEY(retry_of_job_id) REFERENCES release_jobs(id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE OR REPLACE FUNCTION validate_release_retry_job() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE latest_job UUID;
BEGIN
    IF NEW.retry_of_job_id IS NULL THEN RETURN NEW; END IF;
    PERFORM 1 FROM release_jobs failed
    WHERE failed.id=NEW.retry_of_job_id AND failed.release_id=NEW.release_id
      AND failed.operation='DEPLOY' AND failed.status='FAILED' FOR SHARE;
    IF NOT FOUND THEN RAISE EXCEPTION 'retry source must be a failed DEPLOY job for the same release'; END IF;

    SELECT candidate.id INTO latest_job
    FROM release_jobs candidate JOIN releases candidate_release ON candidate_release.id=candidate.release_id
    JOIN releases target ON target.id=NEW.release_id
    WHERE candidate_release.application_id=target.application_id AND candidate_release.environment_id=target.environment_id
    ORDER BY candidate.created_at DESC,candidate.id DESC LIMIT 1;
    IF latest_job IS DISTINCT FROM NEW.retry_of_job_id THEN
        RAISE EXCEPTION 'retry source is not the latest deployment attempt for the target';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS release_retry_job_check ON release_jobs;
CREATE TRIGGER release_retry_job_check
BEFORE INSERT ON release_jobs
FOR EACH ROW EXECUTE FUNCTION validate_release_retry_job();

CREATE OR REPLACE FUNCTION validate_release_retry_approval() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE latest_job UUID;
BEGIN
    IF NEW.status<>'APPROVED' OR OLD.status='APPROVED' OR NEW.retry_source_job_id IS NULL THEN RETURN NEW; END IF;
    PERFORM 1 FROM release_jobs failed
    WHERE failed.id=NEW.retry_source_job_id AND failed.release_id=NEW.id
      AND failed.operation='DEPLOY' AND failed.status='FAILED' FOR SHARE;
    IF NOT FOUND THEN RAISE EXCEPTION 'retry approval source is invalid'; END IF;
    SELECT candidate.id INTO latest_job
    FROM release_jobs candidate JOIN releases candidate_release ON candidate_release.id=candidate.release_id
    WHERE candidate_release.application_id=NEW.application_id AND candidate_release.environment_id=NEW.environment_id
    ORDER BY candidate.created_at DESC,candidate.id DESC LIMIT 1;
    IF latest_job IS DISTINCT FROM NEW.retry_source_job_id THEN RAISE EXCEPTION 'retry approval is stale'; END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS release_retry_approval_check ON releases;
CREATE TRIGGER release_retry_approval_check
BEFORE UPDATE OF status ON releases
FOR EACH ROW EXECUTE FUNCTION validate_release_retry_approval();
