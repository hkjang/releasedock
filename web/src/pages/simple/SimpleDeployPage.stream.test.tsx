import { ThemeProvider } from '@mui/material';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { api, type SimpleLogLine, type SimpleRun, type SimpleTarget } from '../../api/client';
import { App } from '../../app/App';
import { theme } from '../../theme';

const DISCONNECTED_NOTICE = '실시간 로그 연결이 끊겼습니다';

// jsdom has no EventSource, so only the browser end of the stream is stood in
// for. The page, its effects and the upload queue that sets the run being
// streamed are the production ones, reached through the real route.
class TestEventSource extends EventTarget {
  static instances: TestEventSource[] = [];
  readonly url: string;
  readonly withCredentials: boolean;
  closed = false;
  // The spec's numbers, as the browser reports them: 0 CONNECTING, 1 OPEN,
  // 2 CLOSED.
  readyState = 1;

  constructor(url: string, init?: EventSourceInit) {
    super();
    this.url = url;
    this.withCredentials = init?.withCredentials ?? false;
    TestEventSource.instances.push(this);
  }

  close() {
    this.closed = true;
    this.readyState = 2;
  }

  emitLog(line: SimpleLogLine) {
    this.dispatchEvent(new MessageEvent('log', { data: JSON.stringify(line) }));
  }

  // The browser fires error both when it has given the stream up (CLOSED) and
  // when it is about to retry by itself (CONNECTING); only the state tells the
  // two apart.
  emitError(readyState: number) {
    this.readyState = readyState;
    if (readyState === 2) this.closed = true;
    this.dispatchEvent(new Event('error'));
  }

  // The server ends a stream either because the run reached a terminal status
  // or because it hit its own thirty-minute ceiling. The two frames differ.
  emitEnd(status: string) {
    this.dispatchEvent(new MessageEvent('end', { data: JSON.stringify({ status }) }));
  }

  emitMaxDuration() {
    this.dispatchEvent(new MessageEvent('end', { data: JSON.stringify({ reason: 'max_duration' }) }));
  }

  static get open(): TestEventSource[] {
    return TestEventSource.instances.filter((instance) => !instance.closed);
  }
}

function target(): SimpleTarget {
  return {
    id: 'target-1',
    name: '운영 서버',
    description: '',
    uploadDir: '/opt/releasedock/incoming',
    maxUploadBytes: 1024 * 1024 * 1024,
    ready: true,
    notReadyReason: '',
  };
}

function simpleRun(status: SimpleRun['status'], exitCode: number | null = null): SimpleRun {
  return {
    id: 'run-1',
    targetName: '운영 서버',
    filename: 'ai-portal-v2.4.1.tar.gz',
    status,
    exitCode,
    commandSource: 'SHARED',
    commandPath: '/opt/releasedock/deploy.sh',
    sizeBytes: 1024,
    createdAt: '2026-09-28T00:00:00Z',
    startedAt: '2026-09-28T00:00:01Z',
    finishedAt: null,
  };
}

function line(id: number): SimpleLogLine {
  return { id, stream: 'stdout', message: `line ${id}`, createdAt: '2026-09-28T00:00:00Z' };
}

// Drives the page the way an operator does: drop one package in, press the
// button, and let the queue upload it. The run is left RUNNING so the stream
// the queue opened stays the one under test.
async function startUpload(): Promise<TestEventSource> {
  vi.spyOn(api, 'version').mockResolvedValue({ version: '0.5.23' });
  vi.spyOn(api, 'me').mockResolvedValue({
    id: 'user-1',
    username: 'deployer',
    displayName: '배포 담당자',
    roles: ['operator'],
    permissions: ['simple.deploy', 'simple.read'],
  });
  vi.spyOn(api, 'simpleTargets').mockResolvedValue({ items: [target()], commandMode: 'SHARED' });
  vi.spyOn(api, 'startSimpleRun').mockResolvedValue(simpleRun('RUNNING'));
  vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('RUNNING'));
  window.history.replaceState({}, '', '/simple');
  const view = render(<ThemeProvider theme={theme}><App /></ThemeProvider>);

  expect(await screen.findByRole('heading', { name: '배포' })).toBeVisible();
  const input = view.container.querySelector('input[type="file"]');
  if (!input) throw new Error('the deploy page rendered no file input');
  const file = new File(['payload'], 'ai-portal-v2.4.1.tar.gz', { type: 'application/gzip' });
  await act(async () => {
    fireEvent.change(input, { target: { files: [file] } });
  });
  await act(async () => {
    fireEvent.click(screen.getByRole('button', { name: '배포 실행' }));
  });

  await waitFor(() => expect(TestEventSource.instances).toHaveLength(1));
  return TestEventSource.instances[0];
}

describe('the live log the deploy page shows while it uploads', () => {
  beforeEach(() => {
    TestEventSource.instances = [];
    vi.stubGlobal('EventSource', TestEventSource);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('shows the lines the run produces', async () => {
    const source = await startUpload();

    await act(async () => source.emitLog(line(1)));

    expect(await screen.findByText('line 1')).toBeVisible();
  });

  it('says so when the browser gave the stream up for good', async () => {
    // A refused stream (the server allows three per user, and the run detail
    // screen open in other tabs spends them) or an expired session leaves the
    // stream CLOSED and fires error once. Nothing else arrives, so without
    // this the upload reads as progressing with a log that silently stopped.
    const source = await startUpload();
    await act(async () => source.emitLog(line(1)));

    await act(async () => source.emitError(2));

    expect(await screen.findByText(DISCONNECTED_NOTICE, { exact: false })).toBeVisible();
    expect(screen.getByRole('button', { name: '다시 연결' })).toBeVisible();
  });

  it('stays quiet while the browser is reconnecting by itself', async () => {
    // A dropped connection the browser will retry leaves the stream
    // CONNECTING. Announcing that would ask the operator to spend one of the
    // three streams the server allows on a stream that is coming back anyway.
    const source = await startUpload();

    await act(async () => source.emitError(0));

    expect(screen.queryByText(DISCONNECTED_NOTICE, { exact: false })).toBeNull();
  });

  it('opens another stream from the last line after the server hits its own limit mid-upload', async () => {
    // The server stops streaming at its own thirty-minute ceiling with an end
    // frame that names a reason rather than a status, and the run is still
    // deploying. The queue is still waiting on it, so the log must carry on.
    const source = await startUpload();
    expect(source.url).toContain('after=0');

    await act(async () => source.emitLog(line(7)));
    await act(async () => source.emitMaxDuration());

    await waitFor(() => expect(TestEventSource.instances).toHaveLength(2));
    expect(TestEventSource.instances[1].url).toContain('after=7');
    expect(TestEventSource.instances[1].closed).toBe(false);
  });

  it('resumes from the last line when the operator reconnects by hand', async () => {
    // Reopening from the cursor is what fills the gap: the server reads the
    // lines after it out of storage, so what the run wrote while nothing was
    // listening still reaches the screen.
    const source = await startUpload();
    await act(async () => source.emitLog(line(4)));
    await act(async () => source.emitError(2));

    await act(async () => {
      fireEvent.click(await screen.findByRole('button', { name: '다시 연결' }));
    });

    await waitFor(() => expect(TestEventSource.instances).toHaveLength(2));
    expect(TestEventSource.instances[1].url).toContain('after=4');
    expect(screen.queryByText(DISCONNECTED_NOTICE, { exact: false })).toBeNull();
  });

  it('drops the lost-connection notice once the queue has finished with the run', async () => {
    // The notice offers a reconnect, and there is nothing left to reconnect to
    // once the run is done and the queue has moved on. Leaving it up would sit
    // a dead button over a log that is already complete.
    const source = await startUpload();
    await act(async () => source.emitError(2));
    expect(await screen.findByText(DISCONNECTED_NOTICE, { exact: false })).toBeVisible();

    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('SUCCESS', 0));

    await waitFor(() => expect(screen.queryByText(DISCONNECTED_NOTICE, { exact: false })).toBeNull(), {
      timeout: 5000,
    });
  });

  it('opens no further stream once the end frame names the run status', async () => {
    // The run itself ended. Another stream would only be answered with another
    // end frame, and the two would chase each other.
    const source = await startUpload();

    await act(async () => source.emitEnd('SUCCESS'));

    expect(source.closed).toBe(true);
    expect(TestEventSource.open).toHaveLength(0);
    expect(TestEventSource.instances).toHaveLength(1);
  });
});
