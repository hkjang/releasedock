package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type releaseInput struct {
	ApplicationID string `json:"applicationId"`
	EnvironmentID string `json:"environmentId"`
	ProfileID     string `json:"deploymentProfileId"`
	Version       string `json:"version"`
	Notes         string `json:"notes"`
}

func validateReleaseInput(input releaseInput) error {
	if input.ApplicationID == "" || input.EnvironmentID == "" || input.ProfileID == "" {
		return errors.New("applicationId, environmentId, and deploymentProfileId are required")
	}
	input.Version = strings.TrimSpace(input.Version)
	if input.Version == "" || len(input.Version) > 128 || strings.ContainsAny(input.Version, "\x00\r\n/\\") {
		return errors.New("version is required, must not exceed 128 characters, and cannot contain path separators")
	}
	if len(input.Notes) > 16<<10 {
		return errors.New("notes must not exceed 16 KiB")
	}
	return nil
}

func (s *Server) listReleases(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	var total int
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM releases r JOIN applications a ON a.id=r.application_id WHERE ($1='' OR r.status=$1) AND ($2='' OR a.name ILIKE '%'||$2||'%' OR a.code ILIKE '%'||$2||'%' OR r.version ILIKE '%'||$2||'%')`, status, search).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not list releases")
		return
	}
	items, err := s.releaseRows(r, limit, offset, status, search)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not list releases")
		return
	}
	writeJSON(w, http.StatusOK, page(items, total, limit, offset))
}

const releaseSelect = `
	SELECT r.id::text,r.application_id::text,a.name,r.environment_id::text,e.name,
	       r.profile_id::text,p.name,r.version,r.notes,r.status,r.requested_operation,r.rollback_source_release_id::text,r.rollback_source_job_id::text,r.retry_source_job_id::text,
	       r.deployment_preset_id::text,r.quick_release,r.auto_deploy_after_approval,r.quick_base_release_id::text,r.quick_base_job_id::text,rollback_source.version,r.created_by,u.username,
	       r.decision_note,r.created_at,r.updated_at,
	       art.id::text,art.original_filename,art.size_bytes,art.sha256,
	       (r.status IN ('PENDING_REVIEW','UNDER_REVIEW') OR (settings.approval_enabled AND (p.approval_required OR e.protected OR upper(e.code)=ANY(regexp_split_to_array(upper(COALESCE(settings.approval_config->>'protectedEnvironments','')), '[[:space:]]*,[[:space:]]*'))))),
	       j.started_at,j.finished_at,
	       COALESCE(CASE
	           WHEN head.current_release_id=r.id THEN head_basis.rollback_source_release_id IS NOT NULL AND (
	               latest_target.id=head.current_job_id
	               OR (latest_target.operation='ROLLBACK' AND latest_target.status='ROLLED_BACK' AND latest_target.rollback_source_release_id=head.current_release_id AND latest_target.rollback_source_job_id=head.current_job_id)
	           )
	           ELSE head.current_release_id IS NOT NULL AND latest_target.release_id=r.id AND latest_target.operation='DEPLOY' AND latest_target.status='FAILED'
	       END,FALSE),
	       COALESCE(r.status='FAILED' AND latest_target.release_id=r.id AND latest_target.status='FAILED',FALSE)
	FROM releases r
	JOIN applications a ON a.id=r.application_id
	JOIN environments e ON e.id=r.environment_id
	JOIN deployment_profiles p ON p.id=r.profile_id
	JOIN users u ON u.id=r.created_by
	CROSS JOIN app_settings settings
	LEFT JOIN releases rollback_source ON rollback_source.id=r.rollback_source_release_id
	LEFT JOIN LATERAL (SELECT id,original_filename,size_bytes,sha256 FROM release_artifacts WHERE release_id=r.id AND storage_path<>'' ORDER BY created_at DESC,id DESC LIMIT 1) art ON TRUE
	LEFT JOIN LATERAL (SELECT started_at,finished_at FROM release_jobs WHERE release_id=r.id ORDER BY created_at DESC,id DESC LIMIT 1) j ON TRUE
	LEFT JOIN deployment_heads head ON head.application_id=r.application_id AND head.environment_id=r.environment_id
	LEFT JOIN release_jobs head_basis ON head_basis.id=head.current_job_id
	LEFT JOIN LATERAL (
	    SELECT candidate.id,candidate.release_id,candidate.operation,candidate.status,candidate.rollback_source_release_id,candidate.rollback_source_job_id
	    FROM release_jobs candidate JOIN releases candidate_release ON candidate_release.id=candidate.release_id
	    WHERE candidate_release.application_id=r.application_id AND candidate_release.environment_id=r.environment_id
	    ORDER BY candidate.created_at DESC,candidate.id DESC LIMIT 1
	) latest_target ON TRUE`

func (s *Server) releaseRows(r *http.Request, limit, offset int, status, search string) ([]map[string]any, error) {
	rows, err := s.store.Pool.Query(r.Context(), releaseSelect+` WHERE ($1='' OR r.status=$1) AND ($2='' OR a.name ILIKE '%'||$2||'%' OR a.code ILIKE '%'||$2||'%' OR r.version ILIKE '%'||$2||'%') ORDER BY r.created_at DESC LIMIT $3 OFFSET $4`, status, search, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		item, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func scanRelease(row rowScanner) (map[string]any, error) {
	var id, appID, appName, envID, envName, profileID, profileName, version, notes, status, requestedOperation, creatorID, creatorName, decision string
	var rollbackSourceID, rollbackSourceJobID, retrySourceJobID, deploymentPresetID, quickBaseReleaseID, quickBaseJobID, rollbackSourceVersion, artifactID, artifactName, checksum *string
	var artifactSize *int64
	var created, updated time.Time
	var started, finished *time.Time
	var approvalRequired, rollbackEligible, retryEligible, quickRelease, autoDeployAfterApproval bool
	if err := row.Scan(&id, &appID, &appName, &envID, &envName, &profileID, &profileName, &version, &notes, &status, &requestedOperation, &rollbackSourceID, &rollbackSourceJobID, &retrySourceJobID, &deploymentPresetID, &quickRelease, &autoDeployAfterApproval, &quickBaseReleaseID, &quickBaseJobID, &rollbackSourceVersion, &creatorID, &creatorName, &decision, &created, &updated, &artifactID, &artifactName, &artifactSize, &checksum, &approvalRequired, &started, &finished, &rollbackEligible, &retryEligible); err != nil {
		return nil, err
	}
	approvalStatus := ""
	if approvalRequired {
		switch status {
		case "PENDING_REVIEW", "UNDER_REVIEW":
			approvalStatus = "PENDING"
		case "APPROVED":
			approvalStatus = "APPROVED"
		case "REJECTED":
			approvalStatus = "REJECTED"
		}
	}
	return map[string]any{
		"id": id, "applicationId": appID, "applicationName": appName, "environmentId": envID, "environmentName": envName,
		"deploymentProfileId": profileID, "deploymentProfileName": profileName, "version": version, "notes": notes, "status": status,
		"requestedOperation": requestedOperation, "rollbackSourceReleaseId": rollbackSourceID, "rollbackSourceJobId": rollbackSourceJobID, "rollbackSourceVersion": rollbackSourceVersion,
		"retrySourceJobId": retrySourceJobID, "retryRequested": retrySourceJobID != nil,
		"deploymentPresetId": deploymentPresetID, "quickRelease": quickRelease, "autoDeployAfterApproval": autoDeployAfterApproval,
		"quickBaseReleaseId": quickBaseReleaseID, "quickBaseJobId": quickBaseJobID,
		"rollbackEligible": rollbackEligible, "retryEligible": retryEligible,
		"artifactId": artifactID, "artifactName": artifactName, "artifactSize": artifactSize, "checksum": checksum, "contentUploaded": artifactID != nil,
		"createdBy": map[string]any{"id": creatorID, "username": creatorName}, "createdAt": created, "updatedAt": updated, "startedAt": started, "finishedAt": finished,
		"approval": map[string]any{"required": approvalRequired, "status": approvalStatus, "comment": decision},
	}, nil
}

func (s *Server) createRelease(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		s.createMultipartRelease(w, r)
		return
	}
	var input releaseInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := validateReleaseInput(input); err != nil {
		writeError(w, 400, "invalid_release", err.Error())
		return
	}
	id, err := s.insertRelease(r, input)
	if err != nil {
		writeError(w, 409, "release_conflict", err.Error())
		return
	}
	s.getReleaseByID(w, r, id, http.StatusCreated)
}

func (s *Server) insertRelease(r *http.Request, input releaseInput) (string, error) {
	id, _ := secure.NewID()
	p, _ := principalFrom(r)
	tag, err := s.store.Pool.Exec(r.Context(), `INSERT INTO releases(id,application_id,environment_id,profile_id,version,notes,created_by)
	SELECT $1,$2,$3,$4,$5,$6,$7 WHERE EXISTS(
		SELECT 1 FROM deployment_profiles p JOIN applications a ON a.id=p.application_id JOIN environments e ON e.id=p.environment_id
		WHERE p.id=$4 AND p.application_id=$2 AND p.environment_id=$3 AND p.active AND p.enabled AND p.revoked_at IS NULL AND a.active AND e.active)`, id, input.ApplicationID, input.EnvironmentID, input.ProfileID, strings.TrimSpace(input.Version), input.Notes, p.UserID)
	if err != nil {
		return "", errors.New("release version already exists for this application and environment")
	}
	if tag.RowsAffected() == 0 {
		return "", errors.New("deployment profile does not match the selected application and environment")
	}
	s.store.Audit(r.Context(), p.UserID, "release.create", "release", id, "success", remoteIP(r), r.UserAgent(), nil)
	return id, nil
}

func (s *Server) createMultipartRelease(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadAppSettings(r)
	if err != nil {
		writeError(w, 500, "database_error", "could not load storage settings")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, cfg.ArtifactMaxBytes+(2<<20))
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeError(w, 400, "invalid_upload", "invalid or oversized multipart request")
		return
	}
	input := releaseInput{ApplicationID: r.FormValue("applicationId"), EnvironmentID: r.FormValue("environmentId"), ProfileID: r.FormValue("deploymentProfileId"), Version: r.FormValue("version"), Notes: r.FormValue("notes")}
	if input.ProfileID == "" {
		_ = s.store.Pool.QueryRow(r.Context(), `SELECT id::text FROM deployment_profiles WHERE application_id=$1 AND environment_id=$2 AND active AND enabled AND revoked_at IS NULL ORDER BY created_at LIMIT 1`, input.ApplicationID, input.EnvironmentID).Scan(&input.ProfileID)
	}
	if err := validateReleaseInput(input); err != nil {
		writeError(w, 400, "invalid_release", err.Error())
		return
	}
	file, header, err := r.FormFile("artifact")
	if err != nil {
		writeError(w, 400, "artifact_required", "artifact file is required")
		return
	}
	defer file.Close()
	id, err := s.insertRelease(r, input)
	if err != nil {
		writeError(w, 409, "release_conflict", err.Error())
		return
	}
	if _, err = s.persistArtifact(r, id, header.Filename, header.Header.Get("Content-Type"), file, cfg); err != nil {
		_, _ = s.store.Pool.Exec(r.Context(), `DELETE FROM releases WHERE id=$1`, id)
		writeError(w, 400, "invalid_artifact", err.Error())
		return
	}
	s.getReleaseByID(w, r, id, 201)
}

func (s *Server) getRelease(w http.ResponseWriter, r *http.Request) {
	s.getReleaseByID(w, r, r.PathValue("id"), 200)
}
func (s *Server) getReleaseByID(w http.ResponseWriter, r *http.Request, id string, statusCode int) {
	item, err := scanRelease(s.store.Pool.QueryRow(r.Context(), releaseSelect+` WHERE r.id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "release not found")
		return
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not load release")
		return
	}
	steps, _ := s.loadReleaseSteps(r.Context(), id)
	images, _ := s.loadReleaseImages(r.Context(), id)
	item["steps"] = steps
	item["images"] = images
	writeJSON(w, statusCode, item)
}

func (s *Server) loadReleaseSteps(ctx context.Context, releaseID string) ([]map[string]any, error) {
	rows, err := s.store.Pool.Query(ctx, `SELECT s.id::text,s.name,s.status,s.started_at,s.finished_at,s.exit_code,s.message FROM release_job_steps s JOIN release_jobs j ON j.id=s.job_id WHERE j.release_id=$1 ORDER BY s.id`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, name, status string
		var started time.Time
		var finished *time.Time
		var exit *int
		var message *string
		if err := rows.Scan(&id, &name, &status, &started, &finished, &exit, &message); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"id": id, "name": name, "type": name, "status": status, "startedAt": started, "finishedAt": finished, "exitCode": exit, "message": message})
	}
	return items, rows.Err()
}
func (s *Server) loadReleaseImages(ctx context.Context, releaseID string) ([]map[string]any, error) {
	rows, err := s.store.Pool.Query(ctx, `SELECT i.id::text,i.repository,i.tag,i.digest FROM release_images i JOIN release_jobs j ON j.id=i.job_id WHERE j.release_id=$1 ORDER BY i.id`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, repo, tag string
		var digest *string
		if err := rows.Scan(&id, &repo, &tag, &digest); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"id": id, "repository": repo, "tag": tag, "digest": digest})
	}
	return items, rows.Err()
}

func (s *Server) updateRelease(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version   string `json:"version"`
		Notes     string `json:"notes"`
		ProfileID string `json:"deploymentProfileId"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Version) == "" || input.ProfileID == "" {
		writeError(w, 400, "invalid_release", "version and deploymentProfileId are required")
		return
	}
	id := r.PathValue("id")
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE releases r SET version=$2,notes=$3,profile_id=$4,updated_at=now() WHERE r.id=$1 AND r.status IN ('DRAFT','UPLOADED','REJECTED') AND EXISTS(SELECT 1 FROM deployment_profiles p WHERE p.id=$4 AND p.application_id=r.application_id AND p.environment_id=r.environment_id AND p.revoked_at IS NULL)`, id, strings.TrimSpace(input.Version), input.Notes, input.ProfileID)
	if err != nil {
		writeError(w, 409, "release_conflict", "release version conflicts or profile is invalid")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 409, "invalid_state", "only draft, uploaded, or rejected releases can be changed")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "release.update", "release", id, "success", remoteIP(r), r.UserAgent(), nil)
	s.getReleaseByID(w, r, id, 200)
}

func (s *Server) deleteRelease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not delete release")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	var status string
	if err := tx.QueryRow(r.Context(), `SELECT status FROM releases WHERE id=$1 FOR UPDATE`, id).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "release not found")
		return
	} else if err != nil {
		writeError(w, 500, "database_error", "could not delete release")
		return
	}
	if status != "DRAFT" && status != "UPLOADED" && status != "REJECTED" {
		writeError(w, 409, "invalid_state", "only draft, uploaded, or rejected releases can be deleted")
		return
	}
	var storageRoot string
	if err := tx.QueryRow(r.Context(), `SELECT artifact_storage_path FROM app_settings WHERE id='default' FOR SHARE`).Scan(&storageRoot); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load storage settings")
		return
	}
	rows, err := tx.Query(r.Context(), `SELECT storage_path FROM release_artifacts WHERE release_id=$1`, id)
	if err != nil {
		writeError(w, 500, "database_error", "could not delete release")
		return
	}
	paths := []string{}
	for rows.Next() {
		var path string
		_ = rows.Scan(&path)
		if path != "" {
			paths = append(paths, path)
		}
	}
	rows.Close()
	tag, err := tx.Exec(r.Context(), `DELETE FROM releases WHERE id=$1 AND status IN ('DRAFT','UPLOADED','REJECTED')`, id)
	if err != nil {
		writeError(w, 409, "invalid_state", "release cannot be deleted")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 409, "invalid_state", "only draft, uploaded, or rejected releases can be deleted")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "database_error", "release deletion could not be committed")
		return
	}
	for _, path := range paths {
		if err := removeStoredArtifact(storageRoot, path); err != nil {
			s.log.Warn("artifact cleanup after release deletion failed", "release_id", id, "artifact_path", path, "error", err)
		}
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "release.delete", "release", id, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(204)
}

type artifactMetadataInput struct {
	Filename  string `json:"filename"`
	MediaType string `json:"mediaType"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

func safeArtifactName(name string) bool {
	return name == filepath.Base(name) && !strings.ContainsAny(name, "/\\\x00\r\n") && (strings.HasSuffix(strings.ToLower(name), ".tar") || strings.HasSuffix(strings.ToLower(name), ".tar.gz"))
}
func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
func artifactRelativePath(releaseID, artifactID, extension string) (string, error) {
	if !validUUID(releaseID) || !validUUID(artifactID) || (extension != ".tar" && extension != ".tar.gz") {
		return "", errors.New("invalid artifact storage identifier")
	}
	relative := filepath.ToSlash(filepath.Join(releaseID, artifactID+extension))
	if filepath.IsAbs(relative) || strings.Contains(relative, "\\") {
		return "", errors.New("unsafe artifact storage path")
	}
	return relative, nil
}
func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}
func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Pool.Query(r.Context(), `SELECT id::text,original_filename,media_type,size_bytes,sha256,(storage_path<>''),created_at FROM release_artifacts WHERE release_id=$1 ORDER BY created_at,id`, r.PathValue("id"))
	if err != nil {
		writeError(w, 500, "database_error", "could not list artifacts")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, name, media, sha string
		var size int64
		var uploaded bool
		var created time.Time
		if rows.Scan(&id, &name, &media, &size, &sha, &uploaded, &created) != nil {
			writeError(w, 500, "database_error", "could not list artifacts")
			return
		}
		items = append(items, map[string]any{"id": id, "filename": name, "mediaType": media, "sizeBytes": size, "sha256": sha, "contentUploaded": uploaded, "createdAt": created})
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) createArtifactMetadata(w http.ResponseWriter, r *http.Request) {
	var input artifactMetadataInput
	if !decodeJSON(w, r, &input) {
		return
	}
	cfg, err := s.loadAppSettings(r)
	if err != nil {
		writeError(w, 500, "database_error", "could not load storage settings")
		return
	}
	if !safeArtifactName(input.Filename) || input.SizeBytes < 0 || input.SizeBytes > cfg.ArtifactMaxBytes || !validSHA256(input.SHA256) {
		writeError(w, 400, "invalid_artifact", "artifact must be a .tar or .tar.gz with valid size and lowercase SHA-256")
		return
	}
	p, _ := principalFrom(r)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{})
	if err != nil {
		writeError(w, 500, "database_error", "could not create artifact metadata")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	releaseID := r.PathValue("id")
	var status string
	if err = tx.QueryRow(r.Context(), `SELECT status FROM releases WHERE id=$1 FOR UPDATE`, releaseID).Scan(&status); err != nil || (status != "DRAFT" && status != "UPLOADED" && status != "REJECTED") {
		writeError(w, 400, "invalid_artifact", "release not found or cannot accept artifacts")
		return
	}
	var contentUploaded bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM release_artifacts WHERE release_id=$1 AND storage_path<>'')`, releaseID).Scan(&contentUploaded); err != nil {
		writeError(w, 500, "database_error", "could not validate artifact state")
		return
	}
	if contentUploaded {
		writeError(w, http.StatusConflict, "artifact_content_exists", "metadata-only artifacts cannot be added after content upload")
		return
	}
	id, _ := secure.NewID()
	tag, err := tx.Exec(r.Context(), `INSERT INTO release_artifacts(id,release_id,original_filename,media_type,size_bytes,sha256,uploaded_by) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, releaseID, input.Filename, input.MediaType, input.SizeBytes, input.SHA256, p.UserID)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil || tag.RowsAffected() == 0 {
		writeError(w, 400, "invalid_artifact", "release not found or cannot accept artifacts")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "artifact.metadata.create", "artifact", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 201, map[string]any{"id": id, "filename": input.Filename, "mediaType": input.MediaType, "sizeBytes": input.SizeBytes, "sha256": input.SHA256, "contentUploaded": false})
}

func (s *Server) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadAppSettings(r)
	if err != nil {
		writeError(w, 500, "database_error", "could not load storage settings")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, cfg.ArtifactMaxBytes+(2<<20))
	file, header, err := r.FormFile("artifact")
	if err != nil {
		writeError(w, 400, "invalid_upload", "multipart field artifact is required")
		return
	}
	defer file.Close()
	item, err := s.persistArtifact(r, r.PathValue("id"), header.Filename, header.Header.Get("Content-Type"), file, cfg)
	if err != nil {
		writeError(w, 400, "invalid_artifact", err.Error())
		return
	}
	writeJSON(w, 201, item)
}
func (s *Server) persistArtifact(r *http.Request, releaseID, filename, mediaType string, file multipart.File, cfg appSettingsResponse) (map[string]any, error) {
	if !safeArtifactName(filename) {
		return nil, errors.New("only .tar and .tar.gz files with safe filenames are accepted")
	}
	if !validUUID(releaseID) {
		return nil, errors.New("release ID is invalid")
	}
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{})
	if err != nil {
		return nil, errors.New("artifact storage transaction could not be started")
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	result, err := s.persistArtifactTx(r, tx, releaseID, filename, mediaType, file, cfg, false)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(r.Context()); err != nil {
		visible, verifyErr := s.artifactCommitVisible(result.id)
		if visible {
			// COMMIT reached PostgreSQL even though the client observed an
			// error. Treat the durable artifact row as the authoritative result.
		} else if commitDefinitelyRolledBack(err) {
			result.cleanup()
			return nil, errors.New("artifact metadata could not be committed")
		} else {
			s.log.Warn("artifact commit result could not be reconciled; stored content preserved", "artifact_id", result.id, "commit_error", err, "verify_error", verifyErr)
			return nil, errors.New("artifact commit result is unknown; stored content was preserved for recovery")
		}
	}
	principal, _ := principalFrom(r)
	s.store.Audit(r.Context(), principal.UserID, "artifact.upload", "artifact", result.id, "success", remoteIP(r), r.UserAgent(), nil)
	return result.item, nil
}

func (s *Server) artifactCommitVisible(artifactID string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var visible bool
		lastErr = s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM release_artifacts WHERE id=$1)`, artifactID).Scan(&visible)
		if lastErr == nil && visible {
			return true, nil
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return false, lastErr
}

func commitDefinitelyRolledBack(err error) bool {
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		return true
	}
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError)
}

type persistedArtifactResult struct {
	id      string
	item    map[string]any
	cleanup func()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func (s *Server) persistArtifactTx(r *http.Request, tx pgx.Tx, releaseID, filename, mediaType string, file multipart.File, cfg appSettingsResponse, allowQuickInitial bool) (persistedArtifactResult, error) {
	var releaseStatus string
	var quickRelease bool
	if err := tx.QueryRow(r.Context(), `SELECT status,quick_release FROM releases WHERE id=$1 FOR UPDATE`, releaseID).Scan(&releaseStatus, &quickRelease); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return persistedArtifactResult{}, errors.New("release not found or cannot accept artifacts")
		}
		return persistedArtifactResult{}, errors.New("release state could not be locked")
	}
	if releaseStatus != "DRAFT" && releaseStatus != "UPLOADED" && releaseStatus != "REJECTED" {
		return persistedArtifactResult{}, errors.New("release not found or cannot accept artifacts")
	}
	if quickRelease && !allowQuickInitial {
		return persistedArtifactResult{}, errors.New("quick release artifacts are immutable; create a new quick release instead")
	}
	// Hold a shared lock for the complete file write and metadata insert. Storage
	// path changes take an exclusive row lock, so a relative artifact can never
	// be recorded against a different root than the one it was written under.
	if err := tx.QueryRow(r.Context(), `SELECT artifact_storage_path,artifact_max_bytes FROM app_settings WHERE id='default' FOR SHARE`).Scan(&cfg.ArtifactStoragePath, &cfg.ArtifactMaxBytes); err != nil {
		return persistedArtifactResult{}, errors.New("artifact storage settings are unavailable")
	}
	id, _ := secure.NewID()
	if !filepath.IsAbs(cfg.ArtifactStoragePath) {
		return persistedArtifactResult{}, errors.New("artifact storage root must be absolute")
	}
	if err := os.MkdirAll(cfg.ArtifactStoragePath, 0750); err != nil {
		return persistedArtifactResult{}, errors.New("artifact storage root is unavailable")
	}
	root, err := filepath.EvalSymlinks(cfg.ArtifactStoragePath)
	if err != nil {
		return persistedArtifactResult{}, errors.New("artifact storage root is unavailable")
	}
	directory := filepath.Join(root, releaseID)
	if err := os.MkdirAll(directory, 0750); err != nil {
		return persistedArtifactResult{}, errors.New("artifact storage directory is unavailable")
	}
	if info, err := os.Lstat(directory); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return persistedArtifactResult{}, errors.New("artifact storage directory is not a real directory")
	}
	// Persist the release directory entry before PostgreSQL can reference a
	// file below it. The runtime is Linux-only, where directory fsync provides
	// the required crash-durability boundary for mkdir/rename operations.
	if err := syncDirectory(root); err != nil {
		return persistedArtifactResult{}, errors.New("artifact storage directory could not be synchronized")
	}
	ext := ".tar"
	if strings.HasSuffix(strings.ToLower(filename), ".tar.gz") {
		ext = ".tar.gz"
	}
	target := filepath.Join(directory, id+ext)
	relativePath, err := artifactRelativePath(releaseID, id, ext)
	if err != nil {
		return persistedArtifactResult{}, err
	}
	partialToken, err := secure.RandomToken(16)
	if err != nil {
		return persistedArtifactResult{}, errors.New("artifact staging name could not be created")
	}
	partial := target + ".partial-" + partialToken
	preserved := false
	renamed := false
	defer func() {
		if err := os.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("could not remove partial artifact", "path", partial, "error", err)
		}
		if !preserved {
			if renamed {
				if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
					s.log.Warn("could not remove uncommitted artifact", "path", target, "error", err)
				}
			}
			if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.log.Warn("could not remove empty artifact directory", "path", directory, "error", err)
			}
		}
	}()
	output, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return persistedArtifactResult{}, errors.New("artifact storage file could not be created")
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(file, cfg.ArtifactMaxBytes+1))
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || size > cfg.ArtifactMaxBytes {
		return persistedArtifactResult{}, errors.New("artifact exceeds the configured limit or could not be written")
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return persistedArtifactResult{}, errors.New("artifact storage target already exists or cannot be checked")
	}
	if err := os.Rename(partial, target); err != nil {
		return persistedArtifactResult{}, errors.New("artifact could not be atomically committed to storage")
	}
	renamed = true
	if err := syncDirectory(directory); err != nil {
		return persistedArtifactResult{}, errors.New("artifact storage file could not be synchronized")
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	p, _ := principalFrom(r)
	if _, err := tx.Exec(r.Context(), `DELETE FROM release_artifacts WHERE release_id=$1 AND storage_path=''`, releaseID); err != nil {
		return persistedArtifactResult{}, errors.New("stale artifact metadata could not be removed")
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO release_artifacts(id,release_id,original_filename,storage_path,media_type,size_bytes,sha256,uploaded_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, releaseID, filename, relativePath, mediaType, size, checksum, p.UserID); err != nil {
		return persistedArtifactResult{}, errors.New("release not found or cannot accept artifacts")
	}
	tag, err := tx.Exec(r.Context(), `UPDATE releases SET status='UPLOADED',updated_at=now() WHERE id=$1 AND status IN ('DRAFT','UPLOADED','REJECTED')`, releaseID)
	if err != nil || tag.RowsAffected() == 0 {
		return persistedArtifactResult{}, errors.New("artifact metadata could not be committed")
	}
	preserved = true
	cleanup := func() {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("could not remove rolled-back artifact", "path", target, "error", err)
		}
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("could not remove rolled-back artifact directory", "path", directory, "error", err)
		}
	}
	return persistedArtifactResult{id: id, item: map[string]any{"id": id, "filename": filename, "mediaType": mediaType, "sizeBytes": size, "sha256": checksum, "contentUploaded": true}, cleanup: cleanup}, nil
}

func removeStoredArtifact(root, relative string) error {
	if !filepath.IsAbs(root) || relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") {
		return errors.New("unsafe artifact path")
	}
	root = filepath.Clean(root)
	target := filepath.Join(root, filepath.Clean(filepath.FromSlash(relative)))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("artifact path escapes storage root")
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("artifact is not a regular file")
	}
	return os.Remove(target)
}

func (s *Server) workflowRequired(ctx context.Context, releaseID string) (bool, error) {
	var required bool
	err := s.store.Pool.QueryRow(ctx, `SELECT r.status IN ('PENDING_REVIEW','UNDER_REVIEW') OR (settings.approval_enabled AND (p.approval_required OR e.protected OR upper(e.code)=ANY(regexp_split_to_array(upper(COALESCE(settings.approval_config->>'protectedEnvironments','')), '[[:space:]]*,[[:space:]]*')))) FROM releases r JOIN deployment_profiles p ON p.id=r.profile_id JOIN environments e ON e.id=r.environment_id CROSS JOIN app_settings settings WHERE r.id=$1`, releaseID).Scan(&required)
	return required, err
}

func requiredScriptForOperation(operation string) (phase, scriptType string, ok bool) {
	switch operation {
	case "DEPLOY":
		return "DEPLOY", "DEPLOY", true
	case "ROLLBACK":
		return "ROLLBACK", "ROLLBACK", true
	default:
		return "", "", false
	}
}

type dependencyQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func deploymentDependenciesReady(ctx context.Context, queryer dependencyQueryer, profileID, operation string) (bool, error) {
	phase, scriptType, ok := requiredScriptForOperation(operation)
	if !ok {
		return false, errors.New("unsupported release operation")
	}
	var ready bool
	err := queryer.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM deployment_profiles p
		JOIN runner_credentials c ON c.id=p.registry_credential_id
		WHERE p.id=$1 AND p.active AND p.enabled AND p.revoked_at IS NULL
		  AND p.runtime_kind IN ('docker','podman','containerd') AND p.runtime_binary_path LIKE '/%'
		  AND p.registry_url<>'' AND p.registry_host<>'' AND p.registry_project<>''
		  AND c.active AND c.approved_at IS NOT NULL AND c.revoked_at IS NULL
		  AND EXISTS(SELECT 1 FROM deployment_profile_scripts ps JOIN script_versions sv ON sv.id=ps.script_version_id
		             WHERE ps.profile_id=p.id AND ps.phase=$2 AND sv.script_type=$3
		               AND sv.active AND sv.approved_at IS NOT NULL AND sv.revoked_at IS NULL))`, profileID, phase, scriptType).Scan(&ready)
	return ready, err
}

func matchingRunnerAvailable(ctx context.Context, queryer dependencyQueryer, requiredLabels []string) (bool, error) {
	var ready bool
	err := queryer.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM runner_instances
		WHERE active
		  AND managed_by_runner
		  AND worker_id IS NOT NULL
		  AND last_heartbeat_at >= clock_timestamp() - interval '60 seconds'
		  AND $1::text[] <@ labels)`, requiredLabels).Scan(&ready)
	return ready, err
}

func activeTargetJobExists(ctx context.Context, queryer dependencyQueryer, lockKey string) (bool, error) {
	var exists bool
	err := queryer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM release_jobs WHERE lock_key=$1 AND status NOT IN ('SUCCESS','FAILED','ROLLED_BACK'))`, lockKey).Scan(&exists)
	return exists, err
}

// lockReleaseTargetForJob serializes every server-side job producer for one
// immutable application/environment target. The matching migration trigger
// applies the same lock to direct/future release_jobs inserts before older
// validation triggers touch deployment_heads. READ COMMITTED transactions are
// required so the row-locking query observes a job committed while a large
// Quick artifact was being copied; a concurrently held advisory lock is treated
// as an immediate target conflict.
func lockReleaseTargetForJob(ctx context.Context, tx pgx.Tx, lockKey string) (bool, error) {
	var targetLockAcquired bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended(lower($1::text), 20260827))`, lockKey).Scan(&targetLockAcquired); err != nil {
		return false, err
	}
	if !targetLockAcquired {
		return true, nil
	}
	var activeJobID string
	err := tx.QueryRow(ctx, `SELECT id::text
		FROM release_jobs
		WHERE lower(lock_key)=lower($1) AND status NOT IN ('SUCCESS','FAILED','ROLLED_BACK')
		ORDER BY created_at,id
		LIMIT 1
		FOR UPDATE`, lockKey).Scan(&activeJobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func loadTargetCredentialSnapshot(ctx context.Context, tx pgx.Tx, profileID string) (*string, *int, error) {
	var credentialID *string
	if err := tx.QueryRow(ctx, `SELECT target_credential_id::text FROM deployment_profiles WHERE id=$1 FOR SHARE`, profileID).Scan(&credentialID); err != nil {
		return nil, nil, err
	}
	if credentialID == nil {
		return nil, nil, nil
	}
	var version int
	if err := tx.QueryRow(ctx, `SELECT version FROM target_credentials WHERE id=$1 AND active AND approved_at IS NOT NULL AND revoked_at IS NULL FOR SHARE`, *credentialID).Scan(&version); err != nil {
		return nil, nil, errors.New("deployment target credential is inactive or revoked")
	}
	return credentialID, &version, nil
}

func releaseCanQueue(status string, approvalRequired bool) bool {
	switch status {
	case "UPLOADED", "APPROVED", "DRAFT":
		return true
	case "PENDING_REVIEW", "UNDER_REVIEW":
		return !approvalRequired
	case "REJECTED":
		return !approvalRequired
	default:
		return false
	}
}

func releaseTargetLockKey(applicationID, environmentID string) (string, error) {
	if !validUUID(applicationID) || !validUUID(environmentID) {
		return "", errors.New("invalid release target identifiers")
	}
	return strings.ToLower(applicationID) + ":" + strings.ToLower(environmentID), nil
}

func latestFailedDeployJob(ctx context.Context, queryer dependencyQueryer, releaseID string) (string, error) {
	var jobID *string
	err := queryer.QueryRow(ctx, `SELECT CASE WHEN latest.release_id=$1::uuid AND latest.operation='DEPLOY' AND latest.status='FAILED' THEN latest.id::text END
		FROM releases target
		LEFT JOIN LATERAL(
			SELECT job.id,job.release_id,job.operation,job.status
			FROM release_jobs job JOIN releases candidate ON candidate.id=job.release_id
			WHERE candidate.application_id=target.application_id AND candidate.environment_id=target.environment_id
			ORDER BY job.created_at DESC,job.id DESC LIMIT 1
		) latest ON TRUE WHERE target.id=$1`, releaseID).Scan(&jobID)
	if err != nil {
		return "", err
	}
	if jobID == nil {
		return "", errors.New("release is not the latest failed deployment attempt")
	}
	return *jobID, nil
}

type rollbackPlan struct {
	SourceReleaseID string
	SourceJobID     string
	Mode            string
}

func resolveRollbackPlan(ctx context.Context, queryer dependencyQueryer, releaseID string, allowFailedRollbackRetry bool) (rollbackPlan, error) {
	var headReleaseID, headJobID, latestJobID, latestReleaseID, latestOperation, latestStatus string
	var latestSourceID, latestSourceJobID, priorHeadID, priorHeadJobID *string
	err := queryer.QueryRow(ctx, `SELECT head.current_release_id::text,head.current_job_id::text,
		latest.id::text,latest.release_id::text,latest.operation,latest.status,latest.rollback_source_release_id::text,latest.rollback_source_job_id::text,
		basis.rollback_source_release_id::text,basis.rollback_source_job_id::text
		FROM releases target
		JOIN deployment_heads head ON head.application_id=target.application_id AND head.environment_id=target.environment_id
		JOIN release_jobs basis ON basis.id=head.current_job_id AND basis.release_id=head.current_release_id AND basis.operation='DEPLOY' AND basis.status='SUCCESS'
		JOIN LATERAL(
			SELECT candidate.id,candidate.release_id,candidate.operation,candidate.status,candidate.rollback_source_release_id,candidate.rollback_source_job_id
			FROM release_jobs candidate JOIN releases candidate_release ON candidate_release.id=candidate.release_id
			WHERE candidate_release.application_id=target.application_id AND candidate_release.environment_id=target.environment_id
			ORDER BY candidate.created_at DESC,candidate.id DESC LIMIT 1
		) latest ON TRUE
		WHERE target.id=$1
		FOR SHARE OF head`, releaseID).Scan(&headReleaseID, &headJobID, &latestJobID, &latestReleaseID, &latestOperation, &latestStatus, &latestSourceID, &latestSourceJobID, &priorHeadID, &priorHeadJobID)
	if err != nil {
		return rollbackPlan{}, err
	}
	if releaseID == headReleaseID {
		if priorHeadID == nil || priorHeadJobID == nil {
			return rollbackPlan{}, errors.New("the current deployment has no previous verified release")
		}
		eligible := latestJobID == headJobID || latestOperation == "ROLLBACK" && latestStatus == "ROLLED_BACK" && latestSourceID != nil && latestSourceJobID != nil && *latestSourceID == headReleaseID && *latestSourceJobID == headJobID
		if allowFailedRollbackRetry {
			eligible = eligible || latestReleaseID == releaseID && latestOperation == "ROLLBACK" && latestStatus == "FAILED" && latestSourceID != nil && latestSourceJobID != nil && *latestSourceID == *priorHeadID && *latestSourceJobID == *priorHeadJobID
		}
		if !eligible {
			return rollbackPlan{}, errors.New("the current deployment is shadowed by a newer deployment attempt")
		}
		return rollbackPlan{SourceReleaseID: *priorHeadID, SourceJobID: *priorHeadJobID, Mode: "HEAD"}, nil
	}
	eligible := latestReleaseID == releaseID && latestOperation == "DEPLOY" && latestStatus == "FAILED"
	if allowFailedRollbackRetry {
		eligible = eligible || latestReleaseID == releaseID && latestOperation == "ROLLBACK" && latestStatus == "FAILED" && latestSourceID != nil && latestSourceJobID != nil && *latestSourceID == headReleaseID && *latestSourceJobID == headJobID
	}
	if !eligible {
		return rollbackPlan{}, errors.New("release is not the latest failed deployment attempt")
	}
	return rollbackPlan{SourceReleaseID: headReleaseID, SourceJobID: headJobID, Mode: "FAILED_RECOVERY"}, nil
}

type retryContextKey struct{}

func retryRequested(r *http.Request) bool {
	retry, _ := r.Context().Value(retryContextKey{}).(bool)
	return retry
}

func isSerializationFailure(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && (postgresError.Code == "40001" || postgresError.Code == "40P01")
}

func (s *Server) retryRelease(w http.ResponseWriter, r *http.Request) {
	s.enqueueRelease(w, r.WithContext(context.WithValue(r.Context(), retryContextKey{}, true)))
}

func (s *Server) enqueueRelease(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	id := r.PathValue("id")
	retry := retryRequested(r)
	var requestedOperation string
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT requested_operation FROM releases WHERE id=$1`, id).Scan(&requestedOperation); err == nil && requestedOperation == "ROLLBACK" {
		s.rollbackRelease(w, r)
		return
	}
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		writeError(w, 500, "database_error", "could not submit release")
		return
	}
	defer tx.Rollback(r.Context())
	var status, app, version, env, appID, envID, profileID, artifactID, path, checksum, releaseCreatorID string
	var persistedRetrySourceJobID, quickBaseReleaseID, quickBaseJobID *string
	var runnerLabels []string
	var profileMatches, targetActive, required, quickRelease bool
	err = tx.QueryRow(r.Context(), `SELECT r.status,a.code,r.version,e.code,r.application_id::text,r.environment_id::text,r.profile_id::text,r.retry_source_job_id::text,r.created_by,r.quick_release,r.quick_base_release_id::text,r.quick_base_job_id::text,(p.application_id=r.application_id AND p.environment_id=r.environment_id),(a.active AND e.active AND p.active AND p.enabled AND p.revoked_at IS NULL),settings.approval_enabled AND (p.approval_required OR e.protected OR upper(e.code)=ANY(regexp_split_to_array(upper(COALESCE(settings.approval_config->>'protectedEnvironments','')), '[[:space:]]*,[[:space:]]*'))),COALESCE(art.id::text,''),COALESCE(art.storage_path,''),COALESCE(art.sha256,''),p.runner_labels FROM releases r JOIN applications a ON a.id=r.application_id JOIN environments e ON e.id=r.environment_id JOIN deployment_profiles p ON p.id=r.profile_id CROSS JOIN app_settings settings LEFT JOIN LATERAL(SELECT id,storage_path,sha256 FROM release_artifacts WHERE release_id=r.id AND storage_path<>'' ORDER BY created_at DESC,id DESC LIMIT 1) art ON TRUE WHERE r.id=$1 FOR UPDATE OF r`, id).Scan(&status, &app, &version, &env, &appID, &envID, &profileID, &persistedRetrySourceJobID, &releaseCreatorID, &quickRelease, &quickBaseReleaseID, &quickBaseJobID, &profileMatches, &targetActive, &required, &artifactID, &path, &checksum, &runnerLabels)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "release not found")
		return
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not submit release")
		return
	}
	if path == "" {
		writeError(w, 409, "artifact_content_required", "uploaded artifact content is required before submission")
		return
	}
	if retry && status != "FAILED" {
		writeError(w, 409, "retry_requires_failed", "only a failed release job can be retried")
		return
	}
	retrySourceJobID := persistedRetrySourceJobID
	if retry || persistedRetrySourceJobID != nil {
		latestJobID, latestErr := latestFailedDeployJob(r.Context(), tx, id)
		if latestErr != nil || persistedRetrySourceJobID != nil && latestJobID != *persistedRetrySourceJobID {
			if persistedRetrySourceJobID != nil {
				_, resetErr := tx.Exec(r.Context(), `UPDATE releases SET status='FAILED',retry_source_job_id=NULL,operation_requested_by=NULL,decision_note='Retry request invalidated because a newer deployment attempt exists',reviewed_by=NULL,approved_by=NULL,rejected_by=NULL,updated_at=now() WHERE id=$1`, id)
				if resetErr == nil {
					resetErr = tx.Commit(r.Context())
				}
				if resetErr == nil {
					s.store.Audit(r.Context(), p.UserID, "release.retry.invalidate", "release", id, "failure", remoteIP(r), r.UserAgent(), nil)
				}
			}
			writeError(w, http.StatusConflict, "retry_target_stale", "only the latest failed deployment attempt can be retried")
			return
		}
		retrySourceJobID = &latestJobID
	}
	effectiveRetry := retrySourceJobID != nil
	if !retry && status == "FAILED" {
		writeError(w, 409, "retry_required", "use the explicit retry action for a failed release")
		return
	}
	if !profileMatches {
		writeError(w, 409, "release_target_drift", "deployment profile no longer matches the release application and environment")
		return
	}
	if !targetActive {
		writeError(w, 409, "release_target_inactive", "application, environment, and deployment profile must be active")
		return
	}
	dependenciesReady, dependencyErr := deploymentDependenciesReady(r.Context(), tx, profileID, "DEPLOY")
	if dependencyErr != nil || !dependenciesReady {
		writeError(w, http.StatusConflict, "deployment_profile_not_ready", "an active registry credential and approved DEPLOY script are required")
		return
	}
	if required && status != "APPROVED" {
		if status != "UPLOADED" && status != "DRAFT" && status != "REJECTED" && !(effectiveRetry && status == "FAILED") {
			writeError(w, 409, "invalid_state", "release is already in the approval or deployment process")
			return
		}
		_, err = tx.Exec(r.Context(), `UPDATE releases SET status='PENDING_REVIEW',retry_source_job_id=$2,decision_note='',reviewed_by=NULL,approved_by=NULL,rejected_by=NULL,operation_requested_by=CASE WHEN $2::uuid IS NOT NULL THEN $3 ELSE operation_requested_by END,updated_at=now() WHERE id=$1`, id, retrySourceJobID, p.UserID)
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			writeError(w, 500, "database_error", "could not submit for review")
			return
		}
		s.store.Audit(r.Context(), p.UserID, "release.submit_review", "release", id, "success", remoteIP(r), r.UserAgent(), nil)
		s.getReleaseByID(w, r, id, 200)
		return
	}
	if !releaseCanQueue(status, required) && !(effectiveRetry && status == "FAILED") {
		writeError(w, 409, "invalid_state", "release is not ready to deploy")
		return
	}
	runnerReady, runnerErr := matchingRunnerAvailable(r.Context(), tx, runnerLabels)
	if runnerErr != nil {
		writeError(w, 500, "database_error", "could not validate runner availability")
		return
	}
	if !runnerReady {
		writeError(w, http.StatusConflict, "runner_unavailable", "no active online runner matches the deployment profile labels")
		return
	}
	lockKey, err := releaseTargetLockKey(appID, envID)
	if err != nil {
		writeError(w, 500, "database_error", "release target identifiers are invalid")
		return
	}
	targetBusy, busyErr := lockReleaseTargetForJob(r.Context(), tx, lockKey)
	if busyErr != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not validate the deployment target lock")
		return
	}
	if targetBusy {
		writeError(w, http.StatusConflict, "job_conflict", "release target already has an active deployment job")
		return
	}
	targetCredentialID, targetCredentialVersion, credentialErr := loadTargetCredentialSnapshot(r.Context(), tx, profileID)
	if credentialErr != nil {
		writeError(w, http.StatusConflict, "target_credential_not_ready", credentialErr.Error())
		return
	}
	var priorHeadID, priorHeadJobID *string
	err = tx.QueryRow(r.Context(), `SELECT current_release_id::text,current_job_id::text FROM deployment_heads WHERE application_id=$1 AND environment_id=$2 FOR SHARE`, appID, envID).Scan(&priorHeadID, &priorHeadJobID)
	if errors.Is(err, pgx.ErrNoRows) {
		priorHeadID = nil
		priorHeadJobID = nil
	} else if err != nil {
		writeError(w, 500, "database_error", "could not snapshot the current deployment head")
		return
	}
	if quickRelease && (!sameNullableString(quickBaseReleaseID, priorHeadID) || !sameNullableString(quickBaseJobID, priorHeadJobID)) {
		writeError(w, http.StatusConflict, "deployment_head_changed", "the deployed version changed after preflight; create a new quick release")
		return
	}
	jobCreatorID := p.UserID
	if quickRelease {
		jobCreatorID = releaseCreatorID
		priorHeadID, priorHeadJobID = quickBaseReleaseID, quickBaseJobID
	}
	jobID, _ := secure.NewID()
	_, err = tx.Exec(r.Context(), `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,rollback_source_release_id,rollback_source_job_id,retry_of_job_id,target_credential_id,target_credential_version,runner_labels,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, jobID, id, profileID, app, version, env, lockKey, artifactID, path, checksum, priorHeadID, priorHeadJobID, retrySourceJobID, targetCredentialID, targetCredentialVersion, runnerLabels, jobCreatorID)
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE releases SET status='QUEUED',retry_source_job_id=NULL,updated_at=now() WHERE id=$1`, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 409, "job_conflict", "release already has an active deployment job")
		return
	}
	action := "release.enqueue"
	if effectiveRetry {
		action = "release.retry"
	}
	s.store.Audit(r.Context(), p.UserID, action, "release", id, "success", remoteIP(r), r.UserAgent(), nil)
	s.getReleaseByID(w, r, id, 200)
}

type decisionInput struct {
	Comment string `json:"comment"`
}

func (s *Server) reviewRelease(w http.ResponseWriter, r *http.Request) {
	s.decision(w, r, "UNDER_REVIEW", "reviewed_by", []string{"PENDING_REVIEW"}, "release.review")
}
func (s *Server) approveRelease(w http.ResponseWriter, r *http.Request) {
	s.decision(w, r, "APPROVED", "approved_by", []string{"PENDING_REVIEW", "UNDER_REVIEW"}, "release.approve")
}
func (s *Server) rejectRelease(w http.ResponseWriter, r *http.Request) {
	s.decision(w, r, "REJECTED", "rejected_by", []string{"PENDING_REVIEW", "UNDER_REVIEW"}, "release.reject")
}
func (s *Server) decision(w http.ResponseWriter, r *http.Request, newStatus, actorColumn string, allowed []string, action string) {
	var input decisionInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if len(input.Comment) > 4096 {
		writeError(w, 400, "invalid_comment", "comment is too long")
		return
	}
	id := r.PathValue("id")
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		writeError(w, 500, "database_error", "could not update approval")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	var currentStatus, creator, profileID, requestedOperation string
	var rollbackSourceID, rollbackSourceJobID, retrySourceJobID, operationBaseStatus *string
	var required, allowSelf, requireRejectComment, profileMatches, quickRelease, autoDeployAfterApproval bool
	err = tx.QueryRow(r.Context(), `SELECT r.status,COALESCE(r.operation_requested_by,r.created_by),r.profile_id::text,r.requested_operation,r.rollback_source_release_id::text,r.rollback_source_job_id::text,r.retry_source_job_id::text,r.operation_base_status,
		(r.status IN ('PENDING_REVIEW','UNDER_REVIEW') OR (settings.approval_enabled AND (p.approval_required OR e.protected OR upper(e.code)=ANY(regexp_split_to_array(upper(COALESCE(settings.approval_config->>'protectedEnvironments','')), '[[:space:]]*,[[:space:]]*'))))),
		COALESCE((settings.approval_config->>'allowSelfApproval')::boolean,FALSE),
		COALESCE((settings.approval_config->>'requireRejectComment')::boolean,TRUE),
		(p.application_id=r.application_id AND p.environment_id=r.environment_id),r.quick_release,r.auto_deploy_after_approval
		FROM releases r JOIN deployment_profiles p ON p.id=r.profile_id JOIN environments e ON e.id=r.environment_id CROSS JOIN app_settings settings
		WHERE r.id=$1 FOR UPDATE OF r`, id).Scan(&currentStatus, &creator, &profileID, &requestedOperation, &rollbackSourceID, &rollbackSourceJobID, &retrySourceJobID, &operationBaseStatus, &required, &allowSelf, &requireRejectComment, &profileMatches, &quickRelease, &autoDeployAfterApproval)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "release not found")
		return
	}
	if err != nil {
		if isSerializationFailure(err) {
			writeError(w, http.StatusConflict, "approval_conflict", "approval was already handled by another request")
			return
		}
		writeError(w, 500, "database_error", "could not validate approval")
		return
	}
	if !required {
		writeError(w, 409, "approval_disabled", "approval workflow is disabled for this release")
		return
	}
	p, _ := principalFrom(r)
	if newStatus == "APPROVED" {
		if creator == p.UserID && !allowSelf {
			writeError(w, http.StatusForbidden, "self_approval_forbidden", "the release requester cannot approve their own release")
			return
		}
		operation := "DEPLOY"
		if retrySourceJobID != nil {
			latestJobID, latestErr := latestFailedDeployJob(r.Context(), tx, id)
			if latestErr != nil || latestJobID != *retrySourceJobID {
				writeError(w, http.StatusConflict, "retry_target_stale", "a newer deployment attempt exists; recreate the retry request")
				return
			}
		}
		if requestedOperation == "ROLLBACK" {
			operation = "ROLLBACK"
			if rollbackSourceID == nil || rollbackSourceJobID == nil {
				writeError(w, 409, "rollback_source_invalid", "rollback approval has no source release")
				return
			}
			// A rollback retry is represented by the latest exact FAILED
			// ROLLBACK job plus the source release/basis job frozen on the
			// approval request. Allow that shape here while resolveRollbackPlan
			// still requires it to be the latest target attempt and to match the
			// current verified head.
			plan, planErr := resolveRollbackPlan(r.Context(), tx, id, true)
			if planErr != nil || plan.SourceReleaseID != *rollbackSourceID || plan.SourceJobID != *rollbackSourceJobID {
				writeError(w, 409, "rollback_target_stale", "the verified deployment head changed; recreate the rollback request")
				return
			}
			if err := tx.QueryRow(r.Context(), `SELECT source.profile_id::text,(profile.application_id=request.application_id AND profile.environment_id=request.environment_id)
				FROM releases request JOIN releases source ON source.id=request.rollback_source_release_id
				JOIN deployment_profiles profile ON profile.id=source.profile_id
				JOIN release_jobs basis ON basis.id=request.rollback_source_job_id AND basis.release_id=source.id AND basis.operation='DEPLOY' AND basis.status='SUCCESS'
				WHERE request.id=$1 AND source.application_id=request.application_id AND source.environment_id=request.environment_id`, id).Scan(&profileID, &profileMatches); err != nil {
				writeError(w, 409, "rollback_source_invalid", "rollback source release is no longer valid")
				return
			}
		}
		if !profileMatches {
			writeError(w, 409, "release_target_drift", "deployment profile no longer matches the release application and environment")
			return
		}
		dependenciesReady, dependencyErr := deploymentDependenciesReady(r.Context(), tx, profileID, operation)
		if dependencyErr != nil || !dependenciesReady {
			writeError(w, http.StatusConflict, "deployment_profile_not_ready", "an active registry credential and approved operation script are required")
			return
		}
	}
	if newStatus == "REJECTED" && strings.TrimSpace(input.Comment) == "" {
		if requireRejectComment {
			writeError(w, 400, "reject_comment_required", "a rejection comment is required")
			return
		}
	}
	if !contains(allowed, currentStatus) {
		writeError(w, 409, "invalid_state", "release is not in an allowed approval state")
		return
	}
	if newStatus == "APPROVED" && quickRelease && autoDeployAfterApproval && requestedOperation == "DEPLOY" && retrySourceJobID == nil {
		tag, updateErr := tx.Exec(r.Context(), `UPDATE releases SET status='APPROVED',approved_by=$2,decision_note=$3,updated_at=now() WHERE id=$1 AND status=ANY($4)`, id, p.UserID, input.Comment, allowed)
		if updateErr != nil || tag.RowsAffected() == 0 {
			writeError(w, http.StatusConflict, "approval_conflict", "approval could not be applied because deployment inputs changed")
			return
		}
		if _, queueErr := s.queueQuickDeployTx(r.Context(), tx, id, "APPROVED"); queueErr != nil {
			writeQuickQueueError(w, queueErr, "database_error", "approved quick release could not be queued")
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, http.StatusConflict, "approval_conflict", "approval and automatic deployment conflicted with another request")
			return
		}
		s.store.Audit(r.Context(), p.UserID, action, "release", id, "success", remoteIP(r), r.UserAgent(), nil)
		s.store.Audit(r.Context(), p.UserID, "release.enqueue.auto", "release", id, "success", remoteIP(r), r.UserAgent(), nil)
		s.getReleaseByID(w, r, id, http.StatusOK)
		return
	}
	targetStatus := newStatus
	clearRollbackRequest := newStatus == "REJECTED" && requestedOperation == "ROLLBACK"
	clearRetryRequest := newStatus == "REJECTED" && retrySourceJobID != nil
	if clearRollbackRequest {
		if operationBaseStatus == nil || (*operationBaseStatus != "SUCCESS" && *operationBaseStatus != "FAILED") {
			writeError(w, 409, "rollback_request_invalid", "rollback request has no terminal base status")
			return
		}
		targetStatus = *operationBaseStatus
	}
	query := fmt.Sprintf(`UPDATE releases SET status=$2,%s=$3,decision_note=$4,
		requested_operation=CASE WHEN $6 THEN 'DEPLOY' ELSE requested_operation END,
		rollback_source_release_id=CASE WHEN $6 THEN NULL ELSE rollback_source_release_id END,
		rollback_source_job_id=CASE WHEN $6 THEN NULL ELSE rollback_source_job_id END,
		retry_source_job_id=CASE WHEN $7 THEN NULL ELSE retry_source_job_id END,
		operation_requested_by=CASE WHEN $6 OR $7 THEN NULL ELSE operation_requested_by END,
		operation_base_status=CASE WHEN $6 OR $7 THEN NULL ELSE operation_base_status END,
		updated_at=now() WHERE id=$1 AND status=ANY($5)`, actorColumn)
	tag, err := tx.Exec(r.Context(), query, id, targetStatus, p.UserID, input.Comment, allowed, clearRollbackRequest, clearRetryRequest)
	if err != nil {
		writeError(w, 500, "database_error", "could not update approval")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 409, "invalid_state", "release is not in an allowed approval state")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 409, "approval_conflict", "approval could not be committed because deployment inputs changed")
		return
	}
	s.store.Audit(r.Context(), p.UserID, action, "release", id, "success", remoteIP(r), r.UserAgent(), nil)
	s.getReleaseByID(w, r, id, 200)
}

func (s *Server) rollbackRelease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, _ := principalFrom(r)
	retry := retryRequested(r)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		writeError(w, 500, "database_error", "could not prepare rollback")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	var currentStatus, requestedOperation, app, env, appID, envID string
	var persistedSourceID, persistedSourceJobID, persistedBaseStatus *string
	var targetActive bool
	err = tx.QueryRow(r.Context(), `SELECT current.status,current.requested_operation,current.rollback_source_release_id::text,current.rollback_source_job_id::text,current.operation_base_status,a.code,e.code,current.application_id::text,current.environment_id::text,(a.active AND e.active)
		FROM releases current JOIN applications a ON a.id=current.application_id JOIN environments e ON e.id=current.environment_id
		WHERE current.id=$1 FOR UPDATE OF current`, id).Scan(&currentStatus, &requestedOperation, &persistedSourceID, &persistedSourceJobID, &persistedBaseStatus, &app, &env, &appID, &envID, &targetActive)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "release not found")
		return
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not prepare rollback")
		return
	}
	if retry && currentStatus != "FAILED" {
		writeError(w, 409, "retry_requires_failed", "only a failed rollback job can be retried")
		return
	}
	if !retry && currentStatus == "FAILED" && requestedOperation == "ROLLBACK" {
		writeError(w, 409, "retry_required", "use the explicit retry action for a failed rollback")
		return
	}
	if !targetActive {
		writeError(w, 409, "release_target_inactive", "application and environment must be active")
		return
	}
	isTerminalRequest := currentStatus == "SUCCESS" || currentStatus == "FAILED"
	isExistingRequest := requestedOperation == "ROLLBACK" && (currentStatus == "PENDING_REVIEW" || currentStatus == "UNDER_REVIEW" || currentStatus == "APPROVED")
	if !isTerminalRequest && !isExistingRequest {
		writeError(w, 409, "invalid_state", "release is not ready for rollback")
		return
	}
	lockKey, lockKeyErr := releaseTargetLockKey(appID, envID)
	if lockKeyErr != nil {
		writeError(w, 500, "database_error", "release target identifiers are invalid")
		return
	}
	targetBusy, busyErr := lockReleaseTargetForJob(r.Context(), tx, lockKey)
	if busyErr != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not lock the deployment target")
		return
	}
	if targetBusy {
		writeError(w, http.StatusConflict, "job_conflict", "release target already has an active deployment job")
		return
	}
	// Existing approval requests may have originated from an explicit retry
	// of the latest FAILED ROLLBACK job. Keep accepting that exact provenance
	// through approve -> enqueue; the persisted source comparison below still
	// rejects head drift or a newer target attempt.
	plan, planErr := resolveRollbackPlan(r.Context(), tx, id, retry || isExistingRequest)
	if planErr != nil {
		if isExistingRequest && persistedBaseStatus != nil && (*persistedBaseStatus == "SUCCESS" || *persistedBaseStatus == "FAILED") {
			_, resetErr := tx.Exec(r.Context(), `UPDATE releases SET status=$2,requested_operation='DEPLOY',rollback_source_release_id=NULL,rollback_source_job_id=NULL,operation_requested_by=NULL,operation_base_status=NULL,decision_note='Rollback request invalidated because the verified deployment head changed',reviewed_by=NULL,approved_by=NULL,rejected_by=NULL,updated_at=now() WHERE id=$1`, id, *persistedBaseStatus)
			if resetErr == nil {
				resetErr = tx.Commit(r.Context())
			}
			if resetErr == nil {
				s.store.Audit(r.Context(), p.UserID, "release.rollback.invalidate", "release", id, "failure", remoteIP(r), r.UserAgent(), nil)
			}
		}
		writeError(w, 409, "rollback_target_stale", planErr.Error())
		return
	}
	sourceReleaseID := plan.SourceReleaseID
	sourceJobID := plan.SourceJobID
	if isExistingRequest || (retry && requestedOperation == "ROLLBACK" && persistedSourceID != nil) {
		if persistedSourceID == nil || persistedSourceJobID == nil {
			writeError(w, 409, "rollback_source_invalid", "rollback request has no source release")
			return
		}
		if *persistedSourceID != sourceReleaseID || *persistedSourceJobID != sourceJobID {
			writeError(w, 409, "rollback_target_stale", "the verified deployment head changed; recreate the rollback request")
			return
		}
	}
	var targetVersion, profileID, artifactID, path, checksum string
	var runnerLabels []string
	var profileMatches, required bool
	err = tx.QueryRow(r.Context(), `SELECT source.version,source.profile_id::text,art.id::text,art.storage_path,art.sha256,
		(profile.application_id=$2::uuid AND profile.environment_id=$3::uuid),
		(settings.approval_enabled AND (profile.approval_required OR environment.protected OR upper(environment.code)=ANY(regexp_split_to_array(upper(COALESCE(settings.approval_config->>'protectedEnvironments','')), '[[:space:]]*,[[:space:]]*')))),profile.runner_labels
		FROM releases source JOIN deployment_profiles profile ON profile.id=source.profile_id
		JOIN release_jobs basis ON basis.id=$4::uuid AND basis.release_id=source.id AND basis.operation='DEPLOY' AND basis.status='SUCCESS'
		JOIN environments environment ON environment.id=$3::uuid CROSS JOIN app_settings settings
		JOIN LATERAL(SELECT id,storage_path,sha256 FROM release_artifacts WHERE release_id=source.id AND storage_path<>'' ORDER BY created_at DESC,id DESC LIMIT 1) art ON TRUE
		WHERE source.id=$1 AND source.application_id=$2::uuid AND source.environment_id=$3::uuid`, sourceReleaseID, appID, envID, sourceJobID).Scan(&targetVersion, &profileID, &artifactID, &path, &checksum, &profileMatches, &required, &runnerLabels)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 409, "rollback_source_invalid", "rollback source artifact is no longer available")
		return
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not validate rollback source")
		return
	}
	if !profileMatches {
		writeError(w, 409, "release_target_drift", "rollback profile no longer matches the release application and environment")
		return
	}
	dependenciesReady, dependencyErr := deploymentDependenciesReady(r.Context(), tx, profileID, "ROLLBACK")
	if dependencyErr != nil || !dependenciesReady {
		writeError(w, http.StatusConflict, "rollback_profile_not_ready", "an active registry credential and approved ROLLBACK script are required")
		return
	}
	if required && currentStatus != "APPROVED" {
		if isExistingRequest {
			writeError(w, 409, "approval_required", "rollback request is awaiting review and approval")
			return
		}
		tag, updateErr := tx.Exec(r.Context(), `UPDATE releases SET status='PENDING_REVIEW',requested_operation='ROLLBACK',rollback_source_release_id=$2,rollback_source_job_id=$3,operation_requested_by=$4,operation_base_status=COALESCE(operation_base_status,$5),decision_note='',reviewed_by=NULL,approved_by=NULL,rejected_by=NULL,updated_at=now() WHERE id=$1 AND status=$5`, id, sourceReleaseID, sourceJobID, p.UserID, currentStatus)
		if updateErr != nil || tag.RowsAffected() == 0 {
			writeError(w, 409, "rollback_request_conflict", "rollback request could not be submitted for approval")
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, 409, "rollback_request_conflict", "rollback request could not be committed")
			return
		}
		s.store.Audit(r.Context(), p.UserID, "release.rollback.submit_review", "release", id, "success", remoteIP(r), r.UserAgent(), nil)
		s.getReleaseByID(w, r, id, 200)
		return
	}
	runnerReady, runnerErr := matchingRunnerAvailable(r.Context(), tx, runnerLabels)
	if runnerErr != nil {
		writeError(w, 500, "database_error", "could not validate runner availability")
		return
	}
	if !runnerReady {
		writeError(w, http.StatusConflict, "runner_unavailable", "no active online runner matches the rollback profile labels")
		return
	}
	baseStatus := currentStatus
	if persistedBaseStatus != nil {
		baseStatus = *persistedBaseStatus
	}
	if baseStatus != "SUCCESS" && baseStatus != "FAILED" {
		writeError(w, 409, "rollback_request_invalid", "rollback request has no terminal base status")
		return
	}
	targetCredentialID, targetCredentialVersion, credentialErr := loadTargetCredentialSnapshot(r.Context(), tx, profileID)
	if credentialErr != nil {
		writeError(w, http.StatusConflict, "target_credential_not_ready", credentialErr.Error())
		return
	}
	jobID, _ := secure.NewID()
	_, err = tx.Exec(r.Context(), `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,operation,rollback_source_release_id,rollback_source_job_id,target_credential_id,target_credential_version,runner_labels,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'ROLLBACK',$11,$12,$13,$14,$15,$16)`, jobID, id, profileID, app, targetVersion, env, lockKey, artifactID, path, checksum, sourceReleaseID, sourceJobID, targetCredentialID, targetCredentialVersion, runnerLabels, p.UserID)
	if err != nil {
		writeError(w, 409, "job_conflict", "release already has an active job")
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE releases SET status='ROLLBACK',requested_operation='ROLLBACK',rollback_source_release_id=$2,rollback_source_job_id=$3,operation_requested_by=COALESCE(operation_requested_by,$4),operation_base_status=COALESCE(operation_base_status,$5),updated_at=now() WHERE id=$1`, id, sourceReleaseID, sourceJobID, p.UserID, baseStatus); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not queue rollback")
		return
	}
	action := "release.rollback"
	if retry {
		action = "release.rollback.retry"
	}
	s.store.Audit(r.Context(), p.UserID, action, "release", id, "success", remoteIP(r), r.UserAgent(), nil)
	s.getReleaseByID(w, r, id, 200)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	rows, err := s.store.Pool.Query(r.Context(), `SELECT id::text,release_id::text,status,attempts,locked_by,locked_at,started_at,finished_at,failure_message,created_at FROM release_jobs ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		writeError(w, 500, "database_error", "could not list jobs")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, releaseID, status string
		var attempt int
		var claimedBy, failure *string
		var claimedAt, started, finished *time.Time
		var created time.Time
		if rows.Scan(&id, &releaseID, &status, &attempt, &claimedBy, &claimedAt, &started, &finished, &failure, &created) != nil {
			writeError(w, 500, "database_error", "could not list jobs")
			return
		}
		items = append(items, map[string]any{"id": id, "releaseId": releaseID, "status": status, "attempt": attempt, "claimedBy": claimedBy, "claimedAt": claimedAt, "startedAt": started, "finishedAt": finished, "failureMessage": failure, "createdAt": created})
	}
	writeJSON(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
}

func (s *Server) streamReleaseLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "stream_unsupported", "streaming is not supported")
		return
	}
	releaseID := r.PathValue("id")
	var jobStatus string
	err := s.store.Pool.QueryRow(r.Context(), `SELECT status FROM release_jobs WHERE release_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, releaseID).Scan(&jobStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if queryErr := s.store.Pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM releases WHERE id=$1)`, releaseID).Scan(&exists); queryErr != nil || !exists {
			writeError(w, 404, "not_found", "release not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeError(w, 500, "database_error", "could not load release job")
		return
	}
	p, _ := principalFrom(r)
	if !s.acquireLogStream(p.UserID) {
		writeError(w, http.StatusTooManyRequests, "stream_limit", "too many concurrent log streams")
		return
	}
	defer s.releaseLogStream(p.UserID)
	lastID, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if queryID, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64); queryID > lastID {
		lastID = queryID
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	pollTicker := time.NewTicker(time.Second)
	keepaliveTicker := time.NewTicker(15 * time.Second)
	maxDuration := time.NewTimer(30 * time.Minute)
	defer pollTicker.Stop()
	defer keepaliveTicker.Stop()
	defer maxDuration.Stop()
	for {
		rows, err := s.store.Pool.Query(r.Context(), `SELECT l.id,l.stream,l.payload,l.created_at FROM release_job_logs l JOIN release_jobs j ON j.id=l.job_id WHERE j.release_id=$1 AND l.id>$2 ORDER BY l.id LIMIT 500`, releaseID, lastID)
		if err != nil {
			return
		}
		sent := false
		for rows.Next() {
			var id int64
			var stream string
			var payload []byte
			var created time.Time
			if rows.Scan(&id, &stream, &payload, &created) != nil {
				break
			}
			encoded, _ := json.Marshal(map[string]any{"id": id, "stream": stream, "message": string(payload), "createdAt": created})
			fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", id, encoded)
			lastID = id
			sent = true
		}
		rows.Close()
		if sent {
			flusher.Flush()
		}
		terminal := jobStatus == "SUCCESS" || jobStatus == "FAILED" || jobStatus == "ROLLED_BACK"
		_ = s.store.Pool.QueryRow(r.Context(), `SELECT status FROM release_jobs WHERE release_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, releaseID).Scan(&jobStatus)
		terminal = jobStatus == "SUCCESS" || jobStatus == "FAILED" || jobStatus == "ROLLED_BACK"
		if terminal && !sent {
			fmt.Fprint(w, "event: end\ndata: {}\n\n")
			flusher.Flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-maxDuration.C:
			fmt.Fprint(w, "event: end\ndata: {\"reason\":\"max_duration\"}\n\n")
			flusher.Flush()
			return
		case <-keepaliveTicker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-pollTicker.C:
		}
	}
}
