# MCP 연결 가이드

ReleaseDock 은 `/mcp` 에서 MCP(Model Context Protocol) 서버를 엽니다. 들어오는 길은 두 가지입니다.

| 방법 | 누가 쓰나 | 필요한 것 |
| --- | --- | --- |
| 개인 API 키 (`rdk_…`) | CI·스크립트처럼 사람이 없는 자동화, Keycloak 이 없는 설치 | 개인화 메뉴에서 만든 키 |
| Keycloak SSO 토큰 (OAuth 2.1) | 사람이 쓰는 MCP 클라이언트(Claude, Cursor 등) | MCP 주소 하나 — 관리자가 켜 둔 경우 |

두 방법은 같은 RBAC 권한과 같은 안전 규칙을 지납니다. 키 방식은 그대로이고, SSO 방식은 관리자가 켜기 전까지 아무것도 바꾸지 않습니다(기본값 꺼짐).

## 키 없이 SSO 로 연결하기 (사용자)

관리자가 **MCP SSO(OAuth) 인증**을 켜 두었다면 키를 만들 필요가 없습니다.

1. 먼저 브라우저로 ReleaseDock 에 Keycloak SSO 로 한 번 로그인합니다. 이때 계정이 등록됩니다 — 토큰만으로는 계정이 생기지 않습니다.
2. MCP 클라이언트에 MCP 주소만 넣습니다: `https://<ReleaseDock 주소>/mcp` (개인 API 키 페이지 상단에도 표시됩니다).
3. 클라이언트가 Keycloak 로그인 창을 띄웁니다. 이미 Keycloak 에 로그인돼 있으면 거의 바로 넘어갑니다.
4. 연결되면 `tools/list` 에 ReleaseDock 도구가 보입니다.

권한은 **내 역할 권한 중 관리자가 정한 범위(`mcp.oauth.scopes`) 안의 것**입니다. 기본 범위는 읽기(`mcp.use applications.read profiles.read releases.read`)이므로 릴리즈 생성·승인 같은 쓰기 도구는 관리자가 범위를 넓히기 전까지 `permission denied` 로 답합니다. 키에 권한 범위를 주는 것과 같은 규칙입니다.

SSO 토큰은 `/mcp` 에서만 받습니다. REST API·로그 스트림·관리 API 는 지금처럼 키와 브라우저 세션만 받습니다.

## 관리자 설정

설정은 **관리 → 설정 → Keycloak OIDC** 카드의 "MCP를 SSO로 연결" 절에 있습니다(`PUT /api/v1/admin/settings/oidc`). 설정 키 이름은 사내 다른 서비스와 같습니다.

| 키 | 기본값 | 뜻 |
| --- | --- | --- |
| `mcp.oauth.enabled` | `false` | 켜기 스위치. 기본은 꺼짐 |
| `mcp.oauth.resource` | 빈 값 | 리소스 식별자 = 클라이언트가 실제로 접속하는 공개 HTTPS 주소 + `/mcp`. 비우면 일반 설정의 **공개 HTTPS URL** + `/mcp` 로 만들고, 그것도 없으면 요청의 호스트에서 만듭니다(마지막 수단) |
| `mcp.oauth.audience` | 빈 값 | 공백 구분 허용 대상. 토큰의 `aud` **또는 `azp`** 가 이 목록에 있으면 받습니다 |
| `mcp.oauth.scopes` | `mcp.use applications.read profiles.read releases.read` | 공백 구분 권한 코드. SSO 주체에게 주는 범위의 천장. `mcp.use` 필수 |
| (재사용) Issuer URL, Client ID, 내부 평문 HTTP 허용 | 웹 로그인 설정 | 새로 만들지 않습니다 |

켜지는 조건은 셋입니다: Issuer URL 이 있고, 리소스 식별자를 만들 수 있고, 스위치가 켜져 있을 것. 저장 시점에 검사해 하나라도 빠지면 400 으로 거부하고 **아무것도 저장하지 않습니다**. 이미 저장된 값이 나중에 조건을 잃으면(예: Issuer 를 지움) 꺼진 것처럼 동작합니다 — 메타데이터는 404, 토큰은 키 전용 때와 같은 401 — 그리고 로그에 이유를 남깁니다(메타데이터 요청: `MCP OAuth is enabled but inactive reason=…`, 토큰 요청: `MCP OAuth token rejected reason=inactive: …`). 설정 카드도 같은 이유를 경고로 보여 줍니다.

저장 시 거부되는 값:

- `mcp.oauth.resource` 가 절대 URL 이 아니거나 userinfo·query·fragment 가 있음, `/mcp` 로 끝나지 않음, HTTPS 가 아님(내부 평문 HTTP 허용을 켜고 사설 호스트일 때만 `http://` 허용)
- `mcp.oauth.scopes` 가 비었거나 존재하지 않는 권한 코드를 포함하거나 `mcp.use` 가 없음
- `mcp.oauth.enabled=true` 인데 Issuer URL 이 없거나 리소스 식별자를 만들 수 없음, 또는 Issuer discovery 에 실패

### 서버가 하는 일과 하지 않는 일

이 서버는 **리소스 서버**입니다. 로그인과 토큰 발급은 Keycloak 이 합니다.

- `GET /.well-known/oauth-protected-resource` 와 `GET /.well-known/oauth-protected-resource/mcp` 에서 인증 없이 맨 JSON(RFC 9728)을 냅니다: `resource`, `authorization_servers=[Issuer]`, `bearer_methods_supported=["header"]`, `scopes_supported`. `Access-Control-Allow-Origin: *`, 5분 캐시. 꺼져 있으면 404.
- `/mcp` 가 자격 없이 또는 거부된 토큰으로 불리면 `401` 에 `WWW-Authenticate: Bearer realm="ReleaseDock", resource_metadata="…/.well-known/oauth-protected-resource/mcp"` 를 붙입니다(거부된 토큰이면 `, error="invalid_token"` 추가). **`/mcp` 에서만** 붙고 REST 401 에는 붙지 않습니다.
- 같은 `Authorization: Bearer` 헤더에서 `rdk_` 로 시작하면 키, JWT 모양(점 두 개)이면 SSO 토큰, 둘 다 아니면 지금과 같은 `authentication required` 입니다.
- 토큰 검사: Keycloak JWKS 서명(RS·PS·ES 계열만, `HS*`·`none` 거부), `iss` = Issuer URL, `exp`·`nbf`·`iat`(60초 여유), `typ=ID` 거부(ID 토큰은 로그인 증거이지 API 자격이 아님), `cnf` 있으면 거부(DPoP·mTLS 바인딩은 검증 불가), `sub` 필수, 대상 검사(아래).
- JWKS 는 5분 캐시하고, 모르는 `kid` 가 오면 10초에 한 번까지만 다시 읽습니다(키 교체 직후 최대 10초 지연 가능).
- `/authorize`, `/token`, 동적 클라이언트 등록을 만들지 않고, 토큰을 저장하거나 세션으로 바꾸지 않으며, introspection 을 호출하지 않습니다. 따라서 **Keycloak 에서 로그아웃하거나 사용자를 끊어도 이미 발급된 액세스 토큰은 만료까지 삽니다** — 액세스 토큰 수명을 짧게(5분 안팎) 두십시오. ReleaseDock 쪽 계정 비활성화는 다음 요청부터 바로 적용됩니다.

### 대상(audience) 검사

다른 앱에 로그인해 받은 토큰이 이 앱의 `/mcp` 를 열어서는 안 되므로, 다음 중 하나는 맞아야 합니다.

- `aud` 에 리소스 식별자(`https://…/mcp`)가 있다 — Keycloak 에 Audience 매퍼를 둔 정식 경로
- `aud` 또는 `azp` 가 `mcp.oauth.audience` 에 있다 — 매퍼 없이 쓰는 호환 경로. 실제 Keycloak 26 은 `aud` 에 `account` 만 싣고 클라이언트 ID 는 `azp` 에 담으므로, MCP 클라이언트 ID 를 여기 적으면 됩니다

웹 로그인 Client ID 는 자동으로 허용되지 않습니다. 웹 클라이언트 토큰까지 받으려면 그 ID 를 목록에 적으십시오(권장하지 않음 — 클라이언트를 분리하는 편이 좋습니다).

### 계정과 권한

- 토큰의 `sub` 로 이미 등록된 **활성** 계정(`oidc_subject = Issuer|sub`)을 찾습니다. 없으면 `preferred_username` 과 같은 사용자 이름의 **OIDC 계정**(`auth_source='oidc'`)을 찾습니다. 로컬 계정은 이름이 같아도 열리지 않습니다.
- 계정을 만들지 않고, 비활성 계정을 살리지 않으며, 토큰의 role 로 권한을 올리지 않습니다.
- 유효 권한 = 계정의 역할 권한 ∩ `mcp.oauth.scopes`. 토큰의 `scope` 에 ReleaseDock 권한 코드(예: `mcp.use releases.read`)가 실려 오면 그 교집합으로 한 번 더 좁힙니다. 교집합이 비면 빈 권한을 주는 대신 토큰을 거부하고 무엇이 겹치지 않는지 말합니다. Keycloak 에 ReleaseDock 권한 어휘를 가르칠 필요는 없습니다 — `openid profile email` 만 실린 토큰은 천장을 그대로 받습니다.
- SSO 주체는 **키로 들어온 주체와 같은 문**을 지납니다: 개인 키 생성·회전, 대상 자격증명 관리, 관리자 위임처럼 브라우저 세션만 허용하는 작업은 SSO 토큰으로도 할 수 없습니다.

### Keycloak 설정

1. MCP 클라이언트용 **공개(public) 클라이언트**를 웹 로그인 클라이언트와 **별도로** 만듭니다(예: `releasedock-mcp`). Client authentication OFF, **Standard Flow ON**, **PKCE method S256**, Direct Access Grants·Implicit·Service accounts OFF.
2. Valid Redirect URIs 에 쓰는 MCP 클라이언트의 콜백을 정확히 적습니다(Claude: `https://claude.ai/api/mcp/auth_callback`, 로컬 클라이언트는 `http://127.0.0.1:*/callback` 류 — 제품 안내를 확인). `*` 하나로 다 여는 것은 금지.
3. 대상 바인딩 — 둘 중 하나:
   - 정식 경로: 그 클라이언트(또는 전용 client scope)에 **Audience 매퍼**

     | 매퍼 항목 | 값 |
     | --- | --- |
     | Mapper type | Audience |
     | Included Custom Audience | 리소스 식별자, 예: `https://releasedock.company.local/mcp` |
     | Add to access token | ON |
     | Add to ID token | OFF |

   - 호환 경로: 매퍼 없이 ReleaseDock 의 `mcp.oauth.audience` 에 클라이언트 ID(`releasedock-mcp`)를 적습니다.
4. 액세스 토큰 수명은 짧게(5분 안팎).

### 확인하기 (curl)

```bash
# 1. 메타데이터: 200, authorization_servers 에 Keycloak issuer
curl -si https://releasedock.company.local/.well-known/oauth-protected-resource/mcp

# 2. 자격 없는 /mcp: 401 + WWW-Authenticate 에 resource_metadata
curl -si -X POST https://releasedock.company.local/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'

# 3. 토큰으로: 200 과 도구 목록 (TOKEN 은 클라이언트가 받아 온 액세스 토큰)
curl -s -X POST https://releasedock.company.local/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'

# 4. 같은 토큰으로 REST: 401 (SSO 토큰은 /mcp 전용)
curl -si https://releasedock.company.local/api/v1/me -H "Authorization: Bearer $TOKEN"
```

리버스 프록시는 `/mcp` 뿐 아니라 `/.well-known/oauth-protected-resource*` 도 전달해야 하고, `Authorization`·`WWW-Authenticate` 헤더를 보존해야 합니다.

### 거부 메시지별 조치

서버 로그에는 `MCP OAuth token rejected reason=…` 로 어느 검사가 실패했는지 남습니다(토큰 자체는 기록하지 않습니다). 클라이언트가 받는 메시지:

| 메시지 | 로그 reason | 조치 |
| --- | --- | --- |
| `SSO 토큰이 유효하지 않습니다(서명·발급자·만료). 클라이언트에서 다시 로그인하세요.` | `signature:` `iss:` `exp:` `nbf:` `kid:` `alg:` | 만료면 재로그인. `iss` 불일치면 Issuer URL 과 realm 확인. `kid` 미발견이면 Keycloak 키 교체 직후이거나 다른 realm. `alg: HS256` 은 잘못된 클라이언트/토큰 종류 |
| `SSO 토큰이 이 서버를 위해 발급된 것이 아닙니다(aud=[…], azp="…"). 관리자가 … 허용 대상에 "…" 를 더하거나, Keycloak 클라이언트에 Audience 매퍼로 "…" 를 넣으세요.` | `audience:` | 메시지의 `azp` 값을 `mcp.oauth.audience` 에 적거나, Audience 매퍼에 메시지의 리소스 식별자를 넣습니다 |
| `ID 토큰은 API 자격이 아닙니다. …` | `typ: ID token` | 클라이언트가 `id_token` 대신 `access_token` 을 보내야 합니다 |
| `소지자 증명(DPoP·mTLS)이 묶인 토큰은 …` | `cnf:` | 클라이언트의 DPoP 를 끄거나 일반 Bearer 토큰을 쓰게 합니다 |
| `이 SSO 계정은 ReleaseDock 에 등록되지 않았거나 비활성입니다. 먼저 웹으로 한 번 로그인하세요.` | `account:` | 사용자가 웹으로 SSO 로그인(첫 로그인 자동 생성이 꺼져 있으면 관리자가 OIDC 계정을 먼저 만듦). 비활성 계정이면 관리자가 활성화 |
| `SSO 토큰의 scope(…)가 관리자가 허용한 MCP 범위(…)와 겹치지 않습니다. …` | `scope:` | Keycloak 클라이언트가 실어 보내는 scope 를 `mcp.oauth.scopes` 안의 값으로 고치거나, 천장을 넓힙니다 |
| `Keycloak 발급자 정보를 읽지 못해 SSO 토큰을 확인할 수 없습니다. …` (503) | `keys:` | ReleaseDock 서버에서 Issuer 의 discovery·JWKS 에 접근 가능한지(DNS, 방화벽, 내부 CA, 평문 HTTP 허용) 확인 |
| `authentication required` (WWW-Authenticate 없음) | `inactive:` | 스위치가 꺼져 있거나 Issuer·리소스 식별자가 없습니다. 설정 카드의 경고를 확인 |
| `permission required: mcp.use` (403) | — | 계정 역할에 `mcp.use` 가 없거나, 토큰 scope 가 `mcp.use` 없이 ReleaseDock 권한 코드만 실어 왔습니다 |

## 프로토콜 참고

전송 방식은 바뀌지 않았습니다: `POST /mcp` 에 JSON-RPC, `Accept: application/json, text/event-stream`, `MCP-Protocol-Version: 2026-07-28`(stateless, `server/discover`) 또는 `2025-11-25`(legacy `initialize`). `GET /mcp` 는 SSE 스트림입니다. Origin 검증은 일반 설정의 공개 URL 과 추가 허용 Origin 을 따릅니다.
