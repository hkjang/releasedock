# ReleaseDock Backend

ReleaseDock의 단일 바이너리 Go HTTP 서버입니다. PostgreSQL migration을 바이너리에 포함하며, REST API, Keycloak OIDC, OpenAI 호환 스트리밍 프록시, MCP Streamable HTTP, 정적 React SPA 제공을 한 프로세스에서 처리합니다. 실제 배포 명령은 별도 `releasedock-runner`가 PostgreSQL Job Queue를 통해 수행합니다.

## 빌드와 실행

요구 Go 버전은 `1.26`입니다.

```bash
go test ./...
go build -o releasedock-server ./cmd/server
```

릴리즈 빌드에서는 다음 값을 주입할 수 있습니다.

```bash
go build \
  -ldflags "-X main.version=1.0.0 -X main.commit=$(git rev-parse HEAD) -X main.buildTime=$(date -u +%FT%TZ)" \
  -o releasedock-server ./cmd/server
```

런타임 환경변수는 아래 다섯 개뿐입니다. 운영 설정은 모두 관리자 API와 PostgreSQL에 저장됩니다.

| 변수 | 설명 |
| --- | --- |
| `POSTGRES_DSN` | PostgreSQL DSN |
| `BOOTSTRAP_ADMIN` | 최초/복구 관리자 사용자 이름 |
| `BOOTSTRAP_ADMIN_PASSWORD` | 최초 관리자 비밀번호, 최소 12자. 기존 비밀번호는 시작할 때 덮어쓰지 않음 |
| `ENCRYPTION_KEY` | 정확히 32바이트인 AES 키의 base64 또는 64자리 hex 표현 |
| `PORT` | 선택, 1..65535, 기본값 `8080` |

예:

```bash
export POSTGRES_DSN='postgres://releasedock:password@postgres/releasedock?sslmode=disable'
export BOOTSTRAP_ADMIN='admin'
export BOOTSTRAP_ADMIN_PASSWORD='replace-with-a-long-password'
export ENCRYPTION_KEY='base64-encoded-32-byte-key'
export PORT='8080'
./releasedock-server
```

서버는 시작 시 advisory lock 아래 embedded migration을 순서대로 적용하고 bootstrap 사용자에게 보호된 Administrator 역할을 보장합니다. 기본 listen 주소는 `:8080`입니다. 실행 cwd의 `web/index.html`, 실행파일 옆 `web`, 실행파일 상위의 `web` 순으로 React 산출물을 찾으며 SPA route fallback을 제공합니다. `/api/`의 미등록 route는 항상 JSON 404를 반환합니다.

## 인증 및 공통 계약

- 로컬 로그인: `POST /api/v1/auth/login`
- 공개 인증 설정: `GET /api/v1/auth/config`
- 현재 사용자: `GET /api/v1/me`
- 로그아웃: `POST /api/v1/auth/logout`
- 버전: `GET /api/v1/version`
- 상태 점검: `GET /healthz`

브라우저 로그인은 `releasedock_session` HttpOnly/SameSite=Strict cookie와 읽기 가능한 `releasedock_csrf` SameSite=Strict cookie를 발급합니다. `POST`, `PUT`, `PATCH`, `DELETE` 요청은 후자의 값을 `X-CSRF-Token`에 넣어야 합니다. 개인 API 키는 `Authorization: Bearer rdk_...`로 전달하며 CSRF가 필요하지 않습니다. 세션 원문 및 개인 키 원문은 DB에 저장하지 않고 SHA-256 digest만 저장합니다.

오류 응답은 다음 형식입니다.

```json
{"error":{"code":"invalid_state","message":"release is not ready to deploy"}}
```

목록 응답은 기본적으로 `{ "items": [], "total": 0, "limit": 50, "offset": 0, "page": 1, "pageSize": 50 }` 형태입니다. `limit`(최대 200)/`offset`을 사용하며 `page`/`pageSize`는 UI 호환 alias입니다. 관리자 사용자/역할/Script/Registry/Runner/감사, application/environment/profile/release 목록은 `search`와 리소스별 `status` filter를 count와 row query에 동일 적용합니다. JSON 요청은 알 수 없는 필드를 거부합니다.

## Keycloak OIDC

관리자 화면 또는 `PUT /api/v1/admin/settings/oidc`에서 `issuerUrl`, `clientId`, `clientSecret`을 설정하면 issuer의 `/.well-known/openid-configuration`을 조회해 연결을 검증합니다.

보안 검증에는 다음이 포함됩니다.

- discovery 문서의 issuer exact match
- RS256 JWKS 서명, `kid`, 2048-bit 이상 RSA key
- `iss`, `aud`, 다중 audience의 `azp`, `exp`, `iat`, `nonce`
- authorization code + PKCE S256
- 1회용 state와 HttpOnly state cookie
- 내부 CA는 OS trust store에 설치해야 하며 TLS 검증 해제는 허용하지 않음

Keycloak Client의 Valid Redirect URI에는 관리자 설정 응답의 HTTPS `redirectUrl`을 등록합니다. 명시하지 않으면 일반 설정의 HTTPS `publicUrl`에 `/api/v1/auth/oidc/callback`을 붙입니다. 로그인 시작은 `GET /api/v1/auth/oidc/login?return_to=/safe/local/path`이며 callback 성공 후 Secure 세션/CSRF cookie를 설정하고 검증된 same-origin 경로로 `303` 이동합니다. 자동 사용자를 허용한 경우에도 같은 사용자 이름의 로컬 계정과 자동 병합하지 않습니다.

## 주요 REST API

### 개인화 및 키

| Method | Path | 권한/기능 |
| --- | --- | --- |
| `PUT` | `/api/v1/me/profile` | 표시 이름, 이메일, 개인 설정 |
| `GET/POST` | `/api/v1/me/api-keys` | 개인 키 목록/생성 |
| `PUT` | `/api/v1/me/api-keys/{id}` | 이름, permission scope, 만료 변경 |
| `POST` | `/api/v1/me/api-keys/{id}/rotate` | 즉시 회전, 새 원문 1회 반환 |
| `DELETE` | `/api/v1/me/api-keys/{id}` | 즉시 폐기 |

키의 `permissions`는 현재 사용자에게 실제 부여된 permission의 부분집합이어야 하므로 키 생성으로 권한을 상승시킬 수 없습니다. 키 생성·회전·수정·폐기는 CSRF가 적용된 브라우저 세션에서만 가능하고, API 키로 다른 API 키를 발급하거나 변경할 수 없습니다. `expiresAt`은 RFC3339 또는 `YYYY-MM-DD`를 받습니다. 수정 시 생략하면 기존 만료를 유지하고 `clearExpiresAt: true`로 명시 해제합니다.

### 관리자

| 영역 | Endpoints |
| --- | --- |
| 일반/승인/저장소/Runner | `GET/PUT /api/v1/admin/settings/{general,approval,storage,runner}` |
| OIDC/AI | `GET/PUT /api/v1/admin/settings/{oidc,ai}` |
| 사용자 | `GET/POST /api/v1/admin/users`, `PUT /api/v1/admin/users/{id}` |
| 역할/RBAC | `GET/POST /api/v1/admin/roles`, `PUT /api/v1/admin/roles/{id}`, `GET /api/v1/admin/permissions` |
| Script | `GET/POST /api/v1/admin/scripts`, `PUT/DELETE /api/v1/admin/scripts/{id}`, `POST .../{id}/approve` |
| Harbor Registry | `GET/POST /api/v1/admin/registries`, `PUT/DELETE .../{id}` |
| 대상 Credential | `GET/POST /api/v1/admin/target-credentials`, `PUT/DELETE .../{id}`, `POST .../{id}/rotate` |
| Runner 등록 | `GET/POST /api/v1/admin/runners`, `PUT/DELETE .../{id}` |
| 감사 | `GET /api/v1/admin/audit` |

Administrator 권한은 서비스 lockout 방지를 위해 보호됩니다. 다른 역할의 permission 집합은 변경할 수 있습니다. Registry password는 `{username,password}` JSON을 AES-256-GCM으로 저장하고, AAD는 credential ID와 version에 묶습니다. 갱신할 때 version과 암호문을 함께 회전합니다. Script 내용은 immutable version으로 저장하며 SHA-256, 승인/폐기 상태, interpreter 절대경로, timeout을 기록합니다.

배포 대상 Credential은 `SSH_PRIVATE_KEY`, `KUBECONFIG`, `TOKEN`, `OPAQUE_FILE` 형식을 지원합니다. 원문은 AES-256-GCM으로만 저장하고 AAD `target-credential:<id>:v<version>`에 묶으며 API 응답으로 되돌려 주지 않습니다. 생성/회전/폐기 및 profile bind/unbind는 CSRF가 적용된 브라우저 세션과 `admin.credentials.write`가 모두 필요합니다. Profile에는 `targetCredentialId`를 지정하고, `null`/빈 문자열은 명시 해제하며 생략은 기존 연결을 유지합니다. Job은 credential ID/version을 immutable snapshot하고 Runner는 제한된 임시 파일로만 Script에 전달합니다.

Runner는 PostgreSQL에서 `runner_instances.worker_id` 기준으로 자동 등록/heartbeat하도록 설계되어 있습니다. 현재 Runner는 순차 실행이므로 `maxConcurrentJobs`는 `1`만 허용합니다. 관리자 `active=false`는 Runner polling 비활성 정책에 사용됩니다. HTTP client는 OIDC client secret이나 AI bearer가 다른 주소로 전달되지 않도록 outbound redirect를 따르지 않습니다.

`GET/PUT /api/v1/admin/settings/runner`는 `pollIntervalMs`, `lockRetryMs`, `settingsRefreshMs`, `heartbeatIntervalMs`, `staleJobAfterMs`, `logChunkBytes`, `updatedAt`만 노출합니다. PUT은 명시한 camelCase 필드만 변경하며, `staleJobAfterMs`는 `2 * heartbeatIntervalMs`보다 커야 합니다. `workspace_root`, `command_path` 같은 실행 보안 경계는 API로 변경하거나 노출하지 않습니다.

### Application, Environment, Deployment Profile

```text
GET/POST          /api/v1/applications
GET/PATCH/PUT/DELETE /api/v1/applications/{id}
GET/POST          /api/v1/applications/{id}/environments
GET/POST          /api/v1/environments
PUT/DELETE        /api/v1/environments/{id}
GET/POST          /api/v1/deployment-profiles
GET/PUT/DELETE    /api/v1/deployment-profiles/{id}
```

`/api/v1/profiles`도 같은 profile API의 alias입니다. Profile은 `registryId`, `targetCredentialId`, `preScriptId`, `deployScriptId`, `healthScriptId`, `rollbackScriptId`를 받아 Registry/대상 비밀 및 Runner phase(`PRE_DEPLOY`, `DEPLOY`, `POST_DEPLOY`, `ROLLBACK`)와 연결합니다. Script type과 phase가 일치하고 active/approved 상태여야 합니다. 빈 Script/Registry ID는 기존 연결을 제거합니다. `runnerLabels`는 최대 20개의 문자열 배열이며 Job에는 이 요구 라벨을 immutable snapshot으로 저장합니다. enqueue 시 60초 이내 heartbeat를 보낸 active direct-DB Runner 중 요구 라벨을 모두 가진 Runner가 있어야 하고, Runner claim도 `job.runner_labels <@ runner.labels`를 다시 적용합니다. `runtimeBinaryPath`는 runtime 종류에 맞는 `docker`, `podman`, `ctr` executable만 허용하며 `/usr/bin`, `/usr/local/bin`, `/usr/sbin`, `/usr/local/sbin` 밖의 경로는 거부합니다.

### Release와 artifact

간단한 UI 경로는 한 번의 multipart 요청입니다.

```http
POST /api/v1/releases
Content-Type: multipart/form-data

applicationId=<uuid>
environmentId=<uuid>
deploymentProfileId=<uuid>
version=2.4.1
notes=...
artifact=@release-2.4.1.tar.gz
```

API 자동화는 2단계도 지원합니다.

1. `POST /api/v1/releases` JSON으로 metadata 생성
2. `POST /api/v1/releases/{id}/artifacts/upload`의 `artifact` multipart field로 content 저장

`POST /api/v1/releases/{id}/artifacts`는 실제 파일 업로드 전 외부 staging용 checksum metadata만 등록합니다. 실제 content가 한 번 저장된 뒤에는 검토 화면과 실행 artifact가 달라지는 것을 막기 위해 metadata-only 행을 추가할 수 없습니다. `.tar`와 `.tar.gz`만 허용하고 streaming SHA-256을 계산합니다. DB에는 항상 `release-id/artifact-id.tar.gz` 상대경로만 저장하며 Job은 검토된 immutable `artifact_id`, 상대경로, checksum을 함께 snapshot합니다. 실제 파일 접근 시 관리자 `artifact_storage_path` 절대 root 아래 containment와 symlink/regular-file 조건을 검증합니다.

| Method | Path | 기능 |
| --- | --- | --- |
| `GET/POST` | `/api/v1/releases` | 목록/생성 |
| `GET/PUT/DELETE` | `/api/v1/releases/{id}` | 상세/수정/초기 상태 삭제 |
| `POST` | `/api/v1/releases/{id}/submit-review` | 승인 필요 시 검토 요청, 아니면 queue |
| `POST` | `/api/v1/releases/{id}/{review,approve,reject}` | 조건부 승인 상태 전이 |
| `POST` | `/api/v1/releases/{id}/deploy` | 승인 완료 또는 bypass release enqueue |
| `POST` | `/api/v1/releases/{id}/rollback` | 이전 성공 artifact를 대상으로 조건부 승인 후 ROLLBACK job |
| `POST` | `/api/v1/releases/{id}/retry` | FAILED Job을 같은 frozen artifact/target/operation으로 명시 재시도; 필요 시 새 승인 |
| `GET` | `/api/v1/releases/{id}/logs/stream` | `text/event-stream` 실행 로그 |
| `GET` | `/api/v1/jobs` | Job Queue 상태 |

승인 기능이 꺼져 있으면 검토/승인/반려 상태 자체를 거치지 않습니다. 켜져 있으면 profile의 `approvalRequired`, environment의 `protected`, 또는 `protectedEnvironments` 설정에 따라 적용합니다. 기본적으로 요청자 본인 승인을 차단하고 반려 사유를 요구합니다.

승인 대기/검토/승인 완료 release와 non-terminal Job이 참조하는 application/environment identity, profile, credential, script 또는 profile-script mapping은 DB trigger가 변경을 거부합니다. 승인 및 Job INSERT 시에도 active application/environment/profile, release 대상과 profile 관계, operation별 active/approved `DEPLOY` 또는 `ROLLBACK` script, Registry/target credential을 재검증합니다. 따라서 검토된 실행 입력이 enqueue 전후에 바뀌는 TOCTOU 경로를 차단합니다.

검증 완료된 `(application, environment)`별 현재 배포는 `deployment_heads`에 저장됩니다. DEPLOY Job은 enqueue 당시 prior head release와 exact successful basis Job을 함께 snapshot하고, ROLLBACK도 `rollback_source_release_id`와 `rollback_source_job_id`를 승인부터 실행까지 고정합니다. 따라서 환경 이름이나 profile metadata가 바뀌어도 Runner는 exact source image digest만 조회하며 A→B→C→B→A 연속 rollback이 가능합니다. 실패한 rollback은 verified head를 바꾸지 않습니다.

FAILED 재시도는 `retry_source_job_id`/`retry_of_job_id`로 최신 실패 DEPLOY Job을 고정합니다. 승인을 기다리는 사이 다른 배포가 발생하면 승인 또는 최종 enqueue에서 stale retry를 거부·초기화하여 과거 artifact가 우회 배포되지 않습니다.

v0.1은 이전 배포 source/digest를 완전히 snapshot하지 못한 자동 rollback을 안전상 지원하지 않습니다. Profile의 `autoRollback: true`는 API와 DB constraint 양쪽에서 거부되며, 수동 rollback만 별도 승인·감사·digest 검증 경로로 실행합니다. 수동 rollback은 같은 application/environment의 더 최신 Job이 있으면 거부되어 과거 화면에서 잘못된 배포를 되돌릴 수 없습니다.

로그 SSE는 실제 non-terminal Job이 있는 경우에만 장기 연결되고, 사전 실행 상태는 snapshot 후 종료합니다. 연결은 사용자당 3개/서버당 64개, 최대 30분이며 15초 heartbeat를 보냅니다.

## AI streaming proxy

관리자 AI 설정은 `enabled`, `baseUrl`, `model`, `apiKey`, `maxTokens`를 사용합니다. API key는 AES-GCM 암호문으로만 저장됩니다.

```http
POST /api/v1/ai/chat/completions
```

OpenAI 호환 body를 전달하며 관리자가 지정한 model만 사용하고 `stream` 생략 시 `true`를 넣습니다. `max_tokens`와 `max_completion_tokens`는 설정값 및 절대 상한 `262144`를 넘을 수 없습니다. upstream SSE chunk는 buffering 없이 flush합니다. Endpoint는 http(s) host/path만 허용하며 userinfo/query/fragment는 거부합니다. 장기 호출은 사용자당 2개/서버당 32개로 제한합니다. 요청 본문을 남기지 않고 실제 model/token/stream/upstream status를 감사하며, client 연결이 끊겨도 독립된 bounded context로 감사 기록을 완료합니다.

## MCP Streamable HTTP

단일 endpoint는 `GET/POST /mcp`입니다. REST와 같은 인증/RBAC/API-key scope, 승인 정책 및 Origin allowlist를 사용합니다. 서버는 stateless이므로 선택 사항인 `MCP-Session-Id`를 발급하지 않습니다.

- protocol version: stateless 기본 `2026-07-28`, initialize 호환 `2025-11-25`
- method: `initialize`, `ping`, `server/discover`, `notifications/*`, `tools/list`, `tools/call`
- POST request: `Accept`에 `application/json`과 `text/event-stream`을 모두 포함
- modern request: `MCP-Protocol-Version`, JSON-RPC method와 같은 `Mcp-Method`, 그리고 `params._meta`의 protocolVersion/clientCapabilities가 필수입니다. `Mcp-Name`은 `tools/call`처럼 이름 있는 source를 요청할 때만 필요하며 `params.name`과 정확히 일치해야 합니다.
- response: JSON-RPC를 `application/json` 또는 SSE로 반환하며 modern tool result는 `resultType: "complete"`
- discovery tools: application, environment, profile, release, dashboard, Job, bounded log 조회
- mutation tools: release 생성(REST artifact upload handoff), enqueue, retry, review, approve, reject, rollback

예:

```bash
curl -H "Authorization: Bearer rdk_..." \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  --data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"client","version":"1"}}}' \
  http://localhost:8080/mcp
```

## 검증

```bash
go test ./...
go vet ./...
go build -o releasedock-server ./cmd/server
```

Unit test는 encryption context/tamper, token/UUID, 환경변수/PORT, OIDC JWKS signature/claims/nonce, return path 및 artifact traversal, API-key scope/위임 상승과 날짜, AI token/model/audit/concurrency, 공개 version/security headers, MCP protocol/header/unique tool dispatch 계약을 검증합니다. PostgreSQL smoke test에서는 embedded migrations, bootstrap login, CSRF 거부/허용, CRUD, 1회 노출 개인 키, MCP initialize/tools/list를 확인할 수 있습니다.
