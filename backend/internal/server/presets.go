package server

import (
	"context"
	"errors"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/jackc/pgx/v5"
)

var quickArtifactFilenamePattern = regexp.MustCompile(`^([a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?)-v((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?)\.tar\.gz$`)
var semVersionPattern = regexp.MustCompile(`^((?:0|[1-9][0-9]*))\.((?:0|[1-9][0-9]*))\.((?:0|[1-9][0-9]*))(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

type parsedQuickArtifact struct {
	Filename       string
	ArtifactPrefix string
	Version        string
}

func parseQuickArtifactFilename(filename string) (parsedQuickArtifact, error) {
	if filename == "" || len(filename) > 256 || strings.TrimSpace(filename) != filename {
		return parsedQuickArtifact{}, errors.New("filename must match artifact-prefix-v<semver>.tar.gz")
	}
	matches := quickArtifactFilenamePattern.FindStringSubmatch(filename)
	if len(matches) != 3 || len(matches[2]) > 128 {
		return parsedQuickArtifact{}, errors.New("filename must match artifact-prefix-v<semver>.tar.gz")
	}
	if _, err := parseSemVersion(matches[2]); err != nil {
		return parsedQuickArtifact{}, err
	}
	return parsedQuickArtifact{Filename: filename, ArtifactPrefix: matches[1], Version: matches[2]}, nil
}

type semVersionIdentifier struct {
	value   string
	numeric bool
}

type semVersion struct {
	major      string
	minor      string
	patch      string
	prerelease []semVersionIdentifier
}

func parseSemVersion(value string) (semVersion, error) {
	matches := semVersionPattern.FindStringSubmatch(value)
	if len(matches) != 5 {
		return semVersion{}, errors.New("version must be strict SemVer without build metadata")
	}
	parsed := semVersion{major: matches[1], minor: matches[2], patch: matches[3]}
	if matches[4] == "" {
		return parsed, nil
	}
	for _, value := range strings.Split(matches[4], ".") {
		numeric := true
		for _, character := range value {
			if character < '0' || character > '9' {
				numeric = false
				break
			}
		}
		if numeric && len(value) > 1 && value[0] == '0' {
			return semVersion{}, errors.New("numeric pre-release identifiers cannot contain leading zeroes")
		}
		parsed.prerelease = append(parsed.prerelease, semVersionIdentifier{value: value, numeric: numeric})
	}
	return parsed, nil
}

func compareNumericIdentifier(left, right string) int {
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return strings.Compare(left, right)
}

func compareSemVersions(left, right string) (int, error) {
	leftVersion, err := parseSemVersion(left)
	if err != nil {
		return 0, err
	}
	rightVersion, err := parseSemVersion(right)
	if err != nil {
		return 0, err
	}
	for _, values := range [][2]string{{leftVersion.major, rightVersion.major}, {leftVersion.minor, rightVersion.minor}, {leftVersion.patch, rightVersion.patch}} {
		if comparison := compareNumericIdentifier(values[0], values[1]); comparison != 0 {
			return comparison, nil
		}
	}
	if len(leftVersion.prerelease) == 0 || len(rightVersion.prerelease) == 0 {
		switch {
		case len(leftVersion.prerelease) == 0 && len(rightVersion.prerelease) == 0:
			return 0, nil
		case len(leftVersion.prerelease) == 0:
			return 1, nil
		default:
			return -1, nil
		}
	}
	limit := min(len(leftVersion.prerelease), len(rightVersion.prerelease))
	for index := 0; index < limit; index++ {
		leftIdentifier := leftVersion.prerelease[index]
		rightIdentifier := rightVersion.prerelease[index]
		switch {
		case leftIdentifier.numeric && rightIdentifier.numeric:
			if comparison := compareNumericIdentifier(leftIdentifier.value, rightIdentifier.value); comparison != 0 {
				return comparison, nil
			}
		case leftIdentifier.numeric:
			return -1, nil
		case rightIdentifier.numeric:
			return 1, nil
		default:
			if comparison := strings.Compare(leftIdentifier.value, rightIdentifier.value); comparison != 0 {
				return comparison, nil
			}
		}
	}
	switch {
	case len(leftVersion.prerelease) < len(rightVersion.prerelease):
		return -1, nil
	case len(leftVersion.prerelease) > len(rightVersion.prerelease):
		return 1, nil
	default:
		return 0, nil
	}
}

func validateQuickUpgrade(currentVersion *string, nextVersion string) (string, error) {
	if currentVersion == nil {
		return "", nil
	}
	if _, err := parseSemVersion(*currentVersion); err != nil {
		return "current_version_not_semver", errors.New("current deployed version is not strict SemVer; use Operations mode or the audited recovery flow")
	}
	comparison, err := compareSemVersions(nextVersion, *currentVersion)
	if err != nil {
		return "invalid_artifact_filename", err
	}
	if comparison <= 0 {
		return "quick_upgrade_required", errors.New("Quick Deploy only accepts a version newer than the currently deployed version")
	}
	return "", nil
}

func validArtifactPrefix(prefix string) bool {
	if len(prefix) < 1 || len(prefix) > 64 {
		return false
	}
	return quickArtifactFilenamePattern.MatchString(prefix + "-v0.0.0.tar.gz")
}

type deploymentPresetInput struct {
	Name                    string `json:"name"`
	ArtifactPrefix          string `json:"artifactPrefix"`
	ApplicationID           string `json:"applicationId"`
	EnvironmentID           string `json:"environmentId"`
	DeploymentProfileID     string `json:"deploymentProfileId"`
	Active                  *bool  `json:"active"`
	AutoDeployAfterApproval *bool  `json:"autoDeployAfterApproval"`
}

func validateDeploymentPreset(input deploymentPresetInput) error {
	if strings.TrimSpace(input.Name) == "" || len(strings.TrimSpace(input.Name)) > 200 {
		return errors.New("name is required and must not exceed 200 characters")
	}
	if input.ArtifactPrefix != strings.ToLower(strings.TrimSpace(input.ArtifactPrefix)) || !validArtifactPrefix(input.ArtifactPrefix) {
		return errors.New("artifactPrefix must contain 1 to 64 lowercase letters, digits, or internal hyphens")
	}
	if !validUUID(input.ApplicationID) || !validUUID(input.EnvironmentID) || !validUUID(input.DeploymentProfileID) {
		return errors.New("applicationId, environmentId, and deploymentProfileId must be UUIDs")
	}
	return nil
}

const deploymentPresetSelect = `
	SELECT preset.id::text,preset.name,preset.artifact_prefix,
	       preset.application_id::text,application.code,application.name,
	       preset.environment_id::text,environment.code,environment.name,environment.kind,
	       preset.profile_id::text,profile.name,preset.active,
	       preset.auto_deploy_after_approval,preset.created_at,preset.updated_at
	FROM deployment_presets preset
	JOIN applications application ON application.id=preset.application_id
	JOIN environments environment ON environment.id=preset.environment_id
	JOIN deployment_profiles profile ON profile.id=preset.profile_id`

func scanDeploymentPreset(row rowScanner) (map[string]any, error) {
	var id, name, prefix, applicationID, applicationCode, applicationName string
	var environmentID, environmentCode, environmentName, environmentKind string
	var profileID, profileName string
	var active, autoDeploy bool
	var createdAt, updatedAt time.Time
	if err := row.Scan(&id, &name, &prefix, &applicationID, &applicationCode, &applicationName,
		&environmentID, &environmentCode, &environmentName, &environmentKind,
		&profileID, &profileName, &active, &autoDeploy, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "name": name, "artifactPrefix": prefix,
		"applicationId": applicationID, "applicationCode": applicationCode, "applicationName": applicationName,
		"environmentId": environmentID, "environmentCode": environmentCode, "environmentName": environmentName, "environmentKind": environmentKind,
		"deploymentProfileId": profileID, "deploymentProfileName": profileName,
		"active": active, "autoDeployAfterApproval": autoDeploy,
		"createdAt": createdAt, "updatedAt": updatedAt,
	}, nil
}

func (s *Server) listDeploymentPresets(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	if status != "" && status != "active" && status != "inactive" {
		writeError(w, http.StatusBadRequest, "invalid_filter", "status must be active or inactive")
		return
	}
	var total int
	err := s.store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM deployment_presets preset JOIN applications application ON application.id=preset.application_id WHERE preset.revoked_at IS NULL AND ($1='' OR preset.name ILIKE '%'||$1||'%' OR preset.artifact_prefix ILIKE '%'||$1||'%' OR application.name ILIKE '%'||$1||'%') AND ($2='' OR ($2='active' AND preset.active) OR ($2='inactive' AND NOT preset.active))`, search, status).Scan(&total)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not list deployment presets")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), deploymentPresetSelect+` WHERE preset.revoked_at IS NULL AND ($1='' OR preset.name ILIKE '%'||$1||'%' OR preset.artifact_prefix ILIKE '%'||$1||'%' OR application.name ILIKE '%'||$1||'%') AND ($2='' OR ($2='active' AND preset.active) OR ($2='inactive' AND NOT preset.active)) ORDER BY preset.name,preset.id LIMIT $3 OFFSET $4`, search, status, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not list deployment presets")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		item, scanErr := scanDeploymentPreset(rows)
		if scanErr != nil {
			writeError(w, http.StatusInternalServerError, "database_error", "could not list deployment presets")
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not list deployment presets")
		return
	}
	writeJSON(w, http.StatusOK, page(items, total, limit, offset))
}

func (s *Server) createDeploymentPreset(w http.ResponseWriter, r *http.Request) {
	var input deploymentPresetInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := validateDeploymentPreset(input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_deployment_preset", err.Error())
		return
	}
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	autoDeploy := true
	if input.AutoDeployAfterApproval != nil {
		autoDeploy = *input.AutoDeployAfterApproval
	}
	id, _ := secure.NewID()
	principal, _ := principalFrom(r)
	_, err := s.store.Pool.Exec(r.Context(), `INSERT INTO deployment_presets(id,name,artifact_prefix,application_id,environment_id,profile_id,active,auto_deploy_after_approval,created_by,updated_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`, id, strings.TrimSpace(input.Name), input.ArtifactPrefix, input.ApplicationID, input.EnvironmentID, input.DeploymentProfileID, active, autoDeploy, principal.UserID)
	if err != nil {
		writeError(w, http.StatusConflict, "deployment_preset_conflict", "artifact prefix already exists or the application, environment, and profile binding is invalid")
		return
	}
	s.store.Audit(r.Context(), principal.UserID, "deployment_preset.create", "deployment_preset", id, "success", remoteIP(r), r.UserAgent(), nil)
	s.getDeploymentPresetByID(w, r, id, http.StatusCreated)
}

func (s *Server) getDeploymentPreset(w http.ResponseWriter, r *http.Request) {
	s.getDeploymentPresetByID(w, r, r.PathValue("id"), http.StatusOK)
}

func (s *Server) getDeploymentPresetByID(w http.ResponseWriter, r *http.Request, id string, status int) {
	item, err := scanDeploymentPreset(s.store.Pool.QueryRow(r.Context(), deploymentPresetSelect+` WHERE preset.id=$1 AND preset.revoked_at IS NULL`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "deployment preset not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load deployment preset")
		return
	}
	writeJSON(w, status, item)
}

func (s *Server) updateDeploymentPreset(w http.ResponseWriter, r *http.Request) {
	var input deploymentPresetInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := validateDeploymentPreset(input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_deployment_preset", err.Error())
		return
	}
	id := r.PathValue("id")
	principal, _ := principalFrom(r)
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE deployment_presets SET name=$2,artifact_prefix=$3,application_id=$4,environment_id=$5,profile_id=$6,active=COALESCE($7,active),auto_deploy_after_approval=COALESCE($8,auto_deploy_after_approval),updated_by=$9,updated_at=now() WHERE id=$1 AND revoked_at IS NULL`, id, strings.TrimSpace(input.Name), input.ArtifactPrefix, input.ApplicationID, input.EnvironmentID, input.DeploymentProfileID, input.Active, input.AutoDeployAfterApproval, principal.UserID)
	if err != nil {
		writeError(w, http.StatusConflict, "deployment_preset_conflict", "artifact prefix already exists or the application, environment, and profile binding is invalid")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "deployment preset not found")
		return
	}
	s.store.Audit(r.Context(), principal.UserID, "deployment_preset.update", "deployment_preset", id, "success", remoteIP(r), r.UserAgent(), nil)
	s.getDeploymentPresetByID(w, r, id, http.StatusOK)
}

func (s *Server) deleteDeploymentPreset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	principal, _ := principalFrom(r)
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE deployment_presets SET active=FALSE,revoked_at=now(),updated_by=$2,updated_at=now() WHERE id=$1 AND revoked_at IS NULL`, id, principal.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not revoke deployment preset")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "deployment preset not found")
		return
	}
	s.store.Audit(r.Context(), principal.UserID, "deployment_preset.revoke", "deployment_preset", id, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(http.StatusNoContent)
}

type resolvedDeploymentPreset struct {
	ID                      string
	Name                    string
	ArtifactPrefix          string
	ApplicationID           string
	ApplicationCode         string
	ApplicationName         string
	EnvironmentID           string
	EnvironmentCode         string
	EnvironmentName         string
	EnvironmentKind         string
	ProfileID               string
	ProfileName             string
	AutoDeployAfterApproval bool
	ApprovalRequired        bool
	RunnerLabels            []string
	CurrentVersion          *string
	UpdatedAt               time.Time
}

func (s *Server) resolveDeploymentPreset(r *http.Request, parsed parsedQuickArtifact) (resolvedDeploymentPreset, error) {
	var resolved resolvedDeploymentPreset
	err := s.store.Pool.QueryRow(r.Context(), `SELECT preset.id::text,preset.name,preset.artifact_prefix,
		application.id::text,application.code,application.name,
		environment.id::text,environment.code,environment.name,environment.kind,
		profile.id::text,profile.name,preset.auto_deploy_after_approval,
		settings.approval_enabled AND (profile.approval_required OR environment.protected OR upper(environment.code)=ANY(regexp_split_to_array(upper(COALESCE(settings.approval_config->>'protectedEnvironments','')), '[[:space:]]*,[[:space:]]*'))),
		profile.runner_labels,current_release.version,preset.updated_at
		FROM deployment_presets preset
		JOIN applications application ON application.id=preset.application_id
		JOIN environments environment ON environment.id=preset.environment_id AND environment.application_id=application.id
		JOIN deployment_profiles profile ON profile.id=preset.profile_id AND profile.application_id=application.id AND profile.environment_id=environment.id
		CROSS JOIN app_settings settings
		LEFT JOIN deployment_heads head ON head.application_id=application.id AND head.environment_id=environment.id
		LEFT JOIN releases current_release ON current_release.id=head.current_release_id
		WHERE preset.artifact_prefix=$1 AND preset.active AND preset.revoked_at IS NULL
		  AND application.active AND environment.active AND profile.active AND profile.enabled AND profile.revoked_at IS NULL`, parsed.ArtifactPrefix).Scan(
		&resolved.ID, &resolved.Name, &resolved.ArtifactPrefix,
		&resolved.ApplicationID, &resolved.ApplicationCode, &resolved.ApplicationName,
		&resolved.EnvironmentID, &resolved.EnvironmentCode, &resolved.EnvironmentName, &resolved.EnvironmentKind,
		&resolved.ProfileID, &resolved.ProfileName, &resolved.AutoDeployAfterApproval,
		&resolved.ApprovalRequired, &resolved.RunnerLabels, &resolved.CurrentVersion, &resolved.UpdatedAt)
	return resolved, err
}

func (s *Server) preflightQuickRelease(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Filename string `json:"filename"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	parsed, err := parseQuickArtifactFilename(input.Filename)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_artifact_filename", err.Error())
		return
	}
	resolved, err := s.resolveDeploymentPreset(r, parsed)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "deployment_preset_not_found", "no active deployment preset matches the artifact prefix")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not resolve deployment preset")
		return
	}
	var versionExists bool
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM releases WHERE application_id=$1 AND environment_id=$2 AND version=$3)`, resolved.ApplicationID, resolved.EnvironmentID, parsed.Version).Scan(&versionExists); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not validate release version")
		return
	}
	if versionExists {
		writeError(w, http.StatusConflict, "release_version_exists", "this version already exists for the resolved service and environment")
		return
	}
	if code, err := validateQuickUpgrade(resolved.CurrentVersion, parsed.Version); err != nil {
		writeError(w, http.StatusConflict, code, err.Error())
		return
	}
	profileReady, profileErr := deploymentDependenciesReady(r.Context(), s.store.Pool, resolved.ProfileID, "DEPLOY")
	runnerAvailable, runnerErr := matchingRunnerAvailable(r.Context(), s.store.Pool, resolved.RunnerLabels)
	if profileErr != nil || runnerErr != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not validate deployment readiness")
		return
	}
	nextAction := "DEPLOY"
	if resolved.ApprovalRequired {
		nextAction = "APPROVAL"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"filename": parsed.Filename, "artifactPrefix": parsed.ArtifactPrefix, "version": parsed.Version,
		"currentVersion": resolved.CurrentVersion, "approvalRequired": resolved.ApprovalRequired, "nextAction": nextAction,
		"preset":            map[string]any{"id": resolved.ID, "name": resolved.Name, "autoDeployAfterApproval": resolved.AutoDeployAfterApproval, "updatedAt": resolved.UpdatedAt},
		"application":       map[string]any{"id": resolved.ApplicationID, "code": resolved.ApplicationCode, "name": resolved.ApplicationName},
		"environment":       map[string]any{"id": resolved.EnvironmentID, "code": resolved.EnvironmentCode, "name": resolved.EnvironmentName, "kind": resolved.EnvironmentKind},
		"deploymentProfile": map[string]any{"id": resolved.ProfileID, "name": resolved.ProfileName},
		"readiness":         map[string]any{"profileReady": profileReady, "runnerAvailable": runnerAvailable},
	})
}

func rawMultipartFilename(header *multipart.FileHeader) (string, error) {
	disposition := header.Header.Get("Content-Disposition")
	mediaType, parameters, err := mime.ParseMediaType(disposition)
	if err != nil || !strings.EqualFold(mediaType, "form-data") || parameters["name"] != "artifact" {
		return "", errors.New("artifact content disposition is invalid")
	}
	filename := parameters["filename"]
	if filename == "" || filename != header.Filename || filename != filepath.Base(filename) || strings.ContainsAny(filename, "/\\\x00\r\n") {
		return "", errors.New("artifact filename must not contain a path")
	}
	return filename, nil
}

type quickQueueError struct {
	code    string
	message string
}

func (e *quickQueueError) Error() string { return e.message }

func quickQueueFailure(code, message string) error {
	return &quickQueueError{code: code, message: message}
}

func (s *Server) queueQuickDeployTx(ctx context.Context, tx pgx.Tx, releaseID, expectedStatus string) (string, error) {
	var status, application, version, environment, applicationID, environmentID, profileID, requesterID string
	var artifactID, artifactPath, checksum string
	var frozenHeadReleaseID, frozenHeadJobID *string
	var runnerLabels []string
	var profileMatches, targetActive, quickRelease bool
	err := tx.QueryRow(ctx, `SELECT release.status,application.code,release.version,environment.code,
		release.application_id::text,release.environment_id::text,release.profile_id::text,release.created_by,
		(profile.application_id=release.application_id AND profile.environment_id=release.environment_id),
		(application.active AND environment.active AND profile.active AND profile.enabled AND profile.revoked_at IS NULL),
		release.quick_release,release.quick_base_release_id::text,release.quick_base_job_id::text,
		COALESCE(artifact.id::text,''),COALESCE(artifact.storage_path,''),COALESCE(artifact.sha256,''),profile.runner_labels
		FROM releases release
		JOIN applications application ON application.id=release.application_id
		JOIN environments environment ON environment.id=release.environment_id
		JOIN deployment_profiles profile ON profile.id=release.profile_id
		LEFT JOIN LATERAL(SELECT id,storage_path,sha256 FROM release_artifacts WHERE release_id=release.id AND storage_path<>'' ORDER BY created_at DESC,id DESC LIMIT 1) artifact ON TRUE
		WHERE release.id=$1 FOR UPDATE OF release`, releaseID).Scan(
		&status, &application, &version, &environment, &applicationID, &environmentID, &profileID, &requesterID,
		&profileMatches, &targetActive, &quickRelease, &frozenHeadReleaseID, &frozenHeadJobID, &artifactID, &artifactPath, &checksum, &runnerLabels)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", quickQueueFailure("not_found", "quick release not found")
	}
	if err != nil {
		return "", err
	}
	if !quickRelease || status != expectedStatus {
		return "", quickQueueFailure("invalid_state", "quick release is not in the expected state")
	}
	if artifactPath == "" {
		return "", quickQueueFailure("artifact_content_required", "uploaded artifact content is required before deployment")
	}
	if !profileMatches {
		return "", quickQueueFailure("release_target_drift", "deployment profile no longer matches the release target")
	}
	if !targetActive {
		return "", quickQueueFailure("release_target_inactive", "release target is inactive")
	}
	dependenciesReady, dependencyErr := deploymentDependenciesReady(ctx, tx, profileID, "DEPLOY")
	if dependencyErr != nil {
		return "", dependencyErr
	}
	if !dependenciesReady {
		return "", quickQueueFailure("deployment_profile_not_ready", "an active registry credential and approved DEPLOY script are required")
	}
	runnerReady, runnerErr := matchingRunnerAvailable(ctx, tx, runnerLabels)
	if runnerErr != nil {
		return "", runnerErr
	}
	if !runnerReady {
		return "", quickQueueFailure("runner_unavailable", "no active online runner matches the deployment profile labels")
	}
	lockKey, err := releaseTargetLockKey(applicationID, environmentID)
	if err != nil {
		return "", err
	}
	targetBusy, busyErr := lockReleaseTargetForJob(ctx, tx, lockKey)
	if busyErr != nil {
		return "", busyErr
	}
	if targetBusy {
		return "", quickQueueFailure("job_conflict", "release target already has an active deployment job")
	}
	targetCredentialID, targetCredentialVersion, err := loadTargetCredentialSnapshot(ctx, tx, profileID)
	if err != nil {
		return "", quickQueueFailure("target_credential_not_ready", err.Error())
	}
	if err := validateAndLockQuickHead(ctx, tx, applicationID, environmentID, frozenHeadReleaseID, frozenHeadJobID); err != nil {
		return "", err
	}
	jobID, _ := secure.NewID()
	_, err = tx.Exec(ctx, `INSERT INTO release_jobs(id,release_id,profile_id,application,version,environment,lock_key,artifact_id,artifact_path,expected_sha256,rollback_source_release_id,rollback_source_job_id,target_credential_id,target_credential_version,runner_labels,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, jobID, releaseID, profileID, application, version, environment, lockKey, artifactID, artifactPath, checksum, frozenHeadReleaseID, frozenHeadJobID, targetCredentialID, targetCredentialVersion, runnerLabels, requesterID)
	if err != nil {
		return "", quickQueueFailure("job_conflict", "release target already has an active deployment job")
	}
	// release_job_status_sync atomically mirrors the inserted QUEUED job onto
	// the release. The release row is already locked and its expected state was
	// checked above, so a second status update here would race that trigger.
	return jobID, nil
}

func sameNullableString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func validateAndLockQuickHead(ctx context.Context, tx pgx.Tx, applicationID, environmentID string, frozenReleaseID, frozenJobID *string) error {
	var currentHeadReleaseID, currentHeadJobID *string
	var currentReleaseID, currentJobID string
	err := tx.QueryRow(ctx, `SELECT head.current_release_id::text,head.current_job_id::text
		FROM deployment_heads head
		JOIN releases current_release ON current_release.id=head.current_release_id
		JOIN release_jobs current_job ON current_job.id=head.current_job_id
		WHERE head.application_id=$1 AND head.environment_id=$2
		FOR SHARE OF head,current_release,current_job`, applicationID, environmentID).Scan(&currentReleaseID, &currentJobID)
	if errors.Is(err, pgx.ErrNoRows) {
		currentHeadReleaseID, currentHeadJobID = nil, nil
	} else if err != nil {
		return err
	} else {
		currentHeadReleaseID, currentHeadJobID = &currentReleaseID, &currentJobID
	}
	if !sameNullableString(frozenReleaseID, currentHeadReleaseID) || !sameNullableString(frozenJobID, currentHeadJobID) {
		return quickQueueFailure("deployment_head_changed", "the deployed version changed after preflight; run preflight again")
	}
	return nil
}

func writeQuickQueueError(w http.ResponseWriter, err error, fallbackCode, fallbackMessage string) {
	var queueErr *quickQueueError
	if errors.As(err, &queueErr) {
		status := http.StatusConflict
		if queueErr.code == "not_found" {
			status = http.StatusNotFound
		}
		writeError(w, status, queueErr.code, queueErr.message)
		return
	}
	writeError(w, http.StatusInternalServerError, fallbackCode, fallbackMessage)
}

func (s *Server) quickRelease(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r)
	if !principal.Has("releases.submit") {
		writeError(w, http.StatusForbidden, "permission_denied", "releases.submit permission is required for quick deployment")
		return
	}
	cfg, err := s.loadAppSettings(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load artifact settings")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, cfg.ArtifactMaxBytes+(2<<20))
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "artifact_too_large", "multipart request exceeds the configured artifact limit")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_upload", "invalid multipart request")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll() //nolint:errcheck
	}
	notes := r.FormValue("notes")
	if len(notes) > 16<<10 {
		writeError(w, http.StatusBadRequest, "invalid_release", "notes must not exceed 16 KiB")
		return
	}
	expectedPresetIDs, presetIDProvided := r.MultipartForm.Value["expectedPresetId"]
	expectedPresetUpdates, presetUpdateProvided := r.MultipartForm.Value["expectedPresetUpdatedAt"]
	expectedCurrentVersions, currentVersionProvided := r.MultipartForm.Value["expectedCurrentVersion"]
	if !presetIDProvided || len(expectedPresetIDs) != 1 || !validUUID(expectedPresetIDs[0]) ||
		!presetUpdateProvided || len(expectedPresetUpdates) != 1 ||
		!currentVersionProvided || len(expectedCurrentVersions) != 1 {
		writeError(w, http.StatusBadRequest, "preflight_snapshot_required", "expectedPresetId, expectedPresetUpdatedAt, and expectedCurrentVersion must come from preflight")
		return
	}
	expectedPresetID := expectedPresetIDs[0]
	expectedPresetUpdatedAt, parseErr := time.Parse(time.RFC3339Nano, expectedPresetUpdates[0])
	if parseErr != nil {
		writeError(w, http.StatusBadRequest, "preflight_snapshot_required", "expectedPresetUpdatedAt must be an RFC 3339 timestamp from preflight")
		return
	}
	expectedCurrentVersion := expectedCurrentVersions[0]
	file, header, err := r.FormFile("artifact")
	if err != nil {
		writeError(w, http.StatusBadRequest, "artifact_required", "multipart field artifact is required")
		return
	}
	defer file.Close()
	filename, err := rawMultipartFilename(header)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_artifact_filename", err.Error())
		return
	}
	parsed, err := parseQuickArtifactFilename(filename)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_artifact_filename", err.Error())
		return
	}

	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not start quick release")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	var resolved resolvedDeploymentPreset
	err = tx.QueryRow(r.Context(), `SELECT preset.id::text,preset.name,preset.artifact_prefix,
		application.id::text,application.code,application.name,
		environment.id::text,environment.code,environment.name,environment.kind,
		profile.id::text,profile.name,preset.auto_deploy_after_approval,
		settings.approval_enabled AND (profile.approval_required OR environment.protected OR upper(environment.code)=ANY(regexp_split_to_array(upper(COALESCE(settings.approval_config->>'protectedEnvironments','')), '[[:space:]]*,[[:space:]]*'))),
		profile.runner_labels,NULL::text,preset.updated_at
		FROM deployment_presets preset
		JOIN applications application ON application.id=preset.application_id
		JOIN environments environment ON environment.id=preset.environment_id AND environment.application_id=application.id
		JOIN deployment_profiles profile ON profile.id=preset.profile_id AND profile.application_id=application.id AND profile.environment_id=environment.id
		CROSS JOIN app_settings settings
		WHERE preset.artifact_prefix=$1 AND preset.id=$2 AND preset.updated_at=$3
		  AND preset.active AND preset.revoked_at IS NULL
		  AND application.active AND environment.active AND profile.active AND profile.enabled AND profile.revoked_at IS NULL
		FOR SHARE OF preset,application,environment,profile,settings`, parsed.ArtifactPrefix, expectedPresetID, expectedPresetUpdatedAt).Scan(
		&resolved.ID, &resolved.Name, &resolved.ArtifactPrefix,
		&resolved.ApplicationID, &resolved.ApplicationCode, &resolved.ApplicationName,
		&resolved.EnvironmentID, &resolved.EnvironmentCode, &resolved.EnvironmentName, &resolved.EnvironmentKind,
		&resolved.ProfileID, &resolved.ProfileName, &resolved.AutoDeployAfterApproval,
		&resolved.ApprovalRequired, &resolved.RunnerLabels, &resolved.CurrentVersion, &resolved.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "deployment_preset_unavailable", "no active deployment preset matches the artifact prefix")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not lock deployment preset")
		return
	}
	var versionExists bool
	if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM releases WHERE application_id=$1 AND environment_id=$2 AND version=$3)`, resolved.ApplicationID, resolved.EnvironmentID, parsed.Version).Scan(&versionExists); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not validate release version")
		return
	}
	if versionExists {
		writeError(w, http.StatusConflict, "release_conflict", "this version already exists for the resolved service and environment")
		return
	}
	lockKey, lockKeyErr := releaseTargetLockKey(resolved.ApplicationID, resolved.EnvironmentID)
	if lockKeyErr != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "resolved deployment target is invalid")
		return
	}
	targetBusy, busyErr := activeTargetJobExists(r.Context(), tx, lockKey)
	if busyErr != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not validate the deployment target lock")
		return
	}
	if targetBusy {
		writeError(w, http.StatusConflict, "job_conflict", "release target already has an active deployment job")
		return
	}
	var quickBaseReleaseID, quickBaseJobID *string
	var currentReleaseID, currentJobID, currentVersion string
	headErr := tx.QueryRow(r.Context(), `SELECT head.current_release_id::text,head.current_job_id::text,current_release.version
		FROM deployment_heads head
		JOIN releases current_release ON current_release.id=head.current_release_id
		JOIN release_jobs current_job ON current_job.id=head.current_job_id
		WHERE head.application_id=$1 AND head.environment_id=$2`, resolved.ApplicationID, resolved.EnvironmentID).Scan(&currentReleaseID, &currentJobID, &currentVersion)
	if errors.Is(headErr, pgx.ErrNoRows) {
		if expectedCurrentVersion != "" {
			writeError(w, http.StatusConflict, "deployment_head_changed", "the deployed version changed after preflight; run preflight again")
			return
		}
	} else if headErr != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not lock the current deployment head")
		return
	} else {
		if currentVersion != expectedCurrentVersion {
			writeError(w, http.StatusConflict, "deployment_head_changed", "the deployed version changed after preflight; run preflight again")
			return
		}
		quickBaseReleaseID, quickBaseJobID = &currentReleaseID, &currentJobID
		resolved.CurrentVersion = &currentVersion
	}
	if code, err := validateQuickUpgrade(resolved.CurrentVersion, parsed.Version); err != nil {
		writeError(w, http.StatusConflict, code, err.Error())
		return
	}
	releaseID, _ := secure.NewID()
	_, err = tx.Exec(r.Context(), `INSERT INTO releases(id,application_id,environment_id,profile_id,version,notes,created_by,deployment_preset_id,quick_release,auto_deploy_after_approval,quick_base_release_id,quick_base_job_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,TRUE,$9,$10,$11)`, releaseID, resolved.ApplicationID, resolved.EnvironmentID, resolved.ProfileID, parsed.Version, notes, principal.UserID, resolved.ID, resolved.AutoDeployAfterApproval, quickBaseReleaseID, quickBaseJobID)
	if err != nil {
		writeError(w, http.StatusConflict, "release_conflict", "this version already exists for the resolved service and environment")
		return
	}
	artifact, err := s.persistArtifactTx(r, tx, releaseID, filename, header.Header.Get("Content-Type"), file, cfg, true)
	if err != nil {
		if strings.Contains(err.Error(), "exceeds the configured limit") {
			writeError(w, http.StatusRequestEntityTooLarge, "artifact_too_large", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_artifact", err.Error())
		return
	}
	committed := false
	defer func() {
		if !committed {
			artifact.cleanup()
		}
	}()
	dependenciesReady, dependencyErr := deploymentDependenciesReady(r.Context(), tx, resolved.ProfileID, "DEPLOY")
	if dependencyErr != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not validate deployment profile")
		return
	}
	if !dependenciesReady {
		writeError(w, http.StatusConflict, "deployment_profile_not_ready", "an active registry credential and approved DEPLOY script are required")
		return
	}
	auditAction := "release.enqueue"
	if resolved.ApprovalRequired {
		targetBusy, busyErr := lockReleaseTargetForJob(r.Context(), tx, lockKey)
		if busyErr != nil {
			writeError(w, http.StatusInternalServerError, "database_error", "could not lock the deployment target")
			return
		}
		if targetBusy {
			writeError(w, http.StatusConflict, "job_conflict", "release target already has an active deployment job")
			return
		}
		if err := validateAndLockQuickHead(r.Context(), tx, resolved.ApplicationID, resolved.EnvironmentID, quickBaseReleaseID, quickBaseJobID); err != nil {
			writeQuickQueueError(w, err, "database_error", "could not validate the deployment head")
			return
		}
		tag, updateErr := tx.Exec(r.Context(), `UPDATE releases SET status='PENDING_REVIEW',decision_note='',reviewed_by=NULL,approved_by=NULL,rejected_by=NULL,updated_at=now() WHERE id=$1 AND status='UPLOADED'`, releaseID)
		if updateErr != nil || tag.RowsAffected() == 0 {
			writeError(w, http.StatusConflict, "quick_release_conflict", "quick release could not be submitted for review")
			return
		}
		auditAction = "release.submit_review"
	} else if _, err := s.queueQuickDeployTx(r.Context(), tx, releaseID, "UPLOADED"); err != nil {
		writeQuickQueueError(w, err, "database_error", "quick release could not be queued")
		return
	}
	if commitErr := tx.Commit(r.Context()); commitErr != nil {
		visible, verifyErr := s.artifactCommitVisible(artifact.id)
		if visible {
			committed = true
		} else if commitDefinitelyRolledBack(commitErr) {
			writeError(w, http.StatusConflict, "quick_release_conflict", "quick release changed concurrently; retry the upload")
			return
		} else {
			// An indeterminate COMMIT must never delete an artifact that a
			// committed QUEUED job may already reference. Preserve it for
			// reconciliation and surface the uncertainty to the operator.
			committed = true
			s.log.Warn("quick release commit result could not be reconciled; artifact preserved", "release_id", releaseID, "artifact_id", artifact.id, "commit_error", commitErr, "verify_error", verifyErr)
			writeError(w, http.StatusInternalServerError, "commit_result_unknown", "quick release commit result is unknown; artifact was preserved for recovery")
			return
		}
	} else {
		committed = true
	}
	s.store.Audit(r.Context(), principal.UserID, "release.quick.create", "release", releaseID, "success", remoteIP(r), r.UserAgent(), nil)
	s.store.Audit(r.Context(), principal.UserID, "artifact.upload", "artifact", artifact.id, "success", remoteIP(r), r.UserAgent(), nil)
	s.store.Audit(r.Context(), principal.UserID, auditAction, "release", releaseID, "success", remoteIP(r), r.UserAgent(), nil)
	s.getReleaseByID(w, r, releaseID, http.StatusCreated)
}
