ALTER TABLE deployment_profiles
    ADD COLUMN IF NOT EXISTS runner_labels TEXT[] NOT NULL DEFAULT '{}';

ALTER TABLE release_jobs
    ADD COLUMN IF NOT EXISTS runner_labels TEXT[] NOT NULL DEFAULT '{}';

CREATE OR REPLACE FUNCTION runner_labels_valid(candidate TEXT[]) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT candidate IS NOT NULL
       AND cardinality(candidate) <= 20
       AND NOT EXISTS(
           SELECT 1 FROM unnest(candidate) AS label
           WHERE label IS NULL OR length(btrim(label)) NOT BETWEEN 1 AND 64
              OR label ~ E'[\\r\\n]'
       )
       AND cardinality(candidate)=cardinality(ARRAY(SELECT DISTINCT label FROM unnest(candidate) AS label));
$$;

DO $$ BEGIN
    ALTER TABLE deployment_profiles ADD CONSTRAINT deployment_profiles_runner_labels_check
        CHECK(runner_labels_valid(runner_labels));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE release_jobs ADD CONSTRAINT release_jobs_runner_labels_check
        CHECK(runner_labels_valid(runner_labels));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE runner_instances ADD CONSTRAINT runner_instances_labels_check
        CHECK(runner_labels_valid(labels));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE INDEX IF NOT EXISTS runner_instances_labels_idx
    ON runner_instances USING GIN(labels);

CREATE OR REPLACE FUNCTION validate_release_job_runner_routing() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    profile_labels TEXT[];
BEGIN
    SELECT runner_labels INTO profile_labels
    FROM deployment_profiles
    WHERE id=NEW.profile_id
    FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'deployment profile does not exist';
    END IF;
    IF NEW.runner_labels IS DISTINCT FROM profile_labels THEN
        RAISE EXCEPTION 'release job runner labels do not match deployment profile';
    END IF;
    IF NOT EXISTS(
        SELECT 1
        FROM runner_instances
        WHERE active
          AND managed_by_runner
          AND worker_id IS NOT NULL
          AND last_heartbeat_at >= clock_timestamp() - interval '60 seconds'
          AND NEW.runner_labels <@ labels
    ) THEN
        RAISE EXCEPTION 'no active runner matches the deployment profile labels';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS release_job_runner_routing_check ON release_jobs;
CREATE TRIGGER release_job_runner_routing_check
BEFORE INSERT ON release_jobs
FOR EACH ROW EXECUTE FUNCTION validate_release_job_runner_routing();
