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
sha256sum -c releasedock-v0.1.0.tar.gz.sha256
tar -tzf releasedock-v0.1.0.tar.gz
tar -xzf releasedock-v0.1.0.tar.gz
cd releasedock-v0.1.0
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

## 백업

- PostgreSQL: 전체 database와 encryption metadata를 같은 시점에 백업
- artifact: 관리자 설정의 artifact root
- `ENCRYPTION_KEY`: DB와 별도 보안 매체에 보관

DB만 복원하고 encryption key를 잃으면 Harbor/OIDC/AI secret을 복호화할 수 없습니다. 현재 릴리즈에서는 `ENCRYPTION_KEY`를 임의로 교체할 수 없으므로 DB 백업과 별도 보안 매체의 키 백업을 반드시 함께 관리하십시오.
