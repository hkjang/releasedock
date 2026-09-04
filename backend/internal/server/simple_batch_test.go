package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hkjang/releasedock/backend/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newSimpleBatchFixture gives a schema-isolated database with one simple target
// and the user that owns it, which is all the batch question needs.
func newSimpleBatchFixture(t *testing.T) (*Server, string) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	adminPool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect to integration PostgreSQL: %v", err)
	}
	t.Cleanup(adminPool.Close)
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("generate schema suffix: %v", err)
	}
	schema := "releasedock_simple_batch_it_" + hex.EncodeToString(suffix)
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(t.Context(), "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create integration schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE")
	})
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse integration DSN: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatalf("open schema-isolated pool: %v", err)
	}
	st := &store.Store{Pool: pool}
	t.Cleanup(st.Close)
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate simple batch fixture: %v", err)
	}
	const targetID = "aa000000-0000-4000-8000-000000000001"
	if _, err := st.Pool.Exec(t.Context(),
		`INSERT INTO users(id,username,display_name) VALUES('batch-actor','batch-actor','Batch actor')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := st.Pool.Exec(t.Context(),
		`INSERT INTO simple_targets(id,name,upload_dir,command_path,timeout_seconds,created_by)
		 VALUES($1,'batch-target','/var/lib/releasedock/simple/batch','/opt/deploy/run.sh',600,'batch-actor')`,
		targetID); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	return &Server{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}, targetID
}

func seedSimpleRun(t *testing.T, s *Server, targetID, runID, batchID, status string, last bool) {
	t.Helper()
	if _, err := s.store.Pool.Exec(t.Context(),
		`INSERT INTO simple_runs(id,target_id,actor_id,original_filename,stored_path,size_bytes,sha256,
			command_source,resolved_command_path,resolved_timeout_seconds,batch_id,batch_last,status)
		 VALUES($1,$2,'batch-actor',$3,'/var/lib/releasedock/simple/batch/'||$3,1,repeat('a',64),
			'PER_TARGET','/opt/deploy/run.sh',600,$4,$5,$6)`,
		runID, targetID, runID+".tar.gz", batchID, last, status); err != nil {
		t.Fatalf("seed run %s: %v", runID, err)
	}
}

// The stages deferred to the last package of an upload act on the upload as a
// whole, so the question they depend on is about the other packages, not this
// run: did every one of them deploy?
func TestUploadHasFailedPackagesLooksAtTheRestOfTheBatch(t *testing.T) {
	s, targetID := newSimpleBatchFixture(t)
	const batch = "batch-one"
	const marked = "bb000000-0000-4000-8000-000000000003"
	seedSimpleRun(t, s, targetID, "bb000000-0000-4000-8000-000000000001", batch, "SUCCESS", false)
	seedSimpleRun(t, s, targetID, "bb000000-0000-4000-8000-000000000002", batch, "SUCCESS", false)
	seedSimpleRun(t, s, targetID, marked, batch, "RUNNING", true)

	// The run asking the question is still RUNNING and must not count itself.
	if s.uploadHasFailedPackages(t.Context(), batch, marked) {
		t.Fatal("an upload whose other packages all deployed must not be reported as incomplete")
	}

	// A package that timed out never loaded its images, so the registry is
	// missing what it carried.
	seedSimpleRun(t, s, targetID, "bb000000-0000-4000-8000-000000000004", batch, "TIMEOUT", false)
	if !s.uploadHasFailedPackages(t.Context(), batch, marked) {
		t.Fatal("a package that did not deploy must hold the deferred stages back")
	}

	// Another upload's failures are none of this one's business.
	if s.uploadHasFailedPackages(t.Context(), "batch-two", marked) {
		t.Fatal("an unrelated batch must not be counted")
	}
}
