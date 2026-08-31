package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeUploadDirKeepsPathsInsideRoot(t *testing.T) {
	root := "/var/lib/releasedock/simple"
	if got, err := normalizeUploadDir(root, "/var/lib/releasedock/simple/ai-portal"); err != nil || got != "/var/lib/releasedock/simple/ai-portal" {
		t.Fatalf("expected the path to be accepted, got %q err=%v", got, err)
	}
	// The root itself is a valid target directory.
	if _, err := normalizeUploadDir(root, root); err != nil {
		t.Fatalf("expected the root itself to be accepted, got %v", err)
	}
	for _, value := range []string{
		"/etc",
		"/var/lib/releasedock/artifacts",
		"/var/lib/releasedock/simple/../../etc",
		"relative/path",
		"/",
	} {
		if _, err := normalizeUploadDir(root, value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestNormalizeUploadRootStaysUnderManagedData(t *testing.T) {
	if got, err := normalizeUploadRoot("/var/lib/releasedock/simple/"); err != nil || got != "/var/lib/releasedock/simple" {
		t.Fatalf("expected a cleaned path, got %q err=%v", got, err)
	}
	for _, value := range []string{"/opt/uploads", "/var/lib/releasedock", "/", "relative"} {
		if _, err := normalizeUploadRoot(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestSplitCommandArgsKeepsOneArgumentPerLine(t *testing.T) {
	args := splitCommandArgs("--file {{artifact}}\r\n\n--name my service\n")
	if len(args) != 2 {
		t.Fatalf("expected 2 arguments, got %d (%q)", len(args), args)
	}
	// A space inside one line must not split into two arguments.
	if args[1] != "--name my service" {
		t.Fatalf("argument was split on spaces: %q", args[1])
	}
	if joinCommandArgs(args) != "--file {{artifact}}\n--name my service" {
		t.Fatalf("round trip changed the arguments: %q", joinCommandArgs(args))
	}
}

func TestExpandArgsSubstitutesArtifactWithoutSplitting(t *testing.T) {
	args := expandArgs([]string{"--file", artifactPlaceholder, "--name", "keep"}, "/data/my app.tar.gz")
	if len(args) != 4 {
		t.Fatalf("substitution changed the argument count: %q", args)
	}
	if args[1] != "/data/my app.tar.gz" {
		t.Fatalf("unexpected substitution: %q", args[1])
	}
}

func TestResolveCommandUsesTheActiveMode(t *testing.T) {
	target := simpleTarget{
		UploadDir: "/var/lib/releasedock/simple/app", CommandPath: "/opt/deploy/per-target.sh",
		CommandArgs: []string{"a"}, TimeoutSeconds: 120,
	}
	perTarget := simpleSettings{CommandMode: commandModePerTarget}
	command, err := resolveCommand(perTarget, target)
	if err != nil {
		t.Fatalf("per-target resolve: %v", err)
	}
	if command.Path != "/opt/deploy/per-target.sh" || command.Source != commandModePerTarget {
		t.Fatalf("unexpected per-target command: %+v", command)
	}
	// An unset working directory falls back to the target's upload directory.
	if command.Dir != target.UploadDir {
		t.Fatalf("expected the upload directory as the fallback, got %q", command.Dir)
	}
	if command.Timeout != 120*time.Second {
		t.Fatalf("unexpected timeout %s", command.Timeout)
	}

	shared := simpleSettings{
		CommandMode: commandModeShared, SharedCommandPath: "/opt/deploy/shared.sh",
		SharedCommandArgs: []string{"b"}, SharedTimeoutSeconds: 300,
	}
	command, err = resolveCommand(shared, target)
	if err != nil {
		t.Fatalf("shared resolve: %v", err)
	}
	if command.Path != "/opt/deploy/shared.sh" || command.Source != commandModeShared {
		t.Fatalf("shared mode did not override the per-target command: %+v", command)
	}
}

func TestResolveCommandFailsWhenTheActiveModeHasNoCommand(t *testing.T) {
	target := simpleTarget{UploadDir: "/var/lib/releasedock/simple/app"}
	if _, err := resolveCommand(simpleSettings{CommandMode: commandModePerTarget}, target); err == nil {
		t.Fatal("expected per-target mode to fail without a target command")
	}
	if _, err := resolveCommand(simpleSettings{CommandMode: commandModeShared}, target); err == nil {
		t.Fatal("expected shared mode to fail without a shared command")
	}
}

// Switching to shared mode must not read the per-target command, and switching
// back must find it unchanged.
func TestResolveCommandLeavesPerTargetCommandIntactAcrossModes(t *testing.T) {
	target := simpleTarget{
		UploadDir: "/var/lib/releasedock/simple/app", CommandPath: "/opt/deploy/per-target.sh",
		TimeoutSeconds: 60,
	}
	shared := simpleSettings{CommandMode: commandModeShared, SharedCommandPath: "/opt/deploy/shared.sh", SharedTimeoutSeconds: 60}
	if _, err := resolveCommand(shared, target); err != nil {
		t.Fatalf("shared resolve: %v", err)
	}
	back, err := resolveCommand(simpleSettings{CommandMode: commandModePerTarget}, target)
	if err != nil || back.Path != "/opt/deploy/per-target.sh" {
		t.Fatalf("per-target command did not survive the mode switch: %+v err=%v", back, err)
	}
}

func TestValidateCommandFieldsAppliesTheSameRulesToBothPaths(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "deploy.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := validateCommandFields(script, []string{"--flag"}, dir, 600); err != nil {
		t.Fatalf("expected a valid command to be accepted, got %v", err)
	}
	if err := validateCommandFields("relative.sh", nil, dir, 600); err == nil {
		t.Fatal("expected a relative command path to be rejected")
	}
	if err := validateCommandFields(script, []string{"bad\x00arg"}, dir, 600); err == nil {
		t.Fatal("expected a NUL argument to be rejected")
	}
	if err := validateCommandFields(script, nil, dir, 0); err == nil {
		t.Fatal("expected a zero timeout to be rejected")
	}
	if err := validateCommandFields(script, nil, dir, 86401); err == nil {
		t.Fatal("expected an over-long timeout to be rejected")
	}
	if err := validateCommandFields(script, nil, "not-absolute", 600); err == nil {
		t.Fatal("expected a relative working directory to be rejected")
	}
}

func TestEnsureUploadDirRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(real, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := ensureUploadDir(link); err == nil || !strings.Contains(err.Error(), "심볼릭 링크") {
		t.Fatalf("expected a symlink rejection, got %v", err)
	}
}
