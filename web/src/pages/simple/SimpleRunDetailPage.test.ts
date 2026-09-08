import { collectStoredLogs, nextLogCursor, type LogPage } from './SimpleRunDetailPage';
import type { SimpleLogLine } from '../../api/client';

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
