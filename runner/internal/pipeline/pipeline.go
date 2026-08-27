package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hkjang/releasedock/runner/internal/archive"
	runnercrypto "github.com/hkjang/releasedock/runner/internal/crypto"
	"github.com/hkjang/releasedock/runner/internal/executor"
	"github.com/hkjang/releasedock/runner/internal/health"
	"github.com/hkjang/releasedock/runner/internal/logstream"
	"github.com/hkjang/releasedock/runner/internal/manifest"
	"github.com/hkjang/releasedock/runner/internal/model"
	containerruntime "github.com/hkjang/releasedock/runner/internal/runtime"
	"github.com/hkjang/releasedock/runner/internal/store"
)

var (
	sha256Pattern      = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	imageDigestPattern = regexp.MustCompile(`^sha256:[a-fA-F0-9]{64}$`)
)

type Processor struct {
	repository store.Repository
	secretBox  *runnercrypto.SecretBox
	executor   containerruntime.CommandExecutor
	runtime    runtimeFactory
	checker    health.Checker
	// credentialRoot is a systemd RuntimeDirectory under /run. Keeping all
	// registry and deployment credentials here prevents a crash or power loss
	// from leaving plaintext in retained workspaces.
	credentialRoot string
}

type runtimeClient interface {
	Login(context.Context, string, string, containerruntime.Credential, executor.Spec, containerruntime.Output) error
	Load(context.Context, string, string, executor.Spec, containerruntime.Output) error
	Tag(context.Context, string, string, string, executor.Spec, containerruntime.Output) error
	Push(context.Context, string, string, string, executor.Spec, containerruntime.Output) error
	Destination(string, string) (string, error)
}

type runtimeFactory func(model.RuntimeConfig, containerruntime.CommandExecutor) (runtimeClient, error)

func New(repository store.Repository, secretBox *runnercrypto.SecretBox, commandExecutor containerruntime.CommandExecutor) (*Processor, error) {
	if repository == nil || secretBox == nil || commandExecutor == nil {
		return nil, errors.New("repository, secret box, and command executor are required")
	}
	return &Processor{
		repository: repository, secretBox: secretBox, executor: commandExecutor,
		credentialRoot: executor.DefaultCredentialRoot,
		runtime: func(config model.RuntimeConfig, commandExecutor containerruntime.CommandExecutor) (runtimeClient, error) {
			return containerruntime.New(config, commandExecutor)
		},
	}, nil
}

type processState struct {
	job          *model.Job
	settings     model.Settings
	workspace    string
	transient    string
	extracted    string
	artifact     string
	release      manifest.Release
	images       []manifest.ResolvedImage
	destinations []string
	runtime      runtimeClient
	budget       *logstream.Budget
}

func (p *Processor) Process(ctx context.Context, settings model.Settings, job *model.Job) (returnErr error) {
	if job == nil {
		return errors.New("job is required")
	}
	if err := validateOperation(job); err != nil {
		p.finishFailure(job.ID, err)
		return err
	}
	workspace, err := createWorkspace(settings.WorkspaceRoot, job.ID)
	if err != nil {
		p.finishFailure(job.ID, fmt.Errorf("create isolated workspace: %w", err))
		return err
	}
	transient, err := executor.CreateCredentialJobDirectory(p.credentialRoot, job.ID)
	if err != nil {
		_ = cleanupWorkspace(settings.WorkspaceRoot, workspace)
		p.finishFailure(job.ID, fmt.Errorf("create transient credential directory: %w", err))
		return err
	}
	state := &processState{
		job: job, settings: settings, workspace: workspace, transient: transient,
		extracted: filepath.Join(workspace, "package"),
		budget:    logstream.NewBudget(job.Profile.MaxLogBytes),
	}
	terminalSuccess := false
	defer func() {
		if cleanupErr := executor.RemoveCredentialJobDirectory(p.credentialRoot, job.ID); cleanupErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("cleanup transient credentials: %w", cleanupErr))
		}
		for _, name := range []string{".containerd-hosts", ".registry-certs", ".runtime-auth", executor.CredentialDirectory, executor.ExecutorCredentialDirectory} {
			_ = os.RemoveAll(filepath.Join(workspace, name))
		}
		if shouldCleanup(job.Profile, terminalSuccess) {
			if cleanupErr := cleanupWorkspace(settings.WorkspaceRoot, workspace); cleanupErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("cleanup workspace: %w", cleanupErr))
			}
		}
	}()

	jobCtx, cancel := context.WithCancel(ctx)
	heartbeatErr := make(chan error, 1)
	var heartbeatWG sync.WaitGroup
	heartbeatWG.Add(1)
	go func() {
		defer heartbeatWG.Done()
		heartbeatErr <- p.heartbeat(jobCtx, cancel, settings.HeartbeatInterval, job.ID)
	}()
	defer func() {
		cancel()
		heartbeatWG.Wait()
	}()

	runErr := p.run(jobCtx, state)
	select {
	case hbErr := <-heartbeatErr:
		if hbErr != nil && !errors.Is(hbErr, context.Canceled) {
			runErr = errors.Join(runErr, hbErr)
		}
	default:
	}
	if cleanupErr := cleanupRuntimeCredentials(state.transient); cleanupErr != nil {
		runErr = errors.Join(runErr, cleanupErr)
	}
	if runErr != nil {
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer finishCancel()
		if finishErr := p.repository.FinishJob(finishCtx, job.ID, model.StatusFailed, truncate(runErr.Error(), 4000)); finishErr != nil {
			return errors.Join(runErr, finishErr)
		}
		return runErr
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer finishCancel()
	terminalStatus := model.StatusSuccess
	if job.Operation == model.OperationRollback {
		terminalStatus = model.StatusRolledBack
	}
	if err := p.repository.FinishJob(finishCtx, job.ID, terminalStatus, ""); err != nil {
		return err
	}
	terminalSuccess = true
	return nil
}

func validateOperation(job *model.Job) error {
	if job.Profile.AutoRollback {
		return errors.New("automatic rollback is disabled in v0.1; use an explicit digest-verified manual rollback")
	}
	credential := job.TargetCredential
	if credential.ID == "" {
		if credential.Type != "" || credential.Version != 0 || credential.Ciphertext != "" || credential.AAD != "" {
			return errors.New("target credential snapshot is incomplete")
		}
	} else if credential.Version <= 0 || credential.Ciphertext == "" || credential.AAD == "" || !validTargetCredentialType(credential.Type) {
		return errors.New("target credential snapshot is invalid")
	}
	requiredPhase := ""
	switch job.Operation {
	case model.OperationDeploy:
		requiredPhase = "DEPLOY"
	case model.OperationRollback:
		requiredPhase = "ROLLBACK"
		if job.RollbackSourceReleaseID == "" || job.RollbackSourceJobID == "" || len(job.RollbackImages) == 0 {
			return errors.New("manual rollback requires an immutable source DEPLOY job with verified image records")
		}
	default:
		return fmt.Errorf("unsupported job operation %q", job.Operation)
	}
	if !hasPhase(job.Profile.Scripts, requiredPhase) {
		return fmt.Errorf("operation %s requires an approved %s script", job.Operation, requiredPhase)
	}
	return nil
}

func validTargetCredentialType(value string) bool {
	switch value {
	case "SSH_PRIVATE_KEY", "KUBECONFIG", "TOKEN", "OPAQUE_FILE":
		return true
	default:
		return false
	}
}

func (p *Processor) run(ctx context.Context, state *processState) error {
	if state.job.Operation == model.OperationRollback {
		return p.runManualRollback(ctx, state)
	}
	steps := []struct {
		status model.JobStatus
		fn     func(context.Context, *processState, model.Step, containerruntime.Output) (model.StepResult, error)
	}{
		{model.StatusValidating, p.validateArtifact},
	}
	if state.job.Operation == model.OperationDeploy && hasPhase(state.job.Profile.Scripts, "PRE_DEPLOY") {
		steps = append(steps, struct {
			status model.JobStatus
			fn     func(context.Context, *processState, model.Step, containerruntime.Output) (model.StepResult, error)
		}{model.StatusPreCheck, func(ctx context.Context, state *processState, step model.Step, output containerruntime.Output) (model.StepResult, error) {
			return p.runScripts(ctx, state, step, output, "PRE_DEPLOY")
		}})
	}
	steps = append(steps, []struct {
		status model.JobStatus
		fn     func(context.Context, *processState, model.Step, containerruntime.Output) (model.StepResult, error)
	}{
		{model.StatusExtracting, p.extractArtifact},
		{model.StatusImageInspect, p.inspectImages},
		{model.StatusImageLoad, p.loadImages},
		{model.StatusImageTag, p.tagImages},
		{model.StatusImagePush, p.pushImages},
		{model.StatusDeploying, p.deploy},
		{model.StatusVerifying, p.verify},
	}...)
	for _, item := range steps {
		err := p.withStep(ctx, state, item.status, func(step model.Step, output containerruntime.Output) (model.StepResult, error) {
			return item.fn(ctx, state, step, output)
		})
		if err != nil {
			return fmt.Errorf("%s: %w", item.status, err)
		}
	}
	return nil
}

func (p *Processor) runManualRollback(ctx context.Context, state *processState) error {
	steps := []struct {
		status model.JobStatus
		fn     func(model.Step, containerruntime.Output) (model.StepResult, error)
	}{
		{model.StatusValidating, func(step model.Step, output containerruntime.Output) (model.StepResult, error) {
			return p.validateRollbackSource(ctx, state, step, output)
		}},
		{model.StatusRollback, func(step model.Step, output containerruntime.Output) (model.StepResult, error) {
			return p.runScripts(ctx, state, step, output, "ROLLBACK")
		}},
		{model.StatusVerifying, func(step model.Step, output containerruntime.Output) (model.StepResult, error) {
			return p.verify(ctx, state, step, output)
		}},
	}
	for _, item := range steps {
		if err := p.withStep(ctx, state, item.status, item.fn); err != nil {
			return fmt.Errorf("%s: %w", item.status, err)
		}
	}
	return nil
}

func (p *Processor) validateRollbackSource(ctx context.Context, state *processState, _ model.Step, output containerruntime.Output) (model.StepResult, error) {
	runtimeClient, err := p.runtime(state.job.Profile.Runtime, p.executor)
	if err != nil {
		return model.StepResult{}, err
	}
	credential, err := p.decryptCredential(state.job.Profile.Runtime)
	if err != nil {
		return model.StepResult{}, err
	}
	registryClient, err := containerruntime.NewRegistryClient(
		state.job.Profile.Runtime.RegistryURL, state.job.Profile.Runtime.RegistryCAPEM,
		state.job.Profile.Runtime.RegistryInsecure, credential, state.job.Profile.CommandTimeout,
	)
	if err != nil {
		return model.StepResult{}, err
	}
	project := strings.Trim(state.job.Profile.Runtime.RegistryProject, "/")
	for _, image := range state.job.RollbackImages {
		if !imageDigestPattern.MatchString(image.Digest) {
			return model.StepResult{}, fmt.Errorf("rollback source image %s has invalid digest %q", image.DestinationRef, image.Digest)
		}
		expectedDestination, err := runtimeClient.Destination(image.Repository, image.Tag)
		if err != nil {
			return model.StepResult{}, fmt.Errorf("validate rollback image destination: %w", err)
		}
		if expectedDestination != image.DestinationRef {
			return model.StepResult{}, fmt.Errorf("rollback image destination drift: stored %q, profile resolves %q", image.DestinationRef, expectedDestination)
		}
		normalizedDigest := "sha256:" + strings.ToLower(strings.TrimPrefix(image.Digest, "sha256:"))
		repository := project + "/" + strings.Trim(image.Repository, "/")
		registryDigest, err := registryClient.Digest(ctx, repository, normalizedDigest)
		if err != nil {
			return model.StepResult{}, fmt.Errorf("verify rollback source image %s: %w", expectedDestination, err)
		}
		if !strings.EqualFold(registryDigest, normalizedDigest) {
			return model.StepResult{}, fmt.Errorf("rollback source digest mismatch for %s: stored %s, registry %s", expectedDestination, normalizedDigest, registryDigest)
		}
		digestReference := strings.TrimSuffix(expectedDestination, ":"+image.Tag) + "@" + normalizedDigest
		state.destinations = append(state.destinations, digestReference)
		verifiedImage := image
		verifiedImage.Digest = normalizedDigest
		if err := p.repository.RecordImage(ctx, state.job.ID, verifiedImage); err != nil {
			return model.StepResult{}, err
		}
		_, _ = fmt.Fprintf(output.Stdout, "verified rollback image %s\n", digestReference)
	}
	metadata, _ := json.Marshal(map[string]any{
		"sourceReleaseId": state.job.RollbackSourceReleaseID,
		"sourceJobId":     state.job.RollbackSourceJobID,
		"images":          len(state.destinations),
		"digestPinned":    true,
	})
	return model.StepResult{Metadata: metadata}, nil
}

func (p *Processor) withStep(ctx context.Context, state *processState, status model.JobStatus, fn func(model.Step, containerruntime.Output) (model.StepResult, error)) error {
	step, err := p.repository.BeginStep(ctx, state.job.ID, status)
	if err != nil {
		return err
	}
	stdout := logstream.NewWriter(ctx, p.repository, state.job.ID, step.ID, "stdout", state.settings.LogChunkBytes, state.budget)
	stderr := logstream.NewWriter(ctx, p.repository, state.job.ID, step.ID, "stderr", state.settings.LogChunkBytes, state.budget)
	result, runErr := fn(step, containerruntime.Output{Stdout: stdout, Stderr: stderr})
	logErr := errors.Join(stdout.Err(), stderr.Err())
	if logErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("persist command output: %w", logErr))
	}
	if runErr != nil && result.Message == "" {
		result.Message = truncate(runErr.Error(), 2000)
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer finishCancel()
	if finishErr := p.repository.FinishStep(finishCtx, step, runErr == nil, result); finishErr != nil {
		runErr = errors.Join(runErr, finishErr)
	}
	return runErr
}

func (p *Processor) validateArtifact(ctx context.Context, state *processState, _ model.Step, _ containerruntime.Output) (model.StepResult, error) {
	artifact, digest, size, err := stageArtifact(ctx, state.settings.ArtifactRoot, state.job.ArtifactPath, state.workspace, state.job.Profile.Extraction.MaxArchiveBytes)
	metadata, _ := json.Marshal(map[string]any{"sha256": digest, "bytes": size})
	if err != nil {
		return model.StepResult{Metadata: metadata}, err
	}
	if !sha256Pattern.MatchString(state.job.ExpectedSHA256) || !strings.EqualFold(digest, state.job.ExpectedSHA256) {
		return model.StepResult{Metadata: metadata}, fmt.Errorf("SHA256 mismatch: expected %s, got %s", state.job.ExpectedSHA256, digest)
	}
	state.artifact = artifact
	return model.StepResult{Metadata: metadata}, nil
}

func (p *Processor) extractArtifact(ctx context.Context, state *processState, _ model.Step, _ containerruntime.Output) (model.StepResult, error) {
	result, err := archive.Extract(ctx, state.artifact, state.extracted, state.job.Profile.Extraction)
	metadata, _ := json.Marshal(map[string]any{"entries": len(result.Files), "bytes": result.ExtractedBytes})
	return model.StepResult{Metadata: metadata}, err
}

func (p *Processor) inspectImages(ctx context.Context, state *processState, _ model.Step, _ containerruntime.Output) (model.StepResult, error) {
	release, err := manifest.Load(state.extracted, state.job.ManifestPath)
	if err != nil {
		return model.StepResult{}, err
	}
	if release.Application != state.job.Application || release.Version != state.job.Version {
		return model.StepResult{}, fmt.Errorf("manifest identity %s/%s does not match job %s/%s", release.Application, release.Version, state.job.Application, state.job.Version)
	}
	images, err := manifest.ResolveImages(state.extracted, release, state.job.Profile.Extraction.MaxImages)
	if err != nil {
		return model.StepResult{}, err
	}
	runtimeClient, err := p.runtime(state.job.Profile.Runtime, p.executor)
	if err != nil {
		return model.StepResult{}, err
	}
	state.release, state.images, state.runtime = release, images, runtimeClient
	for _, image := range images {
		destination, err := runtimeClient.Destination(image.Repository, image.Tag)
		if err != nil {
			return model.StepResult{}, err
		}
		state.destinations = append(state.destinations, destination)
		relative, _ := filepath.Rel(state.extracted, image.FilePath)
		if err := p.repository.RecordImage(ctx, state.job.ID, model.ImageRecord{
			FilePath: filepath.ToSlash(relative), SourceRef: image.SourceRef,
			DestinationRef: destination, Repository: image.Repository, Tag: image.Tag,
		}); err != nil {
			return model.StepResult{}, err
		}
	}
	metadata, _ := json.Marshal(map[string]any{"images": len(images)})
	return model.StepResult{Metadata: metadata}, nil
}

func (p *Processor) loadImages(ctx context.Context, state *processState, _ model.Step, output containerruntime.Output) (model.StepResult, error) {
	base := p.commandBase(state)
	for _, image := range state.images {
		if err := state.runtime.Load(ctx, state.workspace, image.FilePath, base, output); err != nil {
			return model.StepResult{}, err
		}
	}
	return model.StepResult{}, nil
}

func (p *Processor) tagImages(ctx context.Context, state *processState, _ model.Step, output containerruntime.Output) (model.StepResult, error) {
	base := p.commandBase(state)
	for i, image := range state.images {
		if err := state.runtime.Tag(ctx, state.workspace, image.SourceRef, state.destinations[i], base, output); err != nil {
			return model.StepResult{}, err
		}
	}
	return model.StepResult{}, nil
}

func (p *Processor) pushImages(ctx context.Context, state *processState, _ model.Step, output containerruntime.Output) (model.StepResult, error) {
	credential, err := p.decryptCredential(state.job.Profile.Runtime)
	if err != nil {
		return model.StepResult{}, err
	}
	base := p.commandBase(state)
	if err := state.runtime.Login(ctx, state.workspace, state.transient, credential, base, output); err != nil {
		return model.StepResult{}, err
	}
	registryClient, err := containerruntime.NewRegistryClient(
		state.job.Profile.Runtime.RegistryURL, state.job.Profile.Runtime.RegistryCAPEM,
		state.job.Profile.Runtime.RegistryInsecure, credential, state.job.Profile.CommandTimeout,
	)
	if err != nil {
		return model.StepResult{}, err
	}
	for i, image := range state.images {
		if err := state.runtime.Push(ctx, state.workspace, state.transient, state.destinations[i], base, output); err != nil {
			return model.StepResult{}, err
		}
		repository := strings.Trim(state.job.Profile.Runtime.RegistryProject, "/") + "/" + image.Repository
		digest, err := registryClient.Digest(ctx, repository, image.Tag)
		if err != nil {
			return model.StepResult{}, err
		}
		relative, _ := filepath.Rel(state.extracted, image.FilePath)
		if err := p.repository.RecordImage(ctx, state.job.ID, model.ImageRecord{
			FilePath: filepath.ToSlash(relative), SourceRef: image.SourceRef,
			DestinationRef: state.destinations[i], Repository: image.Repository,
			Tag: image.Tag, Digest: digest,
		}); err != nil {
			return model.StepResult{}, err
		}
	}
	return model.StepResult{}, nil
}

func (p *Processor) deploy(ctx context.Context, state *processState, step model.Step, output containerruntime.Output) (model.StepResult, error) {
	for _, phase := range []string{"DEPLOY", "POST_DEPLOY"} {
		if _, err := p.runScripts(ctx, state, step, output, phase); err != nil {
			return model.StepResult{}, err
		}
	}
	return model.StepResult{}, nil
}

func (p *Processor) runScripts(ctx context.Context, state *processState, _ model.Step, output containerruntime.Output, phase string) (model.StepResult, error) {
	directory := filepath.Join(state.workspace, ".scripts")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return model.StepResult{}, err
	}
	executed := 0
	for index, script := range state.job.Profile.Scripts {
		if script.Phase != phase {
			continue
		}
		if err := validateApprovedScript(script); err != nil {
			return model.StepResult{}, err
		}
		name := fmt.Sprintf("script-%03d-v%d", index, script.Version)
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, script.Content, 0o640); err != nil {
			return model.StepResult{}, fmt.Errorf("materialize approved script: %w", err)
		}
		timeout := script.Timeout
		if timeout <= 0 || timeout > state.job.Profile.CommandTimeout {
			timeout = state.job.Profile.CommandTimeout
		}
		args := append([]string{path}, script.Args...)
		environment := p.restrictedEnv(state)
		spec := executor.Spec{
			Path: script.InterpreterPath, Args: args, Dir: state.workspace,
			Env: environment, Timeout: timeout, Isolated: true,
		}
		result, err := p.runApprovedScript(ctx, state, spec, output)
		removeScriptErr := os.Remove(path)
		err = errors.Join(err, removeScriptErr)
		if err != nil {
			return model.StepResult{ExitCode: result.ExitCode}, fmt.Errorf("approved script %s v%d (%s): %w", script.Name, script.Version, phase, err)
		}
		executed++
	}
	metadata, _ := json.Marshal(map[string]any{"phase": phase, "scripts": executed})
	return model.StepResult{Metadata: metadata}, nil
}

const maximumTargetCredentialBytes = 1 << 20

func (p *Processor) runApprovedScript(ctx context.Context, state *processState, spec executor.Spec, output containerruntime.Output) (executor.Result, error) {
	credential := state.job.TargetCredential
	spec.Env["RELEASEDOCK_CREDENTIAL_TYPE"] = ""
	spec.Env["RELEASEDOCK_CREDENTIAL_FILE"] = ""
	if credential.ID == "" {
		spec.Stdout, spec.Stderr = output.Stdout, output.Stderr
		return p.executor.Run(ctx, spec)
	}
	plaintext, err := p.secretBox.Decrypt(credential.Ciphertext, credential.AAD)
	if err != nil {
		return executor.Result{ExitCode: -1}, fmt.Errorf("decrypt target credential: %w", err)
	}
	defer clear(plaintext)
	if len(plaintext) == 0 || len(plaintext) > maximumTargetCredentialBytes {
		return executor.Result{ExitCode: -1}, fmt.Errorf("target credential must contain between 1 and %d bytes", maximumTargetCredentialBytes)
	}
	credentialPath := filepath.Join(state.transient, executor.CredentialFile)
	if err := writeTargetCredential(credentialPath, plaintext); err != nil {
		return executor.Result{ExitCode: -1}, err
	}
	removed := false
	removeCredential := func() error {
		if removed {
			return nil
		}
		if err := os.Remove(credentialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove target credential handoff: %w", err)
		}
		removed = true
		return nil
	}
	defer func() { _ = removeCredential() }()

	spec.Env["RELEASEDOCK_CREDENTIAL_TYPE"] = credential.Type
	spec.Env["RELEASEDOCK_CREDENTIAL_FILE"] = credentialPath
	stdout := newExactSecretRedactor(output.Stdout, plaintext)
	stderr := newExactSecretRedactor(output.Stderr, plaintext)
	spec.Stdout, spec.Stderr = stdout, stderr
	result, runErr := p.executor.Run(ctx, spec)
	flushErr := errors.Join(stdout.Flush(), stderr.Flush())
	removeErr := removeCredential()
	return result, errors.Join(runErr, flushErr, removeErr)
}

func writeTargetCredential(path string, plaintext []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("create target credential handoff: %w", err)
	}
	written := 0
	for written < len(plaintext) {
		count, writeErr := file.Write(plaintext[written:])
		if writeErr != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return fmt.Errorf("write target credential handoff: %w", writeErr)
		}
		if count <= 0 {
			_ = file.Close()
			_ = os.Remove(path)
			return errors.New("write target credential handoff: short write")
		}
		written += count
	}
	chmodErr := file.Chmod(0o640)
	closeErr := file.Close()
	if chmodErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("finalize target credential handoff: %w", errors.Join(chmodErr, closeErr))
	}
	return nil
}

func (p *Processor) verify(ctx context.Context, state *processState, _ model.Step, output containerruntime.Output) (model.StepResult, error) {
	for index, check := range state.job.Profile.HealthChecks {
		if err := p.checker.Check(ctx, check); err != nil {
			return model.StepResult{}, fmt.Errorf("health check %d: %w", index+1, err)
		}
		_, _ = fmt.Fprintf(output.Stdout, "health check %d (%s) passed\n", index+1, check.Type)
	}
	metadata, _ := json.Marshal(map[string]any{"healthChecks": len(state.job.Profile.HealthChecks)})
	return model.StepResult{Metadata: metadata}, nil
}

func (p *Processor) commandBase(state *processState) executor.Spec {
	return executor.Spec{Env: p.restrictedEnv(state), Timeout: state.job.Profile.CommandTimeout}
}

func (p *Processor) restrictedEnv(state *processState) map[string]string {
	artifact, packageDirectory, rollbackSourceReleaseID, rollbackSourceJobID := state.artifact, state.extracted, "", ""
	if state.job.Operation == model.OperationRollback {
		artifact, packageDirectory = "", ""
		rollbackSourceReleaseID = state.job.RollbackSourceReleaseID
		rollbackSourceJobID = state.job.RollbackSourceJobID
	}
	return map[string]string{
		"PATH":                                   state.settings.CommandPath,
		"HOME":                                   state.workspace,
		"LANG":                                   "C.UTF-8",
		"LC_ALL":                                 "C.UTF-8",
		"RELEASEDOCK_JOB_ID":                     state.job.ID,
		"RELEASEDOCK_RELEASE_ID":                 state.job.ReleaseID,
		"RELEASEDOCK_APPLICATION":                state.job.Application,
		"RELEASEDOCK_VERSION":                    state.job.Version,
		"RELEASEDOCK_ENVIRONMENT":                state.job.Environment,
		"RELEASEDOCK_ARTIFACT":                   artifact,
		"RELEASEDOCK_PACKAGE_DIRECTORY":          packageDirectory,
		"RELEASEDOCK_IMAGES":                     strings.Join(state.destinations, ","),
		"RELEASEDOCK_OPERATION":                  string(state.job.Operation),
		"RELEASEDOCK_ROLLBACK_SOURCE_RELEASE_ID": rollbackSourceReleaseID,
		"RELEASEDOCK_ROLLBACK_SOURCE_JOB_ID":     rollbackSourceJobID,
		"RELEASEDOCK_CREDENTIAL_TYPE":            "",
		"RELEASEDOCK_CREDENTIAL_FILE":            "",
	}
}

func (p *Processor) decryptCredential(config model.RuntimeConfig) (containerruntime.Credential, error) {
	if config.CredentialCiphertext == "" {
		return containerruntime.Credential{}, nil
	}
	plaintext, err := p.secretBox.Decrypt(config.CredentialCiphertext, config.CredentialAAD)
	if err != nil {
		return containerruntime.Credential{}, fmt.Errorf("decrypt registry credential: %w", err)
	}
	defer clear(plaintext)
	var credential containerruntime.Credential
	decoder := json.NewDecoder(bytes.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credential); err != nil {
		return containerruntime.Credential{}, fmt.Errorf("decode registry credential: %w", err)
	}
	return credential, nil
}

func (p *Processor) heartbeat(ctx context.Context, cancel context.CancelFunc, interval time.Duration, jobID string) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := p.repository.Heartbeat(ctx, jobID); err != nil {
				cancel()
				return fmt.Errorf("job heartbeat: %w", err)
			}
		}
	}
}

func (p *Processor) finishFailure(jobID string, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = p.repository.FinishJob(ctx, jobID, model.StatusFailed, truncate(cause.Error(), 4000))
}

func stageArtifact(ctx context.Context, root, relative, workspace string, maximum int64) (string, string, int64, error) {
	source, err := safeArtifact(root, relative)
	if err != nil {
		return "", "", 0, err
	}
	input, err := os.Open(source)
	if err != nil {
		return "", "", 0, err
	}
	defer input.Close()
	destination := filepath.Join(workspace, "release-package")
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return "", "", 0, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(&contextReader{ctx: ctx, reader: input}, maximum+1))
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(destination)
		return "", "", written, errors.Join(copyErr, closeErr)
	}
	if written > maximum {
		_ = os.Remove(destination)
		return "", "", written, fmt.Errorf("archive exceeds maximum %d bytes", maximum)
	}
	return destination, hex.EncodeToString(hash.Sum(nil)), written, nil
}

func safeArtifact(root, relative string) (string, error) {
	if !filepath.IsAbs(root) || relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") {
		return "", errors.New("artifact_root must be absolute and artifact_path must be relative")
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(rootReal, filepath.Clean(filepath.FromSlash(relative)))
	rel, err := filepath.Rel(rootReal, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("artifact path escapes artifact root")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if real != path {
		return "", errors.New("artifact symlinks are rejected")
	}
	info, err := os.Lstat(real)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("artifact is not a regular file")
	}
	return real, nil
}

func createWorkspace(root, jobID string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("workspace_root must be absolute")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("workspace_root must be a real directory")
	}
	prefix := "job-" + sanitize(jobID) + "-"
	workspace, err := os.MkdirTemp(root, prefix)
	if err != nil {
		return "", err
	}
	// Setgid gives staged files the workspace group; sticky prevents the
	// executor UID from replacing Runner-owned inputs while still allowing
	// approved scripts to create their own preprocessing output files.
	if err := os.Chmod(workspace, 0o770|os.ModeSetgid|os.ModeSticky); err != nil {
		_ = os.Remove(workspace)
		return "", err
	}
	return workspace, nil
}

func cleanupWorkspace(root, workspace string) error {
	relative, err := filepath.Rel(root, workspace)
	if err != nil || relative == "." || strings.Contains(relative, string(filepath.Separator)) || !strings.HasPrefix(relative, "job-") {
		return errors.New("refusing to remove workspace outside configured root")
	}
	return os.RemoveAll(workspace)
}

func cleanupRuntimeCredentials(workspace string) error {
	var result error
	for _, name := range []string{".containerd-hosts", ".registry-certs", ".runtime-auth"} {
		if err := os.RemoveAll(filepath.Join(workspace, name)); err != nil {
			result = errors.Join(result, fmt.Errorf("remove %s: %w", name, err))
		}
	}
	if result != nil {
		return fmt.Errorf("remove runtime credentials: %w", result)
	}
	return nil
}

func shouldCleanup(profile model.Profile, success bool) bool {
	if !profile.CleanupWorkspace {
		return false
	}
	return success || !profile.KeepFailedWorkspace
}

func validateApprovedScript(script model.Script) error {
	if script.ID == "" || script.Version <= 0 || script.ApprovedAt.IsZero() {
		return errors.New("script is not an approved immutable version")
	}
	if !filepath.IsAbs(script.InterpreterPath) {
		return errors.New("approved script interpreter must be absolute")
	}
	if !sha256Pattern.MatchString(script.SHA256) {
		return errors.New("approved script SHA256 is invalid")
	}
	digest := sha256.Sum256(script.Content)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), script.SHA256) {
		return errors.New("approved script content does not match its SHA256")
	}
	for _, argument := range script.Args {
		if strings.ContainsRune(argument, '\x00') {
			return errors.New("approved script argument contains NUL")
		}
	}
	return nil
}

func hasPhase(scripts []model.Script, phase string) bool {
	for _, script := range scripts {
		if script.Phase == phase {
			return true
		}
	}
	return false
}

func sanitize(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			builder.WriteRune(r)
		}
		if builder.Len() >= 40 {
			break
		}
	}
	if builder.Len() == 0 {
		return "release"
	}
	return builder.String()
}

func truncate(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(payload []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(payload)
	}
}
