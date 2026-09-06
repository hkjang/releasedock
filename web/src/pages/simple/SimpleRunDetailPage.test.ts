import { stageLabel, statusColor } from './SimpleRunDetailPage';

describe('how a post-deployment stage that did not run is presented', () => {
  it('does not send the reader of a held stage to wait for a later file', () => {
    // Deferred: the stage belongs to the last package of the upload and runs
    // there, so there is nothing for the operator to do.
    expect(stageLabel('SKIPPED')).toContain('마지막 파일');
    // Held: a package of this upload never deployed, so nothing was mirrored
    // and no later file will mirror it. Saying "runs on the last file" here
    // would describe the opposite of what happened.
    expect(stageLabel('HELD')).not.toContain('마지막 파일');
    expect(stageLabel('HELD')).toContain('실행 안 함');
    // Anything that ran keeps its own state name.
    expect(stageLabel('SUCCESS')).toBe('SUCCESS');
    expect(stageLabel('FAILED')).toBe('FAILED');
  });

  it('colours a held stage as a warning rather than an ordinary state', () => {
    expect(statusColor('HELD')).toBe('warning');
    expect(statusColor('SKIPPED')).toBe('default');
    expect(statusColor('SUCCESS')).toBe('success');
    expect(statusColor('FAILED')).toBe('error');
  });
});
