import DnsRoundedIcon from '@mui/icons-material/DnsRounded';
import LockRoundedIcon from '@mui/icons-material/LockRounded';
import LoginRoundedIcon from '@mui/icons-material/LoginRounded';
import {
  Alert,
  Box,
  Button,
  Card,
  CardContent,
  CircularProgress,
  Divider,
  Stack,
  TextField,
  Typography,
} from '@mui/material';
import { type FormEvent, useEffect, useState } from 'react';
import { Navigate, useLocation, useNavigate } from 'react-router-dom';
import { api, type AuthConfig } from '../api/client';
import { useVersion } from '../app/VersionContext';
import { useAuth } from '../auth/AuthContext';

export function LoginPage() {
  const { user, loading, login, attemptingSso } = useAuth();
  const version = useVersion();
  const navigate = useNavigate();
  const location = useLocation();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string>();
  const [authConfig, setAuthConfig] = useState<AuthConfig>({ local_enabled: true, oidc: { enabled: false } });

  useEffect(() => {
    let active = true;
    api.authConfig().then((config) => {
      if (active) setAuthConfig(config);
    }).catch(() => {
      // Keep local login available if the public configuration cannot be loaded.
    });
    return () => { active = false; };
  }, []);

  // The provider had no live session; say so once instead of leaving the
  // visitor wondering why nothing happened.
  const ssoOutcome = new URLSearchParams(location.search).get('sso');

  if (attemptingSso) {
    return (
      <Box sx={{ minHeight: '100vh', display: 'grid', placeItems: 'center' }} role="status">
        <Stack alignItems="center" spacing={2}>
          <CircularProgress />
          <Typography color="text.secondary">SSO 세션을 확인하는 중입니다</Typography>
        </Stack>
      </Box>
    );
  }
  if (!loading && user) return <Navigate to="/" replace />;

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setSubmitting(true);
    setError(undefined);
    try {
      await login(username.trim(), password);
      const state = location.state as { from?: { pathname?: string } } | null;
      navigate(state?.from?.pathname || '/', { replace: true });
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '로그인하지 못했습니다.');
    } finally {
      setSubmitting(false);
    }
  };

  const startOidc = () => {
    const state = location.state as { from?: { pathname?: string; search?: string } } | null;
    const candidate = `${state?.from?.pathname || '/'}${state?.from?.search || ''}`;
    const returnTo = candidate.startsWith('/') && !candidate.startsWith('//') ? candidate : '/';
    window.location.assign(`/api/v1/auth/oidc/login?return_to=${encodeURIComponent(returnTo)}`);
  };

  return (
    <Box sx={{ minHeight: '100vh', display: 'grid', gridTemplateColumns: { xs: '1fr', lg: 'minmax(360px, 1fr) minmax(520px, 0.85fr)' } }}>
      <Box
        sx={{
          display: { xs: 'none', lg: 'flex' },
          flexDirection: 'column',
          justifyContent: 'space-between',
          p: 7,
          background: 'radial-gradient(circle at 30% 24%, rgba(70, 143, 232, .34), transparent 32%), linear-gradient(145deg, #0e1c32, #080d18)',
          borderRight: '1px solid',
          borderColor: 'divider',
        }}
      >
        <Stack direction="row" alignItems="center" spacing={1.5}>
          <Box sx={{ display: 'grid', placeItems: 'center', width: 46, height: 46, borderRadius: 2.5, color: '#07121f', background: 'linear-gradient(145deg, #78b7ff, #45dbb4)' }}>
            <DnsRoundedIcon />
          </Box>
          <Typography variant="h2">ReleaseDock</Typography>
        </Stack>
        <Box sx={{ maxWidth: 700 }}>
          <Typography component="h1" sx={{ fontSize: 'clamp(2.5rem, 5vw, 4.7rem)', lineHeight: 1.04, fontWeight: 800, letterSpacing: '-.045em' }}>
            배포의 모든 단계를<br />안전하게 연결합니다.
          </Typography>
          <Typography sx={{ mt: 3, color: 'text.secondary', fontSize: '1.125rem', maxWidth: 570 }}>
            패키지 검증부터 Harbor Push, 승인, 배포와 상태 확인까지 한곳에서 추적하는 오프라인 릴리즈 오케스트레이터입니다.
          </Typography>
        </Box>
        <Typography color="text.secondary">Release Orchestrator · Deployment Gateway</Typography>
      </Box>

      <Box sx={{ display: 'grid', placeItems: 'center', px: 2, py: 5 }}>
        <Card sx={{ width: 'min(100%, 520px)' }}>
          <CardContent sx={{ p: { xs: 3, sm: 5 } }}>
            <Box sx={{ display: { lg: 'none' }, mb: 3 }}>
              <Typography variant="h2">ReleaseDock</Typography>
            </Box>
            <LockRoundedIcon color="primary" sx={{ fontSize: 40, mb: 1.5 }} />
            <Typography variant="h1" component="h1">로그인</Typography>
            <Typography color="text.secondary" sx={{ mt: 1, mb: 3.5 }}>배포 작업공간에 안전하게 접속하세요.</Typography>
            {ssoOutcome === 'none' && <Alert severity="info" sx={{ mb: 2.5 }}>SSO 세션이 없어 자동 로그인하지 못했습니다. 아래에서 로그인하십시오.</Alert>}
            {ssoOutcome === 'error' && <Alert severity="warning" sx={{ mb: 2.5 }}>SSO 인증이 거부되었습니다. 관리자에게 문의하거나 아래에서 로그인하십시오.</Alert>}
            {error && <Alert severity="error" sx={{ mb: 2.5 }}>{error}</Alert>}
            {authConfig.local_enabled && <Box component="form" onSubmit={(event) => void submit(event)} noValidate>
              <Stack spacing={2.25}>
                <TextField
                  label="사용자 이름"
                  autoComplete="username"
                  autoFocus
                  required
                  fullWidth
                  value={username}
                  onChange={(event) => setUsername(event.target.value)}
                />
                <TextField
                  label="비밀번호"
                  type="password"
                  autoComplete="current-password"
                  required
                  fullWidth
                  value={password}
                  onChange={(event) => setPassword(event.target.value)}
                />
                <Button type="submit" variant="contained" size="large" disabled={submitting || !username.trim() || !password} startIcon={submitting ? <CircularProgress size={18} /> : <LoginRoundedIcon />}>
                  {submitting ? '로그인 중…' : '로컬 계정으로 로그인'}
                </Button>
              </Stack>
            </Box>}
            {authConfig.local_enabled && authConfig.oidc.enabled && <Divider sx={{ my: 3 }}>또는</Divider>}
            {authConfig.oidc.enabled && <Button variant="outlined" fullWidth size="large" onClick={startOidc}>Keycloak SSO로 로그인</Button>}
            {!authConfig.local_enabled && !authConfig.oidc.enabled && <Alert severity="warning">활성화된 로그인 방식이 없습니다. 서비스 관리자에게 문의하세요.</Alert>}
            <Typography variant="caption" color="text.secondary" sx={{ display: 'block', textAlign: 'center', mt: 3 }}>
              ReleaseDock v{version.version}
            </Typography>
          </CardContent>
        </Card>
      </Box>
    </Box>
  );
}
