//go:build linux

package executor

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSocketExecutorStreamsRestrictedCommand(t *testing.T) {
	fixture := newSocketFixture(t, uint32(os.Getuid()))
	defer fixture.close()
	var output bytes.Buffer
	spec := fixture.spec("printf '%s' \"$RELEASEDOCK_VERSION\"")
	spec.Stdout = &output
	result, err := fixture.client(uint32(os.Getuid())).Run(context.Background(), spec)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("run isolated command: %#v, %v", result, err)
	}
	if output.String() != "2.4.1" {
		t.Fatalf("unexpected isolated output %q", output.String())
	}
	fixture.assertOneShotStopped()
}

func TestSocketExecutorAllowsArtifactFreeManualRollback(t *testing.T) {
	fixture := newSocketFixture(t, uint32(os.Getuid()))
	defer fixture.close()
	var output bytes.Buffer
	spec := fixture.spec("printf '%s' \"$RELEASEDOCK_ROLLBACK_SOURCE_RELEASE_ID\"")
	spec.Env["RELEASEDOCK_OPERATION"] = "ROLLBACK"
	spec.Env["RELEASEDOCK_ROLLBACK_SOURCE_RELEASE_ID"] = "source-release-id"
	spec.Env["RELEASEDOCK_ROLLBACK_SOURCE_JOB_ID"] = "source-job-id"
	spec.Env["RELEASEDOCK_ARTIFACT"] = ""
	spec.Env["RELEASEDOCK_PACKAGE_DIRECTORY"] = ""
	spec.Stdout = &output
	if _, err := fixture.client(uint32(os.Getuid())).Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if output.String() != "source-release-id" {
		t.Fatalf("unexpected rollback context %q", output.String())
	}
	fixture.assertOneShotStopped()
}

func TestSocketExecutorValidatesJobLocalTargetCredential(t *testing.T) {
	fixture := newSocketFixture(t, uint32(os.Getuid()))
	defer fixture.close()
	credentialPath := fixture.writeTargetCredential([]byte("job-only-secret"), 0o640)
	var output bytes.Buffer
	spec := fixture.spec(`printf '%s:%s:' "$RELEASEDOCK_CREDENTIAL_TYPE" "$(stat -c %a "$RELEASEDOCK_CREDENTIAL_FILE")"; cat "$RELEASEDOCK_CREDENTIAL_FILE"`)
	spec.Env["RELEASEDOCK_CREDENTIAL_TYPE"] = "SSH_PRIVATE_KEY"
	spec.Env["RELEASEDOCK_CREDENTIAL_FILE"] = credentialPath
	spec.Stdout = &output
	if _, err := fixture.client(uint32(os.Getuid())).Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if output.String() != "SSH_PRIVATE_KEY:600:job-only-secret" {
		t.Fatalf("unexpected credential command output %q", output.String())
	}
	fixture.assertPrivateCredentialRootEmpty()
}

func TestSocketExecutorRejectsUnsafeTargetCredentialMode(t *testing.T) {
	fixture := newSocketFixture(t, uint32(os.Getuid()))
	defer fixture.close()
	credentialPath := fixture.writeTargetCredential([]byte("secret"), 0o644)
	spec := fixture.spec("exit 0")
	spec.Env["RELEASEDOCK_CREDENTIAL_TYPE"] = "TOKEN"
	spec.Env["RELEASEDOCK_CREDENTIAL_FILE"] = credentialPath
	if _, err := fixture.client(uint32(os.Getuid())).Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("expected credential permission rejection, got %v", err)
	}
}

func TestSocketExecutorRemovesPrivateCredentialCopyOnCommandError(t *testing.T) {
	fixture := newSocketFixture(t, uint32(os.Getuid()))
	defer fixture.close()
	credentialPath := fixture.writeTargetCredential([]byte("secret"), 0o640)
	spec := fixture.spec(`touch "$(dirname "$RELEASEDOCK_CREDENTIAL_FILE")/descendant-created"; exit 27`)
	spec.Env["RELEASEDOCK_CREDENTIAL_TYPE"] = "KUBECONFIG"
	spec.Env["RELEASEDOCK_CREDENTIAL_FILE"] = credentialPath
	if _, err := fixture.client(uint32(os.Getuid())).Run(context.Background(), spec); err == nil {
		t.Fatal("expected command failure")
	}
	fixture.assertPrivateCredentialRootEmpty()
}

func TestSocketExecutorRejectsGlobalCredentialPath(t *testing.T) {
	fixture := newSocketFixture(t, uint32(os.Getuid()))
	defer fixture.close()
	spec := fixture.spec("exit 0")
	spec.Env["RELEASEDOCK_CREDENTIAL_TYPE"] = "OPAQUE_FILE"
	spec.Env["RELEASEDOCK_CREDENTIAL_FILE"] = "/etc/releasedock/releasedock.env"
	if _, err := fixture.client(uint32(os.Getuid())).Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "fixed direct child") {
		t.Fatalf("expected global credential path rejection, got %v", err)
	}
}

func TestSocketExecutorEnforcesTimeout(t *testing.T) {
	fixture := newSocketFixture(t, uint32(os.Getuid()))
	defer fixture.close()
	spec := fixture.spec("sleep 5")
	spec.Timeout = 30 * time.Millisecond
	result, err := fixture.client(uint32(os.Getuid())).Run(context.Background(), spec)
	if err == nil || result.ExitCode == 0 || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected remote timeout, got %#v, %v", result, err)
	}
	fixture.assertOneShotStopped()
}

func TestSocketExecutorRejectsForbiddenEnvironment(t *testing.T) {
	fixture := newSocketFixture(t, uint32(os.Getuid()))
	defer fixture.close()
	spec := fixture.spec("exit 0")
	spec.Env["POSTGRES_DSN"] = "must-not-cross-boundary"
	if _, err := fixture.client(uint32(os.Getuid())).Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected environment allowlist error, got %v", err)
	}
}

func TestSocketExecutorRejectsWorldAccessibleWorkspace(t *testing.T) {
	fixture := newSocketFixture(t, uint32(os.Getuid()))
	defer fixture.close()
	if err := os.Chmod(fixture.workspace, 0o777|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.client(uint32(os.Getuid())).Run(context.Background(), fixture.spec("exit 0")); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("expected workspace permission rejection, got %v", err)
	}
}

func TestSocketExecutorRejectsWrongClientUID(t *testing.T) {
	otherUID := uint32(os.Getuid()) + 1
	fixture := newSocketFixture(t, otherUID)
	defer fixture.close()
	if _, err := fixture.client(uint32(os.Getuid())).Run(context.Background(), fixture.spec("exit 0")); err == nil {
		t.Fatal("expected server-side peer UID rejection")
	}
}

func TestSocketClientRejectsWrongServerUID(t *testing.T) {
	fixture := newSocketFixture(t, uint32(os.Getuid()))
	defer fixture.close()
	otherUID := uint32(os.Getuid()) + 1
	if _, err := fixture.client(otherUID).Run(context.Background(), fixture.spec("exit 0")); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected client-side peer UID rejection, got %v", err)
	}
}

type socketFixture struct {
	t              *testing.T
	root           string
	workspace      string
	credentialRoot string
	privateRoot    string
	script         string
	socket         string
	cancel         context.CancelFunc
	listener       net.Listener
	serverStopped  chan error
}

func newSocketFixture(t *testing.T, allowedUID uint32) *socketFixture {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "job-test-012345")
	if err := os.Mkdir(workspace, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace, 0o770|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	scriptRoot := filepath.Join(workspace, ".scripts")
	if err := os.Mkdir(scriptRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "release-package"), []byte("test artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(scriptRoot, "script-000-v1")
	socket := filepath.Join(t.TempDir(), "executor.sock")
	credentialRoot := t.TempDir()
	if err := os.Chmod(credentialRoot, 0o710); err != nil {
		t.Fatal(err)
	}
	privateRoot := t.TempDir()
	if err := os.Chmod(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(root, allowedUID, Runner{})
	if err != nil {
		t.Fatal(err)
	}
	server.CredentialRoot = credentialRoot
	server.ExecutorCredentialRoot = privateRoot
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(ctx, listener) }()
	return &socketFixture{
		t: t, root: root, workspace: workspace, credentialRoot: credentialRoot,
		privateRoot: privateRoot, script: script, socket: socket, cancel: cancel,
		listener: listener, serverStopped: stopped,
	}
}

func (f *socketFixture) writeTargetCredential(content []byte, mode os.FileMode) string {
	f.t.Helper()
	directory, err := CreateCredentialJobDirectory(f.credentialRoot, "job-id")
	if err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(directory, CredentialFile)
	if err := os.WriteFile(path, content, mode); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *socketFixture) assertPrivateCredentialRootEmpty() {
	f.t.Helper()
	entries, err := os.ReadDir(f.privateRoot)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(entries) != 0 {
		f.t.Fatalf("executor private credential RuntimeDirectory survived response: %#v", entries)
	}
}

func (f *socketFixture) spec(content string) Spec {
	f.t.Helper()
	if err := os.WriteFile(f.script, []byte("#!/bin/sh\n"+content+"\n"), 0o640); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(f.script, 0o640); err != nil {
		f.t.Fatal(err)
	}
	return Spec{
		Path: "/bin/sh", Args: []string{f.script}, Dir: f.workspace, Timeout: time.Second, Isolated: true,
		Env: map[string]string{
			"PATH": "/usr/bin:/bin", "HOME": f.workspace, "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8",
			"RELEASEDOCK_JOB_ID": "job-id", "RELEASEDOCK_RELEASE_ID": "release-id",
			"RELEASEDOCK_APPLICATION": "crm", "RELEASEDOCK_VERSION": "2.4.1",
			"RELEASEDOCK_ENVIRONMENT": "PROD", "RELEASEDOCK_ARTIFACT": filepath.Join(f.workspace, "release-package"),
			"RELEASEDOCK_PACKAGE_DIRECTORY":          filepath.Join(f.workspace, "package"),
			"RELEASEDOCK_IMAGES":                     "harbor.internal/crm/api:2.4.1",
			"RELEASEDOCK_OPERATION":                  "DEPLOY",
			"RELEASEDOCK_ROLLBACK_SOURCE_RELEASE_ID": "",
			"RELEASEDOCK_ROLLBACK_SOURCE_JOB_ID":     "",
			"RELEASEDOCK_CREDENTIAL_TYPE":            "",
			"RELEASEDOCK_CREDENTIAL_FILE":            "",
		},
	}
}

func (f *socketFixture) client(serverUID uint32) *Client {
	return &Client{SocketPath: f.socket, PeerUID: serverUID}
}

func (f *socketFixture) close() {
	f.cancel()
	_ = f.listener.Close()
	f.waitStopped("executor server did not stop")
}

func (f *socketFixture) assertOneShotStopped() {
	f.t.Helper()
	f.waitStopped("one-request executor remained active")
}

func (f *socketFixture) waitStopped(message string) {
	f.t.Helper()
	if f.serverStopped == nil {
		return
	}
	select {
	case err := <-f.serverStopped:
		f.serverStopped = nil
		if err != nil {
			f.t.Errorf("stop executor server: %v", err)
		}
	case <-time.After(2 * time.Second):
		f.t.Error(message)
	}
}
