//go:build linux

package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	hostLockHelperRoot  = "RELEASEDOCK_HOST_LOCK_HELPER_ROOT"
	hostLockHelperReady = "RELEASEDOCK_HOST_LOCK_HELPER_READY"
)

func TestPrepareRuntimeCredentialRootsScavengesAbruptExitRemnants(t *testing.T) {
	runnerRoot := t.TempDir()
	if err := os.Chmod(runnerRoot, 0o710); err != nil {
		t.Fatal(err)
	}
	jobDirectory, err := CreateCredentialJobDirectory(runnerRoot, "abrupt-job")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDirectory, CredentialFile), []byte("SIGKILL-secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRunnerCredentialRoot(runnerRoot); err != nil {
		t.Fatal(err)
	}
	assertDirectoryEmpty(t, runnerRoot)

	executorRoot := t.TempDir()
	if err := os.Chmod(executorRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(executorRoot, CredentialFile), []byte("executor-SIGKILL-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(executorRoot, "script-created"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := PrepareExecutorCredentialRoot(executorRoot); err != nil {
		t.Fatal(err)
	}
	assertDirectoryEmpty(t, executorRoot)
}

func TestScavengeWorkspaceSecretsDoesNotFollowSymlinks(t *testing.T) {
	workspaceRoot := t.TempDir()
	if err := os.Chmod(workspaceRoot, 0o750|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(workspaceRoot, "job-abrupt")
	if err := os.Mkdir(workspace, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace, 0o770|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".runtime-auth", ".containerd-hosts", CredentialDirectory, ExecutorCredentialDirectory} {
		directory := filepath.Join(workspace, name)
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "secret"), []byte("crash-remnant"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	external := t.TempDir()
	marker := filepath.Join(external, "must-survive")
	if err := os.WriteFile(marker, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(workspace, ".registry-certs")); err != nil {
		t.Fatal(err)
	}

	if err := ScavengeWorkspaceSecrets(workspaceRoot); err != nil {
		t.Fatal(err)
	}
	for _, name := range legacyWorkspaceSecretDirectories {
		if _, err := os.Lstat(filepath.Join(workspace, name)); !os.IsNotExist(err) {
			t.Errorf("managed crash remnant %s survived: %v", name, err)
		}
	}
	if content, err := os.ReadFile(marker); err != nil || string(content) != "outside" {
		t.Fatalf("workspace scavenger followed an external symlink: %q, %v", content, err)
	}
}

func TestCredentialJobDirectoryRejectsAliasingIdentifiers(t *testing.T) {
	for _, identifier := range []string{"", "../escape", "nested/job", "job with space"} {
		if _, err := CredentialJobDirectory("/run/releasedock-credentials", identifier); err == nil {
			t.Errorf("accepted unsafe job identifier %q", identifier)
		}
	}
}

func TestRunnerHostLockRejectsConcurrentProcessAndRecovers(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o710); err != nil {
		t.Fatal(err)
	}
	first, err := AcquireRunnerHostLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	activeDirectory, err := CreateCredentialJobDirectory(root, "active-job")
	if err != nil {
		t.Fatal(err)
	}
	activeCredential := filepath.Join(activeDirectory, CredentialFile)
	if err := os.WriteFile(activeCredential, []byte("active-secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	if second, err := AcquireRunnerHostLock(root); err == nil || !strings.Contains(err.Error(), "already active") {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("concurrent Runner acquired host lock: %v", err)
	}
	if content, err := os.ReadFile(activeCredential); err != nil || string(content) != "active-secret" {
		t.Fatalf("rejected second Runner disturbed active handoff: %q, %v", content, err)
	}
	if err := RemoveCredentialJobDirectory(root, "active-job"); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRunnerCredentialRoot(root); err != nil {
		t.Fatalf("startup scavenger rejected its fixed host lock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := AcquireRunnerHostLock(root)
	if err != nil {
		t.Fatalf("kernel did not release Runner lock after close: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerHostLockRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o710); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(external, []byte("must-survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, RunnerHostLockFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRunnerHostLock(root); err == nil {
		t.Fatal("Runner host lock followed a symlink")
	}
	if content, err := os.ReadFile(external); err != nil || string(content) != "must-survive" {
		t.Fatalf("Runner host lock modified symlink target: %q, %v", content, err)
	}
}

func TestRunnerHostLockReleasedAfterSIGKILL(t *testing.T) {
	if root := os.Getenv(hostLockHelperRoot); root != "" {
		lock, err := AcquireRunnerHostLock(root)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := os.WriteFile(os.Getenv(hostLockHelperReady), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		select {}
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o710); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestRunnerHostLockReleasedAfterSIGKILL$")
	command.Env = append(os.Environ(), hostLockHelperRoot+"="+root, hostLockHelperReady+"="+ready)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
			t.Fatal("host-lock helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = command.Process.Wait()
	lock, err := AcquireRunnerHostLock(root)
	if err != nil {
		t.Fatalf("SIGKILL did not release Runner host lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty directory %s, got %#v", path, entries)
	}
}
