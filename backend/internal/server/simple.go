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

	stored, size, checksum, err := s.storeSimpleArtifact(target.UploadDir, filename, file, target.MaxUploadBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "upload_failed", err.Error())
		return
	}

	runID, err := secure.NewID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "id_error", "could not allocate an identifier")
		return
	}
	args := expandArgs(command.Args, stored)
	_, err = s.store.Pool.Exec(r.Context(), `INSERT INTO simple_runs
		(id,target_id,actor_id,original_filename,stored_path,size_bytes,sha256,
		 command_source,resolved_command_path,resolved_command_args,resolved_timeout_seconds,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'PENDING')`,
		runID, target.ID, p.UserID, filename, stored, size, checksum,
		command.Source, command.Path, args, int(command.Timeout/time.Second))
	if err != nil {
		// The partial unique index rejects a second in-flight run per target.
		writeError(w, http.StatusConflict, "simple_run_active", "이 대상에서 이미 실행 중인 작업이 있습니다")
		return
	}

	details, _ := json.Marshal(map[string]any{
		"targetId": target.ID, "filename": filename, "sha256": checksum,
		"commandSource": command.Source, "commandPath": command.Path,
	})
	s.store.Audit(r.Context(), p.UserID, "simple_run.create", "simple_run", runID, "success", remoteIP(r), r.UserAgent(), details)

	started = true
	go func() {
		defer s.releaseSimpleRun()
		s.executeSimpleRun(runID, command, args, stored, filename, checksum, target, p.UserID)
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": runID, "targetId": target.ID, "targetName": target.Name,
		"filename": filename, "storedPath": stored, "sizeBytes": size, "sha256": checksum,
		"commandSource": command.Source, "status": "PENDING",
	})
}

// storeSimpleArtifact writes the upload into the target directory under its
// own name, staging through a temporary file so a reader never sees a partial
// package. Re-uploading the same filename is the normal way to redeploy, so
// the rename intentionally replaces any existing file atomically.
func (s *Server) storeSimpleArtifact(dir, filename string, file io.Reader, maxBytes int64) (string, int64, string, error) {
	if err := ensureUploadDir(dir); err != nil {
		return "", 0, "", err
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", 0, "", errors.New("업로드 경로를 확인할 수 없습니다")
	}
	target := filepath.Join(root, filename)
	if filepath.Dir(target) != filepath.Clean(root) {
		return "", 0, "", errors.New("업로드 경로를 벗어나는 파일 이름입니다")
	}
	token, err := secure.RandomToken(16)
	if err != nil {
		return "", 0, "", errors.New("임시 파일 이름을 만들 수 없습니다")
	}
	partial := target + ".partial-" + token
	committed := false
	defer func() {
		if committed {
			return
		}
		if err := os.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("could not remove partial simple upload", "path", partial, "error", err)
		}
	}()

	output, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", 0, "", errors.New("업로드 파일을 만들 수 없습니다")
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(file, maxBytes+1))
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		return "", 0, "", errors.New("업로드 파일을 저장할 수 없습니다")
	}
	if size > maxBytes {
		return "", 0, "", fmt.Errorf("패키지가 허용 크기(%d bytes)를 초과했습니다", maxBytes)
	}
	if err := os.Rename(partial, target); err != nil {
		return "", 0, "", errors.New("업로드 파일을 확정할 수 없습니다")
	}
	committed = true
	if err := syncDirectory(root); err != nil {
		return "", 0, "", errors.New("업로드 경로를 동기화할 수 없습니다")
	}
	return target, size, hex.EncodeToString(hash.Sum(nil)), nil
}

// executeSimpleRun runs the command to completion. It deliberately does not
// use the request context: the run must survive the HTTP response that started
// it, and its own timeout is the only bound.
func (s *Server) executeSimpleRun(runID string, command resolvedCommand, args []string, artifact, filename, checksum string, target simpleTarget, actorID string) {
	ctx := context.Background()
	if _, err := s.store.Pool.Exec(ctx, `UPDATE simple_runs SET status='RUNNING',started_at=now() WHERE id=$1 AND status='PENDING'`, runID); err != nil {
		s.log.Error("could not start simple run", "run", runID, "error", err)
		return
	}

	logs := &simpleRunLogger{server: s, ctx: ctx, runID: runID, remaining: maxSimpleRunLogBytes}
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

	if _, err := s.store.Pool.Exec(ctx, `UPDATE simple_runs
		SET status=$2,exit_code=$3,error=$4,finished_at=now() WHERE id=$1`,
		runID, status, result.ExitCode, message); err != nil {
		s.log.Error("could not record simple run outcome", "run", runID, "error", err)
	}
	s.store.Audit(ctx, actorID, "simple_run."+strings.ToLower(status), "simple_run", runID, strings.ToLower(status), "", "", nil)
}

// simpleRunLogger appends command output line by line, sharing one byte budget
// and one lock across stdout and stderr so interleaved output keeps its order.
type simpleRunLogger struct {
	server    *Server
	ctx       context.Context
	runID     string
	mu        sync.Mutex
	remaining int
	pending   map[string][]byte
}

func (l *simpleRunLogger) writer(stream string) io.Writer {
	return &simpleRunStream{logger: l, stream: stream}
}

func (l *simpleRunLogger) append(stream string, payload []byte) {
	if len(payload) == 0 {
		return
	}
	if l.remaining <= 0 {
		return
	}
	if len(payload) > l.remaining {
		payload = payload[:l.remaining]
	}
	l.remaining -= len(payload)
	if _, err := l.server.store.Pool.Exec(l.ctx,
		`INSERT INTO simple_run_logs(run_id,stream,payload) VALUES($1,$2,$3)`, l.runID, stream, payload); err != nil {
		l.server.log.Warn("could not append simple run log", "run", l.runID, "error", err)
		return
	}
	if l.remaining == 0 {
		_, _ = l.server.store.Pool.Exec(l.ctx,
			`INSERT INTO simple_run_logs(run_id,stream,payload) VALUES($1,'system',$2)`,
			l.runID, []byte("로그 저장 한도에 도달하여 이후 출력은 기록하지 않습니다"))
	}
	_, _ = l.server.store.Pool.Exec(l.ctx, `UPDATE simple_runs SET log_bytes=log_bytes+$2 WHERE id=$1`, l.runID, len(payload))
}

func (l *simpleRunLogger) system(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.append("system", []byte(message))
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

func (s *Server) listSimpleRuns(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	// A user without simple.read on other people's work still sees their own.
	mine := r.URL.Query().Get("mine") == "true"
	p, _ := principalFrom(r)
	actor := ""
	if mine {
		actor = p.UserID
	}
	var total int
	if err := s.store.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM simple_runs WHERE ($1='' OR actor_id=$1)`, actor).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not count simple runs")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT run.id::text,target.name,run.original_filename,
		run.status,COALESCE(run.exit_code,0),run.command_source,run.resolved_command_path,
		run.size_bytes,run.created_at,run.started_at,run.finished_at,COALESCE(user_account.display_name,'')
		FROM simple_runs run
		JOIN simple_targets target ON target.id=run.target_id
		LEFT JOIN users user_account ON user_account.id=run.actor_id
		WHERE ($1='' OR run.actor_id=$1)
		ORDER BY run.created_at DESC LIMIT $2 OFFSET $3`, actor, limit, offset)
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
	var id, targetName, filename, storedPath, checksum, status, source, commandPath, errorText, actorName string
	var args []string
	var exitCode *int
	var size int64
	var timeout int
	var created time.Time
	var startedAt, finishedAt *time.Time
	err := s.store.Pool.QueryRow(r.Context(), `SELECT run.id::text,target.name,run.original_filename,
		run.stored_path,run.sha256,run.status,run.command_source,run.resolved_command_path,
		run.resolved_command_args,run.resolved_timeout_seconds,run.exit_code,run.error,run.size_bytes,
		run.created_at,run.started_at,run.finished_at,COALESCE(user_account.display_name,'')
		FROM simple_runs run
		JOIN simple_targets target ON target.id=run.target_id
		LEFT JOIN users user_account ON user_account.id=run.actor_id
		WHERE run.id=$1`, r.PathValue("id")).
		Scan(&id, &targetName, &filename, &storedPath, &checksum, &status, &source, &commandPath,
			&args, &timeout, &exitCode, &errorText, &size, &created, &startedAt, &finishedAt, &actorName)
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
		"actorName": actorName,
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
