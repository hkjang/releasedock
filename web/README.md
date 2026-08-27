# ReleaseDock Web

ReleaseDock의 React + TypeScript 관리 콘솔입니다. Vite로 빌드하며 MUI와 모든 아이콘을 번들에 포함하므로 운영 시 외부 CDN, Google Fonts 또는 인터넷 연결이 필요하지 않습니다.

## 개발과 검증

```bash
npm ci
npm run dev
npm test
npm run build
```

개발 서버는 `/api` 요청을 기본적으로 `http://localhost:8080`에 프록시합니다. 운영에서는 `dist/`를 Go 서버나 내부 웹 서버에서 제공하고, `/api/v1`과 같은 origin을 사용합니다. React Router의 직접 URL 새로고침이 동작하도록 존재하지 않는 정적 경로는 `index.html`로 fallback해야 합니다.

빌드 버전은 `VITE_RELEASEDOCK_VERSION`으로 주입할 수 있습니다.

```bash
VITE_RELEASEDOCK_VERSION=1.2.0 npm run build
```

API의 `GET /api/v1/version` 응답이 있으면 런타임 버전을 우선 표시하고, API가 아직 시작되지 않았으면 빌드 버전을 표시합니다. 버전은 로그인 화면과 프로필 컨텍스트 메뉴 양쪽에 노출됩니다.

## 화면 구조

- 릴리즈 작업공간: 대시보드, 릴리즈 목록, 패키지 등록, 단계별 상세, 이미지 digest, SSE 실시간 로그, 승인/반려/배포/롤백
- 서비스 관리: 애플리케이션, 환경, 배포 프로필, 스크립트, Harbor, Runner, 승인 정책, AI, OIDC, 스토리지, 사용자, 역할/권한, 감사 로그
- 개인화: 프로필, API 키 생성, 키별 권한 변경, 회전, 폐기

현재 메뉴는 URL을 상태의 기준으로 사용하므로 직접 URL 접근과 새로고침 후에도 같은 화면이 유지됩니다. 접힘 메뉴 상태는 `releasedock.nav.expanded` localStorage 값에 보존됩니다.

## API 계약

API 접근은 `src/api/client.ts`에만 모았습니다. 성공 응답은 현재 서버의 직접 JSON과 `{ "data": ... }` envelope를 모두 처리하고, 오류 응답은 `{ "error": { "code": "...", "message": "..." } }` 형식을 사용합니다. 포털 인증은 same-origin HttpOnly `releasedock_session` 쿠키만 사용하며 세션 토큰을 브라우저 저장소에 기록하지 않습니다. POST/PUT/PATCH/DELETE 요청은 `releasedock_csrf` 쿠키를 읽어 `X-CSRF-Token` 헤더를 자동 전송합니다.

주요 계약은 다음과 같습니다.

- 인증: `/auth/config`, `/auth/login`, `/auth/logout`, `/me`, `/version`, `/auth/oidc/login`
- 릴리즈: `/releases`, `/releases/:id`, `/releases/:id/{submit-review,approve,reject,deploy,rollback}`, `/releases/:id/logs/stream`
- 리소스: `/applications`, `/environments`, `/deployment-profiles`
- 관리: `/admin/settings/{general,oidc,ai,approval,storage}`, `/admin/{scripts,registries,runners,users,roles,audit}`
- 개인화: `/me/profile`, `/me/api-keys`, `/me/api-keys/:id/rotate`

서버 계약이 바뀌면 이 파일의 endpoint만 조정하면 각 화면은 그대로 유지됩니다.

## 접근성과 오프라인 운영

기본 글자 크기는 16px이며 본문 건너뛰기, 키보드 focus 표시, 메뉴의 `aria-current`, 로딩 상태와 오류 재시도를 제공합니다. 관리자 메뉴와 긴 사용자 메뉴에는 어두운 테마에 맞춘 로컬 스크롤바가 적용됩니다. 최상위 Error Boundary가 예기치 않은 렌더링 오류를 복구 화면으로 전환합니다.
