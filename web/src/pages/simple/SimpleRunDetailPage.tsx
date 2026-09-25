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
import { api, ApiError, type SimpleLogLine, type SimpleRun } from '../../api/client';
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

// SKIPPED is not a problem: the stage was configured to run once per upload
// and belongs to another file's run, so it is spelled out rather than shown
// as a bare state name.
function stageLabel(status: string): string {
  return status === 'SKIPPED' ? '건너뜀 (마지막 파일에서 실행)' : status;
}

// MAX_LOG_PAGES bounds how many pages the view fetches at once, so opening a
// run that wrote a very long log does not tie the browser up indefinitely.
export const MAX_LOG_PAGES = 50;

// LOG_TRUNCATED_NOTICE ends a copied log that the view could not hold. The
// download serves the whole thing; a paste that silently stops part way through
// looks like a run that ended there.
export const LOG_TRUNCATED_NOTICE =
  '[releasedock] 화면에 담을 수 있는 줄 수를 넘어 여기까지만 표시했습니다. 전체 로그는 내려받기를 사용하십시오.';

// LOG_LINE_HEIGHT is shared by the log block and the rows inside it. A line the
// command left blank has nothing to give it height, so it is held open to this
// instead - blank lines are how a script separates its steps, and dropping them
// on screen would run those steps together for the reader.
export const LOG_LINE_HEIGHT = 1.6;

export type LogPage = { items: SimpleLogLine[]; lastId: number; hasMore: boolean };

// nextLogCursor leaves the cursor alone when a page came back empty. The last
// page of a log is full whenever the line count is a multiple of the page size,
// so the server reports hasMore and the next request returns nothing; taking
// that page's lastId would rewind to the start of the run and make the live
// stream resend every line the view already has.
export function nextLogCursor(current: number, page: LogPage): number {
  return page.items.length ? page.lastId : current;
}

// collectStoredLogs reads stored lines page by page so a run that finished long
// ago shows its full output. It reports truncated when the page budget ran out
// with more still waiting, because the reader would otherwise take a log that
// stops mid-run for the whole of it - and this log is what tells them whether a
// deployment did what it was supposed to.
export async function collectStoredLogs(
  fetchPage: (after: number) => Promise<LogPage>,
  maxPages = MAX_LOG_PAGES,
): Promise<{ lines: SimpleLogLine[]; cursor: number; truncated: boolean }> {
  const lines: SimpleLogLine[] = [];
  let cursor = 0;
  for (let page = 0; page < maxPages; page += 1) {
    const response = await fetchPage(cursor);
    lines.push(...response.items);
    cursor = nextLogCursor(cursor, response);
    if (!response.hasMore || !response.items.length) return { lines, cursor, truncated: false };
  }
  return { lines, cursor, truncated: true };
}

// appendStreamedLine adds a line the live stream delivered, unless the view
// already holds it. Comparing against the last id is enough: the server sends
// lines in id order only, and a reconnect resumes from Last-Event-ID, so the
// only repeat that can arrive is a line at or before the one held last. A
// repeat hands back the same array so nothing re-renders for it.
export function appendStreamedLine(current: SimpleLogLine[], line: SimpleLogLine): SimpleLogLine[] {
  if (current.length && line.id <= current[current.length - 1].id) return current;
  return [...current, line];
}

// The readyState the browser reports on an EventSource, spelled out rather
// than read off EventSource itself: the stream tests replace the global with a
// stand-in that has no such constants, and a comparison against undefined is
// false for every state.
const STREAM_CLOSED = 2;

// streamDisconnected decides whether an error frame is worth telling the reader
// about. The browser fires error both when it has given up (CLOSED) and when it
// is about to retry on its own (CONNECTING); only the first leaves the log
// stopped for good, and announcing the second would ask for a reconnect that
// spends one of the three streams the server allows a user.
export function streamDisconnected(readyState: number): boolean {
  return readyState === STREAM_CLOSED;
}

// streamEndedRun reads the frame the server sends when it stops streaming. It
// names the run's terminal status when the run is what ended; the frame sent at
// the server's own thirty-minute limit names a reason instead, and that run is
// still producing output somebody is watching for.
export function streamEndedRun(data: string): boolean {
  try {
    const parsed = JSON.parse(data) as { status?: string };
    return typeof parsed.status === 'string' && TERMINAL.includes(parsed.status);
  } catch {
    return false;
  }
}

// One package of the upload this run belongs to, the run being viewed
// included.
export interface UploadPackage {
  id: string;
  filename: string;
  status: string;
  batchLast: boolean;
  createdAt: string;
  current: boolean;
}

// uploadPackages puts the run being viewed back among the other packages of the
// same upload, in the order they were deployed. The stages an administrator set
// to run once per upload act on all of them together, so a run whose stages
// were held is only explained by the package that did not deploy — and until
// now finding it meant picking it out of the run history by eye. An upload of
// one package has nothing to relate, so it lists nothing.
export function uploadPackages(run: SimpleRun): UploadPackage[] {
  const siblings = run.batchSiblings ?? [];
  if (!siblings.length) return [];
  const packages: UploadPackage[] = siblings.map((item) => ({ ...item, current: false }));
  packages.push({
    id: run.id,
    filename: run.filename,
    status: run.status,
    batchLast: run.batchLast ?? false,
    createdAt: run.createdAt,
    current: true,
  });
  // The upload is sequential, so the deploy order is the creation order. The id
  // only settles the tie two runs stored in the same instant would otherwise
  // leave to the sort's own discretion.
  return packages.sort((left, right) =>
    left.createdAt === right.createdAt
      ? left.id.localeCompare(right.id)
      : left.createdAt < right.createdAt ? -1 : 1,
  );
}

// undeployedPackages counts the packages of the upload that finished without
// deploying. One that is still queued or running has not failed, so it is not
// counted: the number is there to say how much of the upload is missing, not
// how much of it is unfinished.
export function undeployedPackages(packages: UploadPackage[]): number {
  return packages.filter((item) => item.status === 'FAILED' || item.status === 'TIMEOUT').length;
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
  const [logTruncated, setLogTruncated] = useState(false);
  const [loadingLogs, setLoadingLogs] = useState(true);
  const [copied, setCopied] = useState(false);
  // Bumped once a closed stream has been accounted for, so a run the server let
  // go of before it finished gets a new one.
  const [streamAttempt, setStreamAttempt] = useState(0);
  const [streamLost, setStreamLost] = useState(false);
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
      const stored = await collectStoredLogs((after) => api.simpleRunLogs(id, after));
      lastIdRef.current = stored.cursor;
      setLogs(stored.lines);
      setLogTruncated(stored.truncated);
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
  // last stored line so nothing is duplicated or skipped. The effect depends on
  // the reload function rather than on the run state it belongs to: that state
  // is a fresh object on every render, and depending on it closed and reopened
  // the stream once per arriving line - three of those land inside the server's
  // one-second poll, which is its whole per-user stream budget, and the refused
  // connection ends the live log with nothing on screen to say so.
  const reloadRun = run.reload;
  useEffect(() => {
    if (!live || loadingLogs) return;
    const source = new EventSource(`${api.simpleRunLogStreamUrl(id)}?after=${lastIdRef.current}`, { withCredentials: true });
    const receive = (rawEvent: Event) => {
      const event = rawEvent as MessageEvent<string>;
      try {
        const parsed = JSON.parse(event.data) as SimpleLogLine;
        lastIdRef.current = parsed.id;
        setLogs((current) => appendStreamedLine(current, parsed));
      } catch {
        /* a malformed frame must not break the view */
      }
    };
    source.addEventListener('log', receive);
    // These are registered with addEventListener rather than assigned to
    // source.onerror/onopen so that anything standing in for EventSource -
    // the stream tests use an EventTarget - reaches them by dispatching an
    // event, the way the browser does.
    source.addEventListener('open', () => setStreamLost(false));
    // A stream the browser has given up on fires this once and then nothing.
    // The run stays RUNNING on screen with a log that stopped, so the reader
    // is told and given the one reconnect; retrying on a timer here would
    // spend the server's stream budget against a limit that is often what
    // closed the stream in the first place.
    source.addEventListener('error', () => {
      if (streamDisconnected(source.readyState)) setStreamLost(true);
    });
    // The server also ends a stream at its own thirty-minute limit, with the
    // run still going - a deployment that long then showed nothing further
    // until the reader thought to refresh. Another stream is asked for only
    // when the run was not what ended, and only after the state has been
    // re-read, so a run that did finish leaves this closed.
    source.addEventListener('end', (rawEvent: Event) => {
      source.close();
      const finished = streamEndedRun((rawEvent as MessageEvent<string>).data);
      void reloadRun().finally(() => {
        if (!finished) setStreamAttempt((attempt) => attempt + 1);
      });
    });
    return () => source.close();
  }, [live, loadingLogs, id, reloadRun, streamAttempt]);

  useEffect(() => {
    if (live) logEndRef.current?.scrollIntoView({ block: 'end' });
  }, [logs, live]);

  // Stored lines are re-read before another stream is asked for, because what
  // the run wrote while nothing was listening only exists in storage - resuming
  // from the frame the dead stream stopped at would leave that gap on screen
  // for good. Bumping the attempt in the same handler keeps it to one stream:
  // the effect sees the reload in progress, closes the dead one and waits, then
  // opens a single stream from the cursor the reload left behind.
  const reconnectStream = () => {
    setStreamLost(false);
    setStreamAttempt((attempt) => attempt + 1);
    void loadStoredLogs();
  };

  const copy = async () => {
    const text = logs.map((line) => line.message).join('\n');
    await navigator.clipboard.writeText(logTruncated ? `${text}\n${LOG_TRUNCATED_NOTICE}` : text);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 2000);
  };

  if (run.loading && !run.data) return <PageLoading label="실행 기록을 불러오는 중입니다" />;
  if (run.error) return <PageError error={run.error} onRetry={() => void run.reload()} />;
  if (!run.data) return null;

  const detail = run.data;
  const packages = uploadPackages(detail);
  const undeployed = undeployedPackages(packages);
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
                      <Chip size="small" label={stageLabel(detail.replicationStatus)} color={statusColor(detail.replicationStatus)} />
                      {Boolean(detail.replicationExecutionId) && (
                        <Typography variant="body2" color="text.secondary">execution {detail.replicationExecutionId}</Typography>
                      )}
                    </Stack>
                  }
                />
              )}
              {detail.appDeployStatus && detail.appDeployStatus !== 'NONE' && (
                <Detail
                  label="앱 배포"
                  value={
                    <Stack direction="row" spacing={1} alignItems="center">
                      <Chip size="small" label={stageLabel(detail.appDeployStatus)} color={statusColor(detail.appDeployStatus)} />
                      {Boolean(detail.appDeployError) && (
                        <Typography variant="body2" color="text.secondary">{detail.appDeployError}</Typography>
                      )}
                    </Stack>
                  }
                />
              )}
            </Box>
          </CardContent>
        </Card>

        {(packages.length > 0 || Boolean(detail.batchSiblingsError)) && (
          <Card>
            <CardContent>
              <Typography variant="subtitle1" sx={{ mb: 0.5 }}>
                같은 업로드의 패키지
                {packages.length > 0 && (
                  <Typography component="span" variant="body2" color="text.secondary"> · {packages.length}개</Typography>
                )}
              </Typography>
              <Typography variant="body2" color="text.secondary" sx={{ mb: 1.5 }}>
                {undeployed > 0
                  ? `${undeployed}개가 배포되지 않았습니다. 업로드당 한 번 실행하는 단계(복제·앱 배포)는 모든 패키지가 배포돼야 실행됩니다.`
                  : '한 번에 올린 패키지들입니다. 업로드당 한 번 실행하는 단계는 마지막 패키지에서 실행됩니다.'}
              </Typography>
              {Boolean(detail.batchSiblingsError) && (
                <Alert severity="warning" sx={{ mb: 1.5 }}>{detail.batchSiblingsError}</Alert>
              )}
              <Divider sx={{ mb: 1 }} />
              <Stack divider={<Divider flexItem />}>
                {packages.map((item) => (
                  <Stack
                    key={item.id}
                    direction="row"
                    alignItems="center"
                    spacing={1.5}
                    sx={{ py: 1, minWidth: 0 }}
                  >
                    <Chip size="small" label={item.status} color={statusColor(item.status)} sx={{ flexShrink: 0 }} />
                    <Box sx={{ flex: 1, minWidth: 0 }}>
                      {item.current ? (
                        <Typography sx={{ wordBreak: 'break-all', fontWeight: 600 }}>{item.filename}</Typography>
                      ) : (
                        <Typography
                          component={RouterLink}
                          to={`/simple/runs/${item.id}`}
                          sx={{ wordBreak: 'break-all', color: 'primary.main' }}
                        >
                          {item.filename}
                        </Typography>
                      )}
                      <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
                        {formatDate(item.createdAt)}
                        {item.current ? ' · 지금 보는 실행' : ''}
                        {item.batchLast ? ' · 마지막 패키지' : ''}
                      </Typography>
                    </Box>
                  </Stack>
                ))}
              </Stack>
            </CardContent>
          </Card>
        )}

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
            {streamLost && (
              <Alert
                severity="warning"
                sx={{ mb: 1.5 }}
                action={<Button color="inherit" size="small" onClick={reconnectStream}>다시 연결</Button>}
              >
                실시간 로그 연결이 끊겼습니다. 아래 로그는 끊긴 시점까지입니다.
              </Alert>
            )}
            {logTruncated && (
              <Alert severity="warning" sx={{ mb: 1.5 }}>
                출력이 길어 처음 {logs.length.toLocaleString()}줄만 표시했습니다. 전체 로그는 위의 &lsquo;로그 내려받기&rsquo;로 받으십시오.
              </Alert>
            )}
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
                  lineHeight: LOG_LINE_HEIGHT,
                  whiteSpace: 'pre-wrap',
                  wordBreak: 'break-all',
                }}
              >
                {logs.length ? (
                  logs.map((line) => (
                    <Box
                      key={line.id}
                      component="span"
                      sx={{
                        display: 'block',
                        minHeight: `${LOG_LINE_HEIGHT}em`,
                        color: line.stream === 'stderr' ? 'error.light' : line.stream === 'system' ? 'text.secondary' : 'inherit',
                      }}
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
