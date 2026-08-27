ALTER TABLE release_jobs
    ADD COLUMN IF NOT EXISTS artifact_id UUID REFERENCES release_artifacts(id);

UPDATE release_jobs j
SET artifact_id=(
    SELECT a.id
    FROM release_artifacts a
    WHERE a.storage_path=j.artifact_path AND a.sha256=j.expected_sha256
      AND (a.release_id=j.release_id OR a.release_id=j.rollback_source_release_id)
    ORDER BY a.created_at DESC,a.id DESC
    LIMIT 1)
WHERE j.artifact_id IS NULL;

CREATE OR REPLACE FUNCTION validate_release_job_artifact() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    artifact_release UUID;
    stored_path TEXT;
    stored_sha256 TEXT;
BEGIN
    IF NEW.artifact_id IS NULL THEN
        RAISE EXCEPTION 'release job must bind an uploaded artifact';
    END IF;
    SELECT release_id,storage_path,sha256
    INTO artifact_release,stored_path,stored_sha256
    FROM release_artifacts WHERE id=NEW.artifact_id
    FOR SHARE;
    IF NOT FOUND OR stored_path='' THEN
        RAISE EXCEPTION 'release job artifact is not uploaded';
    END IF;
    IF stored_path IS DISTINCT FROM NEW.artifact_path OR stored_sha256 IS DISTINCT FROM NEW.expected_sha256 THEN
        RAISE EXCEPTION 'release job artifact metadata does not match its immutable binding';
    END IF;
    IF NEW.operation='DEPLOY' AND artifact_release<>NEW.release_id THEN
        RAISE EXCEPTION 'deploy job artifact belongs to another release';
    END IF;
    IF NEW.operation='ROLLBACK' AND artifact_release<>NEW.rollback_source_release_id THEN
        RAISE EXCEPTION 'rollback job artifact does not belong to its source release';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS release_job_artifact_check ON release_jobs;
CREATE TRIGGER release_job_artifact_check
BEFORE INSERT OR UPDATE OF artifact_id,artifact_path,expected_sha256,operation,rollback_source_release_id ON release_jobs
FOR EACH ROW EXECUTE FUNCTION validate_release_job_artifact();

CREATE OR REPLACE FUNCTION guard_release_artifact_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE release_status TEXT;
BEGIN
    SELECT status INTO release_status FROM releases WHERE id=NEW.release_id FOR SHARE;
    IF release_status NOT IN ('DRAFT','UPLOADED','REJECTED') THEN
        RAISE EXCEPTION 'release cannot accept artifacts in its current state';
    END IF;
    IF NEW.storage_path='' AND EXISTS(
        SELECT 1 FROM release_artifacts WHERE release_id=NEW.release_id AND storage_path<>''
    ) THEN
        RAISE EXCEPTION 'metadata-only artifacts cannot be added after content upload';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS release_artifact_insert_guard ON release_artifacts;
CREATE TRIGGER release_artifact_insert_guard
BEFORE INSERT ON release_artifacts
FOR EACH ROW EXECUTE FUNCTION guard_release_artifact_insert();
