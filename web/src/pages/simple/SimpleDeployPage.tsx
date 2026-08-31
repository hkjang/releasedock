import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Card,
  CardContent,
  Chip,
  CircularProgress,
  MenuItem,
  Stack,
  TextField,
  Typography,
} from '@mui/material';
import CloudUploadRoundedIcon from '@mui/icons-material/CloudUploadRounded';
import PlayArrowRoundedIcon from '@mui/icons-material/PlayArrowRounded';
import { api, ApiError, type SimpleRun, type SimpleTarget } from '../../api/client';
import { PageHeader } from '../../components/PageHeader';
import { formatBytes } from '../../utils/format';

interface LogLine {
  id: number;
  stream: string;
  message: string;
}

const TERMINAL = ['SUCCESS', 'FAILED', 'TIMEOUT'];

function statusColor(status: string): 'default' | 'info' | 'success' | 'error' | 'warning' {
  if (status === 'SUCCESS') return 'success';
  if (status === 'FAILED') return 'error';
  if (status === 'TIMEOUT') return 'warning';
  if (status === 'RUNNING') return 'info';
  return 'default';
}

export function SimpleDeployPage() {
  const [targets, setTargets] = useState<SimpleTarget[]>([]);
  const [targetId, setTargetId] = useState('');
  const [file, setFile] = useState<File>();
  const [dragging, setDragging] = useState(false);
  const [loading, setLoading] = useState(true);
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState('');
  const [run, setRun] = useState<SimpleRun>();
  const [status, setStatus] = useState('');
  const [logs, setLogs] = useState<LogLine[]>([]);
  const logEndRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    let cancelled = false;
    api
      .simpleTargets()
      .then((response) => {
        if (cancelled) return;
        setTargets(response.items);
        // With a single target there is nothing to choose, so preselect it.
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

  const selected = useMemo(() => targets.find((target) => target.id === targetId), [targets, targetId]);

  // Stream the live command output for the run just started.
  useEffect(() => {
    if (!run) return;
    let sequence = 0;
    const source = new EventSource(api.simpleRunLogStreamUrl(run.id), { withCredentials: true });
    const receive = (rawEvent: Event) => {
      const event = rawEvent as MessageEvent<string>;
      sequence += 1;
      try {
        const parsed = JSON.parse(event.data) as { stream?: string; message?: string };
        setLogs((current) => [...current.slice(-4998), { id: sequence, stream: parsed.stream ?? 'stdout', message: parsed.message ?? '' }]);
      } catch {
        setLogs((current) => [...current.slice(-4998), { id: sequence, stream: 'stdout', message: event.data }]);
      }
    };
    source.addEventListener('log', receive);
    source.addEventListener('end', (rawEvent: Event) => {
      const event = rawEvent as MessageEvent<string>;
      try {
        const parsed = JSON.parse(event.data) as { status?: string };
        if (parsed.status) setStatus(parsed.status);
      } catch {
        /* the poll below still settles the final status */
      }
      source.close();
      void api.simpleRun(run.id).then((latest) => setStatus(latest.status)).catch(() => undefined);
    });
    return () => source.close();
  }, [run]);

  useEffect(() => {
    logEndRef.current?.scrollIntoView({ block: 'end' });
  }, [logs]);

  const pickFile = useCallback((candidate?: File) => {
    if (!candidate) return;
    const name = candidate.name.toLowerCase();
    if (!name.endsWith('.tar') && !name.endsWith('.tar.gz')) {
      setError('.tar 또는 .tar.gz 파일만 올릴 수 있습니다.');
      return;
    }
    setError('');
    setFile(candidate);
  }, []);

  const start = async () => {
    if (!selected || !file) return;
    setStarting(true);
    setError('');
    setLogs([]);
    setStatus('PENDING');
    try {
      const created = await api.startSimpleRun(selected.id, file);
      setRun(created);
      setStatus(created.status);
    } catch (cause) {
      setStatus('');
      setError(cause instanceof ApiError ? cause.message : '배포를 시작하지 못했습니다.');
    } finally {
      setStarting(false);
    }
  };

  const busy = starting || (Boolean(status) && !TERMINAL.includes(status));
  const canStart = Boolean(selected?.ready && file) && !busy;

  if (loading) {
    return (
      <Stack alignItems="center" sx={{ py: 8 }}>
        <CircularProgress />
      </Stack>
    );
  }

  return (
    <Stack spacing={3}>
      <PageHeader title="배포" description="패키지를 올리고 배포를 실행합니다." />

      {!targets.length && (
        <Alert severity="info">
          아직 배포 대상이 없습니다. 관리자에게 심플 대상 등록을 요청하십시오.
        </Alert>
      )}

      {error && <Alert severity="error" onClose={() => setError('')}>{error}</Alert>}

      {Boolean(targets.length) && (
        <Card>
          <CardContent>
            <Stack spacing={3}>
              <TextField
                select
                label="배포 대상"
                value={targetId}
                onChange={(event) => setTargetId(event.target.value)}
                disabled={busy}
                helperText={selected ? selected.description || selected.uploadDir : '배포할 서비스를 선택하십시오.'}
                fullWidth
              >
                {targets.map((target) => (
                  <MenuItem key={target.id} value={target.id} disabled={!target.ready}>
                    {target.name}
                    {!target.ready && ' (실행 명령 미설정)'}
                  </MenuItem>
                ))}
              </TextField>

              {selected && !selected.ready && (
                <Alert severity="warning">{selected.notReadyReason || '이 대상은 아직 실행할 수 없습니다.'}</Alert>
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
                  if (!busy) pickFile(event.dataTransfer.files?.[0]);
                }}
                onClick={() => !busy && inputRef.current?.click()}
                sx={{
                  border: '2px dashed',
                  borderColor: dragging ? 'primary.main' : 'divider',
                  borderRadius: 2,
                  p: 5,
                  textAlign: 'center',
                  cursor: busy ? 'default' : 'pointer',
                  bgcolor: dragging ? 'action.hover' : 'transparent',
                }}
              >
                <input
                  ref={inputRef}
                  type="file"
                  accept=".tar,.tar.gz,.gz"
                  hidden
                  onChange={(event) => pickFile(event.target.files?.[0])}
                />
                <CloudUploadRoundedIcon sx={{ fontSize: 44, color: 'text.secondary' }} />
                <Typography sx={{ mt: 1 }}>
                  {file ? `${file.name} (${formatBytes(file.size)})` : '패키지 파일을 끌어 놓거나 눌러서 선택합니다.'}
                </Typography>
                <Typography variant="body2" color="text.secondary">
                  .tar.gz 도커 이미지 압축 파일
                  {selected ? ` · 최대 ${formatBytes(selected.maxUploadBytes)}` : ''}
                </Typography>
              </Box>

              <Stack direction="row" spacing={2} alignItems="center">
                <Button
                  variant="contained"
                  size="large"
                  startIcon={<PlayArrowRoundedIcon />}
                  disabled={!canStart}
                  onClick={() => void start()}
                >
                  배포 실행
                </Button>
                {Boolean(status) && <Chip label={status} color={statusColor(status)} />}
                {busy && <CircularProgress size={20} />}
              </Stack>
            </Stack>
          </CardContent>
        </Card>
      )}

      {Boolean(run) && (
        <Card>
          <CardContent>
            <Typography variant="subtitle1" sx={{ mb: 1 }}>실행 로그</Typography>
            <Typography variant="body2" color="text.secondary" sx={{ mb: 1.5 }}>
              {run?.filename} · {run?.storedPath}
            </Typography>
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
              {logs.length
                ? logs.map((line) => (
                    <Box
                      key={line.id}
                      component="span"
                      sx={{ display: 'block', color: line.stream === 'stderr' ? 'error.light' : line.stream === 'system' ? 'text.secondary' : 'inherit' }}
                    >
                      {line.message}
                    </Box>
                  ))
                : '출력을 기다리는 중입니다...'}
              <div ref={logEndRef} />
            </Box>
          </CardContent>
        </Card>
      )}
    </Stack>
  );
}
