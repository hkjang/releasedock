import { ThemeProvider } from '@mui/material';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { api } from '../../api/client';
import { App } from '../../app/App';
import { theme } from '../../theme';
import type { Release } from '../../types/domain';
import type { ReleaseLogState } from './ReleaseDetailPage';
import { EMPTY_RELEASE_LOGS, LOG_DISPLAY_LIMIT, RELEASE_LOG_TRUNCATED_NOTICE, appendReleaseLogLine } from './ReleaseDetailPage';

// Only the browser transport is replaced; exercise property callbacks too.
class TestEventSource extends EventTarget {
  static instances: TestEventSource[] = [];
  closed = false;
  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  readonly withCredentials: boolean;
  // The spec's numbers, as the browser reports them: 0 CONNECTING, 1 OPEN,
  // 2 CLOSED.
  readyState = 1;
  constructor(readonly url: string, init?: EventSourceInit) {
    super();
    this.withCredentials = init?.withCredentials ?? false;
    TestEventSource.instances.push(this);
  }
  close() { this.closed = true; this.readyState = 2; }
  emit(type: string, data = '', lastEventId = '') {
    const event = new MessageEvent(type, { data, lastEventId });
    if (type === 'open') this.onopen?.(event);
    if (type === 'message') this.onmessage?.(event);
    if (type === 'error') this.onerror?.(event);
    this.dispatchEvent(event);
  }
  // The browser fires error both when it has given the stream up (CLOSED) and
  // when it is about to retry by itself (CONNECTING); only the state tells the
  // two apart.
  emitError(readyState: number) {
    this.readyState = readyState;
    if (readyState === 2) this.closed = true;
    this.emit('error');
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

// One act() for the whole burst. Awaiting a frame at a time costs a React commit
// per line, which the display limit would multiply into thousands of renders;
// dispatching inside a single act() lets React fold them into one commit.
const emitMany = async (source: TestEventSource, ids: number[]) => {
  await act(async () => {
    for (const id of ids) source.emit('log', JSON.stringify(line(id)), String(id));
  });
};
const ids = (count: number, first = 1) => Array.from({ length: count }, (_, index) => first + index);
// Bursting past the display limit renders thousands of rows in one commit,
// which outlasts the 5s default.
const SLOW_RENDER_TIMEOUT = 30_000;
const copiedBody = (lineIds: number[]) => lineIds.map((id) => `${release.createdAt} stdout server line ${id}`).join('\n');

async function renderPage(releaseId = release.id) {
  vi.spyOn(api, 'version').mockResolvedValue({ version: '0.5.24' });
  vi.spyOn(api, 'me').mockResolvedValue({
    id: 'user-1', username: 'deployer', displayName: '배포 담당자', roles: ['developer'], permissions: ['releases.read'],
  });
  vi.spyOn(api, 'release').mockImplementation(async (id) => ({ ...release, id }));
  window.history.replaceState({}, '', `/releases/${encodeURIComponent(releaseId)}`);
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

  describe('once the browser gives the stream up', () => {
    const lostNotice = () => screen.queryByText(/실시간 로그 연결이 끊겼습니다/);
    const reconnectButton = () => screen.queryByRole('button', { name: '다시 연결' });

    it('stays quiet while the browser is still retrying by itself', async () => {
      const { source } = await renderPage();
      await emit(source, 'open');
      await act(async () => source.emitError(0));
      expect(lostNotice()).not.toBeInTheDocument();
      expect(reconnectButton()).toBeNull();
      expect(screen.getByText('로그 연결 대기')).toBeVisible();
      expect(TestEventSource.instances).toHaveLength(1);
    });

    it('warns on a closed stream and resumes one stream from the last cursor when asked', async () => {
      const { source } = await renderPage();
      await emit(source, 'open');
      await emit(source, 'log', JSON.stringify(line(57)), '57');
      await act(async () => source.emitError(2));
      expect(lostNotice()).toBeVisible();
      // The warning alone must not open anything; only the operator does.
      expect(TestEventSource.instances).toHaveLength(1);
      expect(screen.getByText('server line 57')).toBeVisible();
      fireEvent.click(screen.getByRole('button', { name: '다시 연결' }));
      const next = await reconnected(2, 57);
      expect(screen.getByText('server line 57')).toBeVisible();
      await emit(next, 'open');
      expect(lostNotice()).not.toBeInTheDocument();
      await emit(next, 'log', JSON.stringify(line(58)), '58');
      expect(screen.getByText('server line 58')).toBeVisible();
      // A replacement the server also refuses has to say so again.
      await act(async () => next.emitError(2));
      expect(lostNotice()).toBeVisible();
      expect(TestEventSource.instances).toHaveLength(2);
    });

    it('sends the reader to the stored log for the lines after the cut', async () => {
      const { source } = await renderPage();
      await emit(source, 'open');
      await act(async () => source.emitError(2));
      expect(lostNotice()).toHaveTextContent('로그 내려받기');
    });

    it('stays quiet when an error trails a stream that ended normally', async () => {
      const { source } = await renderPage();
      await emit(source, 'open');
      await emit(source, 'end', '{}');
      await act(async () => source.emitError(2));
      expect(lostNotice()).not.toBeInTheDocument();
      expect(TestEventSource.instances).toHaveLength(1);
    });

    it('drops the warning when the route moves to another release', async () => {
      const { source } = await renderPage();
      await act(async () => source.emitError(2));
      expect(lostNotice()).toBeVisible();
      await act(async () => {
        window.history.pushState({}, '', '/releases/release-2');
        window.dispatchEvent(new PopStateEvent('popstate'));
      });
      await reconnected(2, 0, 'release-2');
      expect(lostNotice()).not.toBeInTheDocument();
    });
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

  describe('the stored log download', () => {
    const downloadLink = () => screen.getByRole('link', { name: '로그 내려받기' });

    it('stays reachable while the view holds no lines at all', async () => {
      await renderPage();
      // The stored log owes nothing to this buffer, and an empty buffer is
      // precisely when a reader needs it, so this one must not copy the
      // emptiness guard the clear and copy controls carry.
      expect(screen.getByText('실행 로그가 도착하면 여기에 실시간으로 표시됩니다.')).toBeVisible();
      expect(screen.getByRole('button', { name: '지우기' })).toBeDisabled();
      expect(downloadLink()).toHaveAttribute('href', '/api/v1/releases/release-1/logs?format=text');
      // The href alone does not settle it: a disabled MUI link keeps its href
      // and only stops being reachable through aria-disabled and tabindex.
      expect(downloadLink()).not.toHaveAttribute('aria-disabled');
      expect(downloadLink()).toHaveAttribute('tabindex', '0');
    });

    it('percent-encodes the release id the route carried in', async () => {
      await renderPage('release 1');
      expect(downloadLink()).toHaveAttribute('href', '/api/v1/releases/release%201/logs?format=text');
    });
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

  it('numbers every line of a batch apart, so React keeps the rows distinct', async () => {
    // React folds frames that land in one tick into a single commit, so a row
    // number read inside the state updater is the same for all of them.
    const logged = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { source } = await renderPage();
    await act(async () => {
      for (const id of ids(3)) source.emit('log', JSON.stringify(line(id)), String(id));
    });
    for (const id of ids(3)) expect(screen.getByText(`server line ${id}`)).toBeVisible();
    expect(logged.mock.calls.map(String).filter((text) => text.includes('same key'))).toEqual([]);
  });

  describe('once the display limit drops the oldest lines', () => {
    let writeText: ReturnType<typeof vi.fn>;
    let restoreClipboard: () => void;

    beforeEach(() => {
      // jsdom ships no clipboard, and vi.restoreAllMocks() does not undo
      // defineProperty, so the descriptor is put back by hand.
      writeText = vi.fn(async () => {});
      const original = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
      restoreClipboard = () => {
        if (original) Object.defineProperty(navigator, 'clipboard', original);
        else delete (navigator as { clipboard?: unknown }).clipboard;
      };
    });
    afterEach(() => restoreClipboard());

    const notice = () => screen.queryByText(/오래된 로그 줄이 화면에서 빠졌습니다/);
    const copyLog = async () => {
      fireEvent.click(screen.getByRole('button', { name: '전체 로그 복사' }));
      await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
      return writeText.mock.calls[0][0] as string;
    };

    it('leaves the view and the copied log untouched while the buffer stays at the limit', async () => {
      const { source } = await renderPage();
      await emitMany(source, ids(LOG_DISPLAY_LIMIT));
      expect(notice()).not.toBeInTheDocument();
      expect(screen.getByText('server line 1')).toBeVisible();
      expect(await copyLog()).toBe(copiedBody(ids(LOG_DISPLAY_LIMIT)));
    }, SLOW_RENDER_TIMEOUT);

    it('warns with the displayed line count and leads the copied log with the notice', async () => {
      const { source } = await renderPage();
      await emitMany(source, ids(LOG_DISPLAY_LIMIT + 1));
      expect(notice()).toHaveTextContent(`현재 최근 ${LOG_DISPLAY_LIMIT.toLocaleString()}줄만 표시합니다.`);
      expect(screen.queryByText('server line 1')).not.toBeInTheDocument();
      expect(screen.getByText(`server line ${LOG_DISPLAY_LIMIT + 1}`)).toBeVisible();
      expect(await copyLog()).toBe(`${RELEASE_LOG_TRUNCATED_NOTICE}\n${copiedBody(ids(LOG_DISPLAY_LIMIT, 2))}`);
    }, SLOW_RENDER_TIMEOUT);

    it('sends the reader to the stored log for the lines pushed out', async () => {
      const { source } = await renderPage();
      await emitMany(source, ids(LOG_DISPLAY_LIMIT + 1));
      expect(notice()).toHaveTextContent('로그 내려받기');
    }, SLOW_RENDER_TIMEOUT);

    it('keeps warning after a max_duration reconnect and stops once the buffer is emptied', async () => {
      const { source } = await renderPage();
      await emitMany(source, ids(LOG_DISPLAY_LIMIT + 1));
      await timeout(source);
      const next = await reconnected(2, LOG_DISPLAY_LIMIT + 1);
      expect(notice()).toBeVisible();
      await emit(next, 'log', JSON.stringify(line(LOG_DISPLAY_LIMIT + 2)), String(LOG_DISPLAY_LIMIT + 2));
      expect(notice()).toBeVisible();
      fireEvent.click(screen.getByRole('button', { name: '지우기' }));
      expect(notice()).not.toBeInTheDocument();
    }, SLOW_RENDER_TIMEOUT);

    it('keeps warning after a manual reconnect', async () => {
      const { source } = await renderPage();
      await emitMany(source, ids(LOG_DISPLAY_LIMIT + 1));
      await act(async () => source.emitError(2));
      fireEvent.click(screen.getByRole('button', { name: '다시 연결' }));
      await reconnected(2, LOG_DISPLAY_LIMIT + 1);
      expect(notice()).toBeVisible();
      expect(screen.getByText(`server line ${LOG_DISPLAY_LIMIT + 1}`)).toBeVisible();
    }, SLOW_RENDER_TIMEOUT);

    it('stops warning after leaving and re-entering the tab', async () => {
      const { source } = await renderPage();
      await emitMany(source, ids(LOG_DISPLAY_LIMIT + 1));
      fireEvent.click(screen.getByRole('tab', { name: '실행 단계' }));
      fireEvent.click(screen.getByRole('tab', { name: '실시간 로그' }));
      await reconnected(2, 0);
      expect(notice()).not.toBeInTheDocument();
    }, SLOW_RENDER_TIMEOUT);

    it('stops warning after routing to another release', async () => {
      const { source } = await renderPage();
      await emitMany(source, ids(LOG_DISPLAY_LIMIT + 1));
      expect(notice()).toBeVisible();
      await act(async () => {
        window.history.pushState({}, '', '/releases/release-2');
        window.dispatchEvent(new PopStateEvent('popstate'));
      });
      await reconnected(2, 0, 'release-2');
      expect(notice()).not.toBeInTheDocument();
    }, SLOW_RENDER_TIMEOUT);
  });
});

describe('appendReleaseLogLine', () => {
  const entry = (id: number) => ({ id, message: `line ${id}` });
  const append = (count: number, limit: number) =>
    ids(count).reduce<ReleaseLogState>((state, id) => appendReleaseLogLine(state, entry(id), limit), EMPTY_RELEASE_LOGS);

  it('reports no truncation while the buffer only reaches the limit', () => {
    const state = append(3, 3);
    expect(state.lines.map((item) => item.id)).toEqual([1, 2, 3]);
    expect(state.truncated).toBe(false);
  });

  it('turns truncation on at the first dropped line and never back off', () => {
    const state = append(4, 3);
    expect(state.lines.map((item) => item.id)).toEqual([2, 3, 4]);
    expect(state.truncated).toBe(true);
    const later = appendReleaseLogLine(state, entry(5), 100);
    expect(later.lines.map((item) => item.id)).toEqual([2, 3, 4, 5]);
    expect(later.truncated).toBe(true);
  });

  it('holds the newest lines at the production limit', () => {
    const state = append(LOG_DISPLAY_LIMIT + 1, LOG_DISPLAY_LIMIT);
    expect(state.lines).toHaveLength(LOG_DISPLAY_LIMIT);
    expect(state.lines[0].id).toBe(2);
    expect(state.truncated).toBe(true);
  });
});
