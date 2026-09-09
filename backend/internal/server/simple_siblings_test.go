package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hkjang/releasedock/backend/internal/store"
)

// seedSimpleRunFor is seedSimpleRun for a run somebody else uploaded, which is
// what makes the actor restriction observable.
func seedSimpleRunFor(t *testing.T, s *Server, targetID, actorID, runID, batchID, status string, last bool) {
	t.Helper()
	if _, err := s.store.Pool.Exec(t.Context(),
		`INSERT INTO users(id,username,display_name) VALUES($1,$1,$1) ON CONFLICT (id) DO NOTHING`, actorID); err != nil {
		t.Fatalf("seed user %s: %v", actorID, err)
	}
	if _, err := s.store.Pool.Exec(t.Context(),
		`INSERT INTO simple_runs(id,target_id,actor_id,original_filename,stored_path,size_bytes,sha256,
			command_source,resolved_command_path,resolved_timeout_seconds,batch_id,batch_last,status)
		 VALUES($1,$2,$3,$4,'/var/lib/releasedock/simple/batch/'||$4,1,repeat('a',64),
			'PER_TARGET','/opt/deploy/run.sh',600,$5,$6,$7)`,
		runID, targetID, actorID, runID+".tar.gz", batchID, last, status); err != nil {
		t.Fatalf("seed run %s: %v", runID, err)
	}
}

// The run screen lists the other packages of the upload so a reader told that
// the once-per-upload stages were held can see which package is the reason.
func TestListBatchSiblingsReturnsTheRestOfTheUpload(t *testing.T) {
	s, targetID := newSimpleBatchFixture(t)
	const batch = "batch-one"
	const viewed = "bb000000-0000-4000-8000-000000000003"
	const first = "bb000000-0000-4000-8000-000000000001"
	const failed = "bb000000-0000-4000-8000-000000000002"
	seedSimpleRun(t, s, targetID, first, batch, "SUCCESS", false)
	seedSimpleRun(t, s, targetID, failed, batch, "FAILED", false)
	seedSimpleRun(t, s, targetID, viewed, batch, "SUCCESS", true)
	// Another upload of the same user is a different upload.
	seedSimpleRun(t, s, targetID, "bb000000-0000-4000-8000-000000000004", "batch-two", "FAILED", true)

	items, err := s.listBatchSiblings(t.Context(), batch, viewed, "batch-actor")
	if err != nil {
		t.Fatalf("list siblings: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected the two other packages of the upload, got %d: %v", len(items), items)
	}
	// Oldest first, and never the run whose screen this is.
	if items[0]["id"] != first || items[1]["id"] != failed {
		t.Fatalf("expected the packages in deploy order, got %v and %v", items[0]["id"], items[1]["id"])
	}
	if items[1]["status"] != "FAILED" {
		t.Fatalf("expected the package that did not deploy to say so, got %v", items[1]["status"])
	}
	if items[1]["filename"] != failed+".tar.gz" {
		t.Fatalf("expected the package name, got %v", items[1]["filename"])
	}
	if items[0]["batchLast"] != false {
		t.Fatalf("expected the marker of the last package to be carried, got %v", items[0]["batchLast"])
	}
}

// The batch identifier comes from the client, so a run of somebody else's that
// carries the same one must not be listed on this user's screen.
func TestListBatchSiblingsIsRestrictedToTheRunsActor(t *testing.T) {
	s, targetID := newSimpleBatchFixture(t)
	const batch = "shared-identifier"
	const viewed = "bb000000-0000-4000-8000-000000000001"
	seedSimpleRun(t, s, targetID, viewed, batch, "SUCCESS", true)
	seedSimpleRunFor(t, s, targetID, "someone-else", "bb000000-0000-4000-8000-000000000002", batch, "FAILED", false)

	items, err := s.listBatchSiblings(t.Context(), batch, viewed, "batch-actor")
	if err != nil {
		t.Fatalf("list siblings: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected another user's run to stay out of the list, got %v", items)
	}
}

// A run that was uploaded on its own belongs to no upload, so there is nothing
// to relate and no query to run.
func TestListBatchSiblingsSkipsARunWithNoBatch(t *testing.T) {
	s, targetID := newSimpleBatchFixture(t)
	seedSimpleRun(t, s, targetID, "bb000000-0000-4000-8000-000000000001", "", "SUCCESS", true)

	items, err := s.listBatchSiblings(t.Context(), "", "bb000000-0000-4000-8000-000000000001", "batch-actor")
	if err != nil {
		t.Fatalf("list siblings: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no packages for a run with no upload, got %v", items)
	}
}

// The endpoint itself, so the columns the screen reads are the columns the
// query selects: a run detail carries its upload with it.
func TestGetSimpleRunReportsTheUpload(t *testing.T) {
	s, targetID := newSimpleBatchFixture(t)
	const batch = "batch-one"
	const viewed = "bb000000-0000-4000-8000-000000000002"
	seedSimpleRun(t, s, targetID, "bb000000-0000-4000-8000-000000000001", batch, "FAILED", false)
	seedSimpleRun(t, s, targetID, viewed, batch, "SUCCESS", true)

	request := httptest.NewRequest(http.MethodGet, "/simple/runs/"+viewed, nil)
	request.SetPathValue("id", viewed)
	request = request.WithContext(context.WithValue(t.Context(), principalKey, store.Principal{UserID: "batch-actor"}))
	recorder := httptest.NewRecorder()

	s.getSimpleRun(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		BatchID       string `json:"batchId"`
		BatchLast     bool   `json:"batchLast"`
		SiblingsError string `json:"batchSiblingsError"`
		Siblings      []struct {
			ID        string `json:"id"`
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			BatchLast bool   `json:"batchLast"`
		} `json:"batchSiblings"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode the response: %v", err)
	}
	if response.BatchID != batch || !response.BatchLast {
		t.Fatalf("the run must carry its place in the upload, got %q/%v", response.BatchID, response.BatchLast)
	}
	if response.SiblingsError != "" {
		t.Fatalf("unexpected sibling error: %s", response.SiblingsError)
	}
	if len(response.Siblings) != 1 || response.Siblings[0].Status != "FAILED" {
		t.Fatalf("expected the package that did not deploy, got %+v", response.Siblings)
	}
	if response.Siblings[0].ID != "bb000000-0000-4000-8000-000000000001" {
		t.Fatalf("unexpected sibling: %+v", response.Siblings[0])
	}
}
