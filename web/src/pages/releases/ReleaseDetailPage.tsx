import ApprovalRoundedIcon from '@mui/icons-material/ApprovalRounded';
import CancelRoundedIcon from '@mui/icons-material/CancelRounded';
import CheckCircleRoundedIcon from '@mui/icons-material/CheckCircleRounded';
import ContentCopyRoundedIcon from '@mui/icons-material/ContentCopyRounded';
import DownloadDoneRoundedIcon from '@mui/icons-material/DownloadDoneRounded';
import EditRoundedIcon from '@mui/icons-material/EditRounded';
import ErrorRoundedIcon from '@mui/icons-material/ErrorRounded';
import HourglassTopRoundedIcon from '@mui/icons-material/HourglassTopRounded';
import PlayArrowRoundedIcon from '@mui/icons-material/PlayArrowRounded';
import ReplayRoundedIcon from '@mui/icons-material/ReplayRounded';
import SendRoundedIcon from '@mui/icons-material/SendRounded';
import {
  Alert,
  Box,
  Button,
  Card,
  CardContent,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Divider,
  Grid,
  IconButton,
  Stack,
  Tab,
  Tabs,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material';
import { alpha } from '@mui/material/styles';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useParams } from 'react-router-dom';
import { api } from '../../api/client';
import { useAuth } from '../../auth/AuthContext';
import { PageError, PageLoading } from '../../components/Feedback';
import { PageHeader } from '../../components/PageHeader';
import { StatusChip } from '../../components/StatusChip';
import { useAsync } from '../../hooks/useAsync';
import type { Release, ReleaseStep, StepStatus } from '../../types/domain';
import { formatBytes, formatDate, formatDuration } from '../../utils/format';

interface LogEntry {
  id: number;
  timestamp?: string;
  level?: string;
  step?: string;
  message: string;
}

type ReleaseAction = 'submit-review' | 'review' | 'approve' | 'reject' | 'deploy' | 'rollback' | 'retry' | 'edit';

function useReleaseLogs(releaseId: string | undefined, enabled: boolean) {
  const [logs, setLogs] = useState<LogEntry[]>([]);
  const [connected, setConnected] = useState(false);
  const sequence = useRef(0);

  useEffect(() => {
    if (!releaseId || !enabled) return;
    const source = new EventSource(api.releaseLogStreamUrl(releaseId), { withCredentials: true });
    source.onopen = () => setConnected(true);
    const receiveLog = (rawEvent: Event) => {
      const event = rawEvent as MessageEvent<string>;
      let entry: Omit<LogEntry, 'id'>;
      try {
        const parsed = JSON.parse(event.data) as Partial<LogEntry> & { createdAt?: string; stream?: string; data?: Partial<LogEntry> & { createdAt?: string; stream?: string } };
        const value = parsed.data ?? parsed;
        entry = {
          timestamp: value.timestamp ?? value.createdAt,
          level: value.level ?? value.stream,
          step: value.step,
          message: value.message || event.data,
        };
      } catch {
        entry = { message: event.data };
      }
      sequence.current += 1;
      setLogs((current) => [...current.slice(-4998), { ...entry, id: sequence.current }]);
    };
    source.onmessage = receiveLog;
    source.addEventListener('log', receiveLog);
    source.addEventListener('end', () => {
      setConnected(false);
      source.close();
    });
    source.onerror = () => setConnected(false);
    return () => source.close();
  }, [releaseId, enabled]);

  return { logs, connected, clear: () => setLogs([]) };
}

function StepIcon({ status }: { status: StepStatus }) {
  if (status === 'SUCCESS') return <CheckCircleRoundedIcon color="success" />;
  if (status === 'FAILED') return <ErrorRoundedIcon color="error" />;
  if (status === 'RUNNING') return <CircularProgress size={22} />;
  if (status === 'SKIPPED') return <CancelRoundedIcon color="disabled" />;
  return <HourglassTopRoundedIcon color="disabled" />;
}

function StepTimeline({ steps = [] }: { steps?: ReleaseStep[] }) {
  const defaults: ReleaseStep[] = [
    { id: 'validate', name: '패키지 무결성 검증', type: 'VALIDATE', status: 'PENDING' },
    { id: 'pre-check', name: '관리자 사전 검사', type: 'PRE_CHECK', status: 'PENDING' },
    { id: 'extract', name: '안전한 압축 해제', type: 'EXTRACT', status: 'PENDING' },
    { id: 'image', name: '컨테이너 이미지 검사', type: 'IMAGE_IMPORT', status: 'PENDING' },
    { id: 'push', name: 'Harbor Registry Push', type: 'IMAGE_PUSH', status: 'PENDING' },
    { id: 'deploy', name: '배포 스크립트 실행', type: 'DEPLOY', status: 'PENDING' },
    { id: 'health', name: 'Health Check', type: 'HEALTH_CHECK', status: 'PENDING' },
  ];
  const items = steps.length ? steps : defaults;
  return (
    <Stack>
      {items.map((step, index) => (
        <Stack key={step.id || `${step.type}-${index}`} direction="row" spacing={2} sx={{ minHeight: 82 }}>
          <Box sx={{ position: 'relative', width: 28, flex: '0 0 28px', textAlign: 'center' }}>
            {index < items.length - 1 && <Box sx={{ position: 'absolute', zIndex: 0, top: 28, bottom: -4, left: '50%', width: 2, bgcolor: 'divider', transform: 'translateX(-50%)' }} />}
            <Box sx={{ position: 'relative', zIndex: 1, bgcolor: 'background.paper', lineHeight: 1 }}><StepIcon status={step.status} /></Box>
          </Box>
          <Box sx={{ flex: 1, pb: 2.5 }}>
            <Stack direction={{ xs: 'column', sm: 'row' }} alignItems={{ xs: 'flex-start', sm: 'center' }} gap={1}>
              <Typography fontWeight={750}>{step.name}</Typography>
              <StatusChip status={step.status} />
              <Typography variant="caption" color="text.secondary" sx={{ ml: { sm: 'auto' } }}>{formatDuration(step.durationMs)}</Typography>
            </Stack>
            {(step.message || step.startedAt) && (
              <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>
                {step.message || `${formatDate(step.startedAt)} 시작`}{step.exitCode !== undefined ? ` · 종료 코드 ${step.exitCode}` : ''}
              </Typography>
            )}
          </Box>
        </Stack>
      ))}
    </Stack>
  );
}

function DetailField({ label, value, mono = false }: { label: string; value?: string; mono?: boolean }) {
  return (
    <Box>
      <Typography variant="caption" color="text.secondary" fontWeight={650}>{label}</Typography>
      <Typography sx={{ mt: 0.25, overflowWrap: 'anywhere', fontFamily: mono ? 'ui-monospace, SFMono-Regular, Consolas, monospace' : undefined }}>{value || '—'}</Typography>
    </Box>
  );
}

function ReleaseActions({ release, onUpdated }: { release: Release; onUpdated: (value: Release) => void }) {
  const { hasPermission } = useAuth();
  const [running, setRunning] = useState<ReleaseAction>();
  const [dialogAction, setDialogAction] = useState<ReleaseAction>();
  const [comment, setComment] = useState('');
  const [editVersion, setEditVersion] = useState(release.version);
  const [editNotes, setEditNotes] = useState(release.notes ?? '');
  const [editFile, setEditFile] = useState<File>();
  const [error, setError] = useState<string>();
  const canUploadArtifact = hasPermission('releases.create');
  const validEditFile = !editFile || editFile.name.toLowerCase().endsWith('.tar') || editFile.name.toLowerCase().endsWith('.tar.gz');

  const available = useMemo(() => {
    const actions: ReleaseAction[] = [];
    if (['UPLOADED', 'READY', 'REJECTED'].includes(release.status) && hasPermission('releases.submit')) actions.push(release.approval?.required ? 'submit-review' : 'deploy');
    if (release.status === 'REJECTED' && hasPermission('releases.write')) actions.unshift('edit');
    if (release.status === 'PENDING_REVIEW' && hasPermission('releases.review')) actions.push('review');
    if (['PENDING_REVIEW', 'UNDER_REVIEW'].includes(release.status) && hasPermission('releases.approve')) actions.push('approve');
    if (['PENDING_REVIEW', 'UNDER_REVIEW'].includes(release.status) && hasPermission('releases.reject')) actions.push('reject');
    if (release.status === 'APPROVED' && hasPermission('releases.submit')) actions.push('deploy');
    if (release.retryEligible && hasPermission('releases.submit')) actions.push('retry');
    if (release.rollbackEligible && hasPermission('releases.submit')) actions.push('rollback');
    return actions;
  }, [release, hasPermission]);

  const labels: Record<ReleaseAction, string> = { 'submit-review': '검토 요청', review: '검토 시작', approve: '승인', reject: '반려', deploy: release.requestedOperation === 'ROLLBACK' ? '롤백 실행' : '배포 시작', rollback: '롤백 요청', retry: release.requestedOperation === 'ROLLBACK' ? '롤백 재시도' : '배포 재시도', edit: '릴리즈 수정' };
  const icons: Record<ReleaseAction, typeof SendRoundedIcon> = { 'submit-review': SendRoundedIcon, review: ApprovalRoundedIcon, approve: ApprovalRoundedIcon, reject: CancelRoundedIcon, deploy: release.requestedOperation === 'ROLLBACK' ? ReplayRoundedIcon : PlayArrowRoundedIcon, rollback: ReplayRoundedIcon, retry: ReplayRoundedIcon, edit: EditRoundedIcon };

  const execute = async (action: ReleaseAction) => {
    setRunning(action);
    setError(undefined);
    try {
      if (action === 'edit') {
        if (!release.deploymentProfileId) throw new Error('배포 프로필 정보가 없어 릴리즈를 수정할 수 없습니다.');
        await api.updateRelease(release.id, { version: editVersion.trim(), notes: editNotes.trim(), deploymentProfileId: release.deploymentProfileId });
        if (editFile) await api.uploadReleaseArtifact(release.id, editFile);
        onUpdated(await api.release(release.id));
        setEditFile(undefined);
        setDialogAction(undefined);
        return;
      }
      onUpdated(await api.releaseAction(release.id, action, comment.trim() || undefined));
      setDialogAction(undefined);
      setComment('');
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '작업을 완료하지 못했습니다.');
    } finally {
      setRunning(undefined);
    }
  };

  if (!available.length) return null;
  return (
    <>
      <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
        {available.map((action) => {
          const Icon = icons[action];
          const dangerous = action === 'reject' || action === 'rollback' || (action === 'deploy' && release.requestedOperation === 'ROLLBACK');
          return <Button key={action} variant={action === 'deploy' || action === 'approve' ? 'contained' : 'outlined'} color={dangerous ? 'error' : 'primary'} startIcon={<Icon />} onClick={() => {
            if (action === 'edit') {
              setEditVersion(release.version);
              setEditNotes(release.notes ?? '');
              setEditFile(undefined);
            }
            setDialogAction(action);
          }}>{labels[action]}</Button>;
        })}
      </Stack>
      <Dialog open={Boolean(dialogAction)} onClose={() => !running && setDialogAction(undefined)} fullWidth maxWidth="sm">
        <DialogTitle>{dialogAction ? labels[dialogAction] : ''}</DialogTitle>
        <DialogContent>
          {dialogAction === 'edit' ? (
            <Stack spacing={2} sx={{ pt: 0.5 }}>
              <Alert severity="info">반려 사유를 반영한 뒤 저장하고, 별도의 재검토 요청 또는 배포 시작을 선택하세요.</Alert>
              <TextField label="릴리즈 버전" required value={editVersion} onChange={(event) => setEditVersion(event.target.value)} inputProps={{ maxLength: 128 }} />
              <TextField label="릴리즈 메모" multiline minRows={3} value={editNotes} onChange={(event) => setEditNotes(event.target.value)} inputProps={{ maxLength: 2000 }} />
              {canUploadArtifact && (
                <Button component="label" variant="outlined" startIcon={<DownloadDoneRoundedIcon />}>
                  {editFile ? `교체 패키지: ${editFile.name}` : '패키지 교체 (선택)'}
                  <input hidden type="file" accept=".tar,.tar.gz,application/x-tar,application/gzip" onChange={(event) => setEditFile(event.target.files?.[0])} />
                </Button>
              )}
              {editFile && !validEditFile && <Alert severity="error">.tar 또는 .tar.gz 패키지만 사용할 수 있습니다.</Alert>}
            </Stack>
          ) : <Typography color="text.secondary" sx={{ mb: 2 }}>
            {dialogAction === 'deploy' && (release.requestedOperation === 'ROLLBACK' ? '승인된 이전 정상 버전으로 롤백을 시작합니다.' : '배포 파이프라인을 시작합니다. 같은 대상의 중복 배포는 서버에서 잠금 처리됩니다.')}
            {dialogAction === 'rollback' && '이전의 정상 버전으로 복구를 요청합니다. 운영 영향을 확인하세요.'}
            {dialogAction === 'retry' && '실패한 작업을 같은 승인된 입력으로 새 작업에서 다시 실행합니다. 이전 실패 로그와 이력은 보존됩니다.'}
            {dialogAction === 'approve' && `검토를 완료하고 ${release.requestedOperation === 'ROLLBACK' ? '롤백' : '배포'} 가능 상태로 전환합니다.`}
            {dialogAction === 'review' && '요청 내용과 대상을 확인하고 검토 중 상태로 전환합니다.'}
            {dialogAction === 'reject' && '릴리즈를 반려합니다. 사유를 반드시 입력하세요.'}
            {dialogAction === 'submit-review' && '팀장 또는 승인 권한자에게 검토를 요청합니다.'}
          </Typography>}
          {dialogAction !== 'edit' && <TextField label={dialogAction === 'reject' ? '반려 사유' : '의견 (선택)'} required={dialogAction === 'reject'} multiline minRows={3} fullWidth value={comment} onChange={(event) => setComment(event.target.value)} />}
          {error && <Alert severity="error" sx={{ mt: 2 }}>{error}</Alert>}
        </DialogContent>
        <DialogActions sx={{ p: 2.5 }}>
          <Button onClick={() => setDialogAction(undefined)} disabled={Boolean(running)}>취소</Button>
          <Button variant="contained" color={dialogAction === 'reject' || dialogAction === 'rollback' ? 'error' : 'primary'} disabled={Boolean(running) || (dialogAction === 'reject' && !comment.trim()) || (dialogAction === 'edit' && (!editVersion.trim() || !validEditFile))} onClick={() => dialogAction && void execute(dialogAction)}>
            {running ? '처리 중…' : '확인'}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}

function LogPanel({ releaseId, enabled }: { releaseId: string; enabled: boolean }) {
  const { logs, connected, clear } = useReleaseLogs(releaseId, enabled);
  const endRef = useRef<HTMLDivElement>(null);
  const [autoScroll, setAutoScroll] = useState(true);
  useEffect(() => {
    if (autoScroll) endRef.current?.scrollIntoView({ block: 'nearest' });
  }, [logs, autoScroll]);

  const copy = async () => navigator.clipboard.writeText(logs.map((log) => `${log.timestamp || ''} ${log.level || ''} ${log.message}`).join('\n'));
  return (
    <Box>
      <Stack direction={{ xs: 'column', sm: 'row' }} alignItems={{ xs: 'flex-start', sm: 'center' }} gap={1.25} sx={{ mb: 1.5 }}>
        <Stack direction="row" alignItems="center" spacing={1}>
          <Box sx={{ width: 9, height: 9, borderRadius: '50%', bgcolor: connected ? 'success.main' : 'text.disabled', boxShadow: connected ? '0 0 0 5px rgba(79,209,165,.12)' : undefined }} />
          <Typography variant="body2" color="text.secondary">{connected ? '실시간 로그 연결됨' : '로그 연결 대기'}</Typography>
        </Stack>
        <Stack direction="row" spacing={0.5} sx={{ ml: { sm: 'auto' } }}>
          <Button size="small" onClick={() => setAutoScroll((value) => !value)}>{autoScroll ? '자동 스크롤 켜짐' : '자동 스크롤 꺼짐'}</Button>
          <Tooltip title="로그 복사"><span><IconButton size="small" aria-label="전체 로그 복사" disabled={!logs.length} onClick={() => void copy()}><ContentCopyRoundedIcon fontSize="small" /></IconButton></span></Tooltip>
          <Button size="small" onClick={clear} disabled={!logs.length}>지우기</Button>
        </Stack>
      </Stack>
      <Box
        role="log"
        aria-live="polite"
        sx={{
          height: 430,
          overflow: 'auto',
          borderRadius: 2,
          bgcolor: '#050911',
          border: '1px solid',
          borderColor: 'divider',
          p: 2,
          fontFamily: 'ui-monospace, SFMono-Regular, Consolas, monospace',
          fontSize: '0.875rem',
          lineHeight: 1.7,
        }}
      >
        {logs.length ? logs.map((log) => (
          <Box key={log.id} sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', sm: '160px 80px 1fr' }, gap: { sm: 1 }, color: log.level === 'ERROR' ? 'error.light' : log.level === 'WARN' ? 'warning.light' : 'text.primary' }}>
            <Box component="span" sx={{ color: 'text.disabled' }}>{log.timestamp ? formatDate(log.timestamp) : '—'}</Box>
            <Box component="span" sx={{ color: 'text.secondary' }}>{log.level || 'INFO'}</Box>
            <Box component="span" sx={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{log.message}</Box>
          </Box>
        )) : <Typography color="text.secondary" fontFamily="inherit">실행 로그가 도착하면 여기에 실시간으로 표시됩니다.</Typography>}
        <div ref={endRef} />
      </Box>
    </Box>
  );
}

export function ReleaseDetailPage() {
  const { id } = useParams<{ id: string }>();
  const state = useAsync(() => id ? api.release(id) : Promise.reject(new Error('릴리즈 ID가 없습니다.')), [id]);
  const [tab, setTab] = useState(0);
  const release = state.data;
  const executionStatuses: Release['status'][] = [
    'QUEUED', 'VALIDATING', 'PRE_CHECK', 'EXTRACTING', 'IMAGE_IMPORT', 'IMAGE_INSPECT',
    'IMAGE_LOAD', 'IMAGE_TAG', 'IMAGE_PUSH', 'DEPLOYING', 'VERIFYING', 'ROLLBACK',
  ];
  const active = Boolean(release && ([...executionStatuses, 'PENDING_REVIEW', 'UNDER_REVIEW', 'APPROVED'] as Release['status'][]).includes(release.status));
  const hasExecutionLogs = Boolean(release && ([...executionStatuses, 'SUCCESS', 'FAILED', 'ROLLED_BACK'] as Release['status'][]).includes(release.status));

  const reload = useCallback(() => void state.reload(), [state]);
  useEffect(() => {
    if (!active) return;
    const timer = window.setInterval(reload, 5000);
    return () => window.clearInterval(timer);
  }, [active, reload]);

  if (state.loading && !release) return <PageLoading label="릴리즈 상세 정보를 불러오는 중입니다" />;
  if (state.error && !release) return <PageError error={state.error} onRetry={() => void state.reload()} />;
  if (!release) return null;

  return (
    <>
      <PageHeader
        title={`${release.applicationName || '릴리즈'} v${release.version}`}
        description={`${release.environmentName || release.environmentId} 환경 · ${release.artifactName || '릴리즈 패키지'}`}
        crumbs={[{ label: '릴리즈', to: '/releases' }, { label: `v${release.version}` }]}
        action={<ReleaseActions release={release} onUpdated={state.setData} />}
      />
      {state.error && <Alert severity="warning" sx={{ mb: 2 }}>최신 상태 갱신에 실패했습니다: {state.error.message}</Alert>}
      <Grid container spacing={2.5}>
        <Grid size={{ xs: 12, xl: 8 }}>
          <Card>
            <Box sx={{ px: 2.5, pt: 1 }}>
              <Tabs value={tab} onChange={(_, value: number) => setTab(value)} variant="scrollable" allowScrollButtonsMobile aria-label="릴리즈 상세 탭">
                <Tab label="실행 단계" id="tab-steps" />
                <Tab label="실시간 로그" id="tab-logs" />
                <Tab label={`이미지 (${release.images?.length || 0})`} id="tab-images" />
              </Tabs>
            </Box>
            <Divider />
            <CardContent sx={{ p: { xs: 2.5, md: 3.5 } }}>
              {tab === 0 && <StepTimeline steps={release.steps} />}
              {tab === 1 && (hasExecutionLogs
                ? <LogPanel releaseId={release.id} enabled />
                : <Typography color="text.secondary">배포 작업이 시작되면 실행 로그를 확인할 수 있습니다.</Typography>)}
              {tab === 2 && (
                <Stack spacing={1.5}>
                  {release.images?.length ? release.images.map((image) => (
                    <Box key={`${image.repository}:${image.tag}`} sx={{ p: 2, borderRadius: 2, border: '1px solid', borderColor: 'divider', bgcolor: alpha('#07101f', 0.35) }}>
                      <Stack direction={{ xs: 'column', md: 'row' }} gap={1}>
                        <Box sx={{ flex: 1 }}><Typography fontWeight={750}>{image.repository}:{image.tag}</Typography><Typography variant="body2" color="text.secondary" sx={{ mt: 0.5, overflowWrap: 'anywhere', fontFamily: 'ui-monospace, monospace' }}>{image.digest || 'Digest 확인 전'}</Typography></Box>
                        <Typography color="text.secondary">{formatBytes(image.size)}</Typography>
                      </Stack>
                    </Box>
                  )) : <Typography color="text.secondary">발견된 컨테이너 이미지가 없습니다.</Typography>}
                </Stack>
              )}
            </CardContent>
          </Card>
        </Grid>
        <Grid size={{ xs: 12, xl: 4 }}>
          <Stack spacing={2.5}>
            <Card>
              <CardContent sx={{ p: 3 }}>
                <Stack direction="row" alignItems="center" justifyContent="space-between" sx={{ mb: 2.5 }}>
                  <Typography variant="h3">현재 상태</Typography><StatusChip status={release.status} size="medium" />
                </Stack>
                <Stack spacing={2}>
                  <DetailField label="애플리케이션" value={release.applicationName || release.applicationId} />
                  <DetailField label="배포 환경" value={release.environmentName || release.environmentId} />
                  <DetailField label="배포 프로필" value={release.deploymentProfileName || release.deploymentProfileId} />
                  <DetailField label="요청 작업" value={release.requestedOperation === 'ROLLBACK' ? '롤백' : '배포'} />
                  {release.requestedOperation === 'ROLLBACK' && <DetailField label="롤백 대상 버전" value={release.rollbackSourceVersion || release.rollbackSourceReleaseId} />}
                  <DetailField label="등록 시각" value={formatDate(release.createdAt)} />
                  <DetailField label="완료 시각" value={formatDate(release.finishedAt)} />
                </Stack>
              </CardContent>
            </Card>
            <Card>
              <CardContent sx={{ p: 3 }}>
                <Stack direction="row" spacing={1} alignItems="center" sx={{ mb: 2.5 }}><DownloadDoneRoundedIcon color="primary" /><Typography variant="h3">아티팩트</Typography></Stack>
                <Stack spacing={2}>
                  <DetailField label="파일명" value={release.artifactName} />
                  <DetailField label="크기" value={formatBytes(release.artifactSize)} />
                  <DetailField label="SHA256" value={release.checksum} mono />
                </Stack>
              </CardContent>
            </Card>
            {release.approval?.required && (
              <Card>
                <CardContent sx={{ p: 3 }}>
                  <Typography variant="h3" sx={{ mb: 2 }}>{release.requestedOperation === 'ROLLBACK' ? '롤백 ' : ''}검토 및 승인</Typography>
                  <Stack spacing={1.5}><DetailField label="승인 상태" value={release.approval.status} /><DetailField label="검토자" value={release.approval.reviewer?.displayName} /><DetailField label="의견" value={release.approval.comment} /></Stack>
                </CardContent>
              </Card>
            )}
          </Stack>
        </Grid>
      </Grid>
    </>
  );
}
