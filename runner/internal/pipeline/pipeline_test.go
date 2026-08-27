package pipeline

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	runnercrypto "github.com/hkjang/releasedock/runner/internal/crypto"
	"github.com/hkjang/releasedock/runner/internal/executor"
	"github.com/hkjang/releasedock/runner/internal/model"
	containerruntime "github.com/hkjang/releasedock/runner/internal/runtime"
)

type fakeRepository struct {
	mu       sync.Mutex
	steps    []model.JobStatus
	terminal model.JobStatus
	failure  string
	logs     bytes.Buffer
	images   []model.ImageRecord
	nextStep int64
}

func setTestCredentialRoot(t *testing.T, processor *Processor) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o710); err != nil {
		t.Fatal(err)
	}
	processor.credentialRoot = root
	processor.runtime = func(config model.RuntimeConfig, _ containerruntime.CommandExecutor) (runtimeClient, error) {
		return fakePipelineRuntime{config: config}, nil
	}
	return root
}

type fakePipelineRuntime struct{ config model.RuntimeConfig }

func (fakePipelineRuntime) Login(context.Context, string, string, containerruntime.Credential, executor.Spec, containerruntime.Output) error {
	return nil
}
func (fakePipelineRuntime) Load(context.Context, string, string, executor.Spec, containerruntime.Output) error {
	return nil
}
func (fakePipelineRuntime) Tag(context.Context, string, string, string, executor.Spec, containerruntime.Output) error {
	return nil
}
func (fakePipelineRuntime) Push(context.Context, string, string, string, executor.Spec, containerruntime.Output) error {
	return nil
}
func (f fakePipelineRuntime) Destination(repository, tag string) (string, error) {
	return strings.TrimSuffix(f.config.RegistryHost, "/") + "/" + strings.Trim(f.config.RegistryProject, "/") + "/" + strings.TrimPrefix(repository, "/") + ":" + tag, nil
}

func (f *fakeRepository) LoadSettings(context.Context) (model.Settings, error) {
	return model.Settings{}, nil
}
func (f *fakeRepository) RecoverStaleJobs(context.Context, time.Duration) (int64, error) {
	return 0, nil
}
func (f *fakeRepository) ClaimJob(context.Context, time.Duration) (*model.Job, error) {
	return nil, nil
}
func (f *fakeRepository) Heartbeat(context.Context, string) error { return nil }
func (f *fakeRepository) BeginStep(_ context.Context, jobID string, status model.JobStatus) (model.Step, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextStep++
	f.steps = append(f.steps, status)
	return model.Step{ID: f.nextStep, JobID: jobID, Name: status, Number: int(f.nextStep)}, nil
}
func (f *fakeRepository) FinishStep(context.Context, model.Step, bool, model.StepResult) error {
	return nil
}

func (f *fakeRepository) AppendLog(_ context.Context, _ string, _ int64, _ string, _ int64, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = f.logs.Write(payload)
	return nil
}
func (f *fakeRepository) RecordImage(_ context.Context, _ string, image model.ImageRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images = append(f.images, image)
	return nil
}
func (f *fakeRepository) FinishJob(_ context.Context, _ string, status model.JobStatus, failure string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminal = status
	f.failure = failure
	return nil
}

type credentialFailureExecutor struct {
	t              *testing.T
	secret         string
	capturedEnv    map[string]string
	credentialPath string
}

func (e *credentialFailureExecutor) Run(_ context.Context, spec executor.Spec) (executor.Result, error) {
	if !spec.Isolated {
		return executor.Result{}, nil
	}
	e.capturedEnv = make(map[string]string, len(spec.Env))
	for key, value := range spec.Env {
		e.capturedEnv[key] = value
		if strings.Contains(value, e.secret) {
			e.t.Errorf("plaintext target credential entered command env %s", key)
		}
	}
	e.credentialPath = spec.Env["RELEASEDOCK_CREDENTIAL_FILE"]
	if spec.Env["RELEASEDOCK_CREDENTIAL_TYPE"] != "TOKEN" || e.credentialPath == "" {
		e.t.Errorf("target credential type/path missing from command spec: %#v", spec.Env)
	}
	info, err := os.Lstat(e.credentialPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o640 {
		e.t.Errorf("Runner handoff mode is not 0640: %v, %v", info, err)
	}
	stored, err := os.ReadFile(e.credentialPath)
	if err != nil || string(stored) != e.secret {
		e.t.Errorf("Runner handoff content mismatch: %v", err)
	}
	middle := len(e.secret) / 2
	_, _ = spec.Stdout.Write([]byte("stdout:" + e.secret[:middle]))
	_, _ = spec.Stdout.Write([]byte(e.secret[middle:] + "\n"))
	_, _ = spec.Stderr.Write([]byte("stderr:" + e.secret + "\n"))
	return executor.Result{ExitCode: -1}, errors.New("simulated executor transport failure")
}

func TestTargetCredentialIsEphemeralAndRedactedOnExecutorFailure(t *testing.T) {
	const (
		secret       = "ultra-sensitive-target-secret"
		credentialID = "00000000-0000-0000-0000-00000000c001"
		version      = 3
	)
	artifactRoot, workspaceRoot := t.TempDir(), t.TempDir()
	packageBytes := releasePackage(t)
	if err := os.WriteFile(filepath.Join(artifactRoot, "release.tar"), packageBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(packageBytes)
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'k'}, 32))
	box, err := runnercrypto.NewSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	aad := "target-credential:" + credentialID + ":v3"
	ciphertext, err := box.Encrypt([]byte(secret), aad)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ciphertext, secret) {
		t.Fatal("encrypted DB value contains plaintext target credential")
	}
	repository := &fakeRepository{}
	commandExecutor := &credentialFailureExecutor{t: t, secret: secret}
	processor, err := New(repository, box, commandExecutor)
	if err != nil {
		t.Fatal(err)
	}
	credentialRoot := setTestCredentialRoot(t, processor)
	job := &model.Job{
		ID: "job-target-credential", ReleaseID: "release-target-credential",
		Application: "crm", Version: "2.4.1", Environment: "PROD",
		ArtifactPath: "release.tar", ExpectedSHA256: hex.EncodeToString(digest[:]),
		ManifestPath: "manifest.yaml", Operation: model.OperationDeploy,
		TargetCredential: model.TargetCredential{
			ID: credentialID, Type: "TOKEN", Version: version, Ciphertext: ciphertext, AAD: aad,
		},
		Profile: model.Profile{
			Extraction: model.ExtractionPolicy{MaxArchiveBytes: 1 << 20, MaxExtractedBytes: 1 << 20, MaxFiles: 100, MaxImages: 5},
			Runtime: model.RuntimeConfig{
				Kind: "docker", BinaryPath: "/usr/sbin/docker", RegistryURL: "https://harbor.invalid",
				RegistryHost: "harbor.invalid", RegistryProject: "crm",
			},
			Scripts:        []model.Script{approvedScript("pre", "PRE_DEPLOY"), approvedScript("deploy", "DEPLOY")},
			CommandTimeout: time.Second, MaxLogBytes: 1 << 20, KeepFailedWorkspace: true,
		},
	}
	settings := model.Settings{
		WorkspaceRoot: workspaceRoot, ArtifactRoot: artifactRoot,
		CommandPath: "/usr/bin:/bin", LogChunkBytes: 1024, HeartbeatInterval: time.Hour,
	}
	if err := processor.Process(context.Background(), settings, job); err == nil {
		t.Fatal("expected simulated isolated executor failure")
	}
	if _, err := os.Stat(commandExecutor.credentialPath); !os.IsNotExist(err) {
		t.Fatalf("Runner credential handoff survived executor error: %v", err)
	}
	entries, err := os.ReadDir(credentialRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("transient credential job directory survived executor error: %v, %#v", err, entries)
	}
	entries, err = os.ReadDir(workspaceRoot)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one retained failed workspace: %v, %#v", err, entries)
	}
	workspace := filepath.Join(workspaceRoot, entries[0].Name())
	for _, directory := range []string{executor.CredentialDirectory, executor.ExecutorCredentialDirectory} {
		if _, err := os.Stat(filepath.Join(workspace, directory)); !os.IsNotExist(err) {
			t.Errorf("managed credential directory %s survived cleanup: %v", directory, err)
		}
	}
	persisted := repository.logs.String() + repository.failure
	if strings.Contains(persisted, secret) {
		t.Fatalf("plaintext target credential reached persisted log/failure: %q", persisted)
	}
	if !strings.Contains(repository.logs.String(), string(targetCredentialRedaction)) {
		t.Fatalf("credential output was not visibly redacted: %q", repository.logs.String())
	}
	for key, value := range commandExecutor.capturedEnv {
		if strings.Contains(key+"="+value, secret) {
			t.Fatalf("plaintext target credential reached serialized command spec: %s=%q", key, value)
		}
	}
}

type successfulExecutor struct {
	mu    sync.Mutex
	specs []executor.Spec
}

func (e *successfulExecutor) Run(_ context.Context, spec executor.Spec) (executor.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.specs = append(e.specs, spec)
	if spec.Stdout != nil {
		_, _ = io.WriteString(spec.Stdout, "ok\n")
	}
	return executor.Result{ExitCode: 0}, nil
}

func TestProcessorRunsEveryStateAndRecordsDigest(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		w.WriteHeader(http.StatusOK)
	}))
	defer registry.Close()

	artifactRoot := t.TempDir()
	workspaceRoot := t.TempDir()
	packageBytes := releasePackage(t)
	artifactName := "crm-2.4.1.tar"
	if err := os.WriteFile(filepath.Join(artifactRoot, artifactName), packageBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(packageBytes)
	preScript := approvedScript("pre", "PRE_DEPLOY")
	deployScript := approvedScript("deploy", "DEPLOY")
	repository := &fakeRepository{}
	commandExecutor := &successfulExecutor{}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'k'}, 32))
	box, err := runnercrypto.NewSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := New(repository, box, commandExecutor)
	if err != nil {
		t.Fatal(err)
	}
	setTestCredentialRoot(t, processor)
	settings := model.Settings{
		WorkspaceRoot: workspaceRoot, ArtifactRoot: artifactRoot,
		CommandPath: "/usr/bin:/bin", LogChunkBytes: 1024,
		HeartbeatInterval: time.Hour,
	}
	job := &model.Job{
		ID: "job-1", ReleaseID: "release-1", Application: "crm", Version: "2.4.1",
		Environment: "DEV", ArtifactPath: artifactName, ExpectedSHA256: hex.EncodeToString(digest[:]),
		ManifestPath: "manifest.yaml", Operation: model.OperationDeploy,
		Profile: model.Profile{
			Extraction: model.ExtractionPolicy{MaxArchiveBytes: 1 << 20, MaxExtractedBytes: 1 << 20, MaxFiles: 100, MaxImages: 5},
			Runtime:    model.RuntimeConfig{Kind: "docker", BinaryPath: "/usr/sbin/docker", RegistryURL: registry.URL, RegistryHost: "harbor.local", RegistryProject: "crm"},
			Scripts:    []model.Script{preScript, deployScript}, CommandTimeout: time.Second,
			MaxLogBytes: 1 << 20, CleanupWorkspace: true,
		},
	}
	if err := processor.Process(context.Background(), settings, job); err != nil {
		t.Fatal(err)
	}
	want := []model.JobStatus{
		model.StatusValidating, model.StatusPreCheck, model.StatusExtracting, model.StatusImageInspect,
		model.StatusImageLoad, model.StatusImageTag, model.StatusImagePush,
		model.StatusDeploying, model.StatusVerifying,
	}
	if len(repository.steps) != len(want) {
		t.Fatalf("unexpected states: %#v", repository.steps)
	}
	for i := range want {
		if repository.steps[i] != want[i] {
			t.Fatalf("state %d = %s, want %s", i, repository.steps[i], want[i])
		}
	}
	if repository.terminal != model.StatusSuccess {
		t.Fatalf("terminal status = %s", repository.terminal)
	}
	if len(repository.images) != 2 || repository.images[1].Digest == "" {
		t.Fatalf("image digest was not recorded: %#v", repository.images)
	}
	isolatedCommands := 0
	for _, spec := range commandExecutor.specs {
		if spec.Isolated {
			isolatedCommands++
		}
	}
	if isolatedCommands != 2 {
		t.Fatalf("approved scripts must be isolated while runtime commands stay local: %#v", commandExecutor.specs)
	}
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("workspace was not cleaned: %v, %#v", err, entries)
	}
}

type failIsolatedExecutor struct{}

func (failIsolatedExecutor) Run(_ context.Context, spec executor.Spec) (executor.Result, error) {
	if spec.Isolated {
		return executor.Result{ExitCode: 23}, errors.New("pre-check rejected package")
	}
	return executor.Result{}, nil
}

func TestPreCheckRunsBeforeExtractionAndDoesNotTriggerRollback(t *testing.T) {
	artifactRoot, workspaceRoot := t.TempDir(), t.TempDir()
	packageBytes := releasePackage(t)
	if err := os.WriteFile(filepath.Join(artifactRoot, "release.tar"), packageBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(packageBytes)
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'k'}, 32))
	box, err := runnercrypto.NewSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	repository := &fakeRepository{}
	processor, err := New(repository, box, failIsolatedExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	setTestCredentialRoot(t, processor)
	job := &model.Job{
		ID: "job-precheck", ReleaseID: "release-precheck", Application: "crm", Version: "2.4.1",
		Environment: "PROD", ArtifactPath: "release.tar", ExpectedSHA256: hex.EncodeToString(digest[:]),
		ManifestPath: "manifest.yaml", Operation: model.OperationDeploy,
		Profile: model.Profile{
			Extraction:     model.ExtractionPolicy{MaxArchiveBytes: 1 << 20, MaxExtractedBytes: 1 << 20, MaxFiles: 100, MaxImages: 5},
			Runtime:        model.RuntimeConfig{Kind: "docker", BinaryPath: "/usr/sbin/docker", RegistryURL: "https://harbor.invalid", RegistryHost: "harbor.invalid", RegistryProject: "crm"},
			Scripts:        []model.Script{approvedScript("pre", "PRE_DEPLOY"), approvedScript("deploy", "DEPLOY"), approvedScript("rollback", "ROLLBACK")},
			CommandTimeout: time.Second, MaxLogBytes: 1 << 20, CleanupWorkspace: true,
		},
	}
	settings := model.Settings{WorkspaceRoot: workspaceRoot, ArtifactRoot: artifactRoot, CommandPath: "/usr/bin:/bin", LogChunkBytes: 1024, HeartbeatInterval: time.Hour}
	if err := processor.Process(context.Background(), settings, job); err == nil {
		t.Fatal("expected pre-check failure")
	}
	want := []model.JobStatus{model.StatusValidating, model.StatusPreCheck}
	if len(repository.steps) != len(want) {
		t.Fatalf("pre-check order or rollback behavior is wrong: %#v", repository.steps)
	}
	for index := range want {
		if repository.steps[index] != want[index] {
			t.Fatalf("step %d = %s, want %s", index, repository.steps[index], want[index])
		}
	}
	if repository.terminal != model.StatusFailed {
		t.Fatalf("pre-check failure must end FAILED without rollback, got %s", repository.terminal)
	}
}

func TestCreateWorkspaceGrantsOnlySharedGroupAccess(t *testing.T) {
	root := t.TempDir()
	workspace, err := createWorkspace(root, "release/../../id")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o770 || info.Mode()&os.ModeSetgid == 0 || info.Mode()&os.ModeSticky == 0 {
		t.Fatalf("workspace mode = %v, want setgid+sticky 0770", info.Mode())
	}
	relative, err := filepath.Rel(root, workspace)
	if err != nil || strings.Contains(relative, string(filepath.Separator)) || !strings.HasPrefix(relative, "job-releaseid-") {
		t.Fatalf("unsafe workspace name %q: %v", relative, err)
	}
}

func TestRejectsUnsafeAutomaticRollback(t *testing.T) {
	job := &model.Job{
		Operation: model.OperationDeploy,
		Profile: model.Profile{
			AutoRollback: true,
			Scripts:      []model.Script{approvedScript("deploy", "DEPLOY"), approvedScript("rollback", "ROLLBACK")},
		},
	}
	err := validateOperation(job)
	if err == nil || !strings.Contains(err.Error(), "automatic rollback is disabled") {
		t.Fatalf("unsafe automatic rollback was accepted: %v", err)
	}
}

func TestDeployPriorHeadSnapshotDoesNotEnterScriptRollbackContext(t *testing.T) {
	processor := &Processor{}
	state := &processState{
		job: &model.Job{
			ID: "job-b", ReleaseID: "release-b", Operation: model.OperationDeploy,
			RollbackSourceReleaseID: "release-a", RollbackSourceJobID: "job-a",
		},
		settings:  model.Settings{CommandPath: "/usr/bin:/bin"},
		workspace: "/var/lib/releasedock/workspaces/job-b-123",
		artifact:  "/var/lib/releasedock/workspaces/job-b-123/release-package",
		extracted: "/var/lib/releasedock/workspaces/job-b-123/package",
	}
	environment := processor.restrictedEnv(state)
	if environment["RELEASEDOCK_ROLLBACK_SOURCE_RELEASE_ID"] != "" || environment["RELEASEDOCK_ROLLBACK_SOURCE_JOB_ID"] != "" {
		t.Fatalf("DEPLOY leaked prior-head snapshot as rollback script context: %#v", environment)
	}
}

func TestProcessorRunsExplicitRollback(t *testing.T) {
	const sourceDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v2/crm/api/manifests/"+sourceDigest) {
			t.Errorf("unexpected rollback Registry request %s", r.URL.Path)
		}
		w.Header().Set("Docker-Content-Digest", sourceDigest)
		w.WriteHeader(http.StatusOK)
	}))
	defer registry.Close()
	artifactRoot, workspaceRoot := t.TempDir(), t.TempDir()
	repository := &fakeRepository{}
	commandExecutor := &successfulExecutor{}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'k'}, 32))
	box, err := runnercrypto.NewSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := New(repository, box, commandExecutor)
	if err != nil {
		t.Fatal(err)
	}
	setTestCredentialRoot(t, processor)
	job := &model.Job{
		ID: "job-manual-rollback", ReleaseID: "release-manual-rollback",
		RollbackSourceReleaseID: "release-source", RollbackSourceJobID: "deploy-source-job",
		Application: "crm", Version: "2.3.0", Environment: "DEV",
		Operation: model.OperationRollback,
		RollbackImages: []model.ImageRecord{{
			FilePath: "images/api.tar", SourceRef: "crm-api:2.3.0",
			DestinationRef: "harbor.local/crm/api:2.3.0", Repository: "api", Tag: "2.3.0", Digest: sourceDigest,
		}},
		Profile: model.Profile{
			Extraction: model.ExtractionPolicy{MaxArchiveBytes: 1 << 20, MaxExtractedBytes: 1 << 20, MaxFiles: 100, MaxImages: 5},
			Runtime: model.RuntimeConfig{Kind: "docker", BinaryPath: "/usr/sbin/docker", RegistryURL: registry.URL,
				RegistryHost: "harbor.local", RegistryProject: "crm"},
			Scripts:        []model.Script{approvedScript("rollback", "ROLLBACK")},
			CommandTimeout: time.Second, MaxLogBytes: 1 << 20, CleanupWorkspace: true,
		},
	}
	settings := model.Settings{WorkspaceRoot: workspaceRoot, ArtifactRoot: artifactRoot,
		CommandPath: "/usr/bin:/bin", LogChunkBytes: 1024, HeartbeatInterval: time.Hour}
	if err := processor.Process(context.Background(), settings, job); err != nil {
		t.Fatal(err)
	}
	if repository.terminal != model.StatusRolledBack {
		t.Fatalf("terminal status = %s, want ROLLED_BACK", repository.terminal)
	}
	wantSteps := []model.JobStatus{model.StatusValidating, model.StatusRollback, model.StatusVerifying}
	if !reflect.DeepEqual(repository.steps, wantSteps) {
		t.Fatalf("manual rollback must skip artifact/runtime pipeline: %#v", repository.steps)
	}
	if len(commandExecutor.specs) != 1 || !commandExecutor.specs[0].Isolated {
		t.Fatalf("manual rollback executed runtime commands: %#v", commandExecutor.specs)
	}
	wantReference := "harbor.local/crm/api@" + sourceDigest
	if commandExecutor.specs[0].Env["RELEASEDOCK_IMAGES"] != wantReference ||
		commandExecutor.specs[0].Env["RELEASEDOCK_ARTIFACT"] != "" ||
		commandExecutor.specs[0].Env["RELEASEDOCK_ROLLBACK_SOURCE_RELEASE_ID"] != "release-source" ||
		commandExecutor.specs[0].Env["RELEASEDOCK_ROLLBACK_SOURCE_JOB_ID"] != "deploy-source-job" {
		t.Fatalf("manual rollback context is not digest-pinned: %#v", commandExecutor.specs[0].Env)
	}
}

func TestExplicitRollbackRejectsMissingSourceImages(t *testing.T) {
	job := &model.Job{
		Operation: model.OperationRollback, RollbackSourceReleaseID: "source-release", RollbackSourceJobID: "source-job",
		Profile: model.Profile{Scripts: []model.Script{approvedScript("rollback", "ROLLBACK")}},
	}
	if err := validateOperation(job); err == nil || !strings.Contains(err.Error(), "image records") {
		t.Fatalf("missing rollback source images were accepted: %v", err)
	}
}

func approvedScript(name, phase string) model.Script {
	content := []byte("#!/bin/sh\nexit 0\n")
	digest := sha256.Sum256(content)
	return model.Script{
		ID: "script-" + name, Name: name, Version: 1, Phase: phase,
		InterpreterPath: "/bin/sh", SHA256: hex.EncodeToString(digest[:]),
		Content: content, Timeout: time.Second, ApprovedAt: time.Now(),
	}
}

func releasePackage(t *testing.T) []byte {
	t.Helper()
	image := imageArchive(t)
	manifestYAML := []byte("application: crm\nversion: 2.4.1\nimages:\n  - file: images/api.tar\n    repository: crm/api\n    tag: 2.4.1\n")
	var output bytes.Buffer
	tw := tar.NewWriter(&output)
	for _, entry := range []struct {
		name string
		data []byte
	}{{"manifest.yaml", manifestYAML}, {"images/api.tar", image}} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(entry.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func imageArchive(t *testing.T) []byte {
	t.Helper()
	metadata := []byte(`[{"RepoTags":["crm-api:2.4.1"]}]`)
	var output bytes.Buffer
	tw := tar.NewWriter(&output)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(metadata))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(metadata); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
