#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="$(tr -d '[:space:]' < "${PROJECT_DIR}/VERSION")"
OUTPUT_DIR="${PROJECT_DIR}/release"
PACKAGE_NAME="releasedock-v${VERSION}"
STAGE_PARENT="$(mktemp -d /tmp/releasedock-package.XXXXXX)"
STAGE_DIR="${STAGE_PARENT}/${PACKAGE_NAME}"
ARCHIVE="${OUTPUT_DIR}/${PACKAGE_NAME}.tar.gz"

cleanup_stage() {
	set +e
	if [[ -n "${STAGE_PARENT}" && "${STAGE_PARENT}" == /tmp/releasedock-package.* ]]; then
		rm -rf -- "${STAGE_PARENT}"
	fi
}
trap cleanup_stage EXIT

"${PROJECT_DIR}/scripts/build.sh"

mkdir -p "${STAGE_DIR}/bin" "${STAGE_DIR}/web" "${STAGE_DIR}/systemd"
install -m 0755 "${PROJECT_DIR}/dist/bin/releasedock-server" "${STAGE_DIR}/bin/releasedock-server"
install -m 0755 "${PROJECT_DIR}/dist/bin/releasedock-runner" "${STAGE_DIR}/bin/releasedock-runner"
install -m 0755 "${PROJECT_DIR}/dist/bin/releasedock-executor" "${STAGE_DIR}/bin/releasedock-executor"
cp -a "${PROJECT_DIR}/dist/web/." "${STAGE_DIR}/web/"
install -m 0755 "${PROJECT_DIR}/deploy/install.sh" "${STAGE_DIR}/install.sh"
install -m 0755 "${PROJECT_DIR}/deploy/rollback.sh" "${STAGE_DIR}/rollback.sh"
install -m 0755 "${PROJECT_DIR}/deploy/releasedock.sh" "${STAGE_DIR}/releasedock.sh"
install -m 0644 "${PROJECT_DIR}/deploy/releasedock.env.example" "${STAGE_DIR}/releasedock.env.example"
install -m 0644 "${PROJECT_DIR}/deploy/releasedock-server.service" "${STAGE_DIR}/systemd/releasedock-server.service"
install -m 0644 "${PROJECT_DIR}/deploy/releasedock-runner.service" "${STAGE_DIR}/systemd/releasedock-runner.service"
install -m 0644 "${PROJECT_DIR}/deploy/releasedock-executor.service" "${STAGE_DIR}/systemd/releasedock-executor.service"
install -m 0644 "${PROJECT_DIR}/deploy/releasedock-executor.socket" "${STAGE_DIR}/systemd/releasedock-executor.socket"
install -m 0644 "${PROJECT_DIR}/VERSION" "${STAGE_DIR}/VERSION"
install -m 0644 "${PROJECT_DIR}/docs/offline-install.md" "${STAGE_DIR}/README.md"

if find "${STAGE_DIR}" -type l -print -quit | grep -q .; then
  echo "release staging directory must not contain symbolic links" >&2
  exit 1
fi
find "${STAGE_DIR}" -type d -exec chmod 0755 {} +
find "${STAGE_DIR}" -type f -exec chmod 0644 {} +
chmod 0755 "${STAGE_DIR}/bin/releasedock-server" "${STAGE_DIR}/bin/releasedock-runner" \
	"${STAGE_DIR}/bin/releasedock-executor" "${STAGE_DIR}/install.sh" "${STAGE_DIR}/rollback.sh" \
	"${STAGE_DIR}/releasedock.sh"

(
  cd "${STAGE_DIR}"
  find . -type f ! -name MANIFEST.sha256 -print0 | sort -z | xargs -0 sha256sum > MANIFEST.sha256
)

mkdir -p "${OUTPUT_DIR}"
SOURCE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "${PROJECT_DIR}" log -1 --format=%ct 2>/dev/null || true)}"
if [[ -z "${SOURCE_EPOCH}" ]]; then
  SOURCE_EPOCH=0
fi
if [[ ! "${SOURCE_EPOCH}" =~ ^[0-9]+$ ]]; then
  echo "SOURCE_DATE_EPOCH must be a non-negative integer" >&2
  exit 1
fi
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@${SOURCE_EPOCH}" -C "${STAGE_PARENT}" -czf "${ARCHIVE}" "${PACKAGE_NAME}"
(
  cd "${OUTPUT_DIR}"
  sha256sum "${PACKAGE_NAME}.tar.gz" > "${PACKAGE_NAME}.tar.gz.sha256"
)

trap - EXIT
cleanup_stage
echo "Created ${ARCHIVE}"
