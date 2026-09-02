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

interface LogLine {
  id: number;
  stream: string;
  message: string;
}

type ItemStatus = 'QUEUED' | 'UPLOADING' | 'RUNNING' | 'SUCCESS' | 'FAILED' | 'TIMEOUT' | 'SKIPPED';

interface QueueItem {
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
  if (status === 'TIMEOUT') return 'warning';
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
  const [logs, setLogs] = useState<LogLine[]>([]);
  const logEndRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const cancelledRef = useRef(false);

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

  useEffect(() => {
    if (!activeRunId) return;
    let sequence = 0;
    const source = new EventSource(api.simpleRunLogStreamUrl(activeRunId), { withCredentials: true });
    const receive = (rawEvent: Event) => {
      const event = rawEvent as MessageEvent<string>;
      sequence += 1;
      try {
        const parsed = JSON.parse(event.data) as { stream?: string; message?: string };
        setLogs((current) => [...current.slice(-4998), { id: Date.now() + sequence, stream: parsed.stream ?? 'stdout', message: parsed.message ?? '' }]);
      } catch {
        setLogs((current) => [...current.slice(-4998), { id: Date.now() + sequence, stream: 'stdout', message: event.data }]);
      }
    };
    source.addEventListener('log', receive);
    source.addEventListener('end', () => source.close());
    return () => source.close();
  }, [activeRunId]);

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
    for (;;) {
      await sleep(1500);
      try {
        const run = await api.simpleRun(runId);
        if (TERMINAL.includes(run.status)) return run;
      } catch {
        // A transient read failure should not abandon a run that is still going.
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
    setLogs([]);

    // Every file of one click shares a batch id, and the last one is marked.
    // The stages an administrator set to run once per upload — Harbor
    // replication, the app deployment command — fire on that marked run, so
    // they happen after every package has been uploaded and deployed.
    const batchId = newBatchId();
    // Whether any package pushed a stage onto the marked package, and what
    // became of that package. Compared once the queue is done.
    let deferredStages = false;
    let markedRun: SimpleRun | undefined;

    for (const [index, item] of pending.entries()) {
      const marked = index === pending.length - 1;
      if (cancelledRef.current) {
        patchItem(item.key, { status: 'SKIPPED' });
        continue;
      }
      patchItem(item.key, { status: 'UPLOADING' });
      setLogs((current) => [...current, { id: Date.now(), stream: 'system', message: `── ${item.file.name} ──` }]);
      let runId = '';
      try {
        const created = await api.startSimpleRun(targetId, item.file, {
          id: batchId,
          last: marked,
        });
        runId = created.id;
        patchItem(item.key, { status: 'RUNNING', runId });
        setActiveRunId(runId);
      } catch (cause) {
        const message = cause instanceof ApiError ? cause.message : '배포를 시작하지 못했습니다.';
        patchItem(item.key, { status: 'FAILED', error: message });
        setLogs((current) => [...current, { id: Date.now(), stream: 'stderr', message }]);
        continue;
      }
      const outcome = await waitForTerminal(runId);
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
    setStranded(deferredStages && !stagesReached(markedRun));
  };

  const queuedCount = queue.filter((item) => item.status === 'QUEUED').length;
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
          않아 실행되지 않았습니다. 앞서 배포된 패키지의 이미지는 아직 복제되지 않았으므로 남은 패키지를 다시
          배포하십시오.
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
                      {item.status === 'QUEUED' && !running && (
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
                {!running && Boolean(queue.length) && (
                  <Button color="inherit" onClick={() => setQueue([])}>목록 비우기</Button>
                )}
              </Stack>
            </Stack>
          </CardContent>
        </Card>
      )}

      {Boolean(logs.length) && (
        <Card>
          <CardContent>
            <Typography variant="subtitle1" sx={{ mb: 1.5 }}>실행 로그</Typography>
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
              {logs.map((line) => (
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
