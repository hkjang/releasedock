-- User guide board: administrators write posts, everybody else reads them.
--
-- The body is stored as text and rendered by a small Markdown subset in the
-- browser. No HTML is ever interpolated, so a post cannot inject markup.
CREATE TABLE IF NOT EXISTS guide_posts (
    id UUID PRIMARY KEY,
    title TEXT NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 200),
    summary TEXT NOT NULL DEFAULT '' CHECK (length(summary) <= 500),
    body TEXT NOT NULL DEFAULT '' CHECK (length(body) <= 200000),
    category TEXT NOT NULL DEFAULT 'GUIDE'
        CHECK (category IN ('GUIDE','NOTICE','FAQ')),
    -- Pinned posts sort first; sort_order then orders within each group so an
    -- administrator can arrange a reading sequence.
    pinned BOOLEAN NOT NULL DEFAULT FALSE,
    sort_order INTEGER NOT NULL DEFAULT 100,
    published BOOLEAN NOT NULL DEFAULT TRUE,
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    updated_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS guide_posts_reading_order_idx
    ON guide_posts(pinned DESC, sort_order, created_at DESC) WHERE published;

INSERT INTO permissions(code,description) VALUES
    ('guides.read', 'Read the user guide board'),
    ('admin.guides.read', 'Read guide posts including unpublished drafts'),
    ('admin.guides.write', 'Create, change, and delete guide posts')
ON CONFLICT (code) DO UPDATE SET description=EXCLUDED.description;

-- Reading the guide is useful to everyone, so it goes to every seeded role.
INSERT INTO role_permissions(role_id,permission_code)
SELECT id,'guides.read' FROM roles
ON CONFLICT DO NOTHING;

INSERT INTO role_permissions(role_id,permission_code)
SELECT 'role-admin',code FROM permissions
WHERE code IN ('admin.guides.read','admin.guides.write')
ON CONFLICT DO NOTHING;

-- A first post so the board is not empty on a fresh install. Seeded with a
-- fixed id and ON CONFLICT DO NOTHING so an administrator who edits or deletes
-- it does not get it back on the next migration run.
INSERT INTO guide_posts(id,title,summary,body,category,pinned,sort_order,published) VALUES (
    '00000000-0000-4000-8000-000000000001',
    '심플 모드로 배포하기',
    'tar.gz 패키지를 올려 배포하는 전체 절차와, 실패했을 때 확인할 것들을 정리했습니다.',
$GUIDE$
# 심플 모드로 배포하기

심플 모드는 **파일 하나 올리고 실행 버튼을 누르면 끝**나는 배포 화면입니다.
애플리케이션·환경·배포 프로필 같은 개념을 몰라도 됩니다.

## 준비물

- 배포할 도커 이미지 압축 파일 (`.tar.gz` 또는 `.tar`)
- Keycloak SSO 계정 또는 관리자가 만들어 준 계정

파일 이름 규칙은 없습니다. 버전을 파일 이름에 넣어 두면 나중에 실행 기록에서 찾기 쉽습니다.

## 배포 절차

1. 사이트에 접속해 로그인합니다. Keycloak 세션이 살아 있으면 로그인 화면 없이 바로 들어갑니다.
2. 왼쪽 메뉴에서 **배포**를 엽니다.
3. 배포 대상이 하나뿐이면 고를 것이 없습니다. 여러 개일 때만 선택란이 나타납니다.
4. 점선 영역에 `.tar.gz` 파일을 **끌어다 놓습니다.** 눌러서 선택해도 됩니다.
5. **배포 실행**을 누릅니다.
6. 아래 로그 창에 실행 결과가 실시간으로 표시됩니다.

서버는 올린 파일을 정해진 경로에 저장한 뒤, 관리자가 등록해 둔 명령을 한 번 실행합니다.
이미지를 불러오고 컨테이너를 띄우는 실제 작업은 그 명령이 담당합니다.

## 여러 개를 한 번에 올리기

파일을 여러 개 함께 끌어다 놓을 수 있습니다. 목록에 순서대로 쌓이고, 실행하면 **하나씩 차례로** 처리됩니다.

- 같은 대상에서 동시에 두 건이 돌지 않도록 순차 처리합니다.
- 파일별로 상태(대기 / 업로드 중 / 실행 중 / 성공 / 실패)가 표시됩니다.
- 진행 중에 **남은 파일 중단**을 누르면 이미 시작한 것만 끝내고 멈춥니다.
- 실행 전이라면 목록에서 개별 파일을 지울 수 있습니다.

## 지난 실행 기록과 로그 보기

**실행 기록** 메뉴에서 과거 배포를 볼 수 있습니다. 행을 누르면 **실행 상세**로 이동하고, 그 실행의 전체 로그가 나옵니다.

실행 상세에서 확인할 수 있는 것:

- 상태와 종료 코드, 실패 사유, 소요 시간
- 올린 파일 이름과 크기, SHA-256, 저장된 경로
- 그 배포가 실제로 실행한 명령과 인자
- `로그 내려받기`로 로그를 텍스트 파일로 저장

일반 사용자에게는 **자기가 실행한 기록만** 보입니다. 전체를 보려면 관리자 권한이 필요합니다.

## 실패했을 때

로그의 마지막 줄부터 확인하십시오. 자주 나오는 경우는 아래와 같습니다.

- **`exit=1` 등 0이 아닌 종료 코드** — 배포 명령 자체가 실패했습니다. 로그에 찍힌 명령 출력이 원인입니다.
- **시간 초과** — 명령이 제한 시간 안에 끝나지 않았습니다. 관리자에게 제한 시간 조정을 요청하십시오.
- **`이 대상에서 이미 실행 중인 작업이 있습니다`** — 같은 대상의 앞선 배포가 아직 진행 중입니다. 끝난 뒤 다시 시도하십시오.
- **`실행할 명령이 설정되지 않았습니다`** — 관리자 설정이 아직 완료되지 않았습니다. 관리자에게 알려주십시오.
- **`배포 명령은 성공했으나 복제에 실패했습니다`** — 배포는 됐지만 Harbor 복제 단계에서 실패했습니다. 로그의 `[replication]` 줄을 확인해 관리자에게 전달하십시오.

## 자주 묻는 질문

**같은 파일을 다시 올려도 되나요?**
됩니다. 같은 이름으로 올리면 기존 파일을 덮어씁니다. 같은 패키지를 다시 배포하는 것이 정상적인 흐름입니다.

**파일 이름에 버전을 꼭 넣어야 하나요?**
필수는 아닙니다. 다만 실행 기록에 파일 이름이 그대로 남으므로, 넣어 두면 어떤 버전을 배포했는지 나중에 확인하기 쉽습니다.

**업로드 중에 창을 닫으면 어떻게 되나요?**
업로드가 끝나고 명령이 시작된 뒤라면 서버에서 계속 실행됩니다. 실행 기록에서 결과를 확인할 수 있습니다. 업로드 도중에 닫으면 그 파일은 배포되지 않습니다.

**되돌리기(롤백)는 어떻게 하나요?**
심플 모드에는 롤백 기능이 없습니다. 이전 버전 패키지를 다시 올려 배포하십시오.
$GUIDE$,
    'GUIDE', TRUE, 10, TRUE
) ON CONFLICT (id) DO NOTHING;
