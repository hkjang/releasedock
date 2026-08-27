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

## 폐쇄망 배포

인터넷이 연결된 빌드 환경에서 아래 명령을 실행하면 런타임 의존성이 없는 Linux amd64 바이너리와 정적 웹 자산, 설치 스크립트만 포함한 파일을 만듭니다.

```bash
make package
# release/releasedock-v0.1.0.tar.gz
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
- MCP Origin 검증, 인증 및 권한 검사
- AI endpoint는 관리자 allowlist 설정만 사용하며 streaming을 기본값으로 사용

자세한 내용은 [아키텍처](docs/architecture.md)와 [폐쇄망 운영 가이드](docs/offline-install.md)를 참고하십시오.
