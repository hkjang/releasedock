// Package localexec runs a single administrator-registered command inside the
// API process for simple mode.
//
// The guards here are deliberately ported from
// runner/internal/executor/command.go. The backend and runner are separate Go
// modules with no go.work file, so the code cannot be imported; keeping a
// stdlib-only copy also preserves the backend's two-direct-dependency policy.
// Any hardening change made there should be mirrored here.
//
// Unlike the runner path, simple mode has no separate executor UID: the
// command runs as the API service account. That is why the command may only
// ever come from administrator configuration, never from a request.
package localexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

var environmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// MaxTimeout bounds any configured command timeout.
const MaxTimeout = 24 * time.Hour

type Spec struct {
	Path    string
	Args    []string
	Dir     string
	Env     map[string]string
	Timeout time.Duration
	Stdout  io.Writer
	Stderr  io.Writer
}

type Result struct {
	ExitCode int
	Duration time.Duration
	TimedOut bool
}

// ValidateCommandPath reports whether path is usable as a command. It is
// exported so administrator input is rejected at configuration time rather
// than at the moment a user is waiting on a deployment.
func ValidateCommandPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("command path must be an absolute, cleaned path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect command: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("command path must be an executable regular file")
	}
	return nil
}

// ValidateDir reports whether dir is usable as a working directory.
func ValidateDir(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return errors.New("working directory must be an absolute, cleaned path")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect working directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("working directory must not be a symbolic link")
	}
	if !info.IsDir() {
		return errors.New("working directory must be a directory")
	}
	return nil
}

// ValidateArgs rejects argument values that cannot survive execve.
func ValidateArgs(args []string) error {
	for _, arg := range args {
		if strings.ContainsRune(arg, '\x00') {
			return errors.New("command arguments must not contain NUL")
		}
	}
	return nil
}

// Run executes spec and blocks until it finishes, fails, or times out. The
// command is never passed through a shell: Path and Args go to execve as
// separate arguments, so metacharacters in an argument stay inert data.
func Run(ctx context.Context, spec Spec) (Result, error) {
	if err := ValidateCommandPath(spec.Path); err != nil {
		return Result{ExitCode: -1}, err
	}
	if err := ValidateDir(spec.Dir); err != nil {
		return Result{ExitCode: -1}, err
	}
	if err := ValidateArgs(spec.Args); err != nil {
		return Result{ExitCode: -1}, err
	}
	if spec.Timeout <= 0 || spec.Timeout > MaxTimeout {
		return Result{ExitCode: -1}, fmt.Errorf("command timeout must be between 1s and %s", MaxTimeout)
	}
	environment, err := restrictedEnvironment(spec.Env)
	if err != nil {
		return Result{ExitCode: -1}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = environment
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	// A new process group lets the timeout reap background and double-forked
	// children instead of leaking them past the run.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}

	started := time.Now()
	err = cmd.Run()
	result := Result{ExitCode: 0, Duration: time.Since(started)}
	if err == nil {
		return result, nil
	}
	result.ExitCode = -1
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		return result, fmt.Errorf("command exceeded timeout %s", spec.Timeout)
	}
	if errors.Is(runCtx.Err(), context.Canceled) {
		return result, context.Canceled
	}
	return result, fmt.Errorf("command exited with code %d: %w", result.ExitCode, err)
}

func restrictedEnvironment(values map[string]string) ([]string, error) {
	keys := make([]string, 0, len(values))
	for key, value := range values {
		if !environmentName.MatchString(key) {
			return nil, fmt.Errorf("invalid environment name %q", key)
		}
		if strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("environment %s contains NUL", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result, nil
}
