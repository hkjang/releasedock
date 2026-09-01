import {
  Card,
  Chip,
  MenuItem,
  Pagination,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  TextField,
  Tooltip,
} from '@mui/material';
import { Link as RouterLink, useSearchParams } from 'react-router-dom';
import { api } from '../../api/client';
import { useAuth } from '../../auth/AuthContext';
import { EmptyState, PageError, PageLoading } from '../../components/Feedback';
import { PageHeader } from '../../components/PageHeader';
import { useAsync } from '../../hooks/useAsync';
import { formatBytes, formatDate } from '../../utils/format';

const statuses = [
  ['', '모든 상태'],
  ['RUNNING', '실행 중'],
  ['SUCCESS', '성공'],
  ['FAILED', '실패'],
  ['TIMEOUT', '시간 초과'],
] as const;

function statusColor(status: string): 'default' | 'info' | 'success' | 'error' | 'warning' {
  if (status === 'SUCCESS') return 'success';
  if (status === 'FAILED') return 'error';
  if (status === 'TIMEOUT') return 'warning';
  if (status === 'RUNNING' || status === 'PENDING') return 'info';
  return 'default';
}

export function SimpleRunsPage() {
  const { hasPermission } = useAuth();
  // Only a simple-mode administrator is served other people's runs, so the
  // scope control is hidden for everybody else.
  const canSeeEveryone = hasPermission('admin.simple.read');
  const [params, setParams] = useSearchParams();
  const requestedPage = Number(params.get('page') || 1);
  const page = Number.isInteger(requestedPage) && requestedPage > 0 ? requestedPage : 1;
  const status = params.get('status') || '';
  const mine = params.get('mine') === 'true';
  const state = useAsync(
    () => api.simpleRuns({ page, pageSize: 20, status }, mine),
    [page, status, mine],
  );

  const updateParam = (key: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    if (key !== 'page') next.delete('page');
    setParams(next, { replace: true });
  };

  return (
    <>
      <PageHeader
        title="실행 기록"
        description={canSeeEveryone
          ? '업로드한 패키지와 실행 결과, 명령 로그를 확인합니다. 행을 누르면 전체 로그를 볼 수 있습니다.'
          : '내가 실행한 배포의 결과와 로그를 확인합니다. 행을 누르면 전체 로그를 볼 수 있습니다.'}
      />
      <Card>
        <Stack direction={{ xs: 'column', md: 'row' }} gap={1.5} sx={{ p: 2 }}>
          <TextField
            select
            label="상태"
            value={status}
            onChange={(event) => updateParam('status', event.target.value)}
            sx={{ minWidth: 200 }}
          >
            {statuses.map(([value, label]) => <MenuItem key={value} value={value}>{label}</MenuItem>)}
          </TextField>
          {canSeeEveryone && (
            <TextField
              select
              label="범위"
              value={mine ? 'mine' : 'all'}
              onChange={(event) => updateParam('mine', event.target.value === 'mine' ? 'true' : '')}
              sx={{ minWidth: 200 }}
              helperText="관리자는 모든 사용자의 실행을 볼 수 있습니다."
            >
              <MenuItem value="all">전체 사용자</MenuItem>
              <MenuItem value="mine">내 실행만</MenuItem>
            </TextField>
          )}
        </Stack>

        {state.loading && !state.data && <PageLoading />}
        {state.error && <PageError error={state.error} onRetry={() => void state.reload()} />}
        {state.data && (
          state.data.items.length ? (
            <>
              <TableContainer sx={{ overflowX: 'auto' }}>
                <Table>
                  <TableHead>
                    <TableRow>
                      <TableCell>상태</TableCell>
                      <TableCell>대상</TableCell>
                      <TableCell>파일</TableCell>
                      <TableCell>크기</TableCell>
                      <TableCell>종료 코드</TableCell>
                      <TableCell>실행자</TableCell>
                      <TableCell>시작</TableCell>
                      <TableCell>종료</TableCell>
                    </TableRow>
                  </TableHead>
                  <TableBody>
                    {state.data.items.map((run) => (
                      <TableRow
                        key={run.id}
                        hover
                        component={RouterLink}
                        to={`/simple/runs/${encodeURIComponent(run.id)}`}
                        sx={{ textDecoration: 'none', cursor: 'pointer' }}
                      >
                        <TableCell><Chip size="small" label={run.status} color={statusColor(run.status)} /></TableCell>
                        <TableCell>{run.targetName}</TableCell>
                        <TableCell>{run.filename}</TableCell>
                        <TableCell>{formatBytes(run.sizeBytes)}</TableCell>
                        <TableCell>{run.exitCode ?? '—'}</TableCell>
                        <TableCell>
                          <Tooltip title={run.commandPath || ''}>
                            <span>{run.actorName || '—'}</span>
                          </Tooltip>
                        </TableCell>
                        <TableCell>{formatDate(run.startedAt ?? undefined)}</TableCell>
                        <TableCell>{formatDate(run.finishedAt ?? undefined)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </TableContainer>
              <Stack alignItems="center" sx={{ p: 2 }}>
                <Pagination
                  count={Math.max(1, Math.ceil((state.data.total ?? 0) / (state.data.pageSize || 20)))}
                  page={page}
                  onChange={(_event, value) => updateParam('page', String(value))}
                />
              </Stack>
            </>
          ) : (
            <EmptyState
              title="실행 기록이 없습니다"
              description={status ? '다른 상태로 조회해 보십시오.' : '배포 화면에서 패키지를 올리면 여기에 기록이 남습니다.'}
              filtered={Boolean(status)}
            />
          )
        )}
      </Card>
    </>
  );
}
