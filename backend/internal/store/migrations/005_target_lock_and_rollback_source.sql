ALTER TABLE release_jobs
ADD COLUMN IF NOT EXISTS rollback_source_release_id UUID REFERENCES releases(id);

CREATE OR REPLACE FUNCTION guard_active_job_target_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE target_id UUID;
BEGIN
    target_id:=OLD.id;
    IF TG_TABLE_NAME='applications' THEN
        IF TG_OP<>'DELETE' AND NEW.code IS NOT DISTINCT FROM OLD.code AND NEW.name IS NOT DISTINCT FROM OLD.name THEN
            RETURN NEW;
        END IF;
        IF EXISTS(
            SELECT 1 FROM release_jobs j
            JOIN releases r ON r.id=j.release_id
            WHERE r.application_id=target_id AND j.status NOT IN ('SUCCESS','FAILED','ROLLED_BACK')
        ) THEN
            RAISE EXCEPTION 'application identity is locked by an active release job';
        END IF;
    ELSE
        IF TG_OP<>'DELETE' AND NEW.code IS NOT DISTINCT FROM OLD.code AND NEW.name IS NOT DISTINCT FROM OLD.name THEN
            RETURN NEW;
        END IF;
        IF EXISTS(
            SELECT 1 FROM release_jobs j
            JOIN releases r ON r.id=j.release_id
            WHERE r.environment_id=target_id AND j.status NOT IN ('SUCCESS','FAILED','ROLLED_BACK')
        ) THEN
            RAISE EXCEPTION 'environment identity is locked by an active release job';
        END IF;
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS active_job_application_guard ON applications;
CREATE TRIGGER active_job_application_guard
BEFORE UPDATE OF code,name OR DELETE ON applications
FOR EACH ROW EXECUTE FUNCTION guard_active_job_target_change();

DROP TRIGGER IF EXISTS active_job_environment_guard ON environments;
CREATE TRIGGER active_job_environment_guard
BEFORE UPDATE OF code,name OR DELETE ON environments
FOR EACH ROW EXECUTE FUNCTION guard_active_job_target_change();
