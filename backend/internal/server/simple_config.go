package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hkjang/releasedock/backend/internal/localexec"
	"github.com/jackc/pgx/v5"
)

// artifactPlaceholder is substituted in configured command arguments with the
// absolute path of the package the user just uploaded.
const artifactPlaceholder = "{{artifact}}"

const (
	commandModePerTarget = "PER_TARGET"
	commandModeShared    = "SHARED"
)

// A post-deployment stage either runs after every uploaded package, or once
// for the whole upload. "Once" means on the last run of the batch, by which
// point every package has been uploaded and every command has finished.
const (
	stageScopeEach = "EACH"
	stageScopeOnce = "ONCE"
)

// A stage that is off leaves NONE, one that was left for another run of the
// same upload leaves SKIPPED, and neither counts as a failure.
const (
	stageStatusNone    = "NONE"
	stageStatusSkipped = "SKIPPED"
	stageStatusSuccess = "SUCCESS"
)

type simpleSettings struct {
	DefaultUIMode        string   `json:"defaultUiMode"`
	CommandMode          string   `json:"commandMode"`
	SharedCommandPath    string   `json:"sharedCommandPath"`
	SharedCommandArgs    []string `json:"sharedCommandArgs"`
	SharedWorkingDir     string   `json:"sharedWorkingDir"`
	SharedTimeoutSeconds int      `json:"sharedTimeoutSeconds"`
	UploadRoot           string   `json:"uploadRoot"`
	// Replication is triggered only after the command succeeds, so a failed
	// deployment never mirrors a half-applied state.
	ReplicationEnabled  bool   `json:"replicationEnabled"`
	ReplicationRegistry string `json:"replicationRegistryId"`
	ReplicationPolicyID int64  `json:"replicationPolicyId"`
	ReplicationPolicy   string `json:"replicationPolicyName"`
	ReplicationTimeout  int    `json:"replicationTimeoutSeconds"`
	ReplicationScope    string `json:"replicationScope"`
	// The application deployment command runs after replication, and only if
	// replication did not fail: deploying an image that was never mirrored is
	// exactly the state this stage exists to avoid.
	AppDeployEnabled bool      `json:"appDeployEnabled"`
	AppDeployScope   string    `json:"appDeployScope"`
	AppDeployPath    string    `json:"appDeployCommandPath"`
	AppDeployArgs    []string  `json:"appDeployCommandArgs"`
	AppDeployDir     string    `json:"appDeployWorkingDir"`
	AppDeployTimeout int       `json:"appDeployTimeoutSeconds"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

func loadSimpleSettings(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (simpleSettings, error) {
	var cfg simpleSettings
	var registryID *string
	err := q.QueryRow(ctx, `SELECT default_ui_mode,command_mode,shared_command_path,shared_command_args,
		shared_working_dir,shared_timeout_seconds,upload_root,replication_enabled,
		replication_registry_id::text,COALESCE(replication_policy_id,0),replication_policy_name,
		replication_timeout_seconds,replication_scope,app_deploy_enabled,app_deploy_scope,
		app_deploy_command_path,app_deploy_command_args,app_deploy_working_dir,
		app_deploy_timeout_seconds,updated_at
		FROM simple_settings WHERE id='default'`).
		Scan(&cfg.DefaultUIMode, &cfg.CommandMode, &cfg.SharedCommandPath, &cfg.SharedCommandArgs,
			&cfg.SharedWorkingDir, &cfg.SharedTimeoutSeconds, &cfg.UploadRoot, &cfg.ReplicationEnabled,
			&registryID, &cfg.ReplicationPolicyID, &cfg.ReplicationPolicy,
			&cfg.ReplicationTimeout, &cfg.ReplicationScope, &cfg.AppDeployEnabled, &cfg.AppDeployScope,
			&cfg.AppDeployPath, &cfg.AppDeployArgs, &cfg.AppDeployDir,
			&cfg.AppDeployTimeout, &cfg.UpdatedAt)
	if registryID != nil {
		cfg.ReplicationRegistry = *registryID
	}
	if cfg.SharedCommandArgs == nil {
		cfg.SharedCommandArgs = []string{}
	}
	if cfg.AppDeployArgs == nil {
		cfg.AppDeployArgs = []string{}
	}
	return cfg, err
}

// stageRuns decides whether a post-deployment stage fires on this run. A stage
// scoped to the whole upload runs on the last run of the batch only, so the
// packages that came before it have all been deployed by then.
func stageRuns(scope string, batchLast bool) bool {
	return scope != stageScopeOnce || batchLast
}

// appDeployStageRuns adds one condition to the application deployment on top
// of its own scope: it waits for replication. A replication deferred to the
// last package of the upload has not mirrored anything yet, so rolling the
// application over now would deploy images the registry never received - the
// very state the stage ordering exists to prevent. The deferred replication
// and this stage then both happen on the last run of the batch.
func appDeployStageRuns(scope string, batchLast bool, replicationStatus string) bool {
	if replicationStatus == stageStatusSkipped {
		return false
	}
	return stageRuns(scope, batchLast)
}

// resolvedCommand is the command a run will actually execute, frozen at the
// moment the run is created so later settings changes cannot rewrite history.
type resolvedCommand struct {
	Source  string
	Path    string
	Args    []string
	Dir     string
	Timeout time.Duration
}

// resolveCommand picks the shared command or the target's own command
// according to the active mode. The mode is read at run time, not at page
// render time, so the displayed configuration can never override the stored
// one.
func resolveCommand(cfg simpleSettings, target simpleTarget) (resolvedCommand, error) {
	if cfg.CommandMode == commandModeShared {
		if cfg.SharedCommandPath == "" {
			return resolvedCommand{}, errors.New("공통 명령이 설정되지 않았습니다")
		}
		dir := cfg.SharedWorkingDir
		if dir == "" {
			dir = target.UploadDir
		}
		return resolvedCommand{
			Source:  commandModeShared,
			Path:    cfg.SharedCommandPath,
			Args:    cfg.SharedCommandArgs,
			Dir:     dir,
			Timeout: time.Duration(cfg.SharedTimeoutSeconds) * time.Second,
		}, nil
	}
	if target.CommandPath == "" {
		return resolvedCommand{}, errors.New("이 대상에 실행할 명령이 설정되지 않았습니다")
	}
	dir := target.WorkingDir
	if dir == "" {
		dir = target.UploadDir
	}
	timeout := target.TimeoutSeconds
	if timeout <= 0 {
		timeout = 600
	}
	return resolvedCommand{
		Source:  commandModePerTarget,
		Path:    target.CommandPath,
		Args:    target.CommandArgs,
		Dir:     dir,
		Timeout: time.Duration(timeout) * time.Second,
	}, nil
}

// expandArgs substitutes the artifact placeholder. Substitution happens after
// argument splitting, so an artifact path can never introduce a new argument
// or a shell construct.
func expandArgs(args []string, artifactPath string) []string {
	expanded := make([]string, len(args))
	for i, arg := range args {
		expanded[i] = strings.ReplaceAll(arg, artifactPlaceholder, artifactPath)
	}
	return expanded
}

// normalizeUploadDir keeps every simple-mode upload directory inside the
// administrator-configured root, which must itself stay under the managed data
// root so the systemd ReadWritePaths for the API service still cover it.
func normalizeUploadDir(root, value string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(value))
	if !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return "", errors.New("업로드 경로는 루트가 아닌 절대 경로여야 합니다")
	}
	cleanRoot := filepath.Clean(root)
	relative, err := filepath.Rel(cleanRoot, clean)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("업로드 경로는 %s 하위여야 합니다", cleanRoot)
	}
	return clean, nil
}

// normalizeUploadRoot bounds the configurable root itself. Allowing an
// arbitrary root would let an administrator point uploads at a directory the
// service unit cannot write, or outside the data volume entirely.
func normalizeUploadRoot(value string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(value))
	if !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return "", errors.New("업로드 루트는 루트가 아닌 절대 경로여야 합니다")
	}
	relative, err := filepath.Rel(managedDataRoot, clean)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("업로드 루트는 /var/lib/releasedock 하위여야 합니다")
	}
	return clean, nil
}

// ensureUploadDir creates the directory on demand and refuses to follow a
// symbolic link, so a replaced directory entry cannot redirect writes.
func ensureUploadDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("업로드 경로를 만들 수 없습니다: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("업로드 경로를 확인할 수 없습니다: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("업로드 경로는 심볼릭 링크일 수 없습니다")
	}
	if !info.IsDir() {
		return errors.New("업로드 경로가 디렉터리가 아닙니다")
	}
	return nil
}

// validateCommandFields applies the same rules to a per-target command and to
// the shared command, so neither path can be configured more loosely.
func validateCommandFields(commandPath string, args []string, workingDir string, timeoutSeconds int) error {
	if err := localexec.ValidateCommandPath(commandPath); err != nil {
		return fmt.Errorf("실행 명령: %w", err)
	}
	if err := localexec.ValidateArgs(args); err != nil {
		return err
	}
	if len(args) > 64 {
		return errors.New("명령 인자는 64개를 넘을 수 없습니다")
	}
	if workingDir != "" {
		if err := localexec.ValidateDir(workingDir); err != nil {
			return fmt.Errorf("작업 디렉터리: %w", err)
		}
	}
	if timeoutSeconds < 1 || timeoutSeconds > 86400 {
		return errors.New("제한 시간은 1초 이상 86400초 이하여야 합니다")
	}
	return nil
}
