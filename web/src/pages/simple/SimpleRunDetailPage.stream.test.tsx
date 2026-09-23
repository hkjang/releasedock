import { ThemeProvider } from '@mui/material';
import { act, render, screen, waitFor } from '@testing-library/react';
import { api, type SimpleLogLine, type SimpleRun } from '../../api/client';
import { App } from '../../app/App';
import { theme } from '../../theme';

// jsdom has no EventSource, so the browser end of the stream is stood in for
// here. Only the transport is replaced: the page, its effects and useAsync are
// the production ones, reached through the real route.
class TestEventSource extends EventTarget {
  static instances: TestEventSource[] = [];
  readonly url: string;
  readonly withCredentials: boolean;
  closed = false;

  constructor(url: string, init?: EventSourceInit) {
    super();
    this.url = url;
    this.withCredentials = init?.withCredentials ?? false;
    TestEventSource.instances.push(this);
  }

  close() {
    this.closed = true;
  }

  emitLog(line: SimpleLogLine) {
    this.dispatchEvent(new MessageEvent('log', { data: JSON.stringify(line) }));
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
    createdAt: '2026-09-23T00:00:00Z',
    startedAt: '2026-09-23T00:00:01Z',
    finishedAt: null,
  };
}

function line(id: number): SimpleLogLine {
  return { id, stream: 'stdout', message: `line ${id}`, createdAt: '2026-09-23T00:00:00Z' };
}

function renderRunDetail() {
  vi.spyOn(api, 'version').mockResolvedValue({ version: '0.5.18' });
  vi.spyOn(api, 'me').mockResolvedValue({
    id: 'user-1',
    username: 'deployer',
    displayName: '배포 담당자',
    roles: ['operator'],
    permissions: ['simple.read'],
  });
  vi.spyOn(api, 'simpleRunLogs').mockResolvedValue({ items: [], lastId: 0, hasMore: false });
  window.history.replaceState({}, '', '/simple/runs/run-1');
  return render(<ThemeProvider theme={theme}><App /></ThemeProvider>);
}

async function openStream(): Promise<TestEventSource> {
  renderRunDetail();
  expect(await screen.findByRole('heading', { name: '실행 상세' })).toBeVisible();
  await waitFor(() => expect(TestEventSource.instances).toHaveLength(1));
  return TestEventSource.instances[0];
}

describe('the live log stream of a running deployment', () => {
  beforeEach(() => {
    TestEventSource.instances = [];
    vi.stubGlobal('EventSource', TestEventSource);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('holds one connection open while lines arrive', async () => {
    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('RUNNING'));
    const source = await openStream();

    for (const id of [1, 2, 3, 4]) {
      await act(async () => source.emitLog(line(id)));
    }

    expect(await screen.findByText('line 4')).toBeVisible();
    // Each line re-renders the page. A connection per line exhausts the
    // server's three-streams-per-user budget within a second and the stream
    // that gets refused stops the log without saying so.
    expect(TestEventSource.instances).toHaveLength(1);
    expect(source.closed).toBe(false);
  });

  it('opens another stream after the server hits its own limit mid-run', async () => {
    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('RUNNING'));
    const source = await openStream();
    expect(source.url).toContain('after=0');

    await act(async () => source.emitLog(line(7)));
    await act(async () => source.emitMaxDuration());

    await waitFor(() => expect(TestEventSource.instances).toHaveLength(2));
    // Resumed after the last line held, so nothing is replayed or skipped.
    expect(TestEventSource.instances[1].url).toContain('after=7');
    expect(TestEventSource.instances[1].closed).toBe(false);
  });

  it('opens no further stream when the run finished while the state still reads as running', async () => {
    // The end frame names the terminal status. Reopening on the strength of
    // state that has not caught up yet would be answered with another end
    // frame, and the two would chase each other.
    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('RUNNING'));
    const source = await openStream();

    await act(async () => source.emitEnd('SUCCESS'));

    expect(source.closed).toBe(true);
    expect(TestEventSource.instances).toHaveLength(1);
  });

  it('closes the stream and opens no other once the run has finished', async () => {
    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('RUNNING'));
    const source = await openStream();

    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('SUCCESS', 0));
    await act(async () => source.emitEnd('SUCCESS'));

    expect(await screen.findByText('종료 코드 0')).toBeVisible();
    expect(source.closed).toBe(true);
    expect(TestEventSource.open).toHaveLength(0);
  });
});
