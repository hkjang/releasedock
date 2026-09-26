package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/hkjang/releasedock/backend/internal/store"
)

// newSimpleStreamFixture adds a browser session that may read the seeded run to
// the shared fixture, which is all the log stream route needs beyond it: the
// per-user stream accounting the route goes through is already wired because
// the shared fixture builds its server the way the binary does.
func newSimpleStreamFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	s, targetID := newSimpleBatchFixture(t)
	return s, targetID, simpleStreamSession(t, s)
}

// simpleStreamSession gives batch-actor a browser session that may read runs,
// which is what the log stream route is gated on.
func simpleStreamSession(t *testing.T, s *Server) string {
	t.Helper()
	if _, err := s.store.Pool.Exec(t.Context(),
		`INSERT INTO user_roles(user_id,role_id) VALUES('batch-actor','role-viewer')`); err != nil {
		t.Fatalf("grant simple.read: %v", err)
	}
	token, err := secure.RandomToken(32)
	if err != nil {
		t.Fatal(err)
	}
	csrf, err := secure.RandomToken(24)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Pool.Exec(t.Context(),
		`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at)
		 VALUES($1,'batch-actor',$2,now()+interval '1 hour')`,
		secure.TokenHash(token), secure.TokenHash(csrf)); err != nil {
		t.Fatalf("create stream session: %v", err)
	}
	return token
}

func seedSimpleRunLogs(t *testing.T, s *Server, runID string, lines int) {
	t.Helper()
	if _, err := s.store.Pool.Exec(t.Context(),
		`INSERT INTO simple_run_logs(run_id,stream,payload)
		 SELECT $1,'stdout',convert_to('line '||n,'UTF8') FROM generate_series(1,$2) AS n`,
		runID, lines); err != nil {
		t.Fatalf("seed %d log lines: %v", lines, err)
	}
}

// A run that wrote more than one page of output must reach the reader as fast
// as the database can hand it over. The stream reads a bounded page at a time,
// and waiting a poll interval between pages turned a deployment that logged
// tens of thousands of lines into a log that trickles onto the screen for
// minutes after the run already finished.
func TestSimpleRunLogStreamDrainsFullPagesWithoutWaiting(t *testing.T) {
	s, targetID, token := newSimpleStreamFixture(t)
	const runID = "cc000000-0000-4000-8000-000000000001"
	const lines = 2600
	seedSimpleRun(t, s, targetID, runID, "stream-batch", "SUCCESS", true)
	seedSimpleRunLogs(t, s, runID, lines)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/simple/runs/"+runID+"/logs/stream?after=0", nil)
	request.AddCookie(&http.Cookie{Name: "releasedock_session", Value: token})
	recorder := httptest.NewRecorder()
	started := time.Now()
	s.Handler().ServeHTTP(recorder, request)
	elapsed := time.Since(started)

	if recorder.Code != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{"line 1", fmt.Sprintf("line %d", lines), "event: end"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the stream did not deliver %q", want)
		}
	}
	if got := strings.Count(body, "event: log"); got != lines {
		t.Fatalf("the stream delivered %d log events, want %d", got, lines)
	}
	// Five of the six pages come back full, so the unfixed stream spends five
	// poll intervals waiting for output it could already read.
	if elapsed > 3*time.Second {
		t.Fatalf("draining %d stored lines took %s, which is a poll interval per page", lines, elapsed)
	}
}

// Reading the next page at once is only correct while a page comes back full.
// A page that did not fill means the run has written nothing more yet, and
// polling that in a tight loop would spin against the database for the whole
// of a long deployment.
func TestSimpleRunLogStreamWaitsWhenAPageIsNotFull(t *testing.T) {
	s, targetID, token := newSimpleStreamFixture(t)
	const runID = "cc000000-0000-4000-8000-000000000002"
	seedSimpleRun(t, s, targetID, runID, "stream-batch", "RUNNING", true)
	seedSimpleRunLogs(t, s, runID, 3)

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/simple/runs/"+runID+"/logs/stream?after=0", nil).WithContext(ctx)
	request.AddCookie(&http.Cookie{Name: "releasedock_session", Value: token})
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if got := strings.Count(body, "event: log"); got != 3 {
		t.Fatalf("the stream delivered %d log events, want 3", got)
	}
	// The run is still going, so the stream must stay open until the client
	// leaves rather than declaring the log finished.
	if strings.Contains(body, "event: end") {
		t.Fatal("a running run must not be reported as ended")
	}
}

// The full-mode release log stream is the same loop over a different table, and
// a release script writes far more output than a simple-mode command, so it has
// to drain a backlog the same way.
func TestReleaseLogStreamDrainsFullPagesWithoutWaiting(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	const lines = 2600
	var stepID int64
	if err := fixture.store.Pool.QueryRow(t.Context(),
		`INSERT INTO release_job_steps(job_id,attempt,step_order,name,status,started_at)
		 VALUES($1,1,1,'deploy','SUCCESS',now()) RETURNING id`, fixture.jobA).Scan(&stepID); err != nil {
		t.Fatalf("seed release job step: %v", err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(),
		`INSERT INTO release_job_logs(job_id,step_id,stream,sequence,payload)
		 SELECT $1,$2,'stdout',n,convert_to('line '||n,'UTF8') FROM generate_series(1,$3) AS n`,
		fixture.jobA, stepID, lines); err != nil {
		t.Fatalf("seed %d release log lines: %v", lines, err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/releases/"+fixture.releaseA+"/logs/stream?after=0", nil)
	request.SetPathValue("id", fixture.releaseA)
	request = request.WithContext(context.WithValue(request.Context(), principalKey, store.Principal{UserID: fixture.creatorID}))
	recorder := httptest.NewRecorder()
	started := time.Now()
	fixture.server.streamReleaseLogs(recorder, request)
	elapsed := time.Since(started)

	body := recorder.Body.String()
	for _, want := range []string{"line 1", fmt.Sprintf("line %d", lines), "event: end"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the stream did not deliver %q", want)
		}
	}
	if got := strings.Count(body, "event: log"); got != lines {
		t.Fatalf("the stream delivered %d log events, want %d", got, lines)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("draining %d stored lines took %s, which is a poll interval per page", lines, elapsed)
	}
}
