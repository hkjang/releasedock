package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hkjang/releasedock/runner/internal/config"
	runnercrypto "github.com/hkjang/releasedock/runner/internal/crypto"
	"github.com/hkjang/releasedock/runner/internal/executor"
	"github.com/hkjang/releasedock/runner/internal/pipeline"
	"github.com/hkjang/releasedock/runner/internal/store"
)

var (
	version = "dev"
	commit  = "unknown"
	builtAt = "unknown"
)

func main() {
	showVersion := flag.Bool("version", false, "print runner version")
	flag.Parse()
	if *showVersion {
		fmt.Printf("releasedock-runner %s (%s, %s)\n", version, commit, builtAt)
		return
	}
	if err := run(); err != nil {
		log.Printf("releasedock runner stopped: %v", err)
		os.Exit(1)
	}
}

func run() error {
	bootstrap, err := config.Load()
	if err != nil {
		return err
	}
	secretBox, err := runnercrypto.NewSecretBox(bootstrap.EncryptionKey)
	if err != nil {
		return err
	}
	workerID, err := newWorkerID()
	if err != nil {
		return err
	}
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	repository, err := store.Open(rootCtx, bootstrap.PostgresDSN, workerID)
	if err != nil {
		return err
	}
	defer repository.Close()
	runnerIdentity, runnerName, runnerAddress, err := runnerIdentity()
	if err != nil {
		return err
	}
	runnerActive, err := repository.RegisterRunner(rootCtx, runnerIdentity, runnerName, runnerAddress)
	if err != nil {
		return err
	}
	commandExecutor := executor.Dispatcher{
		Local: executor.Runner{},
		// systemd creates/listens on the socket as root before passing fd 3 to
		// the unprivileged executor. Linux SO_PEERCRED therefore reports UID 0
		// to the connecting Runner; the executor independently verifies Runner UID.
		Isolated: executor.NewClient(0),
	}
	settings, err := repository.LoadSettings(rootCtx)
	if err != nil {
		return err
	}
	if err := validateExecutorWorkspace(settings.WorkspaceRoot); err != nil {
		return err
	}
	// Both target credentials and registry runtime auth live in a tmpfs-backed
	// RuntimeDirectory. Validate its exact systemd ownership/mode and scavenge
	// crash remnants before accepting work. Also remove known legacy secret
	// directories from retained persistent workspaces without following links.
	hostLock, err := executor.AcquireRunnerHostLock(executor.DefaultCredentialRoot)
	if err != nil {
		return err
	}
	defer hostLock.Close()
	// Scavenge only after holding the host lock; a rejected second Runner must
	// never remove the active Runner's in-flight credential directory.
	if err := executor.PrepareRunnerCredentialRoot(executor.DefaultCredentialRoot); err != nil {
		return err
	}
	if err := executor.ScavengeWorkspaceSecrets(settings.WorkspaceRoot); err != nil {
		return err
	}
	processor, err := pipeline.New(repository, secretBox, commandExecutor)
	if err != nil {
		return err
	}
	if recovered, recoveryErr := repository.RecoverStaleJobs(rootCtx, settings.StaleJobAfter); recoveryErr != nil {
		return fmt.Errorf("recover stale jobs: %w", recoveryErr)
	} else if recovered > 0 {
		log.Printf("marked %d stale release job(s) failed", recovered)
	}
	lastRefresh := time.Now()
	lastRunnerHeartbeat := time.Now()
	log.Printf("releasedock-runner %s worker=%s started", version, workerID)
	for {
		if time.Since(lastRefresh) >= settings.SettingsRefresh {
			refreshed, refreshErr := repository.LoadSettings(rootCtx)
			if refreshErr != nil {
				log.Printf("runner settings refresh failed; retaining last valid values: %v", refreshErr)
			} else if refreshErr = validateExecutorWorkspace(refreshed.WorkspaceRoot); refreshErr != nil {
				log.Printf("runner settings refresh rejected; retaining last valid values: %v", refreshErr)
			} else {
				settings = refreshed
			}
			if recovered, recoveryErr := repository.RecoverStaleJobs(rootCtx, settings.StaleJobAfter); recoveryErr != nil {
				log.Printf("stale job recovery failed: %v", recoveryErr)
			} else if recovered > 0 {
				log.Printf("marked %d stale release job(s) failed", recovered)
			}
			lastRefresh = time.Now()
		}
		if time.Since(lastRunnerHeartbeat) >= settings.HeartbeatInterval {
			active, heartbeatErr := repository.HeartbeatRunner(rootCtx)
			if heartbeatErr != nil {
				log.Printf("runner inventory heartbeat failed: %v", heartbeatErr)
			} else {
				runnerActive = active
			}
			lastRunnerHeartbeat = time.Now()
		}
		if !runnerActive {
			timer := time.NewTimer(settings.PollInterval)
			select {
			case <-rootCtx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
			continue
		}
		job, claimErr := repository.ClaimJob(rootCtx, settings.LockRetry)
		if claimErr == nil {
			log.Printf("job=%s release=%s attempt=%d claimed", job.ID, job.ReleaseID, job.Attempt)
			if processErr := processor.Process(rootCtx, settings, job); processErr != nil {
				log.Printf("job=%s failed: %v", job.ID, processErr)
			} else {
				log.Printf("job=%s succeeded", job.ID)
			}
			continue
		}
		if errors.Is(claimErr, store.ErrInactive) {
			runnerActive = false
		} else if !errors.Is(claimErr, store.ErrNoJob) && !errors.Is(claimErr, store.ErrLockBusy) {
			log.Printf("claim release job failed: %v", claimErr)
		}
		timer := time.NewTimer(settings.PollInterval)
		select {
		case <-rootCtx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func validateExecutorWorkspace(workspaceRoot string) error {
	if filepath.Clean(workspaceRoot) != executor.DefaultWorkspaceRoot {
		return fmt.Errorf("workspace_root must be %s for the isolated executor boundary", executor.DefaultWorkspaceRoot)
	}
	return nil
}

func runnerIdentity() (identity, name, address string, err error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", "", "", fmt.Errorf("read runner hostname: %w", err)
	}
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return "", "", "", errors.New("runner hostname is empty")
	}
	return "host:" + hostname, "releasedock-runner@" + hostname, hostname, nil
}

func newWorkerID() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("read hostname: %w", err)
	}
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate worker id: %w", err)
	}
	return fmt.Sprintf("%s-%d-%s", hostname, os.Getpid(), hex.EncodeToString(random)), nil
}
