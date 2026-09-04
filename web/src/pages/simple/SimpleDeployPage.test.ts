import { POLL_FAILURE_BUDGET, pollAbandonReason, stagesDeferred, stagesReached } from './SimpleDeployPage';

describe('once-per-upload stages that lost the package carrying them', () => {
  it('treats only SKIPPED as a stage pushed onto the last package', () => {
    expect(stagesDeferred({ replicationStatus: 'SKIPPED', appDeployStatus: 'SKIPPED' })).toBe(true);
    expect(stagesDeferred({ replicationStatus: 'SUCCESS', appDeployStatus: 'SKIPPED' })).toBe(true);
    expect(stagesDeferred({ replicationStatus: 'SUCCESS', appDeployStatus: 'SUCCESS' })).toBe(false);
    expect(stagesDeferred({ replicationStatus: 'NONE', appDeployStatus: 'NONE' })).toBe(false);
    expect(stagesDeferred({})).toBe(false);
  });

  it('counts a stage as reached once it produced an outcome, failure included', () => {
    expect(stagesReached({ replicationStatus: 'SUCCESS', appDeployStatus: 'SUCCESS' })).toBe(true);
    // A stage that ran and failed already reports its error on that run.
    expect(stagesReached({ replicationStatus: 'FAILED', appDeployStatus: 'NONE' })).toBe(true);
    expect(stagesReached({ replicationStatus: 'SUCCESS', appDeployStatus: 'TIMEOUT' })).toBe(true);
    // The command of the marked package failed, so the stages were never tried.
    expect(stagesReached({ replicationStatus: 'NONE', appDeployStatus: 'NONE' })).toBe(false);
    // The marked package never ran at all: stopped by the user, or rejected.
    expect(stagesReached(undefined)).toBe(false);
    expect(stagesReached({})).toBe(false);
  });

  it('warns exactly when an upload deferred stages that no package then ran', () => {
    const stranded = (
      earlier: Parameters<typeof stagesDeferred>[0],
      marked: Parameters<typeof stagesReached>[0],
    ) => stagesDeferred(earlier) && !stagesReached(marked);

    // Stopped after the first package: the marker never left the queue.
    expect(stranded({ replicationStatus: 'SKIPPED', appDeployStatus: 'SKIPPED' }, undefined)).toBe(true);
    // The last upload was rejected, so its run was never created either.
    expect(stranded({ replicationStatus: 'SUCCESS', appDeployStatus: 'SKIPPED' }, undefined)).toBe(true);
    // The last package deployed and carried the deferred stages through.
    expect(stranded(
      { replicationStatus: 'SKIPPED', appDeployStatus: 'SKIPPED' },
      { replicationStatus: 'SUCCESS', appDeployStatus: 'SUCCESS' },
    )).toBe(false);
    // Nothing was deferred, so a failed last package strands nothing.
    expect(stranded(
      { replicationStatus: 'SUCCESS', appDeployStatus: 'SUCCESS' },
      { replicationStatus: 'NONE', appDeployStatus: 'NONE' },
    )).toBe(false);
  });
});

describe('giving up on a run whose state cannot be read', () => {
  it('keeps polling while a failure may still recover', () => {
    expect(pollAbandonReason(undefined, 1)).toBeNull();
    expect(pollAbandonReason(500, 1)).toBeNull();
    expect(pollAbandonReason(502, POLL_FAILURE_BUDGET - 1)).toBeNull();
    // A read that succeeds resets the count, so a long run never runs out.
    expect(pollAbandonReason(undefined, 0)).toBeNull();
  });

  it('stops at once on a failure that will never recover', () => {
    // The session ended: every later read is rejected the same way.
    expect(pollAbandonReason(401, 1)).toMatch(/로그인/);
    expect(pollAbandonReason(403, 1)).toMatch(/로그인/);
    // The record is gone, so there is nothing left to wait for.
    expect(pollAbandonReason(404, 1)).toMatch(/실행 기록/);
  });

  it('stops once the retry budget for transient failures is spent', () => {
    expect(pollAbandonReason(503, POLL_FAILURE_BUDGET)).toMatch(/서버에 연결하지 못해/);
    // A network error carries no status and is retried on the same budget.
    expect(pollAbandonReason(undefined, POLL_FAILURE_BUDGET)).toMatch(/서버에 연결하지 못해/);
    expect(pollAbandonReason(undefined, POLL_FAILURE_BUDGET + 5)).not.toBeNull();
  });
});
