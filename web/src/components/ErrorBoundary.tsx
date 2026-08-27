import ErrorOutlineRoundedIcon from '@mui/icons-material/ErrorOutlineRounded';
import RefreshRoundedIcon from '@mui/icons-material/RefreshRounded';
import { Box, Button, Card, CardContent, Typography } from '@mui/material';
import { Component, type ErrorInfo, type ReactNode } from 'react';

interface State {
  error?: Error;
}

export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  state: State = {};

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('ReleaseDock UI error', error, info.componentStack);
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <Box sx={{ minHeight: '100vh', display: 'grid', placeItems: 'center', p: 3 }}>
        <Card sx={{ width: 'min(100%, 560px)' }}>
          <CardContent sx={{ p: { xs: 3, sm: 5 }, textAlign: 'center' }}>
            <ErrorOutlineRoundedIcon color="error" sx={{ fontSize: 52, mb: 2 }} />
            <Typography variant="h1" component="h1" sx={{ mb: 1.5 }}>
              화면을 표시하지 못했습니다
            </Typography>
            <Typography color="text.secondary" sx={{ mb: 3 }}>
              예기치 않은 화면 오류가 발생했습니다. 페이지를 새로고침해 다시 시도해 주세요.
            </Typography>
            <Button startIcon={<RefreshRoundedIcon />} variant="contained" onClick={() => window.location.reload()}>
              페이지 새로고침
            </Button>
          </CardContent>
        </Card>
      </Box>
    );
  }
}
