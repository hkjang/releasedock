package runtime

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hkjang/releasedock/runner/internal/executor"
	"github.com/hkjang/releasedock/runner/internal/model"
)

type CommandExecutor interface {
	Run(context.Context, executor.Spec) (executor.Result, error)
}

type Credential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type Output struct {
	Stdout io.Writer
	Stderr io.Writer
}

type Client struct {
	config model.RuntimeConfig
	exec   CommandExecutor
}

func New(config model.RuntimeConfig, commandExecutor CommandExecutor) (*Client, error) {
	return newClient(config, commandExecutor, validateRuntimeBinary)
}

func newClient(config model.RuntimeConfig, commandExecutor CommandExecutor, binaryValidator func(string, string) error) (*Client, error) {
	if commandExecutor == nil {
		return nil, errors.New("command executor is required")
	}
	if config.Kind != "docker" && config.Kind != "podman" && config.Kind != "containerd" {
		return nil, fmt.Errorf("unsupported runtime %q", config.Kind)
	}
	if binaryValidator == nil {
		return nil, errors.New("runtime binary validator is required")
	}
	if err := binaryValidator(config.Kind, config.BinaryPath); err != nil {
		return nil, err
	}
	if _, err := registryHost(config.RegistryHost); err != nil {
		return nil, err
	}
	if config.RegistryCAPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(config.RegistryCAPEM)) {
			return nil, errors.New("registry CA PEM contains no certificates")
		}
	}
	if config.Kind == "docker" && (config.RegistryInsecure || config.RegistryCAPEM != "") {
		return nil, errors.New("Docker registry TLS trust is daemon-managed; install the CA or insecure-registry policy on the Docker host, configure OS trust for digest verification, then leave profile registryInsecure and registryCaPem unset")
	}
	return &Client{config: config, exec: commandExecutor}, nil
}

var runtimeBinaryNames = map[string]string{
	"docker": "docker", "podman": "podman", "containerd": "ctr",
}

var runtimeBinaryDirectories = map[string]struct{}{
	"/usr/bin": {}, "/usr/local/bin": {}, "/usr/sbin": {}, "/usr/local/sbin": {},
}

// validateRuntimeBinary prevents an administrator-controlled profile from
// turning Runner's container-runtime capability into arbitrary command
// execution. The configured path must be one exact well-known runtime name in
// a root-controlled system directory. A root-owned symlink is accepted only
// when its resolved target is itself a secure regular executable.
func validateRuntimeBinary(kind, path string) error {
	if err := validateRuntimeBinaryPath(kind, path); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	directoryInfo, err := os.Stat(directory)
	if err != nil {
		return fmt.Errorf("inspect runtime binary directory: %w", err)
	}
	if err := validateRootOwnedMode(directoryInfo, true, false); err != nil {
		return fmt.Errorf("runtime binary directory is unsafe: %w", err)
	}

	linkInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("runtime binary %s is unavailable: %w", path, err)
	}
	if err != nil {
		return fmt.Errorf("inspect runtime binary: %w", err)
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		if err := validateRootOwner(linkInfo); err != nil {
			return fmt.Errorf("runtime binary symlink is unsafe: %w", err)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve runtime binary symlink: %w", err)
		}
		if !filepath.IsAbs(resolved) {
			return errors.New("runtime binary symlink did not resolve to an absolute path")
		}
		if filepath.Base(resolved) != runtimeBinaryNames[kind] {
			return errors.New("runtime binary symlink target name is invalid")
		}
		if _, ok := runtimeBinaryDirectories[filepath.Dir(resolved)]; !ok {
			return errors.New("runtime binary symlink target escapes the allowed system directories")
		}
		resolvedDirectoryInfo, err := os.Stat(filepath.Dir(resolved))
		if err != nil {
			return fmt.Errorf("inspect runtime binary symlink target directory: %w", err)
		}
		if err := validateRootOwnedMode(resolvedDirectoryInfo, true, false); err != nil {
			return fmt.Errorf("runtime binary symlink target directory is unsafe: %w", err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect resolved runtime binary: %w", err)
	}
	if err := validateRootOwnedMode(info, false, true); err != nil {
		return fmt.Errorf("runtime binary is unsafe: %w", err)
	}
	return nil
}

func validateRuntimeBinaryPath(kind, path string) error {
	expectedName, ok := runtimeBinaryNames[kind]
	if !ok {
		return fmt.Errorf("unsupported runtime %q", kind)
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != expectedName {
		return fmt.Errorf("%s runtime binary must be an exact absolute path ending in %s", kind, expectedName)
	}
	directory := filepath.Dir(path)
	if _, ok := runtimeBinaryDirectories[directory]; !ok {
		return errors.New("runtime binary directory must be /usr/bin, /usr/local/bin, /usr/sbin, or /usr/local/sbin")
	}
	return nil
}

func validateRootOwnedMode(info os.FileInfo, directory, executable bool) error {
	if directory && !info.IsDir() {
		return errors.New("path is not a directory")
	}
	if !directory && !info.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}
	if err := validateRootOwner(info); err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("path is group- or world-writable")
	}
	if executable && info.Mode().Perm()&0o111 == 0 {
		return errors.New("runtime binary is not executable")
	}
	return nil
}

func validateRootOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return errors.New("path is not owned by root")
	}
	return nil
}

func (c *Client) Login(ctx context.Context, workspace, transientRoot string, credential Credential, timeoutSpec executor.Spec, output Output) error {
	if credential.Username == "" || credential.Password == "" {
		if credential.Username != "" || credential.Password != "" {
			return errors.New("registry username and password must both be present")
		}
	}
	if c.config.Kind == "containerd" {
		return c.writeContainerdHosts(transientRoot, credential)
	}
	podmanTLS, err := c.podmanTLSArguments(transientRoot)
	if err != nil {
		return err
	}
	if credential.Username == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(transientRoot, ".runtime-auth"), 0o700); err != nil {
		return fmt.Errorf("create runtime authentication directory: %w", err)
	}
	spec := timeoutSpec
	spec.Env = c.withAuthEnv(transientRoot, timeoutSpec.Env)
	spec.Path = c.config.BinaryPath
	spec.Args = append([]string{"login"}, podmanTLS...)
	spec.Args = append(spec.Args, c.config.RegistryHost, "--username", credential.Username, "--password-stdin")
	spec.Dir = workspace
	spec.Stdin = append([]byte(credential.Password), '\n')
	spec.Stdout, spec.Stderr = output.Stdout, output.Stderr
	_, err = c.exec.Run(ctx, spec)
	clear(spec.Stdin)
	if err != nil {
		return fmt.Errorf("registry login: %w", err)
	}
	return nil
}

func (c *Client) Load(ctx context.Context, workspace, imageTar string, base executor.Spec, output Output) error {
	args := []string{"load", "--input", imageTar}
	if c.config.Kind == "containerd" {
		args = append(c.containerdPrefix(), "images", "import", imageTar)
	}
	return c.run(ctx, workspace, args, base, output, nil, "load image")
}

func (c *Client) Tag(ctx context.Context, workspace, source, destination string, base executor.Spec, output Output) error {
	if source == "" || destination == "" {
		return errors.New("source and destination image references are required")
	}
	args := []string{"tag", source, destination}
	if c.config.Kind == "containerd" {
		args = append(c.containerdPrefix(), "images", "tag", source, destination)
	}
	return c.run(ctx, workspace, args, base, output, nil, "tag image")
}

func (c *Client) Push(ctx context.Context, workspace, transientRoot, destination string, base executor.Spec, output Output) error {
	args := []string{"push"}
	if c.config.Kind == "containerd" {
		args = append(c.containerdPrefix(), "images", "push")
		hostsRoot := filepath.Join(transientRoot, ".containerd-hosts")
		if _, err := os.Stat(hostsRoot); err == nil {
			args = append(args, "--hosts-dir", hostsRoot)
		}
	} else if c.config.Kind == "podman" {
		tlsArguments, err := c.podmanTLSArguments(transientRoot)
		if err != nil {
			return err
		}
		args = append(args, tlsArguments...)
	}
	args = append(args, destination)
	return c.runWithTransient(ctx, workspace, transientRoot, args, base, output, nil, "push image")
}

func (c *Client) Destination(repository, tag string) (string, error) {
	if strings.ContainsAny(repository+tag, " \t\r\n@") || repository == "" || tag == "" {
		return "", errors.New("invalid repository or tag")
	}
	project := strings.Trim(c.config.RegistryProject, "/")
	if project == "" || strings.Contains(project, "..") {
		return "", errors.New("registry project is invalid")
	}
	return c.config.RegistryHost + "/" + project + "/" + strings.TrimPrefix(repository, "/") + ":" + tag, nil
}

func (c *Client) run(ctx context.Context, workspace string, args []string, base executor.Spec, output Output, stdin []byte, action string) error {
	return c.runWithTransient(ctx, workspace, "", args, base, output, stdin, action)
}

func (c *Client) runWithTransient(ctx context.Context, workspace, transientRoot string, args []string, base executor.Spec, output Output, stdin []byte, action string) error {
	spec := base
	if transientRoot == "" {
		spec.Env = cloneEnvironment(base.Env)
	} else {
		spec.Env = c.withAuthEnv(transientRoot, base.Env)
	}
	spec.Path = c.config.BinaryPath
	spec.Args = append([]string(nil), args...)
	spec.Dir = workspace
	spec.Stdin = stdin
	spec.Stdout, spec.Stderr = output.Stdout, output.Stderr
	_, err := c.exec.Run(ctx, spec)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return nil
}

func cloneEnvironment(base map[string]string) map[string]string {
	environment := make(map[string]string, len(base))
	for key, value := range base {
		environment[key] = value
	}
	return environment
}

// withAuthEnv keeps registry credentials in a per-job directory under /run.
// systemd and the pipeline both remove it even when a workspace is retained.
func (c *Client) withAuthEnv(transientRoot string, base map[string]string) map[string]string {
	environment := make(map[string]string, len(base)+1)
	for key, value := range base {
		environment[key] = value
	}
	authRoot := filepath.Join(transientRoot, ".runtime-auth")
	if c.config.Kind == "docker" {
		environment["DOCKER_CONFIG"] = authRoot
	} else if c.config.Kind == "podman" {
		environment["REGISTRY_AUTH_FILE"] = filepath.Join(authRoot, "auth.json")
	}
	return environment
}

func (c *Client) containerdPrefix() []string {
	if c.config.Namespace == "" {
		return nil
	}
	return []string{"--namespace", c.config.Namespace}
}

func (c *Client) podmanTLSArguments(transientRoot string) ([]string, error) {
	if c.config.Kind != "podman" {
		return nil, nil
	}
	arguments := make([]string, 0, 3)
	if c.config.RegistryCAPEM != "" {
		certificatesRoot := filepath.Join(transientRoot, ".registry-certs")
		hostDirectory := filepath.Join(certificatesRoot, c.config.RegistryHost)
		if err := os.MkdirAll(hostDirectory, 0o700); err != nil {
			return nil, fmt.Errorf("create Podman registry certificate directory: %w", err)
		}
		if err := os.WriteFile(filepath.Join(hostDirectory, "ca.crt"), []byte(c.config.RegistryCAPEM), 0o600); err != nil {
			return nil, fmt.Errorf("write Podman registry CA: %w", err)
		}
		// Podman's explicit --cert-dir is the directory containing *.crt,
		// unlike the implicit /etc/containers/certs.d per-host root.
		arguments = append(arguments, "--cert-dir", hostDirectory)
	}
	if c.config.RegistryInsecure {
		arguments = append(arguments, "--tls-verify=false")
	}
	return arguments, nil
}

func (c *Client) writeContainerdHosts(transientRoot string, credential Credential) error {
	host, err := registryHost(c.config.RegistryHost)
	if err != nil {
		return err
	}
	directory := filepath.Join(transientRoot, ".containerd-hosts", host)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create containerd hosts directory: %w", err)
	}
	serverURL := c.config.RegistryURL
	var content strings.Builder
	_, _ = fmt.Fprintf(&content, "server = %q\n\n[host.%q]\n  capabilities = [\"pull\", \"resolve\", \"push\"]\n", serverURL, serverURL)
	if c.config.RegistryInsecure {
		content.WriteString("  skip_verify = true\n")
	}
	if c.config.RegistryCAPEM != "" {
		caPath := filepath.Join(directory, "ca.crt")
		if err := os.WriteFile(caPath, []byte(c.config.RegistryCAPEM), 0o600); err != nil {
			return fmt.Errorf("write containerd registry CA: %w", err)
		}
		_, _ = fmt.Fprintf(&content, "  ca = %q\n", caPath)
	}
	if credential.Username != "" {
		authorization := base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Password))
		_, _ = fmt.Fprintf(&content, "  [host.%q.header]\n    authorization = \"Basic %s\"\n", serverURL, authorization)
	}
	path := filepath.Join(directory, "hosts.toml")
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		return fmt.Errorf("write containerd hosts config: %w", err)
	}
	return nil
}

func registryHost(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "/?#@ \t\r\n") {
		return "", errors.New("registry_host must contain only a host and optional port")
	}
	parsed, err := url.Parse("//" + value)
	if err != nil || parsed.Host != value || parsed.Hostname() == "" {
		return "", errors.New("registry_host is invalid")
	}
	return value, nil
}
