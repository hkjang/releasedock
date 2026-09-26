import { ThemeProvider } from '@mui/material';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { api, type SimpleLogLine, type SimpleRun } from '../../api/client';
import { App } from '../../app/App';
import { theme } from '../../theme';

const DISCONNECTED_NOTICE = '실시간 로그 연결이 끊겼습니다';

// jsdom has no EventSource, so the browser end of the stream is stood in for
// here. Only the transport is replaced: the page, its effects and useAsync are
// the production ones, reached through the real route.
class TestEventSource extends EventTarget {
  static instances: TestEventSource[] = [];
  readonly url: string;
  readonly withCredentials: boolean;
  closed = false;
  // The spec's numbers, as the browser reports them: 0 CONNECTING, 1 OPEN,
  // 2 CLOSED. A stream stands open until something closes it.
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

  emitOpen() {
    this.readyState = 1;
    this.closed = false;
    this.dispatchEvent(new Event('open'));
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

describe('a live log stream the browser could not keep', () => {
  beforeEach(() => {
    TestEventSource.instances = [];
    vi.stubGlobal('EventSource', TestEventSource);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('says so when the browser gave the stream up for good', async () => {
    // A refused stream (the server allows three per user) or a session that
    // expired leaves the stream CLOSED and fires error once. Nothing else
    // arrives, so without this the run reads as still running with a log that
    // silently stopped.
    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('RUNNING'));
    const source = await openStream();

    await act(async () => source.emitError(2));

    expect(await screen.findByText(DISCONNECTED_NOTICE, { exact: false })).toBeVisible();
    expect(screen.getByRole('button', { name: '다시 연결' })).toBeVisible();
  });

  it('stays quiet while the browser is reconnecting by itself', async () => {
    // A dropped connection the browser will retry leaves the stream
    // CONNECTING. Saying it is lost would ask the reader to act on something
    // that is about to fix itself - and every press spends the stream budget.
    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('RUNNING'));
    const source = await openStream();

    await act(async () => source.emitError(0));

    expect(screen.queryByText(DISCONNECTED_NOTICE, { exact: false })).toBeNull();
    expect(screen.queryByRole('button', { name: '다시 연결' })).toBeNull();
  });

  it('takes the notice back once the stream is open again', async () => {
    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('RUNNING'));
    const source = await openStream();

    await act(async () => source.emitError(2));
    expect(await screen.findByText(DISCONNECTED_NOTICE, { exact: false })).toBeVisible();

    await act(async () => source.emitOpen());

    await waitFor(() => expect(screen.queryByText(DISCONNECTED_NOTICE, { exact: false })).toBeNull());
  });

  it('takes the notice back once the run has finished and its whole log has been re-read', async () => {
    // A stream given up on mid-run and a run that then finished is the ordinary
    // case, not a rare one: whatever closed the stream did not stop the run.
    // The refresh re-reads every stored line, so the log on screen is the
    // complete one - leaving it labelled as stopping where the stream did tells
    // the person checking a closed-network deployment that output is missing
    // when none is, and offers a reconnect no finished run can answer.
    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('RUNNING'));
    const source = await openStream();
    await act(async () => source.emitError(2));
    expect(await screen.findByText(DISCONNECTED_NOTICE, { exact: false })).toBeVisible();

    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('SUCCESS', 0));
    vi.spyOn(api, 'simpleRunLogs').mockResolvedValue({
      items: [line(11), line(12)],
      lastId: 12,
      hasMore: false,
    });
    fireEvent.click(screen.getByRole('button', { name: '새로 고침' }));

    expect(await screen.findByText('종료 코드 0')).toBeVisible();
    expect(await screen.findByText('line 12')).toBeVisible();
    await waitFor(() => expect(screen.queryByText(DISCONNECTED_NOTICE, { exact: false })).toBeNull());
    expect(screen.queryByRole('button', { name: '다시 연결' })).toBeNull();
    // The run is over, so nothing reopens a stream - which is also why the
    // 'open' frame that takes the notice back can never arrive here.
    expect(TestEventSource.open).toHaveLength(0);
  });

  it('takes the notice back when the refresh re-read the log of a run still going', async () => {
    // Same re-read, run still RUNNING: the notice has to go for the log being
    // complete, not for the run being over. Waiting on the next stream's 'open'
    // frame would hold a false notice up for as long as the server keeps
    // refusing the stream.
    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('RUNNING'));
    const source = await openStream();
    await act(async () => source.emitError(2));
    expect(await screen.findByText(DISCONNECTED_NOTICE, { exact: false })).toBeVisible();

    vi.spyOn(api, 'simpleRunLogs').mockResolvedValue({
      items: [line(11), line(12)],
      lastId: 12,
      hasMore: false,
    });
    fireEvent.click(screen.getByRole('button', { name: '새로 고침' }));

    expect(await screen.findByText('line 12')).toBeVisible();
    await waitFor(() => expect(screen.queryByText(DISCONNECTED_NOTICE, { exact: false })).toBeNull());
    expect(screen.queryByRole('button', { name: '다시 연결' })).toBeNull();
  });

  it('drops the notice for a finished run even when the log could not be re-read', async () => {
    // The re-read failing leaves the log as the dead stream left it, but the
    // run is over: there is no live connection to have lost and no stream the
    // ' 다시 연결 ' button could open, so the notice would only send the reader
    // pressing a button that does nothing. The failure itself is reported by
    // the log error instead.
    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('RUNNING'));
    const source = await openStream();
    await act(async () => source.emitError(2));
    expect(await screen.findByText(DISCONNECTED_NOTICE, { exact: false })).toBeVisible();

    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('SUCCESS', 0));
    vi.spyOn(api, 'simpleRunLogs').mockRejectedValue(new Error('down'));
    fireEvent.click(screen.getByRole('button', { name: '새로 고침' }));

    expect(await screen.findByText('로그를 불러오지 못했습니다.')).toBeVisible();
    await waitFor(() => expect(screen.queryByText(DISCONNECTED_NOTICE, { exact: false })).toBeNull());
    expect(screen.queryByRole('button', { name: '다시 연결' })).toBeNull();
  });

  it('collects the stored lines and resumes a single stream from the last one held', async () => {
    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('RUNNING'));
    const source = await openStream();
    await act(async () => source.emitError(2));
    expect(await screen.findByText(DISCONNECTED_NOTICE, { exact: false })).toBeVisible();

    // What the run wrote while nothing was listening is only in storage, so a
    // reconnect that resumed from the stream's last frame would leave a hole.
    vi.spyOn(api, 'simpleRunLogs').mockResolvedValue({
      items: [line(11), line(12)],
      lastId: 12,
      hasMore: false,
    });
    fireEvent.click(screen.getByRole('button', { name: '다시 연결' }));

    expect(await screen.findByText('line 12')).toBeVisible();
    // One more stream, not one per press and not a retry loop: the server
    // refuses a fourth and the notice is the only thing asking for one.
    await waitFor(() => expect(TestEventSource.instances).toHaveLength(2));
    expect(TestEventSource.instances[1].url).toContain('after=12');
    expect(TestEventSource.open).toHaveLength(1);
    expect(screen.queryByText(DISCONNECTED_NOTICE, { exact: false })).toBeNull();
  });
});
