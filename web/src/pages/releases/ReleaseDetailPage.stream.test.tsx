import { ThemeProvider } from '@mui/material';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { api } from '../../api/client';
import { App } from '../../app/App';
import { theme } from '../../theme';
import type { Release } from '../../types/domain';

// Only the browser transport is replaced; exercise property callbacks too.
class TestEventSource extends EventTarget {
  static instances: TestEventSource[] = [];
  closed = false;
  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  readonly withCredentials: boolean;
  constructor(readonly url: string, init?: EventSourceInit) {
    super();
    this.withCredentials = init?.withCredentials ?? false;
    TestEventSource.instances.push(this);
  }
  close() { this.closed = true; }
  emit(type: string, data = '', lastEventId = '') {
    const event = new MessageEvent(type, { data, lastEventId });
    if (type === 'open') this.onopen?.(event);
    if (type === 'message') this.onmessage?.(event);
    if (type === 'error') this.onerror?.(event);
    this.dispatchEvent(event);
  }
}

const release: Release = {
  id: 'release-1', version: '2.4.1', status: 'DEPLOYING', applicationId: 'app-1', applicationName: 'Portal',
  environmentId: 'env-1', environmentName: '운영', createdAt: '2026-09-29T00:00:00Z',
};
const line = (id: number) => ({ id, stream: 'stdout', message: `server line ${id}`, createdAt: release.createdAt });
const emit = async (source: TestEventSource, type: string, data = '', id = '') => {
  await act(async () => source.emit(type, data, id));
};
const timeout = (source: TestEventSource) => emit(source, 'end', '{"reason":"max_duration"}');

async function renderPage() {
  vi.spyOn(api, 'version').mockResolvedValue({ version: '0.5.24' });
  vi.spyOn(api, 'me').mockResolvedValue({
    id: 'user-1', username: 'deployer', displayName: '배포 담당자', roles: ['developer'], permissions: ['releases.read'],
  });
  vi.spyOn(api, 'release').mockImplementation(async (id) => ({ ...release, id }));
  window.history.replaceState({}, '', '/releases/release-1');
  const view = render(<ThemeProvider theme={theme}><App /></ThemeProvider>);
  fireEvent.click(await screen.findByRole('tab', { name: '실시간 로그' }));
  await waitFor(() => expect(TestEventSource.instances).toHaveLength(1));
  return { view, source: TestEventSource.instances[0] };
}

async function reconnected(count: number, cursor: number, releaseId = 'release-1') {
  await waitFor(() => expect(TestEventSource.instances).toHaveLength(count));
  const source = TestEventSource.instances[count - 1];
  expect(source.url).toBe(`/api/v1/releases/${releaseId}/logs/stream?after=${cursor}`);
  expect(source.withCredentials).toBe(true);
  expect(TestEventSource.instances.filter((item) => !item.closed)).toEqual([source]);
  return source;
}

describe('release live logs through App', () => {
  beforeEach(() => {
    TestEventSource.instances = [];
    vi.stubGlobal('EventSource', TestEventSource);
  });
  afterEach(() => vi.unstubAllGlobals());

  it('resumes timeout streams with server IDs, retaining lines and one connection across renders', async () => {
    const { view, source } = await renderPage();
    await emit(source, 'open');
    await emit(source, 'log', JSON.stringify(line(41)), '41');
    await emit(source, 'log', JSON.stringify(line(57)), '57');
    view.rerender(<ThemeProvider theme={theme}><App /></ThemeProvider>);
    expect(TestEventSource.instances).toHaveLength(1);
    await timeout(source);
    const next = await reconnected(2, 57);
    expect(source.closed).toBe(true);
    await emit(next, 'log', JSON.stringify(line(58)), '58');
    for (const id of [41, 57, 58]) expect(screen.getByText(`server line ${id}`)).toBeVisible();
    await timeout(next);
    await reconnected(3, 58);
  });

  it.each(['{}', 'not json', '{"reason":"unknown"}', 'null'])('closes without reconnecting on end %s', async (frame) => {
    const { source } = await renderPage();
    await emit(source, 'end', frame);
    expect(source.closed).toBe(true);
    expect(TestEventSource.instances).toHaveLength(1);
  });

  it('resumes empty streams at zero and never manually reconnects on error', async () => {
    const { source } = await renderPage();
    await emit(source, 'error');
    expect(TestEventSource.instances).toHaveLength(1);
    await timeout(source);
    const next = await reconnected(2, 0);
    await timeout(next);
    await reconnected(3, 0);
  });

  it('reads raw/enveloped payload IDs and message events, preserves plain text and never retreats', async () => {
    const { source } = await renderPage();
    await emit(source, 'message', JSON.stringify(line(41)));
    await emit(source, 'log', JSON.stringify({ data: line(57) }));
    await emit(source, 'log', 'plain text', 'invalid');
    await emit(source, 'log', JSON.stringify(line(2)), '-1');
    await emit(source, 'log', JSON.stringify({ id: 'bad', message: 'invalid ID' }));
    for (const text of ['server line 41', 'server line 57', 'plain text', 'invalid ID']) expect(screen.getByText(text)).toBeVisible();
    await timeout(source);
    await reconnected(2, 57);
  });

  it('uses Last-Event-ID even for plain text and ignores invalid or older cursors', async () => {
    const { source } = await renderPage();
    await emit(source, 'message', 'header-only line', '57');
    for (const id of ['41', '-1', 'NaN', '58.5', '9007199254740992']) {
      await emit(source, 'log', 'another line', id);
    }
    expect(screen.getByText('header-only line')).toBeVisible();
    await timeout(source);
    await reconnected(2, 57);
  });

  it('clears only displayed lines and closes on tab exit and unmount; reentry starts from zero', async () => {
    const { view, source } = await renderPage();
    await emit(source, 'log', JSON.stringify(line(57)));
    fireEvent.click(screen.getByRole('button', { name: '지우기' }));
    expect(screen.queryByText('server line 57')).not.toBeInTheDocument();
    await timeout(source);
    const next = await reconnected(2, 57);
    fireEvent.click(screen.getByRole('tab', { name: '실행 단계' }));
    expect(next.closed).toBe(true);
    await timeout(next);
    expect(TestEventSource.instances).toHaveLength(2);
    fireEvent.click(screen.getByRole('tab', { name: '실시간 로그' }));
    const fresh = await reconnected(3, 0);
    view.unmount();
    expect(fresh.closed).toBe(true);
    await timeout(fresh);
    expect(TestEventSource.instances).toHaveLength(3);
  });

  it('does not carry the cursor, displayed lines or connected state to another release route', async () => {
    const { source } = await renderPage();
    await emit(source, 'open');
    await emit(source, 'log', JSON.stringify(line(57)));
    expect(screen.getByText('실시간 로그 연결됨')).toBeVisible();
    await act(async () => {
      window.history.pushState({}, '', '/releases/release-2');
      window.dispatchEvent(new PopStateEvent('popstate'));
    });
    const next = await reconnected(2, 0, 'release-2');
    expect(source.closed).toBe(true);
    expect(screen.queryByText('server line 57')).not.toBeInTheDocument();
    expect(screen.getByText('실행 로그가 도착하면 여기에 실시간으로 표시됩니다.')).toBeVisible();
    expect(screen.getByText('로그 연결 대기')).toBeVisible();
    await emit(next, 'open');
    expect(screen.getByText('실시간 로그 연결됨')).toBeVisible();
    await timeout(next);
    await reconnected(3, 0, 'release-2');
  });
});
