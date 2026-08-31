import AutoAwesomeRoundedIcon from '@mui/icons-material/AutoAwesomeRounded';
import CheckRoundedIcon from '@mui/icons-material/CheckRounded';
import InfoOutlinedIcon from '@mui/icons-material/InfoOutlined';
import LanRoundedIcon from '@mui/icons-material/LanRounded';
import MemoryRoundedIcon from '@mui/icons-material/MemoryRounded';
import SaveRoundedIcon from '@mui/icons-material/SaveRounded';
import SecurityRoundedIcon from '@mui/icons-material/SecurityRounded';
import SettingsRoundedIcon from '@mui/icons-material/SettingsRounded';
import StorageRoundedIcon from '@mui/icons-material/StorageRounded';
import {
  Alert,
  Box,
  Button,
  Card,
  CardContent,
  Checkbox,
  CircularProgress,
  FormControlLabel,
  InputAdornment,
  MenuItem,
  Stack,
  Switch,
  TextField,
  Typography,
} from '@mui/material';
import { useEffect, useState, type ReactNode } from 'react';
import { api } from '../../api/client';
import { useAuth } from '../../auth/AuthContext';
import { PageError, PageLoading } from '../../components/Feedback';
import { PageHeader } from '../../components/PageHeader';
import { useAsync } from '../../hooks/useAsync';
import type { SettingValue } from '../../types/domain';

export type SettingSection = 'general' | 'oidc' | 'ai' | 'approval' | 'storage' | 'runner' | 'simple' | 'network';

const metadata: Record<SettingSection, { title: string; description: string; icon: typeof SettingsRoundedIcon }> = {
  general: { title: '일반 설정', description: '서비스 표시, 업로드 제한과 외부 API 보안 정책을 관리합니다.', icon: SettingsRoundedIcon },
  oidc: { title: 'Keycloak OIDC', description: 'Issuer, Client ID, Client Secret만으로 사내 SSO를 연결합니다.', icon: LanRoundedIcon },
  ai: { title: 'AI 설정', description: 'OpenAI 호환 API와 스트리밍, 모델별 토큰 한도를 관리합니다.', icon: AutoAwesomeRoundedIcon },
  approval: { title: '검토 및 승인 정책', description: '팀장 검토가 필요한 환경과 승인·반려 흐름을 선택적으로 적용합니다.', icon: SecurityRoundedIcon },
  storage: { title: '아티팩트 스토리지', description: '오프라인망 내 Local 디스크 또는 마운트된 NFS 저장소를 구성합니다.', icon: StorageRoundedIcon },
  runner: { title: 'Runner 설정', description: 'Job polling, lease 복구, heartbeat와 로그 전송 단위를 관리합니다.', icon: MemoryRoundedIcon },
  simple: { title: '심플 모드', description: '기본 화면 모드와, 서비스별 명령을 쓸지 공통 명령 하나를 쓸지 선택합니다.', icon: SettingsRoundedIcon },
  network: { title: '관리자 접근 IP', description: '관리 기능을 사용할 수 있는 출발지 IP를 제한합니다.', icon: LanRoundedIcon },
};

const initialValues: Record<SettingSection, SettingValue> = {
  general: { serviceName: 'ReleaseDock', artifactMaxSizeGb: 20, publicUrl: '', secureCookies: false, allowedOrigins: [] },
  oidc: { enabled: false, issuerUrl: '', clientId: '', clientSecret: '', redirectUrl: '', scopes: 'openid profile email', defaultRole: 'viewer', autoProvision: true },
  ai: { enabled: false, baseUrl: '', apiKey: '', model: '', streamingDefault: true, maxTokens: 32768 },
  approval: { enabled: false, protectedEnvironments: '', allowSelfApproval: false, requireRejectComment: true },
  storage: { driver: 'local', localPath: '/var/lib/releasedock/artifacts' },
  runner: { pollIntervalMs: 2000, lockRetryMs: 5000, settingsRefreshMs: 30000, heartbeatIntervalMs: 5000, staleJobAfterMs: 60000, logChunkBytes: 16384 },
  simple: { defaultUiMode: 'full', commandMode: 'PER_TARGET', sharedCommandPath: '', sharedCommandArgs: '', sharedWorkingDir: '', sharedTimeoutSeconds: 600, uploadRoot: '/var/lib/releasedock/simple' },
  network: { adminIpAllowlistEnabled: false, adminIpAllowlist: '', trustedProxyCidrs: '' },
};

function SettingCard({ title, description, children }: { title: string; description?: string; children: ReactNode }) {
  return (
    <Card>
      <CardContent sx={{ p: { xs: 2.5, md: 3.5 } }}>
        <Typography variant="h3">{title}</Typography>
        {description && <Typography color="text.secondary" variant="body2" sx={{ mt: 0.5, mb: 3 }}>{description}</Typography>}
        {!description && <Box sx={{ mb: 2.5 }} />}
        {children}
      </CardContent>
    </Card>
  );
}

interface FieldsProps {
  values: SettingValue;
  set: (key: string, value: unknown) => void;
  disabled: boolean;
}

function GeneralFields({ values, set, disabled }: FieldsProps) {
  const allowedOrigins = Array.isArray(values.allowedOrigins)
    ? values.allowedOrigins.join('\n')
    : String(values.allowedOrigins ?? '');
  return (
    <Stack spacing={2.25}>
      <TextField disabled={disabled} label="서비스 표시 이름" value={String(values.serviceName ?? '')} onChange={(e) => set('serviceName', e.target.value)} fullWidth />
      <TextField disabled={disabled} label="최대 아티팩트 크기" type="number" value={Number(values.artifactMaxSizeGb ?? 20)} onChange={(e) => set('artifactMaxSizeGb', Number(e.target.value))} fullWidth slotProps={{ input: { endAdornment: <InputAdornment position="end">GB</InputAdornment> } }} inputProps={{ min: 1, max: 1024 }} />
      <TextField disabled={disabled} label="공개 HTTPS URL" value={String(values.publicUrl ?? '')} onChange={(e) => set('publicUrl', e.target.value)} placeholder="https://releasedock.company.local" helperText="TLS reverse proxy에서 사용하는 외부 origin입니다. OIDC redirect와 MCP Origin 검증에 사용됩니다." fullWidth />
      <FormControlLabel control={<Switch disabled={disabled} checked={Boolean(values.secureCookies)} onChange={(e) => set('secureCookies', e.target.checked)} />} label="HTTPS Secure Cookie 강제" />
      <TextField disabled={disabled} label="MCP 추가 허용 Origin" value={allowedOrigins} onChange={(e) => set('allowedOrigins', e.target.value.split(/\r?\n/).map((value) => value.trim()).filter(Boolean))} multiline minRows={2} helperText="Origin을 한 줄에 하나씩 입력합니다. 공개 HTTPS URL은 자동으로 허용됩니다." fullWidth />
      <Alert severity="info" icon={<InfoOutlinedIcon />}>
        REST API와 MCP는 같은 RBAC 권한을 사용합니다. 개인 API 키별 권한 범위는 개인화 메뉴에서 언제든 변경하고 회전할 수 있습니다. 릴리즈 잠금과 안전 압축 해제는 항상 적용됩니다.
      </Alert>
      <Stack direction={{ xs: 'column', md: 'row' }} spacing={2}>
        <TextField label="REST API Base URL" value={`${window.location.origin}/api/v1`} fullWidth slotProps={{ input: { readOnly: true } }} />
        <TextField label="MCP Endpoint" value={`${window.location.origin}/mcp`} fullWidth slotProps={{ input: { readOnly: true } }} />
      </Stack>
    </Stack>
  );
}

function OidcFields({ values, set, disabled }: FieldsProps) {
  const enabled = Boolean(values.enabled);
  return (
    <Stack spacing={2.25}>
      <Alert severity={enabled ? 'success' : 'info'}>{enabled ? 'OIDC 로그인이 활성화되어 있습니다.' : 'OIDC가 비활성화되어 로컬 계정으로만 로그인합니다.'}</Alert>
      <FormControlLabel control={<Switch disabled={disabled} checked={enabled} onChange={(e) => set('enabled', e.target.checked)} />} label="Keycloak SSO 활성화" />
      <TextField label="Issuer URL" required={enabled} disabled={disabled || !enabled} value={String(values.issuerUrl ?? '')} onChange={(e) => set('issuerUrl', e.target.value)} helperText="예: https://keycloak.company.local/realms/company" fullWidth />
      <Stack direction={{ xs: 'column', md: 'row' }} spacing={2}>
        <TextField label="Client ID" required={enabled} disabled={disabled || !enabled} value={String(values.clientId ?? '')} onChange={(e) => set('clientId', e.target.value)} fullWidth />
        <TextField label="Client Secret" type="password" required={enabled && !values.secretConfigured} disabled={disabled || !enabled} value={String(values.clientSecret ?? '')} onChange={(e) => set('clientSecret', e.target.value)} helperText={values.secretConfigured ? '저장된 Secret을 변경할 때만 입력하세요.' : '서버에서 암호화하여 저장합니다.'} fullWidth />
      </Stack>
      <TextField
        label="Redirect URI (선택)"
        disabled={disabled || !enabled}
        value={String(values.redirectUrl ?? '')}
        onChange={(e) => set('redirectUrl', e.target.value)}
        fullWidth
        placeholder={String(values.effectiveRedirectUri ?? '')}
        helperText="비워 두면 서버가 자동으로 결정합니다. 일반 설정의 공개 HTTPS URL이 있으면 그 값을, 없으면 접속에 사용된 주소를 사용합니다. 특정 값을 강제해야 할 때만 입력하십시오."
      />
      {Boolean(values.effectiveRedirectUri) && (
        <TextField
          label="Keycloak에 등록할 Redirect URI"
          value={String(values.effectiveRedirectUri ?? '')}
          fullWidth
          slotProps={{ input: { readOnly: true } }}
          helperText="위 설정을 비워 두어도 서버는 이 값을 사용합니다. Keycloak Client의 Valid redirect URIs에 이 값을 등록하십시오."
        />
      )}
      <Stack direction={{ xs: 'column', md: 'row' }} spacing={2}>
        <TextField label="Scopes" disabled={disabled || !enabled} value={String(values.scopes ?? '')} onChange={(e) => set('scopes', e.target.value)} fullWidth />
      </Stack>
      <Stack direction={{ xs: 'column', md: 'row' }} spacing={2} alignItems={{ md: 'center' }}>
        <TextField select label="신규 사용자 기본 역할" disabled={disabled || !enabled} value={String(values.defaultRole ?? 'viewer')} onChange={(e) => set('defaultRole', e.target.value)} sx={{ minWidth: 260 }}>
          <MenuItem value="viewer">Viewer</MenuItem><MenuItem value="operator">Operator</MenuItem><MenuItem value="admin">Administrator</MenuItem>
        </TextField>
        <FormControlLabel control={<Checkbox checked={Boolean(values.autoProvision)} onChange={(e) => set('autoProvision', e.target.checked)} disabled={disabled || !enabled} />} label="첫 로그인 시 사용자 자동 생성" />
      </Stack>
    </Stack>
  );
}

function AiFields({ values, set, disabled }: FieldsProps) {
  const enabled = Boolean(values.enabled);
  return (
    <Stack spacing={2.25}>
      <Alert severity="info">AI 호출은 스트리밍이 기본이며, 모델 컨텍스트와 출력 한도는 최대 256K 토큰(262,144)까지 설정할 수 있습니다.</Alert>
      <FormControlLabel control={<Switch disabled={disabled} checked={enabled} onChange={(e) => set('enabled', e.target.checked)} />} label="AI 로그 분석 활성화" />
      <TextField label="OpenAI 호환 Endpoint" disabled={disabled || !enabled} required={enabled} value={String(values.baseUrl ?? '')} onChange={(e) => set('baseUrl', e.target.value)} placeholder="https://ai-gateway.internal/v1/chat/completions" fullWidth />
      <Stack direction={{ xs: 'column', md: 'row' }} spacing={2}>
        <TextField label="API Key" type="password" disabled={disabled || !enabled} value={String(values.apiKey ?? '')} onChange={(e) => set('apiKey', e.target.value)} helperText={values.keyConfigured ? '저장된 키를 변경할 때만 입력하세요.' : 'ENCRYPTION_KEY로 암호화 저장됩니다.'} fullWidth />
        <TextField label="Model" disabled={disabled || !enabled} required={enabled} value={String(values.model ?? '')} onChange={(e) => set('model', e.target.value)} placeholder="사내 모델 이름" fullWidth />
      </Stack>
      <TextField label="Max tokens" type="number" disabled={disabled || !enabled} value={Number(values.maxTokens ?? 32768)} onChange={(e) => set('maxTokens', Math.min(262144, Math.max(1, Number(e.target.value))))} inputProps={{ min: 1, max: 262144 }} helperText="최대 262,144" fullWidth />
      <Box>
        <FormControlLabel control={<Switch checked disabled />} label="응답 스트리밍 기본 사용 (항상 적용)" />
      </Box>
    </Stack>
  );
}

function ApprovalFields({ values, set, disabled }: FieldsProps) {
  const enabled = Boolean(values.enabled);
  return (
    <Stack spacing={2.25}>
      <Alert severity={enabled ? 'warning' : 'success'}>
        {enabled ? '지정 환경의 배포에 검토 → 승인/반려 절차가 추가됩니다.' : '승인 정책이 꺼져 있어 검토·승인·반려 단계 없이 바로 배포할 수 있습니다.'}
      </Alert>
      <FormControlLabel control={<Switch disabled={disabled} checked={enabled} onChange={(e) => set('enabled', e.target.checked)} />} label="팀장 검토 및 승인 프로세스 사용" />
      <Typography color="text.secondary">승인이 활성화되면 보호 환경, 아래 환경 코드 또는 배포 프로필에서 “승인 필요”로 지정한 작업에만 검토·승인·반려 단계가 적용됩니다.</Typography>
      <TextField
        disabled={disabled || !enabled}
        label="항상 승인할 환경 코드"
        value={String(values.protectedEnvironments ?? '')}
        onChange={(e) => set('protectedEnvironments', e.target.value)}
        placeholder="PRD, DR"
        helperText="쉼표로 구분합니다. 환경 자체의 보호 설정과 배포 프로필 승인 설정도 함께 적용됩니다."
        fullWidth
      />
      <FormControlLabel
        control={<Switch disabled={disabled || !enabled} checked={Boolean(values.allowSelfApproval)} onChange={(e) => set('allowSelfApproval', e.target.checked)} />}
        label="요청자 본인 승인 허용"
      />
      <FormControlLabel
        control={<Switch disabled={disabled || !enabled} checked={values.requireRejectComment === undefined ? true : Boolean(values.requireRejectComment)} onChange={(e) => set('requireRejectComment', e.target.checked)} />}
        label="반려 사유 필수"
      />
    </Stack>
  );
}

function StorageFields({ values, set, disabled }: FieldsProps) {
  return (
    <Stack spacing={2.25}>
      <TextField label="Storage Driver" value="Local / NFS Mount" disabled sx={{ maxWidth: 340 }} />
      <TextField disabled={disabled} label="아티팩트 저장 경로" value={String(values.localPath ?? '')} onChange={(e) => set('localPath', e.target.value)} helperText="systemd 보안 경계 안의 /var/lib/releasedock 하위 경로를 사용하세요. NFS도 이 하위에 마운트합니다." fullWidth />
    </Stack>
  );
}

function RunnerFields({ values, set, disabled }: FieldsProps) {
  const numberField = (key: string, label: string, min: number, max: number, helperText: string) => (
    <TextField
      key={key}
      disabled={disabled}
      label={label}
      type="number"
      value={Number(values[key] ?? min)}
      onChange={(event) => set(key, Number(event.target.value))}
      inputProps={{ min, max, step: 1 }}
      helperText={helperText}
      fullWidth
    />
  );
  return (
    <Stack spacing={2.25}>
      <Alert severity="info">Runner는 설정 새로고침 주기 안에 변경을 반영합니다. 실행 중인 Job의 고정된 입력과 제한값은 바뀌지 않습니다.</Alert>
      <Stack direction={{ xs: 'column', md: 'row' }} spacing={2}>
        {numberField('pollIntervalMs', 'Job Poll 주기 (ms)', 100, 60000, '대기 중 새 Job을 확인하는 간격')}
        {numberField('lockRetryMs', 'Target Lock 재시도 (ms)', 100, 300000, '같은 대상 배포 잠금 충돌 시 재시도 간격')}
      </Stack>
      <Stack direction={{ xs: 'column', md: 'row' }} spacing={2}>
        {numberField('settingsRefreshMs', '설정 새로고침 (ms)', 1000, 3600000, 'Runner가 DB 설정을 다시 읽는 간격')}
        {numberField('heartbeatIntervalMs', 'Heartbeat 주기 (ms)', 1000, 300000, 'Runner와 실행 Job의 생존 신호 간격')}
      </Stack>
      <Stack direction={{ xs: 'column', md: 'row' }} spacing={2}>
        {numberField('staleJobAfterMs', 'Stale Job 판정 (ms)', 5000, 3600000, 'Heartbeat 주기의 2배보다 커야 하며, stale Job은 자동 재실행하지 않고 실패 처리됩니다.')}
        {numberField('logChunkBytes', '로그 Chunk 크기 (byte)', 1024, 1048576, 'DB에 스트리밍 로그를 기록하는 전송 단위')}
      </Stack>
      <Alert severity="warning">Workspace 경로와 실행 PATH는 executor 보안 경계이므로 관리자 화면에서 변경할 수 없습니다.</Alert>
    </Stack>
  );
}

function SimpleFields({ values, set, disabled }: FieldsProps) {
  const shared = values.commandMode === 'SHARED';
  return (
    <Stack spacing={2.25}>
      <TextField select label="기본 화면 모드" disabled={disabled} value={String(values.defaultUiMode ?? 'full')} onChange={(e) => set('defaultUiMode', e.target.value)} helperText="로그인한 사용자가 처음 보게 될 화면입니다. 개인이 직접 전환한 경우에는 그 선택이 우선합니다." fullWidth>
        <MenuItem value="full">전체 모드 (릴리즈 오케스트레이션)</MenuItem>
        <MenuItem value="simple">심플 모드 (업로드 후 명령 실행)</MenuItem>
      </TextField>

      <TextField label="업로드 루트" disabled={disabled} value={String(values.uploadRoot ?? '')} onChange={(e) => set('uploadRoot', e.target.value)} helperText="심플 대상의 업로드 경로는 모두 이 경로 하위여야 합니다. /var/lib/releasedock 하위만 허용되며, 다른 위치를 쓰려면 releasedock-server.service의 ReadWritePaths도 함께 바꿔야 합니다." fullWidth />

      <TextField select label="명령 지정 방식" disabled={disabled} value={String(values.commandMode ?? 'PER_TARGET')} onChange={(e) => set('commandMode', e.target.value)} fullWidth
        helperText={shared
          ? '모든 대상이 아래 공통 명령 하나를 실행합니다. 대상 이름과 파일 경로는 환경변수로 전달됩니다.'
          : '심플 대상마다 등록된 명령을 각각 실행합니다.'}>
        <MenuItem value="PER_TARGET">서비스별 명령 (대상마다 따로 등록)</MenuItem>
        <MenuItem value="SHARED">공통 명령 하나 (모든 대상 동일)</MenuItem>
      </TextField>

      <Alert severity={shared ? 'info' : 'success'}>
        {shared
          ? '공통 명령 모드입니다. 심플 대상에 등록된 개별 명령은 사용되지 않지만 삭제되지 않으므로, 서비스별 모드로 되돌리면 그대로 다시 적용됩니다.'
          : '서비스별 명령 모드입니다. 이 모드로 저장하려면 활성 대상 모두에 실행 명령이 설정되어 있어야 합니다.'}
      </Alert>

      <TextField label="공통 명령 절대 경로" disabled={disabled} required={shared} value={String(values.sharedCommandPath ?? '')} onChange={(e) => set('sharedCommandPath', e.target.value)} placeholder="/opt/deploy/apply.sh" helperText="실행 가능한 일반 파일의 절대 경로여야 합니다." fullWidth />
      <TextField label="공통 명령 인자" disabled={disabled} value={String(values.sharedCommandArgs ?? '')} onChange={(e) => set('sharedCommandArgs', e.target.value)} multiline minRows={3} helperText="한 줄에 인자 하나입니다. {{artifact}}는 업로드한 파일의 절대 경로로 바뀝니다. 셸을 거치지 않으므로 인자 안의 특수문자는 그대로 전달됩니다." fullWidth />
      <Stack direction={{ xs: 'column', md: 'row' }} spacing={2}>
        <TextField label="공통 작업 디렉터리" disabled={disabled} value={String(values.sharedWorkingDir ?? '')} onChange={(e) => set('sharedWorkingDir', e.target.value)} helperText="비우면 각 대상의 업로드 경로에서 실행합니다." fullWidth />
        <TextField label="공통 제한 시간 (초)" type="number" disabled={disabled} value={Number(values.sharedTimeoutSeconds ?? 600)} onChange={(e) => set('sharedTimeoutSeconds', Number(e.target.value))} inputProps={{ min: 1, max: 86400 }} fullWidth />
      </Stack>

      <Alert severity="warning">
        심플 모드의 명령은 격리된 executor가 아니라 API 서비스 계정으로 직접 실행됩니다. 여기에 등록하는 명령은 그 계정의 권한으로 무엇이든 할 수 있으므로, 신뢰할 수 있는 스크립트만 등록하십시오.
      </Alert>
    </Stack>
  );
}

function NetworkFields({ values, set, disabled }: FieldsProps) {
  const enabled = Boolean(values.adminIpAllowlistEnabled);
  const callerIp = String(values.callerIp ?? '');
  const peerIp = String(values.peerIp ?? '');
  const forwardedFor = String(values.forwardedFor ?? '');
  const proxySuspected = Boolean(values.proxySuspected);
  const allowlist = String(values.adminIpAllowlist ?? '');
  const entries = allowlist.split('\n').map((line) => line.trim()).filter(Boolean);
  const alreadyListed = entries.includes(callerIp);
  const addToAllowlist = (value: string) =>
    set('adminIpAllowlist', entries.includes(value) ? allowlist : [...entries, value].join('\n'));
  const addTrustedProxy = (value: string) => {
    const proxies = String(values.trustedProxyCidrs ?? '').split('\n').map((line) => line.trim()).filter(Boolean);
    if (!proxies.includes(value)) set('trustedProxyCidrs', [...proxies, value].join('\n'));
  };

  return (
    <Stack spacing={2.25}>
      <Alert severity={enabled ? 'warning' : 'info'}>
        {enabled
          ? '허용 목록에 없는 IP에서는 모든 관리 API가 차단됩니다. 차단된 시도는 감사 로그에 기록됩니다.'
          : '현재는 IP 제한이 없습니다. 활성화하면 아래 목록의 IP에서만 관리 기능을 사용할 수 있습니다.'}
      </Alert>

      {/* Always rendered: an operator must be able to read the address the
          server actually sees before turning the allowlist on. */}
      <Alert
        severity={callerIp && !alreadyListed && enabled ? 'warning' : 'success'}
        action={!disabled && callerIp && !alreadyListed ? (
          <Button size="small" onClick={() => addToAllowlist(callerIp)}>목록에 추가</Button>
        ) : undefined}
      >
        <Stack spacing={0.25}>
          <span>
            현재 접속 IP: <strong>{callerIp || '확인할 수 없습니다'}</strong>
            {alreadyListed && ' — 허용 목록에 포함되어 있습니다.'}
            {!alreadyListed && callerIp && ' — 이 IP가 목록에 없으면 저장이 거부됩니다.'}
          </span>
          {Boolean(peerIp) && peerIp !== callerIp && (
            <Typography variant="caption" color="text.secondary">
              직접 접속 지점(프록시): {peerIp}
            </Typography>
          )}
        </Stack>
      </Alert>

      {proxySuspected && (
        <Alert
          severity="warning"
          action={!disabled && peerIp ? (
            <Button size="small" onClick={() => addTrustedProxy(peerIp)}>프록시 신뢰 등록</Button>
          ) : undefined}
        >
          리버스 프록시 뒤에 있는 것으로 보입니다. 요청에 <code>X-Forwarded-For: {forwardedFor}</code>가 있지만
          <strong> {peerIp}</strong>이(가) 신뢰 프록시로 등록되지 않아 무시하고 있습니다. 그래서 위의 현재 접속 IP가
          실제 클라이언트가 아니라 프록시 주소로 표시됩니다. 프록시를 신뢰 목록에 등록하고 저장하면 실제 클라이언트 IP가
          표시됩니다.
        </Alert>
      )}

      <FormControlLabel control={<Switch disabled={disabled} checked={enabled} onChange={(e) => set('adminIpAllowlistEnabled', e.target.checked)} />} label="관리자 접근 IP 제한 활성화" />

      <TextField
        label="허용 IP / CIDR"
        disabled={disabled}
        value={allowlist}
        onChange={(e) => set('adminIpAllowlist', e.target.value)}
        multiline
        minRows={4}
        fullWidth
        placeholder={callerIp}
        helperText="한 줄에 하나씩 입력합니다. 단일 주소(10.1.2.3)와 대역(192.168.10.0/24)을 모두 사용할 수 있습니다. 루프백(127.0.0.1, ::1)은 서버 콘솔 복구를 위해 항상 허용됩니다."
      />

      <TextField
        label="신뢰하는 리버스 프록시 CIDR"
        disabled={disabled}
        value={String(values.trustedProxyCidrs ?? '')}
        onChange={(e) => set('trustedProxyCidrs', e.target.value)}
        multiline
        minRows={2}
        fullWidth
        helperText="nginx 등 앞단 프록시를 쓴다면 반드시 입력하십시오. 비워 두면 모든 접속이 프록시 IP로 보여 IP 제한이 의미를 잃습니다. 여기에 등록된 주소에서 온 요청에 한해 X-Forwarded-For를 신뢰합니다."
      />

      <Alert severity="info" icon={<InfoOutlinedIcon />}>
        이 제한은 <code>/api/v1/admin/</code> 이하의 모든 관리 API와 <code>admin.</code> 권한을 요구하는 요청에 적용됩니다. 일반 사용자의 배포·조회 기능에는 영향을 주지 않습니다.
      </Alert>
    </Stack>
  );
}

export function SettingsPage({ section }: { section: SettingSection }) {
  const { hasPermission } = useAuth();
  // Simple mode has its own permission pair; the rest share admin.settings.*.
  const canWrite = hasPermission(section === 'simple' ? 'admin.simple.write' : 'admin.settings.write');
  const state = useAsync(() => api.getSettings(section), [section]);
  const [values, setValues] = useState<SettingValue>(initialValues[section]);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{ severity: 'success' | 'error'; text: string }>();
  const meta = metadata[section];
  const Icon = meta.icon;

  useEffect(() => {
    setValues({ ...initialValues[section], ...(state.data ?? {}) });
  }, [state.data, section]);

  const set = (key: string, value: unknown) => {
    setValues((current) => ({ ...current, [key]: value }));
    setMessage(undefined);
  };
  const save = async () => {
    setSaving(true);
    setMessage(undefined);
    try {
      const saved = await api.saveSettings(section, values);
      setValues({ ...initialValues[section], ...saved });
      setMessage({ severity: 'success', text: '설정을 저장했습니다. 새 작업부터 적용됩니다.' });
    } catch (reason) {
      setMessage({ severity: 'error', text: reason instanceof Error ? reason.message : '설정을 저장하지 못했습니다.' });
    } finally {
      setSaving(false);
    }
  };

  return (
    <>
      <PageHeader title={meta.title} description={meta.description} action={canWrite ? <Button variant="contained" startIcon={saving ? <CircularProgress size={18} /> : <SaveRoundedIcon />} disabled={saving || state.loading} onClick={() => void save()}>{saving ? '저장 중…' : '변경사항 저장'}</Button> : undefined} />
      {state.loading && <PageLoading label="설정을 불러오는 중입니다" />}
      {state.error && !state.loading && <PageError error={state.error} onRetry={() => void state.reload()} />}
      {!state.loading && !state.error && (
        <Stack spacing={2.5}>
          {message && <Alert severity={message.severity} icon={message.severity === 'success' ? <CheckRoundedIcon /> : undefined}>{message.text}</Alert>}
          {!canWrite && <Alert severity="info">이 설정을 조회할 수는 있지만 변경할 권한은 없습니다.</Alert>}
          <SettingCard title={meta.title} description={`${meta.description} 민감한 값은 서버에서 암호화되어 저장됩니다.`}>
            <Stack direction="row" alignItems="center" spacing={1} sx={{ mb: 3, color: 'primary.light' }}><Icon /><Typography variant="body2" fontWeight={750}>관리자 전용 설정</Typography></Stack>
            {section === 'general' && <GeneralFields values={values} set={set} disabled={!canWrite} />}
            {section === 'oidc' && <OidcFields values={values} set={set} disabled={!canWrite} />}
            {section === 'ai' && <AiFields values={values} set={set} disabled={!canWrite} />}
            {section === 'approval' && <ApprovalFields values={values} set={set} disabled={!canWrite} />}
            {section === 'storage' && <StorageFields values={values} set={set} disabled={!canWrite} />}
            {section === 'runner' && <RunnerFields values={values} set={set} disabled={!canWrite} />}
            {section === 'simple' && <SimpleFields values={values} set={set} disabled={!canWrite} />}
            {section === 'network' && <NetworkFields values={values} set={set} disabled={!canWrite} />}
          </SettingCard>
        </Stack>
      )}
    </>
  );
}
