CREATE TABLE IF NOT EXISTS target_credentials (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    credential_type TEXT NOT NULL CHECK(credential_type IN ('SSH_PRIVATE_KEY','KUBECONFIG','TOKEN','OPAQUE_FILE')),
    version INTEGER NOT NULL DEFAULT 1 CHECK(version > 0),
    ciphertext TEXT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    approved_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    approved_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    revoked_at TIMESTAMPTZ,
    created_by TEXT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE deployment_profiles ADD COLUMN IF NOT EXISTS target_credential_id UUID;
ALTER TABLE release_jobs ADD COLUMN IF NOT EXISTS target_credential_id UUID;
ALTER TABLE release_jobs ADD COLUMN IF NOT EXISTS target_credential_version INTEGER;

DO $$ BEGIN
    ALTER TABLE deployment_profiles ADD CONSTRAINT deployment_profiles_target_credential_fk
        FOREIGN KEY(target_credential_id) REFERENCES target_credentials(id) ON DELETE SET NULL;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE release_jobs ADD CONSTRAINT release_jobs_target_credential_fk
        FOREIGN KEY(target_credential_id) REFERENCES target_credentials(id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

INSERT INTO permissions(code,description) VALUES
    ('admin.credentials.read','Read deployment target credential metadata'),
    ('admin.credentials.write','Create, rotate, bind, and revoke encrypted deployment target credentials')
ON CONFLICT(code) DO UPDATE SET description=EXCLUDED.description;
INSERT INTO role_permissions(role_id,permission_code)
SELECT 'role-admin',code FROM permissions WHERE code IN ('admin.credentials.read','admin.credentials.write')
ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION guard_target_credential_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS(
        SELECT 1 FROM deployment_profiles p
        WHERE p.target_credential_id=OLD.id AND profile_has_frozen_execution(p.id)
    ) THEN
        RAISE EXCEPTION 'target credential is locked by a reviewed or active release';
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS active_job_target_credential_guard ON target_credentials;
CREATE TRIGGER active_job_target_credential_guard
BEFORE UPDATE OR DELETE ON target_credentials
FOR EACH ROW EXECUTE FUNCTION guard_target_credential_change();

CREATE OR REPLACE FUNCTION validate_release_job_target_credential() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    profile_credential UUID;
    profile_version INTEGER;
BEGIN
    SELECT p.target_credential_id INTO profile_credential
    FROM deployment_profiles p WHERE p.id=NEW.profile_id FOR SHARE;

    IF profile_credential IS NULL THEN
        IF NEW.target_credential_id IS NOT NULL OR NEW.target_credential_version IS NOT NULL THEN
            RAISE EXCEPTION 'job target credential does not match deployment profile';
        END IF;
    ELSE
        SELECT version INTO profile_version FROM target_credentials
        WHERE id=profile_credential AND active AND approved_at IS NOT NULL AND revoked_at IS NULL FOR SHARE;
        IF profile_version IS NULL THEN
            RAISE EXCEPTION 'deployment target credential is inactive or revoked';
        ELSIF NEW.target_credential_id IS DISTINCT FROM profile_credential OR NEW.target_credential_version IS DISTINCT FROM profile_version THEN
            RAISE EXCEPTION 'job target credential snapshot does not match deployment profile';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS release_job_target_credential_check ON release_jobs;
CREATE TRIGGER release_job_target_credential_check
BEFORE INSERT ON release_jobs
FOR EACH ROW EXECUTE FUNCTION validate_release_job_target_credential();

CREATE OR REPLACE FUNCTION validate_release_approval_target_credential() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE effective_profile UUID; bound_credential UUID;
BEGIN
    IF NEW.status<>'APPROVED' OR OLD.status='APPROVED' THEN RETURN NEW; END IF;
    IF NEW.requested_operation='ROLLBACK' THEN
        SELECT profile_id INTO effective_profile FROM releases WHERE id=NEW.rollback_source_release_id;
    ELSE
        effective_profile:=NEW.profile_id;
    END IF;
    SELECT target_credential_id INTO bound_credential FROM deployment_profiles WHERE id=effective_profile FOR SHARE;
    IF NOT FOUND THEN RAISE EXCEPTION 'deployment profile is missing'; END IF;
    IF bound_credential IS NOT NULL THEN
        PERFORM 1 FROM target_credentials WHERE id=bound_credential AND active AND approved_at IS NOT NULL AND revoked_at IS NULL FOR SHARE;
        IF NOT FOUND THEN RAISE EXCEPTION 'deployment target credential is inactive or revoked'; END IF;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS release_approval_target_credential_check ON releases;
CREATE TRIGGER release_approval_target_credential_check
BEFORE UPDATE OF status ON releases
FOR EACH ROW EXECUTE FUNCTION validate_release_approval_target_credential();
