# 아키텍처

## 신뢰 경계

```text
Browser -> ReleaseDock API -> PostgreSQL / artifact storage
                              |
                              v
                      PostgreSQL job queue
                              |
                              v
                      Release Runner -> Harbor / health check
                             |
                    root-owned Unix socket
                             |
                             v
                unprivileged Script Executor -> deploy target
```

API는 사용자 요청과 상태 전이를 검증하고 작업을 큐에 적재합니다. API와 Runner는 서로 다른 OS 사용자로 실행하며 Runner만 PostgreSQL, root encryption key, registry credential과 container runtime에 접근합니다. 승인 Script는 세 번째 UID인 executor에서만 실행됩니다. executor에는 환경 파일·DB 연결·암호화 키·Docker 그룹이 없고, systemd가 root로 만든 고정 Unix socket을 사용하며 job workspace에만 쓰기 권한을 가집니다. 양쪽은 Linux `SO_PEERCRED`로 Runner와 root-owned listener UID를 검증합니다.

executor의 systemd namespace는 `/etc/releasedock`과 container-runtime socket을 숨기며 `ProtectProc=invisible`로 Runner의 `/proc/<pid>/environ` 접근을 막습니다. Runner와 executor는 setgid+sticky job workspace 및 execute-only tmpfs credential handoff 경로에만 `releasedock-workspace` group으로 접근합니다. sticky bit는 executor가 Runner 소유 artifact/script를 바꾸는 것을 막고, handoff root의 group read bit를 제거해 다른 Job을 열거하지 못하게 합니다. Unix socket 자체는 `root:releasedock-executor-client`이고 Runner만 이 전용 client 그룹에 속하며, executor는 systemd가 넘긴 fd 3으로만 listen합니다. 승인 Script 요청은 interpreter 절대 경로, Runner 소유 script 파일, 환경변수 allowlist, 최대 24시간 timeout으로 제한되며 stdin은 허용하지 않습니다. Linux가 listener credential을 `listen(2)` 시점에 고정하므로, systemd가 root로 listen한 뒤 fd 3을 넘긴 socket을 Runner가 UID 0으로 검증합니다.

executor는 승인 Script 요청 하나만 처리하고 terminal 응답을 기록한 뒤 client close를 잠시 기다리고 종료합니다. `KillMode=control-group`과 최종 SIGKILL이 background/double-fork/`setsid` 자손까지 정리하고 서비스가 완전히 내려간 뒤, systemd socket backlog의 다음 요청이 새 executor 프로세스를 기동합니다. 따라서 이전 Job의 프로세스가 다음 Job과 같은 executor cgroup에 공존하지 않습니다.

Runner main loop는 Job을 inline으로 하나씩 처리하며 `/run/releasedock-credentials/.runner.lock`의 non-blocking kernel `flock`으로 같은 host의 두 번째 Runner 프로세스를 거부합니다. 따라서 execute-only handoff root 아래 동시에 활성화되는 target credential은 하나뿐입니다.

## 상태 머신

```text
DRAFT -> UPLOADED
UPLOADED -> PENDING_REVIEW -> UNDER_REVIEW -> APPROVED -> QUEUED
UPLOADED ------------------------------------------------> QUEUED  (승인 불필요)
QUEUED -> VALIDATING -> PRE_CHECK(설정된 경우) -> EXTRACTING
       -> IMAGE_INSPECT -> IMAGE_LOAD -> IMAGE_TAG
       -> IMAGE_PUSH -> DEPLOYING -> VERIFYING -> SUCCESS
각 실행 단계 -> FAILED
SUCCESS/FAILED -> PENDING_REVIEW -> APPROVED -> VALIDATING(source image digest/reference)
               -> ROLLBACK -> VERIFYING -> ROLLED_BACK (수동 롤백 승인 시)
```

승인 정책이 비활성화됐거나 profile에 승인자가 지정되지 않았으면 `PENDING_REVIEW`, `APPROVED`, `REJECTED` 상태는 생성하지 않습니다.

수동 롤백 Job은 enqueue 시 source release와 그 image를 공급한 정확한 성공 DEPLOY basis Job ID를 함께 고정합니다. Runner는 이름·버전·profile 같은 변경 가능한 label로 source를 다시 추측하지 않고 해당 Job의 image reference/digest만 읽습니다. Harbor에서 각 digest를 다시 확인하고 digest-pinned reference를 승인된 ROLLBACK Script에 전달한 다음 health check를 수행합니다. 이 경로는 원본 artifact를 다시 압축 해제하거나 image load/tag/push를 반복하지 않으며, source image가 없거나 digest가 달라진 경우 Script 실행 전에 명시적으로 실패합니다.

애플리케이션/환경별 `deployment_heads`는 검증이 끝난 실제 배포 head와 그 image를 공급한 성공 DEPLOY basis Job을 가리킵니다. 각 DEPLOY Job은 enqueue 시점의 prior head를 immutable snapshot으로 보존합니다. Runner는 DEPLOY `SUCCESS` 또는 수동 ROLLBACK `ROLLED_BACK`을 기록하는 같은 transaction에서만 head를 갱신하고, 실패나 stale Job 복구에서는 head를 바꾸지 않습니다. 따라서 A→B→C 배포 후 C→B, B→A 순서의 연속 롤백도 생성 시각 추측 없이 검증된 이력을 따라갑니다.

## 설정 모델

환경 변수는 PostgreSQL 접속, 최초 관리자, bootstrap password, root encryption key, HTTP listen port 다섯 가지로 고정합니다. 다음 값은 관리자 API와 UI에서 관리합니다.

- Keycloak issuer, client ID/secret, scope, 자동 사용자 생성과 기본 역할
- 공개 HTTPS URL, Secure cookie와 MCP 허용 Origin
- artifact 위치와 업로드 용량 제한
- Harbor registry/robot credential
- 배포 대상 credential(SSH private key/Kubeconfig/token/opaque file), profile binding과 회전
- runner 활성 상태와 label(작업의 필수 label이 Runner label의 부분집합일 때만 claim; 현재 Runner당 동시 작업 1개)
- Runner polling/lock retry/settings refresh/heartbeat/stale 판정과 로그 chunk 크기
- script template/version/timeout
- approval 사용 여부, 보호 환경, 자기 승인과 반려 사유 정책
- AI endpoint/model/API key/max tokens(최대 262,144)/timeout
- health check와 수동 rollback 정책(`autoRollback`은 안전한 이전 배포 head snapshot이 없는 v0.1에서 비활성화)

secret 필드는 AES-256-GCM으로 암호화하며 secret 종류 또는 credential ID/version을 additional authenticated data로 묶습니다. 개인 API key는 생성·회전 시에만 원문을 반환하고 DB에는 prefix와 고엔트로피 토큰의 SHA-256 digest만 저장합니다.

배포 대상 credential은 Job에 ID/version snapshot만 저장합니다. Runner가 Script 직전에만 복호화해 tmpfs 기반 `/run/releasedock-credentials/job-<job-id>/credential`의 Runner 소유 `0640` handoff로 만들고, executor는 소유권·group·mode·symlink 부재를 검증한 뒤 전용 `/run/releasedock-executor-private`의 executor 소유 `0600` 파일로 복사하여 그 path/type만 Script 환경에 전달합니다. Registry auth와 containerd hosts도 같은 Runner RuntimeDirectory의 Job 경로에 둡니다. 양쪽 임시 파일은 오류/timeout을 포함한 모든 종료 경로에서 지우고, systemd RuntimeDirectory lifecycle 및 startup scavenger가 SIGKILL/재시작/전원장애 경로를 보완합니다. stdout/stderr에 그대로 출력된 plaintext는 chunk 경계를 넘어 redaction합니다. 단, 승인 Script는 credential을 실제 대상에 사용할 수 있는 신뢰 코드이므로 인코딩·변형·외부 전송까지 막는 DLP 경계는 아닙니다.

## API와 MCP

REST API는 `/api/v1` 아래에 버전 고정하며 브라우저 session과 scoped API key를 지원합니다. 로그와 AI 결과는 SSE를 기본으로 스트리밍합니다.

MCP는 `/mcp`의 authenticated Streamable HTTP transport로 제공합니다. 최소 도구는 application/release 조회, job/log 조회이며 상태 변경 도구에는 같은 REST permission과 승인 정책을 적용합니다. 브라우저 Origin은 관리자 설정의 허용 목록과 일치해야 합니다.

## UI 프레임워크

MUI를 사용합니다. 운영 화면에 필요한 Data Grid 형태, dialog, form validation, 접근성 속성, 일관된 theme을 빠르게 제공하면서 모든 asset을 Vite bundle 안에 포함할 수 있습니다. 외부 CDN과 웹 폰트를 쓰지 않고 시스템 글꼴을 사용합니다. 본문 기본 크기는 16px, 중요 제어는 16~18px로 유지합니다.
