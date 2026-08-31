import AddRoundedIcon from '@mui/icons-material/AddRounded';
import DeleteOutlineRoundedIcon from '@mui/icons-material/DeleteOutlineRounded';
import EditRoundedIcon from '@mui/icons-material/EditRounded';
import ReplayRoundedIcon from '@mui/icons-material/ReplayRounded';
import SearchRoundedIcon from '@mui/icons-material/SearchRounded';
import VisibilityOffRoundedIcon from '@mui/icons-material/VisibilityOffRounded';
import VisibilityRoundedIcon from '@mui/icons-material/VisibilityRounded';
import {
  Alert,
  Box,
  Button,
  Card,
  Checkbox,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  IconButton,
  InputAdornment,
  MenuItem,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TablePagination,
  TableRow,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material';
import { useMemo, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import { api, unwrapItems } from '../../api/client';
import { useAuth } from '../../auth/AuthContext';
import { EmptyState, PageError, PageLoading } from '../../components/Feedback';
import { PageHeader } from '../../components/PageHeader';
import { StatusChip } from '../../components/StatusChip';
import { useAsync } from '../../hooks/useAsync';
import { formatDate } from '../../utils/format';

interface ResourceRow extends Record<string, unknown> {
  id: string;
}

interface FieldConfig {
  key: string;
  label: string;
  type?: 'text' | 'number' | 'password' | 'multiline' | 'secret-multiline' | 'boolean' | 'select';
  required?: boolean;
  requiredOnCreate?: boolean;
  createOnly?: boolean;
  helperText?: string;
  defaultValue?: unknown;
  options?: Array<{ label: string; value: string }>;
  relation?: 'applications' | 'environments' | 'profiles' | 'registries' | 'scripts' | 'targetCredentials';
  scriptType?: 'PRE_CHECK' | 'DEPLOY' | 'HEALTH_CHECK' | 'ROLLBACK';
  pattern?: RegExp;
}

interface ColumnConfig {
  key: string;
  label: string;
  status?: boolean;
  secret?: boolean;
}

export interface ResourceConfig {
  title: string;
  description: string;
  singular: string;
  endpoint: string;
  columns: ColumnConfig[];
  fields: FieldConfig[];
  allowCreate?: boolean;
  allowDelete?: boolean;
  allowSecretRotation?: boolean;
  writePermission: string;
}

export const resourceConfigs = {
  applications: {
    title: '애플리케이션 관리', description: '릴리즈 대상 시스템과 식별 코드를 관리합니다.', singular: '애플리케이션', endpoint: '/applications',
    writePermission: 'applications.write',
    columns: [{ key: 'name', label: '이름' }, { key: 'code', label: '코드' }, { key: 'description', label: '설명' }, { key: 'active', label: '상태', status: true }],
    fields: [{ key: 'name', label: '이름', required: true }, { key: 'code', label: '코드', required: true, helperText: '영문 소문자, 숫자, 하이픈 사용을 권장합니다.' }, { key: 'description', label: '설명', type: 'multiline' }, { key: 'active', label: '활성화', type: 'boolean', defaultValue: true }],
  },
  environments: {
    title: '환경 관리', description: 'DEV, STG, PRD 등 배포 환경과 보호 정책을 관리합니다.', singular: '환경', endpoint: '/environments',
    writePermission: 'applications.write',
    columns: [{ key: 'name', label: '이름' }, { key: 'code', label: '코드' }, { key: 'kind', label: '구분' }, { key: 'applicationId', label: '애플리케이션 ID' }, { key: 'active', label: '상태', status: true }],
    fields: [{ key: 'applicationId', label: '애플리케이션', required: true, relation: 'applications' }, { key: 'name', label: '이름', required: true }, { key: 'code', label: '코드', required: true }, { key: 'kind', label: '환경 구분', type: 'select', required: true, options: [{ label: '개발 (DEV)', value: 'DEV' }, { label: '스테이징 (STG)', value: 'STG' }, { label: '운영 (PRD)', value: 'PRD' }] }, { key: 'description', label: '설명', type: 'multiline' }, { key: 'protected', label: '보호 환경', type: 'boolean', defaultValue: false }, { key: 'active', label: '활성화', type: 'boolean', defaultValue: true }],
  },
  profiles: {
    title: '배포 프로필', description: 'Registry, 승인 스크립트, 검증, 승인과 실행 제한을 묶은 환경별 정책입니다.', singular: '배포 프로필', endpoint: '/deployment-profiles',
    writePermission: 'profiles.write',
    columns: [{ key: 'name', label: '이름' }, { key: 'applicationId', label: '애플리케이션 ID' }, { key: 'environmentId', label: '환경 ID' }, { key: 'runnerLabels', label: 'Runner 레이블' }, { key: 'runtimeKind', label: 'Runtime' }, { key: 'registryHost', label: 'Registry' }, { key: 'approvalRequired', label: '승인', status: true }, { key: 'active', label: '상태', status: true }],
    fields: [{ key: 'name', label: '이름', required: true }, { key: 'applicationId', label: '애플리케이션', required: true, relation: 'applications' }, { key: 'environmentId', label: '환경', required: true, relation: 'environments' }, { key: 'description', label: '설명', type: 'multiline' }, { key: 'runnerLabels', label: '필수 Runner 레이블', helperText: '쉼표로 구분합니다. 예: prod,docker. 비우면 모든 활성 Runner가 대상입니다.' }, { key: 'registryId', label: 'Harbor Registry', relation: 'registries' }, { key: 'preScriptId', label: 'Pre-check Script', relation: 'scripts', scriptType: 'PRE_CHECK' }, { key: 'deployScriptId', label: 'Deploy Script', relation: 'scripts', scriptType: 'DEPLOY' }, { key: 'healthScriptId', label: 'Health-check Script', relation: 'scripts', scriptType: 'HEALTH_CHECK' }, { key: 'rollbackScriptId', label: 'Rollback Script', relation: 'scripts', scriptType: 'ROLLBACK' }, { key: 'runtimeKind', label: 'Container Runtime', type: 'select', required: true, defaultValue: 'docker', options: [{ label: 'Docker', value: 'docker' }, { label: 'Podman', value: 'podman' }, { label: 'containerd', value: 'containerd' }] }, { key: 'runtimeBinaryPath', label: 'Runtime 실행 파일', defaultValue: '/usr/bin/docker', helperText: '선택한 Runtime과 이름이 일치하는 /usr/bin, /usr/local/bin, /usr/sbin, /usr/local/sbin 경로만 허용됩니다.' }, { key: 'containerdNamespace', label: 'containerd Namespace', defaultValue: 'default' }, { key: 'registryUrl', label: 'Harbor Registry URL', helperText: 'Credential을 선택하면 자동 반영됩니다.' }, { key: 'registryHost', label: 'Registry Host' }, { key: 'registryProject', label: 'Registry Project' }, { key: 'registryInsecure', label: 'Registry TLS 검증 생략', type: 'boolean', defaultValue: false }, { key: 'registryCaPem', label: 'Registry CA PEM', type: 'multiline', helperText: '사설 CA의 PEM 인증서를 입력합니다. Docker는 데몬 신뢰 저장소에도 동일한 CA가 필요합니다.' }, { key: 'healthChecks', label: 'Health Check JSON', type: 'multiline', defaultValue: '[]' }, { key: 'maxArchiveBytes', label: '최대 아티팩트 크기(byte)', type: 'number', required: true, defaultValue: 10737418240, helperText: '압축 파일 자체의 최대 크기입니다. 기본 10 GiB.' }, { key: 'maxExtractedBytes', label: '최대 압축 해제 크기(byte)', type: 'number', required: true, defaultValue: 21474836480, helperText: '압축 풀기 후 모든 파일의 합계 상한입니다. 기본 20 GiB.' }, { key: 'maxArchiveFiles', label: '최대 파일 수', type: 'number', required: true, defaultValue: 10000 }, { key: 'maxImages', label: '최대 컨테이너 이미지 수', type: 'number', required: true, defaultValue: 100 }, { key: 'maxLogBytes', label: '작업별 최대 로그 크기(byte)', type: 'number', required: true, defaultValue: 52428800, helperText: '단계별 로그 저장 상한입니다. 기본 50 MiB.' }, { key: 'allowSymlinks', label: '아티팩트 심볼릭 링크 허용', type: 'boolean', defaultValue: false, helperText: '보안을 위해 기본적으로 비활성화합니다.' }, { key: 'timeoutSeconds', label: '실행 제한 시간(초)', type: 'number', required: true, defaultValue: 600 }, { key: 'approvalRequired', label: '승인 필요', type: 'boolean', defaultValue: false }, { key: 'cleanupWorkspace', label: '성공 후 작업공간 정리', type: 'boolean', defaultValue: true }, { key: 'keepFailedWorkspace', label: '실패 작업공간 보존', type: 'boolean', defaultValue: false }, { key: 'active', label: '활성화', type: 'boolean', defaultValue: true }],
  },
  presets: {
    title: '배포 프리셋', description: '파일명 접두어를 서비스와 배포 정책에 연결해 일반 사용자의 원클릭 배포를 구성합니다.', singular: '배포 프리셋', endpoint: '/admin/deployment-presets',
    writePermission: 'admin.presets.write',
    columns: [{ key: 'name', label: '서비스' }, { key: 'artifactPrefix', label: '파일명 접두어' }, { key: 'applicationName', label: '애플리케이션' }, { key: 'environmentName', label: '배포 대상' }, { key: 'deploymentProfileName', label: '운영 정책' }, { key: 'autoDeployAfterApproval', label: '승인 후 자동 시작', status: true }, { key: 'active', label: '상태', status: true }],
    fields: [{ key: 'name', label: '사용자 표시 이름', required: true, helperText: '예: AI Portal 운영' }, { key: 'artifactPrefix', label: '파일명 접두어', required: true, pattern: /^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$/, helperText: '1~64자의 영문 소문자·숫자·내부 하이픈만 사용합니다. 예: ai-portal' }, { key: 'applicationId', label: '애플리케이션', required: true, relation: 'applications' }, { key: 'environmentId', label: '배포 대상', required: true, relation: 'environments' }, { key: 'deploymentProfileId', label: '운영 정책', required: true, relation: 'profiles', helperText: '선택한 서비스와 배포 대상에 맞는 정책만 표시됩니다.' }, { key: 'autoDeployAfterApproval', label: '승인 후 자동으로 배포 시작', type: 'boolean', defaultValue: true }, { key: 'active', label: '활성화', type: 'boolean', defaultValue: true }],
  },
  simpleTargets: {
    title: '심플 대상', description: '심플 모드에서 사용자가 고를 배포 대상입니다. 업로드 경로와 실행할 명령을 지정합니다.', singular: '심플 대상', endpoint: '/admin/simple-targets',
    writePermission: 'admin.simple.write',
    columns: [{ key: 'name', label: '이름' }, { key: 'uploadDir', label: '업로드 경로' }, { key: 'commandPath', label: '실행 명령' }, { key: 'timeoutSeconds', label: '제한 시간(초)' }, { key: 'active', label: '상태', status: true }],
    fields: [
      { key: 'name', label: '사용자 표시 이름', required: true, helperText: '예: AI 포털 운영' },
      { key: 'description', label: '설명', type: 'multiline' },
      { key: 'uploadDir', label: '업로드 경로', required: true, helperText: '패키지를 저장할 절대 경로입니다. 심플 설정의 업로드 루트 하위여야 합니다. 예: /var/lib/releasedock/simple/ai-portal' },
      { key: 'commandPath', label: '실행 명령 절대 경로', helperText: '공통 명령 모드에서는 사용되지 않습니다. 예: /opt/deploy/ai-portal.sh' },
      { key: 'commandArgs', label: '명령 인자', type: 'multiline', helperText: '한 줄에 인자 하나입니다. {{artifact}}는 업로드한 파일의 절대 경로로 바뀝니다. 셸을 거치지 않으므로 인자 안의 특수문자는 그대로 전달됩니다.' },
      { key: 'workingDir', label: '작업 디렉터리', helperText: '비우면 업로드 경로에서 실행합니다.' },
      { key: 'timeoutSeconds', label: '제한 시간(초)', type: 'number', defaultValue: 600, helperText: '1~86400. 초과하면 자식 프로세스까지 함께 종료합니다.' },
      { key: 'maxUploadBytes', label: '최대 업로드 크기(byte)', type: 'number', defaultValue: 10737418240, helperText: '기본 10 GiB.' },
      { key: 'active', label: '활성화', type: 'boolean', defaultValue: true },
    ],
  },
  scripts: {
    title: '스크립트 템플릿', description: 'Runner에서 허용 목록으로 실행할 버전 관리 스크립트를 관리합니다.', singular: '스크립트', endpoint: '/admin/scripts',
    writePermission: 'admin.scripts.write',
    columns: [{ key: 'name', label: '이름' }, { key: 'type', label: '종류' }, { key: 'version', label: '버전' }, { key: 'timeoutSeconds', label: '제한 시간(초)' }, { key: 'active', label: '상태', status: true }],
    fields: [{ key: 'name', label: '이름', required: true }, { key: 'type', label: '종류', type: 'select', required: true, options: [{ label: '사전 검사', value: 'PRE_CHECK' }, { label: '배포', value: 'DEPLOY' }, { label: '상태 확인', value: 'HEALTH_CHECK' }, { label: '롤백', value: 'ROLLBACK' }] }, { key: 'version', label: '버전', required: true, defaultValue: '1' }, { key: 'interpreterPath', label: 'Interpreter 절대 경로', required: true, defaultValue: '/bin/bash' }, { key: 'content', label: '스크립트 내용', type: 'multiline', required: true, helperText: '사용자 입력은 환경 변수 대신 분리된 인자로 전달됩니다.' }, { key: 'timeoutSeconds', label: '제한 시간(초)', type: 'number', defaultValue: 600 }, { key: 'active', label: '승인 및 활성화', type: 'boolean', defaultValue: true }],
  },
  registries: {
    title: 'Harbor Registry', description: '사내 Harbor endpoint와 암호화된 Robot Account 자격 증명을 관리합니다.', singular: 'Registry', endpoint: '/admin/registries',
    writePermission: 'admin.registries.write',
    columns: [{ key: 'name', label: '이름' }, { key: 'endpoint', label: 'Endpoint' }, { key: 'project', label: 'Project' }, { key: 'username', label: '계정' }, { key: 'active', label: '상태', status: true }],
    fields: [{ key: 'name', label: '이름', required: true }, { key: 'endpoint', label: 'Endpoint', required: true, helperText: '예: https://harbor.company.local' }, { key: 'project', label: 'Project', required: true }, { key: 'username', label: 'Robot Account', required: true }, { key: 'password', label: '비밀번호 / 토큰', type: 'password', requiredOnCreate: true, helperText: '저장 시 서버의 ENCRYPTION_KEY로 암호화됩니다. 수정할 때 비우면 기존 값을 유지합니다.' }, { key: 'insecureSkipVerify', label: 'TLS 인증서 검증 생략', type: 'boolean', defaultValue: false }, { key: 'active', label: '활성화', type: 'boolean', defaultValue: true }],
  },
  targetCredentials: {
    title: '배포 대상 Credential', description: 'SSH, Kubernetes, API 배포 스크립트에 필요한 키를 암호화하고 버전별로 회전·폐기합니다.', singular: 'Credential', endpoint: '/admin/target-credentials',
    writePermission: 'admin.credentials.write',
    columns: [{ key: 'name', label: '이름' }, { key: 'type', label: '종류' }, { key: 'version', label: '버전' }, { key: 'secretConfigured', label: 'Secret', status: true }, { key: 'active', label: '상태', status: true }],
    fields: [{ key: 'name', label: '이름', required: true }, { key: 'type', label: '종류', type: 'select', required: true, options: [{ label: 'SSH Private Key', value: 'SSH_PRIVATE_KEY' }, { label: 'Kubeconfig', value: 'KUBECONFIG' }, { label: 'Bearer / API Token', value: 'TOKEN' }, { label: '기타 Secret File', value: 'OPAQUE_FILE' }] }, { key: 'secret', label: 'Secret 내용', type: 'secret-multiline', requiredOnCreate: true, createOnly: true, helperText: '줄바꿈을 그대로 유지합니다. 최초 저장 후 다시 표시되지 않으며, 변경은 회전 기능을 사용합니다.' }, { key: 'active', label: '활성화', type: 'boolean', defaultValue: true }],
    allowSecretRotation: true,
  },
  runners: {
    title: 'Runner 관리', description: '실행 중인 Runner가 자동 등록됩니다. 새 작업 허용 여부와 레이블을 관리하며, 현재 버전은 Runner마다 한 번에 1개 작업만 처리합니다.', singular: 'Runner', endpoint: '/admin/runners',
    writePermission: 'admin.runners.write',
    columns: [{ key: 'name', label: '이름' }, { key: 'address', label: '주소' }, { key: 'status', label: '연결 상태', status: true }, { key: 'labels', label: '레이블' }, { key: 'lastHeartbeatAt', label: '최근 Heartbeat' }],
    fields: [{ key: 'name', label: '이름', required: true }, { key: 'address', label: '주소', required: true }, { key: 'labels', label: '레이블', helperText: '쉼표로 구분 (예: prod,docker)' }, { key: 'active', label: '새 작업 허용', type: 'boolean', defaultValue: true }],
    allowCreate: false,
  },
  users: {
    title: '사용자 관리', description: '로컬 및 OIDC 사용자 상태와 역할을 관리합니다.', singular: '사용자', endpoint: '/admin/users',
    writePermission: 'admin.users.write',
    columns: [{ key: 'username', label: '사용자 이름' }, { key: 'displayName', label: '표시 이름' }, { key: 'email', label: '이메일' }, { key: 'source', label: '인증 소스' }, { key: 'roles', label: '역할' }, { key: 'active', label: '상태', status: true }],
    fields: [{ key: 'username', label: '사용자 이름', requiredOnCreate: true, createOnly: true, helperText: '로컬 사용자 생성 시 필요하며 생성 후에는 변경되지 않습니다.' }, { key: 'password', label: '초기 비밀번호', type: 'password', requiredOnCreate: true, createOnly: true, helperText: '12자 이상 입력합니다. 이후 비밀번호 변경은 사용자 개인화 페이지에서 처리합니다.' }, { key: 'displayName', label: '표시 이름', required: true }, { key: 'email', label: '이메일' }, { key: 'roles', label: '역할 ID 또는 이름', required: true, helperText: '쉼표로 구분 (예: operator, viewer)' }, { key: 'active', label: '활성화', type: 'boolean', defaultValue: true }],
    allowDelete: false,
  },
  roles: {
    title: '역할 및 권한', description: '릴리즈, 배포, 승인, 키, 관리 API 권한을 역할별로 변경합니다.', singular: '역할', endpoint: '/admin/roles',
    writePermission: 'admin.rbac.write',
    columns: [{ key: 'name', label: '역할' }, { key: 'description', label: '설명' }, { key: 'permissions', label: '권한' }, { key: 'system', label: '시스템 역할', status: true }],
    fields: [{ key: 'name', label: '역할 이름', required: true }, { key: 'description', label: '설명', type: 'multiline' }, { key: 'permissions', label: '권한', required: true, helperText: '쉼표로 구분 (예: releases.read,releases.create,releases.submit)' }],
  },
} satisfies Record<string, ResourceConfig>;

function displayValue(row: ResourceRow, column: ColumnConfig) {
  const value = row[column.key];
  if (column.secret) return value ? '••••••••' : '—';
  if (column.status) {
    const status = typeof value === 'boolean' ? (value ? 'ACTIVE' : 'INACTIVE') : String(value ?? 'UNKNOWN');
    return <StatusChip status={status} />;
  }
  if (Array.isArray(value)) return value.join(', ');
  if (column.key.endsWith('At')) return formatDate(String(value || ''));
  if (value === null || value === undefined || value === '') return '—';
  return String(value);
}

function defaults(config: ResourceConfig): Record<string, unknown> {
  return Object.fromEntries(config.fields.map((field) => [field.key, field.defaultValue ?? (field.type === 'boolean' ? false : '')]));
}

type RelationData = Record<NonNullable<FieldConfig['relation']>, ResourceRow[]>;
type RelationAccess = Record<NonNullable<FieldConfig['relation']>, boolean>;

export async function loadRelations(config: ResourceConfig, access: RelationAccess): Promise<RelationData> {
  const needed = new Set(config.fields.map((field) => field.relation).filter(Boolean));
  if (config.endpoint === '/deployment-profiles') needed.add('targetCredentials');
  const load = async (relation: NonNullable<FieldConfig['relation']>, endpoint: string) => {
    if (!needed.has(relation) || !access[relation]) return [];
    const rows: ResourceRow[] = [];
    for (let page = 1; page <= 50; page += 1) {
      const response = await api.getResource<ResourceRow>(endpoint, { page, pageSize: 200 });
      const items = unwrapItems(response);
      rows.push(...items);
      if (Array.isArray(response) || items.length < 200 || rows.length >= response.total) break;
    }
    return rows;
  };
  const [applications, environments, profiles, registries, scripts, targetCredentials] = await Promise.all([
    load('applications', '/applications'),
    load('environments', '/environments'),
    load('profiles', '/deployment-profiles'),
    load('registries', '/admin/registries'),
    load('scripts', '/admin/scripts'),
    load('targetCredentials', '/admin/target-credentials'),
  ]);
  return { applications, environments, profiles, registries, scripts, targetCredentials };
}

export function buildResourcePayload(config: ResourceConfig, values: Record<string, unknown>, editing: boolean, canWriteTargetCredentialBinding: boolean) {
  const activeFields = config.fields.filter((field) => !editing || !field.createOnly);
  const payload = Object.fromEntries(activeFields.map((field) => [field.key, values[field.key]]));
  if (config.endpoint === '/deployment-profiles' && canWriteTargetCredentialBinding) payload.targetCredentialId = values.targetCredentialId ?? '';
  activeFields.forEach((field) => {
    if (field.type === 'number') payload[field.key] = Number(payload[field.key]);
    if ((field.key === 'roles' || field.key === 'permissions' || field.key === 'labels' || field.key === 'runnerLabels') && typeof payload[field.key] === 'string') {
      payload[field.key] = (payload[field.key] as string).split(',').map((item) => item.trim()).filter(Boolean);
    }
    if (field.type === 'password' && editing && !payload[field.key]) delete payload[field.key];
  });
  return payload;
}

function ResourceDialog({ config, row, open, onClose, onSaved }: { config: ResourceConfig; row?: ResourceRow; open: boolean; onClose: () => void; onSaved: (saved: ResourceRow) => Promise<void> }) {
  const { hasPermission } = useAuth();
  const relationAccess = useMemo<RelationAccess>(() => ({
    applications: hasPermission('applications.read'),
    environments: hasPermission('applications.read'),
    profiles: hasPermission('profiles.read'),
    registries: hasPermission('admin.registries.read'),
    scripts: hasPermission('admin.scripts.read'),
    targetCredentials: hasPermission('admin.credentials.read'),
  }), [hasPermission]);
  const canReadTargetCredentials = relationAccess.targetCredentials;
  const canWriteTargetCredentialBinding = canReadTargetCredentials && hasPermission('admin.credentials.write');
  const [values, setValues] = useState<Record<string, unknown>>(() => ({ ...defaults(config), ...row }));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string>();
  const [showSecret, setShowSecret] = useState(false);
  const relations = useAsync(() => loadRelations(config, relationAccess), [config.endpoint, relationAccess]);
  const setValue = (key: string, value: unknown) => setValues((current) => ({
    ...current,
    [key]: value,
    ...(key === 'applicationId' ? { environmentId: '', deploymentProfileId: '' } : {}),
    ...(key === 'environmentId' ? { deploymentProfileId: '' } : {}),
    ...(key === 'runtimeKind' ? { runtimeBinaryPath: { docker: '/usr/bin/docker', podman: '/usr/bin/podman', containerd: '/usr/bin/ctr' }[String(value)] ?? '' } : {}),
  }));
  const activeFields = config.fields.filter((field) => !row || !field.createOnly);
  const targetCredentialField: FieldConfig = { key: 'targetCredentialId', label: '배포 대상 Credential', relation: 'targetCredentials' };
  const valid = activeFields.every((field) => {
    const value = String(values[field.key] ?? '').trim();
    const required = field.required || (!row && field.requiredOnCreate);
    return (!required || field.type === 'boolean' || value) && (!field.pattern || !value || field.pattern.test(value));
  });

  const relationOptions = (field: FieldConfig): Array<{ label: string; value: string }> => {
    if (!field.relation) return field.options ?? [];
    let rows = relations.data?.[field.relation] ?? [];
    if (field.relation === 'environments' && values.applicationId) rows = rows.filter((item) => !item.applicationId || item.applicationId === values.applicationId);
    if (field.relation === 'profiles') rows = rows.filter((item) => (!values.applicationId || !item.applicationId || item.applicationId === values.applicationId) && (!values.environmentId || !item.environmentId || item.environmentId === values.environmentId));
    if (field.relation === 'scripts' && field.scriptType) rows = rows.filter((item) => item.type === field.scriptType && item.active !== false && item.approved !== false);
    const options = rows.map((item) => ({
      value: item.id,
      label: field.relation === 'applications'
        ? `${String(item.name || item.id)}${item.code ? ` (${String(item.code)})` : ''}`
        : field.relation === 'environments'
          ? `${String(item.name || item.id)}${item.kind || item.code ? ` (${String(item.kind || item.code)})` : ''}`
          : field.relation === 'scripts'
            ? `${String(item.name || item.id)} v${String(item.version || '1')}`
            : field.relation === 'targetCredentials'
              ? `${String(item.name || item.id)} (${String(item.type || 'SECRET')} v${String(item.version || '1')})`
              : `${String(item.name || item.id)}${item.project ? ` / ${String(item.project)}` : ''}`,
    }));
    const current = String(values[field.key] ?? '');
    if (current && !options.some((option) => option.value === current)) options.unshift({ value: current, label: `현재 선택 (${current})` });
    return options;
  };

  const save = async () => {
    setSaving(true);
    setError(undefined);
    const payload = buildResourcePayload(config, values, Boolean(row), canWriteTargetCredentialBinding);
    try {
      const saved = row
        ? await api.updateResource<ResourceRow>(config.endpoint, row.id, payload)
        : await api.createResource<ResourceRow>(config.endpoint, payload);
      await onSaved(saved);
      onClose();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '저장하지 못했습니다.');
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog open={open} onClose={() => !saving && onClose()} fullWidth maxWidth="md">
      <DialogTitle>{row ? `${config.singular} 수정` : `${config.singular} 등록`}</DialogTitle>
      <DialogContent>
        <Stack spacing={2.25} sx={{ pt: 1 }}>
          {activeFields.map((field) => field.type === 'boolean' ? (
            <FormControlLabel key={field.key} control={<Checkbox checked={Boolean(values[field.key])} onChange={(event) => setValue(field.key, event.target.checked)} />} label={field.label} />
          ) : (
            <TextField
              key={field.key}
              label={field.label}
              required={field.required || (!row && field.requiredOnCreate)}
              type={field.type === 'number' || field.type === 'password' ? field.type : 'text'}
              multiline={field.type === 'multiline' || field.type === 'secret-multiline'}
              minRows={field.type === 'multiline' || field.type === 'secret-multiline' ? (field.key === 'content' ? 10 : field.type === 'secret-multiline' ? 7 : 3) : undefined}
              select={field.type === 'select' || Boolean(field.relation)}
              value={field.type === 'multiline' && values[field.key] && typeof values[field.key] === 'object' ? JSON.stringify(values[field.key], null, 2) : Array.isArray(values[field.key]) ? (values[field.key] as unknown[]).join(', ') : values[field.key] ?? ''}
              onChange={(event) => setValue(field.key, event.target.value)}
              helperText={field.relation && relations.error ? `선택 목록을 불러오지 못했습니다: ${relations.error.message}` : field.helperText}
              error={Boolean(field.pattern && String(values[field.key] ?? '') && !field.pattern.test(String(values[field.key])))}
              disabled={Boolean(field.relation && (relations.loading || (field.relation === 'environments' && !values.applicationId) || (field.relation === 'profiles' && (!values.applicationId || !values.environmentId))))}
              inputProps={{ ...(field.type === 'number' ? { min: 0 } : {}), ...(field.pattern ? { pattern: field.pattern.source } : {}), ...(field.key === 'artifactPrefix' ? { maxLength: 64 } : {}) }}
              slotProps={field.type === 'secret-multiline' ? { input: { endAdornment: <InputAdornment position="end"><IconButton aria-label={showSecret ? 'Secret 숨기기' : 'Secret 보기'} onClick={() => setShowSecret((current) => !current)} edge="end">{showSecret ? <VisibilityOffRoundedIcon /> : <VisibilityRoundedIcon />}</IconButton></InputAdornment> } } : undefined}
              sx={field.type === 'secret-multiline' && !showSecret ? { '& textarea': { color: 'transparent', caretColor: 'text.primary' } } : undefined}
              fullWidth
            >
              {field.relation && !field.required && <MenuItem value="">선택 안 함</MenuItem>}
              {relationOptions(field).map((option) => <MenuItem key={option.value} value={option.value}>{option.label}</MenuItem>)}
            </TextField>
          ))}
          {config.endpoint === '/deployment-profiles' && canReadTargetCredentials && (
            <TextField
              label={targetCredentialField.label}
              select
              value={values.targetCredentialId ?? ''}
              onChange={(event) => setValue('targetCredentialId', event.target.value)}
              helperText={relations.error ? `선택 목록을 불러오지 못했습니다: ${relations.error.message}` : canWriteTargetCredentialBinding ? '선택한 Secret은 스크립트 실행 직전에만 일시 파일로 전달되고 즉시 삭제됩니다.' : 'Credential 변경에는 admin.credentials.write 권한이 필요합니다.'}
              disabled={relations.loading || !canWriteTargetCredentialBinding}
              fullWidth
            >
              <MenuItem value="">선택 안 함</MenuItem>
              {relationOptions(targetCredentialField).map((option) => <MenuItem key={option.value} value={option.value}>{option.label}</MenuItem>)}
            </TextField>
          )}
          {error && <Alert severity="error">{error}</Alert>}
        </Stack>
      </DialogContent>
      <DialogActions sx={{ p: 2.5 }}>
        <Button onClick={onClose} disabled={saving}>취소</Button>
        <Button variant="contained" disabled={saving || !valid} onClick={() => void save()} startIcon={saving ? <CircularProgress size={18} /> : undefined}>{saving ? '저장 중…' : '저장'}</Button>
      </DialogActions>
    </Dialog>
  );
}

export function AdminResourcePage({ config }: { config: ResourceConfig }) {
  const { hasPermission } = useAuth();
  const canWrite = hasPermission(config.writePermission);
  const [params, setParams] = useSearchParams();
  const parsedPage = Number(params.get('page') || 1);
  const parsedPageSize = Number(params.get('pageSize') || 25);
  const page = Number.isInteger(parsedPage) && parsedPage > 0 ? parsedPage : 1;
  const pageSize = [25, 50, 100].includes(parsedPageSize) ? parsedPageSize : 25;
  const search = params.get('search') || '';
  const state = useAsync(() => api.getResource<ResourceRow>(config.endpoint, { page, pageSize, search }), [config.endpoint, page, pageSize, search]);
  const [editing, setEditing] = useState<ResourceRow | null>();
  const [deleting, setDeleting] = useState<ResourceRow>();
  const [rotating, setRotating] = useState<ResourceRow>();
  const [rotationSecret, setRotationSecret] = useState('');
  const [rotationError, setRotationError] = useState<string>();
  const [rotationBusy, setRotationBusy] = useState(false);
  const [showRotationSecret, setShowRotationSecret] = useState(false);
  const [deleteError, setDeleteError] = useState<string>();
  const [deleteBusy, setDeleteBusy] = useState(false);
  const rows = useMemo(() => unwrapItems(state.data), [state.data]);
  const total = Array.isArray(state.data) ? rows.length : state.data?.total ?? rows.length;
  const revokeInsteadOfDelete = config.endpoint === '/admin/target-credentials';
  const deleteVerb = revokeInsteadOfDelete ? '폐기' : '삭제';
  const updateQuery = (key: 'page' | 'pageSize' | 'search', value: string) => {
    const next = new URLSearchParams(params);
    if (value && !(key === 'page' && value === '1') && !(key === 'pageSize' && value === '25')) next.set(key, value);
    else next.delete(key);
    if (key !== 'page') next.delete('page');
    setParams(next, { replace: true });
  };

  const remove = async () => {
    if (!deleting) return;
    setDeleteBusy(true);
    setDeleteError(undefined);
    try {
      await api.deleteResource(config.endpoint, deleting.id);
      setDeleting(undefined);
      if (rows.length === 1 && page > 1) updateQuery('page', String(page - 1));
      else await state.reload();
    } catch (reason) {
      setDeleteError(reason instanceof Error ? reason.message : `${deleteVerb}하지 못했습니다.`);
    } finally {
      setDeleteBusy(false);
    }
  };

  const rotateSecret = async () => {
    if (!rotating || !rotationSecret) return;
    setRotationBusy(true);
    setRotationError(undefined);
    try {
      await api.rotateResourceSecret(config.endpoint, rotating.id, rotationSecret);
      setRotating(undefined);
      setRotationSecret('');
      await state.reload();
    } catch (reason) {
      setRotationError(reason instanceof Error ? reason.message : 'Credential을 회전하지 못했습니다.');
    } finally {
      setRotationBusy(false);
    }
  };

  return (
    <>
      <PageHeader title={config.title} description={config.description} action={!canWrite || config.allowCreate === false ? undefined : <Button variant="contained" startIcon={<AddRoundedIcon />} onClick={() => setEditing(null)}>새 {config.singular}</Button>} />
      <Card>
        <Box sx={{ p: 2 }}>
          <TextField label="검색" value={search} onChange={(event) => updateQuery('search', event.target.value)} sx={{ width: { xs: '100%', sm: 360 } }} slotProps={{ input: { startAdornment: <InputAdornment position="start"><SearchRoundedIcon /></InputAdornment> } }} />
        </Box>
        {state.loading && <Box sx={{ p: 3 }}><PageLoading /></Box>}
        {state.error && !state.loading && <Box sx={{ p: 2 }}><PageError error={state.error} onRetry={() => void state.reload()} /></Box>}
        {!state.loading && !state.error && (rows.length ? (
          <>
            <TableContainer>
              <Table aria-label={config.title}>
                <TableHead><TableRow>{config.columns.map((column) => <TableCell key={column.key}>{column.label}</TableCell>)}{canWrite && <TableCell align="right">작업</TableCell>}</TableRow></TableHead>
                <TableBody>{rows.map((row) => (
                  <TableRow key={row.id} hover>
                    {config.columns.map((column, index) => <TableCell key={column.key}>{index === 0 ? <Typography fontWeight={700}>{displayValue(row, column)}</Typography> : displayValue(row, column)}</TableCell>)}
                    {canWrite && <TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>
                      <Tooltip title="수정"><IconButton aria-label={`${config.singular} 수정`} onClick={() => setEditing(row)}><EditRoundedIcon fontSize="small" /></IconButton></Tooltip>
                      {config.allowSecretRotation && row.active !== false && <Tooltip title="Secret 회전"><IconButton aria-label={`${config.singular} Secret 회전`} onClick={() => { setRotationSecret(''); setRotationError(undefined); setShowRotationSecret(false); setRotating(row); }}><ReplayRoundedIcon fontSize="small" /></IconButton></Tooltip>}
                      {config.allowDelete !== false && config.endpoint !== '/admin/runners' && (!revokeInsteadOfDelete || row.active !== false) && <Tooltip title={deleteVerb}><IconButton aria-label={`${config.singular} ${deleteVerb}`} color="error" onClick={() => setDeleting(row)}><DeleteOutlineRoundedIcon fontSize="small" /></IconButton></Tooltip>}
                    </TableCell>}
                  </TableRow>
                ))}</TableBody>
              </Table>
            </TableContainer>
            <TablePagination
              component="div"
              count={total}
              page={Math.min(page - 1, Math.max(0, Math.ceil(total / pageSize) - 1))}
              onPageChange={(_, value) => updateQuery('page', String(value + 1))}
              rowsPerPage={pageSize}
              onRowsPerPageChange={(event) => updateQuery('pageSize', event.target.value)}
              rowsPerPageOptions={[25, 50, 100]}
              labelRowsPerPage="페이지당 항목"
              labelDisplayedRows={({ from, to, count }) => `${from}–${to} / ${count}`}
            />
          </>
        ) : <EmptyState filtered={Boolean(search)} title={search ? '검색 결과가 없습니다' : `등록된 ${config.singular}이(가) 없습니다`} />)}
      </Card>

      {canWrite && editing !== undefined && <ResourceDialog key={editing?.id || 'new'} config={config} row={editing || undefined} open onClose={() => setEditing(undefined)} onSaved={async () => { await state.reload(); }} />}
      <Dialog open={Boolean(deleting)} onClose={() => !deleteBusy && setDeleting(undefined)}>
        <DialogTitle>{config.singular} {deleteVerb}</DialogTitle>
        <DialogContent><Typography>{revokeInsteadOfDelete ? '이 Credential을 폐기하면 새 작업에서 사용할 수 없습니다. 이미 고정된 작업도 실행 전에 안전하게 거부됩니다.' : '이 항목을 삭제하시겠습니까? 사용 중인 항목은 서버 정책에 따라 삭제가 거부될 수 있습니다.'}</Typography>{deleteError && <Alert severity="error" sx={{ mt: 2 }}>{deleteError}</Alert>}</DialogContent>
        <DialogActions sx={{ p: 2.5 }}><Button onClick={() => setDeleting(undefined)} disabled={deleteBusy}>취소</Button><Button color="error" variant="contained" disabled={deleteBusy} onClick={() => void remove()}>{deleteBusy ? `${deleteVerb} 중…` : deleteVerb}</Button></DialogActions>
      </Dialog>
      <Dialog open={Boolean(rotating)} onClose={() => !rotationBusy && setRotating(undefined)} fullWidth maxWidth="sm">
        <DialogTitle>Credential Secret 회전</DialogTitle>
        <DialogContent>
          <Stack spacing={2} sx={{ pt: 1 }}>
            <Alert severity="warning">기존 Secret은 즉시 폐기되며, 이 Credential을 사용하는 새 작업은 증가한 버전을 고정합니다.</Alert>
            <TextField label="새 Secret 내용" value={rotationSecret} onChange={(event) => setRotationSecret(event.target.value)} required multiline minRows={7} autoFocus fullWidth slotProps={{ input: { endAdornment: <InputAdornment position="end"><IconButton aria-label={showRotationSecret ? 'Secret 숨기기' : 'Secret 보기'} onClick={() => setShowRotationSecret((current) => !current)} edge="end">{showRotationSecret ? <VisibilityOffRoundedIcon /> : <VisibilityRoundedIcon />}</IconButton></InputAdornment> } }} sx={!showRotationSecret ? { '& textarea': { color: 'transparent', caretColor: 'text.primary' } } : undefined} />
            {rotationError && <Alert severity="error">{rotationError}</Alert>}
          </Stack>
        </DialogContent>
        <DialogActions sx={{ p: 2.5 }}><Button onClick={() => setRotating(undefined)} disabled={rotationBusy}>취소</Button><Button variant="contained" disabled={rotationBusy || !rotationSecret} onClick={() => void rotateSecret()}>{rotationBusy ? '회전 중…' : 'Secret 회전'}</Button></DialogActions>
      </Dialog>
    </>
  );
}
