-- Make the two post-deployment stages configurable: how often the Harbor
-- replication rule fires when several packages are uploaded at once, and
-- whether an application deployment command runs after it.
--
-- Uploading several packages produces one run per file, so "replicate once"
-- has to mean "replicate on the last run of the upload batch": every package
-- is uploaded and its command has already finished by then, because the runs
-- of a batch are strictly sequential. The batch is declared by the client that
-- started the runs; a run with no batch counts as its own last one, which
-- keeps single-file uploads and API clients unchanged. ONCE is the default
-- because mirroring once per upload is what a multi-package deployment means.
ALTER TABLE simple_settings
    ADD COLUMN IF NOT EXISTS replication_scope TEXT NOT NULL DEFAULT 'ONCE',
    ADD COLUMN IF NOT EXISTS app_deploy_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS app_deploy_scope TEXT NOT NULL DEFAULT 'ONCE',
    ADD COLUMN IF NOT EXISTS app_deploy_command_path TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS app_deploy_command_args TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS app_deploy_working_dir TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS app_deploy_timeout_seconds INTEGER NOT NULL DEFAULT 600;

DO $$ BEGIN
    ALTER TABLE simple_settings ADD CONSTRAINT simple_settings_replication_scope
        CHECK (replication_scope IN ('EACH','ONCE'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    ALTER TABLE simple_settings ADD CONSTRAINT simple_settings_app_deploy_scope
        CHECK (app_deploy_scope IN ('EACH','ONCE'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    ALTER TABLE simple_settings ADD CONSTRAINT simple_settings_app_deploy_timeout
        CHECK (app_deploy_timeout_seconds BETWEEN 1 AND 86400);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- Enabling without a command would fail on every run, so the pairing is a
-- storage-level invariant rather than only an API check.
DO $$ BEGIN
    ALTER TABLE simple_settings ADD CONSTRAINT simple_settings_app_deploy_command
        CHECK (NOT app_deploy_enabled OR app_deploy_command_path LIKE '/%');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- Which upload the run belongs to, and whether it is the one that carries the
-- once-per-batch stages. Defaulting batch_last to TRUE means every existing
-- row, and every run created without batch fields, behaves as it did before.
ALTER TABLE simple_runs
    ADD COLUMN IF NOT EXISTS batch_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS batch_last BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS app_deploy_status TEXT NOT NULL DEFAULT 'NONE',
    ADD COLUMN IF NOT EXISTS app_deploy_error TEXT NOT NULL DEFAULT '';

-- SKIPPED joins the replication states: a run that deliberately did not
-- replicate must be distinguishable from one where replication was off.
ALTER TABLE simple_runs DROP CONSTRAINT IF EXISTS simple_runs_replication_status_check;
DO $$ BEGIN
    ALTER TABLE simple_runs ADD CONSTRAINT simple_runs_replication_status_check
        CHECK (replication_status IN ('NONE','SKIPPED','RUNNING','SUCCESS','FAILED','TIMEOUT'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    ALTER TABLE simple_runs ADD CONSTRAINT simple_runs_app_deploy_status_check
        CHECK (app_deploy_status IN ('NONE','SKIPPED','RUNNING','SUCCESS','FAILED','TIMEOUT'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE INDEX IF NOT EXISTS simple_runs_batch_idx ON simple_runs(batch_id, created_at)
    WHERE batch_id <> '';

-- The batch columns are provenance: the executor must not be able to move a
-- run into another batch, or make a middle run claim the once-per-batch work.
CREATE OR REPLACE FUNCTION guard_simple_run_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status IN ('SUCCESS','FAILED','TIMEOUT') THEN
        RAISE EXCEPTION 'simple run % is already terminal', OLD.id;
    END IF;
    IF NEW.target_id IS DISTINCT FROM OLD.target_id
       OR NEW.actor_id IS DISTINCT FROM OLD.actor_id
       OR NEW.stored_path IS DISTINCT FROM OLD.stored_path
       OR NEW.sha256 IS DISTINCT FROM OLD.sha256
       OR NEW.command_source IS DISTINCT FROM OLD.command_source
       OR NEW.resolved_command_path IS DISTINCT FROM OLD.resolved_command_path
       OR NEW.resolved_command_args IS DISTINCT FROM OLD.resolved_command_args
       OR NEW.batch_id IS DISTINCT FROM OLD.batch_id
       OR NEW.batch_last IS DISTINCT FROM OLD.batch_last THEN
        RAISE EXCEPTION 'simple run provenance is immutable';
    END IF;
    RETURN NEW;
END;
$$;
