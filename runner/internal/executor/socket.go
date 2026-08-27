package executor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	DefaultSocketPath            = "/run/releasedock-executor/executor.sock"
	DefaultWorkspaceRoot         = "/var/lib/releasedock/workspaces"
	CredentialDirectory          = ".target-credential"
	CredentialFile               = "credential"
	ExecutorCredentialDirectory  = ".executor-target-credential"
	protocolVersion              = 1
	maxRequestBytes              = 256 << 10
	maxFrameBytes                = 64 << 10
	maxOutputChunk               = 32 << 10
	maxArguments                 = 128
	maxArgumentBytes             = 64 << 10
	maxEnvironmentBytes          = 128 << 10
	maximumTargetCredentialBytes = 1 << 20
	maxIsolatedTimeout           = 24 * time.Hour
	initialReadTimeout           = 5 * time.Second
)

var isolatedEnvironment = map[string]struct{}{
	"PATH": {}, "HOME": {}, "LANG": {}, "LC_ALL": {},
	"RELEASEDOCK_JOB_ID": {}, "RELEASEDOCK_RELEASE_ID": {},
	"RELEASEDOCK_APPLICATION": {}, "RELEASEDOCK_VERSION": {},
	"RELEASEDOCK_ENVIRONMENT": {}, "RELEASEDOCK_PACKAGE_DIRECTORY": {},
	"RELEASEDOCK_ARTIFACT": {}, "RELEASEDOCK_IMAGES": {},
	"RELEASEDOCK_OPERATION": {}, "RELEASEDOCK_ROLLBACK_SOURCE_RELEASE_ID": {},
	"RELEASEDOCK_ROLLBACK_SOURCE_JOB_ID": {},
	"RELEASEDOCK_CREDENTIAL_TYPE":        {}, "RELEASEDOCK_CREDENTIAL_FILE": {},
}

type wireRequest struct {
	Version       int               `json:"version"`
	Path          string            `json:"path"`
	Args          []string          `json:"args"`
	Dir           string            `json:"dir"`
	Env           map[string]string `json:"env"`
	TimeoutMillis int64             `json:"timeoutMillis"`
}

type wireFrame struct {
	Type           string `json:"type"`
	Data           []byte `json:"data,omitempty"`
	ExitCode       int    `json:"exitCode,omitempty"`
	DurationMillis int64  `json:"durationMillis,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Client delegates an isolated command to the fixed local Unix socket. Both
// filesystem permissions and SO_PEERCRED are checked. In production PeerUID is
// root because systemd performs listen(2) before passing fd 3 to the executor;
// this prevents an approved script from replacing the socket with its listener.
type Client struct {
	SocketPath string
	PeerUID    uint32
}

func NewClient(peerUID uint32) *Client {
	return &Client{SocketPath: DefaultSocketPath, PeerUID: peerUID}
}

func (c *Client) Run(ctx context.Context, spec Spec) (Result, error) {
	if !spec.Isolated {
		return Result{ExitCode: -1}, errors.New("socket executor accepts isolated commands only")
	}
	if len(spec.Stdin) != 0 {
		return Result{ExitCode: -1}, errors.New("isolated command stdin is not allowed")
	}
	request, err := requestFromSpec(spec)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	socketPath := c.SocketPath
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("connect isolated executor: %w", err)
	}
	defer connection.Close()
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return Result{ExitCode: -1}, errors.New("isolated executor connection is not Unix domain socket")
	}
	uid, err := peerUID(unixConnection)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("inspect isolated executor peer: %w", err)
	}
	if uid != c.PeerUID {
		return Result{ExitCode: -1}, fmt.Errorf("isolated executor listener UID %d does not match expected UID %d", uid, c.PeerUID)
	}

	deadline := time.Now().Add(spec.Timeout + 15*time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return Result{ExitCode: -1}, err
	}
	stopClose := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopClose()
	if err := writePacket(connection, request, maxRequestBytes); err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("send isolated command: %w", err)
	}

	for {
		var frame wireFrame
		if err := readPacket(connection, &frame, maxFrameBytes); err != nil {
			if ctx.Err() != nil {
				return Result{ExitCode: -1}, ctx.Err()
			}
			return Result{ExitCode: -1}, fmt.Errorf("read isolated command response: %w", err)
		}
		switch frame.Type {
		case "stdout":
			if err := writeOutput(spec.Stdout, frame.Data); err != nil {
				return Result{ExitCode: -1}, fmt.Errorf("write isolated stdout: %w", err)
			}
		case "stderr":
			if err := writeOutput(spec.Stderr, frame.Data); err != nil {
				return Result{ExitCode: -1}, fmt.Errorf("write isolated stderr: %w", err)
			}
		case "result":
			result := Result{ExitCode: frame.ExitCode, Duration: time.Duration(frame.DurationMillis) * time.Millisecond}
			if frame.Error != "" {
				return result, errors.New(frame.Error)
			}
			return result, nil
		case "error":
			if frame.Error == "" {
				frame.Error = "isolated executor rejected request"
			}
			return Result{ExitCode: -1}, errors.New(frame.Error)
		default:
			return Result{ExitCode: -1}, fmt.Errorf("unknown isolated executor frame %q", frame.Type)
		}
	}
}

func requestFromSpec(spec Spec) (wireRequest, error) {
	if spec.Timeout < time.Millisecond || spec.Timeout > maxIsolatedTimeout {
		return wireRequest{}, fmt.Errorf("isolated timeout must be between 1ms and %s", maxIsolatedTimeout)
	}
	request := wireRequest{
		Version: protocolVersion, Path: spec.Path, Args: append([]string(nil), spec.Args...),
		Dir: spec.Dir, Env: cloneEnvironment(spec.Env), TimeoutMillis: spec.Timeout.Milliseconds(),
	}
	if err := validateRequestShape(request); err != nil {
		return wireRequest{}, err
	}
	return request, nil
}

// Server executes validated requests from exactly one configured Runner UID.
// It deliberately has no database, encryption key, registry runtime, or config
// file dependency.
type Server struct {
	WorkspaceRoot          string
	CredentialRoot         string
	ExecutorCredentialRoot string
	RunnerUID              uint32
	Local                  CommandExecutor
}

func NewServer(workspaceRoot string, runnerUID uint32, local CommandExecutor) (*Server, error) {
	if !filepath.IsAbs(workspaceRoot) || filepath.Clean(workspaceRoot) == string(filepath.Separator) {
		return nil, errors.New("executor workspace root must be an absolute non-root path")
	}
	if local == nil {
		return nil, errors.New("local command executor is required")
	}
	return &Server{
		WorkspaceRoot: filepath.Clean(workspaceRoot), CredentialRoot: DefaultCredentialRoot,
		ExecutorCredentialRoot: DefaultExecutorCredentialRoot, RunnerUID: runnerUID, Local: local,
	}, nil
}

// Serve handles exactly one request and returns. The systemd service therefore
// exits after every approved script, and KillMode=control-group removes any
// background, double-forked, or setsid descendants before socket activation can
// start the next executor instance.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("Unix listener is required")
	}
	if _, ok := listener.(*net.UnixListener); !ok {
		return errors.New("executor listener must be a Unix domain socket")
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	connection, err := listener.Accept()
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return fmt.Errorf("accept executor connection: %w", err)
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		_ = connection.Close()
		return errors.New("executor accepted a non-Unix connection")
	}
	s.handle(ctx, unixConnection)
	return nil
}

func (s *Server) handle(parent context.Context, connection *net.UnixConn) {
	defer connection.Close()
	uid, err := peerUID(connection)
	if err != nil || uid != s.RunnerUID {
		return
	}
	_ = connection.SetReadDeadline(time.Now().Add(initialReadTimeout))
	var request wireRequest
	if err := readPacket(connection, &request, maxRequestBytes); err != nil {
		_ = writePacket(connection, wireFrame{Type: "error", Error: "invalid isolated executor request"}, maxFrameBytes)
		return
	}
	_ = connection.SetReadDeadline(time.Time{})
	spec, err := s.validateRequest(request)
	if err != nil {
		_ = writePacket(connection, wireFrame{Type: "error", Error: truncateProtocolError(err.Error())}, maxFrameBytes)
		return
	}
	cleanupCredential, err := prepareExecutorCredential(&spec, s.ExecutorCredentialRoot)
	if err != nil {
		_ = writePacket(connection, wireFrame{Type: "error", Error: truncateProtocolError(err.Error())}, maxFrameBytes)
		return
	}
	credentialRemoved := false
	removeCredential := func() error {
		if credentialRemoved {
			return nil
		}
		err := cleanupCredential()
		if err == nil {
			credentialRemoved = true
		}
		return err
	}
	defer func() { _ = removeCredential() }()

	commandContext, cancel := context.WithCancel(parent)
	defer cancel()
	disconnected := make(chan struct{})
	go func() {
		defer close(disconnected)
		var unexpected [1]byte
		_, _ = connection.Read(unexpected[:])
		cancel()
	}()
	writer := &frameWriter{connection: connection}
	spec.Stdout = streamWriter{writer: writer, kind: "stdout"}
	spec.Stderr = streamWriter{writer: writer, kind: "stderr"}
	result, runErr := s.Local.Run(commandContext, spec)
	runErr = errors.Join(runErr, removeCredential())
	frame := wireFrame{Type: "result", ExitCode: result.ExitCode, DurationMillis: result.Duration.Milliseconds()}
	if runErr != nil {
		frame.Error = truncateProtocolError(runErr.Error())
	}
	_ = writer.write(frame)
	_ = connection.CloseWrite()
	select {
	case <-disconnected:
	case <-time.After(time.Second):
	}
}

func prepareExecutorCredential(spec *Spec, privateRoot string) (func() error, error) {
	handoff := spec.Env["RELEASEDOCK_CREDENTIAL_FILE"]
	if handoff == "" {
		return func() error { return nil }, nil
	}
	if err := PrepareExecutorCredentialRoot(privateRoot); err != nil {
		return nil, err
	}
	credentialPath := filepath.Join(privateRoot, CredentialFile)
	cleanup := func() error {
		// The approved script can create siblings beside its private 0600 copy.
		// Clear every direct child of this validated, fixed RuntimeDirectory so
		// no descendant-controlled file survives the response.
		clearErr := clearDirectChildren(privateRoot)
		chmodErr := os.Chmod(privateRoot, 0o700)
		return errors.Join(clearErr, chmodErr)
	}
	fail := func(err error) (func() error, error) {
		_ = cleanup()
		return nil, err
	}
	input, err := os.Open(handoff)
	if err != nil {
		return fail(fmt.Errorf("open target credential handoff: %w", err))
	}
	output, err := os.OpenFile(credentialPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = input.Close()
		return fail(fmt.Errorf("create executor-owned target credential: %w", err))
	}
	written, copyErr := io.Copy(output, io.LimitReader(input, maximumTargetCredentialBytes+1))
	inputCloseErr := input.Close()
	chmodErr := output.Chmod(0o600)
	outputCloseErr := output.Close()
	if copyErr != nil || inputCloseErr != nil || chmodErr != nil || outputCloseErr != nil {
		return fail(fmt.Errorf("copy target credential handoff: %w", errors.Join(copyErr, inputCloseErr, chmodErr, outputCloseErr)))
	}
	if written == 0 || written > maximumTargetCredentialBytes {
		return fail(fmt.Errorf("target credential must contain between 1 and %d bytes", maximumTargetCredentialBytes))
	}
	spec.Env["RELEASEDOCK_CREDENTIAL_FILE"] = credentialPath
	return cleanup, nil
}

func (s *Server) validateRequest(request wireRequest) (Spec, error) {
	if err := validateRequestShape(request); err != nil {
		return Spec{}, err
	}
	root, err := filepath.EvalSymlinks(s.WorkspaceRoot)
	if err != nil {
		return Spec{}, errors.New("executor workspace root is unavailable")
	}
	directory := filepath.Clean(request.Dir)
	realDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil || realDirectory != directory {
		return Spec{}, errors.New("command workspace must be a real directory")
	}
	relative, err := filepath.Rel(root, realDirectory)
	if err != nil || relative == "." || strings.Contains(relative, string(filepath.Separator)) || !strings.HasPrefix(relative, "job-") {
		return Spec{}, errors.New("command workspace must be one job directory under the executor root")
	}
	directoryInfo, err := os.Lstat(realDirectory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return Spec{}, errors.New("command workspace is invalid")
	}
	owner, err := ownerUID(directoryInfo)
	if err != nil || owner != s.RunnerUID || directoryInfo.Mode().Perm() != 0o770 || directoryInfo.Mode()&os.ModeSetgid == 0 || directoryInfo.Mode()&os.ModeSticky == 0 {
		return Spec{}, errors.New("command workspace ownership or permissions are invalid")
	}

	script := filepath.Clean(request.Args[0])
	expectedScriptRoot := filepath.Join(realDirectory, ".scripts")
	scriptRootInfo, err := os.Lstat(expectedScriptRoot)
	if err != nil || !scriptRootInfo.IsDir() || scriptRootInfo.Mode()&os.ModeSymlink != 0 {
		return Spec{}, errors.New("job script directory must be a real directory")
	}
	scriptRootOwner, err := ownerUID(scriptRootInfo)
	if err != nil || scriptRootOwner != s.RunnerUID || scriptRootInfo.Mode().Perm()&0o007 != 0 {
		return Spec{}, errors.New("job script directory ownership or permissions are invalid")
	}
	scriptRelative, err := filepath.Rel(expectedScriptRoot, script)
	if err != nil || scriptRelative == "." || strings.Contains(scriptRelative, string(filepath.Separator)) {
		return Spec{}, errors.New("first command argument must be a direct child of the job script directory")
	}
	scriptInfo, err := os.Lstat(script)
	if err != nil || !scriptInfo.Mode().IsRegular() || scriptInfo.Mode()&os.ModeSymlink != 0 {
		return Spec{}, errors.New("approved script must be a regular non-symlink file")
	}
	realScript, err := filepath.EvalSymlinks(script)
	if err != nil || realScript != script {
		return Spec{}, errors.New("approved script path cannot contain symlinks")
	}
	scriptOwner, err := ownerUID(scriptInfo)
	if err != nil || scriptOwner != s.RunnerUID || scriptInfo.Mode().Perm()&0o004 != 0 || scriptInfo.Mode().Perm()&0o040 == 0 {
		return Spec{}, errors.New("approved script ownership or permissions are invalid")
	}
	if request.Env["HOME"] != realDirectory {
		return Spec{}, errors.New("isolated HOME must match the job workspace")
	}
	switch request.Env["RELEASEDOCK_OPERATION"] {
	case "DEPLOY":
		expectedArtifact := filepath.Join(realDirectory, "release-package")
		if request.Env["RELEASEDOCK_PACKAGE_DIRECTORY"] != filepath.Join(realDirectory, "package") || request.Env["RELEASEDOCK_ARTIFACT"] != expectedArtifact || request.Env["RELEASEDOCK_ROLLBACK_SOURCE_RELEASE_ID"] != "" || request.Env["RELEASEDOCK_ROLLBACK_SOURCE_JOB_ID"] != "" {
			return Spec{}, errors.New("deploy artifact and package paths must match the job workspace")
		}
		artifactInfo, err := os.Lstat(expectedArtifact)
		if err != nil || !artifactInfo.Mode().IsRegular() || artifactInfo.Mode()&os.ModeSymlink != 0 {
			return Spec{}, errors.New("staged artifact must be a regular non-symlink file")
		}
		artifactOwner, err := ownerUID(artifactInfo)
		if err != nil || artifactOwner != s.RunnerUID || artifactInfo.Mode().Perm()&0o004 != 0 || artifactInfo.Mode().Perm()&0o040 == 0 {
			return Spec{}, errors.New("staged artifact ownership or permissions are invalid")
		}
	case "ROLLBACK":
		if request.Env["RELEASEDOCK_ARTIFACT"] != "" || request.Env["RELEASEDOCK_PACKAGE_DIRECTORY"] != "" || request.Env["RELEASEDOCK_ROLLBACK_SOURCE_RELEASE_ID"] == "" || request.Env["RELEASEDOCK_ROLLBACK_SOURCE_JOB_ID"] == "" {
			return Spec{}, errors.New("manual rollback must use a source release without artifact paths")
		}
	default:
		return Spec{}, errors.New("isolated ReleaseDock operation is invalid")
	}
	if err := s.validateTargetCredential(request, realDirectory, directoryInfo); err != nil {
		return Spec{}, err
	}

	return Spec{
		Path: request.Path, Args: append([]string(nil), request.Args...), Dir: realDirectory,
		Env: cloneEnvironment(request.Env), Timeout: time.Duration(request.TimeoutMillis) * time.Millisecond,
	}, nil
}

func (s *Server) validateTargetCredential(request wireRequest, workspace string, workspaceInfo os.FileInfo) error {
	credentialType := request.Env["RELEASEDOCK_CREDENTIAL_TYPE"]
	credentialPath := request.Env["RELEASEDOCK_CREDENTIAL_FILE"]
	if credentialType == "" && credentialPath == "" {
		return nil
	}
	if credentialType == "" || credentialPath == "" {
		return errors.New("target credential type and file must be provided together")
	}
	switch credentialType {
	case "SSH_PRIVATE_KEY", "KUBECONFIG", "TOKEN", "OPAQUE_FILE":
	default:
		return errors.New("target credential type is invalid")
	}
	credentialDirectory, err := CredentialJobDirectory(s.CredentialRoot, request.Env["RELEASEDOCK_JOB_ID"])
	if err != nil {
		return err
	}
	if credentialPath != filepath.Join(credentialDirectory, CredentialFile) {
		return errors.New("target credential must be the fixed direct child of its job RuntimeDirectory")
	}
	credentialRootInfo, err := os.Lstat(s.CredentialRoot)
	if err != nil || !credentialRootInfo.IsDir() || credentialRootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("target credential RuntimeDirectory is invalid")
	}
	realCredentialRoot, err := filepath.EvalSymlinks(s.CredentialRoot)
	if err != nil || realCredentialRoot != filepath.Clean(s.CredentialRoot) {
		return errors.New("target credential RuntimeDirectory cannot contain symlinks")
	}
	rootOwner, err := ownerUID(credentialRootInfo)
	if err != nil || rootOwner != s.RunnerUID || credentialRootInfo.Mode().Perm() != 0o710 || credentialRootInfo.Mode()&os.ModeSetgid != 0 {
		return errors.New("target credential RuntimeDirectory ownership or permissions are invalid")
	}
	directoryInfo, err := os.Lstat(credentialDirectory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("target credential directory is invalid")
	}
	realDirectory, err := filepath.EvalSymlinks(credentialDirectory)
	if err != nil || realDirectory != credentialDirectory {
		return errors.New("target credential directory cannot contain symlinks")
	}
	directoryOwner, err := ownerUID(directoryInfo)
	if err != nil || directoryOwner != s.RunnerUID || directoryInfo.Mode().Perm() != 0o710 || directoryInfo.Mode()&os.ModeSetgid == 0 {
		return errors.New("target credential directory ownership or permissions are invalid")
	}
	workspaceGroup, err := ownerGID(workspaceInfo)
	if err != nil {
		return errors.New("job workspace group is unavailable")
	}
	directoryGroup, err := ownerGID(directoryInfo)
	if err != nil || directoryGroup != workspaceGroup {
		return errors.New("target credential directory group is invalid")
	}
	rootGroup, err := ownerGID(credentialRootInfo)
	if err != nil || rootGroup != workspaceGroup {
		return errors.New("target credential RuntimeDirectory group is invalid")
	}
	credentialInfo, err := os.Lstat(credentialPath)
	if err != nil || !credentialInfo.Mode().IsRegular() || credentialInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("target credential must be a regular non-symlink file")
	}
	realCredential, err := filepath.EvalSymlinks(credentialPath)
	if err != nil || realCredential != credentialPath {
		return errors.New("target credential path cannot contain symlinks")
	}
	credentialOwner, err := ownerUID(credentialInfo)
	if err != nil || credentialOwner != s.RunnerUID || credentialInfo.Mode().Perm() != 0o640 {
		return errors.New("target credential ownership or permissions are invalid")
	}
	credentialGroup, err := ownerGID(credentialInfo)
	if err != nil || credentialGroup != workspaceGroup {
		return errors.New("target credential group is invalid")
	}
	return nil
}

func validateRequestShape(request wireRequest) error {
	if request.Version != protocolVersion {
		return errors.New("unsupported executor protocol version")
	}
	if !filepath.IsAbs(request.Path) || len(request.Path) > 4096 || strings.ContainsRune(request.Path, '\x00') {
		return errors.New("isolated executable path is invalid")
	}
	if !filepath.IsAbs(request.Dir) || len(request.Dir) > 4096 || strings.ContainsRune(request.Dir, '\x00') {
		return errors.New("isolated working directory is invalid")
	}
	if request.TimeoutMillis < 1 || request.TimeoutMillis > maxIsolatedTimeout.Milliseconds() {
		return errors.New("isolated timeout is outside the allowed range")
	}
	if len(request.Args) == 0 || len(request.Args) > maxArguments {
		return errors.New("isolated command argument count is invalid")
	}
	argumentBytes := 0
	for _, argument := range request.Args {
		if strings.ContainsRune(argument, '\x00') {
			return errors.New("isolated command argument contains NUL")
		}
		argumentBytes += len(argument)
	}
	if argumentBytes > maxArgumentBytes {
		return errors.New("isolated command arguments are too large")
	}
	environmentBytes := 0
	for key, value := range request.Env {
		if _, ok := isolatedEnvironment[key]; !ok {
			return fmt.Errorf("environment %s is not allowed for isolated commands", key)
		}
		if !environmentName.MatchString(key) || strings.ContainsRune(value, '\x00') {
			return errors.New("isolated command environment is invalid")
		}
		environmentBytes += len(key) + len(value)
	}
	if environmentBytes > maxEnvironmentBytes {
		return errors.New("isolated command environment is too large")
	}
	for _, required := range []string{"PATH", "HOME", "LANG", "LC_ALL", "RELEASEDOCK_JOB_ID", "RELEASEDOCK_RELEASE_ID", "RELEASEDOCK_OPERATION"} {
		if request.Env[required] == "" {
			return fmt.Errorf("isolated command environment %s is required", required)
		}
	}
	for _, required := range []string{
		"RELEASEDOCK_ARTIFACT", "RELEASEDOCK_PACKAGE_DIRECTORY", "RELEASEDOCK_ROLLBACK_SOURCE_RELEASE_ID", "RELEASEDOCK_ROLLBACK_SOURCE_JOB_ID",
		"RELEASEDOCK_CREDENTIAL_TYPE", "RELEASEDOCK_CREDENTIAL_FILE",
	} {
		if _, ok := request.Env[required]; !ok {
			return fmt.Errorf("isolated command environment %s is required", required)
		}
	}
	return nil
}

type frameWriter struct {
	connection net.Conn
	mu         sync.Mutex
	err        error
}

func (w *frameWriter) write(frame wireFrame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	_ = w.connection.SetWriteDeadline(time.Now().Add(30 * time.Second))
	w.err = writePacket(w.connection, frame, maxFrameBytes)
	return w.err
}

type streamWriter struct {
	writer *frameWriter
	kind   string
}

func (w streamWriter) Write(value []byte) (int, error) {
	for offset := 0; offset < len(value); {
		end := min(offset+maxOutputChunk, len(value))
		if err := w.writer.write(wireFrame{Type: w.kind, Data: value[offset:end]}); err != nil {
			return offset, err
		}
		offset = end
	}
	return len(value), nil
}

func writePacket(writer io.Writer, value any, maximum uint32) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload) == 0 || uint64(len(payload)) > uint64(maximum) {
		return errors.New("executor protocol packet exceeds limit")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(payload)))
	if err := writeAll(writer, size[:]); err != nil {
		return err
	}
	return writeAll(writer, payload)
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

func readPacket(reader io.Reader, target any, maximum uint32) error {
	var size [4]byte
	if _, err := io.ReadFull(reader, size[:]); err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(size[:])
	if length == 0 || length > maximum {
		return errors.New("executor protocol packet size is invalid")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("executor protocol packet has trailing JSON")
	}
	return nil
}

func writeOutput(writer io.Writer, value []byte) error {
	if writer == nil || len(value) == 0 {
		return nil
	}
	_, err := writer.Write(value)
	return err
}

func cloneEnvironment(source map[string]string) map[string]string {
	target := make(map[string]string, len(source))
	for key, value := range source {
		target[key] = value
	}
	return target
}

func truncateProtocolError(value string) string {
	const maximum = 4096
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}
