package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hkjang/releasedock/backend/internal/store"
)

// Both log streams resume from the larger of the Last-Event-ID header and the
// ?after query, and each of the two values is the only one a real client sends
// in one of the two reconnect paths. The browser opens the stream with ?after
// and, when it reconnects on its own, replays that same now-stale URL with a
// fresh Last-Event-ID header; the manual "reconnect" control instead refetches
// the stored log and opens a brand new EventSource, which carries ?after and no
// header at all. Reading only one of the two would either repeat lines already
// on screen or skip lines that arrived while the connection was down, so the
// tests below pin the priority from both directions.

// streamedLogIDs returns the ids of the log frames in an SSE body in the order
// they were written. Comparing ids rather than payload text keeps the check off
// the prefix trap next door: strings.Contains(body, "line 1") also matches
// "line 10".
func streamedLogIDs(t *testing.T, body string) []int64 {
	t.Helper()
	var ids []int64
	for _, frame := range strings.Split(body, "\n\n") {
		if !strings.Contains(frame, "event: log") {
			continue
		}
		var id int64
		found := false
		for _, line := range strings.Split(frame, "\n") {
			raw, ok := strings.CutPrefix(line, "id: ")
			if !ok {
				continue
			}
			parsed, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
			if err != nil {
				t.Fatalf("log frame carried an unparseable id %q: %v", raw, err)
			}
			id, found = parsed, true
		}
		if !found {
			t.Fatalf("a log frame arrived without an id: %q", frame)
		}
		ids = append(ids, id)
	}
	return ids
}

// storedSimpleRunLogIDs reads the ids the database actually assigned. The
// column is a sequence shared by the schema, so the seeded rows are not
// guaranteed to start at 1 and a cursor test that assumed so would be pinning
// the fixture rather than the stream.
func storedSimpleRunLogIDs(t *testing.T, s *Server, runID string) []int64 {
	t.Helper()
	rows, err := s.store.Pool.Query(t.Context(),
		`SELECT id FROM simple_run_logs WHERE run_id=$1 ORDER BY id`, runID)
	if err != nil {
		t.Fatalf("read seeded log ids: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan seeded log id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read seeded log ids: %v", err)
	}
	return ids
}

// simpleCursorStream runs one finished run's log stream through the real route
// and returns the ids it delivered. The run is terminal, so the handler sends
// everything past the cursor and returns on its own instead of holding the test
// open for a poll interval.
func simpleCursorStream(t *testing.T, s *Server, token, runID, query, header string) []int64 {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/simple/runs/"+runID+"/logs/stream"+query, nil)
	request.AddCookie(&http.Cookie{Name: "releasedock_session", Value: token})
	if header != "" {
		request.Header.Set("Last-Event-ID", header)
	}
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: end") {
		t.Fatalf("a finished run must end its stream, body=%s", body)
	}
	return streamedLogIDs(t, body)
}

func assertLogIDs(t *testing.T, got, want []int64, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s delivered %d log frames %v, want %d %v", what, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s delivered ids %v, want %v", what, got, want)
		}
	}
}

// The first connection a client makes carries the cursor in the URL, and the
// manual reconnect control keeps doing so for the life of that EventSource.
func TestSimpleRunLogStreamResumesFromTheAfterQuery(t *testing.T) {
	s, targetID, token := newSimpleStreamFixture(t)
	const runID = "cc000000-0000-4000-8000-000000000011"
	seedSimpleRun(t, s, targetID, runID, "cursor-batch", "SUCCESS", true)
	seedSimpleRunLogs(t, s, runID, 10)
	ids := storedSimpleRunLogIDs(t, s, runID)

	got := simpleCursorStream(t, s, token, runID, "?after="+strconv.FormatInt(ids[4], 10), "")

	assertLogIDs(t, got, ids[5:], "a stream opened with ?after alone")
}

// A browser that reconnects on its own sends no query it did not already have,
// but it does send the id of the last frame it saw as a header. Reading only
// the query would replay the whole log on every dropped connection.
func TestSimpleRunLogStreamResumesFromTheLastEventIDHeader(t *testing.T) {
	s, targetID, token := newSimpleStreamFixture(t)
	const runID = "cc000000-0000-4000-8000-000000000012"
	seedSimpleRun(t, s, targetID, runID, "cursor-batch", "SUCCESS", true)
	seedSimpleRunLogs(t, s, runID, 10)
	ids := storedSimpleRunLogIDs(t, s, runID)

	got := simpleCursorStream(t, s, token, runID, "", strconv.FormatInt(ids[4], 10))

	assertLogIDs(t, got, ids[5:], "a stream opened with Last-Event-ID alone")
}

// The real browser reconnect: the URL still carries the after=0 the page opened
// with minutes ago, while the header carries the id of the last line on screen.
// The header has to win or every one of those lines arrives a second time.
func TestSimpleRunLogStreamPrefersTheHeaderWhenItIsAheadOfTheQuery(t *testing.T) {
	s, targetID, token := newSimpleStreamFixture(t)
	const runID = "cc000000-0000-4000-8000-000000000013"
	seedSimpleRun(t, s, targetID, runID, "cursor-batch", "SUCCESS", true)
	seedSimpleRunLogs(t, s, runID, 10)
	ids := storedSimpleRunLogIDs(t, s, runID)

	got := simpleCursorStream(t, s, token, runID, "?after=0", strconv.FormatInt(ids[6], 10))

	assertLogIDs(t, got, ids[7:], "a browser reconnect carrying a stale after=0")
}

// The other direction: the manual reconnect refetches the stored log, so its
// new URL is ahead of whatever header a proxy or a replayed connection attaches.
// The query has to win there or the lines it just collected arrive twice.
func TestSimpleRunLogStreamPrefersTheQueryWhenItIsAheadOfTheHeader(t *testing.T) {
	s, targetID, token := newSimpleStreamFixture(t)
	const runID = "cc000000-0000-4000-8000-000000000014"
	seedSimpleRun(t, s, targetID, runID, "cursor-batch", "SUCCESS", true)
	seedSimpleRunLogs(t, s, runID, 10)
	ids := storedSimpleRunLogIDs(t, s, runID)

	got := simpleCursorStream(t, s, token, runID, "?after="+strconv.FormatInt(ids[6], 10), strconv.FormatInt(ids[1], 10))

	assertLogIDs(t, got, ids[7:], "a manual reconnect carrying a stale header")
}

// The full-mode stream parses the same two inputs in its own copy of the code,
// and the release detail page reconnects the same way, so the header has to
// mean the same thing on both sides.
func TestReleaseLogStreamResumesFromTheLastEventIDHeader(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	var stepID int64
	if err := fixture.store.Pool.QueryRow(t.Context(),
		`INSERT INTO release_job_steps(job_id,attempt,step_order,name,status,started_at)
		 VALUES($1,1,1,'deploy','SUCCESS',now()) RETURNING id`, fixture.jobA).Scan(&stepID); err != nil {
		t.Fatalf("seed release job step: %v", err)
	}
	var ids []int64
	rows, err := fixture.store.Pool.Query(t.Context(),
		`INSERT INTO release_job_logs(job_id,step_id,stream,sequence,payload)
		 SELECT $1,$2,'stdout',n,convert_to('line '||n,'UTF8') FROM generate_series(1,10) AS n
		 RETURNING id`, fixture.jobA, stepID)
	if err != nil {
		t.Fatalf("seed release log lines: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan seeded release log id: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("seed release log lines: %v", err)
	}
	if len(ids) != 10 {
		t.Fatalf("seeded %d release log lines, want 10", len(ids))
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/releases/"+fixture.releaseA+"/logs/stream", nil)
	request.SetPathValue("id", fixture.releaseA)
	request.Header.Set("Last-Event-ID", strconv.FormatInt(ids[4], 10))
	request = request.WithContext(context.WithValue(request.Context(), principalKey, store.Principal{UserID: fixture.creatorID}))
	recorder := httptest.NewRecorder()
	fixture.server.streamReleaseLogs(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "event: end") {
		t.Fatalf("a finished job must end its stream, body=%s", body)
	}
	assertLogIDs(t, streamedLogIDs(t, body), ids[5:], "a release stream opened with Last-Event-ID alone")
}
