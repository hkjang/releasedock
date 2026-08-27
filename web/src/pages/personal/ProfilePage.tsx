import SaveRoundedIcon from '@mui/icons-material/SaveRounded';
import {
  Alert,
  Avatar,
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
import { api } from '../../api/client';
import { useAuth } from '../../auth/AuthContext';
import { PageHeader } from '../../components/PageHeader';
import { formatDate } from '../../utils/format';

export function ProfilePage() {
  const { user, refresh } = useAuth();
  const [displayName, setDisplayName] = useState(user?.displayName || '');
  const [email, setEmail] = useState(user?.email || '');
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{ type: 'success' | 'error'; text: string }>();
	const [currentPassword, setCurrentPassword] = useState('');
	const [newPassword, setNewPassword] = useState('');
	const [confirmPassword, setConfirmPassword] = useState('');
	const [passwordSaving, setPasswordSaving] = useState(false);

  useEffect(() => {
    setDisplayName(user?.displayName || '');
    setEmail(user?.email || '');
  }, [user]);

  const save = async (event: FormEvent) => {
    event.preventDefault();
    setSaving(true);
    setMessage(undefined);
    try {
      await api.updateProfile({ displayName: displayName.trim(), email: email.trim() });
      await refresh();
      setMessage({ type: 'success', text: '프로필을 저장했습니다.' });
    } catch (reason) {
      setMessage({ type: 'error', text: reason instanceof Error ? reason.message : '프로필을 저장하지 못했습니다.' });
    } finally {
      setSaving(false);
    }
  };

	const changePassword = async (event: FormEvent) => {
		event.preventDefault();
		if (newPassword !== confirmPassword) {
			setMessage({ type: 'error', text: '새 비밀번호 확인이 일치하지 않습니다.' });
			return;
		}
		setPasswordSaving(true);
		setMessage(undefined);
		try {
			await api.changePassword(currentPassword, newPassword);
			setCurrentPassword('');
			setNewPassword('');
			setConfirmPassword('');
			setMessage({ type: 'success', text: '비밀번호를 변경하고 다른 기기의 세션을 종료했습니다.' });
		} catch (reason) {
			setMessage({ type: 'error', text: reason instanceof Error ? reason.message : '비밀번호를 변경하지 못했습니다.' });
		} finally {
			setPasswordSaving(false);
		}
	};

  return (
    <>
      <PageHeader title="내 프로필" description="서비스 관리 설정과 분리된 개인 계정 정보입니다." />
      <Stack spacing={2.5}>
        {message && <Alert severity={message.type}>{message.text}</Alert>}
        <Card>
          <CardContent sx={{ p: { xs: 2.5, md: 4 } }}>
            <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2.5} alignItems={{ xs: 'flex-start', sm: 'center' }}>
              <Avatar sx={{ width: 72, height: 72, bgcolor: 'primary.dark', fontSize: '1.5rem', fontWeight: 800 }}>{(user?.displayName || user?.username || 'U').slice(0, 2).toUpperCase()}</Avatar>
              <Box><Typography variant="h2">{user?.displayName || user?.username}</Typography><Typography color="text.secondary">{user?.username} · {user?.source === 'oidc' ? 'Keycloak OIDC' : '로컬 계정'}</Typography></Box>
            </Stack>
            <Divider sx={{ my: 3.5 }} />
            <Box component="form" onSubmit={(event) => void save(event)}>
              <Stack spacing={2.25}>
                <TextField label="사용자 이름" value={user?.username || ''} disabled helperText="사용자 이름은 관리자가 변경할 수 있습니다." />
                <TextField label="표시 이름" required value={displayName} onChange={(event) => setDisplayName(event.target.value)} />
                <TextField label="이메일" type="email" value={email} onChange={(event) => setEmail(event.target.value)} disabled={user?.source === 'oidc'} helperText={user?.source === 'oidc' ? 'Keycloak에서 동기화되는 정보입니다.' : undefined} />
                <Box><Typography variant="caption" color="text.secondary">역할</Typography><Typography sx={{ mt: 0.25 }}>{user?.roles.join(', ') || '—'}</Typography></Box>
                <Box><Typography variant="caption" color="text.secondary">최근 로그인</Typography><Typography sx={{ mt: 0.25 }}>{formatDate(user?.lastLoginAt)}</Typography></Box>
                <Box sx={{ display: 'flex', justifyContent: 'flex-end' }}><Button type="submit" variant="contained" disabled={saving || !displayName.trim()} startIcon={saving ? <CircularProgress size={18} /> : <SaveRoundedIcon />}>{saving ? '저장 중…' : '프로필 저장'}</Button></Box>
              </Stack>
            </Box>
          </CardContent>
        </Card>
		{user?.source !== 'oidc' && <Card>
			<CardContent sx={{ p: { xs: 2.5, md: 4 } }}>
				<Typography variant="h3">비밀번호 변경</Typography>
				<Typography color="text.secondary" variant="body2" sx={{ mt: 0.5, mb: 3 }}>최소 12자로 설정하세요. 변경하면 현재 브라우저를 제외한 로그인 세션이 종료됩니다.</Typography>
				<Box component="form" onSubmit={(event) => void changePassword(event)}>
					<Stack spacing={2.25}>
						<TextField label="현재 비밀번호" type="password" required value={currentPassword} onChange={(event) => setCurrentPassword(event.target.value)} autoComplete="current-password" />
						<TextField label="새 비밀번호" type="password" required value={newPassword} onChange={(event) => setNewPassword(event.target.value)} autoComplete="new-password" inputProps={{ minLength: 12, maxLength: 1024 }} />
						<TextField label="새 비밀번호 확인" type="password" required value={confirmPassword} onChange={(event) => setConfirmPassword(event.target.value)} autoComplete="new-password" error={Boolean(confirmPassword && newPassword !== confirmPassword)} />
						<Box sx={{ display: 'flex', justifyContent: 'flex-end' }}><Button type="submit" variant="contained" disabled={passwordSaving || currentPassword.length === 0 || newPassword.length < 12 || newPassword !== confirmPassword}>{passwordSaving ? '변경 중…' : '비밀번호 변경'}</Button></Box>
					</Stack>
				</Box>
			</CardContent>
		</Card>}
      </Stack>
    </>
  );
}
