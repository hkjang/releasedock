package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

func TestFreshDatabaseMigrationsAndBootstrap(t *testing.T) {
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
		t.Fatalf("generate schema suffix: %v", err)
	}
	schema := "releasedock_it_" + hex.EncodeToString(random)
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
		t.Fatalf("parse integration DSN: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("open schema-isolated pool: %v", err)
	}
	st := &Store{Pool: pool}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate a fresh database: %v", err)
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	wantMigrations := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			wantMigrations++
		}
	}
	var appliedMigrations int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&appliedMigrations); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	if appliedMigrations != wantMigrations {
		t.Fatalf("applied migration count = %d, want %d", appliedMigrations, wantMigrations)
	}

	const originalPassword = "integration-password-1"
	if err := st.BootstrapAdmin(ctx, "Admin", originalPassword); err != nil {
		t.Fatalf("bootstrap administrator: %v", err)
	}
	var userID, originalHash string
	var administrator bool
	if err := st.Pool.QueryRow(ctx, `
		SELECT u.id,u.password_hash,EXISTS(
			SELECT 1 FROM user_roles ur WHERE ur.user_id=u.id AND ur.role_id='role-admin'
		)
		FROM users u WHERE u.username='admin'`).Scan(&userID, &originalHash, &administrator); err != nil {
		t.Fatalf("load bootstrap administrator: %v", err)
	}
	if userID == "" || !administrator || bcrypt.CompareHashAndPassword([]byte(originalHash), []byte(originalPassword)) != nil {
		t.Fatal("bootstrap account was not created with the protected Administrator role and password")
	}

	if err := st.BootstrapAdmin(ctx, "admin", "different-password-2"); err != nil {
		t.Fatalf("repeat bootstrap administrator: %v", err)
	}
	var currentHash string
	if err := st.Pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id=$1`, userID).Scan(&currentHash); err != nil {
		t.Fatalf("reload bootstrap password: %v", err)
	}
	if currentHash != originalHash {
		t.Fatal("repeated bootstrap unexpectedly replaced the administrator password")
	}
	testRunnerSettingsPersistence(t, ctx, st)
	testVerifiedDeploymentHeadChain(t, ctx, st, userID)
}

func testRunnerSettingsPersistence(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	var poll, lockRetry, refresh, heartbeat, stale, chunk int
	if err := st.Pool.QueryRow(ctx, `
		SELECT poll_interval_ms,lock_retry_ms,settings_refresh_ms,heartbeat_interval_ms,
		       stale_job_after_ms,log_chunk_bytes
		FROM runner_settings WHERE singleton=TRUE`).Scan(&poll, &lockRetry, &refresh, &heartbeat, &stale, &chunk); err != nil {
		t.Fatalf("load default Runner settings: %v", err)
	}
	if poll != 2_000 || lockRetry != 5_000 || refresh != 30_000 || heartbeat != 5_000 || stale != 60_000 || chunk != 16_384 {
		t.Fatalf("unexpected default Runner settings: %d %d %d %d %d %d", poll, lockRetry, refresh, heartbeat, stale, chunk)
	}
	if _, err := st.Pool.Exec(ctx, `
		UPDATE runner_settings SET poll_interval_ms=3000,lock_retry_ms=7000,
			settings_refresh_ms=45000,heartbeat_interval_ms=6000,
			stale_job_after_ms=75000,log_chunk_bytes=32768,updated_at=clock_timestamp()
		WHERE singleton=TRUE`); err != nil {
		t.Fatalf("persist Runner settings: %v", err)
	}
	if err := st.Pool.QueryRow(ctx, `
		SELECT poll_interval_ms,lock_retry_ms,settings_refresh_ms,heartbeat_interval_ms,
		       stale_job_after_ms,log_chunk_bytes
		FROM runner_settings WHERE singleton=TRUE`).Scan(&poll, &lockRetry, &refresh, &heartbeat, &stale, &chunk); err != nil {
		t.Fatalf("reload Runner settings: %v", err)
	}
	if poll != 3_000 || lockRetry != 7_000 || refresh != 45_000 || heartbeat != 6_000 || stale != 75_000 || chunk != 32_768 {
		t.Fatalf("Runner settings did not persist: %d %d %d %d %d %d", poll, lockRetry, refresh, heartbeat, stale, chunk)
	}
}

func testVerifiedDeploymentHeadChain(t *testing.T, ctx context.Context, st *Store, userID string) {
	t.Helper()
	const (
		applicationID    = "10000000-0000-4000-8000-000000000001"
		environmentID    = "20000000-0000-4000-8000-000000000001"
		profileID        = "30000000-0000-4000-8000-000000000001"
		credentialID     = "40000000-0000-4000-8000-000000000001"
		deployScript     = "50000000-0000-4000-8000-000000000001"
		rollbackScript   = "50000000-0000-4000-8000-000000000002"
		runnerID         = "60000000-0000-4000-8000-000000000001"
		releaseA         = "70000000-0000-4000-8000-000000000001"
		releaseB         = "70000000-0000-4000-8000-000000000002"
		releaseC         = "70000000-0000-4000-8000-000000000003"
		releaseD         = "70000000-0000-4000-8000-000000000004"
		releaseE         = "70000000-0000-4000-8000-000000000005"
		releaseF         = "70000000-0000-4000-8000-000000000006"
		releaseG         = "70000000-0000-4000-8000-000000000007"
		releaseH         = "70000000-0000-4000-8000-000000000008"
		artifactA        = "80000000-0000-4000-8000-000000000001"
		artifactB        = "80000000-0000-4000-8000-000000000002"
		artifactC        = "80000000-0000-4000-8000-000000000003"
		artifactD        = "80000000-0000-4000-8000-000000000004"
		artifactE        = "80000000-0000-4000-8000-000000000005"
		artifactF        = "80000000-0000-4000-8000-000000000006"
		artifactG        = "80000000-0000-4000-8000-000000000007"
		artifactH        = "80000000-0000-4000-8000-000000000008"
		jobA             = "90000000-0000-4000-8000-000000000001"
		jobB             = "90000000-0000-4000-8000-000000000002"
		jobC             = "90000000-0000-4000-8000-000000000003"
		jobD             = "90000000-0000-4000-8000-000000000004"
		failedRollbackC  = "90000000-0000-4000-8000-000000000005"
		rollbackD        = "90000000-0000-4000-8000-000000000006"
		jobE             = "90000000-0000-4000-8000-000000000007"
		recoverE         = "90000000-0000-4000-8000-000000000008"
		rollbackC        = "90000000-0000-4000-8000-000000000009"
		rollbackB        = "90000000-0000-4000-8000-00000000000a"
		jobF             = "90000000-0000-4000-8000-00000000000c"
		jobG             = "90000000-0000-4000-8000-00000000000d"
		jobH             = "90000000-0000-4000-8000-00000000000e"
		targetCredential = "a0000000-0000-4000-8000-000000000001"
	)

	seed := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO applications(id,code,name,created_by) VALUES($1,'head-test','Head test',$2)`, []any{applicationID, userID}},
		{`INSERT INTO environments(id,application_id,code,name,created_by) VALUES($1,$2,'prod','Production',$3)`, []any{environmentID, applicationID, userID}},
		{`INSERT INTO runner_credentials(id,name,endpoint,project,username,ciphertext,approved_at,approved_by,created_by) VALUES($1,'head-registry','https://registry.example.test','releasedock','robot','encrypted',now(),$2,$2)`, []any{credentialID, userID}},
		{`INSERT INTO deployment_profiles(id,application_id,environment_id,name,registry_url,registry_host,registry_project,registry_credential_id,created_by) VALUES($1,$2,$3,'head-profile','https://registry.example.test','registry.example.test','releasedock',$4,$5)`, []any{profileID, applicationID, environmentID, credentialID, userID}},
		{`INSERT INTO script_versions(id,name,script_type,version,interpreter_path,content,sha256,approved_at,approved_by,created_by) VALUES($1,'deploy','DEPLOY',1,'/bin/sh','exit 0',repeat('a',64),now(),$3,$3),($2,'rollback','ROLLBACK',1,'/bin/sh','exit 0',repeat('b',64),now(),$3,$3)`, []any{deployScript, rollbackScript, userID}},
		{`INSERT INTO deployment_profile_scripts(profile_id,script_version_id,phase,execution_order) VALUES($1,$2,'DEPLOY',1),($1,$3,'ROLLBACK',2)`, []any{profileID, deployScript, rollbackScript}},
		{`INSERT INTO runner_instances(id,worker_id,name,address,managed_by_runner,last_heartbeat_at,created_by) VALUES($1,'head-worker','head-worker','direct-db',TRUE,clock_timestamp(),$2)`, []any{runnerID, userID}},
	}
	for _, statement := range seed {
		if _, err := st.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed deployment head dependencies: %v", err)
		}
	}

	releases := []struct{ id, version, artifact string }{
		{releaseA, "A", artifactA}, {releaseB, "B", artifactB}, {releaseC, "C", artifactC}, {releaseD, "D", artifactD}, {releaseE, "E", artifactE},
		{releaseF, "F", artifactF}, {releaseG, "G", artifactG}, {releaseH, "H", artifactH},
	}
	for _, release := range releases {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO releases(id,application_id,environment_id,profile_id,version,created_by) VALUES($1,$2,$3,$4,$5,$6)`, release.id, applicationID, environmentID, profileID, release.version, userID); err != nil {
			t.Fatalf("insert release %s: %v", release.version, err)
		}
		if _, err := st.Pool.Exec(ctx, `INSERT INTO release_artifacts(id,release_id,original_filename,storage_path,size_bytes,sha256,uploaded_by) VALUES($1,$2,$3,$4,1,repeat('c',64),$5)`, release.artifact, release.id, release.version+".tar", release.id+"/"+release.artifact+".tar", userID); err != nil {
			t.Fatalf("insert artifact %s: %v", release.version, err)
		}
	}

	insertJob := func(jobID, releaseID, artifactID, operation string, sourceRelease, sourceJob any) {
		t.Helper()
		_, err := st.Pool.Exec(ctx, `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,operation,rollback_source_release_id,rollback_source_job_id,created_by)
			SELECT $1,$2,$3,'head-test',r.version,'prod',$4,$5,a.storage_path,a.sha256,$6,$7,$8,$9
			FROM releases r JOIN release_artifacts a ON a.id=$5 WHERE r.id=$2`, jobID, releaseID, profileID, applicationID+":"+environmentID, artifactID, operation, sourceRelease, sourceJob, userID)
		if err != nil {
			t.Fatalf("insert %s job %s: %v", operation, jobID, err)
		}
	}
	finishJob := func(jobID, status string) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, `UPDATE release_jobs SET status=$2,finished_at=clock_timestamp() WHERE id=$1`, jobID, status); err != nil {
			t.Fatalf("finish job %s as %s: %v", jobID, status, err)
		}
	}
	assertHead := func(releaseID, jobID string) {
		t.Helper()
		var gotRelease, gotJob string
		if err := st.Pool.QueryRow(ctx, `SELECT current_release_id::text,current_job_id::text FROM deployment_heads WHERE application_id=$1 AND environment_id=$2`, applicationID, environmentID).Scan(&gotRelease, &gotJob); err != nil {
			t.Fatalf("load deployment head: %v", err)
		}
		if gotRelease != releaseID || gotJob != jobID {
			t.Fatalf("deployment head=(%s,%s), want (%s,%s)", gotRelease, gotJob, releaseID, jobID)
		}
	}

	insertJob(jobA, releaseA, artifactA, "DEPLOY", nil, nil)
	finishJob(jobA, "SUCCESS")
	assertHead(releaseA, jobA)
	insertJob(jobB, releaseB, artifactB, "DEPLOY", releaseA, jobA)
	finishJob(jobB, "SUCCESS")
	insertJob(jobC, releaseC, artifactC, "DEPLOY", releaseB, jobB)
	finishJob(jobC, "SUCCESS")
	assertHead(releaseC, jobC)

	insertJob(failedRollbackC, releaseC, artifactB, "ROLLBACK", releaseB, jobB)
	finishJob(failedRollbackC, "FAILED")
	assertHead(releaseC, jobC)
	insertJob(jobD, releaseD, artifactD, "DEPLOY", releaseC, jobC)
	finishJob(jobD, "SUCCESS")
	assertHead(releaseD, jobD)
	insertJob(rollbackD, releaseD, artifactC, "ROLLBACK", releaseC, jobC)
	finishJob(rollbackD, "ROLLED_BACK")
	assertHead(releaseC, jobC)

	insertJob(jobE, releaseE, artifactE, "DEPLOY", releaseC, jobC)
	finishJob(jobE, "FAILED")
	assertHead(releaseC, jobC)
	insertJob(recoverE, releaseE, artifactC, "ROLLBACK", releaseC, jobC)
	finishJob(recoverE, "ROLLED_BACK")
	assertHead(releaseC, jobC)

	insertJob(rollbackC, releaseC, artifactB, "ROLLBACK", releaseB, jobB)
	finishJob(rollbackC, "ROLLED_BACK")
	assertHead(releaseB, jobB)
	insertJob(rollbackB, releaseB, artifactA, "ROLLBACK", releaseA, jobA)
	finishJob(rollbackB, "ROLLED_BACK")
	assertHead(releaseA, jobA)

	// A retry request freezes the exact failed DEPLOY job across approval. A
	// newer successful deployment makes both approval and direct retry stale.
	insertJob(jobF, releaseF, artifactF, "DEPLOY", releaseA, jobA)
	finishJob(jobF, "FAILED")
	if _, err := st.Pool.Exec(ctx, `UPDATE app_settings SET approval_enabled=TRUE`); err != nil {
		t.Fatalf("enable global retry approval fixture: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE deployment_profiles SET approval_required=TRUE WHERE id=$1`, profileID); err != nil {
		t.Fatalf("enable retry approval fixture: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE releases SET status='PENDING_REVIEW',retry_source_job_id=$2 WHERE id=$1`, releaseF, jobF); err != nil {
		t.Fatalf("create retry approval request: %v", err)
	}
	insertJob(jobG, releaseG, artifactG, "DEPLOY", releaseA, jobA)
	finishJob(jobG, "SUCCESS")
	assertHead(releaseG, jobG)
	if _, err := st.Pool.Exec(ctx, `UPDATE releases SET status='APPROVED' WHERE id=$1`, releaseF); err == nil {
		t.Fatal("database approved a retry after a newer deployment")
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,operation,rollback_source_release_id,rollback_source_job_id,retry_of_job_id,created_by)
		SELECT '90000000-0000-4000-8000-00000000000f',$1,$2,'head-test','F','prod',$3,$4,a.storage_path,a.sha256,'DEPLOY',$5,$6,$7,$8 FROM release_artifacts a WHERE a.id=$4`, releaseF, profileID, applicationID+":"+environmentID, artifactF, releaseG, jobG, jobF, userID); err == nil {
		t.Fatal("database queued a retry whose failed source was no longer the latest attempt")
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE releases SET status='FAILED',retry_source_job_id=NULL WHERE id=$1`, releaseF); err != nil {
		t.Fatalf("clear stale retry fixture: %v", err)
	}

	// Bound target credentials must be snapshot by exact ID and version.
	if _, err := st.Pool.Exec(ctx, `INSERT INTO target_credentials(id,name,credential_type,version,ciphertext,approved_by,created_by) VALUES($1,'head-target','TOKEN',1,'encrypted',$2,$2)`, targetCredential, userID); err != nil {
		t.Fatalf("create target credential fixture: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE deployment_profiles SET target_credential_id=$1 WHERE id=$2`, targetCredential, profileID); err != nil {
		t.Fatalf("bind target credential fixture: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,operation,rollback_source_release_id,rollback_source_job_id,created_by)
		SELECT $1,$2,$3,'head-test','H','prod',$4,$5,a.storage_path,a.sha256,'DEPLOY',$6,$7,$8 FROM release_artifacts a WHERE a.id=$5`, jobH, releaseH, profileID, applicationID+":"+environmentID, artifactH, releaseG, jobG, userID); err == nil {
		t.Fatal("database queued a job without its bound target credential snapshot")
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,operation,rollback_source_release_id,rollback_source_job_id,target_credential_id,target_credential_version,created_by)
		SELECT $1,$2,$3,'head-test','H','prod',$4,$5,a.storage_path,a.sha256,'DEPLOY',$6,$7,$8,1,$9 FROM release_artifacts a WHERE a.id=$5`, jobH, releaseH, profileID, applicationID+":"+environmentID, artifactH, releaseG, jobG, targetCredential, userID); err != nil {
		t.Fatalf("queue exact target credential snapshot: %v", err)
	}
	finishJob(jobH, "FAILED")

	if _, err := st.Pool.Exec(ctx, `UPDATE deployment_profiles SET auto_rollback=TRUE WHERE id=$1`, profileID); err == nil {
		t.Fatal("database accepted unsafe auto_rollback=true")
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,operation,rollback_source_release_id,rollback_source_job_id,created_by)
		SELECT '90000000-0000-4000-8000-00000000000b',$1,$2,'head-test','B','prod',$3,$4,a.storage_path,a.sha256,'ROLLBACK',$5,$6,$7 FROM release_artifacts a WHERE a.id=$4`, releaseC, profileID, applicationID+":"+environmentID, artifactB, releaseB, jobB, userID); err == nil {
		t.Fatal("database accepted rollback of a stale historical release")
	}
}
