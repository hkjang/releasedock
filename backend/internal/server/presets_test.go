package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/hkjang/releasedock/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

func TestQuickArtifactFilenameParser(t *testing.T) {
	t.Parallel()
	valid := map[string]parsedQuickArtifact{
		"ai-portal-v2.4.1.tar.gz":          {ArtifactPrefix: "ai-portal", Version: "2.4.1"},
		"aicc-v0.2.1-rc1.tar.gz":           {ArtifactPrefix: "aicc", Version: "0.2.1-rc1"},
		"text2sql-v10.20.30-rc.1.tar.gz":   {ArtifactPrefix: "text2sql", Version: "10.20.30-rc.1"},
		"service-v1.0.0-hotfix-1.tar.gz":   {ArtifactPrefix: "service", Version: "1.0.0-hotfix-1"},
		"service-v2-v1.0.0-alpha.1.tar.gz": {ArtifactPrefix: "service-v2", Version: "1.0.0-alpha.1"},
		"service-v1.0.0-RC.1.tar.gz":       {ArtifactPrefix: "service", Version: "1.0.0-RC.1"},
	}
	for filename, expected := range valid {
		parsed, err := parseQuickArtifactFilename(filename)
		if err != nil || parsed.ArtifactPrefix != expected.ArtifactPrefix || parsed.Version != expected.Version {
			t.Errorf("parse %q = %+v, %v; want prefix=%q version=%q", filename, parsed, err, expected.ArtifactPrefix, expected.Version)
		}
	}
	invalid := []string{
		"AI-portal-v1.2.3.tar.gz", "ai_portal-v1.2.3.tar.gz", "ai.portal-v1.2.3.tar.gz",
		"ai-portal-v01.2.3.tar.gz", "ai-portal-v1.02.3.tar.gz", "ai-portal-v1.2.03.tar.gz",
		"ai-portal-v1.2.3-01.tar.gz", "ai-portal-v1.2.3-rc.01.tar.gz",
		"ai-portal-v1.2.3+build.tar.gz", "ai-portal-v1.2.tar.gz", "ai-portal-v1.2.3.tar",
		"../ai-portal-v1.2.3.tar.gz", "-v1.2.3.tar.gz", "ai-portal-v1.2.3.TAR.GZ",
	}
	for _, filename := range invalid {
		if parsed, err := parseQuickArtifactFilename(filename); err == nil {
			t.Errorf("invalid filename %q was parsed as %+v", filename, parsed)
		}
	}
}

func TestSemVersionPrecedenceForQuickUpgrade(t *testing.T) {
	t.Parallel()
	ordered := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "2.0.0",
	}
	for index := 1; index < len(ordered); index++ {
		comparison, err := compareSemVersions(ordered[index], ordered[index-1])
		if err != nil || comparison <= 0 {
			t.Fatalf("SemVer precedence %q > %q failed: comparison=%d err=%v", ordered[index], ordered[index-1], comparison, err)
		}
	}
	current := "2.4.1"
	if code, err := validateQuickUpgrade(&current, "2.4.1-hotfix1"); err == nil || code != "quick_upgrade_required" {
		t.Fatalf("stable-to-lower-prerelease was accepted: code=%q err=%v", code, err)
	}
	if code, err := validateQuickUpgrade(&current, "2.4.2-hotfix.1"); err != nil || code != "" {
		t.Fatalf("newer prerelease was rejected: code=%q err=%v", code, err)
	}
	legacy := "release-2026-08"
	if code, err := validateQuickUpgrade(&legacy, "3.0.0"); err == nil || code != "current_version_not_semver" {
		t.Fatalf("non-SemVer current version was accepted: code=%q err=%v", code, err)
	}
}

type quickPreflightSnapshot struct {
	Preset struct {
		ID        string `json:"id"`
		UpdatedAt string `json:"updatedAt"`
	} `json:"preset"`
	CurrentVersion *string `json:"currentVersion"`
}

func seedQuickPreset(t *testing.T, fixture rollbackRetryFixture, prefix string, autoDeploy bool) string {
	t.Helper()
	id, err := secure.NewID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.store.Pool.Exec(t.Context(), `INSERT INTO deployment_presets(id,name,artifact_prefix,application_id,environment_id,profile_id,auto_deploy_after_approval,created_by,updated_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8)`, id, "Preset "+prefix, prefix, fixture.application, fixture.environment, fixture.profile, autoDeploy, fixture.creatorID)
	if err != nil {
		t.Fatalf("seed deployment preset: %v", err)
	}
	return id
}

func configureQuickFixture(t *testing.T, fixture rollbackRetryFixture, storageRoot string, approval bool, maxBytes int64) {
	t.Helper()
	cleanup := []string{
		`DELETE FROM deployment_heads`,
		`UPDATE releases SET rollback_source_job_id=NULL,retry_source_job_id=NULL,rollback_source_release_id=NULL,requested_operation='DEPLOY',operation_requested_by=NULL,operation_base_status=NULL`,
		`UPDATE release_jobs SET rollback_source_job_id=NULL,retry_of_job_id=NULL,rollback_source_release_id=NULL`,
		`DELETE FROM release_jobs`,
		`DELETE FROM releases`,
	}
	for _, statement := range cleanup {
		if _, err := fixture.store.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatalf("reset quick release fixture history: %v", err)
		}
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `UPDATE app_settings SET approval_enabled=$1,artifact_storage_path=$2,artifact_max_bytes=$3`, approval, storageRoot, maxBytes); err != nil {
		t.Fatalf("configure quick release settings: %v", err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `UPDATE deployment_profiles SET approval_required=$2 WHERE id=$1`, fixture.profile, approval); err != nil {
		t.Fatalf("configure quick release profile: %v", err)
	}
}

func seedVerifiedDeployment(t *testing.T, fixture rollbackRetryFixture, version string, priorReleaseID, priorJobID *string) (string, string) {
	return seedDeployment(t, fixture, version, priorReleaseID, priorJobID, true)
}

func seedActiveDeployment(t *testing.T, fixture rollbackRetryFixture, version string, priorReleaseID, priorJobID *string) (string, string) {
	return seedDeployment(t, fixture, version, priorReleaseID, priorJobID, false)
}

func seedDeployment(t *testing.T, fixture rollbackRetryFixture, version string, priorReleaseID, priorJobID *string, complete bool) (string, string) {
	t.Helper()
	releaseID, _ := secure.NewID()
	artifactID, _ := secure.NewID()
	jobID, _ := secure.NewID()
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO releases(id,application_id,environment_id,profile_id,version,created_by) VALUES($1,$2,$3,$4,$5,$6)`, releaseID, fixture.application, fixture.environment, fixture.profile, version, fixture.creatorID); err != nil {
		t.Fatalf("seed verified release %s: %v", version, err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO release_artifacts(id,release_id,original_filename,storage_path,size_bytes,sha256,uploaded_by) VALUES($1,$2,$3,$4,1,repeat('e',64),$5)`, artifactID, releaseID, "seed-v"+version+".tar.gz", releaseID+"/"+artifactID+".tar.gz", fixture.creatorID); err != nil {
		t.Fatalf("seed verified artifact %s: %v", version, err)
	}
	lockKey := fixture.application + ":" + fixture.environment
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,rollback_source_release_id,rollback_source_job_id,created_by) SELECT $1,$2,$3,'retry-app',$4,'prod',$5,$6,artifact.storage_path,artifact.sha256,$7,$8,$9 FROM release_artifacts artifact WHERE artifact.id=$6`, jobID, releaseID, fixture.profile, version, lockKey, artifactID, priorReleaseID, priorJobID, fixture.creatorID); err != nil {
		t.Fatalf("seed verified job %s: %v", version, err)
	}
	if complete {
		if _, err := fixture.store.Pool.Exec(t.Context(), `UPDATE release_jobs SET status='SUCCESS',finished_at=clock_timestamp() WHERE id=$1`, jobID); err != nil {
			t.Fatalf("complete verified job %s: %v", version, err)
		}
	}
	return releaseID, jobID
}

func quickPreflight(t *testing.T, fixture rollbackRetryFixture, filename string) quickPreflightSnapshot {
	t.Helper()
	body, err := json.Marshal(map[string]string{"filename": filename})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/releases/preflight", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), principalKey, store.Principal{UserID: fixture.creatorID}))
	recorder := httptest.NewRecorder()
	fixture.server.preflightQuickRelease(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("preflight status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var snapshot quickPreflightSnapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}
	if snapshot.Preset.ID == "" || snapshot.Preset.UpdatedAt == "" {
		t.Fatalf("preflight omitted optimistic snapshot: %s", recorder.Body.String())
	}
	return snapshot
}

func quickReleaseRequest(fixture rollbackRetryFixture, filename string, artifact []byte, notes string, snapshot quickPreflightSnapshot) *httptest.ResponseRecorder {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("notes", notes)
	_ = writer.WriteField("expectedPresetId", snapshot.Preset.ID)
	_ = writer.WriteField("expectedPresetUpdatedAt", snapshot.Preset.UpdatedAt)
	currentVersion := ""
	if snapshot.CurrentVersion != nil {
		currentVersion = *snapshot.CurrentVersion
	}
	_ = writer.WriteField("expectedCurrentVersion", currentVersion)
	part, _ := writer.CreateFormFile("artifact", filename)
	_, _ = part.Write(artifact)
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/releases/quick", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request = request.WithContext(context.WithValue(request.Context(), principalKey, store.Principal{
		UserID: fixture.creatorID, Permissions: []string{"releases.create", "releases.submit"},
	}))
	recorder := httptest.NewRecorder()
	fixture.server.quickRelease(recorder, request)
	return recorder
}

func responseReleaseID(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var response struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.ID == "" {
		t.Fatalf("decode quick release response: err=%v body=%s", err, recorder.Body.String())
	}
	return response.ID
}

func countStoredFiles(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr == nil && !entry.IsDir() {
			count++
		}
		return walkErr
	})
	if err != nil {
		t.Fatalf("walk artifact storage: %v", err)
	}
	return count
}

func countStoredEntries(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr == nil && path != root {
			count++
		}
		return walkErr
	})
	if err != nil {
		t.Fatalf("walk artifact storage entries: %v", err)
	}
	return count
}

func TestQuickReleaseNoApprovalQueuesExactlyOneJobIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	storageRoot := t.TempDir()
	configureQuickFixture(t, fixture, storageRoot, false, 1<<20)
	seedQuickPreset(t, fixture, "quick-no-approval", true)
	filename := "quick-no-approval-v1.2.3.tar.gz"
	snapshot := quickPreflight(t, fixture, filename)
	recorder := quickReleaseRequest(fixture, filename, []byte("immutable artifact"), "quick deploy", snapshot)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("quick release status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	releaseID := responseReleaseID(t, recorder)
	var status, requester, jobCreator string
	var quick bool
	var jobs int
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT status,created_by,quick_release FROM releases WHERE id=$1`, releaseID).Scan(&status, &requester, &quick); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*),min(created_by) FROM release_jobs WHERE release_id=$1`, releaseID).Scan(&jobs, &jobCreator); err != nil {
		t.Fatal(err)
	}
	if status != "QUEUED" || !quick || jobs != 1 || requester != fixture.creatorID || jobCreator != requester || countStoredFiles(t, storageRoot) != 1 {
		t.Fatalf("quick queue invariant failed: status=%s quick=%v jobs=%d requester=%s jobCreator=%s files=%d", status, quick, jobs, requester, jobCreator, countStoredFiles(t, storageRoot))
	}
	duplicatePreflightBody, _ := json.Marshal(map[string]string{"filename": filename})
	duplicatePreflightRequest := httptest.NewRequest(http.MethodPost, "/api/v1/releases/preflight", bytes.NewReader(duplicatePreflightBody))
	duplicatePreflightRequest.Header.Set("Content-Type", "application/json")
	duplicatePreflight := httptest.NewRecorder()
	fixture.server.preflightQuickRelease(duplicatePreflight, duplicatePreflightRequest)
	if duplicatePreflight.Code != http.StatusConflict || !strings.Contains(duplicatePreflight.Body.String(), "release_version_exists") {
		t.Fatalf("duplicate version preflight status=%d body=%s", duplicatePreflight.Code, duplicatePreflight.Body.String())
	}
	duplicate := quickReleaseRequest(fixture, filename, []byte("duplicate artifact"), "duplicate", snapshot)
	if duplicate.Code != http.StatusConflict || !strings.Contains(duplicate.Body.String(), "release_conflict") {
		t.Fatalf("duplicate quick release status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	if countStoredFiles(t, storageRoot) != 1 {
		t.Fatalf("duplicate quick release left an orphan file: files=%d", countStoredFiles(t, storageRoot))
	}
}

func TestQuickReleaseAutoApprovalIsAtomicAndReplaySafeIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	storageRoot := t.TempDir()
	configureQuickFixture(t, fixture, storageRoot, true, 1<<20)
	seedQuickPreset(t, fixture, "quick-auto", true)
	filename := "quick-auto-v2.0.0-rc.1.tar.gz"
	snapshot := quickPreflight(t, fixture, filename)
	created := quickReleaseRequest(fixture, filename, []byte("pending artifact"), "approval", snapshot)
	if created.Code != http.StatusCreated {
		t.Fatalf("create pending quick release status=%d body=%s", created.Code, created.Body.String())
	}
	releaseID := responseReleaseID(t, created)
	selfApproval := releaseIntegrationRequest(t, fixture, releaseID, fixture.creatorID, "/api/v1/releases/"+releaseID+"/approve", strings.NewReader(`{"comment":"self"}`))
	if selfApproval.Code != http.StatusForbidden || !strings.Contains(selfApproval.Body.String(), "self_approval_forbidden") {
		t.Fatalf("quick release requester self-approved: status=%d body=%s", selfApproval.Code, selfApproval.Body.String())
	}

	start := make(chan struct{})
	responses := make([]*httptest.ResponseRecorder, 2)
	var wait sync.WaitGroup
	for index := range responses {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			request := httptest.NewRequest(http.MethodPost, "/api/v1/releases/"+releaseID+"/approve", strings.NewReader(`{"comment":"approved"}`))
			request.SetPathValue("id", releaseID)
			request = request.WithContext(context.WithValue(request.Context(), principalKey, store.Principal{UserID: fixture.approverID}))
			responses[index] = httptest.NewRecorder()
			fixture.server.approveRelease(responses[index], request)
		}(index)
	}
	close(start)
	wait.Wait()
	successes, conflicts := 0, 0
	for _, response := range responses {
		switch response.Code {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected concurrent approval status=%d body=%s", response.Code, response.Body.String())
		}
	}
	var status, approvedBy, jobCreator string
	var jobs int
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT status,approved_by FROM releases WHERE id=$1`, releaseID).Scan(&status, &approvedBy); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*),min(created_by) FROM release_jobs WHERE release_id=$1`, releaseID).Scan(&jobs, &jobCreator); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || conflicts != 1 || status != "QUEUED" || approvedBy != fixture.approverID || jobs != 1 || jobCreator != fixture.creatorID {
		t.Fatalf("auto approval invariant failed: success=%d conflict=%d status=%s approvedBy=%s jobs=%d jobCreator=%s", successes, conflicts, status, approvedBy, jobs, jobCreator)
	}
}

func TestQuickReleaseRejectsRawPathFilenameIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	storageRoot := t.TempDir()
	configureQuickFixture(t, fixture, storageRoot, false, 1<<20)
	seedQuickPreset(t, fixture, "quick-path", true)
	canonical := "quick-path-v3.1.0.tar.gz"
	snapshot := quickPreflight(t, fixture, canonical)
	recorder := quickReleaseRequest(fixture, "../"+canonical, []byte("must not persist"), "path", snapshot)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_artifact_filename") {
		t.Fatalf("raw path filename status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var releases int
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM releases WHERE version='3.1.0'`).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if releases != 0 || countStoredFiles(t, storageRoot) != 0 {
		t.Fatalf("raw path request left state: releases=%d files=%d", releases, countStoredFiles(t, storageRoot))
	}
}

func TestQuickReleaseArtifactCannotBeReplacedByOperationsAPIIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	storageRoot := t.TempDir()
	configureQuickFixture(t, fixture, storageRoot, true, 1<<20)
	seedQuickPreset(t, fixture, "quick-immutable", false)
	filename := "quick-immutable-v3.2.0.tar.gz"
	created := quickReleaseRequest(fixture, filename, []byte("original"), "immutable", quickPreflight(t, fixture, filename))
	if created.Code != http.StatusCreated {
		t.Fatalf("create immutable quick release status=%d body=%s", created.Code, created.Body.String())
	}
	releaseID := responseReleaseID(t, created)
	if _, err := fixture.store.Pool.Exec(t.Context(), `UPDATE releases SET status='REJECTED' WHERE id=$1`, releaseID); err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(t.TempDir(), "replacement.tar.gz")
	if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.Open(replacementPath)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/releases/"+releaseID+"/artifacts/upload", nil)
	request = request.WithContext(context.WithValue(request.Context(), principalKey, store.Principal{UserID: fixture.creatorID}))
	if _, err := fixture.server.persistArtifact(request, releaseID, "replacement.tar.gz", "application/gzip", replacement, appSettingsResponse{}); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("operations API replaced quick artifact: %v", err)
	}
	var artifacts int
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM release_artifacts WHERE release_id=$1`, releaseID).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if artifacts != 1 || countStoredFiles(t, storageRoot) != 1 {
		t.Fatalf("artifact replacement changed state: artifacts=%d files=%d", artifacts, countStoredFiles(t, storageRoot))
	}
}

func TestQuickReleaseAutoApprovalFailureRollsBackApprovalIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	storageRoot := t.TempDir()
	configureQuickFixture(t, fixture, storageRoot, true, 1<<20)
	seedQuickPreset(t, fixture, "quick-atomic-failure", true)
	filename := "quick-atomic-failure-v3.0.0.tar.gz"
	created := quickReleaseRequest(fixture, filename, []byte("pending artifact"), "approval", quickPreflight(t, fixture, filename))
	if created.Code != http.StatusCreated {
		t.Fatalf("create pending quick release status=%d body=%s", created.Code, created.Body.String())
	}
	releaseID := responseReleaseID(t, created)
	if _, err := fixture.store.Pool.Exec(t.Context(), `UPDATE runner_instances SET active=FALSE WHERE id=$1`, fixture.runner); err != nil {
		t.Fatal(err)
	}
	approval := releaseIntegrationRequest(t, fixture, releaseID, fixture.approverID, "/api/v1/releases/"+releaseID+"/approve", strings.NewReader(`{"comment":"approved"}`))
	if approval.Code != http.StatusConflict || !strings.Contains(approval.Body.String(), "runner_unavailable") {
		t.Fatalf("unavailable-runner approval status=%d body=%s", approval.Code, approval.Body.String())
	}
	var status string
	var approvedBy *string
	var jobs int
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT status,approved_by FROM releases WHERE id=$1`, releaseID).Scan(&status, &approvedBy); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM release_jobs WHERE release_id=$1`, releaseID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if status != "PENDING_REVIEW" || approvedBy != nil || jobs != 0 {
		t.Fatalf("failed auto enqueue partially approved release: status=%s approvedBy=%v jobs=%d", status, approvedBy, jobs)
	}
}

func TestQuickReleaseAutoApprovalRejectsChangedDeploymentHeadIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	configureQuickFixture(t, fixture, t.TempDir(), true, 1<<20)
	baseReleaseID, baseJobID := seedVerifiedDeployment(t, fixture, "1.0.0", nil, nil)
	seedQuickPreset(t, fixture, "quick-frozen-auto", true)
	filename := "quick-frozen-auto-v1.0.1.tar.gz"
	created := quickReleaseRequest(fixture, filename, []byte("pending"), "frozen head", quickPreflight(t, fixture, filename))
	if created.Code != http.StatusCreated {
		t.Fatalf("create frozen-head quick release status=%d body=%s", created.Code, created.Body.String())
	}
	releaseID := responseReleaseID(t, created)
	newHeadReleaseID, newHeadJobID := seedVerifiedDeployment(t, fixture, "1.1.0", &baseReleaseID, &baseJobID)
	approval := releaseIntegrationRequest(t, fixture, releaseID, fixture.approverID, "/api/v1/releases/"+releaseID+"/approve", strings.NewReader(`{"comment":"stale"}`))
	if approval.Code != http.StatusConflict || !strings.Contains(approval.Body.String(), "deployment_head_changed") {
		t.Fatalf("stale-head auto approval status=%d body=%s", approval.Code, approval.Body.String())
	}
	assertQuickHeadConflictState(t, fixture, releaseID, "PENDING_REVIEW", newHeadReleaseID, newHeadJobID)
}

func TestQuickReleaseManualEnqueueRejectsChangedDeploymentHeadIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	configureQuickFixture(t, fixture, t.TempDir(), true, 1<<20)
	baseReleaseID, baseJobID := seedVerifiedDeployment(t, fixture, "2.0.0", nil, nil)
	seedQuickPreset(t, fixture, "quick-frozen-manual", false)
	filename := "quick-frozen-manual-v2.0.1.tar.gz"
	created := quickReleaseRequest(fixture, filename, []byte("pending"), "manual frozen head", quickPreflight(t, fixture, filename))
	if created.Code != http.StatusCreated {
		t.Fatalf("create manual frozen-head quick release status=%d body=%s", created.Code, created.Body.String())
	}
	releaseID := responseReleaseID(t, created)
	approval := releaseIntegrationRequest(t, fixture, releaseID, fixture.approverID, "/api/v1/releases/"+releaseID+"/approve", strings.NewReader(`{"comment":"approved"}`))
	if approval.Code != http.StatusOK {
		t.Fatalf("approve manual quick release status=%d body=%s", approval.Code, approval.Body.String())
	}
	newHeadReleaseID, newHeadJobID := seedVerifiedDeployment(t, fixture, "2.1.0", &baseReleaseID, &baseJobID)
	enqueue := releaseIntegrationRequest(t, fixture, releaseID, fixture.creatorID, "/api/v1/releases/"+releaseID+"/deploy", nil)
	if enqueue.Code != http.StatusConflict || !strings.Contains(enqueue.Body.String(), "deployment_head_changed") {
		t.Fatalf("stale-head manual enqueue status=%d body=%s", enqueue.Code, enqueue.Body.String())
	}
	assertQuickHeadConflictState(t, fixture, releaseID, "APPROVED", newHeadReleaseID, newHeadJobID)
}

func TestQuickReleaseNoApprovalRejectsBusyTargetWithoutDeadlockIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	storageRoot := t.TempDir()
	configureQuickFixture(t, fixture, storageRoot, false, 1<<20)
	baseReleaseID, baseJobID := seedVerifiedDeployment(t, fixture, "4.0.0", nil, nil)
	activeReleaseID, activeJobID := seedActiveDeployment(t, fixture, "4.1.0", &baseReleaseID, &baseJobID)
	seedQuickPreset(t, fixture, "quick-busy", true)
	filename := "quick-busy-v4.2.0.tar.gz"
	result := quickReleaseRequest(fixture, filename, []byte("must not copy"), "busy", quickPreflight(t, fixture, filename))
	if result.Code != http.StatusConflict || !strings.Contains(result.Body.String(), "job_conflict") {
		t.Fatalf("busy target quick release status=%d body=%s", result.Code, result.Body.String())
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := fixture.store.Pool.Exec(ctx, `UPDATE release_jobs SET status='SUCCESS',finished_at=clock_timestamp() WHERE id=$1`, activeJobID); err != nil {
		t.Fatalf("active deployment could not finish after quick rejection: %v", err)
	}
	var headReleaseID string
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT current_release_id::text FROM deployment_heads WHERE application_id=$1 AND environment_id=$2`, fixture.application, fixture.environment).Scan(&headReleaseID); err != nil {
		t.Fatal(err)
	}
	if headReleaseID != activeReleaseID || countStoredEntries(t, storageRoot) != 0 {
		t.Fatalf("busy-target rejection state: head=%s want=%s entries=%d", headReleaseID, activeReleaseID, countStoredEntries(t, storageRoot))
	}
}

func TestQuickReleaseAutoApprovalRejectsBusyTargetWithoutDeadlockIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	configureQuickFixture(t, fixture, t.TempDir(), true, 1<<20)
	baseReleaseID, baseJobID := seedVerifiedDeployment(t, fixture, "5.0.0", nil, nil)
	seedQuickPreset(t, fixture, "quick-busy-approval", true)
	filename := "quick-busy-approval-v5.1.0.tar.gz"
	created := quickReleaseRequest(fixture, filename, []byte("pending"), "busy approval", quickPreflight(t, fixture, filename))
	if created.Code != http.StatusCreated {
		t.Fatalf("create busy approval fixture status=%d body=%s", created.Code, created.Body.String())
	}
	releaseID := responseReleaseID(t, created)
	activeReleaseID, activeJobID := seedActiveDeployment(t, fixture, "5.2.0", &baseReleaseID, &baseJobID)
	approval := releaseIntegrationRequest(t, fixture, releaseID, fixture.approverID, "/api/v1/releases/"+releaseID+"/approve", strings.NewReader(`{"comment":"busy"}`))
	if approval.Code != http.StatusConflict || !strings.Contains(approval.Body.String(), "job_conflict") {
		t.Fatalf("busy target auto approval status=%d body=%s", approval.Code, approval.Body.String())
	}
	var status string
	var approvedBy *string
	var jobs int
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT status,approved_by FROM releases WHERE id=$1`, releaseID).Scan(&status, &approvedBy); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM release_jobs WHERE release_id=$1`, releaseID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if status != "PENDING_REVIEW" || approvedBy != nil || jobs != 0 {
		t.Fatalf("busy approval partially committed: status=%s approvedBy=%v jobs=%d", status, approvedBy, jobs)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := fixture.store.Pool.Exec(ctx, `UPDATE release_jobs SET status='SUCCESS',finished_at=clock_timestamp() WHERE id=$1`, activeJobID); err != nil {
		t.Fatalf("active deployment could not finish after approval rejection: %v", err)
	}
	var headReleaseID string
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT current_release_id::text FROM deployment_heads WHERE application_id=$1 AND environment_id=$2`, fixture.application, fixture.environment).Scan(&headReleaseID); err != nil {
		t.Fatal(err)
	}
	if headReleaseID != activeReleaseID {
		t.Fatalf("active deployment did not become head: got=%s want=%s", headReleaseID, activeReleaseID)
	}
}

func TestQuickQueueSeesJobCreatedAfterEarlierStatementAndDoesNotDeadlockIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	configureQuickFixture(t, fixture, t.TempDir(), true, 1<<20)
	baseReleaseID, baseJobID := seedVerifiedDeployment(t, fixture, "6.0.0", nil, nil)
	seedQuickPreset(t, fixture, "quick-late-job", false)
	filename := "quick-late-job-v6.1.0.tar.gz"
	created := quickReleaseRequest(fixture, filename, []byte("pending"), "late target job", quickPreflight(t, fixture, filename))
	if created.Code != http.StatusCreated {
		t.Fatalf("create late-job quick release status=%d body=%s", created.Code, created.Body.String())
	}
	releaseID := responseReleaseID(t, created)
	approved := releaseIntegrationRequest(t, fixture, releaseID, fixture.approverID, "/api/v1/releases/"+releaseID+"/approve", strings.NewReader(`{"comment":"approved"}`))
	if approved.Code != http.StatusOK {
		t.Fatalf("approve late-job quick release status=%d body=%s", approved.Code, approved.Body.String())
	}

	// Establish an earlier statement on the queue transaction before C exists.
	// READ COMMITTED must still observe/lock C in the later active-job query.
	queueTx, err := fixture.store.Pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer queueTx.Rollback(context.Background()) //nolint:errcheck
	var queueBackendPID int
	var isolation, initialStatus string
	if err := queueTx.QueryRow(t.Context(), `SELECT pg_backend_pid(),current_setting('transaction_isolation'),status FROM releases WHERE id=$1`, releaseID).Scan(&queueBackendPID, &isolation, &initialStatus); err != nil {
		t.Fatal(err)
	}
	if isolation != "read committed" || initialStatus != "APPROVED" {
		t.Fatalf("queue transaction setup isolation=%q status=%q", isolation, initialStatus)
	}

	newHeadReleaseID, newHeadJobID := seedActiveDeployment(t, fixture, "6.2.0", &baseReleaseID, &baseJobID)
	finishTx, err := fixture.store.Pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer finishTx.Rollback(context.Background()) //nolint:errcheck
	if _, err := finishTx.Exec(t.Context(), `UPDATE release_jobs SET status='SUCCESS',finished_at=clock_timestamp() WHERE id=$1`, newHeadJobID); err != nil {
		t.Fatalf("stage concurrent deployment completion: %v", err)
	}

	queueResult := make(chan error, 1)
	go func() {
		_, queueErr := fixture.server.queueQuickDeployTx(context.Background(), queueTx, releaseID, "APPROVED")
		queueResult <- queueErr
	}()
	deadline := time.Now().Add(3 * time.Second)
	blockedOnFinishingJob := false
	for time.Now().Before(deadline) {
		select {
		case queueErr := <-queueResult:
			t.Fatalf("quick queue returned before the concurrent completion committed: %v", queueErr)
		default:
		}
		var waitEventType *string
		if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT wait_event_type FROM pg_stat_activity WHERE pid=$1`, queueBackendPID).Scan(&waitEventType); err != nil {
			t.Fatal(err)
		}
		if waitEventType != nil && *waitEventType == "Lock" {
			blockedOnFinishingJob = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blockedOnFinishingJob {
		t.Fatal("quick queue did not wait on the finishing active-job row")
	}

	commitContext, cancelCommit := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelCommit()
	if err := finishTx.Commit(commitContext); err != nil {
		t.Fatalf("concurrent deployment completion deadlocked: %v", err)
	}
	select {
	case queueErr := <-queueResult:
		var conflict *quickQueueError
		if !errors.As(queueErr, &conflict) || conflict.code != "deployment_head_changed" {
			t.Fatalf("quick queue after concurrent head advance error=%v", queueErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quick queue did not return after concurrent deployment committed")
	}
	if err := queueTx.Rollback(t.Context()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal(err)
	}

	assertQuickHeadConflictState(t, fixture, releaseID, "APPROVED", newHeadReleaseID, newHeadJobID)
}

func TestReleaseJobTargetTriggerRejectsDirectActiveTargetIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	configureQuickFixture(t, fixture, t.TempDir(), false, 1<<20)
	baseReleaseID, baseJobID := seedVerifiedDeployment(t, fixture, "7.0.0", nil, nil)
	_, activeJobID := seedActiveDeployment(t, fixture, "7.1.0", &baseReleaseID, &baseJobID)

	releaseID, _ := secure.NewID()
	artifactID, _ := secure.NewID()
	jobID, _ := secure.NewID()
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO releases(id,application_id,environment_id,profile_id,version,created_by) VALUES($1,$2,$3,$4,'7.2.0',$5)`, releaseID, fixture.application, fixture.environment, fixture.profile, fixture.creatorID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO release_artifacts(id,release_id,original_filename,storage_path,size_bytes,sha256,uploaded_by) VALUES($1,$2,'direct-v7.2.0.tar.gz',$3,1,repeat('d',64),$4)`, artifactID, releaseID, releaseID+"/"+artifactID+".tar.gz", fixture.creatorID); err != nil {
		t.Fatal(err)
	}
	lockKey := fixture.application + ":" + fixture.environment
	_, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,rollback_source_release_id,rollback_source_job_id,created_by)
		SELECT $1,$2,$3,'retry-app','7.2.0','prod',$4,$5,artifact.storage_path,artifact.sha256,$6,$7,$8 FROM release_artifacts artifact WHERE artifact.id=$5`, jobID, releaseID, fixture.profile, lockKey, artifactID, baseReleaseID, baseJobID, fixture.creatorID)
	if err == nil || !strings.Contains(err.Error(), "release target already has an active deployment job") {
		t.Fatalf("direct job insert bypassed target lock trigger: %v", err)
	}
	var jobs int
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM release_jobs WHERE release_id=$1`, releaseID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("rejected direct insert left %d jobs", jobs)
	}
	finishContext, cancelFinish := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelFinish()
	if _, err := fixture.store.Pool.Exec(finishContext, `UPDATE release_jobs SET status='SUCCESS',finished_at=clock_timestamp() WHERE id=$1`, activeJobID); err != nil {
		t.Fatalf("active job could not finish after direct insert rejection: %v", err)
	}
}

func assertQuickHeadConflictState(t *testing.T, fixture rollbackRetryFixture, releaseID, wantedStatus, wantedHeadReleaseID, wantedHeadJobID string) {
	t.Helper()
	var status, headReleaseID, headJobID string
	var approvedBy *string
	var jobs int
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT status,approved_by FROM releases WHERE id=$1`, releaseID).Scan(&status, &approvedBy); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM release_jobs WHERE release_id=$1`, releaseID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT current_release_id::text,current_job_id::text FROM deployment_heads WHERE application_id=$1 AND environment_id=$2`, fixture.application, fixture.environment).Scan(&headReleaseID, &headJobID); err != nil {
		t.Fatal(err)
	}
	if status != wantedStatus || jobs != 0 || headReleaseID != wantedHeadReleaseID || headJobID != wantedHeadJobID {
		t.Fatalf("stale head changed state: status=%s approvedBy=%v jobs=%d head=(%s,%s), want status=%s head=(%s,%s)", status, approvedBy, jobs, headReleaseID, headJobID, wantedStatus, wantedHeadReleaseID, wantedHeadJobID)
	}
	if wantedStatus == "PENDING_REVIEW" && approvedBy != nil {
		t.Fatalf("failed auto approval retained approver: %v", approvedBy)
	}
}

func TestQuickReleaseRejectsStalePresetAndCleansArtifactsIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	storageRoot := t.TempDir()
	configureQuickFixture(t, fixture, storageRoot, false, 1<<20)
	presetID := seedQuickPreset(t, fixture, "quick-stale", true)
	filename := "quick-stale-v4.0.0.tar.gz"
	snapshot := quickPreflight(t, fixture, filename)
	if _, err := fixture.store.Pool.Exec(t.Context(), `UPDATE deployment_presets SET name=name||' changed',updated_at=clock_timestamp() WHERE id=$1`, presetID); err != nil {
		t.Fatal(err)
	}
	recorder := quickReleaseRequest(fixture, filename, []byte("must not persist"), "stale", snapshot)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "deployment_preset_unavailable") {
		t.Fatalf("stale preset status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var releases, artifacts, jobs int
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM releases WHERE version='4.0.0'`).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM release_artifacts artifact JOIN releases release ON release.id=artifact.release_id WHERE release.version='4.0.0'`).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM release_jobs job JOIN releases release ON release.id=job.release_id WHERE release.version='4.0.0'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if releases != 0 || artifacts != 0 || jobs != 0 || countStoredFiles(t, storageRoot) != 0 {
		t.Fatalf("stale preflight left state: releases=%d artifacts=%d jobs=%d files=%d", releases, artifacts, jobs, countStoredFiles(t, storageRoot))
	}
}

func TestQuickReleaseOversizeIs413AndLeavesNoStateIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	storageRoot := t.TempDir()
	configureQuickFixture(t, fixture, storageRoot, false, 32)
	seedQuickPreset(t, fixture, "quick-oversize", true)
	filename := "quick-oversize-v5.0.0.tar.gz"
	recorder := quickReleaseRequest(fixture, filename, bytes.Repeat([]byte("x"), 64), "oversize", quickPreflight(t, fixture, filename))
	if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), "artifact_too_large") {
		t.Fatalf("oversize status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var releases int
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM releases WHERE version='5.0.0'`).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if releases != 0 || countStoredFiles(t, storageRoot) != 0 || countStoredEntries(t, storageRoot) != 0 {
		t.Fatalf("oversize request left state: releases=%d files=%d entries=%d", releases, countStoredFiles(t, storageRoot), countStoredEntries(t, storageRoot))
	}
}

func TestQuickReleaseDowngradeRejectedInPreflightAndLockedUploadIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	storageRoot := t.TempDir()
	configureQuickFixture(t, fixture, storageRoot, false, 1<<20)
	seedVerifiedDeployment(t, fixture, "3.0.0", nil, nil)
	presetID := seedQuickPreset(t, fixture, "quick-upgrade-only", true)
	filename := "quick-upgrade-only-v2.9.0.tar.gz"
	body, _ := json.Marshal(map[string]string{"filename": filename})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/releases/preflight", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	fixture.server.preflightQuickRelease(recorder, request)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "quick_upgrade_required") {
		t.Fatalf("downgrade preflight status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var updatedAt time.Time
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT updated_at FROM deployment_presets WHERE id=$1`, presetID).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	current := "3.0.0"
	snapshot := quickPreflightSnapshot{CurrentVersion: &current}
	snapshot.Preset.ID = presetID
	snapshot.Preset.UpdatedAt = updatedAt.Format(time.RFC3339Nano)
	upload := quickReleaseRequest(fixture, filename, []byte("downgrade"), "must reject", snapshot)
	if upload.Code != http.StatusConflict || !strings.Contains(upload.Body.String(), "quick_upgrade_required") {
		t.Fatalf("locked downgrade upload status=%d body=%s", upload.Code, upload.Body.String())
	}
	var releases int
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM releases WHERE version='2.9.0'`).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if releases != 0 || countStoredEntries(t, storageRoot) != 0 {
		t.Fatalf("downgrade left state: releases=%d entries=%d", releases, countStoredEntries(t, storageRoot))
	}
}

func TestQuickReleaseDatabaseRejectsMismatchedPresetSnapshotIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	presetID := seedQuickPreset(t, fixture, "quick-db-guard", true)
	otherEnvironment, _ := secure.NewID()
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO environments(id,application_id,code,name,created_by) VALUES($1,$2,'other','Other',$3)`, otherEnvironment, fixture.application, fixture.creatorID); err != nil {
		t.Fatal(err)
	}
	releaseID, _ := secure.NewID()
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO releases(id,application_id,environment_id,profile_id,version,created_by,deployment_preset_id,quick_release) VALUES($1,$2,$3,$4,'6.0.0',$5,$6,TRUE)`, releaseID, fixture.application, otherEnvironment, fixture.profile, fixture.creatorID, presetID); err == nil {
		t.Fatal("database accepted a quick release whose target did not match its preset")
	}
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO releases(id,application_id,environment_id,profile_id,version,created_by,quick_release) VALUES($1,$2,$3,$4,'6.0.1',$5,TRUE)`, releaseID, fixture.application, fixture.environment, fixture.profile, fixture.creatorID); err == nil {
		t.Fatal("database accepted a quick release without a deployment preset")
	}
	releaseID, _ = secure.NewID()
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO releases(id,application_id,environment_id,profile_id,version,created_by,deployment_preset_id,quick_release) VALUES($1,$2,$3,$4,'6.0.2',$5,$6,TRUE)`, releaseID, fixture.application, fixture.environment, fixture.profile, fixture.creatorID, presetID); err == nil {
		t.Fatal("database accepted a quick release that omitted the current deployment head")
	}
	releaseID, _ = secure.NewID()
	if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO releases(id,application_id,environment_id,profile_id,version,created_by,auto_deploy_after_approval) VALUES($1,$2,$3,$4,'6.0.3',$5,TRUE)`, releaseID, fixture.application, fixture.environment, fixture.profile, fixture.creatorID); err == nil {
		t.Fatal("database accepted automatic deployment on a non-quick release")
	}
}

func TestDeploymentPresetAdminCRUDRBACAndAuditIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	const roleID = "role-preset-manager"
	grants := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO roles(id,name,description) VALUES($1,'Preset manager','Preset administration test')`, []any{roleID}},
		{`INSERT INTO role_permissions(role_id,permission_code) VALUES($1,'admin.presets.read'),($1,'admin.presets.write')`, []any{roleID}},
		{`INSERT INTO user_roles(user_id,role_id) VALUES($1,$2)`, []any{fixture.creatorID, roleID}},
	}
	for _, grant := range grants {
		if _, err := fixture.store.Pool.Exec(t.Context(), grant.query, grant.args...); err != nil {
			t.Fatalf("grant preset manager role: %v", err)
		}
	}
	newKey := func(id string, scopes []string) string {
		t.Helper()
		secret, err := secure.RandomToken(24)
		if err != nil {
			t.Fatal(err)
		}
		token := "rdk_" + secret
		if _, err := fixture.store.Pool.Exec(t.Context(), `INSERT INTO api_keys(id,user_id,name,prefix,secret_hash,scopes) VALUES($1,$2,$3,$4,$5,$6)`, id, fixture.creatorID, id, token[:16], secure.TokenHash(token), scopes); err != nil {
			t.Fatalf("create preset API key: %v", err)
		}
		return token
	}
	readToken := newKey("preset-read-key", []string{"admin.presets.read"})
	writeToken := newKey("preset-write-key", []string{"admin.presets.read", "admin.presets.write"})
	request := func(method, path, token string, payload map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		if payload != nil {
			if err := json.NewEncoder(&body).Encode(payload); err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(method, path, &body)
		req.Header.Set("Authorization", "Bearer "+token)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		recorder := httptest.NewRecorder()
		fixture.server.Handler().ServeHTTP(recorder, req)
		return recorder
	}
	payload := map[string]any{
		"name": "AI Portal production", "artifactPrefix": "ai-portal",
		"applicationId": fixture.application, "environmentId": fixture.environment,
		"deploymentProfileId": fixture.profile, "active": true, "autoDeployAfterApproval": true,
	}
	forbidden := request(http.MethodPost, "/api/v1/admin/deployment-presets", readToken, payload)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("read-only key created preset: status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}
	created := request(http.MethodPost, "/api/v1/admin/deployment-presets", writeToken, payload)
	if created.Code != http.StatusCreated {
		t.Fatalf("create preset status=%d body=%s", created.Code, created.Body.String())
	}
	var createdBody struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil || createdBody.ID == "" {
		t.Fatalf("decode created preset: %v body=%s", err, created.Body.String())
	}
	read := request(http.MethodGet, "/api/v1/admin/deployment-presets/"+createdBody.ID, readToken, nil)
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"artifactPrefix":"ai-portal"`) {
		t.Fatalf("read preset status=%d body=%s", read.Code, read.Body.String())
	}
	payload["name"] = "AI Portal production updated"
	payload["autoDeployAfterApproval"] = false
	updated := request(http.MethodPut, "/api/v1/admin/deployment-presets/"+createdBody.ID, writeToken, payload)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"autoDeployAfterApproval":false`) {
		t.Fatalf("update preset status=%d body=%s", updated.Code, updated.Body.String())
	}
	revoked := request(http.MethodDelete, "/api/v1/admin/deployment-presets/"+createdBody.ID, writeToken, nil)
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("revoke preset status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	payload["name"] = "AI Portal replacement"
	recreated := request(http.MethodPost, "/api/v1/admin/deployment-presets", writeToken, payload)
	if recreated.Code != http.StatusCreated {
		t.Fatalf("reusing revoked prefix status=%d body=%s", recreated.Code, recreated.Body.String())
	}
	var auditEvents int
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_logs WHERE actor_id=$1 AND action IN ('deployment_preset.create','deployment_preset.update','deployment_preset.revoke')`, fixture.creatorID).Scan(&auditEvents); err != nil {
		t.Fatal(err)
	}
	if auditEvents != 4 {
		t.Fatalf("preset CRUD audit events=%d, want 4", auditEvents)
	}
}
