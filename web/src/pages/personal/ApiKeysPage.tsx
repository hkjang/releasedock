import AddRoundedIcon from '@mui/icons-material/AddRounded';
import ContentCopyRoundedIcon from '@mui/icons-material/ContentCopyRounded';
import DeleteOutlineRoundedIcon from '@mui/icons-material/DeleteOutlineRounded';
import EditRoundedIcon from '@mui/icons-material/EditRounded';
import KeyRoundedIcon from '@mui/icons-material/KeyRounded';
import ReplayRoundedIcon from '@mui/icons-material/ReplayRounded';
import {
  Alert,
  Box,
  Button,
  Card,
  Checkbox,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControl,
  FormControlLabel,
  FormGroup,
  FormLabel,
  IconButton,
  InputAdornment,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material';
import { useMemo, useState } from 'react';
import { api, unwrapItems } from '../../api/client';
import { useAuth } from '../../auth/AuthContext';
import { EmptyState, PageError, PageLoading } from '../../components/Feedback';
import { PageHeader } from '../../components/PageHeader';
import { StatusChip } from '../../components/StatusChip';
import { useAsync } from '../../hooks/useAsync';
import type { ApiKey } from '../../types/domain';
import { formatDate } from '../../utils/format';

const permissionLabels: Record<string, string> = {
  'applications.read': '애플리케이션·환경 조회', 'applications.write': '애플리케이션·환경 변경',
  'profiles.read': '배포 프로필 조회', 'profiles.write': '배포 프로필 변경',
  'releases.read': '릴리즈 조회', 'releases.create': '릴리즈 등록', 'releases.write': '릴리즈 변경', 'releases.submit': '배포·롤백 실행',
  'releases.review': '릴리즈 검토', 'releases.approve': '릴리즈 승인', 'releases.reject': '릴리즈 반려',
  'ai.use': 'AI API 사용', 'mcp.use': 'MCP 도구 사용', 'audit.read': '감사 로그 조회',
};

interface PermissionOption { value: string; label: string }

function ApiKeyDialog({ item, permissionOptions, open, onClose, onSaved, onSecret }: { item?: ApiKey; permissionOptions: PermissionOption[]; open: boolean; onClose: () => void; onSaved: () => Promise<void>; onSecret: (secret: string) => void }) {
  const [name, setName] = useState(item?.name || '');
  const [selected, setSelected] = useState<string[]>(() => {
    const allowed = new Set(permissionOptions.map((permission) => permission.value));
    const existing = item?.permissions.filter((permission) => allowed.has(permission)) ?? [];
    return existing.length ? existing : permissionOptions.slice(0, 1).map((permission) => permission.value);
  });
  const [expiresAt, setExpiresAt] = useState(item?.expiresAt?.slice(0, 10) || '');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string>();
  const toggle = (permission: string) => setSelected((current) => current.includes(permission) ? current.filter((value) => value !== permission) : [...current, permission]);
  const save = async () => {
    setSaving(true); setError(undefined);
    try {
      if (item) await api.updateApiKey(item.id, { name: name.trim(), permissions: selected });
      else {
        const created = await api.createApiKey({ name: name.trim(), permissions: selected, expiresAt: expiresAt || undefined });
        if (created.secret) onSecret(created.secret);
      }
      await onSaved(); onClose();
    } catch (reason) { setError(reason instanceof Error ? reason.message : 'API 키를 저장하지 못했습니다.'); }
    finally { setSaving(false); }
  };
  return (
    <Dialog open={open} onClose={() => !saving && onClose()} fullWidth maxWidth="sm">
      <DialogTitle>{item ? 'API 키 권한 변경' : '새 API 키 생성'}</DialogTitle>
      <DialogContent><Stack spacing={2.5} sx={{ pt: 1 }}>
        <TextField label="키 이름" required value={name} onChange={(e) => setName(e.target.value)} placeholder="예: Jenkins 운영 배포" />
        {!item && <TextField label="만료일 (선택)" type="date" value={expiresAt} onChange={(e) => setExpiresAt(e.target.value)} slotProps={{ inputLabel: { shrink: true } }} />}
        <FormControl component="fieldset"><FormLabel component="legend">허용 권한</FormLabel><FormGroup sx={{ mt: 1, display: 'grid', gridTemplateColumns: { sm: '1fr 1fr' } }}>{permissionOptions.map((permission) => <FormControlLabel key={permission.value} control={<Checkbox checked={selected.includes(permission.value)} onChange={() => toggle(permission.value)} />} label={permission.label} />)}</FormGroup></FormControl>
        <Alert severity="info">최소 권한만 부여하세요. 권한은 키를 재발급하지 않고 변경할 수 있습니다.</Alert>
        {error && <Alert severity="error">{error}</Alert>}
      </Stack></DialogContent>
      <DialogActions sx={{ p: 2.5 }}><Button onClick={onClose} disabled={saving}>취소</Button><Button variant="contained" disabled={saving || !name.trim() || !selected.length} onClick={() => void save()} startIcon={saving ? <CircularProgress size={18} /> : undefined}>{saving ? '저장 중…' : item ? '변경 저장' : '키 생성'}</Button></DialogActions>
    </Dialog>
  );
}

export function ApiKeysPage() {
  const { user } = useAuth();
  const state = useAsync(api.apiKeys, []);
  const [editing, setEditing] = useState<ApiKey | null>();
  const [confirm, setConfirm] = useState<{ type: 'rotate' | 'revoke'; key: ApiKey }>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [secret, setSecret] = useState<string>();
  const [copied, setCopied] = useState(false);
  const keys = useMemo(() => unwrapItems(state.data), [state.data]);
  const permissionOptions = useMemo(() => (user?.permissions ?? [])
    .filter((permission) => permission !== 'keys.manage')
    .map((permission) => ({ value: permission, label: permissionLabels[permission] ?? permission })), [user]);

  const execute = async () => {
    if (!confirm) return;
    setBusy(true); setError(undefined);
    try {
      if (confirm.type === 'rotate') {
        const rotated = await api.rotateApiKey(confirm.key.id);
        setSecret(rotated.secret);
      } else await api.revokeApiKey(confirm.key.id);
      setConfirm(undefined); await state.reload();
    } catch (reason) { setError(reason instanceof Error ? reason.message : '작업을 완료하지 못했습니다.'); }
    finally { setBusy(false); }
  };
  const copySecret = async () => { if (!secret) return; await navigator.clipboard.writeText(secret); setCopied(true); };

  return (
    <>
      <PageHeader title="내 API 키" description="REST API와 MCP에 사용할 개인 키를 생성하고, 권한을 변경하거나 주기적으로 회전합니다." action={<Button variant="contained" startIcon={<AddRoundedIcon />} disabled={!permissionOptions.length} onClick={() => setEditing(null)}>새 API 키</Button>} />
      <Alert severity="warning" sx={{ mb: 2.5 }}>API 키는 비밀번호와 같습니다. 서비스별로 키를 분리하고, 유출이 의심되면 즉시 회전하거나 폐기하세요.</Alert>
      {!permissionOptions.length && <Alert severity="info" sx={{ mb: 2.5 }}>현재 계정에 API 키로 위임할 수 있는 권한이 없습니다.</Alert>}
      <Card>
        {state.loading && <Box sx={{ p: 3 }}><PageLoading /></Box>}
        {state.error && !state.loading && <Box sx={{ p: 2 }}><PageError error={state.error} onRetry={() => void state.reload()} /></Box>}
        {!state.loading && !state.error && (keys.length ? (
          <TableContainer><Table aria-label="내 API 키"><TableHead><TableRow><TableCell>이름 / 접두사</TableCell><TableCell>권한</TableCell><TableCell>최근 사용</TableCell><TableCell>만료일</TableCell><TableCell>상태</TableCell><TableCell align="right">작업</TableCell></TableRow></TableHead><TableBody>
            {keys.map((key) => <TableRow key={key.id} hover><TableCell><Typography fontWeight={750}>{key.name}</Typography><Typography variant="body2" color="text.secondary" fontFamily="ui-monospace, monospace">{key.prefix}••••••••</Typography></TableCell><TableCell><Stack direction="row" spacing={0.5} useFlexGap flexWrap="wrap">{key.permissions.slice(0, 3).map((permission) => <Chip key={permission} label={permission} size="small" variant="outlined" />)}{key.permissions.length > 3 && <Chip label={`+${key.permissions.length - 3}`} size="small" />}</Stack></TableCell><TableCell sx={{ whiteSpace: 'nowrap' }}>{formatDate(key.lastUsedAt)}</TableCell><TableCell sx={{ whiteSpace: 'nowrap' }}>{formatDate(key.expiresAt)}</TableCell><TableCell><StatusChip status={key.active ? 'ACTIVE' : 'INACTIVE'} /></TableCell><TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>{key.active && <><Tooltip title="권한 변경"><IconButton aria-label={`${key.name} 권한 변경`} onClick={() => setEditing(key)}><EditRoundedIcon fontSize="small" /></IconButton></Tooltip><Tooltip title="키 회전"><IconButton aria-label={`${key.name} 키 회전`} onClick={() => setConfirm({ type: 'rotate', key })}><ReplayRoundedIcon fontSize="small" /></IconButton></Tooltip><Tooltip title="키 폐기"><IconButton aria-label={`${key.name} 키 폐기`} color="error" onClick={() => setConfirm({ type: 'revoke', key })}><DeleteOutlineRoundedIcon fontSize="small" /></IconButton></Tooltip></>}</TableCell></TableRow>)}
          </TableBody></Table></TableContainer>
        ) : <EmptyState title="생성된 API 키가 없습니다" description="자동화 또는 MCP 클라이언트별로 최소 권한 키를 생성하세요." action={<Button variant="contained" startIcon={<KeyRoundedIcon />} disabled={!permissionOptions.length} onClick={() => setEditing(null)}>첫 API 키 생성</Button>} />)}
      </Card>

      {editing !== undefined && <ApiKeyDialog key={editing?.id || 'new'} item={editing || undefined} permissionOptions={permissionOptions} open onClose={() => setEditing(undefined)} onSaved={state.reload} onSecret={setSecret} />}
      <Dialog open={Boolean(confirm)} onClose={() => !busy && setConfirm(undefined)}><DialogTitle>{confirm?.type === 'rotate' ? 'API 키 회전' : 'API 키 폐기'}</DialogTitle><DialogContent><Typography>{confirm?.type === 'rotate' ? '기존 키는 즉시 사용할 수 없게 되고 새 키가 발급됩니다. 연동 시스템을 곧바로 갱신할 준비가 되었는지 확인하세요.' : '이 키를 즉시 폐기합니다. 이 작업은 되돌릴 수 없습니다.'}</Typography>{error && <Alert severity="error" sx={{ mt: 2 }}>{error}</Alert>}</DialogContent><DialogActions sx={{ p: 2.5 }}><Button onClick={() => setConfirm(undefined)} disabled={busy}>취소</Button><Button variant="contained" color={confirm?.type === 'revoke' ? 'error' : 'primary'} disabled={busy} onClick={() => void execute()}>{busy ? '처리 중…' : confirm?.type === 'rotate' ? '회전' : '폐기'}</Button></DialogActions></Dialog>
      <Dialog open={Boolean(secret)} onClose={() => { setSecret(undefined); setCopied(false); }} fullWidth maxWidth="sm"><DialogTitle>새 API 키</DialogTitle><DialogContent><Alert severity="warning" sx={{ mb: 2 }}>이 키는 지금 한 번만 표시됩니다. 안전한 비밀 저장소에 보관하세요.</Alert><TextField value={secret || ''} fullWidth slotProps={{ input: { readOnly: true, endAdornment: <InputAdornment position="end"><IconButton aria-label="API 키 복사" onClick={() => void copySecret()}><ContentCopyRoundedIcon /></IconButton></InputAdornment> } }} helperText={copied ? '클립보드에 복사했습니다.' : '키 전체를 복사해 보관하세요.'} /></DialogContent><DialogActions sx={{ p: 2.5 }}><Button variant="contained" onClick={() => { setSecret(undefined); setCopied(false); }}>보관 완료</Button></DialogActions></Dialog>
    </>
  );
}
