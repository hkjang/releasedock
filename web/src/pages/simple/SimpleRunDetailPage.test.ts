import {
  collectStoredLogs,
  nextLogCursor,
  undeployedPackages,
  uploadPackages,
  type LogPage,
} from './SimpleRunDetailPage';
import type { SimpleBatchSibling, SimpleLogLine, SimpleRun } from '../../api/client';

function line(id: number): SimpleLogLine {
  return { id, stream: 'stdout', message: `line ${id}`, createdAt: '2026-09-09T00:00:00Z' };
}

// pager serves the given lines in fixed size pages the way the server does:
// hasMore is set whenever the page came back full, so a log whose length is a
// multiple of the page size is followed by one empty page.
function pager(lines: SimpleLogLine[], pageSize: number) {
  const requests: number[] = [];
  const fetchPage = async (after: number): Promise<LogPage> => {
    requests.push(after);
    const items = lines.filter((entry) => entry.id > after).slice(0, pageSize);
    return { items, lastId: items.length ? items[items.length - 1].id : 0, hasMore: items.length === pageSize };
  };
  return { fetchPage, requests };
}

describe('paging through the stored lines of a run', () => {
  it('collects every line of a log shorter than one page', async () => {
    const { fetchPage, requests } = pager([line(1), line(2)], 3);
    const stored = await collectStoredLogs(fetchPage);
    expect(stored.lines.map((entry) => entry.id)).toEqual([1, 2]);
    expect(stored.cursor).toBe(2);
    expect(stored.truncated).toBe(false);
    expect(requests).toEqual([0]);
  });

  it('follows the cursor across pages', async () => {
    const { fetchPage, requests } = pager([line(1), line(2), line(3), line(4), line(5)], 2);
    const stored = await collectStoredLogs(fetchPage);
    expect(stored.lines.map((entry) => entry.id)).toEqual([1, 2, 3, 4, 5]);
    expect(stored.cursor).toBe(5);
    expect(stored.truncated).toBe(false);
    expect(requests).toEqual([0, 2, 4]);
  });

  // The empty page a full last page brings with it used to move the cursor back
  // to 0, which pointed the live stream at the first line of the run.
  it('keeps the cursor when the log ends on a page boundary', async () => {
    const { fetchPage, requests } = pager([line(7), line(9), line(11), line(13)], 2);
    const stored = await collectStoredLogs(fetchPage);
    expect(stored.lines.map((entry) => entry.id)).toEqual([7, 9, 11, 13]);
    expect(stored.cursor).toBe(13);
    expect(stored.truncated).toBe(false);
    expect(requests).toEqual([0, 9, 13]);
  });

  it('reports a log that did not fit in the page budget', async () => {
    const { fetchPage } = pager([line(1), line(2), line(3), line(4), line(5), line(6)], 2);
    const stored = await collectStoredLogs(fetchPage, 2);
    expect(stored.lines.map((entry) => entry.id)).toEqual([1, 2, 3, 4]);
    expect(stored.cursor).toBe(4);
    expect(stored.truncated).toBe(true);
  });

  it('does not call a log complete when the budget ended exactly on the last line', async () => {
    const { fetchPage } = pager([line(1), line(2), line(3), line(4)], 2);
    const stored = await collectStoredLogs(fetchPage, 2);
    // The server still says hasMore, so the view cannot tell this apart from a
    // log that goes on; saying so is better than implying the run stopped here.
    expect(stored.truncated).toBe(true);
  });

  it('handles a run that stored nothing', async () => {
    const { fetchPage } = pager([], 2);
    const stored = await collectStoredLogs(fetchPage);
    expect(stored.lines).toEqual([]);
    expect(stored.cursor).toBe(0);
    expect(stored.truncated).toBe(false);
  });

  it('leaves the cursor alone for an empty page and advances it for a filled one', () => {
    expect(nextLogCursor(41, { items: [], lastId: 0, hasMore: true })).toBe(41);
    expect(nextLogCursor(41, { items: [line(42)], lastId: 42, hasMore: false })).toBe(42);
  });
});

function sibling(id: string, createdAt: string, status: SimpleBatchSibling['status'], last = false): SimpleBatchSibling {
  return { id, filename: `${id}.tar.gz`, status, batchLast: last, createdAt };
}

function runDetail(overrides: Partial<SimpleRun> = {}): SimpleRun {
  return {
    id: 'run-b',
    targetName: 'portal',
    filename: 'run-b.tar.gz',
    status: 'SUCCESS',
    exitCode: 0,
    commandSource: 'PER_TARGET',
    commandPath: '/opt/deploy/run.sh',
    sizeBytes: 1,
    createdAt: '2026-09-09T00:00:02Z',
    startedAt: null,
    finishedAt: null,
    ...overrides,
  };
}

describe('the packages of one upload', () => {
  it('lists the run being viewed among its siblings in deploy order', () => {
    const detail = runDetail({
      batchId: 'batch-1',
      batchLast: true,
      batchSiblings: [
        sibling('run-c', '2026-09-09T00:00:03Z', 'SUCCESS'),
        sibling('run-a', '2026-09-09T00:00:01Z', 'FAILED'),
      ],
    });
    const packages = uploadPackages(detail);
    expect(packages.map((item) => item.id)).toEqual(['run-a', 'run-b', 'run-c']);
    expect(packages.map((item) => item.current)).toEqual([false, true, false]);
    expect(packages[1].batchLast).toBe(true);
  });

  // A single package is its own upload, so there is no relationship to draw and
  // the section stays off the screen.
  it('lists nothing for a run that was uploaded on its own', () => {
    expect(uploadPackages(runDetail())).toEqual([]);
    expect(uploadPackages(runDetail({ batchId: 'batch-1', batchSiblings: [] }))).toEqual([]);
  });

  it('settles two packages stored in the same instant by id', () => {
    const detail = runDetail({
      createdAt: '2026-09-09T00:00:01Z',
      batchSiblings: [sibling('run-a', '2026-09-09T00:00:01Z', 'SUCCESS')],
    });
    expect(uploadPackages(detail).map((item) => item.id)).toEqual(['run-a', 'run-b']);
  });

  it('counts the packages that finished without deploying', () => {
    const detail = runDetail({
      status: 'FAILED',
      batchSiblings: [
        sibling('run-a', '2026-09-09T00:00:01Z', 'SUCCESS'),
        sibling('run-c', '2026-09-09T00:00:03Z', 'TIMEOUT'),
      ],
    });
    expect(undeployedPackages(uploadPackages(detail))).toBe(2);
  });

  // A package still going has not failed. Counting it would tell the reader the
  // upload is missing something when it is only unfinished.
  it('does not count a package that has not finished', () => {
    const detail = runDetail({
      status: 'RUNNING',
      batchSiblings: [sibling('run-a', '2026-09-09T00:00:01Z', 'PENDING')],
    });
    expect(undeployedPackages(uploadPackages(detail))).toBe(0);
  });
});
