import SearchRoundedIcon from '@mui/icons-material/SearchRounded';
import {
  Box,
  Card,
  InputAdornment,
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
  Typography,
} from '@mui/material';
import { useSearchParams } from 'react-router-dom';
import { api } from '../../api/client';
import { EmptyState, PageError, PageLoading } from '../../components/Feedback';
import { PageHeader } from '../../components/PageHeader';
import { StatusChip } from '../../components/StatusChip';
import { useAsync } from '../../hooks/useAsync';
import { formatDate } from '../../utils/format';

export function AuditPage() {
  const [params, setParams] = useSearchParams();
  const requestedPage = Number(params.get('page') || 1);
  const page = Number.isInteger(requestedPage) && requestedPage > 0 ? requestedPage : 1;
  const search = params.get('search') || '';
  const status = params.get('status') || '';
  const state = useAsync(() => api.audits({ page, pageSize: 50, search, status }), [page, search, status]);
  const update = (key: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value); else next.delete(key);
    if (key !== 'page') next.delete('page');
    setParams(next, { replace: true });
  };
  return (
    <>
      <PageHeader title="감사 로그" description="로그인, 설정 변경, 릴리즈, 승인과 배포 작업을 변경 불가능한 이력으로 조회합니다." />
      <Card>
        <Stack direction={{ xs: 'column', md: 'row' }} spacing={1.5} sx={{ p: 2 }}>
          <TextField label="감사 로그 검색" placeholder="사용자, 작업, 리소스" value={search} onChange={(e) => update('search', e.target.value)} fullWidth slotProps={{ input: { startAdornment: <InputAdornment position="start"><SearchRoundedIcon /></InputAdornment> } }} />
          <TextField select label="결과" value={status} onChange={(e) => update('status', e.target.value)} sx={{ minWidth: 180 }}><MenuItem value="">전체</MenuItem><MenuItem value="SUCCESS">성공</MenuItem><MenuItem value="FAILURE">실패</MenuItem></TextField>
        </Stack>
        {state.loading && <Box sx={{ p: 3 }}><PageLoading /></Box>}
        {state.error && !state.loading && <Box sx={{ p: 2 }}><PageError error={state.error} onRetry={() => void state.reload()} /></Box>}
        {state.data && !state.loading && (state.data.items.length ? (
          <>
            <TableContainer><Table aria-label="감사 로그"><TableHead><TableRow><TableCell>시각</TableCell><TableCell>사용자</TableCell><TableCell>작업</TableCell><TableCell>대상</TableCell><TableCell>결과</TableCell><TableCell>접속 IP</TableCell><TableCell>세부 내용</TableCell></TableRow></TableHead><TableBody>
              {state.data.items.map((event) => <TableRow key={event.id} hover><TableCell sx={{ whiteSpace: 'nowrap' }}>{formatDate(event.createdAt)}</TableCell><TableCell><Typography fontWeight={700}>{event.actor}</Typography></TableCell><TableCell>{event.action}</TableCell><TableCell>{event.resource}</TableCell><TableCell><StatusChip status={event.result} /></TableCell><TableCell>{event.ipAddress || '—'}</TableCell><TableCell sx={{ maxWidth: 350 }}><Typography variant="body2" noWrap title={event.detail}>{event.detail || '—'}</Typography></TableCell></TableRow>)}
            </TableBody></Table></TableContainer>
            {state.data.total > state.data.pageSize && <Box sx={{ p: 2, display: 'flex', justifyContent: 'center' }}><Pagination page={page} count={Math.ceil(state.data.total / state.data.pageSize)} onChange={(_, value) => update('page', String(value))} /></Box>}
          </>
        ) : <EmptyState title="기록된 감사 이벤트가 없습니다" description="사용자와 배포 작업이 시작되면 이곳에 이력이 표시됩니다." />)}
      </Card>
    </>
  );
}
