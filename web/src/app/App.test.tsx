import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ThemeProvider } from '@mui/material';
import { App } from './App';
import { theme } from '../theme';

function json(value: unknown, status = 200) {
  return Promise.resolve(new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } }));
}

function renderApp() {
  return render(<ThemeProvider theme={theme}><App /></ThemeProvider>);
}

describe('ReleaseDock application', () => {
  it('shows the product version on the login screen', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input);
      if (url.endsWith('/version')) return json({ data: { version: '9.8.7' } });
      if (url.endsWith('/me')) return json({ error: { code: 'UNAUTHORIZED', message: '로그인이 필요합니다.' } }, 401);
      return json({ error: { message: 'not found' } }, 404);
    });
    window.history.replaceState({}, '', '/login');
    renderApp();
    expect(await screen.findByRole('heading', { name: '로그인' })).toBeVisible();
    expect(await screen.findByText('ReleaseDock v9.8.7')).toBeVisible();
  });

  it('restores a deep-linked menu route after refresh', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input);
      if (url.endsWith('/version')) return json({ data: { version: '1.0.0' } });
      if (url.endsWith('/me')) return json({ data: { id: 'u1', username: 'admin', displayName: '관리자', roles: ['admin'], permissions: ['releases.read'] } });
      if (url.includes('/releases?')) return json({ data: { items: [], total: 0, page: 1, pageSize: 20 } });
      return json({ error: { message: 'not found' } }, 404);
    });
    window.history.replaceState({}, '', '/releases');
    renderApp();
    expect(await screen.findByRole('heading', { name: '릴리즈' })).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: '주 메뉴 열기' }));
    await waitFor(() => expect(screen.getByRole('link', { name: '릴리즈' })).toHaveAttribute('aria-current', 'page'));
    expect(screen.queryByRole('link', { name: '새 릴리즈 등록' })).not.toBeInTheDocument();
    expect(screen.queryByText('서비스 관리')).not.toBeInTheDocument();
  });
});
