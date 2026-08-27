import { ThemeProvider } from '@mui/material';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { api, ApiError } from '../../api/client';
import { App } from '../../app/App';
import { theme } from '../../theme';
import type { QuickReleasePreflight, Release } from '../../types/domain';

const preflight: QuickReleasePreflight = {
  filename: 'ai-portal-v2.4.1.tar.gz',
  artifactPrefix: 'ai-portal',
  version: '2.4.1',
  currentVersion: '2.4.0',
  approvalRequired: true,
  nextAction: 'APPROVAL',
  preset: { id: 'preset-1', name: 'AI Portal production', autoDeployAfterApproval: true, updatedAt: '2026-08-27T00:00:00Z' },
  application: { id: 'app-1', code: 'ai-portal', name: 'AI Portal' },
  environment: { id: 'environment-1', code: 'prd', name: '운영', kind: 'PRD' },
  deploymentProfile: { id: 'profile-1', name: 'AI Portal production' },
  readiness: { profileReady: true, runnerAvailable: true },
};

function renderPage() {
  vi.spyOn(api, 'version').mockResolvedValue({ version: '0.2.0' });
  vi.spyOn(api, 'me').mockResolvedValue({
    id: 'user-1',
    username: 'deployer',
    displayName: '배포 담당자',
    roles: ['developer'],
    permissions: ['releases.read', 'releases.create', 'releases.submit'],
  });
  window.history.replaceState({}, '', '/releases/new');
  return render(<ThemeProvider theme={theme}><App /></ThemeProvider>);
}

describe('quick release page', () => {
  it('resolves a package and requests deployment with one primary action', async () => {
    const preflightSpy = vi.spyOn(api, 'quickReleasePreflight').mockResolvedValue(preflight);
    const release: Release = {
      id: 'release-1', version: '2.4.1', status: 'PENDING_REVIEW', applicationId: 'app-1', applicationName: 'AI Portal',
      environmentId: 'environment-1', environmentName: '운영', createdAt: '2026-08-27T00:00:00Z',
    };
    const createSpy = vi.spyOn(api, 'createQuickRelease').mockResolvedValue(release);
    vi.spyOn(api, 'release').mockResolvedValue(release);
    renderPage();

    expect(await screen.findByRole('heading', { name: '새 버전 배포' })).toBeVisible();
    const artifact = new File(['package'], 'ai-portal-v2.4.1.tar.gz', { type: 'application/gzip' });
    await userEvent.upload(screen.getByLabelText('릴리즈 패키지 선택'), artifact);

    await waitFor(() => expect(preflightSpy).toHaveBeenCalledWith(artifact.name));
    expect(await screen.findByText('AI Portal')).toBeVisible();
    expect(screen.getByText('v2.4.0')).toBeVisible();
    expect(screen.getByText('v2.4.1')).toBeVisible();
    expect(screen.queryByText('배포 프로필')).not.toBeInTheDocument();
    expect(screen.queryByText(preflight.preset.name)).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: '배포 요청' }));
    await waitFor(() => expect(createSpy).toHaveBeenCalledWith({
      artifact,
      expectedPresetId: 'preset-1',
      expectedPresetUpdatedAt: '2026-08-27T00:00:00Z',
      expectedCurrentVersion: '2.4.0',
      notes: undefined,
    }));
  });

  it('rejects an invalid filename locally without calling the server', async () => {
    const preflightSpy = vi.spyOn(api, 'quickReleasePreflight');
    renderPage();
    expect(await screen.findByRole('heading', { name: '새 버전 배포' })).toBeVisible();

    await userEvent.upload(screen.getByLabelText('릴리즈 패키지 선택'), new File(['package'], 'AI_Portal-v01.2.3.tar.gz', { type: 'application/gzip' }));

    expect(await screen.findByText(/파일명을 확인하세요/)).toBeVisible();
    expect(preflightSpy).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: '배포 요청' })).toBeDisabled();
  });

  it('explains a duplicate version before uploading the package', async () => {
    vi.spyOn(api, 'quickReleasePreflight').mockRejectedValue(new ApiError('version exists', 409, 'release_version_exists'));
    const createSpy = vi.spyOn(api, 'createQuickRelease');
    renderPage();

    await userEvent.upload(
      await screen.findByLabelText('릴리즈 패키지 선택'),
      new File(['package'], 'ai-portal-v2.4.1.tar.gz', { type: 'application/gzip' }),
    );

    expect(await screen.findByText(/동일한 버전이 이미 등록되어 있습니다/)).toBeVisible();
    expect(createSpy).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: '배포 요청' })).toBeDisabled();
  });

  it('refreshes the comparison instead of submitting stale settings', async () => {
    const refreshed = {
      ...preflight,
      currentVersion: '2.4.0-hotfix1',
      preset: { ...preflight.preset, updatedAt: '2026-08-27T00:01:00Z' },
    };
    const preflightSpy = vi.spyOn(api, 'quickReleasePreflight')
      .mockResolvedValueOnce(preflight)
      .mockResolvedValueOnce(refreshed);
    vi.spyOn(api, 'createQuickRelease').mockRejectedValue(new ApiError('deployment preset changed', 409, 'deployment_preset_unavailable'));
    renderPage();

    const artifact = new File(['package'], 'ai-portal-v2.4.1.tar.gz', { type: 'application/gzip' });
    await userEvent.upload(await screen.findByLabelText('릴리즈 패키지 선택'), artifact);
    await screen.findByText('v2.4.0');
    await userEvent.click(screen.getByRole('button', { name: '배포 요청' }));

    expect(await screen.findByText(/최신 정보를 다시 확인했습니다/)).toBeVisible();
    await waitFor(() => expect(preflightSpy).toHaveBeenCalledTimes(2));
    expect(await screen.findByText('v2.4.0-hotfix1')).toBeVisible();
  });

  it('stops a duplicate version retry loop', async () => {
    const preflightSpy = vi.spyOn(api, 'quickReleasePreflight').mockResolvedValue(preflight);
    vi.spyOn(api, 'createQuickRelease').mockRejectedValue(new ApiError('version already exists', 409, 'release_conflict'));
    renderPage();

    const artifact = new File(['package'], 'ai-portal-v2.4.1.tar.gz', { type: 'application/gzip' });
    await userEvent.upload(await screen.findByLabelText('릴리즈 패키지 선택'), artifact);
    await screen.findByText('v2.4.0');
    await userEvent.click(screen.getByRole('button', { name: '배포 요청' }));

    expect(await screen.findByText(/동일한 버전이 이미 등록되어 있습니다/)).toBeVisible();
    expect(preflightSpy).toHaveBeenCalledTimes(1);
    expect(screen.getByRole('button', { name: '배포 요청' })).toBeDisabled();
  });

  it('keeps a valid package ready when another deployment is temporarily active', async () => {
    vi.spyOn(api, 'quickReleasePreflight').mockResolvedValue(preflight);
    const createSpy = vi.spyOn(api, 'createQuickRelease').mockRejectedValue(new ApiError('target busy', 409, 'job_conflict'));
    renderPage();

    const artifact = new File(['package'], 'ai-portal-v2.4.1.tar.gz', { type: 'application/gzip' });
    await userEvent.upload(await screen.findByLabelText('릴리즈 패키지 선택'), artifact);
    await screen.findByText('v2.4.0');
    await userEvent.click(screen.getByRole('button', { name: '배포 요청' }));

    expect(await screen.findByText(/다른 배포가 진행 중입니다/)).toBeVisible();
    expect(screen.getByRole('button', { name: '배포 요청' })).toBeEnabled();
    expect(createSpy).toHaveBeenCalledTimes(1);
  });
});
