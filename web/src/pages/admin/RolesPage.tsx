import { useEffect, useMemo, useState } from 'react';
import AddRoundedIcon from '@mui/icons-material/AddRounded';
import ContentCopyRoundedIcon from '@mui/icons-material/ContentCopyRounded';
import DeleteOutlineRoundedIcon from '@mui/icons-material/DeleteOutlineRounded';
import LockRoundedIcon from '@mui/icons-material/LockRounded';
import SaveRoundedIcon from '@mui/icons-material/SaveRounded';
import SearchRoundedIcon from '@mui/icons-material/SearchRounded';
import {
  Alert,
  Box,
  Button,
  Card,
  CardContent,
  Checkbox,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Divider,
  FormControlLabel,
  IconButton,
  InputAdornment,
  List,
  ListItemButton,
  ListItemText,
  MenuItem,
  Stack,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material';
import { api } from '../../api/client';
import { useAuth } from '../../auth/AuthContext';
import { PageError, PageLoading } from '../../components/Feedback';
import { PageHeader } from '../../components/PageHeader';
import { useAsync } from '../../hooks/useAsync';

interface Role {
  id: string;
  name: string;
  description: string;
  system: boolean;
  permissions: string[];
}

interface Permission {
  code: string;
  description: string;
}

/** role-admin is refused by the server for any modification. */
const PROTECTED_ROLE_ID = 'role-admin';

const GROUP_LABELS: Record<string, string> = {
  admin: '서비스 관리',
  applications: '애플리케이션 및 환경',
  profiles: '배포 프로필',
  releases: '릴리즈 및 배포',
  simple: '심플 모드',
  keys: '개인 API 키',
  audit: '감사',
  ai: 'AI',
  mcp: 'MCP',
};

function groupOf(code: string): string {
  const [head] = code.split('.');
  return head || 'other';
}

function groupLabel(group: string): string {
  return GROUP_LABELS[group] ?? group;
}

export function RolesPage() {
  const { hasPermission } = useAuth();
  const canWrite = hasPermission('admin.rbac.write');
  const roles = useAsync(async () => {
    const response = await api.getResource<Role>('/admin/roles', { page: 1, pageSize: 200 });
    return Array.isArray(response) ? response : response.items;
  }, []);
  const permissions = useAsync(() => api.permissions(), []);

  const [selectedId, setSelectedId] = useState('');
  const [draft, setDraft] = useState<string[]>([]);
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [filter, setFilter] = useState('');
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{ severity: 'success' | 'error'; text: string }>();
  const [createOpen, setCreateOpen] = useState(false);

  const roleList = roles.data ?? [];
  const selected = useMemo(() => roleList.find((role) => role.id === selectedId), [roleList, selectedId]);

  useEffect(() => {
    if (!roleList.length) return;
    if (!roleList.some((role) => role.id === selectedId)) setSelectedId(roleList[0].id);
  }, [roleList, selectedId]);

  useEffect(() => {
    if (!selected) return;
    setDraft([...selected.permissions]);
    setName(selected.name);
    setDescription(selected.description);
    setMessage(undefined);
  }, [selected]);

  // The server refuses to grant a permission the actor does not hold, so the
  // same limit is shown here rather than surfacing a 403 after the fact.
  const grantable = (code: string) => hasPermission(code);
  const locked = Boolean(selected && selected.id === PROTECTED_ROLE_ID);
  const editable = canWrite && Boolean(selected) && !locked;

  const grouped = useMemo(() => {
    const query = filter.trim().toLowerCase();
    const buckets = new Map<string, Permission[]>();
    (permissions.data ?? []).forEach((permission) => {
      if (query && !permission.code.toLowerCase().includes(query) && !permission.description.toLowerCase().includes(query)) return;
      const group = groupOf(permission.code);
      buckets.set(group, [...(buckets.get(group) ?? []), permission]);
    });
    return [...buckets.entries()].sort(([a], [b]) => groupLabel(a).localeCompare(groupLabel(b), 'ko'));
  }, [permissions.data, filter]);

  const dirty = useMemo(() => {
    if (!selected) return false;
    const before = [...selected.permissions].sort().join(',');
    const after = [...draft].sort().join(',');
    return before !== after || name !== selected.name || description !== selected.description;
  }, [selected, draft, name, description]);

  const toggle = (code: string) =>
    setDraft((current) => (current.includes(code) ? current.filter((value) => value !== code) : [...current, code]));

  const toggleGroup = (items: Permission[], on: boolean) =>
    setDraft((current) => {
      const codes = items.filter((item) => grantable(item.code)).map((item) => item.code);
      return on ? [...new Set([...current, ...codes])] : current.filter((value) => !codes.includes(value));
    });

  const save = async () => {
    if (!selected) return;
    setSaving(true);
    setMessage(undefined);
    try {
      // Name and description go through the resource update; permissions have
      // their own endpoint, and updateResource already calls both.
      await api.updateResource<Role>('/admin/roles', selected.id, { name, description, permissions: draft });
      await roles.reload();
      setMessage({ severity: 'success', text: '역할을 저장했습니다. 해당 역할을 가진 사용자에게 즉시 적용됩니다.' });
    } catch (reason) {
      setMessage({ severity: 'error', text: reason instanceof Error ? reason.message : '저장하지 못했습니다.' });
    } finally {
      setSaving(false);
    }
  };

  const remove = async () => {
    if (!selected) return;
    setSaving(true);
    try {
      await api.deleteResource('/admin/roles', selected.id);
      setSelectedId('');
      await roles.reload();
      setMessage({ severity: 'success', text: '역할을 삭제했습니다.' });
    } catch (reason) {
      setMessage({ severity: 'error', text: reason instanceof Error ? reason.message : '삭제하지 못했습니다.' });
    } finally {
      setSaving(false);
    }
  };

  if (roles.loading || permissions.loading) return <PageLoading label="역할과 권한을 불러오는 중입니다" />;
  if (roles.error) return <PageError error={roles.error} onRetry={() => void roles.reload()} />;
  if (permissions.error) return <PageError error={permissions.error} onRetry={() => void permissions.reload()} />;

  const selectedCount = draft.length;
  const totalCount = (permissions.data ?? []).length;

  return (
    <>
      <PageHeader
        title="역할 및 권한"
        description="역할별 권한을 그룹 단위로 확인하고 변경합니다."
        action={canWrite ? (
          <Button variant="contained" startIcon={<AddRoundedIcon />} onClick={() => setCreateOpen(true)}>역할 추가</Button>
        ) : undefined}
      />

      <Stack direction={{ xs: 'column', md: 'row' }} spacing={2.5} alignItems="stretch">
        <Card sx={{ width: { xs: '100%', md: 300 }, flexShrink: 0 }}>
          <List disablePadding>
            {roleList.map((role) => (
              <ListItemButton
                key={role.id}
                selected={role.id === selectedId}
                onClick={() => setSelectedId(role.id)}
                sx={{ py: 1.25 }}
              >
                <ListItemText
                  primary={
                    <Stack direction="row" alignItems="center" spacing={0.75}>
                      <span>{role.name}</span>
                      {role.id === PROTECTED_ROLE_ID && (
                        <Tooltip title="보호된 역할이라 변경할 수 없습니다"><LockRoundedIcon sx={{ fontSize: 15, color: 'text.secondary' }} /></Tooltip>
                      )}
                    </Stack>
                  }
                  secondary={`권한 ${role.permissions.length}개${role.system ? ' · 시스템 역할' : ''}`}
                />
              </ListItemButton>
            ))}
          </List>
        </Card>

        <Card sx={{ flex: 1, minWidth: 0 }}>
          <CardContent>
            {!selected ? (
              <Typography color="text.secondary">왼쪽에서 역할을 선택하십시오.</Typography>
            ) : (
              <Stack spacing={2.25}>
                {message && <Alert severity={message.severity}>{message.text}</Alert>}
                {locked && (
                  <Alert severity="info">
                    <strong>{selected.name}</strong>은 보호된 역할입니다. 권한을 바꿀 수 없으며 항상 모든 권한을 가집니다.
                  </Alert>
                )}
                {!canWrite && <Alert severity="info">조회만 가능합니다. 변경에는 admin.rbac.write 권한이 필요합니다.</Alert>}

                <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2}>
                  <TextField label="역할 이름" value={name} onChange={(event) => setName(event.target.value)} disabled={!editable} fullWidth />
                  <TextField label="설명" value={description} onChange={(event) => setDescription(event.target.value)} disabled={!editable} fullWidth />
                </Stack>

                <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2} alignItems={{ sm: 'center' }}>
                  <TextField
                    placeholder="권한 검색 (코드 또는 설명)"
                    value={filter}
                    onChange={(event) => setFilter(event.target.value)}
                    slotProps={{ input: { startAdornment: <InputAdornment position="start"><SearchRoundedIcon fontSize="small" /></InputAdornment> } }}
                    sx={{ flex: 1 }}
                  />
                  <Chip label={`선택 ${selectedCount} / 전체 ${totalCount}`} />
                </Stack>

                <Divider />

                <Stack spacing={2} sx={{ maxHeight: 520, overflowY: 'auto', pr: 0.5 }}>
                  {grouped.map(([group, items]) => {
                    const codes = items.map((item) => item.code);
                    const chosen = codes.filter((code) => draft.includes(code));
                    const assignable = items.filter((item) => grantable(item.code));
                    const allChosen = assignable.length > 0 && assignable.every((item) => draft.includes(item.code));
                    return (
                      <Box key={group}>
                        <FormControlLabel
                          control={
                            <Checkbox
                              checked={allChosen}
                              indeterminate={chosen.length > 0 && !allChosen}
                              disabled={!editable || assignable.length === 0}
                              onChange={(event) => toggleGroup(items, event.target.checked)}
                            />
                          }
                          label={
                            <Typography fontWeight={750}>
                              {groupLabel(group)}{' '}
                              <Typography component="span" variant="body2" color="text.secondary">
                                ({chosen.length}/{codes.length})
                              </Typography>
                            </Typography>
                          }
                        />
                        <Stack sx={{ pl: 3.5 }}>
                          {items.map((permission) => {
                            const allowed = grantable(permission.code);
                            return (
                              <Tooltip
                                key={permission.code}
                                title={allowed ? '' : '본인이 보유하지 않은 권한은 부여할 수 없습니다'}
                                placement="top-start"
                              >
                                <FormControlLabel
                                  control={
                                    <Checkbox
                                      size="small"
                                      checked={draft.includes(permission.code)}
                                      disabled={!editable || !allowed}
                                      onChange={() => toggle(permission.code)}
                                    />
                                  }
                                  label={
                                    <Box>
                                      <Typography variant="body2" component="span" sx={{ fontFamily: 'monospace' }}>{permission.code}</Typography>
                                      {permission.description && (
                                        <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
                                          {permission.description}
                                        </Typography>
                                      )}
                                    </Box>
                                  }
                                  sx={{ alignItems: 'flex-start', my: 0.25 }}
                                />
                              </Tooltip>
                            );
                          })}
                        </Stack>
                      </Box>
                    );
                  })}
                  {!grouped.length && <Typography color="text.secondary">검색 결과가 없습니다.</Typography>}
                </Stack>

                <Divider />

                <Stack direction="row" spacing={1.5} alignItems="center">
                  <Button
                    variant="contained"
                    startIcon={saving ? <CircularProgress size={18} /> : <SaveRoundedIcon />}
                    disabled={!editable || !dirty || saving}
                    onClick={() => void save()}
                  >
                    {saving ? '저장 중…' : '변경사항 저장'}
                  </Button>
                  <Button
                    color="inherit"
                    disabled={!dirty || saving}
                    onClick={() => {
                      setDraft([...selected.permissions]);
                      setName(selected.name);
                      setDescription(selected.description);
                    }}
                  >
                    되돌리기
                  </Button>
                  <Box sx={{ flex: 1 }} />
                  {canWrite && !selected.system && (
                    <Tooltip title="역할 삭제">
                      <IconButton color="error" aria-label="역할 삭제" disabled={saving} onClick={() => void remove()}>
                        <DeleteOutlineRoundedIcon />
                      </IconButton>
                    </Tooltip>
                  )}
                </Stack>
                {selected.system && (
                  <Typography variant="caption" color="text.secondary">
                    시스템 역할은 삭제할 수 없습니다. 권한 구성만 변경할 수 있습니다.
                  </Typography>
                )}
              </Stack>
            )}
          </CardContent>
        </Card>
      </Stack>

      <CreateRoleDialog
        open={createOpen}
        roles={roleList}
        onClose={() => setCreateOpen(false)}
        onCreated={async (created) => {
          await roles.reload();
          setSelectedId(created.id);
        }}
      />
    </>
  );
}

function CreateRoleDialog({
  open,
  roles,
  onClose,
  onCreated,
}: {
  open: boolean;
  roles: Role[];
  onClose: () => void;
  onCreated: (created: Role) => Promise<void>;
}) {
  const { hasPermission } = useAuth();
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [copyFrom, setCopyFrom] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string>();

  useEffect(() => {
    if (open) {
      setName('');
      setDescription('');
      setCopyFrom('');
      setError(undefined);
    }
  }, [open]);

  const create = async () => {
    setSaving(true);
    setError(undefined);
    try {
      // Copying drops anything the actor cannot grant, which the server would
      // reject anyway.
      const source = roles.find((role) => role.id === copyFrom);
      const permissions = (source?.permissions ?? []).filter((code) => hasPermission(code));
      const created = await api.createResource<Role>('/admin/roles', { name, description, permissions });
      await onCreated(created);
      onClose();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '역할을 만들지 못했습니다.');
    } finally {
      setSaving(false);
    }
  };

  const source = roles.find((role) => role.id === copyFrom);
  const copyable = (source?.permissions ?? []).filter((code) => hasPermission(code)).length;
  const dropped = (source?.permissions ?? []).length - copyable;

  return (
    <Dialog open={open} onClose={() => !saving && onClose()} fullWidth maxWidth="sm">
      <DialogTitle>역할 추가</DialogTitle>
      <DialogContent>
        <Stack spacing={2.25} sx={{ pt: 1 }}>
          <TextField label="역할 이름" required value={name} onChange={(event) => setName(event.target.value)} fullWidth />
          <TextField label="설명" value={description} onChange={(event) => setDescription(event.target.value)} multiline minRows={2} fullWidth />
          <TextField
            select
            label="권한 복사 (선택)"
            value={copyFrom}
            onChange={(event) => setCopyFrom(event.target.value)}
            helperText="기존 역할의 권한 구성을 그대로 가져와 시작합니다. 생성 후 세부 조정할 수 있습니다."
            slotProps={{ input: { startAdornment: copyFrom ? <InputAdornment position="start"><ContentCopyRoundedIcon fontSize="small" /></InputAdornment> : undefined } }}
            fullWidth
          >
            <MenuItem value="">복사하지 않음</MenuItem>
            {roles.map((role) => (
              <MenuItem key={role.id} value={role.id}>{role.name} (권한 {role.permissions.length}개)</MenuItem>
            ))}
          </TextField>
          {Boolean(dropped) && (
            <Alert severity="info">
              복사 대상의 권한 {dropped}개는 본인이 보유하지 않아 제외됩니다. {copyable}개만 부여됩니다.
            </Alert>
          )}
          {error && <Alert severity="error">{error}</Alert>}
        </Stack>
      </DialogContent>
      <DialogActions sx={{ p: 2.5 }}>
        <Button onClick={onClose} disabled={saving}>취소</Button>
        <Button variant="contained" disabled={saving || !name.trim()} onClick={() => void create()}>
          {saving ? '만드는 중…' : '만들기'}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
