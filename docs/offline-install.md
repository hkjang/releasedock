# 폐쇄망 설치와 운영

## 사전 조건

- Linux x86_64
- PostgreSQL 14 이상(ReleaseDock이 접근 가능한 DSN)
- 이미지 배포 기능 사용 시 Docker, Podman 또는 containerd 중 하나
- 사내 Harbor와 배포 대상 네트워크
- systemd 247 이상(`ProtectProc` 및 Unix socket activation 사용)

ReleaseDock 패키지에는 정적 서버·Runner·Script executor 바이너리, 웹 asset과 systemd/설치 자산만 들어 있습니다. 외부 PostgreSQL/Harbor/컨테이너 런타임을 자동 설치하거나 인터넷에서 image를 pull하지 않습니다.

설치기는 API용 `releasedock`, Registry/runtime용 `releasedock-runner`, 승인 Script용 `releasedock-executor` OS 계정을 분리합니다. Runner와 executor는 `releasedock-workspace` 그룹만 공유하고, executor socket의 `releasedock-executor-client` 그룹에는 Runner만 속합니다. executor는 supplementary group이 없는 잠긴 `nologin` 계정이며 `/etc/releasedock`과 Docker/containerd/Podman socket에 접근할 수 없고 환경 파일도 받지 않습니다. 호스트에 `docker` 그룹이 있으면 Runner 계정만 해당 그룹에 추가합니다. 업그레이드 설치기는 관리자가 Runner에 부여한 Podman/containerd/custom runtime group을 보존하지만 executor group은 항상 `releasedock-workspace` 하나로 재설정합니다. Docker 그룹은 사실상 root 수준 권한이므로 Runner 전용 호스트를 권장합니다.

승인 Script executor는 요청마다 새로 기동되고 한 요청 뒤 종료됩니다. systemd는 service cgroup의 background/double-fork 자손을 모두 제거한 다음에만 socket backlog의 다음 요청을 새 프로세스로 전달합니다.

Runner는 `/run/releasedock-credentials/.runner.lock`의 kernel `flock`을 프로세스 수명 동안 유지하므로 같은 호스트에서 두 번째 Runner 프로세스는 시작을 거부합니다. 한 호스트에는 패키지의 `releasedock-runner.service` 하나만 운영하십시오.

관리자가 profile에 배포 대상 credential을 연결하면 DB에는 `ENCRYPTION_KEY`로 암호화된 값만 저장됩니다. Runner의 job별 `0640` handoff와 Registry auth/CA/hosts 파일은 디스크 workspace가 아니라 tmpfs 기반 `/run/releasedock-credentials`에만 생성됩니다. executor의 SSH 호환 `0600` copy도 별도 private RuntimeDirectory에 존재하며 Script 1회 실행 후 제거됩니다. systemd가 crash/restart 시 두 RuntimeDirectory를 정리하고, 두 프로세스도 시작할 때 고정 경로만 검증·sweep합니다. 승인 Script는 credential을 사용할 수 있는 신뢰 코드이며, 그대로 출력한 plaintext는 로그 저장 전에 redaction됩니다.

아티팩트 저장소는 systemd 쓰기 경계와 일치하도록 `/var/lib/releasedock` 하위만 사용할 수 있습니다. NFS/NAS를 사용할 때도 `/var/lib/releasedock/artifacts` 또는 그 하위에 마운트하십시오.

Docker의 Registry CA와 insecure-registry 정책은 CLI job별로 설정할 수 없으므로 Docker daemon과 OS trust store에 사전 설치해야 하며 profile의 `registryCaPem`/`registryInsecure`로 대체할 수 없습니다. Podman은 job-local `--cert-dir`/`--tls-verify=false`, containerd는 job-local `hosts.toml`의 `ca`/`skip_verify`를 사용합니다. Podman/containerd socket 권한은 설치기가 자동 부여하지 않으므로 선택한 runtime의 system service group을 Runner에 별도로 부여하십시오.

Runtime binary는 종류별 `docker`/`podman`/`ctr` 이름과 `/usr/bin`, `/usr/local/bin`, `/usr/sbin`, `/usr/local/sbin` 경로만 허용합니다. 해당 디렉터리와 실제 binary(허용 경로 안의 root-owned symlink target 포함)는 root 소유, 실행 가능, group/world 쓰기 불가여야 합니다.

## 반입 검증

```bash
sha256sum -c releasedock-v0.3.4.tar.gz.sha256
tar -tzf releasedock-v0.3.4.tar.gz
tar -xzf releasedock-v0.3.4.tar.gz
cd releasedock-v0.3.4
sudo ./install.sh
```

버전에 맞는 파일명으로 위 명령을 실행합니다. 설치기는 `/opt/releasedock`, `/etc/releasedock`, `/var/lib/releasedock`를 사용합니다. 기존 설정 파일은 덮어쓰지 않습니다. 새 release는 `/opt/releasedock/releases`와 같은 filesystem의 root-only staging directory에서 완성된 뒤 원자적으로 publish되고, `current` symlink도 원자적으로 교체됩니다. 같은 VERSION의 in-place 재설치는 거부되므로 수정 build는 반드시 새 버전으로 패키징하십시오.
업그레이드에서 설치 당시 실행 중인 service만 `try-restart`하여 새 binary로 전환하고, 아직 시작하지 않은 service는 자동으로 시작하지 않습니다.

환경 파일을 채운 뒤 다음처럼 시작합니다.

```bash
sudo systemctl start releasedock-executor.socket releasedock-server releasedock-runner
```

## 환경 파일

`/etc/releasedock/releasedock.env`는 `root:releasedock` 소유의 `0640`으로 유지하고 다섯 변수만 설정합니다. 다른 일반 사용자에게는 읽기 권한을 부여하지 마십시오.

```dotenv
POSTGRES_DSN=postgres://releasedock:password@postgres.internal/releasedock?sslmode=require
BOOTSTRAP_ADMIN=admin
BOOTSTRAP_ADMIN_PASSWORD=replace-before-first-start
ENCRYPTION_KEY=replace-with-base64-32-byte-value
PORT=8080
```

bootstrap password는 최초 로그인 후 변경합니다. 이후 환경 파일에서 `BOOTSTRAP_ADMIN_PASSWORD` 값을 제거하면 안 됩니다(허용 환경 변수 계약은 유지되지만 재부팅 때 사용되지는 않습니다). 운영에서는 systemd credential 또는 root-only 파일로 환경 파일 자체를 보호하십시오.

## 웹 포털 내장

웹 포털은 `releasedock-server` 바이너리에 내장되어 있습니다. 패키지에 별도의 `web/` 디렉터리가 없고, 실행 파일 하나로 API 와 화면을 모두 제공합니다.

자산 선택 순서는 다음과 같습니다.

1. `RELEASEDOCK_WEB_ROOT` 환경 변수로 지정한 디렉터리 — 재빌드 없이 화면을 교체해야 할 때만 사용합니다.
2. 바이너리에 내장된 사본 — 기본값입니다.
3. 실행 파일 옆에서 발견한 `web/` 또는 `web/dist/` — 개발 중 `go run` 을 위한 경로입니다.

기동 로그의 `source` 값으로 무엇이 선택됐는지 확인할 수 있습니다.

```text
level=INFO msg="serving web assets" source=embedded
```

`install.sh` 는 패키지에 `web/` 이 있으면 계속 설치하므로 v0.3.4 이전 패키지도 그대로 설치됩니다.

## Standalone 기동 (systemd 없이)

패키지에 포함된 `releasedock.sh` 로 systemd 없이 바로 띄울 수 있습니다. 평가 환경이나 심플 모드만 사용하는 설치에 적합합니다.

```bash
cd releasedock-v0.3.4
cp releasedock.env.example releasedock.env   # 값을 채웁니다
chmod 600 releasedock.env

./releasedock.sh doctor      # 먼저 환경을 진단합니다
./releasedock.sh start       # 기동 (server + runner)
./releasedock.sh status      # 상태 확인
./releasedock.sh logs server -f
./releasedock.sh restart
./releasedock.sh stop
```

소스 체크아웃에서는 `make build` 후 `make start` / `make stop` / `make restart` / `make status` / `make logs` / `make doctor` 를 쓸 수 있습니다.

### 동작 방식

- 서비스별로 `run/<service>.pid` 와 `logs/<service>.log` 를 만듭니다. 경로는 `RELEASEDOCK_RUN_DIR`, `RELEASEDOCK_LOG_DIR` 로 바꿉니다.
- `start` 는 기동 전 필수 항목(실행 파일, POSTGRES_DSN, ENCRYPTION_KEY, PORT)을 먼저 확인하고, 프로세스가 곧바로 죽으면 로그 마지막 20줄을 보여주며 실패로 끝냅니다. 점검을 건너뛰려면 `--no-preflight` 를 씁니다.
- 이미 실행 중이면 다시 띄우지 않습니다. systemd 로 같은 서비스가 돌고 있으면 **기동을 거부합니다.** 중복 기동은 DB 잠금과 충돌을 일으킵니다.
- `stop` 은 SIGTERM 을 보내 최대 35초 기다린 뒤에만 SIGKILL 로 넘어갑니다. API 는 30초 graceful shutdown 을 사용합니다.
- PID 재사용으로 엉뚱한 프로세스를 죽이지 않도록, `/proc/<pid>/exe` 가 우리 바이너리를 가리키는지 확인한 뒤에만 신호를 보냅니다.

### 제약

승인 스크립트를 격리 UID 로 실행하는 executor 는 **systemd socket activation 이 필요합니다.** standalone 에서는 그 socket 이 없으므로 전체 모드의 배포 스크립트 실행 단계가 실패합니다. `doctor` 가 이 상태를 경고로 알려줍니다.

심플 모드는 명령을 API 프로세스가 직접 실행하므로 standalone 에서 완전히 동작합니다. 전체 릴리즈 파이프라인이 필요하면 `install.sh` 로 systemd 설치를 하십시오.

### doctor 점검 항목

| 구분 | 확인 내용 |
| --- | --- |
| 실행 파일 | server/runner/executor 존재와 실행 권한, 웹 자산 위치 |
| 환경 설정 | 환경 파일 권한, 필수 변수, ENCRYPTION_KEY 32바이트, PORT 범위, 예제 값이 그대로인지 |
| 포트 | PORT 사용 여부와 점유 프로세스 |
| 데이터 경로 | `/var/lib/releasedock` 이하 존재·심볼릭 링크·쓰기 권한, run/log 경로 |
| PostgreSQL | 접속 가능 여부와 적용된 마이그레이션 수 (psql 또는 pg_isready 필요) |
| 프로세스 | 실행 상태, `/healthz`, 버전 |
| 격리 경계 | executor socket 유무, systemd unit 설치 여부, 심플 모드 실행 주체 |

실패가 하나라도 있으면 종료 코드 1 을 반환하므로 설치 자동화에서 그대로 쓸 수 있습니다.

## 백업

- PostgreSQL: 전체 database와 encryption metadata를 같은 시점에 백업
- artifact: 관리자 설정의 artifact root
- `ENCRYPTION_KEY`: DB와 별도 보안 매체에 보관

DB만 복원하고 encryption key를 잃으면 Harbor/OIDC/AI secret을 복호화할 수 없습니다. 현재 릴리즈에서는 `ENCRYPTION_KEY`를 임의로 교체할 수 없으므로 DB 백업과 별도 보안 매체의 키 백업을 반드시 함께 관리하십시오.
