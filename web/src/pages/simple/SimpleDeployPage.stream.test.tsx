import { ThemeProvider } from '@mui/material';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { api, ApiError, type SimpleLogLine, type SimpleRun, type SimpleTarget } from '../../api/client';
import { App } from '../../app/App';
import { theme } from '../../theme';
import {
  appendDeployLogLine,
  DEPLOY_LOG_DISPLAY_LIMIT,
  DEPLOY_LOG_TRUNCATED_NOTICE,
  EMPTY_DEPLOY_LOGS,
} from './SimpleDeployPage';

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

const PACKAGE_NAME = 'ai-portal-v2.4.1.tar.gz';

// Mounts the page through the real route with one package waiting in the queue.
// The permissions are the signed-in operator's: reading the run history is a
// permission of its own, so an account that may deploy and nothing else is a
// case the page has to render for.
async function dropPackage(permissions = ['simple.deploy', 'simple.read']): Promise<void> {
  vi.spyOn(api, 'version').mockResolvedValue({ version: '0.5.23' });
  vi.spyOn(api, 'me').mockResolvedValue({
    id: 'user-1',
    username: 'deployer',
    displayName: '배포 담당자',
    roles: ['operator'],
    permissions,
  });
  vi.spyOn(api, 'simpleTargets').mockResolvedValue({ items: [target()], commandMode: 'SHARED' });
  vi.spyOn(api, 'startSimpleRun').mockResolvedValue(simpleRun('RUNNING'));
  vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('RUNNING'));
  window.history.replaceState({}, '', '/simple');
  const view = render(<ThemeProvider theme={theme}><App /></ThemeProvider>);

  expect(await screen.findByRole('heading', { name: '배포' })).toBeVisible();
  const input = view.container.querySelector('input[type="file"]');
  if (!input) throw new Error('the deploy page rendered no file input');
  const file = new File(['payload'], PACKAGE_NAME, { type: 'application/gzip' });
  await act(async () => {
    fireEvent.change(input, { target: { files: [file] } });
  });
}

// Drives the page the way an operator does: drop one package in, press the
// button, and let the queue upload it. The run is left RUNNING so the stream
// the queue opened stays the one under test.
async function startUpload(permissions?: string[]): Promise<TestEventSource> {
  await dropPackage(permissions);
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

  // One upload's packages share this buffer - it is emptied when a batch
  // starts, not between packages - so the lines pushed out of it are usually
  // the first packages' output, and this screen has neither a copy button nor
  // a link to the stored log. A reader judging the deployment from a log whose
  // beginning quietly went missing has no way to tell that it did.
  // Drawing five thousand log rows is what this costs, so it is the only
  // rendered case: the boundary itself is pinned down on the pure helper below.
  it('warns once the batch log pushes its oldest line out, keeps the warning across a reconnect, and clears it for the next batch', async () => {
    const source = await startUpload();
    // The separator the queue writes for the package holds the first line of
    // the buffer, so this run's output pushes it, and only it, out.
    await act(async () => {
      for (let id = 1; id <= DEPLOY_LOG_DISPLAY_LIMIT; id += 1) source.emitLog(line(id));
    });

    const notice = await screen.findByText(DEPLOY_LOG_TRUNCATED_NOTICE, { exact: false });
    expect(notice).toBeVisible();
    expect(notice).toHaveTextContent(`현재 최근 ${DEPLOY_LOG_DISPLAY_LIMIT.toLocaleString()}줄만 표시합니다.`);
    expect(notice).toHaveTextContent('로그 내려받기');
    // The front of the log is what went: the separator naming the package is
    // gone from the view while the newest line is still there.
    expect(screen.queryByText('── ai-portal-v2.4.1.tar.gz ──')).toBeNull();
    expect(screen.getByText(`line ${DEPLOY_LOG_DISPLAY_LIMIT}`)).toBeVisible();

    // The server's own ceiling reopens the stream onto the same buffer, and the
    // lines are still missing from it, so the notice has to survive that.
    await act(async () => source.emitMaxDuration());
    await waitFor(() => expect(TestEventSource.instances).toHaveLength(2));
    expect(screen.getByText(DEPLOY_LOG_TRUNCATED_NOTICE, { exact: false })).toBeVisible();

    // Nor does the run finishing restore what went missing.
    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('SUCCESS', 0));
    await waitFor(() => expect(screen.queryByRole('button', { name: '남은 파일 중단' })).toBeNull(), { timeout: 5000 });
    expect(screen.getByText(DEPLOY_LOG_TRUNCATED_NOTICE, { exact: false })).toBeVisible();

    // The next batch empties the buffer, and a notice about lines that are no
    // longer missing would send the reader after a log that is whole.
    const input = document.querySelector('input[type="file"]');
    if (!input) throw new Error('the deploy page rendered no file input');
    await act(async () => {
      fireEvent.change(input, { target: { files: [new File(['payload'], 'ai-portal-v2.4.2.tar.gz')] } });
    });
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: '배포 실행' }));
    });

    await waitFor(() => expect(screen.queryByText(DEPLOY_LOG_TRUNCATED_NOTICE, { exact: false })).toBeNull());
  }, 20000);
});

// The deploy screen is the only place that knows which run each package of the
// upload became. It spends three separate notices sending the reader to that
// run - the truncated live log, the abandoned-poll error, the stranded-stages
// warning - and until now none of them could be followed from here: the run id
// was held in the queue and thrown away when the page was left.
describe("the deploy queue's link to each package's run", () => {
  const RUN_LINK = `${PACKAGE_NAME} 실행 상세`;

  beforeEach(() => {
    TestEventSource.instances = [];
    vi.stubGlobal('EventSource', TestEventSource);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('has nothing to link to before the package is uploaded', async () => {
    await dropPackage();

    expect(screen.getByText(PACKAGE_NAME)).toBeVisible();
    expect(screen.queryByRole('link', { name: RUN_LINK })).toBeNull();
  });

  it('links a package to its own run as soon as the upload created it', async () => {
    await startUpload();

    const link = await screen.findByRole('link', { name: RUN_LINK });
    expect(link).toHaveAttribute('href', '/simple/runs/run-1');
  });

  it('keeps the link once the run has finished', async () => {
    // This is when it matters most: the queue has stopped streaming, the live
    // log is as complete as it will ever be on this screen, and the stored log
    // and its download only exist on the run's own page.
    const source = await startUpload();
    vi.spyOn(api, 'simpleRun').mockResolvedValue(simpleRun('SUCCESS', 0));
    await act(async () => source.emitEnd('SUCCESS'));

    await waitFor(() => expect(screen.queryByRole('button', { name: '남은 파일 중단' })).toBeNull(), {
      timeout: 5000,
    });
    expect(screen.getByRole('link', { name: RUN_LINK })).toHaveAttribute('href', '/simple/runs/run-1');
  });

  it('keeps the link to a run the queue gave up reading', async () => {
    // The queue stops asking when the record cannot be read and says the
    // deployment may still be going, so the result has to be looked up - and
    // this row holds the only pointer to the run it has to be looked up by.
    await startUpload();
    vi.spyOn(api, 'simpleRun').mockRejectedValue(new ApiError('실행 기록을 찾을 수 없습니다.', 404));

    expect(await screen.findByText('확인 불가', {}, { timeout: 5000 })).toBeVisible();
    expect(screen.getByRole('link', { name: RUN_LINK })).toHaveAttribute('href', '/simple/runs/run-1');
  });

  it('offers no link to an operator who may deploy but not read the run history', async () => {
    // The run detail route is guarded by simple.read and sends an account
    // without it to the forbidden page, so a link drawn for that account only
    // throws the reader out of the deployment they are watching.
    await startUpload(['simple.deploy']);

    expect(await screen.findByText(PACKAGE_NAME)).toBeVisible();
    expect(screen.queryByRole('link', { name: RUN_LINK })).toBeNull();
  });
});

// The boundary is checked here rather than through the page because proving it
// on screen costs a render of five thousand log rows per case.
describe('appendDeployLogLine', () => {
  const entry = (id: number) => ({ id, stream: 'stdout', message: `line ${id}` });

  it('keeps every line while the buffer only reaches the limit', () => {
    let state = EMPTY_DEPLOY_LOGS;
    for (let id = 1; id <= 3; id += 1) state = appendDeployLogLine(state, entry(id), 3);

    expect(state.lines.map((row) => row.message)).toEqual(['line 1', 'line 2', 'line 3']);
    expect(state.truncated).toBe(false);
  });

  it('drops the oldest line and reports the loss once a line is pushed out', () => {
    let state = EMPTY_DEPLOY_LOGS;
    for (let id = 1; id <= 4; id += 1) state = appendDeployLogLine(state, entry(id), 3);

    expect(state.lines.map((row) => row.message)).toEqual(['line 2', 'line 3', 'line 4']);
    expect(state.truncated).toBe(true);
  });

  it('goes on reporting a loss that already happened', () => {
    // The flag describes the buffer, not the last append: the beginning stays
    // missing however many lines arrive afterwards.
    let state = appendDeployLogLine({ lines: [entry(1), entry(2), entry(3)], truncated: true }, entry(4), 10);

    state = appendDeployLogLine(state, entry(5), 10);

    expect(state.lines).toHaveLength(5);
    expect(state.truncated).toBe(true);
  });
});
