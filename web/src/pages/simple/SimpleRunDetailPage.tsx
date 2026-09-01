import { useCallback, useEffect, useRef, useState } from 'react';
import { Link as RouterLink, useParams } from 'react-router-dom';
import {
  Alert,
  Box,
  Button,
  Card,
  CardContent,
  Chip,
  CircularProgress,
  Divider,
  IconButton,
  Stack,
  Tooltip,
  Typography,
} from '@mui/material';
import ContentCopyRoundedIcon from '@mui/icons-material/ContentCopyRounded';
import DownloadRoundedIcon from '@mui/icons-material/DownloadRounded';
import RefreshRoundedIcon from '@mui/icons-material/RefreshRounded';
import { api, ApiError, type SimpleLogLine } from '../../api/client';
import { PageError, PageLoading } from '../../components/Feedback';
import { PageHeader } from '../../components/PageHeader';
import { useAsync } from '../../hooks/useAsync';
import { formatBytes, formatDate, formatDuration } from '../../utils/format';

const TERMINAL = ['SUCCESS', 'FAILED', 'TIMEOUT'];

function statusColor(status: string): 'default' | 'info' | 'success' | 'error' | 'warning' {
  if (status === 'SUCCESS') return 'success';
  if (status === 'FAILED') return 'error';
  if (status === 'TIMEOUT') return 'warning';
  if (status === 'RUNNING' || status === 'PENDING') return 'info';
  return 'default';
}

function Detail({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <Box sx={{ minWidth: 0 }}>
      <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>{label}</Typography>
      <Typography sx={{ wordBreak: 'break-all' }}>{value}</Typography>
    </Box>
  );
}

export function SimpleRunDetailPage() {
  const { id = '' } = useParams();
  const run = useAsync(() => api.simpleRun(id), [id]);
  const [logs, setLogs] = useState<SimpleLogLine[]>([]);
  const [logError, setLogError] = useState('');
  const [loadingLogs, setLoadingLogs] = useState(true);
  const [copied, setCopied] = useState(false);
  const lastIdRef = useRef(0);
  const logEndRef = useRef<HTMLDivElement>(null);

  const status = run.data?.status ?? '';
  const live = Boolean(status) && !TERMINAL.includes(status);

  // Stored lines are fetched page by page rather than streamed, so a run that
  // finished long ago shows its full output.
  const loadStoredLogs = useCallback(async () => {
    setLoadingLogs(true);
    setLogError('');
    try {
      let after = 0;
      const collected: SimpleLogLine[] = [];
      for (let page = 0; page < 50; page += 1) {
        const response = await api.simpleRunLogs(id, after);
        collected.push(...response.items);
        after = response.lastId;
        if (!response.hasMore) break;
      }
      lastIdRef.current = after;
      setLogs(collected);
    } catch (cause) {
      setLogError(cause instanceof ApiError ? cause.message : '로그를 불러오지 못했습니다.');
    } finally {
      setLoadingLogs(false);
    }
  }, [id]);

  useEffect(() => {
    void loadStoredLogs();
  }, [loadStoredLogs]);

  // Only a run that is still going needs the stream, and it resumes from the
  // last stored line so nothing is duplicated or skipped.
  useEffect(() => {
    if (!live || loadingLogs) return;
    const source = new EventSource(`${api.simpleRunLogStreamUrl(id)}?after=${lastIdRef.current}`, { withCredentials: true });
    const receive = (rawEvent: Event) => {
      const event = rawEvent as MessageEvent<string>;
      try {
        const parsed = JSON.parse(event.data) as SimpleLogLine;
        lastIdRef.current = parsed.id;
        setLogs((current) => (current.some((line) => line.id === parsed.id) ? current : [...current, parsed]));
      } catch {
        /* a malformed frame must not break the view */
      }
    };
    source.addEventListener('log', receive);
    source.addEventListener('end', () => {
      source.close();
      void run.reload();
    });
    return () => source.close();
  }, [live, loadingLogs, id, run]);

  useEffect(() => {
    if (live) logEndRef.current?.scrollIntoView({ block: 'end' });
  }, [logs, live]);

  const copy = async () => {
    await navigator.clipboard.writeText(logs.map((line) => line.message).join('\n'));
    setCopied(true);
    window.setTimeout(() => setCopied(false), 2000);
  };

  if (run.loading && !run.data) return <PageLoading label="실행 기록을 불러오는 중입니다" />;
  if (run.error) return <PageError error={run.error} onRetry={() => void run.reload()} />;
  if (!run.data) return null;

  const detail = run.data;
  const duration =
    detail.startedAt && detail.finishedAt
      ? formatDuration(new Date(detail.finishedAt).getTime() - new Date(detail.startedAt).getTime())
      : '—';

  return (
    <>
      <PageHeader
        title="실행 상세"
        description={`${detail.targetName} · ${detail.filename}`}
        crumbs={[{ label: '실행 기록', to: '/simple/runs' }, { label: '실행 상세' }]}
        action={
          <Stack direction="row" spacing={1}>
            <Button startIcon={<RefreshRoundedIcon />} onClick={() => { void run.reload(); void loadStoredLogs(); }}>
              새로 고침
            </Button>
            <Button
              component="a"
              href={api.simpleRunLogDownloadUrl(detail.id)}
              startIcon={<DownloadRoundedIcon />}
              variant="outlined"
            >
              로그 내려받기
            </Button>
          </Stack>
        }
      />

      <Stack spacing={2.5}>
        <Card>
          <CardContent>
            <Stack direction="row" alignItems="center" spacing={1.5} sx={{ mb: 2 }}>
              <Chip label={detail.status} color={statusColor(detail.status)} />
              {live && <CircularProgress size={18} />}
              {detail.exitCode !== null && detail.exitCode !== undefined && (
                <Typography variant="body2" color="text.secondary">종료 코드 {detail.exitCode}</Typography>
              )}
            </Stack>
            {Boolean(detail.error) && <Alert severity="error" sx={{ mb: 2 }}>{detail.error}</Alert>}
            <Box sx={{ display: 'grid', gap: 2, gridTemplateColumns: { xs: '1fr', sm: '1fr 1fr', lg: '1fr 1fr 1fr' } }}>
              <Detail label="배포 대상" value={detail.targetName} />
              <Detail label="실행자" value={detail.actorName || '—'} />
              <Detail label="파일" value={detail.filename} />
              <Detail label="크기" value={formatBytes(detail.sizeBytes)} />
              <Detail label="SHA-256" value={<Typography component="span" sx={{ fontFamily: 'monospace', fontSize: 13, wordBreak: 'break-all' }}>{detail.sha256 || '—'}</Typography>} />
              <Detail label="저장 경로" value={detail.storedPath || '—'} />
              <Detail label="명령 방식" value={detail.commandSource === 'SHARED' ? '공통 명령' : '서비스별 명령'} />
              <Detail label="실행 명령" value={<Typography component="span" sx={{ fontFamily: 'monospace', fontSize: 13, wordBreak: 'break-all' }}>{detail.commandPath}</Typography>} />
              <Detail
                label="명령 인자"
                value={
                  detail.commandArgs?.length ? (
                    <Typography component="span" sx={{ fontFamily: 'monospace', fontSize: 13, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                      {detail.commandArgs.join('\n')}
                    </Typography>
                  ) : '—'
                }
              />
              <Detail label="제한 시간" value={detail.timeoutSeconds ? `${detail.timeoutSeconds}초` : '—'} />
              <Detail label="시작" value={formatDate(detail.startedAt ?? undefined)} />
              <Detail label="종료" value={formatDate(detail.finishedAt ?? undefined)} />
              <Detail label="소요 시간" value={duration} />
              {detail.replicationStatus && detail.replicationStatus !== 'NONE' && (
                <Detail
                  label="Harbor 복제"
                  value={
                    <Stack direction="row" spacing={1} alignItems="center">
                      <Chip size="small" label={detail.replicationStatus} color={statusColor(detail.replicationStatus)} />
                      {Boolean(detail.replicationExecutionId) && (
                        <Typography variant="body2" color="text.secondary">execution {detail.replicationExecutionId}</Typography>
                      )}
                    </Stack>
                  }
                />
              )}
            </Box>
          </CardContent>
        </Card>

        <Card>
          <CardContent>
            <Stack direction="row" alignItems="center" sx={{ mb: 1.5 }}>
              <Typography variant="subtitle1" sx={{ flex: 1 }}>
                실행 로그
                <Typography component="span" variant="body2" color="text.secondary"> · {logs.length}줄</Typography>
              </Typography>
              <Tooltip title={copied ? '복사했습니다' : '로그 복사'}>
                <span>
                  <IconButton size="small" aria-label="로그 복사" disabled={!logs.length} onClick={() => void copy()}>
                    <ContentCopyRoundedIcon fontSize="small" />
                  </IconButton>
                </span>
              </Tooltip>
            </Stack>
            <Divider sx={{ mb: 1.5 }} />
            {logError && <Alert severity="error" sx={{ mb: 1.5 }}>{logError}</Alert>}
            {loadingLogs ? (
              <Stack alignItems="center" sx={{ py: 4 }}><CircularProgress size={24} /></Stack>
            ) : (
              <Box
                component="pre"
                sx={{
                  m: 0,
                  p: 2,
                  maxHeight: 560,
                  overflow: 'auto',
                  borderRadius: 1,
                  bgcolor: 'background.default',
                  fontSize: 13,
                  lineHeight: 1.6,
                  whiteSpace: 'pre-wrap',
                  wordBreak: 'break-all',
                }}
              >
                {logs.length ? (
                  logs.map((line) => (
                    <Box
                      key={line.id}
                      component="span"
                      sx={{ display: 'block', color: line.stream === 'stderr' ? 'error.light' : line.stream === 'system' ? 'text.secondary' : 'inherit' }}
                    >
                      {line.message}
                    </Box>
                  ))
                ) : (
                  <Typography color="text.secondary">기록된 출력이 없습니다.</Typography>
                )}
                <div ref={logEndRef} />
              </Box>
            )}
          </CardContent>
        </Card>

        <Button component={RouterLink} to="/simple/runs" sx={{ alignSelf: 'flex-start' }}>
          실행 기록으로 돌아가기
        </Button>
      </Stack>
    </>
  );
}
