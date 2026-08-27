import { Chip } from '@mui/material';
import type { ChipProps } from '@mui/material/Chip';

const labels: Record<string, string> = {
  DRAFT: '임시 저장',
  UPLOADED: '업로드됨',
  VALIDATING: '검증 중',
  PRE_CHECK: '사전 검사',
  READY: '준비됨',
  PENDING_REVIEW: '검토 대기',
  UNDER_REVIEW: '검토 중',
  APPROVED: '승인됨',
  REJECTED: '반려됨',
  EXTRACTING: '압축 해제',
  IMAGE_IMPORT: '이미지 가져오기',
  IMAGE_INSPECT: '이미지 검사',
  IMAGE_LOAD: '이미지 로드',
  IMAGE_TAG: '이미지 태깅',
  IMAGE_PUSH: 'Harbor 전송',
  DEPLOYING: '배포 중',
  VERIFYING: '상태 확인',
  SUCCESS: '성공',
  FAILED: '실패',
  ROLLBACK: '롤백 중',
  ROLLED_BACK: '롤백 완료',
  QUEUED: '실행 대기',
  PENDING: '대기',
  RUNNING: '실행 중',
  SKIPPED: '건너뜀',
  ACTIVE: '활성',
  INACTIVE: '비활성',
  FAILURE: '실패',
};

export type SimpleReleaseStatus = 'CHECKING' | 'PREPARING' | 'APPROVAL' | 'DEPLOYING' | 'SUCCESS' | 'FAILED' | 'REJECTED' | 'UNKNOWN';

const simpleLabels: Record<SimpleReleaseStatus, string> = {
  CHECKING: '패키지 확인 중',
  PREPARING: '배포 준비 중',
  APPROVAL: '승인 진행 중',
  DEPLOYING: '배포 중',
  SUCCESS: '배포 완료',
  FAILED: '배포 실패',
  REJECTED: '요청 반려',
  UNKNOWN: '상태 확인 중',
};

export function toSimpleReleaseStatus(status?: string): SimpleReleaseStatus {
  const normalized = (status || '').toUpperCase();
  if (['DRAFT', 'UPLOADED', 'VALIDATING', 'PRE_CHECK', 'EXTRACTING'].includes(normalized)) return 'CHECKING';
  if (['READY', 'QUEUED', 'IMAGE_IMPORT', 'IMAGE_INSPECT', 'IMAGE_LOAD', 'IMAGE_TAG', 'IMAGE_PUSH'].includes(normalized)) return 'PREPARING';
  if (['PENDING_REVIEW', 'UNDER_REVIEW', 'APPROVED'].includes(normalized)) return 'APPROVAL';
  if (['DEPLOYING', 'VERIFYING', 'ROLLBACK'].includes(normalized)) return 'DEPLOYING';
  if (['SUCCESS', 'ROLLED_BACK'].includes(normalized)) return 'SUCCESS';
  if (normalized === 'REJECTED') return 'REJECTED';
  if (['FAILED', 'FAILURE'].includes(normalized)) return 'FAILED';
  return 'UNKNOWN';
}

function statusColor(status: string): ChipProps['color'] {
  if (['SUCCESS', 'APPROVED', 'READY', 'ACTIVE'].includes(status)) return 'success';
  if (['FAILED', 'FAILURE', 'REJECTED'].includes(status)) return 'error';
  if (['RUNNING', 'UNDER_REVIEW', 'DEPLOYING', 'VERIFYING', 'VALIDATING', 'PRE_CHECK', 'EXTRACTING', 'IMAGE_IMPORT', 'IMAGE_INSPECT', 'IMAGE_LOAD', 'IMAGE_TAG', 'IMAGE_PUSH', 'CHECKING', 'PREPARING'].includes(status)) return 'info';
  if (['PENDING', 'QUEUED', 'PENDING_REVIEW', 'ROLLBACK', 'APPROVAL'].includes(status)) return 'warning';
  return 'default';
}

export function StatusChip({ status, size = 'small' }: { status?: string; size?: 'small' | 'medium' }) {
  const normalized = (status || 'UNKNOWN').toUpperCase();
  return <Chip label={labels[normalized] ?? status ?? '알 수 없음'} color={statusColor(normalized)} size={size} variant="outlined" />;
}

export function SimpleReleaseStatusChip({ status, size = 'small' }: { status?: string; size?: 'small' | 'medium' }) {
  const simplified = toSimpleReleaseStatus(status);
  return <Chip label={simpleLabels[simplified]} color={statusColor(simplified)} size={size} variant="outlined" />;
}
