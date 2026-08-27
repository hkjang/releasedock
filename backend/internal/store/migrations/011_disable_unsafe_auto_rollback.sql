-- Automatic rollback previously reused the failed deployment's current
-- artifact and image context. Until a prior deployment snapshot and a
-- post-rollback health verification are available, only explicit audited
-- rollback jobs are safe.
UPDATE deployment_profiles SET auto_rollback=FALSE WHERE auto_rollback=TRUE;

ALTER TABLE deployment_profiles
    DROP CONSTRAINT IF EXISTS deployment_profiles_auto_rollback_disabled;

ALTER TABLE deployment_profiles
    ADD CONSTRAINT deployment_profiles_auto_rollback_disabled
    CHECK (auto_rollback=FALSE);
