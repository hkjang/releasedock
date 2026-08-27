#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" -ne 0 ]]; then
  echo "install.sh must run as root" >&2
  exit 1
fi

PACKAGE_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
if [[ ! -f "${PACKAGE_DIR}/MANIFEST.sha256" ]]; then
  echo "MANIFEST.sha256 is missing" >&2
  exit 1
fi
(
  cd "${PACKAGE_DIR}"
  if find . -type l -print -quit | grep -q .; then
    echo "package must not contain symbolic links" >&2
    exit 1
  fi
  if find . ! -type f ! -type d -print -quit | grep -q .; then
    echo "package must contain only regular files and directories" >&2
    exit 1
  fi
  sha256sum --check --strict MANIFEST.sha256

  manifest_list="$(mktemp)"
  actual_list="$(mktemp)"
  trap 'rm -f -- "${manifest_list}" "${actual_list}"' EXIT
  awk '{print $2}' MANIFEST.sha256 | LC_ALL=C sort > "${manifest_list}"
  find . -type f ! -name MANIFEST.sha256 -print | LC_ALL=C sort > "${actual_list}"
  if ! cmp -s "${manifest_list}" "${actual_list}"; then
    echo "package contains files missing from MANIFEST.sha256" >&2
    exit 1
  fi
)
VERSION="$(tr -d '[:space:]' < "${PACKAGE_DIR}/VERSION")"
INSTALL_ROOT="/opt/releasedock"
RELEASE_DIR="${INSTALL_ROOT}/releases/${VERSION}"
CONFIG_DIR="/etc/releasedock"
STATE_DIR="/var/lib/releasedock"
LOG_DIR="/var/log/releasedock"
STAGING_DIR=""
LINK_STAGE_DIR=""

if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "invalid VERSION: ${VERSION}" >&2
  exit 1
fi

cleanup_install() {
  set +e
  if [[ -n "${STAGING_DIR}" && "${STAGING_DIR}" == "${INSTALL_ROOT}/releases/.staging-${VERSION}."* ]]; then
    rm -rf -- "${STAGING_DIR}"
  fi
  if [[ -n "${LINK_STAGE_DIR}" && "${LINK_STAGE_DIR}" == "${INSTALL_ROOT}/.link-staging."* ]]; then
    rm -f -- "${LINK_STAGE_DIR}/current"
    rmdir -- "${LINK_STAGE_DIR}" 2>/dev/null
  fi
}
trap cleanup_install EXIT

install -d -o root -g root -m 0755 "${INSTALL_ROOT}" "${INSTALL_ROOT}/releases"
if [[ -e "${RELEASE_DIR}" || -L "${RELEASE_DIR}" ]]; then
  echo "ReleaseDock ${VERSION} is already installed; refusing an in-place overwrite" >&2
  exit 1
fi
STAGING_DIR="$(mktemp -d "${INSTALL_ROOT}/releases/.staging-${VERSION}.XXXXXX")"
chown root:root "${STAGING_DIR}"
chmod 0755 "${STAGING_DIR}"

if ! getent group releasedock >/dev/null 2>&1; then
	groupadd --system releasedock
fi
if ! getent group releasedock-workspace >/dev/null 2>&1; then
	groupadd --system releasedock-workspace
fi
if ! getent group releasedock-executor-client >/dev/null 2>&1; then
	groupadd --system releasedock-executor-client
fi
if ! id releasedock >/dev/null 2>&1; then
  useradd --system --gid releasedock --home-dir "${STATE_DIR}" --shell /usr/sbin/nologin releasedock
fi
if ! id releasedock-runner >/dev/null 2>&1; then
	useradd --system --gid releasedock-workspace --home-dir "${STATE_DIR}/workspaces" --shell /usr/sbin/nologin releasedock-runner
fi
usermod --home "${STATE_DIR}/workspaces" --shell /usr/sbin/nologin --lock \
  --gid releasedock-workspace releasedock-runner
# Preserve administrator-granted Podman/containerd service groups across an
# upgrade while ensuring the fixed config/socket groups are always present.
usermod -a -G releasedock,releasedock-executor-client releasedock-runner
if ! id releasedock-executor >/dev/null 2>&1; then
	useradd --system --gid releasedock-workspace --home-dir "${STATE_DIR}/workspaces" --shell /usr/sbin/nologin releasedock-executor
fi
# The executor is deliberately stripped of every supplementary group. Its
# primary workspace group is the only filesystem capability it needs; in
# particular it must never inherit releasedock (secret files) or docker.
usermod --home "${STATE_DIR}/workspaces" --shell /usr/sbin/nologin --lock \
  --gid releasedock-workspace --groups '' releasedock-executor
if getent group docker >/dev/null 2>&1; then
  usermod -a -G docker releasedock-runner
fi
if [[ "$(id -u releasedock)" == "$(id -u releasedock-runner)" ||
      "$(id -u releasedock)" == "$(id -u releasedock-executor)" ||
      "$(id -u releasedock-runner)" == "$(id -u releasedock-executor)" ]]; then
  echo "releasedock, releasedock-runner, and releasedock-executor must have distinct UIDs" >&2
  exit 1
fi
if [[ "$(id -nG releasedock-executor)" != "releasedock-workspace" ]]; then
  echo "releasedock-executor must belong only to releasedock-workspace" >&2
  exit 1
fi
IFS=',' read -r -a socket_members <<< "$(getent group releasedock-executor-client | cut -d: -f4)"
for socket_member in "${socket_members[@]}"; do
  if [[ -n "${socket_member}" && "${socket_member}" != "releasedock-runner" ]]; then
    echo "releasedock-executor-client must contain only releasedock-runner" >&2
    exit 1
  fi
done

install -d -o releasedock -g releasedock-workspace -m 0750 "${STATE_DIR}"
install -d -o releasedock -g releasedock -m 0750 "${STATE_DIR}/artifacts" "${LOG_DIR}"
install -d -o releasedock-runner -g releasedock-workspace -m 2750 "${STATE_DIR}/workspaces"
install -d -o root -g releasedock -m 0750 "${CONFIG_DIR}"
install -d -o root -g root -m 0755 "${STAGING_DIR}/bin" "${STAGING_DIR}/web"

install -o root -g root -m 0755 "${PACKAGE_DIR}/bin/releasedock-server" "${STAGING_DIR}/bin/releasedock-server"
install -o root -g root -m 0755 "${PACKAGE_DIR}/bin/releasedock-runner" "${STAGING_DIR}/bin/releasedock-runner"
install -o root -g root -m 0755 "${PACKAGE_DIR}/bin/releasedock-executor" "${STAGING_DIR}/bin/releasedock-executor"
cp -a "${PACKAGE_DIR}/web/." "${STAGING_DIR}/web/"
chown -R root:root "${STAGING_DIR}/web"
find "${STAGING_DIR}/web" -type d -exec chmod 0755 {} +
find "${STAGING_DIR}/web" -type f -exec chmod 0644 {} +
install -o root -g root -m 0644 "${PACKAGE_DIR}/VERSION" "${STAGING_DIR}/VERSION"
install -o root -g root -m 0644 "${PACKAGE_DIR}/README.md" "${STAGING_DIR}/README.md"

if [[ ! -f "${CONFIG_DIR}/releasedock.env" ]]; then
  install -o root -g releasedock -m 0640 "${PACKAGE_DIR}/releasedock.env.example" "${CONFIG_DIR}/releasedock.env"
  echo "Created ${CONFIG_DIR}/releasedock.env; replace every placeholder before starting services."
fi

install -o root -g root -m 0644 "${PACKAGE_DIR}/systemd/releasedock-server.service" /etc/systemd/system/releasedock-server.service
install -o root -g root -m 0644 "${PACKAGE_DIR}/systemd/releasedock-runner.service" /etc/systemd/system/releasedock-runner.service
install -o root -g root -m 0644 "${PACKAGE_DIR}/systemd/releasedock-executor.service" /etc/systemd/system/releasedock-executor.service
install -o root -g root -m 0644 "${PACKAGE_DIR}/systemd/releasedock-executor.socket" /etc/systemd/system/releasedock-executor.socket

if ! mv -Tn -- "${STAGING_DIR}" "${RELEASE_DIR}"; then
  echo "could not publish ReleaseDock ${VERSION}" >&2
  exit 1
fi
if [[ -e "${STAGING_DIR}" ]]; then
  echo "ReleaseDock ${VERSION} appeared during installation; refusing to overwrite it" >&2
  exit 1
fi
STAGING_DIR=""

LINK_STAGE_DIR="$(mktemp -d "${INSTALL_ROOT}/.link-staging.XXXXXX")"
ln -s -- "${RELEASE_DIR}" "${LINK_STAGE_DIR}/current"
mv -Tf -- "${LINK_STAGE_DIR}/current" "${INSTALL_ROOT}/current"
rmdir -- "${LINK_STAGE_DIR}"
LINK_STAGE_DIR=""

systemctl daemon-reload
systemctl enable releasedock-executor.socket releasedock-server.service releasedock-runner.service
systemctl try-restart releasedock-executor.service releasedock-server.service releasedock-runner.service

trap - EXIT

echo "ReleaseDock ${VERSION} installed."
echo "Edit ${CONFIG_DIR}/releasedock.env, then run: systemctl start releasedock-executor.socket releasedock-server releasedock-runner"
