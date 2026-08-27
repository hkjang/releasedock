CREATE TABLE IF NOT EXISTS deployment_presets (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    artifact_prefix TEXT NOT NULL
        CHECK (artifact_prefix ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'),
    application_id UUID NOT NULL REFERENCES applications(id),
    environment_id UUID NOT NULL REFERENCES environments(id),
    profile_id UUID NOT NULL REFERENCES deployment_profiles(id),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    auto_deploy_after_approval BOOLEAN NOT NULL DEFAULT TRUE,
    created_by TEXT NOT NULL REFERENCES users(id),
    updated_by TEXT REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS deployment_presets_target_idx
    ON deployment_presets(application_id, environment_id, profile_id)
    WHERE revoked_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS deployment_presets_active_prefix_idx
    ON deployment_presets(artifact_prefix)
    WHERE revoked_at IS NULL;

-- Serialize every release job producer by immutable deployment target before
-- any of the older release_jobs BEFORE INSERT triggers can take a shared lock
-- on deployment_heads. The deliberately early trigger name is significant:
-- PostgreSQL fires same-event triggers in name order. Application handlers
-- take the same transaction advisory lock before checking active jobs, while
-- this trigger keeps future/direct job producers inside the same protocol.
CREATE OR REPLACE FUNCTION lock_release_job_target() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(lower(NEW.lock_key), 20260827));
    PERFORM 1
    FROM release_jobs job
    WHERE lower(job.lock_key)=lower(NEW.lock_key)
      AND job.status NOT IN ('SUCCESS','FAILED','ROLLED_BACK')
    ORDER BY job.created_at,job.id
    LIMIT 1
    FOR UPDATE;
    IF FOUND THEN
        RAISE EXCEPTION 'release target already has an active deployment job';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS release_job_00_target_lock ON release_jobs;
CREATE TRIGGER release_job_00_target_lock
BEFORE INSERT ON release_jobs
FOR EACH ROW EXECUTE FUNCTION lock_release_job_target();

CREATE OR REPLACE FUNCTION validate_deployment_preset_binding() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1
    FROM applications application
    JOIN environments environment
      ON environment.id=NEW.environment_id
     AND environment.application_id=application.id
    JOIN deployment_profiles profile
      ON profile.id=NEW.profile_id
     AND profile.application_id=application.id
     AND profile.environment_id=environment.id
    WHERE application.id=NEW.application_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'deployment preset application, environment, and profile do not match';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS deployment_preset_binding_check ON deployment_presets;
CREATE TRIGGER deployment_preset_binding_check
BEFORE INSERT OR UPDATE OF application_id,environment_id,profile_id ON deployment_presets
FOR EACH ROW EXECUTE FUNCTION validate_deployment_preset_binding();

ALTER TABLE releases
    ADD COLUMN IF NOT EXISTS deployment_preset_id UUID REFERENCES deployment_presets(id),
    ADD COLUMN IF NOT EXISTS quick_release BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS auto_deploy_after_approval BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS quick_base_release_id UUID REFERENCES releases(id),
    ADD COLUMN IF NOT EXISTS quick_base_job_id UUID REFERENCES release_jobs(id);

DO $$ BEGIN
    ALTER TABLE releases ADD CONSTRAINT releases_quick_preset_required
        CHECK (NOT quick_release OR deployment_preset_id IS NOT NULL);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE releases ADD CONSTRAINT releases_auto_deploy_quick_only
        CHECK (NOT auto_deploy_after_approval OR quick_release);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE releases ADD CONSTRAINT releases_quick_base_pair
        CHECK ((quick_base_release_id IS NULL)=(quick_base_job_id IS NULL));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE releases ADD CONSTRAINT releases_quick_base_only
        CHECK (quick_release OR (quick_base_release_id IS NULL AND quick_base_job_id IS NULL));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE OR REPLACE FUNCTION guard_quick_release_binding() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.quick_release IS DISTINCT FROM OLD.quick_release THEN
        RAISE EXCEPTION 'release source cannot be changed';
    END IF;
    IF OLD.quick_release AND (
        NEW.application_id IS DISTINCT FROM OLD.application_id OR
        NEW.environment_id IS DISTINCT FROM OLD.environment_id OR
        NEW.profile_id IS DISTINCT FROM OLD.profile_id OR
        NEW.version IS DISTINCT FROM OLD.version OR
        NEW.deployment_preset_id IS DISTINCT FROM OLD.deployment_preset_id OR
        NEW.auto_deploy_after_approval IS DISTINCT FROM OLD.auto_deploy_after_approval OR
        NEW.quick_base_release_id IS DISTINCT FROM OLD.quick_base_release_id OR
        NEW.quick_base_job_id IS DISTINCT FROM OLD.quick_base_job_id
    ) THEN
        RAISE EXCEPTION 'quick release preset binding is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION validate_quick_release_preset_binding() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.quick_release THEN
        PERFORM 1 FROM deployment_presets preset
        WHERE preset.id=NEW.deployment_preset_id
          AND preset.application_id=NEW.application_id
          AND preset.environment_id=NEW.environment_id
          AND preset.profile_id=NEW.profile_id
          AND (TG_OP<>'INSERT' OR (preset.active AND preset.revoked_at IS NULL));
        IF NOT FOUND THEN
            RAISE EXCEPTION 'quick release does not match its deployment preset snapshot';
        END IF;
        IF NEW.quick_base_release_id IS NULL THEN
            IF EXISTS(
                SELECT 1 FROM deployment_heads head
                WHERE head.application_id=NEW.application_id
                  AND head.environment_id=NEW.environment_id
            ) THEN
                RAISE EXCEPTION 'quick release omitted the current deployment head';
            END IF;
        ELSE
            PERFORM 1
            FROM deployment_heads head
            JOIN releases base_release ON base_release.id=head.current_release_id
            JOIN release_jobs base_job ON base_job.id=head.current_job_id
            WHERE head.application_id=NEW.application_id
              AND head.environment_id=NEW.environment_id
              AND head.current_release_id=NEW.quick_base_release_id
              AND head.current_job_id=NEW.quick_base_job_id
              AND base_release.application_id=NEW.application_id
              AND base_release.environment_id=NEW.environment_id
              AND base_job.release_id=base_release.id
              AND base_job.operation='DEPLOY'
              AND base_job.status='SUCCESS';
            IF NOT FOUND THEN
                RAISE EXCEPTION 'quick release base is not the current verified deployment head';
            END IF;
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS quick_release_preset_binding_check ON releases;
CREATE TRIGGER quick_release_preset_binding_check
BEFORE INSERT OR UPDATE OF quick_release,deployment_preset_id,application_id,environment_id,profile_id,quick_base_release_id,quick_base_job_id ON releases
FOR EACH ROW EXECUTE FUNCTION validate_quick_release_preset_binding();

DROP TRIGGER IF EXISTS quick_release_binding_guard ON releases;
CREATE TRIGGER quick_release_binding_guard
BEFORE UPDATE OF quick_release,application_id,environment_id,profile_id,version,deployment_preset_id,auto_deploy_after_approval,quick_base_release_id,quick_base_job_id ON releases
FOR EACH ROW EXECUTE FUNCTION guard_quick_release_binding();

INSERT INTO permissions(code,description) VALUES
    ('admin.presets.read', 'Read deployment presets'),
    ('admin.presets.write', 'Create, change, and revoke deployment presets')
ON CONFLICT (code) DO UPDATE SET description=EXCLUDED.description;

INSERT INTO role_permissions(role_id,permission_code)
SELECT 'role-admin',code FROM permissions
WHERE code IN ('admin.presets.read','admin.presets.write')
ON CONFLICT DO NOTHING;
