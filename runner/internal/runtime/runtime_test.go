package runtime

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hkjang/releasedock/runner/internal/executor"
	"github.com/hkjang/releasedock/runner/internal/model"
)

type recordingExecutor struct{ specs []executor.Spec }

func (r *recordingExecutor) Run(_ context.Context, spec executor.Spec) (executor.Result, error) {
	r.specs = append(r.specs, spec)
	return executor.Result{}, nil
}

func newTestClient(config model.RuntimeConfig, commandExecutor CommandExecutor) (*Client, error) {
	return newClient(config, commandExecutor, validateRuntimeBinaryPath)
}

func TestLoginUsesIsolatedAuthenticationDirectory(t *testing.T) {
	for _, testCase := range []struct {
		kind, variable, suffix string
	}{
		{kind: "docker", variable: "DOCKER_CONFIG", suffix: ".runtime-auth"},
		{kind: "podman", variable: "REGISTRY_AUTH_FILE", suffix: filepath.Join(".runtime-auth", "auth.json")},
	} {
		t.Run(testCase.kind, func(t *testing.T) {
			recorder := &recordingExecutor{}
			binaryDirectory := "/usr/bin"
			if testCase.kind == "docker" {
				binaryDirectory = "/usr/sbin"
			}
			client, err := newTestClient(model.RuntimeConfig{
				Kind: testCase.kind, BinaryPath: filepath.Join(binaryDirectory, testCase.kind),
				RegistryHost: "harbor.local", RegistryProject: "crm",
			}, recorder)
			if err != nil {
				t.Fatal(err)
			}
			workspace := t.TempDir()
			transientRoot := t.TempDir()
			if err := client.Login(context.Background(), workspace, transientRoot, Credential{
				Username: "robot", Password: "secret",
			}, executor.Spec{Timeout: time.Second, Env: map[string]string{"PATH": "/usr/bin"}}, Output{io.Discard, io.Discard}); err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(transientRoot, testCase.suffix)
			if got := recorder.specs[0].Env[testCase.variable]; got != want {
				t.Fatalf("%s = %q, want %q", testCase.variable, got, want)
			}
			if recorder.specs[0].Env["PATH"] != "/usr/bin" {
				t.Fatal("base environment was not preserved")
			}
		})
	}
}

func TestDockerCommandsUseSeparatedArguments(t *testing.T) {
	recorder := &recordingExecutor{}
	client, err := newTestClient(model.RuntimeConfig{Kind: "docker", BinaryPath: "/usr/sbin/docker", RegistryHost: "harbor.local:8443", RegistryProject: "crm"}, recorder)
	if err != nil {
		t.Fatal(err)
	}
	base := executor.Spec{Timeout: time.Second, Env: map[string]string{}}
	output := Output{io.Discard, io.Discard}
	if err := client.Tag(context.Background(), t.TempDir(), "api:1", "harbor.local:8443/crm/api:1", base, output); err != nil {
		t.Fatal(err)
	}
	want := []string{"tag", "api:1", "harbor.local:8443/crm/api:1"}
	if !reflect.DeepEqual(recorder.specs[0].Args, want) {
		t.Fatalf("arguments were changed: %#v", recorder.specs[0].Args)
	}
}

func TestPodmanUsesJobLocalRegistryTLSOptions(t *testing.T) {
	recorder := &recordingExecutor{}
	caPEM := testCAPEM(t)
	client, err := newTestClient(model.RuntimeConfig{
		Kind: "podman", BinaryPath: "/usr/bin/podman", RegistryURL: "https://harbor.local",
		RegistryHost: "harbor.local", RegistryProject: "crm", RegistryCAPEM: caPEM, RegistryInsecure: true,
	}, recorder)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	transientRoot := t.TempDir()
	base := executor.Spec{Timeout: time.Second, Env: map[string]string{}}
	output := Output{io.Discard, io.Discard}
	if err := client.Login(context.Background(), workspace, transientRoot, Credential{Username: "robot", Password: "secret"}, base, output); err != nil {
		t.Fatal(err)
	}
	if err := client.Push(context.Background(), workspace, transientRoot, "harbor.local/crm/api:1", base, output); err != nil {
		t.Fatal(err)
	}
	certificatesRoot := filepath.Join(transientRoot, ".registry-certs")
	registryCertificates := filepath.Join(certificatesRoot, "harbor.local")
	wantLogin := []string{"login", "--cert-dir", registryCertificates, "--tls-verify=false", "harbor.local", "--username", "robot", "--password-stdin"}
	wantPush := []string{"push", "--cert-dir", registryCertificates, "--tls-verify=false", "harbor.local/crm/api:1"}
	if !reflect.DeepEqual(recorder.specs[0].Args, wantLogin) || !reflect.DeepEqual(recorder.specs[1].Args, wantPush) {
		t.Fatalf("Podman TLS arguments missing: %#v", recorder.specs)
	}
	stored, err := os.ReadFile(filepath.Join(certificatesRoot, "harbor.local", "ca.crt"))
	if err != nil || string(stored) != caPEM {
		t.Fatalf("job-local Podman CA was not written: %v", err)
	}
}

func TestContainerdHostsIncludesCAAndSkipVerify(t *testing.T) {
	caPEM := testCAPEM(t)
	client, err := newTestClient(model.RuntimeConfig{
		Kind: "containerd", BinaryPath: "/usr/bin/ctr", RegistryURL: "https://harbor.local:8443",
		RegistryHost: "harbor.local:8443", RegistryProject: "crm", RegistryCAPEM: caPEM, RegistryInsecure: true,
	}, &recordingExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	transientRoot := t.TempDir()
	if err := client.Login(context.Background(), workspace, transientRoot, Credential{Username: "robot", Password: "secret"}, executor.Spec{Timeout: time.Second}, Output{}); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(transientRoot, ".containerd-hosts", "harbor.local:8443")
	hosts, err := os.ReadFile(filepath.Join(directory, "hosts.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"skip_verify = true", `ca = "` + filepath.Join(directory, "ca.crt") + `"`, "authorization = \"Basic "} {
		if !strings.Contains(string(hosts), expected) {
			t.Fatalf("hosts.toml lacks %q:\n%s", expected, hosts)
		}
	}
	storedCA, err := os.ReadFile(filepath.Join(directory, "ca.crt"))
	if err != nil || string(storedCA) != caPEM {
		t.Fatalf("containerd CA was not written: %v", err)
	}
}

func TestDockerRejectsPerProfileTLSFlags(t *testing.T) {
	for _, config := range []model.RuntimeConfig{
		{Kind: "docker", BinaryPath: "/usr/sbin/docker", RegistryHost: "harbor.local", RegistryProject: "crm", RegistryInsecure: true},
		{Kind: "docker", BinaryPath: "/usr/sbin/docker", RegistryHost: "harbor.local", RegistryProject: "crm", RegistryCAPEM: testCAPEM(t)},
	} {
		if _, err := newTestClient(config, &recordingExecutor{}); err == nil || !strings.Contains(err.Error(), "daemon-managed") {
			t.Fatalf("expected Docker daemon trust error, got %v", err)
		}
	}
}

func TestRuntimeBinaryPathAllowlist(t *testing.T) {
	for _, testCase := range []struct{ kind, path string }{
		{kind: "docker", path: "/usr/bin/sh"},
		{kind: "docker", path: "/opt/docker"},
		{kind: "docker", path: "/usr/bin/../bin/docker"},
		{kind: "podman", path: "/usr/bin/docker"},
		{kind: "containerd", path: "ctr"},
	} {
		if err := validateRuntimeBinaryPath(testCase.kind, testCase.path); err == nil {
			t.Errorf("accepted unsafe %s runtime path %q", testCase.kind, testCase.path)
		}
	}
}

func TestRuntimeBinaryMustBeRootOwnedNonWritableExecutable(t *testing.T) {
	secure := fakeRuntimeFileInfo{mode: 0o755, uid: 0}
	if err := validateRootOwnedMode(secure, false, true); err != nil {
		t.Fatalf("secure binary rejected: %v", err)
	}
	for name, info := range map[string]fakeRuntimeFileInfo{
		"non-root":       {mode: 0o755, uid: 1000},
		"group-writable": {mode: 0o775, uid: 0},
		"world-writable": {mode: 0o757, uid: 0},
		"not-executable": {mode: 0o644, uid: 0},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateRootOwnedMode(info, false, true); err == nil {
				t.Fatal("unsafe runtime binary metadata was accepted")
			}
		})
	}
}

type fakeRuntimeFileInfo struct {
	mode os.FileMode
	uid  uint32
}

func (f fakeRuntimeFileInfo) Name() string       { return "runtime" }
func (f fakeRuntimeFileInfo) Size() int64        { return 1 }
func (f fakeRuntimeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeRuntimeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeRuntimeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeRuntimeFileInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

func testCAPEM(t *testing.T) string {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ReleaseDock test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: encoded}))
}
