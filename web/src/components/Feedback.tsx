import CloudOffRoundedIcon from '@mui/icons-material/CloudOffRounded';
import InboxRoundedIcon from '@mui/icons-material/InboxRounded';
import RefreshRoundedIcon from '@mui/icons-material/RefreshRounded';
import SearchOffRoundedIcon from '@mui/icons-material/SearchOffRounded';
import { Alert, Box, Button, CircularProgress, Skeleton, Stack, Typography } from '@mui/material';
import type { ReactNode } from 'react';

export function PageLoading({ label = '정보를 불러오는 중입니다' }: { label?: string }) {
  return (
    <Stack spacing={2} role="status" aria-live="polite">
      <Typography color="text.secondary" sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
        <CircularProgress size={20} /> {label}
      </Typography>
      <Skeleton variant="rounded" height={88} />
      <Skeleton variant="rounded" height={220} />
    </Stack>
  );
}

export function PageError({ error, onRetry }: { error: Error; onRetry?: () => void }) {
  return (
    <Alert
      severity="error"
      icon={<CloudOffRoundedIcon />}
      action={
        onRetry ? (
          <Button color="inherit" startIcon={<RefreshRoundedIcon />} onClick={onRetry}>
            다시 시도
          </Button>
        ) : undefined
      }
      sx={{ alignItems: 'center' }}
    >
      <Typography fontWeight={700}>서버 요청을 완료하지 못했습니다.</Typography>
      <Typography variant="body2">{error.message}</Typography>
    </Alert>
  );
}

export function EmptyState({
  title = '표시할 항목이 없습니다',
  description = '조건을 변경하거나 새 항목을 등록해 주세요.',
  action,
  filtered = false,
}: {
  title?: string;
  description?: string;
  action?: ReactNode;
  filtered?: boolean;
}) {
  const Icon = filtered ? SearchOffRoundedIcon : InboxRoundedIcon;
  return (
    <Box sx={{ py: 8, px: 3, textAlign: 'center', color: 'text.secondary' }}>
      <Icon sx={{ fontSize: 48, mb: 1.5, opacity: 0.75 }} />
      <Typography variant="h3" color="text.primary" sx={{ mb: 0.75 }}>
        {title}
      </Typography>
      <Typography sx={{ mb: action ? 2.5 : 0 }}>{description}</Typography>
      {action}
    </Box>
  );
}
