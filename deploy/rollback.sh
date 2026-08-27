#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" -ne 0 ]]; then
  echo "rollback.sh must run as root" >&2
  exit 1
fi

if [[ "$#" -ne 1 || ! "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "usage: $0 <installed-version>" >&2
  exit 2
fi

INSTALL_ROOT="/opt/releasedock"
TARGET="${INSTALL_ROOT}/releases/$1"
if [[ ! -x "${TARGET}/bin/releasedock-server" || ! -x "${TARGET}/bin/releasedock-runner" || ! -x "${TARGET}/bin/releasedock-executor" ]]; then
  echo "release is not installed: $1" >&2
  exit 1
fi

LINK_STAGE_DIR="$(mktemp -d "${INSTALL_ROOT}/.rollback-link-staging.XXXXXX")"
cleanup_link() {
  set +e
  if [[ -n "${LINK_STAGE_DIR}" && "${LINK_STAGE_DIR}" == "${INSTALL_ROOT}/.rollback-link-staging."* ]]; then
    rm -f -- "${LINK_STAGE_DIR}/current"
    rmdir -- "${LINK_STAGE_DIR}" 2>/dev/null
  fi
}
trap cleanup_link EXIT
ln -s -- "${TARGET}" "${LINK_STAGE_DIR}/current"
mv -Tf -- "${LINK_STAGE_DIR}/current" "${INSTALL_ROOT}/current"
rmdir -- "${LINK_STAGE_DIR}"
LINK_STAGE_DIR=""
trap - EXIT

systemctl restart releasedock-executor.service releasedock-server releasedock-runner
echo "ReleaseDock rolled back to $1"
