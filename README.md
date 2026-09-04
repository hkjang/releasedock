# ReleaseDock

ReleaseDock는 폐쇄망에서 운영하는 Release Orchestrator / Deployment Gateway입니다. 업로드한 릴리즈 패키지를 검증하고 격리된 Runner에서 이미지 검사, 사내 Harbor push, 승인된 배포 스크립트 실행, 상태 확인 및 롤백까지 일관된 이력으로 관리합니다.

## 구성

- `backend/`: Go API, 인증, 설정, RBAC, 감사, AI streaming proxy, MCP endpoint
- `runner/`: PostgreSQL job queue/Harbor 실행기와 secret 없는 승인 Script executor
- `web/`: React + TypeScript + MUI 운영 포털
- `deploy/`: systemd 기반 폐쇄망 설치 자산과 standalone 제어 스크립트 (`releasedock.sh`)
- `backend/internal/webassets/`: 서버 바이너리에 포털을 내장하는 embed 지점 (빌드 시 채워지며 커밋하지 않음)
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

기동·종료·재기동과 환경 진단은 `make start` / `make stop` / `make restart` / `make status` / `make doctor` 를 사용합니다.

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

관리자는 **심플 모드 설정**에서 명령을 서비스별로 따로 둘지, 공통 명령 하나로 통일할지 선택합니다. 기본 화면 모드도 여기서 지정하며, 심플 모드에서는 배포·실행 기록만 메뉴에 노출됩니다. 활성 대상이 하나면 사용자는 대상을 고를 필요조차 없고, 여러 파일을 한 번에 끌어다 놓으면 순차적으로 처리됩니다.

심플 모드 설정에서 Harbor Registry 와 복제 규칙을 골라 두면, 배포 명령이 성공한 뒤 그 규칙을 자동으로 실행하고 완료까지 기다립니다. 여러 파일을 한 번에 올릴 때 복제를 파일마다 할지 마지막 파일에서 한 번만 할지도 여기서 정합니다. 복제 다음 단계로 앱을 실제 교체하는 **앱 배포 명령**을 이어서 실행할 수도 있으며, 이 명령은 파일마다 실행되는 배포 명령과 별개입니다. 각 단계는 체크로 켜고 끕니다.

실행 기록에서는 각 실행의 전체 로그와 실제로 사용된 명령 스냅샷을 확인하고 평문으로 내려받을 수 있습니다. 일반 사용자는 자신의 실행만, `admin.simple.read` 권한을 가진 관리자는 모든 사용자의 실행을 봅니다.

```text
업로드 → 지정 경로에 저장 → 지정된 명령 실행 → 로그 스트리밍
```

심플 모드의 명령은 격리된 executor가 아니라 **API 서비스 계정이 직접 실행**합니다. 이는 전체 모드의 3-UID 격리를 사용하지 않는다는 뜻이므로, 등록 권한과 스크립트 신뢰 범위를 반드시 확인하십시오. 자세한 내용과 보안 트레이드오프는 [심플 모드 가이드](docs/simple-mode.md)에 있습니다.

## Keycloak SSO

연동에 필요한 값은 **Issuer, Client ID, Client Secret** 세 가지입니다. Redirect URI는 입력하지 않아도 되며, 서버가 일반 설정의 **사이트 공개 주소** 또는 접속에 사용된 주소에서 파생하여 로그인 state에 고정한 뒤 토큰 교환에서 그대로 재사용합니다. Keycloak에 등록할 값은 관리 화면에 그대로 표시됩니다.

`Keycloak 세션이 있으면 자동 로그인` 을 켜면 세션이 살아 있는 사용자는 로그인 화면 없이 바로 들어갑니다. `prompt=none` 을 사용하므로 세션이 없으면 화면 없이 조용히 실패해 평소의 로그인 화면이 표시되고, 브라우저 세션당 한 번만 시도하며 사용자가 직접 로그아웃한 뒤에는 다시 시도하지 않습니다.

폐쇄망 Keycloak이 TLS 없이 운영되거나 backchannel(`token_endpoint`, `jwks_uri`)을 평문 HTTP로 내려주는 경우 `내부 평문 HTTP endpoint 허용`을 켜면 연동됩니다. 이 옵션을 켜도 공개 라우팅 가능한 호스트에는 평문을 허용하지 않습니다. discovery 오류 메시지는 문제가 된 endpoint 이름과 실제 값, 이유를 함께 알려줍니다.

## 관리자 접근 IP 제한

관리 화면에서 관리 기능을 사용할 수 있는 출발지 IP 허용 목록을 관리합니다. `/api/v1/admin/` 이하의 모든 관리 API에 적용되며, 자기 자신을 잠그는 저장은 거부되고 루프백은 항상 허용됩니다. 리버스 프록시 뒤에서 운영한다면 신뢰 프록시 CIDR을 함께 등록해야 합니다.

## 사용자 가이드 게시판

관리자가 가이드·공지·FAQ 게시글을 작성하면 사용자 화면의 **사용자 가이드** 메뉴에 표시됩니다. 본문은 Markdown 일부(제목·목록·코드블록·인용·굵게·인라인 코드)를 지원하며 **HTML 은 해석하지 않습니다.** 설치 시 심플 모드 배포 가이드가 하나 들어 있습니다.

## Standalone 기동

웹 포털은 서버 바이너리에 내장되어 있습니다. 따라서 **실행 파일 하나와 환경 파일 하나**만 있으면 심플 모드가 완전히 동작하며, 별도의 `web/` 디렉터리가 필요하지 않습니다.

```bash
./bin/releasedock-server          # 이것만으로 API + 웹 포털이 뜹니다
```

systemd 없이 기동·종료·재기동을 하려면 패키지의 `releasedock.sh` 를 사용합니다.

```bash
./releasedock.sh doctor     # 환경 진단 (실패가 있으면 종료 코드 1)
./releasedock.sh start      # 기동
./releasedock.sh status
./releasedock.sh restart
./releasedock.sh stop
./releasedock.sh logs server -f
```

소스 체크아웃에서는 `make build` 후 `make doctor` / `make start` / `make stop` / `make restart` / `make status` / `make logs` 로 같은 일을 합니다.

`doctor` 는 실행 파일, 환경 변수, 포트, 데이터 경로, PostgreSQL 접속과 마이그레이션 수, 프로세스 상태와 `/healthz`, 격리 경계까지 점검합니다. 승인 스크립트를 격리 UID 로 실행하는 executor 는 systemd socket activation 이 필요하므로 standalone 에서는 심플 모드만 완전히 동작합니다. 자세한 내용은 [폐쇄망 운영 가이드](docs/offline-install.md)에 있습니다.

## 폐쇄망 배포

인터넷이 연결된 빌드 환경에서 아래 명령을 실행하면 런타임 의존성이 없는 Linux amd64 바이너리와 정적 웹 자산, 설치 스크립트만 포함한 파일을 만듭니다.

```bash
make package
# release/releasedock-v0.5.8.tar.gz
```

압축 파일과 같은 위치의 `.sha256`을 폐쇄망으로 함께 반입하고 검증한 뒤 설치합니다. PostgreSQL, Docker/Podman/containerd, Harbor와 배포 대상은 폐쇄망 내부에서 별도로 제공되어야 합니다.

## 보안 원칙

- tar path traversal, 링크 탈출, 압축 폭탄 제한
- shell 문자열 결합 없이 `exec.CommandContext` 인자 실행
- 승인 Script를 root-owned Unix socket과 별도 executor UID로 격리
- 암호화 secret store와 개인 API key 해시 저장/회전
- 역할별로 변경 가능한 RBAC permission과 개인 API key별 축소 scope
- 운영 배포 동시 실행 lock, 단계별 append-only 로그와 감사 이력
- OIDC issuer/JWKS 검증, state/nonce/PKCE, 로그인 state에 고정한 redirect URI 재사용
- 관리 API 출발지 IP 허용 목록과 신뢰 프록시 기반 X-Forwarded-For 해석
- 심플 모드는 셸 없는 인자 실행·환경변수 allowlist·프로세스 그룹 정리를 적용하지만, 별도 executor UID 격리 밖에서 실행됩니다
- 실행 기록과 로그는 본인 것만 조회 가능하며, 전체 조회는 관리 권한과 API key scope를 함께 요구합니다
- 가이드 게시글 본문은 React 요소로 조립하여 HTML을 해석하지 않으므로 게시글을 통한 스크립트 주입이 불가능합니다
- MCP Origin 검증, 인증 및 권한 검사
- AI endpoint는 관리자 allowlist 설정만 사용하며 streaming을 기본값으로 사용

자세한 내용은 [아키텍처](docs/architecture.md), [심플 모드 가이드](docs/simple-mode.md), [Quick Deploy 운영 가이드](docs/quick-deploy.md)와 [폐쇄망 운영 가이드](docs/offline-install.md)를 참고하십시오.
