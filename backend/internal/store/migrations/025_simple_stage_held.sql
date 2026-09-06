-- Tell the two ways a post-deployment stage can end without running apart.
--
-- SKIPPED means the stage was deferred to the last package of the upload and
-- will run there. HELD means it will not run at all: a package of that upload
-- never deployed, so mirroring the registry and rolling the application over
-- would publish an upload that is only partly in place. The run is failed for
-- exactly that reason, and the operator reading it has to be able to see that
-- nothing was mirrored instead of being told to wait for a later file.
ALTER TABLE simple_runs DROP CONSTRAINT IF EXISTS simple_runs_replication_status_check;
DO $$ BEGIN
    ALTER TABLE simple_runs ADD CONSTRAINT simple_runs_replication_status_check
        CHECK (replication_status IN ('NONE','SKIPPED','HELD','RUNNING','SUCCESS','FAILED','TIMEOUT'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

ALTER TABLE simple_runs DROP CONSTRAINT IF EXISTS simple_runs_app_deploy_status_check;
DO $$ BEGIN
    ALTER TABLE simple_runs ADD CONSTRAINT simple_runs_app_deploy_status_check
        CHECK (app_deploy_status IN ('NONE','SKIPPED','HELD','RUNNING','SUCCESS','FAILED','TIMEOUT'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
