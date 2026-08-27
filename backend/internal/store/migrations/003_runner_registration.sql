ALTER TABLE runner_instances ADD COLUMN IF NOT EXISTS worker_id TEXT;
ALTER TABLE runner_instances ADD COLUMN IF NOT EXISTS managed_by_runner BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE runner_instances ALTER COLUMN token_prefix DROP NOT NULL;
ALTER TABLE runner_instances ALTER COLUMN token_hash DROP NOT NULL;
ALTER TABLE runner_instances ALTER COLUMN created_by DROP NOT NULL;
ALTER TABLE runner_instances DROP CONSTRAINT IF EXISTS runner_instances_max_concurrent_jobs_check;
ALTER TABLE runner_instances ADD CONSTRAINT runner_instances_max_concurrent_jobs_check CHECK(max_concurrent_jobs=1);
CREATE UNIQUE INDEX IF NOT EXISTS runner_instances_worker_id_idx ON runner_instances(worker_id) WHERE worker_id IS NOT NULL;
