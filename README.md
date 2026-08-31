# ReleaseDock

ReleaseDock는 폐쇄망에서 운영하는 Release Orchestrator / Deployment Gateway입니다. 업로드한 릴리즈 패키지를 검증하고 격리된 Runner에서 이미지 검사, 사내 Harbor push, 승인된 배포 스크립트 실행, 상태 확인 및 롤백까지 일관된 이력으로 관리합니다.

## 구성

- `backend/`: Go API, 인증, 설정, RBAC, 감사, AI streaming proxy, MCP endpoint
- `runner/`: PostgreSQL job queue/Harbor 실행기와 secret 없는 승인 Script executor
- `web/`: React + TypeScript + MUI 운영 포털
- `deploy/`: systemd 기반 폐쇄망 설치 자산
- `scripts/`: 재현 가능한 빌드와 릴리즈 패키징
- `docs/`: 아키텍처, 보안 모델, API 및 운영 문서

웹 프로세스는 Docker socket 또는 운영 서버 키에 접근하지 않습니다. Registry secret과 Docker socket은 Runner 경계 안에 머물고, 관리자가 버전 고정한 script는 DB·암호화 키·container runtime 권한이 없는 별도 OS 사용자의 `releasedock-executor`에서만 실행됩니다.

## 필수 환경 변수

ReleaseDock 프로세스가 읽는 환경 변수는 아래 다섯 종류뿐입니다. 그 외 모든 운영 설정은 관리자 화면과 PostgreSQL에 저장됩니다.

```dotenv
POSTGRES_DSN=postgres://releasedock:password@db.internal:5432/releasedock?sslmode=require
BOOTSTRAP_ADMIN=admin
BOOTSTRAP_ADMIN_PASSWORD=change-this-on-first-login
ENCRYPTION_KEY=base64-encoded-32-byte-key
PORT=8080
```

`BOOTSTRAP_ADMIN*` 값은 최초 관리자 생성 시에만 사용되며 기존 비밀번호를 시작할 때 덮어쓰지 않습니다. 최초 로그인 후 개인 프로필에서 비밀번호를 변경하십시오. `ENCRYPTION_KEY`는 정확히 32바이트를 base64로 인코딩한 값이어야 합니다. `PORT`는 생략하면 `8080`이며 1~65535 범위여야 합니다.

```bash
openssl rand -base64 32
```

## 개발

요구 도구는 Go 1.26.6 이상, Node.js 22 이상/npm, PostgreSQL 14 이상입니다. 빌드는 인터넷이 연결된 환경에서 의존성을 내려받아 수행하고, 생성된 패키지만 폐쇄망으로 반입합니다.

```bash
make test
make build
```

개발 상세는 각 하위 디렉터리의 README를 참고하십시오.

## Quick Deploy

관리자가 **서비스 배포 프리셋**에서 아티팩트 접두어와 애플리케이션·환경·배포 정책을 한 번 연결하면, 일반 사용자는 배포 프로필을 알 필요가 없습니다. 새 릴리즈 화면에 규칙에 맞는 패키지를 끌어 놓으면 서비스, 현재 버전, 신규 버전과 대상 환경을 자동으로 확인하고 **배포 요청** 한 번으로 진행합니다.

```text
<artifact-prefix>-v<semver>.tar.gz

ai-portal-v2.4.1.tar.gz
ai-portal-v2.4.2-rc1.tar.gz
```

접두어는 소문자 영문·숫자와 내부 하이픈만 허용하며 패키지 이름은 대소문자를 구분합니다. 버전은 SemVer core와 선택적 prerelease를 사용하며 Quick Deploy는 현재 운영 버전보다 높은 순방향 버전만 허용합니다. 이전 버전 복구는 별도 복구 절차를 사용합니다. 승인 정책이 적용된 대상은 자동으로 검토 요청 상태가 되고, 승인이 필요하지 않은 대상은 즉시 배포 대기열로 이동합니다. 관리자가 프리셋의 승인 후 자동 배포를 활성화하면 승인과 배포 작업 생성도 하나의 작업으로 처리됩니다.

## 심플 모드

전체 릴리즈 오케스트레이션이 과한 현장을 위한 두 번째 진입점입니다. 사용자는 Keycloak SSO로 로그인하여 관리자가 등록한 대상을 고르고, `tar.gz`를 올리고, 실행 버튼을 누릅니다. 서버는 **파일을 지정 경로에 저장하고 지정된 명령 하나를 실행**할 뿐이며 이미지 로드·태그·Harbor push·승인·버전 비교는 하지 않습니다. 이미지 로드를 포함한 실제 절차는 관리자가 등록한 스크립트가 담당합니다.

관리자는 **심플 모드 설정**에서 명령을 서비스별로 따로 둘지, 공통 명령 하나로 통일할지 선택합니다. 기본 화면 모드도 여기서 지정하며, 심플 모드에서는 배포·실행 기록만 메뉴에 노출됩니다.

```text
업로드 → 지정 경로에 저장 → 지정된 명령 실행 → 로그 스트리밍
```

심플 모드의 명령은 격리된 executor가 아니라 **API 서비스 계정이 직접 실행**합니다. 이는 전체 모드의 3-UID 격리를 사용하지 않는다는 뜻이므로, 등록 권한과 스크립트 신뢰 범위를 반드시 확인하십시오. 자세한 내용과 보안 트레이드오프는 [심플 모드 가이드](docs/simple-mode.md)에 있습니다.

## 관리자 접근 IP 제한

관리 화면에서 관리 기능을 사용할 수 있는 출발지 IP 허용 목록을 관리합니다. `/api/v1/admin/` 이하의 모든 관리 API에 적용되며, 자기 자신을 잠그는 저장은 거부되고 루프백은 항상 허용됩니다. 리버스 프록시 뒤에서 운영한다면 신뢰 프록시 CIDR을 함께 등록해야 합니다.

## 폐쇄망 배포

인터넷이 연결된 빌드 환경에서 아래 명령을 실행하면 런타임 의존성이 없는 Linux amd64 바이너리와 정적 웹 자산, 설치 스크립트만 포함한 파일을 만듭니다.

```bash
make package
# release/releasedock-v0.3.0.tar.gz
```

압축 파일과 같은 위치의 `.sha256`을 폐쇄망으로 함께 반입하고 검증한 뒤 설치합니다. PostgreSQL, Docker/Podman/containerd, Harbor와 배포 대상은 폐쇄망 내부에서 별도로 제공되어야 합니다.

## 보안 원칙

- tar path traversal, 링크 탈출, 압축 폭탄 제한
- shell 문자열 결합 없이 `exec.CommandContext` 인자 실행
- 승인 Script를 root-owned Unix socket과 별도 executor UID로 격리
- 암호화 secret store와 개인 API key 해시 저장/회전
- 역할별로 변경 가능한 RBAC permission과 개인 API key별 축소 scope
- 운영 배포 동시 실행 lock, 단계별 append-only 로그와 감사 이력
- OIDC issuer/JWKS 검증, state/nonce/PKCE
- 관리 API 출발지 IP 허용 목록과 신뢰 프록시 기반 X-Forwarded-For 해석
- 심플 모드는 셸 없는 인자 실행·환경변수 allowlist·프로세스 그룹 정리를 적용하지만, 별도 executor UID 격리 밖에서 실행됩니다
- MCP Origin 검증, 인증 및 권한 검사
- AI endpoint는 관리자 allowlist 설정만 사용하며 streaming을 기본값으로 사용

자세한 내용은 [아키텍처](docs/architecture.md), [심플 모드 가이드](docs/simple-mode.md), [Quick Deploy 운영 가이드](docs/quick-deploy.md)와 [폐쇄망 운영 가이드](docs/offline-install.md)를 참고하십시오.
