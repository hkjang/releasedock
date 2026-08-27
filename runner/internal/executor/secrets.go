package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DefaultCredentialRoot is a systemd RuntimeDirectory backed by /run. It is
	// owned by Runner and is only traversable (not listable) by the executor's
	// workspace group.
	DefaultCredentialRoot = "/run/releasedock-credentials"
	// DefaultExecutorCredentialRoot is a private, per-activation systemd
	// RuntimeDirectory. systemd removes it when the one-request executor exits.
	DefaultExecutorCredentialRoot = "/run/releasedock-executor-private"
	RunnerHostLockFile            = ".runner.lock"
)

var legacyWorkspaceSecretDirectories = []string{
	".containerd-hosts",
	".registry-certs",
	".runtime-auth",
	CredentialDirectory,
	ExecutorCredentialDirectory,
}

// CredentialJobDirectory returns the one fixed per-job path below the Runner
// RuntimeDirectory. Job identifiers are deliberately not sanitized: rejecting
// unexpected characters prevents two identifiers from aliasing one path.
func CredentialJobDirectory(root, jobID string) (string, error) {
	if !validCredentialJobID(jobID) {
		return "", errors.New("credential handoff job ID contains unsupported characters")
	}
	return filepath.Join(filepath.Clean(root), "job-"+jobID), nil
}

func validCredentialJobID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

// PrepareRunnerCredentialRoot validates the systemd-created tmpfs directory
// and removes direct per-job remnants before Runner starts polling. This is the
// SIGKILL/restart recovery path; a power cycle clears /run itself.
func PrepareRunnerCredentialRoot(root string) error {
	if err := validateOwnedDirectory(root, uint32(os.Geteuid()), uint32(os.Getegid()), 0o710, false); err != nil {
		return fmt.Errorf("validate Runner credential RuntimeDirectory: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read Runner credential RuntimeDirectory: %w", err)
	}
	var result error
	for _, entry := range entries {
		if entry.Name() == RunnerHostLockFile {
			if err := validateRunnerLockFile(filepath.Join(root, entry.Name())); err != nil {
				result = errors.Join(result, err)
			}
			continue
		}
		if !strings.HasPrefix(entry.Name(), "job-") || !validDirectName(entry.Name()) {
			result = errors.Join(result, fmt.Errorf("unexpected entry %q in Runner credential RuntimeDirectory", entry.Name()))
			continue
		}
		if err := removeDirectChild(root, entry.Name()); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func validateRunnerLockFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Runner host lock must be a regular non-symlink file")
	}
	owner, err := ownerUID(info)
	if err != nil || owner != uint32(os.Geteuid()) || info.Mode().Perm() != 0o600 {
		return errors.New("Runner host lock ownership or permissions are invalid")
	}
	group, err := ownerGID(info)
	if err != nil || group != uint32(os.Getegid()) {
		return errors.New("Runner host lock group is invalid")
	}
	return nil
}

// CreateCredentialJobDirectory creates the only shared secret-bearing
// directory for a job. The workspace group gets execute-only access so an
// executor can open the exact handoff path without enumerating other jobs.
func CreateCredentialJobDirectory(root, jobID string) (string, error) {
	if err := validateOwnedDirectory(root, uint32(os.Geteuid()), uint32(os.Getegid()), 0o710, false); err != nil {
		return "", fmt.Errorf("validate Runner credential RuntimeDirectory: %w", err)
	}
	directory, err := CredentialJobDirectory(root, jobID)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(directory, 0o710); err != nil {
		return "", fmt.Errorf("create job credential RuntimeDirectory: %w", err)
	}
	if err := os.Chmod(directory, 0o710|os.ModeSetgid); err != nil {
		_ = os.Remove(directory)
		return "", fmt.Errorf("secure job credential RuntimeDirectory: %w", err)
	}
	return directory, nil
}

// RemoveCredentialJobDirectory accepts only the deterministic direct child
// for jobID and never removes the RuntimeDirectory root.
func RemoveCredentialJobDirectory(root, jobID string) error {
	directory, err := CredentialJobDirectory(root, jobID)
	if err != nil {
		return err
	}
	return removeDirectChild(filepath.Clean(root), filepath.Base(directory))
}

// PrepareExecutorCredentialRoot validates and clears the executor's private
// RuntimeDirectory on every activation. It complements systemd's automatic
// RuntimeDirectory cleanup for abrupt termination.
func PrepareExecutorCredentialRoot(root string) error {
	if err := validateOwnedDirectory(root, uint32(os.Geteuid()), uint32(os.Getegid()), 0o700, false); err != nil {
		return fmt.Errorf("validate executor credential RuntimeDirectory: %w", err)
	}
	return clearDirectChildren(root)
}

// ScavengeWorkspaceSecrets removes only known legacy/transient directories
// below strictly validated direct job workspaces. It never follows a top-level
// symlink. This clears registry auth and old credential handoffs left by a
// pre-tmpfs Runner after SIGKILL.
func ScavengeWorkspaceSecrets(root string) error {
	if err := validateOwnedDirectory(root, uint32(os.Geteuid()), uint32(os.Getegid()), 0o750, true); err != nil {
		return fmt.Errorf("validate workspace root for secret scavenging: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read workspace root for secret scavenging: %w", err)
	}
	var result error
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "job-") {
			continue
		}
		if !validDirectName(entry.Name()) {
			result = errors.Join(result, fmt.Errorf("invalid job workspace entry %q", entry.Name()))
			continue
		}
		workspace := filepath.Join(root, entry.Name())
		if err := validateOwnedDirectory(workspace, uint32(os.Geteuid()), uint32(os.Getegid()), 0o770, true); err != nil {
			result = errors.Join(result, fmt.Errorf("validate job workspace %q: %w", entry.Name(), err))
			continue
		}
		info, err := os.Lstat(workspace)
		if err != nil || info.Mode()&os.ModeSticky == 0 {
			result = errors.Join(result, fmt.Errorf("job workspace %q is not sticky", entry.Name()))
			continue
		}
		for _, name := range legacyWorkspaceSecretDirectories {
			if err := removeDirectChild(workspace, name); err != nil {
				result = errors.Join(result, fmt.Errorf("scavenge %s/%s: %w", entry.Name(), name, err))
			}
		}
	}
	return result
}

func validateOwnedDirectory(path string, uid, gid uint32, permissions os.FileMode, setgid bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return errors.New("path must be an absolute non-root path")
	}
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path must be a real directory")
	}
	real, err := filepath.EvalSymlinks(clean)
	if err != nil || real != clean {
		return errors.New("directory path cannot contain symlinks")
	}
	owner, err := ownerUID(info)
	if err != nil || owner != uid {
		return errors.New("directory owner is invalid")
	}
	group, err := ownerGID(info)
	if err != nil || group != gid {
		return errors.New("directory group is invalid")
	}
	if info.Mode().Perm() != permissions || (info.Mode()&os.ModeSetgid != 0) != setgid {
		return errors.New("directory permissions are invalid")
	}
	return nil
}

func clearDirectChildren(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		if !validDirectName(entry.Name()) {
			result = errors.Join(result, fmt.Errorf("invalid direct child %q", entry.Name()))
			continue
		}
		if err := removeDirectChild(root, entry.Name()); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func removeDirectChild(root, name string) error {
	if !validDirectName(name) {
		return fmt.Errorf("refusing to remove invalid direct child %q", name)
	}
	path := filepath.Join(filepath.Clean(root), name)
	relative, err := filepath.Rel(filepath.Clean(root), path)
	if err != nil || relative != name || strings.Contains(relative, string(filepath.Separator)) {
		return fmt.Errorf("refusing to remove path outside managed root: %q", path)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove managed path %q: %w", path, err)
	}
	return nil
}

func validDirectName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsRune(name, filepath.Separator)
}
