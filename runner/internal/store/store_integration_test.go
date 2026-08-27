package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hkjang/releasedock/runner/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFinishJobMaintainsDeploymentHeadChain(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to integration PostgreSQL: %v", err)
	}
	t.Cleanup(adminPool.Close)
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	schema := "releasedock_runner_it_" + hex.EncodeToString(random)
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create integration schema: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dropCancel()
		_, _ = adminPool.Exec(dropCtx, "DROP SCHEMA "+identifier+" CASCADE")
	})

	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, deploymentHeadTestSchema); err != nil {
		t.Fatalf("create deployment-head test schema: %v", err)
	}

	const (
		worker = "runner-head-test"
		appID  = "00000000-0000-0000-0000-000000000001"
		envID  = "00000000-0000-0000-0000-000000000002"
		relA   = "00000000-0000-0000-0000-000000000101"
		relB   = "00000000-0000-0000-0000-000000000102"
		relC   = "00000000-0000-0000-0000-000000000103"
		relD   = "00000000-0000-0000-0000-000000000104"
		jobA   = "00000000-0000-0000-0000-000000000201"
		jobB   = "00000000-0000-0000-0000-000000000202"
		jobC   = "00000000-0000-0000-0000-000000000203"
		jobD   = "00000000-0000-0000-0000-000000000204"
		jobCB  = "00000000-0000-0000-0000-000000000205"
		jobBA  = "00000000-0000-0000-0000-000000000206"
	)
	if _, err := pool.Exec(ctx, `INSERT INTO applications(id) VALUES($1::uuid)`, appID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO environments(id,application_id) VALUES($2::uuid,$1::uuid)`, appID, envID); err != nil {
		t.Fatal(err)
	}
	store := &PGStore{pool: pool, workerID: worker}
	finish := func(releaseID, jobID, operation, sourceID, sourceJobID string, status model.JobStatus) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO releases(id,application_id,environment_id,status)
			VALUES($1::uuid,$2::uuid,$3::uuid,'VERIFYING')
			ON CONFLICT(id) DO UPDATE SET status='VERIFYING',updated_at=clock_timestamp()`, releaseID, appID, envID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO release_jobs
			    (id,release_id,operation,rollback_source_release_id,rollback_source_job_id,status,locked_by,created_at,updated_at)
			VALUES($1::uuid,$2::uuid,$3,NULLIF($4,'')::uuid,NULLIF($5,'')::uuid,'VERIFYING',$6,clock_timestamp(),clock_timestamp())`, jobID, releaseID, operation, sourceID, sourceJobID, worker); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO release_locks(lock_key,job_id,worker_id)
			VALUES('target:' || $1,$1::uuid,$2)`, jobID, worker); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishJob(ctx, jobID, status, "test failure"); err != nil {
			t.Fatalf("finish %s %s: %v", operation, status, err)
		}
	}
	assertHead := func(releaseID, basisJobID string) {
		t.Helper()
		var actualRelease, actualJob string
		if err := pool.QueryRow(ctx, `
			SELECT current_release_id::text,current_job_id::text
			FROM deployment_heads WHERE application_id=$1::uuid AND environment_id=$2::uuid`, appID, envID).
			Scan(&actualRelease, &actualJob); err != nil {
			t.Fatal(err)
		}
		if actualRelease != releaseID || actualJob != basisJobID {
			t.Fatalf("deployment head = %s/%s, want %s/%s", actualRelease, actualJob, releaseID, basisJobID)
		}
	}

	finish(relA, jobA, "DEPLOY", "", "", model.StatusSuccess)
	assertHead(relA, jobA)
	finish(relB, jobB, "DEPLOY", relA, jobA, model.StatusSuccess)
	assertHead(relB, jobB)
	finish(relC, jobC, "DEPLOY", relB, jobB, model.StatusSuccess)
	assertHead(relC, jobC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO release_images(job_id,file_path,source_ref,destination_ref,repository,tag,digest)
		VALUES($1::uuid,'images/api.tar','api:1','harbor.local/project/api:1','api','1',
		       'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa')`, jobB); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	renamed := &model.Job{
		RollbackSourceReleaseID: relB, RollbackSourceJobID: jobB,
		Application: "renamed-application", Version: "renamed-version", Environment: "renamed-environment",
		Profile: model.Profile{ID: "00000000-0000-0000-0000-00000000ffff"},
	}
	loadErr := store.loadRollbackImages(ctx, tx, renamed)
	_ = tx.Rollback(ctx)
	if loadErr != nil || len(renamed.RollbackImages) != 1 {
		t.Fatalf("immutable rollback basis was affected by mutable labels/profile: %v, %#v", loadErr, renamed.RollbackImages)
	}

	// Recovery of a stale newer deployment must not advance or erase the
	// verified head. Replaying the abandoned DEPLOY would be unsafe.
	if _, err := pool.Exec(ctx, `
		INSERT INTO releases(id,application_id,environment_id,status)
		VALUES($1::uuid,$2::uuid,$3::uuid,'DEPLOYING')`, relD, appID, envID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO release_jobs
		    (id,release_id,operation,rollback_source_release_id,rollback_source_job_id,status,locked_by,heartbeat_at,created_at,updated_at)
		VALUES($1::uuid,$2::uuid,'DEPLOY',$3::uuid,$4::uuid,'DEPLOYING',$5,
		       clock_timestamp()-interval '2 hours',clock_timestamp(),clock_timestamp())`, jobD, relD, relC, jobC, worker); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO release_locks(lock_key,job_id,worker_id)
		VALUES('target:' || $1,$1::uuid,$2)`, jobD, worker); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.RecoverStaleJobs(ctx, time.Hour)
	if err != nil || recovered != 1 {
		t.Fatalf("recover stale D = %d, %v", recovered, err)
	}
	assertHead(relC, jobC)

	// Rolling C back to B restores B's successful DEPLOY basis; a subsequent
	// rollback can therefore follow B's immutable prior-head snapshot to A.
	finish(relC, jobCB, "ROLLBACK", relB, jobB, model.StatusRolledBack)
	assertHead(relB, jobB)
	finish(relB, jobBA, "ROLLBACK", relA, jobA, model.StatusRolledBack)
	assertHead(relA, jobA)
}

const deploymentHeadTestSchema = `
	CREATE TABLE applications(id UUID PRIMARY KEY);
	CREATE TABLE environments(id UUID PRIMARY KEY, application_id UUID NOT NULL REFERENCES applications(id));
	CREATE TABLE releases(
		id UUID PRIMARY KEY,
		application_id UUID NOT NULL REFERENCES applications(id),
		environment_id UUID NOT NULL REFERENCES environments(id),
		status TEXT NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
	);
	CREATE TABLE release_jobs(
		id UUID PRIMARY KEY,
		release_id UUID NOT NULL REFERENCES releases(id),
		operation TEXT NOT NULL,
		rollback_source_release_id UUID REFERENCES releases(id),
		rollback_source_job_id UUID REFERENCES release_jobs(id),
		status TEXT NOT NULL,
		locked_by TEXT,
		locked_at TIMESTAMPTZ,
		failure_message TEXT,
		finished_at TIMESTAMPTZ,
		heartbeat_at TIMESTAMPTZ,
		created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
	);
	CREATE TABLE release_locks(
		lock_key TEXT PRIMARY KEY,
		job_id UUID NOT NULL REFERENCES release_jobs(id),
		worker_id TEXT NOT NULL
	);
	CREATE TABLE release_images(
		job_id UUID NOT NULL REFERENCES release_jobs(id),
		file_path TEXT NOT NULL,
		source_ref TEXT NOT NULL,
		destination_ref TEXT NOT NULL,
		repository TEXT NOT NULL,
		tag TEXT NOT NULL,
		digest TEXT,
		PRIMARY KEY(job_id,file_path)
	);
	CREATE TABLE deployment_heads(
		application_id UUID NOT NULL REFERENCES applications(id),
		environment_id UUID NOT NULL REFERENCES environments(id),
		current_release_id UUID NOT NULL REFERENCES releases(id),
		current_job_id UUID NOT NULL UNIQUE REFERENCES release_jobs(id),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
		PRIMARY KEY(application_id,environment_id)
	)`
