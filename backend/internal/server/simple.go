package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hkjang/releasedock/backend/internal/localexec"
	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/jackc/pgx/v5"
)

// maxSimpleRunLogBytes caps what one run may append. The command keeps running
// past the cap; only its output stops being stored.
const maxSimpleRunLogBytes = 8 << 20

// maxSimpleRunSystemLogBytes is a separate reserve for the server's own
// progress lines. They are few and short, and they are the only record in the
// log of what happened after the command ended - the "exit=" line and the
// replication, app deployment and post-deploy notices - so a chatty command
// that fills the shared cap must not silence them too.
const maxSimpleRunSystemLogBytes = 64 << 10

// streamSystem marks a log row written by the server rather than by the
// command.
const streamSystem = "system"

// maxSimpleConcurrentRuns bounds how many commands the API process runs at
// once, independent of the one-run-per-target database constraint.
const maxSimpleConcurrentRuns = 4

func (s *Server) acquireSimpleRun() bool {
	s.simpleMu.Lock()
	defer s.simpleMu.Unlock()
	if s.simpleActive >= maxSimpleConcurrentRuns {
		return false
	}
	s.simpleActive++
	return true
}

func (s *Server) releaseSimpleRun() {
	s.simpleMu.Lock()
	defer s.simpleMu.Unlock()
	if s.simpleActive > 0 {
		s.simpleActive--
	}
}

// RecoverSimpleRuns closes out runs that were in flight when the process
// stopped. Their child processes died with the process group, so leaving the
// rows RUNNING would block the target forever.
func (s *Server) RecoverSimpleRuns(ctx context.Context) {
	tag, err := s.store.Pool.Exec(ctx, `UPDATE simple_runs
		SET status='FAILED',error='서버가 재시작되어 실행 결과를 확인할 수 없습니다',finished_at=now()
		WHERE status IN ('PENDING','RUNNING')`)
	if err != nil {
		s.log.Warn("could not recover interrupted simple runs", "error", err)
		return
	}
	if tag.RowsAffected() > 0 {
		s.log.Warn("closed interrupted simple runs", "count", tag.RowsAffected())
	}
}

// uiMode tells the browser which shell to render. Kept separate from /me so
// the Principal contract every other screen depends on stays unchanged.
func (s *Server) uiMode(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	cfg, err := loadSimpleSettings(r.Context(), s.store.Pool)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load simple settings")
		return
	}
	var preferences struct {
		UIMode string `json:"uiMode"`
	}
	var raw json.RawMessage
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT preferences FROM users WHERE id=$1`, p.UserID).Scan(&raw); err == nil {
		_ = json.Unmarshal(raw, &preferences)
	}

	canUseSimple := p.Has("simple.deploy") || p.Has("simple.read")
	canUseFull := p.Has("releases.read")
	effective := cfg.DefaultUIMode
	if preferences.UIMode == "simple" || preferences.UIMode == "full" {
		effective = preferences.UIMode
	}
	// Never strand a user in a mode they have no permission for.
	if effective == "simple" && !canUseSimple {
		effective = "full"
	}
	if effective == "full" && !canUseFull && canUseSimple {
		effective = "simple"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"defaultUiMode":   cfg.DefaultUIMode,
		"preferredUiMode": preferences.UIMode,
		"effectiveUiMode": effective,
		"canUseSimple":    canUseSimple,
		"canUseFull":      canUseFull,
		"commandMode":     cfg.CommandMode,
	})
}

// listUserSimpleTargets returns what the simple-mode screen needs in one call:
// the targets a user may deploy to, and whether the active command mode leaves
// each one runnable.
func (s *Server) listUserSimpleTargets(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadSimpleSettings(r.Context(), s.store.Pool)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load simple settings")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT `+simpleTargetColumns+` FROM simple_targets
		WHERE revoked_at IS NULL AND active ORDER BY name`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not list simple targets")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		target, err := scanSimpleTarget(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database_error", "could not list simple targets")
			return
		}
		ready := true
		reason := ""
		if _, err := resolveCommand(cfg, target); err != nil {
			ready = false
			reason = err.Error()
		}
		items = append(items, map[string]any{
			"id": target.ID, "name": target.Name, "description": target.Description,
			"uploadDir": target.UploadDir, "maxUploadBytes": target.MaxUploadBytes,
			"ready": ready, "notReadyReason": reason,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "commandMode": cfg.CommandMode})
}

// uploadBatch identifies the upload a run belongs to. Several packages dropped
// at once become one run per file, and the stages that must happen only once
// for the whole upload ride on the run the client marked as its last.
type uploadBatch struct {
	ID   string
	Last bool
}

// readUploadBatch reads the optional batch fields from the multipart form. A
// request that says nothing is one package on its own, so it is its own last
// run and the once-per-upload stages fire for it as they always have.
func readUploadBatch(r *http.Request) uploadBatch {
	batch := uploadBatch{Last: true}
	// The identifier is only ever echoed back and grouped on, but keeping it to
	// an opaque token means nothing arbitrary is stored on the run. A rejected
	// identifier costs the grouping, never the ordering: the marker that says
	// which run carries the once-per-upload stages is read regardless.
	id := strings.TrimSpace(r.FormValue("batchId"))
	if len(id) <= 64 && strings.IndexFunc(id, func(char rune) bool {
		return !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') && char != '-' && char != '_'
	}) < 0 {
		batch.ID = id
	}
	if raw := strings.TrimSpace(r.FormValue("batchLast")); raw != "" {
		if last, err := strconv.ParseBool(raw); err == nil {
			batch.Last = last
		}
	}
	return batch
}

// createSimpleRun stores the uploaded package in the target's directory and
// starts the configured command. Nothing else happens on this path: no image
// load, tag, registry push, approval, or version check.
func (s *Server) createSimpleRun(w http.ResponseWriter, r *http.Request) {
	targetID := r.PathValue("id")
	cfg, err := loadSimpleSettings(r.Context(), s.store.Pool)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load simple settings")
		return
	}
	// The target may be omitted entirely. With a single active target there is
	// nothing to choose, so a user can just drop files and deploy.
	target, err := s.resolveSimpleTarget(r, targetID)
	if errors.Is(err, errSimpleTargetAmbiguous) {
		writeError(w, http.StatusBadRequest, "target_required", "배포 대상이 여러 개이므로 대상을 선택해야 합니다")
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "배포 대상을 찾을 수 없습니다")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load simple target")
		return
	}

	// Resolve the command from the settings as they stand right now, not from
	// whatever the browser last rendered.
	command, err := resolveCommand(cfg, target)
	if err != nil {
		writeError(w, http.StatusConflict, "command_not_configured", err.Error())
		return
	}
	if err := validateCommandFields(command.Path, command.Args, command.Dir, int(command.Timeout/time.Second)); err != nil {
		writeError(w, http.StatusConflict, "command_not_executable", err.Error())
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, target.MaxUploadBytes+(2<<20))
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_upload", "패키지 업로드를 읽을 수 없습니다")
		return
	}
	defer r.MultipartForm.RemoveAll() //nolint:errcheck
	file, header, err := r.FormFile("artifact")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_upload", "artifact 파일이 필요합니다")
		return
	}
	defer file.Close() //nolint:errcheck

	filename := filepath.Base(strings.TrimSpace(header.Filename))
	if !safeArtifactName(filename) {
		writeError(w, http.StatusBadRequest, "invalid_filename", "파일 이름은 경로 없이 .tar 또는 .tar.gz로 끝나야 합니다")
		return
	}

	batch := readUploadBatch(r)

	p, _ := principalFrom(r)
	if !s.acquireSimpleRun() {
		writeError(w, http.StatusTooManyRequests, "simple_run_limit", "동시에 실행할 수 있는 작업 수를 초과했습니다")
		return
	}
	started := false
	defer func() {
		if !started {
			s.releaseSimpleRun()
		}
	}()

	staged, err := s.stageSimpleArtifact(target.UploadDir, filename, file, target.MaxUploadBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "upload_failed", err.Error())
		return
	}
	published := false
	defer func() {
		if !published {
			staged.discard()
		}
	}()

	runID, err := secure.NewID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "id_error", "could not allocate an identifier")
		return
	}
	args := expandArgs(command.Args, staged.path)
	_, err = s.store.Pool.Exec(r.Context(), `INSERT INTO simple_runs
		(id,target_id,actor_id,original_filename,stored_path,size_bytes,sha256,
		 command_source,resolved_command_path,resolved_command_args,resolved_timeout_seconds,
		 batch_id,batch_last,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'PENDING')`,
		runID, target.ID, p.UserID, filename, staged.path, staged.size, staged.checksum,
		command.Source, command.Path, args, int(command.Timeout/time.Second),
		batch.ID, batch.Last)
	if err != nil {
		// The partial unique index rejects a second in-flight run per target.
		writeError(w, http.StatusConflict, "simple_run_active", "이 대상에서 이미 실행 중인 작업이 있습니다")
		return
	}

	// The run now holds the target, so no other upload can be accepted for it
	// and the package may finally take its own name.
	if err := staged.commit(); err != nil {
		s.failSimpleRun(r.Context(), runID, err.Error())
		writeError(w, http.StatusInternalServerError, "upload_failed", err.Error())
		return
	}
	published = true

	details, _ := json.Marshal(map[string]any{
		"targetId": target.ID, "filename": filename, "sha256": staged.checksum,
		"commandSource": command.Source, "commandPath": command.Path,
	})
	s.store.Audit(r.Context(), p.UserID, "simple_run.create", "simple_run", runID, "success", remoteIP(r), r.UserAgent(), details)

	started = true
	go func() {
		defer s.releaseSimpleRun()
		s.executeSimpleRun(runID, command, args, staged.path, filename, staged.checksum, target, p.UserID, batch)
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": runID, "targetId": target.ID, "targetName": target.Name,
		"filename": filename, "storedPath": staged.path, "sizeBytes": staged.size, "sha256": staged.checksum,
		"commandSource": command.Source, "status": "PENDING",
		"batchId": batch.ID, "batchLast": batch.Last,
	})
}

// failSimpleRun closes a run that was accepted but could never start. The row
// already holds the target through the one-in-flight index, so leaving it
// PENDING would block every later upload to that target until the process
// restarts.
func (s *Server) failSimpleRun(ctx context.Context, runID, reason string) {
	if _, err := s.store.Pool.Exec(ctx,
		`UPDATE simple_runs SET status='FAILED',error=$2,finished_at=now() WHERE id=$1 AND status='PENDING'`,
		runID, reason); err != nil {
		s.log.Error("could not close an unstarted simple run", "run", runID, "error", err)
	}
}

// stagedArtifact is an upload that is written and hashed but not yet visible
// under its own name. Re-uploading the same filename is the normal way to
// redeploy, so committing replaces whatever sits at that path - which is
// exactly why publishing has to wait: the file being replaced may be the
// package a run that is still executing was handed, and an upload the server
// goes on to reject must never swap it out underneath that command.
type stagedArtifact struct {
	server   *Server
	path     string
	partial  string
	size     int64
	checksum string
}

// stageSimpleArtifact writes the upload into the target directory under a
// unique temporary name. Nothing at the package's own path is touched until
// commit, so a caller that fails after this point can discard the upload
// without disturbing the directory.
func (s *Server) stageSimpleArtifact(dir, filename string, file io.Reader, maxBytes int64) (*stagedArtifact, error) {
	if err := ensureUploadDir(dir); err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, errors.New("업로드 경로를 확인할 수 없습니다")
	}
	target := filepath.Join(root, filename)
	if filepath.Dir(target) != filepath.Clean(root) {
		return nil, errors.New("업로드 경로를 벗어나는 파일 이름입니다")
	}
	token, err := secure.RandomToken(16)
	if err != nil {
		return nil, errors.New("임시 파일 이름을 만들 수 없습니다")
	}
	staged := &stagedArtifact{server: s, path: target, partial: target + ".partial-" + token}
	kept := false
	defer func() {
		if !kept {
			staged.discard()
		}
	}()

	output, err := os.OpenFile(staged.partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return nil, errors.New("업로드 파일을 만들 수 없습니다")
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(file, maxBytes+1))
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		return nil, errors.New("업로드 파일을 저장할 수 없습니다")
	}
	if size > maxBytes {
		return nil, fmt.Errorf("패키지가 허용 크기(%d bytes)를 초과했습니다", maxBytes)
	}
	staged.size = size
	staged.checksum = hex.EncodeToString(hash.Sum(nil))
	kept = true
	return staged, nil
}

// commit gives the package its own name atomically, so a reader never sees a
// partial file and never sees the previous package half replaced.
func (a *stagedArtifact) commit() error {
	if err := os.Rename(a.partial, a.path); err != nil {
		return errors.New("업로드 파일을 확정할 수 없습니다")
	}
	a.partial = ""
	if err := syncDirectory(filepath.Dir(a.path)); err != nil {
		return errors.New("업로드 경로를 동기화할 수 없습니다")
	}
	return nil
}

// discard removes the staged file. Its name is unique to this upload, so this
// can never delete a package another run is using.
func (a *stagedArtifact) discard() {
	if a == nil || a.partial == "" {
		return
	}
	if err := os.Remove(a.partial); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.server.log.Warn("could not remove partial simple upload", "path", a.partial, "error", err)
	}
	a.partial = ""
}

// executeSimpleRun runs the command to completion. It deliberately does not
// use the request context: the run must survive the HTTP response that started
// it, and its own timeout is the only bound.
func (s *Server) executeSimpleRun(runID string, command resolvedCommand, args []string, artifact, filename, checksum string, target simpleTarget, actorID string, batch uploadBatch) {
	ctx := context.Background()
	if _, err := s.store.Pool.Exec(ctx, `UPDATE simple_runs SET status='RUNNING',started_at=now() WHERE id=$1 AND status='PENDING'`, runID); err != nil {
		s.log.Error("could not start simple run", "run", runID, "error", err)
		return
	}

	logs := &simpleRunLogger{server: s, ctx: ctx, runID: runID, budget: newLogBudget()}
	logs.system(fmt.Sprintf("$ %s %s", command.Path, strings.Join(args, " ")))

	result, runErr := localexec.Run(ctx, localexec.Spec{
		Path:    command.Path,
		Args:    args,
		Dir:     command.Dir,
		Timeout: command.Timeout,
		Env: map[string]string{
			"PATH":               "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"HOME":               command.Dir,
			"LANG":               "C.UTF-8",
			"LC_ALL":             "C.UTF-8",
			"RELEASEDOCK_RUN_ID": runID,
			"RELEASEDOCK_TARGET": target.Name,
			"ARTIFACT":           artifact,
			"ARTIFACT_DIR":       target.UploadDir,
			"ARTIFACT_NAME":      filename,
			"ARTIFACT_SHA256":    checksum,
			"ACTOR":              actorID,
		},
		Stdout: logs.writer("stdout"),
		Stderr: logs.writer("stderr"),
	})
	logs.flush()

	status := "SUCCESS"
	message := ""
	switch {
	case result.TimedOut:
		status = "TIMEOUT"
		message = fmt.Sprintf("제한 시간 %s를 초과하여 종료했습니다", command.Timeout)
	case runErr != nil:
		status = "FAILED"
		message = runErr.Error()
	}
	logs.system(fmt.Sprintf("exit=%d status=%s duration=%s", result.ExitCode, status, result.Duration.Round(time.Millisecond)))
	logs.flush()

	// The two post-deployment stages run only after a clean command, and their
	// results are part of the run outcome: mirroring or an app deployment that
	// never happened must not be reported as a green deployment.
	replicationStatus := stageStatusNone
	var replicationExecutionID int64
	replicationError := ""
	appDeployStatus := stageStatusNone
	appDeployError := ""
	// Read at the end of the run so a stage enabled during a long deployment
	// still applies, and a stage disabled meanwhile is honoured.
	cfg, cfgErr := loadSimpleSettings(ctx, s.store.Pool)
	if cfgErr != nil {
		s.log.Warn("could not load simple settings for post-run stages", "run", runID, "error", cfgErr)
		logs.system("[post-deploy] 배포 후 단계 설정을 읽지 못해 복제와 앱 배포를 실행하지 못했습니다: " + cfgErr.Error())
		status, message = outcomeWithoutStageSettings(status, message, cfgErr)
	}
	if cfgErr == nil && status == "SUCCESS" {
		if cfg.ReplicationEnabled && cfg.ReplicationRegistry != "" && cfg.ReplicationPolicyID > 0 {
			if !stageRuns(cfg.ReplicationScope, batch.Last) {
				replicationStatus = stageStatusSkipped
				logs.system("[replication] 업로드당 한 번만 실행하도록 설정되어 있어 마지막 파일에서 실행합니다")
			} else {
				replicationStatus, replicationExecutionID, replicationError = s.runReplication(ctx, cfg, runID, logs)
			}
			logs.flush()
		}
		if stageFailed(replicationStatus) {
			status = "FAILED"
			message = "배포 명령은 성공했으나 복제에 실패했습니다: " + replicationError
		} else if cfg.AppDeployEnabled && cfg.AppDeployPath != "" {
			// Deploying an image that was never mirrored is exactly what the
			// ordering exists to prevent, so this is reached only on a
			// replication that succeeded or was not asked for. A replication
			// merely deferred to the last package has not mirrored anything
			// yet, so the application deployment waits for it there too.
			if !appDeployStageRuns(cfg.AppDeployScope, batch.Last, replicationStatus) {
				appDeployStatus = stageStatusSkipped
				if replicationStatus == stageStatusSkipped {
					logs.system("[app-deploy] 복제를 마지막 파일에서 실행하므로 앱 배포도 마지막 파일에서 실행합니다")
				} else {
					logs.system("[app-deploy] 업로드당 한 번만 실행하도록 설정되어 있어 마지막 파일에서 실행합니다")
				}
			} else {
				appDeployStatus, appDeployError = s.runAppDeploy(ctx, cfg, runID, logs, target, artifact, filename, checksum, actorID)
			}
			logs.flush()
			if stageFailed(appDeployStatus) {
				status = "FAILED"
				message = "배포 명령과 복제는 성공했으나 앱 배포에 실패했습니다: " + appDeployError
			}
		}
	}

	if _, err := s.store.Pool.Exec(ctx, `UPDATE simple_runs
		SET status=$2,exit_code=$3,error=$4,replication_status=$5,
		    replication_execution_id=NULLIF($6,0),replication_error=$7,
		    app_deploy_status=$8,app_deploy_error=$9,finished_at=now()
		WHERE id=$1`,
		runID, status, result.ExitCode, message,
		replicationStatus, replicationExecutionID, replicationError,
		appDeployStatus, appDeployError); err != nil {
		s.log.Error("could not record simple run outcome", "run", runID, "error", err)
	}
	s.store.Audit(ctx, actorID, "simple_run."+strings.ToLower(status), "simple_run", runID, strings.ToLower(status), "", "", nil)
}

// logBudget splits the storage a run may use. stdout and stderr share the
// command budget so a run cannot store more than the documented cap, while the
// server's own progress lines draw on a reserve of their own: they are what
// tells the reader how the run ended, and losing them to a command that talked
// too much would leave a truncated log with no outcome in it.
type logBudget struct {
	command int
	system  int
}

func newLogBudget() logBudget {
	return logBudget{command: maxSimpleRunLogBytes, system: maxSimpleRunSystemLogBytes}
}

// take charges size against the stream's budget and reports how much of it may
// be stored. exhausted is true only on the payload that uses up the last of the
// command budget, which is the one moment the reader is told that later command
// output is dropped.
func (b *logBudget) take(stream string, size int) (allowed int, exhausted bool) {
	remaining := &b.command
	if stream == streamSystem {
		remaining = &b.system
	}
	if size <= 0 || *remaining <= 0 {
		return 0, false
	}
	allowed = size
	if allowed > *remaining {
		allowed = *remaining
	}
	*remaining -= allowed
	return allowed, stream != streamSystem && *remaining == 0
}

// simpleRunLogger appends command output line by line, sharing one byte budget
// and one lock across stdout and stderr so interleaved output keeps its order.
type simpleRunLogger struct {
	server  *Server
	ctx     context.Context
	runID   string
	mu      sync.Mutex
	budget  logBudget
	pending map[string][]byte
}

func (l *simpleRunLogger) writer(stream string) io.Writer {
	return &simpleRunStream{logger: l, stream: stream}
}

func (l *simpleRunLogger) append(stream string, payload []byte) {
	allowed, exhausted := l.budget.take(stream, len(payload))
	if allowed <= 0 {
		return
	}
	payload = payload[:allowed]
	if _, err := l.server.store.Pool.Exec(l.ctx,
		`INSERT INTO simple_run_logs(run_id,stream,payload) VALUES($1,$2,$3)`, l.runID, stream, payload); err != nil {
		l.server.log.Warn("could not append simple run log", "run", l.runID, "error", err)
		return
	}
	if exhausted {
		_, _ = l.server.store.Pool.Exec(l.ctx,
			`INSERT INTO simple_run_logs(run_id,stream,payload) VALUES($1,'system',$2)`,
			l.runID, []byte("로그 저장 한도에 도달하여 이후 명령 출력은 기록하지 않습니다"))
	}
	_, _ = l.server.store.Pool.Exec(l.ctx, `UPDATE simple_runs SET log_bytes=log_bytes+$2 WHERE id=$1`, l.runID, len(payload))
}

func (l *simpleRunLogger) system(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.append(streamSystem, []byte(message))
}

// write buffers until a newline so a stored row is a whole line, and forces a
// flush if a single line grows past 64 KiB.
func (l *simpleRunLogger) write(stream string, chunk []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending == nil {
		l.pending = map[string][]byte{}
	}
	buffer := append(l.pending[stream], chunk...)
	for {
		index := indexByte(buffer, '\n')
		if index < 0 {
			break
		}
		l.append(stream, trimCR(buffer[:index]))
		buffer = buffer[index+1:]
	}
	if len(buffer) > 64<<10 {
		l.append(stream, buffer)
		buffer = nil
	}
	l.pending[stream] = buffer
}

func (l *simpleRunLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for stream, buffer := range l.pending {
		if len(buffer) > 0 {
			l.append(stream, buffer)
		}
		l.pending[stream] = nil
	}
}

func indexByte(data []byte, target byte) int {
	for i, b := range data {
		if b == target {
			return i
		}
	}
	return -1
}

func trimCR(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\r' {
		return data[:len(data)-1]
	}
	return data
}

type simpleRunStream struct {
	logger *simpleRunLogger
	stream string
}

func (w *simpleRunStream) Write(chunk []byte) (int, error) {
	w.logger.write(w.stream, chunk)
	return len(chunk), nil
}

// canReadEveryRun reports whether the caller may see other people's runs.
// Simple-mode administration is the natural gate: it is already the permission
// that configures the commands those runs execute.
func canReadEveryRun(r *http.Request) bool {
	p, _ := principalFrom(r)
	return p.Has("admin.simple.read")
}

// authorizeRun resolves a run the caller is allowed to read. A run belonging to
// somebody else is reported as missing rather than forbidden so the endpoint
// does not confirm that an id exists.
func (s *Server) authorizeRun(r *http.Request, runID string) (string, error) {
	var actorID string
	if err := s.store.Pool.QueryRow(r.Context(),
		`SELECT actor_id FROM simple_runs WHERE id=$1`, runID).Scan(&actorID); err != nil {
		return "", err
	}
	if canReadEveryRun(r) {
		return actorID, nil
	}
	p, _ := principalFrom(r)
	if actorID != p.UserID {
		return "", pgx.ErrNoRows
	}
	return actorID, nil
}

func (s *Server) listSimpleRuns(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	// Without simple-mode administration a caller only ever sees their own
	// runs, regardless of what the query asks for.
	p, _ := principalFrom(r)
	actor := p.UserID
	if canReadEveryRun(r) {
		actor = strings.TrimSpace(r.URL.Query().Get("actor"))
		if r.URL.Query().Get("mine") == "true" {
			actor = p.UserID
		}
	}
	status := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))
	switch status {
	case "", "PENDING", "RUNNING", "SUCCESS", "FAILED", "TIMEOUT":
	default:
		writeError(w, http.StatusBadRequest, "invalid_filter", "status must be PENDING, RUNNING, SUCCESS, FAILED, or TIMEOUT")
		return
	}
	var total int
	if err := s.store.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM simple_runs WHERE ($1='' OR actor_id=$1) AND ($2='' OR status=$2)`, actor, status).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not count simple runs")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT run.id::text,target.name,run.original_filename,
		run.status,COALESCE(run.exit_code,0),run.command_source,run.resolved_command_path,
		run.size_bytes,run.created_at,run.started_at,run.finished_at,COALESCE(user_account.display_name,'')
		FROM simple_runs run
		JOIN simple_targets target ON target.id=run.target_id
		LEFT JOIN users user_account ON user_account.id=run.actor_id
		WHERE ($1='' OR run.actor_id=$1) AND ($2='' OR run.status=$2)
		ORDER BY run.created_at DESC LIMIT $3 OFFSET $4`, actor, status, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not list simple runs")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, targetName, filename, status, source, commandPath, actorName string
		var exitCode int
		var size int64
		var created time.Time
		var startedAt, finishedAt *time.Time
		if rows.Scan(&id, &targetName, &filename, &status, &exitCode, &source, &commandPath,
			&size, &created, &startedAt, &finishedAt, &actorName) != nil {
			writeError(w, http.StatusInternalServerError, "database_error", "could not list simple runs")
			return
		}
		items = append(items, map[string]any{
			"id": id, "targetName": targetName, "filename": filename, "status": status,
			"exitCode": exitCode, "commandSource": source, "commandPath": commandPath,
			"sizeBytes": size, "createdAt": created, "startedAt": startedAt,
			"finishedAt": finishedAt, "actorName": actorName,
		})
	}
	writeJSON(w, http.StatusOK, page(items, total, limit, offset))
}

func (s *Server) getSimpleRun(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authorizeRun(r, r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "실행 기록을 찾을 수 없습니다")
		return
	}
	var id, targetName, filename, storedPath, checksum, status, source, commandPath, errorText, actorName string
	var replicationStatus, replicationError string
	var appDeployStatus, appDeployError string
	var replicationExecutionID int64
	var args []string
	var exitCode *int
	var size int64
	var timeout int
	var created time.Time
	var startedAt, finishedAt *time.Time
	err := s.store.Pool.QueryRow(r.Context(), `SELECT run.id::text,target.name,run.original_filename,
		run.stored_path,run.sha256,run.status,run.command_source,run.resolved_command_path,
		run.resolved_command_args,run.resolved_timeout_seconds,run.exit_code,run.error,run.size_bytes,
		run.replication_status,COALESCE(run.replication_execution_id,0),run.replication_error,
		run.app_deploy_status,run.app_deploy_error,
		run.created_at,run.started_at,run.finished_at,COALESCE(user_account.display_name,'')
		FROM simple_runs run
		JOIN simple_targets target ON target.id=run.target_id
		LEFT JOIN users user_account ON user_account.id=run.actor_id
		WHERE run.id=$1`, r.PathValue("id")).
		Scan(&id, &targetName, &filename, &storedPath, &checksum, &status, &source, &commandPath,
			&args, &timeout, &exitCode, &errorText, &size,
			&replicationStatus, &replicationExecutionID, &replicationError,
			&appDeployStatus, &appDeployError,
			&created, &startedAt, &finishedAt, &actorName)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "실행 기록을 찾을 수 없습니다")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load simple run")
		return
	}
	if args == nil {
		args = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "targetName": targetName, "filename": filename, "storedPath": storedPath,
		"sha256": checksum, "status": status, "commandSource": source, "commandPath": commandPath,
		"commandArgs": args, "timeoutSeconds": timeout, "exitCode": exitCode, "error": errorText,
		"sizeBytes": size, "createdAt": created, "startedAt": startedAt, "finishedAt": finishedAt,
		"actorName":         actorName,
		"replicationStatus": replicationStatus, "replicationExecutionId": replicationExecutionID,
		"replicationError": replicationError,
		"appDeployStatus":  appDeployStatus, "appDeployError": appDeployError,
	})
}

// streamSimpleRunLogs mirrors streamReleaseLogs: poll every second, keepalive
// every 15s, resume from Last-Event-ID, and stop once the run is terminal.
func (s *Server) streamSimpleRunLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "stream_unsupported", "streaming is not supported")
		return
	}
	runID := r.PathValue("id")
	if _, err := s.authorizeRun(r, runID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "실행 기록을 찾을 수 없습니다")
		return
	}
	var status string
	err := s.store.Pool.QueryRow(r.Context(), `SELECT status FROM simple_runs WHERE id=$1`, runID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "실행 기록을 찾을 수 없습니다")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load simple run")
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
	w.WriteHeader(http.StatusOK)

	pollTicker := time.NewTicker(time.Second)
	keepaliveTicker := time.NewTicker(15 * time.Second)
	maxDuration := time.NewTimer(30 * time.Minute)
	defer pollTicker.Stop()
	defer keepaliveTicker.Stop()
	defer maxDuration.Stop()
	for {
		rows, err := s.store.Pool.Query(r.Context(),
			`SELECT id,stream,payload,created_at FROM simple_run_logs WHERE run_id=$1 AND id>$2 ORDER BY id LIMIT 500`, runID, lastID)
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
		_ = s.store.Pool.QueryRow(r.Context(), `SELECT status FROM simple_runs WHERE id=$1`, runID).Scan(&status)
		if !sent && (status == "SUCCESS" || status == "FAILED" || status == "TIMEOUT") {
			encoded, _ := json.Marshal(map[string]any{"status": status})
			fmt.Fprintf(w, "event: end\ndata: %s\n\n", encoded)
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

// errSimpleTargetAmbiguous means the caller omitted the target while more than
// one active target exists, so the server must not guess.
var errSimpleTargetAmbiguous = errors.New("simple target is ambiguous")

// resolveSimpleTarget loads the requested target, or the only active one when
// the caller did not name it. This is what lets the deploy screen accept a
// plain drag-and-drop with no selection step.
func (s *Server) resolveSimpleTarget(r *http.Request, targetID string) (simpleTarget, error) {
	if strings.TrimSpace(targetID) != "" {
		return scanSimpleTarget(s.store.Pool.QueryRow(r.Context(),
			`SELECT `+simpleTargetColumns+` FROM simple_targets WHERE id=$1 AND revoked_at IS NULL AND active`, targetID))
	}
	var count int
	if err := s.store.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM simple_targets WHERE revoked_at IS NULL AND active`).Scan(&count); err != nil {
		return simpleTarget{}, err
	}
	switch {
	case count == 0:
		return simpleTarget{}, pgx.ErrNoRows
	case count > 1:
		return simpleTarget{}, errSimpleTargetAmbiguous
	}
	return scanSimpleTarget(s.store.Pool.QueryRow(r.Context(),
		`SELECT `+simpleTargetColumns+` FROM simple_targets WHERE revoked_at IS NULL AND active LIMIT 1`))
}

// maxSimpleLogPage bounds one page of log lines so a very long run cannot force
// an unbounded response.
const maxSimpleLogPage = 2000

// listSimpleRunLogs returns stored output for a run. Unlike the SSE stream this
// works for a run that already finished, which is what makes the history usable
// after the fact. `format=text` returns the whole log as a plain download.
func (s *Server) listSimpleRunLogs(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if _, err := s.authorizeRun(r, runID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "실행 기록을 찾을 수 없습니다")
		return
	}
	if strings.EqualFold(r.URL.Query().Get("format"), "text") {
		s.downloadSimpleRunLog(w, r, runID)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit := maxSimpleLogPage
	if requested, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && requested > 0 && requested < limit {
		limit = requested
	}
	rows, err := s.store.Pool.Query(r.Context(),
		`SELECT id,stream,payload,created_at FROM simple_run_logs
		 WHERE run_id=$1 AND id>$2 ORDER BY id LIMIT $3`, runID, after, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not read run logs")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	var lastID int64
	for rows.Next() {
		var id int64
		var stream string
		var payload []byte
		var created time.Time
		if rows.Scan(&id, &stream, &payload, &created) != nil {
			writeError(w, http.StatusInternalServerError, "database_error", "could not read run logs")
			return
		}
		items = append(items, map[string]any{"id": id, "stream": stream, "message": string(payload), "createdAt": created})
		lastID = id
	}
	// The caller polls with `after` until hasMore is false, then switches to the
	// stream if the run is still going.
	writeJSON(w, http.StatusOK, map[string]any{
		"items":   items,
		"lastId":  lastID,
		"hasMore": len(items) == limit,
	})
}

func (s *Server) downloadSimpleRunLog(w http.ResponseWriter, r *http.Request, runID string) {
	var filename, status string
	if err := s.store.Pool.QueryRow(r.Context(),
		`SELECT original_filename,status FROM simple_runs WHERE id=$1`, runID).Scan(&filename, &status); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "실행 기록을 찾을 수 없습니다")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(),
		`SELECT stream,payload FROM simple_run_logs WHERE run_id=$1 ORDER BY id`, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not read run logs")
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// The run id is a UUID, so it is safe inside the header value.
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"releasedock-run-%s.log\"", runID))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	for rows.Next() {
		var stream string
		var payload []byte
		if rows.Scan(&stream, &payload) != nil {
			return
		}
		prefix := ""
		if stream == "stderr" {
			prefix = "[stderr] "
		}
		fmt.Fprintf(w, "%s%s\n", prefix, payload)
	}
}

// runReplication triggers the configured Harbor rule and waits for it to
// finish, reporting progress into the run log. It is called only after the
// deployment command succeeded, so a failed deployment never mirrors a
// half-applied state.
func (s *Server) runReplication(ctx context.Context, cfg simpleSettings, runID string, logs *simpleRunLogger) (status string, executionID int64, failure string) {
	registry, err := s.loadHarborRegistry(ctx, cfg.ReplicationRegistry)
	if err != nil {
		message := "복제 레지스트리를 사용할 수 없습니다: " + err.Error()
		logs.system("[replication] " + message)
		return "FAILED", 0, message
	}
	label := cfg.ReplicationPolicy
	if label == "" {
		label = fmt.Sprintf("#%d", cfg.ReplicationPolicyID)
	}
	logs.system(fmt.Sprintf("[replication] %s 규칙 %s 시작", registry.Name, label))

	executionID, err = s.startReplication(ctx, registry, cfg.ReplicationPolicyID)
	if err != nil {
		logs.system("[replication] 실패: " + err.Error())
		return "FAILED", 0, err.Error()
	}
	if executionID == 0 {
		// Harbor accepted the trigger but did not identify the execution, so
		// there is nothing to poll. Reporting success here would be a guess.
		logs.system("[replication] 요청은 접수됐으나 실행 ID를 확인하지 못해 상태를 추적하지 않습니다")
		return "SUCCESS", 0, ""
	}

	timeout := time.Duration(cfg.ReplicationTimeout) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	started := time.Now()
	lastReported := ""
	for {
		if time.Now().After(deadline) {
			message := fmt.Sprintf("복제가 제한 시간 %s 안에 끝나지 않았습니다 (execution %d)", timeout, executionID)
			logs.system("[replication] " + message)
			return "TIMEOUT", executionID, message
		}
		select {
		case <-ctx.Done():
			return "FAILED", executionID, "복제 대기가 취소되었습니다"
		case <-time.After(3 * time.Second):
		}
		execution, err := s.replicationExecution(ctx, registry, executionID)
		if err != nil {
			// A transient read failure should not abandon a replication that
			// is still progressing; the deadline is the real bound.
			continue
		}
		if execution.Status != lastReported {
			logs.system(fmt.Sprintf("[replication] %s (성공 %d / 실패 %d / 전체 %d)",
				execution.Status, execution.Succeed, execution.Failed, execution.Total))
			lastReported = execution.Status
		}
		done, ok := replicationTerminal(execution.Status)
		if !done {
			continue
		}
		elapsed := time.Since(started).Round(time.Second)
		if ok {
			logs.system(fmt.Sprintf("[replication] 성공 (%s)", elapsed))
			return "SUCCESS", executionID, ""
		}
		message := fmt.Sprintf("복제가 %s 상태로 끝났습니다 (실패 %d건)", execution.Status, execution.Failed)
		if execution.StatusTx != "" {
			message += ": " + execution.StatusTx
		}
		logs.system("[replication] " + message)
		return "FAILED", executionID, message
	}
}

// stageFailed reads a post-deployment stage outcome. A stage that never ran,
// deliberately or because it is off, is not a failure; anything else is.
func stageFailed(status string) bool {
	return status != stageStatusNone && status != stageStatusSkipped && status != stageStatusSuccess
}

// runAppDeploy runs the configured application deployment command. It is a
// separate command from the per-package deployment command: that one unpacks
// and loads whatever was just uploaded, while this one rolls the application
// over once the images it needs are in place.
func (s *Server) runAppDeploy(ctx context.Context, cfg simpleSettings, runID string, logs *simpleRunLogger,
	target simpleTarget, artifact, filename, checksum, actorID string) (status string, failure string) {
	if err := validateCommandFields(cfg.AppDeployPath, cfg.AppDeployArgs, cfg.AppDeployDir, cfg.AppDeployTimeout); err != nil {
		message := "앱 배포 명령 설정이 올바르지 않습니다: " + err.Error()
		logs.system("[app-deploy] " + message)
		return "FAILED", message
	}
	dir := cfg.AppDeployDir
	if dir == "" {
		dir = target.UploadDir
	}
	args := expandArgs(cfg.AppDeployArgs, artifact)
	logs.system(fmt.Sprintf("[app-deploy] $ %s %s", cfg.AppDeployPath, strings.Join(args, " ")))
	started := time.Now()

	result, runErr := localexec.Run(ctx, localexec.Spec{
		Path:    cfg.AppDeployPath,
		Args:    args,
		Dir:     dir,
		Timeout: time.Duration(cfg.AppDeployTimeout) * time.Second,
		Env: map[string]string{
			"PATH":               "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"HOME":               dir,
			"LANG":               "C.UTF-8",
			"LC_ALL":             "C.UTF-8",
			"RELEASEDOCK_RUN_ID": runID,
			"RELEASEDOCK_TARGET": target.Name,
			"RELEASEDOCK_STAGE":  "app-deploy",
			"ARTIFACT":           artifact,
			"ARTIFACT_DIR":       target.UploadDir,
			"ARTIFACT_NAME":      filename,
			"ARTIFACT_SHA256":    checksum,
			"ACTOR":              actorID,
		},
		Stdout: logs.writer("stdout"),
		Stderr: logs.writer("stderr"),
	})
	logs.flush()

	elapsed := result.Duration.Round(time.Millisecond)
	if elapsed == 0 {
		elapsed = time.Since(started).Round(time.Millisecond)
	}
	switch {
	case result.TimedOut:
		message := fmt.Sprintf("제한 시간 %d초를 초과하여 종료했습니다", cfg.AppDeployTimeout)
		logs.system("[app-deploy] " + message)
		return "TIMEOUT", message
	case runErr != nil:
		logs.system(fmt.Sprintf("[app-deploy] 실패 exit=%d: %s", result.ExitCode, runErr.Error()))
		return "FAILED", runErr.Error()
	}
	logs.system(fmt.Sprintf("[app-deploy] 성공 (%s)", elapsed))
	return "SUCCESS", ""
}
