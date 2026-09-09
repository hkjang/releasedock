package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hkjang/releasedock/backend/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
)

// The upload path turns one database error into "a run is already in flight",
// so which errors carry that meaning has to be exact.
func TestIsUniqueViolationOnlyMatchesADuplicateRow(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"duplicate row", &pgconn.PgError{Code: "23505"}, true},
		{"wrapped duplicate row", fmt.Errorf("insert run: %w", &pgconn.PgError{Code: "23505"}), true},
		{"foreign key violation", &pgconn.PgError{Code: "23503"}, false},
		{"check constraint", &pgconn.PgError{Code: "23514"}, false},
		{"serialization failure", &pgconn.PgError{Code: "40001"}, false},
		{"cancelled request", context.Canceled, false},
		{"connection lost", errors.New("conn closed"), false},
		{"no error", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUniqueViolation(tc.err); got != tc.want {
				t.Fatalf("isUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// pointSimpleTargetAtTempDirs gives the seeded target a real upload directory
// and a command that exists, and answers with the upload directory so a test
// can check what the rejected upload left behind.
func pointSimpleTargetAtTempDirs(t *testing.T, s *Server, targetID string) string {
	t.Helper()
	uploadDir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		t.Fatalf("create the upload directory: %v", err)
	}
	command := filepath.Join(t.TempDir(), "deploy.sh")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write the deployment command: %v", err)
	}
	if _, err := s.store.Pool.Exec(t.Context(),
		`UPDATE simple_targets SET upload_dir=$2,command_path=$3 WHERE id=$1`, targetID, uploadDir, command); err != nil {
		t.Fatalf("point the target at the test directories: %v", err)
	}
	return uploadDir
}

// postSimpleUpload sends one package as the given user and answers with the
// recorded response.
func postSimpleUpload(t *testing.T, s *Server, targetID, actorID string) *httptest.ResponseRecorder {
	t.Helper()
	body, boundary := uploadBody(t, "app-v1.tar.gz", bytes.Repeat([]byte("package"), 100), nil, true)
	request := httptest.NewRequest(http.MethodPost, "/simple/targets/"+targetID+"/runs", body)
	request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	request.SetPathValue("id", targetID)
	request = request.WithContext(context.WithValue(t.Context(), principalKey, store.Principal{UserID: actorID}))
	recorder := httptest.NewRecorder()
	s.createSimpleRun(recorder, request)
	return recorder
}

// errorResponse reads the code and message a rejected upload answered with.
func errorResponse(t *testing.T, recorder *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var response struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode the response: %v", err)
	}
	return response.Error.Code, response.Error.Message
}

// assertUploadDirEmpty checks that a rejected upload published nothing and left
// no staged file behind either.
func assertUploadDirEmpty(t *testing.T, uploadDir string) {
	t.Helper()
	entries, err := os.ReadDir(uploadDir)
	if err != nil {
		t.Fatalf("read the upload directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the rejected upload left %d file(s) in the target directory", len(entries))
	}
}

// A second upload while the target is busy is the one rejection the uploader
// can do something about, so it keeps the conflict.
func TestCreateSimpleRunReportsARunAlreadyInFlightAsAConflict(t *testing.T) {
	s, targetID := newSimpleBatchFixture(t)
	uploadDir := pointSimpleTargetAtTempDirs(t, s, targetID)
	seedSimpleRun(t, s, targetID, "cc000000-0000-4000-8000-000000000001", "", "RUNNING", false)

	recorder := postSimpleUpload(t, s, targetID, "batch-actor")

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if code, _ := errorResponse(t, recorder); code != "simple_run_active" {
		t.Fatalf("error code = %q, want simple_run_active", code)
	}
	assertUploadDirEmpty(t, uploadDir)
	if s.simpleActive != 0 {
		t.Fatalf("the rejected upload held %d concurrency slot(s)", s.simpleActive)
	}
}

// Every other failure of the insert used to be answered with the same conflict,
// which told the operator to wait for a run that does not exist. Here the actor
// has no row in users, so the insert fails the foreign key rather than the
// one-run-per-target index.
func TestCreateSimpleRunDoesNotBlameARunInFlightForAnotherFailure(t *testing.T) {
	s, targetID := newSimpleBatchFixture(t)
	uploadDir := pointSimpleTargetAtTempDirs(t, s, targetID)

	recorder := postSimpleUpload(t, s, targetID, "ghost-actor")

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	code, message := errorResponse(t, recorder)
	if code == "simple_run_active" {
		t.Fatalf("a failure that is not a duplicate was reported as an active run: %s", recorder.Body.String())
	}
	if code != "database_error" || message != "실행 기록을 저장하지 못했습니다" {
		t.Fatalf("error = %q/%q", code, message)
	}
	assertUploadDirEmpty(t, uploadDir)
	if s.simpleActive != 0 {
		t.Fatalf("the failed upload held %d concurrency slot(s)", s.simpleActive)
	}
	var runs int
	if err := s.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM simple_runs`).Scan(&runs); err != nil {
		t.Fatalf("count the runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("the failed upload recorded %d run(s)", runs)
	}
}
