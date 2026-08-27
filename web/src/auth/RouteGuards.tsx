import { Box, CircularProgress, Typography } from '@mui/material';
import type { ReactNode } from 'react';
import { Navigate, Outlet, useLocation } from 'react-router-dom';
import { useAuth } from './AuthContext';

function FullScreenLoading() {
  return (
    <Box sx={{ minHeight: '100vh', display: 'grid', placeItems: 'center' }} role="status">
      <Box sx={{ textAlign: 'center' }}>
        <CircularProgress />
        <Typography color="text.secondary" sx={{ mt: 2 }}>사용자 정보를 확인하는 중입니다</Typography>
      </Box>
    </Box>
  );
}

export function RequireAuth() {
  const { user, loading } = useAuth();
  const location = useLocation();
  if (loading) return <FullScreenLoading />;
  if (!user) return <Navigate to="/login" state={{ from: location }} replace />;
  return <Outlet />;
}

export function RequireAdmin() {
  const { isAdmin } = useAuth();
  return isAdmin ? <Outlet /> : <Navigate to="/forbidden" replace />;
}

export function RequirePermission({ permission, children }: { permission: string; children: ReactNode }) {
  const { hasPermission } = useAuth();
  return hasPermission(permission) ? children : <Navigate to="/forbidden" replace />;
}
