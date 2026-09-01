-- Trigger a Harbor replication rule once a simple-mode command succeeds.
--
-- The rule is global rather than per target: a deployment that has to be
-- mirrored usually mirrors to the same destination regardless of which service
-- was deployed, and one setting is far easier to keep correct.
ALTER TABLE simple_settings
    ADD COLUMN IF NOT EXISTS replication_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS replication_registry_id UUID REFERENCES runner_credentials(id),
    ADD COLUMN IF NOT EXISTS replication_policy_id BIGINT,
    ADD COLUMN IF NOT EXISTS replication_policy_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS replication_timeout_seconds INTEGER NOT NULL DEFAULT 900;

DO $$ BEGIN
    ALTER TABLE simple_settings ADD CONSTRAINT simple_settings_replication_timeout
        CHECK (replication_timeout_seconds BETWEEN 1 AND 86400);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- Enabling without a target would fail on every run, so the pairing is a
-- storage-level invariant rather than only an API check.
DO $$ BEGIN
    ALTER TABLE simple_settings ADD CONSTRAINT simple_settings_replication_target
        CHECK (NOT replication_enabled OR (replication_registry_id IS NOT NULL AND replication_policy_id IS NOT NULL));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- Per-run outcome, so the history shows what happened to the replication that
-- a given deployment kicked off.
ALTER TABLE simple_runs
    ADD COLUMN IF NOT EXISTS replication_status TEXT NOT NULL DEFAULT 'NONE'
        CHECK (replication_status IN ('NONE','RUNNING','SUCCESS','FAILED','TIMEOUT')),
    ADD COLUMN IF NOT EXISTS replication_execution_id BIGINT,
    ADD COLUMN IF NOT EXISTS replication_error TEXT NOT NULL DEFAULT '';
