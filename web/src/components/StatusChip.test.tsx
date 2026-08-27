import { render, screen } from '@testing-library/react';
import { ThemeProvider } from '@mui/material';
import { SimpleReleaseStatusChip, StatusChip, toSimpleReleaseStatus } from './StatusChip';
import { theme } from '../theme';

describe('StatusChip', () => {
  it('renders a visible Korean release status label', () => {
    render(<ThemeProvider theme={theme}><StatusChip status="PENDING_REVIEW" /></ThemeProvider>);
    expect(screen.getByText('검토 대기')).toBeVisible();
  });
});

describe('simple release status', () => {
  it.each([
    ['VALIDATING', 'CHECKING'],
    ['IMAGE_PUSH', 'PREPARING'],
    ['PENDING_REVIEW', 'APPROVAL'],
    ['VERIFYING', 'DEPLOYING'],
    ['ROLLED_BACK', 'SUCCESS'],
    ['REJECTED', 'REJECTED'],
    ['FUTURE_STATUS', 'UNKNOWN'],
  ])('maps %s to %s', (status, expected) => {
    expect(toSimpleReleaseStatus(status)).toBe(expected);
  });

  it('shows a plain-language status label', () => {
    render(<ThemeProvider theme={theme}><SimpleReleaseStatusChip status="IMAGE_PUSH" /></ThemeProvider>);
    expect(screen.getByText('배포 준비 중')).toBeVisible();
  });

  it('does not present a rejected request as an execution failure', () => {
    render(<ThemeProvider theme={theme}><SimpleReleaseStatusChip status="REJECTED" /></ThemeProvider>);
    expect(screen.getByText('요청 반려')).toBeVisible();
    expect(screen.queryByText('배포 실패')).not.toBeInTheDocument();
  });
});
