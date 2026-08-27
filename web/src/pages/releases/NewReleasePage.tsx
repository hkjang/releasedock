import ArrowForwardRoundedIcon from '@mui/icons-material/ArrowForwardRounded';
import CheckCircleRoundedIcon from '@mui/icons-material/CheckCircleRounded';
import CloseRoundedIcon from '@mui/icons-material/CloseRounded';
import CloudUploadRoundedIcon from '@mui/icons-material/CloudUploadRounded';
import InfoOutlinedIcon from '@mui/icons-material/InfoOutlined';
import RocketLaunchRoundedIcon from '@mui/icons-material/RocketLaunchRounded';
import {
  Alert,
  Box,
  Button,
  Card,
  CardContent,
  Chip,
  CircularProgress,
  Grid,
  IconButton,
  Stack,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material';
import { alpha } from '@mui/material/styles';
import { type DragEvent, type FormEvent, useEffect, useMemo, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api, ApiError } from '../../api/client';
import { useAuth } from '../../auth/AuthContext';
import { PageHeader } from '../../components/PageHeader';
import type { QuickReleasePreflight } from '../../types/domain';
import { formatBytes } from '../../utils/format';
import { parseReleasePackageName } from '../../utils/releasePackage';

function preflightErrorMessage(reason: unknown): string {
  if (reason instanceof ApiError) {
    if (reason.code === 'deployment_preset_not_found') return '이 파일명에 연결된 배포 설정이 없습니다. 관리자에게 파일명 접두어 등록을 요청하세요.';
    if (reason.code === 'release_version_exists') return '같은 서비스와 배포 대상에 동일한 버전이 이미 등록되어 있습니다. 새 버전의 패키지를 선택하세요.';
    if (reason.code === 'quick_upgrade_required') return 'Quick Deploy는 현재 운영 버전보다 높은 SemVer만 배포할 수 있습니다. 이전 버전 복구는 릴리즈 상세에서 요청하세요.';
    if (reason.code === 'current_version_not_semver') return '현재 운영 버전이 SemVer 형식이 아닙니다. 운영자 고급 등록으로 SemVer 기준 버전을 먼저 배포하세요.';
    if (reason.code === 'job_conflict') return '이 배포 대상에서 다른 배포가 진행 중입니다. 완료된 뒤 다시 확인하세요.';
    if (reason.status === 403) return '패키지 분석 권한이 없습니다. 관리자에게 배포 권한을 요청하세요.';
    if (reason.status === 409) return '배포 설정을 확정하지 못했습니다. 잠시 후 다시 선택해 주세요.';
  }
  return reason instanceof Error ? reason.message : '패키지 정보를 확인하지 못했습니다.';
}

function quickReleaseErrorMessage(reason: unknown): string {
  if (!(reason instanceof ApiError)) return reason instanceof Error ? reason.message : '배포를 요청하지 못했습니다.';
  if (reason.code === 'job_conflict') return '이 배포 대상에서 다른 배포가 진행 중입니다. 완료된 뒤 같은 버튼으로 다시 요청할 수 있습니다.';
  if (reason.code === 'artifact_too_large') return '패키지가 관리자가 설정한 최대 업로드 크기를 초과합니다.';
  if (reason.code === 'commit_result_unknown') return '요청 저장 결과를 확정할 수 없습니다. 중복 요청하지 말고 릴리즈 목록에서 결과를 확인하거나 운영자에게 문의하세요.';
  if (reason.status === 403) return '원클릭 배포 요청 권한이 없습니다.';
  return reason.message || '배포를 요청하지 못했습니다.';
}

function ComparisonValue({ label, value, emphasized = false }: { label: string; value?: string; emphasized?: boolean }) {
  return (
    <Box sx={{ flex: 1, minWidth: 0 }}>
      <Typography variant="body2" color="text.secondary" fontWeight={650}>{label}</Typography>
      <Typography sx={{ mt: 0.5, fontSize: { xs: '1.35rem', md: '1.7rem' }, fontWeight: 800, color: emphasized ? 'primary.light' : 'text.primary', overflowWrap: 'anywhere' }}>
        {value || '첫 배포'}
      </Typography>
    </Box>
  );
}

function PreflightSummary({ value }: { value: QuickReleasePreflight }) {
  const ready = value.readiness.profileReady && value.readiness.runnerAvailable;
  const approvalRequired = value.nextAction === 'APPROVAL';
  return (
    <Stack spacing={2.5} aria-live="polite">
      <Stack direction={{ xs: 'column', sm: 'row' }} alignItems={{ xs: 'flex-start', sm: 'center' }} gap={1}>
        <Stack direction="row" alignItems="center" spacing={1}>
          <CheckCircleRoundedIcon color="success" />
          <Typography variant="h3">패키지 자동 인식 완료</Typography>
        </Stack>
        <Chip label="배포 설정 확인됨" color="primary" variant="outlined" sx={{ ml: { sm: 'auto' } }} />
      </Stack>

      <Grid container spacing={1.5}>
        <Grid size={{ xs: 12, md: 6 }}>
          <Box sx={{ height: '100%', p: 2.25, borderRadius: 2, border: '1px solid', borderColor: 'divider', bgcolor: alpha('#07101f', 0.4) }}>
            <Typography variant="body2" color="text.secondary" fontWeight={650}>서비스</Typography>
            <Typography variant="h3" sx={{ mt: 0.5 }}>{value.application.name}</Typography>
            <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>{value.application.code}</Typography>
          </Box>
        </Grid>
        <Grid size={{ xs: 12, md: 6 }}>
          <Box sx={{ height: '100%', p: 2.25, borderRadius: 2, border: '1px solid', borderColor: 'divider', bgcolor: alpha('#07101f', 0.4) }}>
            <Typography variant="body2" color="text.secondary" fontWeight={650}>배포 대상</Typography>
            <Typography variant="h3" sx={{ mt: 0.5 }}>{value.environment.name}</Typography>
            <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>{value.environment.kind || value.environment.code}</Typography>
          </Box>
        </Grid>
      </Grid>

      <Stack direction="row" alignItems="center" spacing={{ xs: 1.25, sm: 3 }} sx={{ p: { xs: 2, sm: 2.75 }, borderRadius: 2.5, border: '1px solid', borderColor: 'primary.dark', bgcolor: alpha('#68a9ff', 0.08) }}>
        <ComparisonValue label="현재 버전" value={value.currentVersion ? `v${value.currentVersion}` : undefined} />
        <ArrowForwardRoundedIcon color="primary" sx={{ flexShrink: 0, fontSize: { xs: 28, sm: 36 } }} />
        <ComparisonValue label="신규 버전" value={`v${value.version}`} emphasized />
      </Stack>

      {!ready && (
        <Alert severity="warning">
          {!value.readiness.profileReady && '관리자가 이 서비스의 배포 정책 설정을 완료해야 합니다. '}
          {!value.readiness.runnerAvailable && '현재 사용할 수 있는 배포 실행 서버가 없습니다. '}
          설정을 확인한 후 다시 시도하세요.
        </Alert>
      )}
      {ready && (
        <Alert severity={approvalRequired ? 'info' : 'success'} icon={<InfoOutlinedIcon />}>
          {approvalRequired
            ? `요청과 동시에 승인 절차가 시작됩니다.${value.preset.autoDeployAfterApproval ? ' 승인되면 별도 조작 없이 자동으로 배포합니다.' : ''}`
            : '요청과 동시에 안전 검증과 배포를 자동으로 시작합니다.'}
        </Alert>
      )}
    </Stack>
  );
}

export function NewReleasePage() {
  const navigate = useNavigate();
  const { hasPermission } = useAuth();
  const requestSequence = useRef(0);
  const fileInput = useRef<HTMLInputElement>(null);
  const [file, setFile] = useState<File>();
  const [preflight, setPreflight] = useState<QuickReleasePreflight>();
  const [analyzing, setAnalyzing] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [notes, setNotes] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string>();
  const [selectionError, setSelectionError] = useState<string>();
  const parsed = useMemo(() => file ? parseReleasePackageName(file.name) : undefined, [file]);
  const canSubmit = hasPermission('releases.submit');
  const canUseAdvanced = hasPermission('applications.read') && hasPermission('profiles.read');
  const ready = Boolean(!selectionError && canSubmit && preflight?.readiness.profileReady && preflight.readiness.runnerAvailable);

  useEffect(() => {
    const sequence = ++requestSequence.current;
    setPreflight(undefined);
    setError(undefined);
    if (!file || !parsed) {
      setAnalyzing(false);
      return;
    }
    setAnalyzing(true);
    void api.quickReleasePreflight(file.name)
      .then((result) => {
        if (requestSequence.current === sequence) setPreflight(result);
      })
      .catch((reason) => {
        if (requestSequence.current === sequence) setError(preflightErrorMessage(reason));
      })
      .finally(() => {
        if (requestSequence.current === sequence) setAnalyzing(false);
      });
  }, [file, parsed]);

  const chooseFile = (next?: File, selectionIssue?: string) => {
    setFile(next);
    setSelectionError(selectionIssue);
    setDragging(false);
    if (!next && fileInput.current) fileInput.current.value = '';
  };

  const drop = (event: DragEvent<HTMLElement>) => {
    event.preventDefault();
    if (submitting) return;
    const files = Array.from(event.dataTransfer.files);
    chooseFile(files[0], files.length > 1 ? '패키지는 한 번에 하나만 올릴 수 있습니다.' : undefined);
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!file || !preflight || !ready) return;
    setSubmitting(true);
    setError(undefined);
    try {
      const release = await api.createQuickRelease({
        artifact: file,
        expectedPresetId: preflight.preset.id,
        expectedPresetUpdatedAt: preflight.preset.updatedAt,
        expectedCurrentVersion: preflight.currentVersion ?? '',
        notes: notes.trim() || undefined,
      });
      navigate(`/releases/${release.id}`, { replace: true });
    } catch (reason) {
      const stalePreflight = reason instanceof ApiError && reason.status === 409 && [
        'deployment_preset_unavailable',
        'quick_release_conflict',
        'deployment_profile_not_ready',
        'runner_unavailable',
        'deployment_head_changed',
        'release_target_drift',
        'release_target_inactive',
      ].includes(reason.code ?? '');
      if (stalePreflight) {
        setError('확인 이후 배포 설정 또는 현재 버전이 변경되었습니다. 최신 정보를 다시 확인했습니다. 내용을 확인한 뒤 다시 요청하세요.');
        try {
          setPreflight(await api.quickReleasePreflight(file.name));
        } catch (refreshReason) {
          setPreflight(undefined);
          setError(preflightErrorMessage(refreshReason));
        }
      } else if (reason instanceof ApiError && reason.status === 409) {
        if (reason.code !== 'job_conflict') setPreflight(undefined);
        setError(reason.code === 'release_conflict' || reason.code === 'release_version_exists' ? '같은 서비스와 배포 대상에 동일한 버전이 이미 등록되어 있습니다. 새 버전의 패키지를 선택하세요.' : quickReleaseErrorMessage(reason));
      } else {
        if (reason instanceof ApiError && reason.code === 'commit_result_unknown') setPreflight(undefined);
        setError(quickReleaseErrorMessage(reason));
      }
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <>
      <PageHeader title="새 버전 배포" description="패키지 파일 하나만 올리면 서비스와 버전을 인식하고 알맞은 배포 대상으로 연결합니다." crumbs={[{ label: '릴리즈', to: '/releases' }, { label: '새 버전 배포' }]} />
      <Card>
        <CardContent sx={{ p: { xs: 2.5, md: 4 } }}>
          <Box component="form" onSubmit={(event) => void submit(event)}>
            <Stack spacing={3}>
              <Box>
                <Typography variant="h3">배포 패키지</Typography>
                <Typography color="text.secondary" variant="body2" sx={{ mt: 0.5 }}>
                  <Box component="span" sx={{ fontFamily: 'ui-monospace, SFMono-Regular, Consolas, monospace' }}>service-v1.2.3.tar.gz</Box> 형식의 파일을 선택하세요.
                </Typography>
              </Box>

              <Box
                component="label"
                onDragEnter={(event) => { event.preventDefault(); if (!submitting) setDragging(true); }}
                onDragOver={(event) => { event.preventDefault(); if (!submitting) { event.dataTransfer.dropEffect = 'copy'; setDragging(true); } }}
                onDragLeave={(event) => { event.preventDefault(); if (!event.currentTarget.contains(event.relatedTarget as Node | null)) setDragging(false); }}
                onDrop={drop}
                sx={{
                  position: 'relative',
                  display: 'grid',
                  placeItems: 'center',
                  minHeight: 230,
                  px: 3,
                  py: 4,
                  textAlign: 'center',
                  cursor: submitting ? 'default' : 'pointer',
                  border: '2px dashed',
                  borderColor: dragging ? 'secondary.main' : file ? (parsed ? 'primary.main' : 'error.main') : 'divider',
                  bgcolor: dragging ? alpha('#42d6b0', 0.1) : file ? alpha('#68a9ff', 0.08) : alpha('#07101f', 0.35),
                  borderRadius: 2.5,
                  transition: 'border-color .15s, background-color .15s, transform .15s',
                  transform: dragging ? 'scale(1.005)' : 'none',
                  '&:hover': { borderColor: 'primary.main', bgcolor: alpha('#68a9ff', 0.08) },
                  '&:focus-within': { outline: '3px solid', outlineColor: alpha('#68a9ff', 0.35), outlineOffset: 3 },
                }}
              >
                <input
                  ref={fileInput}
                  aria-label="릴리즈 패키지 선택"
                  type="file"
                  accept=".tar.gz,application/gzip"
                  disabled={submitting}
                  style={{ position: 'absolute', width: 1, height: 1, padding: 0, margin: -1, overflow: 'hidden', clipPath: 'inset(50%)', whiteSpace: 'nowrap', border: 0 }}
                  onClick={(event) => { event.currentTarget.value = ''; }}
                  onChange={(event) => chooseFile(event.target.files?.[0])}
                />
                {file && (
                  <Tooltip title="선택 취소">
                    <IconButton
                      aria-label="선택한 패키지 제거"
                      disabled={submitting}
                      onClick={(event) => { event.preventDefault(); chooseFile(undefined); }}
                      sx={{ position: 'absolute', top: 12, right: 12 }}
                    >
                      <CloseRoundedIcon />
                    </IconButton>
                  </Tooltip>
                )}
                <Box>
                  {analyzing ? <CircularProgress size={48} sx={{ mb: 1 }} /> : <CloudUploadRoundedIcon color={dragging ? 'secondary' : 'primary'} sx={{ fontSize: 52, mb: 1 }} />}
                  <Typography fontWeight={800} sx={{ overflowWrap: 'anywhere' }}>
                    {file ? file.name : dragging ? '여기에 놓아주세요' : '클릭하거나 패키지를 끌어다 놓으세요'}
                  </Typography>
                  <Typography color="text.secondary" variant="body2" sx={{ mt: 0.5 }}>
                    {file ? `${formatBytes(file.size)}${analyzing ? ' · 서비스 확인 중…' : ''}` : '.tar.gz · 한 번에 한 파일'}
                  </Typography>
                </Box>
              </Box>

              {file && !parsed && (
                <Alert severity="error">
                  파일명을 확인하세요. 예: <Box component="span" sx={{ fontFamily: 'ui-monospace, monospace' }}>ai-portal-v2.4.1.tar.gz</Box> 또는 <Box component="span" sx={{ fontFamily: 'ui-monospace, monospace' }}>ai-portal-v2.4.1-rc.1.tar.gz</Box>
                </Alert>
              )}
              {selectionError && <Alert severity="warning">{selectionError}</Alert>}
              {error && <Alert severity="error">{error}</Alert>}
              {preflight && <PreflightSummary value={preflight} />}
              {preflight && !canSubmit && <Alert severity="warning">원클릭 배포 요청 권한이 없습니다. 권한을 요청하거나 운영자 고급 등록을 사용하세요.</Alert>}

              {preflight && (
                <TextField
                  label="배포 메모 (선택)"
                  placeholder="변경 내용이나 운영 참고 사항을 남길 수 있습니다."
                  multiline
                  minRows={2}
                  value={notes}
                  onChange={(event) => setNotes(event.target.value)}
                  inputProps={{ maxLength: 2000 }}
                  helperText={`${notes.length}/2,000`}
                />
              )}

              <Stack direction={{ xs: 'column-reverse', sm: 'row' }} justifyContent="flex-end" spacing={1.5}>
                {canUseAdvanced && <Button variant="text" onClick={() => navigate('/releases/new/advanced')} disabled={submitting}>운영자 고급 등록</Button>}
                <Button variant="outlined" onClick={() => navigate('/releases')} disabled={submitting}>취소</Button>
                <Button
                  type="submit"
                  variant="contained"
                  size="large"
                  disabled={submitting || analyzing || !file || !preflight || !ready}
                  startIcon={submitting ? <CircularProgress size={18} /> : <RocketLaunchRoundedIcon />}
                  sx={{ minWidth: { sm: 170 } }}
                >
                  {submitting ? '배포 요청 중…' : '배포 요청'}
                </Button>
              </Stack>
            </Stack>
          </Box>
        </CardContent>
      </Card>
    </>
  );
}
