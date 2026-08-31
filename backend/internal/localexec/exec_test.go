package localexec

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func TestRunRejectsRelativeCommandPath(t *testing.T) {
	_, err := Run(context.Background(), Spec{Path: "sh", Dir: t.TempDir(), Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected an absolute-path rejection, got %v", err)
	}
}

func TestRunRejectsNonExecutableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, err := Run(context.Background(), Spec{Path: path, Dir: t.TempDir(), Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("expected an executable-file rejection, got %v", err)
	}
}

func TestRunRejectsRelativeWorkingDirectory(t *testing.T) {
	_, err := Run(context.Background(), Spec{Path: writeScript(t, "exit 0"), Dir: "relative", Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("expected a working-directory rejection, got %v", err)
	}
}

func TestRunRejectsMissingTimeout(t *testing.T) {
	_, err := Run(context.Background(), Spec{Path: writeScript(t, "exit 0"), Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected a timeout rejection, got %v", err)
	}
}

func TestRunRejectsInvalidEnvironmentName(t *testing.T) {
	_, err := Run(context.Background(), Spec{
		Path: writeScript(t, "exit 0"), Dir: t.TempDir(), Timeout: time.Second,
		Env: map[string]string{"lower case": "value"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid environment name") {
		t.Fatalf("expected an environment-name rejection, got %v", err)
	}
}

func TestRunCapturesOutputAndEnvironment(t *testing.T) {
	var stdout, stderr bytes.Buffer
	dir := t.TempDir()
	result, err := Run(context.Background(), Spec{
		Path:    writeScript(t, `echo "artifact=$ARTIFACT"; echo "oops" >&2`),
		Dir:     dir,
		Timeout: 5 * time.Second,
		Env:     map[string]string{"ARTIFACT": "/data/app.tar.gz", "PATH": "/usr/bin:/bin"},
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", result.ExitCode)
	}
	if got := strings.TrimSpace(stdout.String()); got != "artifact=/data/app.tar.gz" {
		t.Fatalf("unexpected stdout %q", got)
	}
	if got := strings.TrimSpace(stderr.String()); got != "oops" {
		t.Fatalf("unexpected stderr %q", got)
	}
}

// The command must never reach a shell, so metacharacters in an argument stay
// inert data rather than becoming a second command.
func TestRunPassesArgumentsWithoutShellInterpretation(t *testing.T) {
	var stdout bytes.Buffer
	_, err := Run(context.Background(), Spec{
		Path:    writeScript(t, `printf '%s' "$1"`),
		Args:    []string{"; rm -rf / #"},
		Dir:     t.TempDir(),
		Timeout: 5 * time.Second,
		Env:     map[string]string{"PATH": "/usr/bin:/bin"},
		Stdout:  &stdout,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stdout.String() != "; rm -rf / #" {
		t.Fatalf("argument was not passed literally: %q", stdout.String())
	}
}

func TestRunReportsNonZeroExitCode(t *testing.T) {
	result, err := Run(context.Background(), Spec{
		Path: writeScript(t, "exit 7"), Dir: t.TempDir(), Timeout: 5 * time.Second,
		Env: map[string]string{"PATH": "/usr/bin:/bin"},
	})
	if err == nil {
		t.Fatal("expected an error for a non-zero exit")
	}
	if result.ExitCode != 7 {
		t.Fatalf("expected exit code 7, got %d", result.ExitCode)
	}
	if result.TimedOut {
		t.Fatal("a non-zero exit must not be reported as a timeout")
	}
}

// A timeout must reap the whole process group, including a backgrounded child
// that outlives its parent.
func TestRunTimesOutAndKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "survivor")
	script := writeScript(t, `sh -c 'sleep 5; touch `+marker+`' & sleep 5`)
	started := time.Now()
	result, err := Run(context.Background(), Spec{
		Path: script, Dir: dir, Timeout: 500 * time.Millisecond,
		Env: map[string]string{"PATH": "/usr/bin:/bin"},
	})
	if err == nil || !result.TimedOut {
		t.Fatalf("expected a timeout, got err=%v result=%+v", err, result)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("timeout did not interrupt the command promptly: %s", elapsed)
	}
	// Give the orphan the time it would have needed to create the marker.
	time.Sleep(2 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a backgrounded child survived the timeout")
	}
}

func TestValidateDirRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(real, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := ValidateDir(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("expected a symlink rejection, got %v", err)
	}
}

func TestValidateArgsRejectsNUL(t *testing.T) {
	if err := ValidateArgs([]string{"ok", "bad\x00value"}); err == nil {
		t.Fatal("expected a NUL rejection")
	}
}
