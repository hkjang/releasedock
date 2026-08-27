import AddRoundedIcon from '@mui/icons-material/AddRounded';
import SearchRoundedIcon from '@mui/icons-material/SearchRounded';
import {
  Box,
  Button,
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
import { Link as RouterLink, useSearchParams } from 'react-router-dom';
import { api } from '../../api/client';
import { useAuth } from '../../auth/AuthContext';
import { EmptyState, PageError, PageLoading } from '../../components/Feedback';
import { PageHeader } from '../../components/PageHeader';
import { SimpleReleaseStatusChip } from '../../components/StatusChip';
import { useAsync } from '../../hooks/useAsync';
import { formatBytes, formatDate } from '../../utils/format';

const statuses = [
  ['', '모든 상태'],
  ['PENDING_REVIEW', '검토 대기'],
  ['DEPLOYING', '배포 중'],
  ['SUCCESS', '성공'],
  ['FAILED', '실패'],
  ['ROLLED_BACK', '롤백 완료'],
] as const;

export function ReleasesPage() {
  const { hasPermission } = useAuth();
  const canCreate = hasPermission('releases.create');
  const [params, setParams] = useSearchParams();
  const requestedPage = Number(params.get('page') || 1);
  const page = Number.isInteger(requestedPage) && requestedPage > 0 ? requestedPage : 1;
  const search = params.get('search') || '';
  const status = params.get('status') || '';
  const state = useAsync(() => api.releases({ page, pageSize: 20, search, status }), [page, search, status]);

  const updateParam = (key: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    if (key !== 'page') next.delete('page');
    setParams(next, { replace: true });
  };

  return (
    <>
      <PageHeader title="릴리즈" description="등록된 패키지와 배포 진행 상태, 결과 이력을 조회합니다." action={canCreate ? <Button component={RouterLink} to="/releases/new" variant="contained" startIcon={<AddRoundedIcon />}>새 버전 배포</Button> : undefined} />
      <Card>
        <Stack direction={{ xs: 'column', md: 'row' }} gap={1.5} sx={{ p: 2 }}>
          <TextField
            label="릴리즈 검색"
            placeholder="애플리케이션 또는 버전"
            value={search}
            onChange={(event) => updateParam('search', event.target.value)}
            sx={{ flex: 1, minWidth: 220 }}
            slotProps={{ input: { startAdornment: <InputAdornment position="start"><SearchRoundedIcon /></InputAdornment> } }}
          />
          <TextField select label="상태" value={status} onChange={(event) => updateParam('status', event.target.value)} sx={{ minWidth: 190 }}>
            {statuses.map(([value, label]) => <MenuItem key={value || 'all'} value={value}>{label}</MenuItem>)}
          </TextField>
        </Stack>

        {state.loading && <Box sx={{ p: 3 }}><PageLoading label="릴리즈 목록을 불러오는 중입니다" /></Box>}
        {state.error && !state.loading && <Box sx={{ p: 2 }}><PageError error={state.error} onRetry={() => void state.reload()} /></Box>}
        {state.data && !state.loading && (
          <>
            {state.data.items.length === 0 ? (
              <EmptyState filtered={Boolean(search || status)} title={search || status ? '검색 조건에 맞는 릴리즈가 없습니다' : '등록된 릴리즈가 없습니다'} action={canCreate && !search && !status ? <Button component={RouterLink} to="/releases/new" variant="contained">첫 릴리즈 등록</Button> : undefined} />
            ) : (
              <TableContainer>
                <Table aria-label="릴리즈 목록">
                  <TableHead><TableRow><TableCell>애플리케이션 / 버전</TableCell><TableCell>환경</TableCell><TableCell>아티팩트</TableCell><TableCell>상태</TableCell><TableCell>등록자</TableCell><TableCell>등록 시각</TableCell></TableRow></TableHead>
                  <TableBody>
                    {state.data.items.map((release) => (
                      <TableRow key={release.id} hover component={RouterLink} to={`/releases/${release.id}`} sx={{ cursor: 'pointer', textDecoration: 'none' }}>
                        <TableCell><Typography fontWeight={750}>{release.applicationName || release.applicationId}</Typography><Typography variant="body2" color="text.secondary">v{release.version}</Typography></TableCell>
                        <TableCell>{release.environmentName || release.environmentId}</TableCell>
                        <TableCell><Typography variant="body2" noWrap sx={{ maxWidth: 250 }}>{release.artifactName || '—'}</Typography><Typography variant="caption" color="text.secondary">{formatBytes(release.artifactSize)}</Typography></TableCell>
                        <TableCell><SimpleReleaseStatusChip status={release.status} /></TableCell>
                        <TableCell>{release.createdBy?.displayName || release.createdBy?.username || '—'}</TableCell>
                        <TableCell sx={{ whiteSpace: 'nowrap' }}>{formatDate(release.createdAt)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </TableContainer>
            )}
            {state.data.total > state.data.pageSize && (
              <Box sx={{ p: 2, display: 'flex', justifyContent: 'center' }}>
                <Pagination page={page} count={Math.ceil(state.data.total / state.data.pageSize)} onChange={(_, value) => updateParam('page', String(value))} color="primary" />
              </Box>
            )}
          </>
        )}
      </Card>
    </>
  );
}
