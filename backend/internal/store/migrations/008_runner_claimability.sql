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
        RAISE EXCEPTION 'no claimable active runner matches the deployment profile labels';
    END IF;
    RETURN NEW;
END;
$$;
