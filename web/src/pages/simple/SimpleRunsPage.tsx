import {
  Card,
  Chip,
  Pagination,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
} from '@mui/material';
import { useSearchParams } from 'react-router-dom';
import { api } from '../../api/client';
import { EmptyState, PageError, PageLoading } from '../../components/Feedback';
import { PageHeader } from '../../components/PageHeader';
import { useAsync } from '../../hooks/useAsync';
import { formatBytes, formatDate } from '../../utils/format';

function statusColor(status: string): 'default' | 'info' | 'success' | 'error' | 'warning' {
  if (status === 'SUCCESS') return 'success';
  if (status === 'FAILED') return 'error';
  if (status === 'TIMEOUT') return 'warning';
  if (status === 'RUNNING' || status === 'PENDING') return 'info';
  return 'default';
}

export function SimpleRunsPage() {
  const [params, setParams] = useSearchParams();
  const requestedPage = Number(params.get('page') || 1);
  const page = Number.isInteger(requestedPage) && requestedPage > 0 ? requestedPage : 1;
  const state = useAsync(() => api.simpleRuns({ page, pageSize: 20 }), [page]);

  const setPage = (value: number) => {
    const next = new URLSearchParams(params);
    next.set('page', String(value));
    setParams(next, { replace: true });
  };

  return (
    <>
      <PageHeader title="실행 기록" description="업로드한 패키지와 실행 결과를 확인합니다." />
      {state.loading && !state.data && <PageLoading />}
      {state.error && <PageError error={state.error} onRetry={() => void state.reload()} />}
      {state.data && (
        <Card>
          {state.data.items.length ? (
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
                      <TableRow key={run.id} hover>
                        <TableCell><Chip size="small" label={run.status} color={statusColor(run.status)} /></TableCell>
                        <TableCell>{run.targetName}</TableCell>
                        <TableCell>{run.filename}</TableCell>
                        <TableCell>{formatBytes(run.sizeBytes)}</TableCell>
                        <TableCell>{run.exitCode ?? '—'}</TableCell>
                        <TableCell>{run.actorName || '—'}</TableCell>
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
                  onChange={(_event, value) => setPage(value)}
                />
              </Stack>
            </>
          ) : (
            <EmptyState title="실행 기록이 없습니다" description="배포 화면에서 패키지를 올리면 여기에 기록이 남습니다." />
          )}
        </Card>
      )}
    </>
  );
}
