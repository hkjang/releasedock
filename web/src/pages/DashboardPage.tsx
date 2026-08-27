import AddRoundedIcon from '@mui/icons-material/AddRounded';
import ApprovalRoundedIcon from '@mui/icons-material/ApprovalRounded';
import CheckCircleRoundedIcon from '@mui/icons-material/CheckCircleRounded';
import PendingActionsRoundedIcon from '@mui/icons-material/PendingActionsRounded';
import RocketLaunchRoundedIcon from '@mui/icons-material/RocketLaunchRounded';
import TrendingUpRoundedIcon from '@mui/icons-material/TrendingUpRounded';
import {
  Box,
  Button,
  Card,
  CardContent,
  Grid,
  LinearProgress,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material';
import { alpha } from '@mui/material/styles';
import { Link as RouterLink } from 'react-router-dom';
import { api } from '../api/client';
import { useAuth } from '../auth/AuthContext';
import { EmptyState, PageError, PageLoading } from '../components/Feedback';
import { PageHeader } from '../components/PageHeader';
import { StatusChip } from '../components/StatusChip';
import { useAsync } from '../hooks/useAsync';
import type { DashboardSummary, Release } from '../types/domain';
import { formatDate } from '../utils/format';

async function loadDashboard(): Promise<DashboardSummary> {
  try {
    return await api.dashboard();
  } catch {
    const result = await api.releases({ page: 1, pageSize: 10 });
    const releases = result.items;
    const successes = releases.filter((release) => release.status === 'SUCCESS').length;
    return {
      totalReleases: result.total,
      activeDeployments: releases.filter((release) => ['QUEUED', 'VALIDATING', 'PRE_CHECK', 'EXTRACTING', 'IMAGE_IMPORT', 'IMAGE_INSPECT', 'IMAGE_LOAD', 'IMAGE_TAG', 'IMAGE_PUSH', 'DEPLOYING', 'VERIFYING', 'ROLLBACK'].includes(release.status)).length,
      pendingApprovals: releases.filter((release) => release.status === 'PENDING_REVIEW').length,
      successRate: releases.length ? Math.round((successes / releases.length) * 100) : 0,
      recentReleases: releases,
    };
  }
}

function StatCard({ label, value, suffix, icon: Icon, color }: { label: string; value: number; suffix?: string; icon: typeof RocketLaunchRoundedIcon; color: string }) {
  return (
    <Card sx={{ height: '100%' }}>
      <CardContent sx={{ p: 2.75 }}>
        <Stack direction="row" alignItems="flex-start" justifyContent="space-between" gap={2}>
          <Box>
            <Typography color="text.secondary" variant="body2" fontWeight={650}>{label}</Typography>
            <Typography sx={{ mt: 0.8, fontSize: '2rem', lineHeight: 1.2, fontWeight: 800 }}>
              {value.toLocaleString()}<Typography component="span" color="text.secondary" sx={{ ml: 0.5, fontSize: '0.9375rem' }}>{suffix}</Typography>
            </Typography>
          </Box>
          <Box sx={{ width: 44, height: 44, borderRadius: 2.25, display: 'grid', placeItems: 'center', color, bgcolor: alpha(color, 0.13) }}>
            <Icon />
          </Box>
        </Stack>
      </CardContent>
    </Card>
  );
}

function RecentReleases({ releases, canCreate }: { releases: Release[]; canCreate: boolean }) {
  if (!releases.length) {
    return <EmptyState title="등록된 릴리즈가 없습니다" description="업그레이드 패키지를 올리면 검증부터 배포까지 단계별로 추적할 수 있습니다." action={canCreate ? <Button component={RouterLink} to="/releases/new" variant="contained">릴리즈 등록</Button> : undefined} />;
  }
  return (
    <TableContainer>
      <Table aria-label="최근 릴리즈">
        <TableHead>
          <TableRow>
            <TableCell>애플리케이션</TableCell>
            <TableCell>버전</TableCell>
            <TableCell>환경</TableCell>
            <TableCell>상태</TableCell>
            <TableCell>등록 시각</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {releases.slice(0, 8).map((release) => (
            <TableRow key={release.id} hover component={RouterLink} to={`/releases/${release.id}`} sx={{ textDecoration: 'none', cursor: 'pointer' }}>
              <TableCell><Typography fontWeight={700}>{release.applicationName || release.applicationId}</Typography></TableCell>
              <TableCell>{release.version}</TableCell>
              <TableCell>{release.environmentName || release.environmentId}</TableCell>
              <TableCell><StatusChip status={release.status} /></TableCell>
              <TableCell sx={{ whiteSpace: 'nowrap' }}>{formatDate(release.createdAt)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}

export function DashboardPage() {
  const { hasPermission } = useAuth();
  const canCreate = hasPermission('releases.create');
  const state = useAsync(loadDashboard, []);
  return (
    <>
      <PageHeader
        title="릴리즈 대시보드"
        description="검증, 승인, 배포와 상태 확인 흐름을 한눈에 확인합니다."
        action={canCreate ? <Button component={RouterLink} to="/releases/new" variant="contained" startIcon={<AddRoundedIcon />}>새 릴리즈</Button> : undefined}
      />
      {state.loading && <PageLoading />}
      {state.error && !state.loading && <PageError error={state.error} onRetry={() => void state.reload()} />}
      {state.data && !state.loading && (
        <Stack spacing={3}>
          <Grid container spacing={2.25}>
            <Grid size={{ xs: 12, sm: 6, xl: 3 }}><StatCard label="전체 릴리즈" value={state.data.totalReleases} suffix="건" icon={RocketLaunchRoundedIcon} color="#68a9ff" /></Grid>
            <Grid size={{ xs: 12, sm: 6, xl: 3 }}><StatCard label="진행 중 배포" value={state.data.activeDeployments} suffix="건" icon={PendingActionsRoundedIcon} color="#53c7f5" /></Grid>
            <Grid size={{ xs: 12, sm: 6, xl: 3 }}><StatCard label="승인 대기" value={state.data.pendingApprovals} suffix="건" icon={ApprovalRoundedIcon} color="#f5b942" /></Grid>
            <Grid size={{ xs: 12, sm: 6, xl: 3 }}><StatCard label="최근 성공률" value={state.data.successRate} suffix="%" icon={TrendingUpRoundedIcon} color="#4fd1a5" /></Grid>
          </Grid>

          <Grid container spacing={2.25}>
            <Grid size={{ xs: 12, xl: 8 }}>
              <Card>
                <Box sx={{ px: 2.75, pt: 2.5, pb: 1.5, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                  <Box>
                    <Typography variant="h3">최근 릴리즈</Typography>
                    <Typography variant="body2" color="text.secondary">가장 최근 등록된 작업입니다.</Typography>
                  </Box>
                  <Button component={RouterLink} to="/releases" size="small">전체 보기</Button>
                </Box>
                <RecentReleases releases={state.data.recentReleases} canCreate={canCreate} />
              </Card>
            </Grid>
            <Grid size={{ xs: 12, xl: 4 }}>
              <Card sx={{ height: '100%' }}>
                <CardContent sx={{ p: 3 }}>
                  <Stack direction="row" alignItems="center" spacing={1.2} sx={{ mb: 3 }}>
                    <CheckCircleRoundedIcon color="success" />
                    <Typography variant="h3">배포 건전성</Typography>
                  </Stack>
                  <Typography sx={{ fontSize: '2.6rem', fontWeight: 800 }}>{state.data.successRate}%</Typography>
                  <Typography color="text.secondary" variant="body2" sx={{ mb: 1.5 }}>최근 조회 범위의 성공 비율</Typography>
                  <LinearProgress variant="determinate" value={state.data.successRate} color={state.data.successRate >= 80 ? 'success' : 'warning'} sx={{ height: 9, borderRadius: 10 }} />
                  <Box sx={{ mt: 4, p: 2, bgcolor: alpha('#68a9ff', 0.08), borderRadius: 2, border: '1px solid', borderColor: 'divider' }}>
                    <Typography fontWeight={700}>운영 안전 체크</Typography>
                    <Typography color="text.secondary" variant="body2" sx={{ mt: 0.5 }}>
                      실패한 배포는 상세 화면에서 단계 로그와 이미지 digest를 확인하고 필요 시 이전 버전으로 롤백하세요.
                    </Typography>
                  </Box>
                </CardContent>
              </Card>
            </Grid>
          </Grid>
        </Stack>
      )}
    </>
  );
}
