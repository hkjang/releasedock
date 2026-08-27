//go:build linux

package executor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	systemdHelperEnv     = "RELEASEDOCK_EXECUTOR_SYSTEMD_HELPER"
	systemdHelperPIDFile = "RELEASEDOCK_EXECUTOR_SYSTEMD_PID_FILE"
	socketHelperEnv      = "RELEASEDOCK_EXECUTOR_SOCKET_HELPER"
	socketHelperMarker   = "RELEASEDOCK_EXECUTOR_SOCKET_MARKER"
)

func TestSystemdUnitsEnforcePerRequestControlGroup(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate executor test source")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	executorUnit := filepath.Join(repositoryRoot, "deploy/releasedock-executor.service")
	assertUnitContains(t, executorUnit,
		"KillMode=control-group", "SendSIGKILL=yes", "ProtectControlGroups=true",
		"RuntimeDirectory=releasedock-executor-private", "RuntimeDirectoryMode=0700",
		"ReadOnlyPaths=-/run/releasedock-credentials", "ProtectProc=invisible",
		"InaccessiblePaths=/etc/releasedock",
		"UnsetEnvironment=POSTGRES_DSN BOOTSTRAP_ADMIN BOOTSTRAP_ADMIN_PASSWORD ENCRYPTION_KEY PORT")
	assertUnitExcludes(t, executorUnit, "EnvironmentFile=")
	runnerUnit := filepath.Join(repositoryRoot, "deploy/releasedock-runner.service")
	assertUnitContains(t, runnerUnit,
		"Requires=releasedock-executor.socket", "After=network-online.target releasedock-server.service releasedock-executor.socket",
		"Group=releasedock-workspace", "SupplementaryGroups=releasedock releasedock-executor-client",
		"RuntimeDirectory=releasedock-credentials", "RuntimeDirectoryMode=0710",
		"UnsetEnvironment=BOOTSTRAP_ADMIN BOOTSTRAP_ADMIN_PASSWORD PORT")
	assertUnitExcludes(t, runnerUnit, "Requires=releasedock-executor.service")
	assertUnitContains(t, filepath.Join(repositoryRoot, "deploy/releasedock-executor.socket"),
		"SocketUser=root", "SocketGroup=releasedock-executor-client", "SocketMode=0660")
}

func TestOfflineInstallPublishesReleasesAndLinksAtomically(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate executor test source")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	installScript := filepath.Join(repositoryRoot, "deploy/install.sh")
	assertUnitContains(t, installScript,
		`mktemp -d "${INSTALL_ROOT}/releases/.staging-${VERSION}.XXXXXX"`,
		`if [[ -e "${RELEASE_DIR}" || -L "${RELEASE_DIR}" ]]`,
		`mv -Tn -- "${STAGING_DIR}" "${RELEASE_DIR}"`,
		`mv -Tf -- "${LINK_STAGE_DIR}/current" "${INSTALL_ROOT}/current"`,
		"usermod -a -G releasedock,releasedock-executor-client releasedock-runner",
		"--gid releasedock-workspace --groups '' releasedock-executor",
		"systemctl try-restart releasedock-executor.service releasedock-server.service releasedock-runner.service")
	assertUnitExcludes(t, installScript, "ln -sfn", `install -d -o root -g root -m 0755 "${RELEASE_DIR}"`,
		`--groups "${runner_supplementary_groups}" releasedock-runner`)
	rollbackScript := filepath.Join(repositoryRoot, "deploy/rollback.sh")
	assertUnitContains(t, rollbackScript,
		`mktemp -d "${INSTALL_ROOT}/.rollback-link-staging.XXXXXX"`,
		`mv -Tf -- "${LINK_STAGE_DIR}/current" "${INSTALL_ROOT}/current"`)
	assertUnitExcludes(t, rollbackScript, "ln -sfn")
}

// This integration regression uses a transient user service when systemd is
// available. Its detached child starts a new session and ignores SIGTERM; the
// service cgroup must still remove it after the main process exits.
func TestSystemdControlGroupKillsDetachedDescendant(t *testing.T) {
	if os.Getenv(systemdHelperEnv) == "1" {
		runDetachedDescendantHelper(t)
		return
	}
	systemdRun, err := exec.LookPath("systemd-run")
	if err != nil {
		t.Skip("systemd-run is unavailable")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is unavailable")
	}
	if err := exec.Command("systemctl", "--user", "show-environment").Run(); err != nil {
		t.Skipf("systemd user manager is unavailable: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "detached.pid")
	unit := fmt.Sprintf("releasedock-executor-cgroup-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	command := exec.Command(systemdRun,
		"--user", "--wait", "--collect", "--service-type=exec",
		"--unit="+unit, "--property=KillMode=control-group",
		"--property=SendSIGKILL=yes", "--property=TimeoutStopSec=1s",
		"--setenv="+systemdHelperEnv+"=1",
		"--setenv="+systemdHelperPIDFile+"="+pidFile,
		executable, "-test.run=^TestSystemdControlGroupKillsDetachedDescendant$",
	)
	output, runErr := command.CombinedOutput()
	// systemd reports a timeout result when a TERM-ignoring descendant needs
	// the final SIGKILL. That non-zero status is expected if the helper ran.
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("run transient executor boundary: %v\n%s", runErr, output)
	}
	rawPID, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("detached executor descendant %d survived service cgroup teardown", pid)
}

func TestSystemdSocketReactivatesExecutorForSequentialRequests(t *testing.T) {
	if os.Getenv(socketHelperEnv) == "1" {
		runSocketActivationHelper(t)
		return
	}
	if err := exec.Command("systemctl", "--user", "show-environment").Run(); err != nil {
		t.Skipf("systemd user manager is unavailable: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	marker := filepath.Join(directory, "activations")
	socketPath := filepath.Join(directory, "executor.sock")
	stem := fmt.Sprintf("releasedock-executor-reactivation-%d-%d", os.Getpid(), time.Now().UnixNano())
	serviceName, socketName := stem+".service", stem+".socket"
	servicePath, socketUnitPath := filepath.Join(directory, serviceName), filepath.Join(directory, socketName)
	service := fmt.Sprintf(`[Unit]
Requires=%s
After=%s

[Service]
Type=simple
Environment=%s=1
Environment=%s=%s
ExecStart=%s -test.run=^TestSystemdSocketReactivatesExecutorForSequentialRequests$
Sockets=%s
KillMode=control-group
`, socketName, socketName, socketHelperEnv, socketHelperMarker, marker, executable, socketName)
	socketUnit := fmt.Sprintf(`[Socket]
ListenStream=%s
SocketMode=0600
Service=%s
RemoveOnStop=true
`, socketPath, serviceName)
	if err := os.WriteFile(servicePath, []byte(service), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socketUnitPath, []byte(socketUnit), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("systemctl", "--user", "link", "--runtime", servicePath, socketUnitPath).CombinedOutput(); err != nil {
		t.Skipf("link transient user units: %v: %s", err, output)
	}
	defer func() {
		_ = exec.Command("systemctl", "--user", "stop", socketName, serviceName).Run()
		_ = exec.Command("systemctl", "--user", "disable", "--runtime", socketName, serviceName).Run()
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	}()
	if output, err := exec.Command("systemctl", "--user", "start", socketName).CombinedOutput(); err != nil {
		t.Fatalf("start executor test socket: %v: %s", err, output)
	}
	for activation := 1; activation <= 2; activation++ {
		connection, err := net.DialTimeout("unix", socketPath, 2*time.Second)
		if err != nil {
			t.Fatalf("activation %d connect: %v", activation, err)
		}
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := connection.Write([]byte{byte(activation)}); err != nil {
			connection.Close()
			t.Fatalf("activation %d write: %v", activation, err)
		}
		var response [1]byte
		if _, err := connection.Read(response[:]); err != nil {
			connection.Close()
			t.Fatalf("activation %d response: %v", activation, err)
		}
		connection.Close()
		if response[0] != byte(activation) {
			t.Fatalf("activation %d response = %d", activation, response[0])
		}
		waitForUserServiceInactive(t, serviceName)
	}
	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(content))
	if len(lines) != 2 || lines[0] == lines[1] {
		t.Fatalf("expected two distinct executor activations, got %q", content)
	}
}

func runSocketActivationHelper(t *testing.T) {
	marker := os.Getenv(socketHelperMarker)
	if marker == "" {
		t.Fatal("socket helper marker is missing")
	}
	file := os.NewFile(3, "systemd-executor-listener")
	if file == nil {
		t.Fatal("socket activation descriptor is missing")
	}
	listener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	var request [1]byte
	if _, err := connection.Read(request[:]); err != nil {
		t.Fatal(err)
	}
	output, err := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := fmt.Fprintln(output, os.Getpid())
	closeErr := output.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal(errors.Join(writeErr, closeErr))
	}
	if _, err := connection.Write(request[:]); err != nil {
		t.Fatal(err)
	}
}

func waitForUserServiceInactive(t *testing.T, service string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		output, _ := exec.Command("systemctl", "--user", "show", "--property=ActiveState", "--value", service).Output()
		if strings.TrimSpace(string(output)) == "inactive" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("one-request service %s did not become inactive", service)
}

func runDetachedDescendantHelper(t *testing.T) {
	pidFile := os.Getenv(systemdHelperPIDFile)
	if pidFile == "" {
		t.Fatal("helper PID file is missing")
	}
	command := exec.Command("setsid", "/bin/sh", "-c", `trap '' TERM; sleep 30 & wait`)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(command.Process.Pid)), 0o600); err != nil {
		_ = command.Process.Kill()
		t.Fatal(err)
	}
}

func assertUnitContains(t *testing.T, path string, values ...string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		if !strings.Contains(string(content), value) {
			t.Errorf("%s does not contain %q", path, value)
		}
	}
}

func assertUnitExcludes(t *testing.T, path string, values ...string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		if strings.Contains(string(content), value) {
			t.Errorf("%s unexpectedly contains %q", path, value)
		}
	}
}
