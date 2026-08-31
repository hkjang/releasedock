#!/usr/bin/env bash
# ReleaseDock standalone control script.
#
# Runs ReleaseDock without systemd: start, stop, restart, status, logs, doctor.
# Intended for the simple-mode use case and for evaluation, where the API
# process alone is enough. See the isolation note in `doctor` output before
# using it for the full release pipeline.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# Works both from a source checkout (deploy/) and from an unpacked release,
# where the binaries sit next to this script.
if [[ -x "${SCRIPT_DIR}/bin/releasedock-server" ]]; then
  ROOT_DIR="${SCRIPT_DIR}"
elif [[ -f "${SCRIPT_DIR}/../scripts/build.sh" ]]; then
  # Source checkout: keep run/ and logs/ inside the ignored dist tree even
  # before a build has produced the binaries.
  ROOT_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)/dist"
elif [[ -x "/opt/releasedock/current/bin/releasedock-server" ]]; then
  ROOT_DIR="/opt/releasedock/current"
else
  ROOT_DIR="${RELEASEDOCK_ROOT:-${SCRIPT_DIR}}"
fi
ROOT_DIR="${RELEASEDOCK_ROOT:-${ROOT_DIR}}"

BIN_DIR="${ROOT_DIR}/bin"
RUN_DIR="${RELEASEDOCK_RUN_DIR:-${ROOT_DIR}/run}"
LOG_DIR="${RELEASEDOCK_LOG_DIR:-${ROOT_DIR}/logs}"
SERVICES=(server runner)

# The env file is the single place operators put configuration, matching the
# systemd EnvironmentFile so the two modes stay interchangeable.
find_env_file() {
  if [[ -n "${RELEASEDOCK_ENV_FILE:-}" ]]; then
    printf '%s\n' "${RELEASEDOCK_ENV_FILE}"
    return
  fi
  for candidate in "${ROOT_DIR}/releasedock.env" "/etc/releasedock/releasedock.env"; do
    if [[ -f "${candidate}" ]]; then
      printf '%s\n' "${candidate}"
      return
    fi
  done
  printf '%s\n' "${ROOT_DIR}/releasedock.env"
}
ENV_FILE="$(find_env_file)"

RED=''; GREEN=''; YELLOW=''; BLUE=''; BOLD=''; RESET=''
if [[ -t 1 ]]; then
  RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; BLUE=$'\033[34m'; BOLD=$'\033[1m'; RESET=$'\033[0m'
fi

ok()   { printf '  %s✔%s %s\n' "${GREEN}" "${RESET}" "$1"; }
warn() { printf '  %s!%s %s\n' "${YELLOW}" "${RESET}" "$1"; WARN_COUNT=$((WARN_COUNT + 1)); }
bad()  { printf '  %s✘%s %s\n' "${RED}" "${RESET}" "$1"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
info() { printf '  %s·%s %s\n' "${BLUE}" "${RESET}" "$1"; }
head1() { printf '\n%s%s%s\n' "${BOLD}" "$1" "${RESET}"; }
die()  { printf '%serror:%s %s\n' "${RED}" "${RESET}" "$1" >&2; exit 1; }

pid_file() { printf '%s/%s.pid\n' "${RUN_DIR}" "$1"; }
log_file() { printf '%s/%s.log\n' "${LOG_DIR}" "$1"; }

valid_service() {
  local candidate="$1"
  for service in "${SERVICES[@]}"; do
    [[ "${service}" == "${candidate}" ]] && return 0
  done
  return 1
}

# identity_ok verifies a pid really is our binary before we trust or signal it.
# Linux truncates /proc/<pid>/comm to 15 characters, so "releasedock-server"
# can never match there; the executable link and the full command line are the
# only reliable checks.
identity_ok() {
  local pid="$1" binary="$2" exe target
  target="$(readlink -f "${binary}" 2>/dev/null || printf '%s' "${binary}")"
  exe="$(readlink -f "/proc/${pid}/exe" 2>/dev/null || true)"
  [[ -n "${exe}" && "${exe}" == "${target}" ]] && return 0
  ps -o args= -p "${pid}" 2>/dev/null | grep -qF -- "${binary}"
}

# Reads the recorded pid only when that pid is still one of our processes, so a
# reused pid can never be signalled by mistake.
running_pid() {
  local service="$1" file pid
  file="$(pid_file "${service}")"
  [[ -f "${file}" ]] || return 1
  pid="$(tr -d '[:space:]' < "${file}" 2>/dev/null || true)"
  [[ "${pid}" =~ ^[0-9]+$ ]] || return 1
  kill -0 "${pid}" 2>/dev/null || return 1
  identity_ok "${pid}" "${BIN_DIR}/releasedock-${service}" || return 1
  printf '%s\n' "${pid}"
}

load_env() {
  [[ -f "${ENV_FILE}" ]] || die "환경 파일이 없습니다: ${ENV_FILE}"
  set -a
  # shellcheck disable=SC1090
  source "${ENV_FILE}"
  set +a
}

systemd_active() {
  command -v systemctl >/dev/null 2>&1 || return 1
  systemctl is-active --quiet "releasedock-$1.service" 2>/dev/null
}

start_service() {
  local service="$1" binary pid
  binary="${BIN_DIR}/releasedock-${service}"
  [[ -x "${binary}" ]] || die "실행 파일이 없습니다: ${binary}"
  if pid="$(running_pid "${service}")"; then
    info "${service} 이미 실행 중입니다 (pid ${pid})"
    return 0
  fi
  if systemd_active "${service}"; then
    die "systemd의 releasedock-${service}.service가 이미 실행 중입니다. 중복 기동은 DB 잠금과 충돌을 일으킵니다. 'systemctl stop releasedock-${service}' 후 다시 시도하십시오."
  fi
  mkdir -p "${RUN_DIR}" "${LOG_DIR}"
  # nohup keeps the process alive past this shell without an intervening
  # fork, so $! really is the binary. setsid would fork and leave us
  # holding a pid that exits immediately.
  nohup "${binary}" >>"$(log_file "${service}")" 2>&1 &
  pid=$!
  printf '%s\n' "${pid}" > "$(pid_file "${service}")"
  # Give a failing process a moment to exit so we report it here rather than
  # claiming success and leaving the operator to discover it in the log.
  local waited=0
  while (( waited < 20 )); do
    if ! kill -0 "${pid}" 2>/dev/null; then
      rm -f "$(pid_file "${service}")"
      printf '%s\n' "--- $(log_file "${service}") 마지막 20줄 ---" >&2
      tail -n 20 "$(log_file "${service}")" >&2 || true
      die "${service} 기동에 실패했습니다."
    fi
    sleep 0.1
    waited=$((waited + 1))
  done
  ok "${service} 기동됨 (pid ${pid}, 로그 $(log_file "${service}"))"
}

stop_service() {
  local service="$1" pid waited=0
  if ! pid="$(running_pid "${service}")"; then
    info "${service} 실행 중이 아닙니다"
    rm -f "$(pid_file "${service}")"
    return 0
  fi
  # SIGTERM lets the server drain in-flight requests; the API installs a
  # graceful shutdown with a 30s budget.
  kill -TERM "${pid}" 2>/dev/null || true
  while (( waited < 350 )); do
    kill -0 "${pid}" 2>/dev/null || break
    sleep 0.1
    waited=$((waited + 1))
  done
  if kill -0 "${pid}" 2>/dev/null; then
    warn "${service}가 35초 안에 종료되지 않아 SIGKILL을 보냅니다 (pid ${pid})"
    kill -KILL "${pid}" 2>/dev/null || true
    sleep 0.5
  fi
  rm -f "$(pid_file "${service}")"
  ok "${service} 종료됨"
}

status_service() {
  local service="$1" pid
  if pid="$(running_pid "${service}")"; then
    printf '  %-8s %s실행 중%s  pid %s\n' "${service}" "${GREEN}" "${RESET}" "${pid}"
  elif systemd_active "${service}"; then
    printf '  %-8s %ssystemd%s 로 실행 중 (이 스크립트가 관리하지 않음)\n' "${service}" "${YELLOW}" "${RESET}"
  else
    printf '  %-8s %s정지%s\n' "${service}" "${RED}" "${RESET}"
  fi
}

resolve_services() {
  if [[ $# -eq 0 || "$1" == "all" ]]; then
    printf '%s\n' "${SERVICES[@]}"
    return
  fi
  for candidate in "$@"; do
    valid_service "${candidate}" || die "알 수 없는 서비스: ${candidate} (server, runner, all)"
    printf '%s\n' "${candidate}"
  done
}

# ---------------------------------------------------------------- doctor

WARN_COUNT=0
FAIL_COUNT=0

check_binaries() {
  head1 "실행 파일"
  for service in "${SERVICES[@]}" executor; do
    local binary="${BIN_DIR}/releasedock-${service}"
    if [[ -x "${binary}" ]]; then
      ok "releasedock-${service}"
    elif [[ -e "${binary}" ]]; then
      bad "releasedock-${service} 실행 권한이 없습니다: ${binary}"
    elif [[ "${service}" == "executor" ]]; then
      warn "releasedock-executor 없음 (승인 스크립트 격리 실행에 필요, 심플 모드에는 불필요)"
    else
      bad "releasedock-${service} 없음: ${binary}"
    fi
  done
  # Assets are embedded in the server binary; a directory beside it is only
  # an optional override target and is not required.
  if [[ -f "${ROOT_DIR}/web/index.html" ]]; then
    info "웹 자산 디스크 사본이 있습니다 (${ROOT_DIR}/web). 기본값은 바이너리 내장본이며, RELEASEDOCK_WEB_ROOT 로 지정할 때만 이 사본을 씁니다."
  else
    info "웹 자산은 서버 바이너리에 내장되어 있습니다. 기동 로그의 source 값으로 확인하십시오."
  fi
}

check_env() {
  head1 "환경 설정"
  if [[ ! -f "${ENV_FILE}" ]]; then
    bad "환경 파일 없음: ${ENV_FILE}"
    info "deploy/releasedock.env.example 을 복사해 값을 채우십시오."
    return
  fi
  ok "환경 파일 ${ENV_FILE}"
  local mode
  mode="$(stat -c '%a' "${ENV_FILE}" 2>/dev/null || echo '')"
  if [[ -n "${mode}" && "${mode}" != "600" && "${mode}" != "640" && "${mode}" != "400" && "${mode}" != "440" ]]; then
    warn "환경 파일 권한이 ${mode}입니다. Secret이 들어 있으므로 0640 이하를 권장합니다."
  fi
  set -a
  # shellcheck disable=SC1090
  source "${ENV_FILE}"
  set +a

  [[ -n "${POSTGRES_DSN:-}" ]] && ok "POSTGRES_DSN 설정됨" || bad "POSTGRES_DSN 이 비어 있습니다"
  [[ -n "${BOOTSTRAP_ADMIN:-}" ]] && ok "BOOTSTRAP_ADMIN=${BOOTSTRAP_ADMIN}" || bad "BOOTSTRAP_ADMIN 이 비어 있습니다"
  if [[ -z "${BOOTSTRAP_ADMIN_PASSWORD:-}" ]]; then
    bad "BOOTSTRAP_ADMIN_PASSWORD 이 비어 있습니다"
  elif (( ${#BOOTSTRAP_ADMIN_PASSWORD} < 12 )); then
    bad "BOOTSTRAP_ADMIN_PASSWORD 는 12자 이상이어야 합니다"
  elif [[ "${BOOTSTRAP_ADMIN_PASSWORD}" == "replace-before-first-start" ]]; then
    bad "BOOTSTRAP_ADMIN_PASSWORD 가 예제 값 그대로입니다"
  else
    ok "BOOTSTRAP_ADMIN_PASSWORD 길이 충족"
  fi

  # The server requires exactly 32 bytes, given as base64 or 64 hex characters.
  local key="${ENCRYPTION_KEY:-}"
  if [[ -z "${key}" ]]; then
    bad "ENCRYPTION_KEY 이 비어 있습니다  (openssl rand -base64 32)"
  elif [[ "${key}" == "replace-with-base64-encoded-32-byte-key" ]]; then
    bad "ENCRYPTION_KEY 가 예제 값 그대로입니다  (openssl rand -base64 32)"
  elif [[ "${key}" =~ ^[0-9a-fA-F]{64}$ ]]; then
    ok "ENCRYPTION_KEY 32바이트 (hex)"
  elif command -v base64 >/dev/null 2>&1 && [[ "$(printf '%s' "${key}" | base64 -d 2>/dev/null | wc -c || echo 0)" == "32" ]]; then
    ok "ENCRYPTION_KEY 32바이트 (base64)"
  else
    bad "ENCRYPTION_KEY 가 32바이트가 아닙니다  (openssl rand -base64 32)"
  fi

  local port="${PORT:-8080}"
  if [[ "${port}" =~ ^[0-9]+$ ]] && (( port >= 1 && port <= 65535 )); then
    ok "PORT=${port}"
  else
    bad "PORT 는 1~65535 범위여야 합니다: ${port}"
  fi
}

check_ports() {
  head1 "포트"
  local port="${PORT:-8080}"
  local listener=''
  if command -v ss >/dev/null 2>&1; then
    listener="$(ss -ltnp 2>/dev/null | awk -v p=":${port}\$" '$4 ~ p {print $0; exit}')"
  elif command -v netstat >/dev/null 2>&1; then
    listener="$(netstat -ltnp 2>/dev/null | awk -v p=":${port}\$" '$4 ~ p {print $0; exit}')"
  else
    info "ss/netstat 이 없어 포트 확인을 건너뜁니다"
    return
  fi
  if [[ -z "${listener}" ]]; then
    ok "포트 ${port} 사용 가능"
  elif [[ "${listener}" == *releasedock-server* ]]; then
    ok "포트 ${port} 를 releasedock-server 가 사용 중입니다"
  else
    bad "포트 ${port} 가 다른 프로세스에 사용 중입니다: ${listener}"
  fi
}

check_paths() {
  head1 "데이터 경로"
  local state="${RELEASEDOCK_STATE_DIR:-/var/lib/releasedock}"
  for path in "${state}" "${state}/artifacts" "${state}/simple"; do
    if [[ ! -e "${path}" ]]; then
      info "${path} 없음 (첫 사용 시 생성됩니다)"
    elif [[ -L "${path}" ]]; then
      bad "${path} 가 심볼릭 링크입니다"
    elif [[ ! -d "${path}" ]]; then
      bad "${path} 가 디렉터리가 아닙니다"
    elif [[ -w "${path}" ]]; then
      ok "${path} 쓰기 가능"
    else
      warn "${path} 에 현재 사용자가 쓸 수 없습니다"
    fi
  done
  for path in "${RUN_DIR}" "${LOG_DIR}"; do
    if mkdir -p "${path}" 2>/dev/null; then
      ok "${path} 사용 가능"
    else
      bad "${path} 를 만들 수 없습니다"
    fi
  done
}

check_database() {
  head1 "PostgreSQL"
  local dsn="${POSTGRES_DSN:-}"
  if [[ -z "${dsn}" ]]; then
    info "POSTGRES_DSN 이 없어 확인을 건너뜁니다"
    return
  fi
  if command -v psql >/dev/null 2>&1; then
    if psql "${dsn}" -Atqc 'SELECT 1' >/dev/null 2>&1; then
      ok "접속 성공"
      local applied
      applied="$(psql "${dsn}" -Atqc 'SELECT count(*) FROM schema_migrations' 2>/dev/null || echo '')"
      if [[ "${applied}" =~ ^[0-9]+$ ]]; then
        ok "적용된 마이그레이션 ${applied}개"
      else
        info "schema_migrations 가 아직 없습니다. 서버를 처음 기동하면 생성됩니다."
      fi
    else
      bad "접속 실패. DSN, 방화벽, sslmode 를 확인하십시오."
    fi
  elif command -v pg_isready >/dev/null 2>&1; then
    if pg_isready -d "${dsn}" >/dev/null 2>&1; then
      ok "pg_isready 응답 정상"
    else
      bad "pg_isready 응답 없음"
    fi
  else
    info "psql/pg_isready 가 없어 확인을 건너뜁니다"
  fi
}

check_runtime() {
  head1 "프로세스"
  for service in "${SERVICES[@]}"; do
    status_service "${service}"
  done
  local pid port="${PORT:-8080}"
  if pid="$(running_pid server)"; then
    if command -v curl >/dev/null 2>&1; then
      local body
      body="$(curl -fsS --max-time 5 "http://127.0.0.1:${port}/healthz" 2>/dev/null || true)"
      if [[ "${body}" == *'"ok"'* ]]; then
        ok "/healthz 정상"
      else
        bad "/healthz 응답이 정상이 아닙니다. DB 연결을 확인하십시오."
      fi
      local version
      version="$(curl -fsS --max-time 5 "http://127.0.0.1:${port}/api/v1/version" 2>/dev/null || true)"
      [[ -n "${version}" ]] && info "version ${version}"
    else
      info "curl 이 없어 health check 를 건너뜁니다"
    fi
  fi
}

check_isolation() {
  head1 "격리 경계"
  if [[ -S /run/releasedock-executor/executor.sock ]]; then
    ok "executor socket 이 활성화되어 있습니다. 승인 스크립트가 격리 실행됩니다."
  else
    warn "executor socket 이 없습니다 (systemd socket activation 필요)."
    info "심플 모드는 영향이 없습니다. 전체 모드의 승인 스크립트 실행은 systemd 설치가 필요합니다."
  fi
  if command -v systemctl >/dev/null 2>&1; then
    local units=0
    for unit in releasedock-server releasedock-runner releasedock-executor; do
      systemctl list-unit-files "${unit}.*" >/dev/null 2>&1 && units=$((units + 1))
    done
    (( units > 0 )) && info "systemd unit 이 설치되어 있습니다. standalone 과 동시에 기동하지 마십시오."
  fi
  info "심플 모드 명령은 API 서비스 계정 권한으로 실행됩니다 (docs/simple-mode.md 참고)."
}

doctor() {
  printf '%sReleaseDock doctor%s  root=%s\n' "${BOLD}" "${RESET}" "${ROOT_DIR}"
  WARN_COUNT=0
  FAIL_COUNT=0
  check_binaries
  check_env
  check_ports
  check_paths
  check_database
  check_runtime
  check_isolation
  head1 "결과"
  if (( FAIL_COUNT == 0 && WARN_COUNT == 0 )); then
    ok "문제를 찾지 못했습니다."
  else
    printf '  실패 %s%d%s · 경고 %s%d%s\n' "${RED}" "${FAIL_COUNT}" "${RESET}" "${YELLOW}" "${WARN_COUNT}" "${RESET}"
  fi
  (( FAIL_COUNT == 0 )) || return 1
}

# Only the checks that must hold before a process can usefully start.
preflight() {
  local port="${PORT:-8080}"
  [[ -x "${BIN_DIR}/releasedock-server" ]] || die "실행 파일이 없습니다: ${BIN_DIR}/releasedock-server"
  [[ -n "${POSTGRES_DSN:-}" ]] || die "POSTGRES_DSN 이 비어 있습니다 (${ENV_FILE})"
  [[ -n "${ENCRYPTION_KEY:-}" ]] || die "ENCRYPTION_KEY 이 비어 있습니다 (${ENV_FILE})"
  [[ "${port}" =~ ^[0-9]+$ ]] && (( port >= 1 && port <= 65535 )) || die "PORT 가 올바르지 않습니다: ${port}"
}

usage() {
  cat <<'USAGE'
ReleaseDock standalone 제어 스크립트

사용법:
  releasedock.sh start   [server|runner|all]   기동 (기본: all)
  releasedock.sh stop    [server|runner|all]   종료
  releasedock.sh restart [server|runner|all]   재기동
  releasedock.sh status                        상태 확인
  releasedock.sh logs    [server|runner] [-f]  로그 보기
  releasedock.sh doctor                        환경 진단
  releasedock.sh version                       버전 표시

옵션:
  --no-preflight        기동 전 필수 점검을 건너뜁니다

환경 변수:
  RELEASEDOCK_ROOT       설치 루트 (기본: 스크립트 위치에서 자동 판별)
  RELEASEDOCK_ENV_FILE   환경 파일 (기본: <root>/releasedock.env → /etc/releasedock/releasedock.env)
  RELEASEDOCK_RUN_DIR    PID 파일 위치 (기본: <root>/run)
  RELEASEDOCK_LOG_DIR    로그 위치     (기본: <root>/logs)
  RELEASEDOCK_STATE_DIR  데이터 경로   (기본: /var/lib/releasedock)

standalone 은 systemd 없이 API 서버와 Runner 를 직접 띄웁니다. 승인 스크립트를
격리 UID 로 실행하는 executor 는 systemd socket activation 이 필요하므로,
전체 릴리즈 파이프라인을 쓰려면 deploy/install.sh 로 설치하십시오. 심플 모드는
standalone 으로 완전히 동작합니다.
USAGE
}

main() {
  local command="${1:-}" no_preflight=0
  [[ $# -gt 0 ]] && shift || true
  local args=()
  for argument in "$@"; do
    case "${argument}" in
      --no-preflight) no_preflight=1 ;;
      *) args+=("${argument}") ;;
    esac
  done

  case "${command}" in
    start)
      load_env
      (( no_preflight )) || preflight
      while read -r service; do start_service "${service}"; done < <(resolve_services "${args[@]+"${args[@]}"}")
      ;;
    stop)
      while read -r service; do stop_service "${service}"; done < <(resolve_services "${args[@]+"${args[@]}"}")
      ;;
    restart)
      load_env
      (( no_preflight )) || preflight
      local list
      list="$(resolve_services "${args[@]+"${args[@]}"}")"
      while read -r service; do stop_service "${service}"; done <<<"${list}"
      while read -r service; do start_service "${service}"; done <<<"${list}"
      ;;
    status)
      printf '%s상태%s  root=%s\n' "${BOLD}" "${RESET}" "${ROOT_DIR}"
      for service in "${SERVICES[@]}"; do status_service "${service}"; done
      ;;
    logs)
      local service="${args[0]:-server}" follow=0
      [[ "${args[1]:-}" == "-f" || "${args[0]:-}" == "-f" ]] && follow=1
      [[ "${service}" == "-f" ]] && service=server
      valid_service "${service}" || die "알 수 없는 서비스: ${service}"
      local file
      file="$(log_file "${service}")"
      [[ -f "${file}" ]] || die "로그가 없습니다: ${file}"
      if (( follow )); then tail -n 200 -f "${file}"; else tail -n 200 "${file}"; fi
      ;;
    doctor)
      [[ -f "${ENV_FILE}" ]] && { set -a; # shellcheck disable=SC1090
        source "${ENV_FILE}"; set +a; }
      doctor
      ;;
    version)
      if [[ -f "${ROOT_DIR}/VERSION" ]]; then cat "${ROOT_DIR}/VERSION"; else echo "unknown"; fi
      ;;
    ''|-h|--help|help)
      usage
      ;;
    *)
      usage
      die "알 수 없는 명령: ${command}"
      ;;
  esac
}

main "$@"
