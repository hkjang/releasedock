package executor

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunnerUsesArgumentsAndRestrictedEnvironment(t *testing.T) {
	var output bytes.Buffer
	result, err := (Runner{}).Run(context.Background(), Spec{
		Path: "/usr/bin/env", Dir: t.TempDir(), Timeout: time.Second,
		Env: map[string]string{"ONLY_THIS": "yes"}, Stdout: &output,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("run: %#v, %v", result, err)
	}
	if strings.TrimSpace(output.String()) != "ONLY_THIS=yes" {
		t.Fatalf("process inherited environment: %q", output.String())
	}
}

func TestRunnerTimeoutKillsProcessGroup(t *testing.T) {
	result, err := (Runner{}).Run(context.Background(), Spec{
		Path: "/bin/sh", Args: []string{"-c", "sleep 5"}, Dir: t.TempDir(),
		Timeout: 30 * time.Millisecond,
	})
	if err == nil || result.ExitCode == 0 {
		t.Fatalf("expected timeout, got %#v, %v", result, err)
	}
}

func TestRunnerRejectsRelativeExecutable(t *testing.T) {
	if _, err := (Runner{}).Run(context.Background(), Spec{Path: "docker", Dir: t.TempDir(), Timeout: time.Second}); err == nil {
		t.Fatal("expected absolute executable error")
	}
}

func TestRunnerRefusesIsolatedCommand(t *testing.T) {
	if _, err := (Runner{}).Run(context.Background(), Spec{
		Path: "/bin/true", Dir: t.TempDir(), Timeout: time.Second, Isolated: true,
	}); err == nil || !strings.Contains(err.Error(), "cannot run in the local runner") {
		t.Fatalf("expected isolation boundary error, got %v", err)
	}
}

type executorFunc func(context.Context, Spec) (Result, error)

func (f executorFunc) Run(ctx context.Context, spec Spec) (Result, error) { return f(ctx, spec) }

func TestDispatcherRoutesOnlyMarkedCommands(t *testing.T) {
	localCalls, isolatedCalls := 0, 0
	dispatcher := Dispatcher{
		Local: executorFunc(func(context.Context, Spec) (Result, error) {
			localCalls++
			return Result{}, nil
		}),
		Isolated: executorFunc(func(_ context.Context, spec Spec) (Result, error) {
			if !spec.Isolated {
				t.Fatal("isolated executor received unmarked command")
			}
			isolatedCalls++
			return Result{}, nil
		}),
	}
	_, _ = dispatcher.Run(context.Background(), Spec{})
	_, _ = dispatcher.Run(context.Background(), Spec{Isolated: true})
	if localCalls != 1 || isolatedCalls != 1 {
		t.Fatalf("unexpected routing: local=%d isolated=%d", localCalls, isolatedCalls)
	}
}
