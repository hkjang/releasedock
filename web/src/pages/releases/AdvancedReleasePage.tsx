import CloudUploadRoundedIcon from '@mui/icons-material/CloudUploadRounded';
import InfoOutlinedIcon from '@mui/icons-material/InfoOutlined';
import {
  Alert,
  Box,
  Button,
  Card,
  CardContent,
  CircularProgress,
  FormControl,
  FormHelperText,
  InputLabel,
  MenuItem,
  Select,
  Stack,
  TextField,
  Typography,
} from '@mui/material';
import { alpha } from '@mui/material/styles';
import { type FormEvent, useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api, unwrapItems } from '../../api/client';
import { PageError, PageLoading } from '../../components/Feedback';
import { PageHeader } from '../../components/PageHeader';
import { useAsync } from '../../hooks/useAsync';
import { formatBytes } from '../../utils/format';

async function loadOptions() {
  const [applications, environments, profiles] = await Promise.all([api.applications(), api.environments(), api.profiles()]);
  return {
    applications: unwrapItems(applications).filter((item) => item.active !== false),
    environments: unwrapItems(environments).filter((item) => item.active !== false),
    profiles: unwrapItems(profiles).filter((item) => item.active !== false),
  };
}

export function AdvancedReleasePage() {
  const options = useAsync(loadOptions, []);
  const navigate = useNavigate();
  const [applicationId, setApplicationId] = useState('');
  const [environmentId, setEnvironmentId] = useState('');
  const [profileId, setProfileId] = useState('');
  const [version, setVersion] = useState('');
  const [notes, setNotes] = useState('');
  const [file, setFile] = useState<File>();
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string>();

  const filteredProfiles = useMemo(() => options.data?.profiles.filter((profile) => (!profile.applicationId || profile.applicationId === applicationId) && (!profile.environmentId || profile.environmentId === environmentId)) ?? [], [options.data, applicationId, environmentId]);
  const filteredEnvironments = useMemo(() => options.data?.environments.filter((environment) => !applicationId || !environment.applicationId || environment.applicationId === applicationId) ?? [], [options.data, applicationId]);
  const validFile = file && (file.name.toLowerCase().endsWith('.tar') || file.name.toLowerCase().endsWith('.tar.gz'));

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!file || !validFile) return;
    setSubmitting(true);
    setError(undefined);
    try {
      const release = await api.createRelease({ applicationId, environmentId, deploymentProfileId: profileId || undefined, version: version.trim(), notes: notes.trim(), artifact: file });
      navigate(`/releases/${release.id}`, { replace: true });
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '릴리즈를 등록하지 못했습니다.');
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <>
      <PageHeader title="운영자 고급 등록" description="서비스, 환경과 운영 정책을 직접 지정해야 하는 예외 릴리즈를 등록합니다." crumbs={[{ label: '릴리즈', to: '/releases' }, { label: '새 버전 배포', to: '/releases/new' }, { label: '고급 등록' }]} />
      {options.loading && <PageLoading label="등록 옵션을 불러오는 중입니다" />}
      {options.error && !options.loading && <PageError error={options.error} onRetry={() => void options.reload()} />}
      {options.data && !options.loading && (
        <Card>
          <CardContent sx={{ p: { xs: 2.5, md: 4 } }}>
            {error && <Alert severity="error" sx={{ mb: 3 }}>{error}</Alert>}
            <Box component="form" onSubmit={(event) => void submit(event)}>
              <Stack spacing={3}>
                <Alert severity="warning">이 화면은 운영자를 위한 예외 경로입니다. 일반 배포는 파일명 기반 원클릭 배포를 사용하세요.</Alert>
                <Box>
                  <Typography variant="h3" sx={{ mb: 0.5 }}>대상 정보</Typography>
                  <Typography color="text.secondary" variant="body2">애플리케이션과 환경에 맞는 배포 프로필을 선택하세요.</Typography>
                </Box>
                <Stack direction={{ xs: 'column', md: 'row' }} spacing={2}>
                  <FormControl fullWidth required>
                    <InputLabel id="advanced-application-label">애플리케이션</InputLabel>
                    <Select labelId="advanced-application-label" label="애플리케이션" value={applicationId} onChange={(event) => { setApplicationId(event.target.value); setEnvironmentId(''); setProfileId(''); }}>
                      {options.data.applications.map((item) => <MenuItem key={item.id} value={item.id}>{item.name} ({item.code})</MenuItem>)}
                    </Select>
                    {!options.data.applications.length && <FormHelperText error>먼저 관리 메뉴에서 애플리케이션을 등록하세요.</FormHelperText>}
                  </FormControl>
                  <FormControl fullWidth required>
                    <InputLabel id="advanced-environment-label">대상 환경</InputLabel>
                    <Select labelId="advanced-environment-label" label="대상 환경" value={environmentId} onChange={(event) => { setEnvironmentId(event.target.value); setProfileId(''); }}>
                      {filteredEnvironments.map((item) => <MenuItem key={item.id} value={item.id}>{item.name} ({item.kind || item.code})</MenuItem>)}
                    </Select>
                    {!options.data.environments.length && <FormHelperText error>먼저 관리 메뉴에서 환경을 등록하세요.</FormHelperText>}
                  </FormControl>
                </Stack>
                <Stack direction={{ xs: 'column', md: 'row' }} spacing={2}>
                  <TextField label="릴리즈 버전" placeholder="예: 2.4.1" required fullWidth value={version} onChange={(event) => setVersion(event.target.value)} helperText="애플리케이션에서 고유한 SemVer 형식을 권장합니다." />
                  <FormControl fullWidth>
                    <InputLabel id="advanced-profile-label">배포 프로필</InputLabel>
                    <Select labelId="advanced-profile-label" label="배포 프로필" value={profileId} onChange={(event) => setProfileId(event.target.value)} disabled={!applicationId || !environmentId}>
                      <MenuItem value="">자동 선택</MenuItem>
                      {filteredProfiles.map((item) => <MenuItem key={item.id} value={item.id}>{item.name}{item.approvalRequired ? ' · 승인 필요' : ''}</MenuItem>)}
                    </Select>
                    <FormHelperText>미선택 시 서버의 기본 프로필을 사용합니다.</FormHelperText>
                  </FormControl>
                </Stack>

                <Box sx={{ pt: 1 }}>
                  <Typography variant="h3" sx={{ mb: 1 }}>릴리즈 패키지</Typography>
                  <Box component="label" sx={{ display: 'grid', placeItems: 'center', minHeight: 210, px: 3, py: 4, textAlign: 'center', cursor: 'pointer', border: '2px dashed', borderColor: file ? 'primary.main' : 'divider', bgcolor: file ? alpha('#68a9ff', 0.08) : alpha('#07101f', 0.35), borderRadius: 2.5, transition: 'border-color .15s, background-color .15s', '&:hover': { borderColor: 'primary.main', bgcolor: alpha('#68a9ff', 0.08) } }}>
                    <input hidden type="file" accept=".tar,.tar.gz,application/x-tar,application/gzip" onChange={(event) => setFile(event.target.files?.[0])} />
                    <Box>
                      <CloudUploadRoundedIcon color="primary" sx={{ fontSize: 48, mb: 1 }} />
                      <Typography fontWeight={750}>{file ? file.name : '클릭하여 패키지를 선택하세요'}</Typography>
                      <Typography color="text.secondary" variant="body2" sx={{ mt: 0.5 }}>{file ? formatBytes(file.size) : '.tar, .tar.gz 파일 지원'}</Typography>
                    </Box>
                  </Box>
                  {file && !validFile && <Alert severity="error" sx={{ mt: 1.5 }}>지원하지 않는 파일 형식입니다.</Alert>}
                </Box>
                <TextField label="릴리즈 메모" placeholder="변경 내용과 운영 참고 사항을 입력하세요." multiline minRows={3} value={notes} onChange={(event) => setNotes(event.target.value)} inputProps={{ maxLength: 2000 }} helperText={`${notes.length}/2,000`} />
                <Alert severity="info" icon={<InfoOutlinedIcon />}>
                  업로드 후 서버에서 SHA256 무결성 검사와 경로 traversal 방지 검사를 수행합니다. 배포 스크립트는 관리자가 승인한 템플릿만 실행됩니다.
                </Alert>
                <Stack direction="row" justifyContent="flex-end" spacing={1.5}>
                  <Button variant="text" onClick={() => navigate('/releases/new')} disabled={submitting}>원클릭 배포로 돌아가기</Button>
                  <Button variant="outlined" onClick={() => navigate('/releases')} disabled={submitting}>취소</Button>
                  <Button type="submit" variant="contained" disabled={submitting || !applicationId || !environmentId || !version.trim() || !validFile} startIcon={submitting ? <CircularProgress size={18} /> : <CloudUploadRoundedIcon />}>
                    {submitting ? '업로드 중…' : '릴리즈 등록'}
                  </Button>
                </Stack>
              </Stack>
            </Box>
          </CardContent>
        </Card>
      )}
    </>
  );
}
