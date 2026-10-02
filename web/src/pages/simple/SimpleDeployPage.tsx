import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Card,
  CardContent,
  Chip,
  CircularProgress,
  IconButton,
  LinearProgress,
  MenuItem,
  Stack,
  TextField,
  Typography,
} from '@mui/material';
import CloudUploadRoundedIcon from '@mui/icons-material/CloudUploadRounded';
import DeleteOutlineRoundedIcon from '@mui/icons-material/DeleteOutlineRounded';
import PlayArrowRoundedIcon from '@mui/icons-material/PlayArrowRounded';
import { api, ApiError, type SimpleRun, type SimpleTarget } from '../../api/client';
import { PageHeader } from '../../components/PageHeader';
import { formatBytes } from '../../utils/format';
import { streamDisconnected, streamEndedRun } from './SimpleRunDetailPage';

interface LogLine {
  id: number;
  stream: string;
  message: string;
}

// DEPLOY_LOG_DISPLAY_LIMIT bounds the lines the live log holds. One upload's
// packages share that buffer - it is emptied when a batch starts, not between
// packages - so the limit falls on the whole batch and what it pushes out is
// the output of the packages that went first.
export const DEPLOY_LOG_DISPLAY_LIMIT = 4999;

// DEPLOY_LOG_TRUNCATED_NOTICE opens the warning. The lines that went missing
// are the oldest ones, which is the opposite of what the run detail view drops,
// so its wording cannot be borrowed: a reader told the end was cut would read
// the surviving tail as the whole deployment.
export const DEPLOY_LOG_TRUNCATED_NOTICE = '표시 한도를 넘어 앞부분의 오래된 로그 줄이 화면에서 빠졌습니다.';

export interface DeployLogState {
  lines: LogLine[];
  truncated: boolean;
}

// appendDeployLogLine trims and records the loss in one step, so the flag can
// never disagree with the buffer it describes. Reaching the limit exactly is
// not truncation; only a line actually pushed out is.
export function appendDeployLogLine(state: DeployLogState, entry: LogLine, limit = DEPLOY_LOG_DISPLAY_LIMIT): DeployLogState {
  const lines = [...state.lines, entry];
  const dropped = lines.length > limit;
  return { lines: dropped ? lines.slice(lines.length - limit) : lines, truncated: state.truncated || dropped };
}

export const EMPTY_DEPLOY_LOGS: DeployLogState = { lines: [], truncated: false };

type ItemStatus = 'QUEUED' | 'UPLOADING' | 'RUNNING' | 'SUCCESS' | 'FAILED' | 'TIMEOUT' | 'SKIPPED' | 'UNKNOWN';

export interface QueueItem {
  key: string;
  file: File;
  status: ItemStatus;
  runId?: string;
  error?: string;
  exitCode?: number | null;
}

const TERMINAL = ['SUCCESS', 'FAILED', 'TIMEOUT'];

function statusColor(status: string): 'default' | 'info' | 'success' | 'error' | 'warning' {
  if (status === 'SUCCESS') return 'success';
  if (status === 'FAILED') return 'error';
  if (status === 'TIMEOUT' || status === 'UNKNOWN') return 'warning';
  if (status === 'RUNNING' || status === 'UPLOADING') return 'info';
  return 'default';
}

function statusLabel(status: ItemStatus): string {
  switch (status) {
    case 'QUEUED': return '대기';
    case 'UPLOADING': return '업로드 중';
    case 'RUNNING': return '실행 중';
    case 'SUCCESS': return '성공';
    case 'FAILED': return '실패';
    case 'TIMEOUT': return '시간 초과';
    case 'SKIPPED': return '건너뜀';
    case 'UNKNOWN': return '확인 불가';
  }
}

function acceptableName(name: string): boolean {
  const lower = name.toLowerCase();
  return lower.endsWith('.tar') || lower.endsWith('.tar.gz');
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

// A stage scoped to run once per upload is deferred to the package the client
// marked as the last of the batch, and that marker is decided when a package
// is uploaded. So an upload that never reaches its last package - the user
// stopped the remaining files, or the last upload was rejected - leaves the
// deferred stages with no run to carry them: the packages that did deploy
// report success while nothing was mirrored and the application was never
// rolled over. The two helpers below detect exactly that.

// stagesDeferred reports whether a finished run pushed a stage onto the last
// package of its upload. SKIPPED is the only status that means "not here".
export function stagesDeferred(run: Pick<SimpleRun, 'replicationStatus' | 'appDeployStatus'>): boolean {
  return run.replicationStatus === 'SKIPPED' || run.appDeployStatus === 'SKIPPED';
}

// stagesReached reports whether the run that was supposed to carry the
// deferred stages actually got to them. A run that never started is absent,
// and a run whose own command failed stops before the stages and leaves them
// NONE. A stage that ran and failed counts as reached: its error is already
// reported on that run, so there is nothing extra to warn about.
export function stagesReached(run?: Pick<SimpleRun, 'replicationStatus' | 'appDeployStatus'>): boolean {
  if (!run) return false;
  return [run.replicationStatus, run.appDeployStatus].some(
    (status) => status !== undefined && status !== 'NONE' && status !== 'SKIPPED',
  );
}

// A package that did not deploy still holds its File in the browser, so it can
// be sent again without picking it from disk. Being able to do that is what
// makes the warning above actionable: the stages deferred to a package that
// never deployed leave the upload half applied, and the fix is to deploy the
// packages that are left. Only QUEUED packages are picked up by the start
// button, though, so without this every one of them - stopped by the operator,
// rejected at upload, failed while deploying - was stuck in the list with no
// way to run it and no way to remove it either.
export function retryablePackage(status: ItemStatus): boolean {
  return status === 'FAILED' || status === 'TIMEOUT' || status === 'SKIPPED' || status === 'UNKNOWN';
}

// requeue puts those packages back in line where they were, dropping what the
// previous attempt reported so a row does not keep showing an error for a run
// that is about to be replaced. UNKNOWN is included on purpose: its run may
// still be going, and if it is, the one-in-flight-per-target index rejects the
// upload with a message that says so instead of deploying the package twice.
export function requeue(items: QueueItem[]): QueueItem[] {
  return items.map((item) =>
    retryablePackage(item.status) ? { key: item.key, file: item.file, status: 'QUEUED' } : item,
  );
}

const POLL_INTERVAL_MS = 1500;
// How many reads in a row may fail before the queue stops asking. The run
// itself always reaches a terminal state on its own - the server times it out
// and marks orphaned runs FAILED on boot - so the only thing that can go on
// forever is the reading, and the queue is strictly sequential: it reads the
// stop button only between packages, so one unreadable run locks the page
// until the operator reloads it and the packages behind it never upload.
export const POLL_FAILURE_BUDGET = 40;

// pollAbandonReason returns why the queue should stop asking about a run, or
// null while it should keep asking. A read rejected because the session ended
// or because the record is gone will never start succeeding, so it stops at
// once; anything else - a proxy hiccup, a server being restarted - is retried
// until the budget above is spent.
export function pollAbandonReason(status: number | undefined, consecutiveFailures: number): string | null {
  if (status === 401 || status === 403) return '로그인이 만료되어 배포 진행 상태를 더 이상 확인할 수 없습니다.';
  if (status === 404) return '실행 기록을 찾을 수 없어 배포 진행 상태를 확인할 수 없습니다.';
  if (consecutiveFailures >= POLL_FAILURE_BUDGET) {
    return `서버에 연결하지 못해 약 ${Math.round((POLL_FAILURE_BUDGET * POLL_INTERVAL_MS) / 1000)}초 동안 배포 진행 상태를 확인하지 못했습니다.`;
  }
  return null;
}

// Raised when the queue gave up reading a run. The package is neither a
// success nor a failure - the deployment may well still be running - so it
// must not be reported as either.
class RunStateUnknown extends Error {}

const UNKNOWN_ADVICE = '배포는 계속 진행 중일 수 있으니 실행 목록에서 결과를 확인하십시오.';

// Groups the runs one click produces. The server only ever echoes and groups
// on it, so a random token is enough; crypto.randomUUID is not available on
// pages served over plain HTTP in every browser, hence the fallback.
function newBatchId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID().replace(/-/g, '');
  }
  return `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 10)}`;
}

export function SimpleDeployPage() {
  const [targets, setTargets] = useState<SimpleTarget[]>([]);
  const [targetId, setTargetId] = useState('');
  const [queue, setQueue] = useState<QueueItem[]>([]);
  const [dragging, setDragging] = useState(false);
  const [loading, setLoading] = useState(true);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState('');
  const [stranded, setStranded] = useState(false);
  const [activeRunId, setActiveRunId] = useState('');
  const [logs, setLogs] = useState<DeployLogState>(EMPTY_DEPLOY_LOGS);
  const [streamLost, setStreamLost] = useState(false);
  // Bumped to ask for another stream on the run already being uploaded.
  const [streamAttempt, setStreamAttempt] = useState(0);
  const logEndRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const cancelledRef = useRef(false);
  // The id of the last line the stream delivered, which is where a reconnect
  // resumes. It is the server's log id, not the display id below.
  const logCursorRef = useRef(0);
  // Display ids are handed out from one counter rather than read off the clock,
  // because a reconnect restarts the per-stream numbering: two lines a
  // millisecond-plus-counter scheme gave the same id would collide as React
  // keys and the log would draw one of them twice.
  const logSeqRef = useRef(0);
  const nextLineId = () => {
    logSeqRef.current += 1;
    return logSeqRef.current;
  };

  useEffect(() => {
    let cancelled = false;
    api
      .simpleTargets()
      .then((response) => {
        if (cancelled) return;
        setTargets(response.items);
        // A single target needs no choice at all, and the server resolves it
        // on its own, so the selector stays hidden.
        if (response.items.length === 1) setTargetId(response.items[0].id);
      })
      .catch((cause: unknown) => {
        if (!cancelled) setError(cause instanceof ApiError ? cause.message : '배포 대상을 불러오지 못했습니다.');
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // With more than one target the server cannot guess, so a choice is required.
  const mustChooseTarget = targets.length > 1;
  const selected = useMemo(() => targets.find((target) => target.id === targetId), [targets, targetId]);
  const effectiveTarget = mustChooseTarget ? selected : targets[0];

  // The live log of the package currently uploading. It resumes from the cursor
  // rather than from the start of the run so that a reconnect neither replays
  // what is already on screen nor drops what the run wrote while nothing was
  // listening: the server reads the lines after the cursor out of storage.
  useEffect(() => {
    if (!activeRunId) return;
    const source = new EventSource(`${api.simpleRunLogStreamUrl(activeRunId)}?after=${logCursorRef.current}`, {
      withCredentials: true,
    });
    const receive = (rawEvent: Event) => {
      const event = rawEvent as MessageEvent<string>;
      try {
        const parsed = JSON.parse(event.data) as { id?: number; stream?: string; message?: string };
        if (typeof parsed.id === 'number') logCursorRef.current = parsed.id;
        const entry = { id: nextLineId(), stream: parsed.stream ?? 'stdout', message: parsed.message ?? '' };
        setLogs((current) => appendDeployLogLine(current, entry));
      } catch {
        const entry = { id: nextLineId(), stream: 'stdout', message: event.data };
        setLogs((current) => appendDeployLogLine(current, entry));
      }
    };
    source.addEventListener('log', receive);
    // Registered with addEventListener rather than assigned to source.onerror
    // and source.onopen so that anything standing in for EventSource - the
    // stream test uses an EventTarget - reaches them by dispatching an event,
    // the way the browser does.
    source.addEventListener('open', () => setStreamLost(false));
    // A stream the browser has given up on fires this once and then nothing
    // arrives. The upload carries on and the queue keeps polling, so the page
    // went on reading as progressing above a log that had silently stopped -
    // and this log is what tells the operator whether the deployment worked.
    // Retrying on a timer here would spend the server's three-per-user stream
    // budget against the very limit that most often closed the stream, so the
    // operator is told and given the one reconnect instead.
    source.addEventListener('error', () => {
      if (streamDisconnected(source.readyState)) setStreamLost(true);
    });
    // The server also ends a stream at its own thirty-minute ceiling, with the
    // run still deploying, and the queue is still waiting on that run. Another
    // stream is asked for only when the frame did not name a terminal status;
    // a run that did finish leaves this closed, because reopening would only
    // be answered with another end frame.
    source.addEventListener('end', (rawEvent: Event) => {
      source.close();
      if (!streamEndedRun((rawEvent as MessageEvent<string>).data)) {
        setStreamAttempt((attempt) => attempt + 1);
      }
    });
    return () => source.close();
  }, [activeRunId, streamAttempt]);

  // Keeps this to one stream: the effect closes the dead one and opens a single
  // replacement from the cursor the lost stream left behind.
  const reconnectStream = () => {
    setStreamLost(false);
    setStreamAttempt((attempt) => attempt + 1);
  };

  useEffect(() => {
    logEndRef.current?.scrollIntoView({ block: 'end' });
  }, [logs]);

  const addFiles = useCallback((files: FileList | File[] | null) => {
    if (!files) return;
    const list = Array.from(files);
    const rejected = list.filter((file) => !acceptableName(file.name));
    const accepted = list.filter((file) => acceptableName(file.name));
    if (rejected.length) {
      setError(`.tar 또는 .tar.gz 파일만 올릴 수 있습니다: ${rejected.map((file) => file.name).join(', ')}`);
    } else {
      setError('');
    }
    if (!accepted.length) return;
    setQueue((current) => {
      const existing = new Set(current.map((item) => `${item.file.name}:${item.file.size}`));
      const additions = accepted
        .filter((file) => !existing.has(`${file.name}:${file.size}`))
        .map((file, index) => ({
          key: `${file.name}:${file.size}:${Date.now()}:${index}`,
          file,
          status: 'QUEUED' as ItemStatus,
        }));
      return [...current, ...additions];
    });
  }, []);

  const removeItem = (key: string) => setQueue((current) => current.filter((item) => item.key !== key));

  const patchItem = (key: string, patch: Partial<QueueItem>) =>
    setQueue((current) => current.map((item) => (item.key === key ? { ...item, ...patch } : item)));

  // Runs are strictly sequential: the database permits only one in-flight run
  // per target, so a parallel upload of several files would be rejected.
  const waitForTerminal = async (runId: string): Promise<SimpleRun> => {
    let failures = 0;
    for (;;) {
      await sleep(POLL_INTERVAL_MS);
      try {
        const run = await api.simpleRun(runId);
        failures = 0;
        if (TERMINAL.includes(run.status)) return run;
      } catch (cause) {
        // A transient read failure should not abandon a run that is still
        // going, but a read that cannot recover must not loop for ever.
        failures += 1;
        const reason = pollAbandonReason(cause instanceof ApiError ? cause.status : undefined, failures);
        if (reason) throw new RunStateUnknown(reason);
      }
    }
  };

  const start = async () => {
    const pending = queue.filter((item) => item.status === 'QUEUED');
    if (!pending.length) return;
    if (mustChooseTarget && !targetId) {
      setError('배포 대상을 선택하십시오.');
      return;
    }
    cancelledRef.current = false;
    setRunning(true);
    setError('');
    setStranded(false);
    // A new upload starts from an empty buffer, so nothing is missing from it
    // yet. A reconnect keeps both the lines and the warning instead.
    setLogs(EMPTY_DEPLOY_LOGS);

    // Every file of one click shares a batch id, and the last one is marked.
    // The stages an administrator set to run once per upload — Harbor
    // replication, the app deployment command — fire on that marked run, so
    // they happen after every package has been uploaded and deployed.
    const batchId = newBatchId();
    // Whether any package pushed a stage onto the marked package, and what
    // became of that package. Compared once the queue is done.
    let deferredStages = false;
    let markedRun: SimpleRun | undefined;
    // Set when the queue stopped reading a run. What the upload ended up doing
    // is then unknown, so the stranded warning below - which claims the stages
    // did not run - must not be drawn on top of it.
    let unknown = false;

    for (const [index, item] of pending.entries()) {
      const marked = index === pending.length - 1;
      if (cancelledRef.current) {
        patchItem(item.key, { status: 'SKIPPED' });
        continue;
      }
      patchItem(item.key, { status: 'UPLOADING' });
      const separator = { id: nextLineId(), stream: 'system', message: `── ${item.file.name} ──` };
      setLogs((current) => appendDeployLogLine(current, separator));
      let runId = '';
      try {
        const created = await api.startSimpleRun(targetId, item.file, {
          id: batchId,
          last: marked,
        });
        runId = created.id;
        patchItem(item.key, { status: 'RUNNING', runId });
        // Each package is a run of its own, so its log starts from the top and
        // whatever the previous package left unresolved is no longer current.
        logCursorRef.current = 0;
        setStreamLost(false);
        setActiveRunId(runId);
      } catch (cause) {
        const message = cause instanceof ApiError ? cause.message : '배포를 시작하지 못했습니다.';
        patchItem(item.key, { status: 'FAILED', error: message });
        const failure = { id: nextLineId(), stream: 'stderr', message };
        setLogs((current) => appendDeployLogLine(current, failure));
        continue;
      }
      let outcome: SimpleRun;
      try {
        outcome = await waitForTerminal(runId);
      } catch (cause) {
        const message = `${cause instanceof RunStateUnknown ? cause.message : '배포 진행 상태를 확인하지 못했습니다.'} ${UNKNOWN_ADVICE}`;
        patchItem(item.key, { status: 'UNKNOWN', error: message });
        const abandoned = { id: nextLineId(), stream: 'stderr', message };
        setLogs((current) => appendDeployLogLine(current, abandoned));
        setError(message);
        unknown = true;
        // The target is still held by this run, so the packages left in the
        // queue would only be rejected. They stay queued for a later attempt.
        break;
      }
      patchItem(item.key, {
        status: outcome.status as ItemStatus,
        exitCode: outcome.exitCode,
        error: outcome.error,
      });
      if (stagesDeferred(outcome)) deferredStages = true;
      if (marked) markedRun = outcome;
    }
    setActiveRunId('');
    setRunning(false);
    setStranded(!unknown && deferredStages && !stagesReached(markedRun));
  };

  const queuedCount = queue.filter((item) => item.status === 'QUEUED').length;
  const retryCount = queue.filter((item) => retryablePackage(item.status)).length;
  const canStart = queuedCount > 0 && !running && Boolean(effectiveTarget?.ready ?? true) && (!mustChooseTarget || Boolean(targetId));

  if (loading) {
    return (
      <Stack alignItems="center" sx={{ py: 8 }}>
        <CircularProgress />
      </Stack>
    );
  }

  return (
    <Stack spacing={3}>
      <PageHeader title="배포" description="패키지를 끌어다 놓고 배포를 실행합니다. 여러 개를 한 번에 올릴 수 있습니다." />

      {!targets.length && (
        <Alert severity="info">아직 배포 대상이 없습니다. 관리자에게 심플 대상 등록을 요청하십시오.</Alert>
      )}

      {error && <Alert severity="error" onClose={() => setError('')}>{error}</Alert>}

      {stranded && (
        <Alert severity="warning" onClose={() => setStranded(false)}>
          업로드당 한 번만 실행하도록 설정된 단계(복제·앱 배포)가 마지막 패키지로 미뤄졌으나, 그 패키지가 배포되지
          않아 실행되지 않았습니다. 앞서 배포된 패키지의 이미지는 아직 복제되지 않았으므로 목록에서 <strong>다시
          시도</strong>를 눌러 남은 패키지를 다시 배포하십시오.
        </Alert>
      )}

      {Boolean(targets.length) && (
        <Card>
          <CardContent>
            <Stack spacing={3}>
              {mustChooseTarget ? (
                <TextField
                  select
                  label="배포 대상"
                  value={targetId}
                  onChange={(event) => setTargetId(event.target.value)}
                  disabled={running}
                  helperText={selected ? selected.description || selected.uploadDir : '배포 대상이 여러 개이므로 하나를 선택하십시오.'}
                  fullWidth
                >
                  {targets.map((target) => (
                    <MenuItem key={target.id} value={target.id} disabled={!target.ready}>
                      {target.name}
                      {!target.ready && ' (실행 명령 미설정)'}
                    </MenuItem>
                  ))}
                </TextField>
              ) : (
                <Typography variant="body2" color="text.secondary">
                  배포 대상: <strong>{targets[0].name}</strong> · {targets[0].uploadDir}
                </Typography>
              )}

              {effectiveTarget && !effectiveTarget.ready && (
                <Alert severity="warning">{effectiveTarget.notReadyReason || '이 대상은 아직 실행할 수 없습니다.'}</Alert>
              )}

              <Box
                onDragOver={(event) => {
                  event.preventDefault();
                  setDragging(true);
                }}
                onDragLeave={() => setDragging(false)}
                onDrop={(event) => {
                  event.preventDefault();
                  setDragging(false);
                  if (!running) addFiles(event.dataTransfer.files);
                }}
                onClick={() => !running && inputRef.current?.click()}
                sx={{
                  border: '2px dashed',
                  borderColor: dragging ? 'primary.main' : 'divider',
                  borderRadius: 2,
                  p: 5,
                  textAlign: 'center',
                  cursor: running ? 'default' : 'pointer',
                  bgcolor: dragging ? 'action.hover' : 'transparent',
                }}
              >
                <input
                  ref={inputRef}
                  type="file"
                  accept=".tar,.tar.gz,.gz"
                  multiple
                  hidden
                  onChange={(event) => {
                    addFiles(event.target.files);
                    event.target.value = '';
                  }}
                />
                <CloudUploadRoundedIcon sx={{ fontSize: 44, color: 'text.secondary' }} />
                <Typography sx={{ mt: 1 }}>패키지 파일을 끌어 놓거나 눌러서 선택합니다. 여러 개를 함께 놓아도 됩니다.</Typography>
                <Typography variant="body2" color="text.secondary">
                  .tar.gz 도커 이미지 압축 파일
                  {effectiveTarget ? ` · 파일당 최대 ${formatBytes(effectiveTarget.maxUploadBytes)}` : ''}
                </Typography>
              </Box>

              {Boolean(queue.length) && (
                <Stack spacing={1}>
                  {queue.map((item) => (
                    <Stack
                      key={item.key}
                      direction="row"
                      alignItems="center"
                      spacing={1.5}
                      sx={{ px: 1.5, py: 1, borderRadius: 1, bgcolor: 'background.default' }}
                    >
                      <Chip size="small" label={statusLabel(item.status)} color={statusColor(item.status)} sx={{ minWidth: 84 }} />
                      <Box sx={{ flex: 1, minWidth: 0 }}>
                        <Typography noWrap>{item.file.name}</Typography>
                        <Typography variant="caption" color={item.error ? 'error.light' : 'text.secondary'}>
                          {item.error ? item.error : formatBytes(item.file.size)}
                          {item.exitCode !== undefined && item.exitCode !== null && ` · exit ${item.exitCode}`}
                        </Typography>
                      </Box>
                      {!running && (
                        <IconButton size="small" aria-label={`${item.file.name} 제거`} onClick={() => removeItem(item.key)}>
                          <DeleteOutlineRoundedIcon fontSize="small" />
                        </IconButton>
                      )}
                    </Stack>
                  ))}
                </Stack>
              )}

              {running && <LinearProgress />}

              <Stack direction="row" spacing={2} alignItems="center">
                <Button
                  variant="contained"
                  size="large"
                  startIcon={<PlayArrowRoundedIcon />}
                  disabled={!canStart}
                  onClick={() => void start()}
                >
                  {queuedCount > 1 ? `${queuedCount}개 배포 실행` : '배포 실행'}
                </Button>
                {running && (
                  <Button color="inherit" onClick={() => { cancelledRef.current = true; }}>
                    남은 파일 중단
                  </Button>
                )}
                {!running && retryCount > 0 && (
                  <Button variant="outlined" onClick={() => setQueue((current) => requeue(current))}>
                    {retryCount > 1 ? `${retryCount}개 다시 시도` : '다시 시도'}
                  </Button>
                )}
                {!running && Boolean(queue.length) && (
                  <Button color="inherit" onClick={() => setQueue([])}>목록 비우기</Button>
                )}
              </Stack>
            </Stack>
          </CardContent>
        </Card>
      )}

      {Boolean(logs.lines.length) && (
        <Card>
          <CardContent>
            <Typography variant="subtitle1" sx={{ mb: 1.5 }}>실행 로그</Typography>
            {/* One upload's packages share this buffer, so the lines it pushed
                out are the output of the packages that went first - and this
                screen offers no way to read them back. */}
            {logs.truncated && (
              <Alert severity="warning" sx={{ mb: 1.5 }}>
                {DEPLOY_LOG_TRUNCATED_NOTICE} 현재 최근 {logs.lines.length.toLocaleString()}줄만 표시합니다. 각 실행의
                전체 로그는 &lsquo;실행 기록&rsquo;에서 해당 실행을 열어 &lsquo;로그 내려받기&rsquo;로 받을 수 있습니다.
              </Alert>
            )}
            {/* Only while a run is actually being streamed: once the queue has
                moved past it there is nothing left to reconnect to, and the
                notice would sit over a finished log with a button that does
                nothing. */}
            {streamLost && Boolean(activeRunId) && (
              <Alert
                severity="warning"
                sx={{ mb: 1.5 }}
                action={<Button color="inherit" size="small" onClick={reconnectStream}>다시 연결</Button>}
              >
                실시간 로그 연결이 끊겼습니다. 아래 로그는 끊긴 시점까지입니다. 배포 자체는 계속 진행됩니다.
              </Alert>
            )}
            <Box
              component="pre"
              sx={{
                m: 0,
                p: 2,
                maxHeight: 420,
                overflow: 'auto',
                borderRadius: 1,
                bgcolor: 'background.default',
                fontSize: 13,
                lineHeight: 1.6,
                whiteSpace: 'pre-wrap',
                wordBreak: 'break-all',
              }}
            >
              {logs.lines.map((line) => (
                <Box
                  key={line.id}
                  component="span"
                  sx={{ display: 'block', color: line.stream === 'stderr' ? 'error.light' : line.stream === 'system' ? 'text.secondary' : 'inherit' }}
                >
                  {line.message}
                </Box>
              ))}
              <div ref={logEndRef} />
            </Box>
          </CardContent>
        </Card>
      )}
    </Stack>
  );
}
