#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_DIR="${PROJECT_DIR}/dist"
VERSION="$(tr -d '[:space:]' < "${PROJECT_DIR}/VERSION")"
COMMIT="$(git -C "${PROJECT_DIR}" rev-parse --short=12 HEAD 2>/dev/null || true)"
SOURCE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "${PROJECT_DIR}" log -1 --format=%ct 2>/dev/null || true)}"

if [[ -z "${COMMIT}" ]]; then
  COMMIT="unknown"
fi
if [[ -z "${SOURCE_EPOCH}" ]]; then
  SOURCE_EPOCH=0
fi
if [[ ! "${SOURCE_EPOCH}" =~ ^[0-9]+$ ]]; then
  echo "SOURCE_DATE_EPOCH must be a non-negative integer" >&2
  exit 1
fi
BUILD_TIME="$(date -u -d "@${SOURCE_EPOCH}" +'%Y-%m-%dT%H:%M:%SZ')"
if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "invalid VERSION: ${VERSION}" >&2
  exit 1
fi

mkdir -p "${BUILD_DIR}/bin"

# Build the frontend on a native Linux temporary filesystem. This makes npm's
# atomic rename/link behavior reliable when the repository itself is on a
# WSL/DrvFS or network mount, and prevents node_modules from entering dist.
WEB_BUILD_ROOT="$(mktemp -d /tmp/releasedock-web-build.XXXXXX)"
cleanup_web_build() {
	set +e
	if [[ -n "${WEB_BUILD_ROOT}" && "${WEB_BUILD_ROOT}" == /tmp/releasedock-web-build.* ]]; then
		rm -rf -- "${WEB_BUILD_ROOT}"
	fi
}
trap cleanup_web_build EXIT
install -m 0644 "${PROJECT_DIR}/web/index.html" "${PROJECT_DIR}/web/package.json" \
	"${PROJECT_DIR}/web/package-lock.json" "${PROJECT_DIR}/web/tsconfig.json" \
	"${PROJECT_DIR}/web/tsconfig.app.json" "${PROJECT_DIR}/web/tsconfig.node.json" \
	"${PROJECT_DIR}/web/vite.config.ts" "${WEB_BUILD_ROOT}/"
cp -a "${PROJECT_DIR}/web/src" "${WEB_BUILD_ROOT}/src"
if [[ -d "${PROJECT_DIR}/web/public" ]]; then
	cp -a "${PROJECT_DIR}/web/public" "${WEB_BUILD_ROOT}/public"
fi

(
  cd "${WEB_BUILD_ROOT}"
  npm ci
  npm test -- --run
  VITE_RELEASEDOCK_VERSION="${VERSION}" npm run build
)

(
  cd "${PROJECT_DIR}/backend"
  go test ./...
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
    -o "${BUILD_DIR}/bin/releasedock-server" ./cmd/server
)

(
  cd "${PROJECT_DIR}/runner"
  go test ./...
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.builtAt=${BUILD_TIME}" \
		-o "${BUILD_DIR}/bin/releasedock-runner" ./cmd/runner
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.builtAt=${BUILD_TIME}" \
		-o "${BUILD_DIR}/bin/releasedock-executor" ./cmd/executor
)

rm -rf "${BUILD_DIR}/web"
cp -a "${WEB_BUILD_ROOT}/dist" "${BUILD_DIR}/web"
printf '%s\n' "${VERSION}" > "${BUILD_DIR}/VERSION"

trap - EXIT
cleanup_web_build
echo "Built ReleaseDock ${VERSION} in ${BUILD_DIR}"
