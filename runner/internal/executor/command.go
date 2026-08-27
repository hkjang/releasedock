package executor

import (
	"bytes"
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

type Spec struct {
	Path    string
	Args    []string
	Dir     string
	Env     map[string]string
	Stdin   []byte
	Timeout time.Duration
	Stdout  io.Writer
	Stderr  io.Writer
	// Isolated marks administrator-approved deployment scripts. The production
	// dispatcher must send these commands to the separate executor process;
	// Runner deliberately refuses to execute them in the secret-bearing process.
	Isolated bool
}

type Result struct {
	ExitCode int
	Duration time.Duration
}

type Runner struct{}

func (Runner) Run(ctx context.Context, spec Spec) (Result, error) {
	if spec.Isolated {
		return Result{ExitCode: -1}, errors.New("isolated command cannot run in the local runner process")
	}
	if !filepath.IsAbs(spec.Path) {
		return Result{ExitCode: -1}, errors.New("command path must be absolute")
	}
	info, err := os.Stat(spec.Path)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("inspect command: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return Result{ExitCode: -1}, errors.New("command path must be an executable regular file")
	}
	if spec.Dir == "" || !filepath.IsAbs(spec.Dir) {
		return Result{ExitCode: -1}, errors.New("command working directory must be absolute")
	}
	if spec.Timeout <= 0 {
		return Result{ExitCode: -1}, errors.New("command timeout must be positive")
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
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}
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
		return result, fmt.Errorf("command exceeded timeout %s", spec.Timeout)
	}
	if errors.Is(runCtx.Err(), context.Canceled) {
		return result, context.Canceled
	}
	return result, fmt.Errorf("command exited with code %d: %w", result.ExitCode, err)
}

// CommandExecutor is implemented by the local process runner and the Unix
// socket client used for isolated approved scripts.
type CommandExecutor interface {
	Run(context.Context, Spec) (Result, error)
}

// Dispatcher keeps container-runtime commands local while enforcing that an
// approved script can never silently fall back into the DB/key-bearing Runner.
type Dispatcher struct {
	Local    CommandExecutor
	Isolated CommandExecutor
}

func (d Dispatcher) Run(ctx context.Context, spec Spec) (Result, error) {
	if spec.Isolated {
		if d.Isolated == nil {
			return Result{ExitCode: -1}, errors.New("isolated executor is unavailable")
		}
		return d.Isolated.Run(ctx, spec)
	}
	if d.Local == nil {
		return Result{ExitCode: -1}, errors.New("local executor is unavailable")
	}
	return d.Local.Run(ctx, spec)
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
