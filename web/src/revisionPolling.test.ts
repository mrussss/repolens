import { afterEach, describe, expect, it, vi } from 'vitest';
import { mergeRevisionReads, RevisionPollResult, startRevisionPolling } from './revisionPolling';

afterEach(() => {
  vi.useRealTimers();
});

async function flushMicrotasks() {
  for (let i = 0; i < 8; i++) await Promise.resolve();
}

describe('AnalysisRevision polling', () => {
  it('retains a repository’s last revisions when that read fails', () => {
    const preparing = [{ id: 'revision-1', status: 'PREPARING' }];
    const merged = mergeRevisionReads(
      { 'repo-1': preparing, 'repo-removed': [{ id: 'old' }] },
      [
        { repositoryId: 'repo-1', failed: true },
        { repositoryId: 'repo-2', revisions: [{ id: 'revision-2', status: 'READY' }] },
      ],
    );

    expect(merged).toEqual({
      'repo-1': preparing,
      'repo-2': [{ id: 'revision-2', status: 'READY' }],
    });
    expect(merged['repo-1']).toBe(preparing);
  });

  it('coalesces refresh requests while one read is in flight', async () => {
    vi.useFakeTimers();
    type Value = { status: 'PREPARING' | 'READY' };
    const pending: Array<(result: RevisionPollResult<Value>) => void> = [];
    let active = 0;
    let maximumActive = 0;
    const poll = vi.fn(() => {
      active++;
      maximumActive = Math.max(maximumActive, active);
      return new Promise<RevisionPollResult<Value>>((resolve) => {
        pending.push((result) => {
          active--;
          resolve(result);
        });
      });
    });
    const onValue = vi.fn();
    const poller = startRevisionPolling({ poll, hasPreparing: (value) => value.status === 'PREPARING', onValue, onError: vi.fn() });

    poller.refresh();
    poller.refresh();
    poller.refresh();
    expect(poll).toHaveBeenCalledOnce();

    pending.shift()!({ value: { status: 'PREPARING' } });
    await flushMicrotasks();
    expect(poll).toHaveBeenCalledTimes(2);
    expect(maximumActive).toBe(1);

    pending.shift()!({ value: { status: 'READY' } });
    await flushMicrotasks();
    expect(onValue).toHaveBeenCalledTimes(2);
    expect(maximumActive).toBe(1);
    poller.stop();
  });

  it('does not publish a pending result after the poller is stopped', async () => {
    let resolvePoll!: (result: RevisionPollResult<{ status: 'READY' }>) => void;
    const onValue = vi.fn();
    const poller = startRevisionPolling({
      poll: (): Promise<RevisionPollResult<{ status: 'READY' }>> => new Promise((resolve) => { resolvePoll = resolve; }),
      hasPreparing: () => false,
      onValue,
      onError: vi.fn(),
    });

    poller.refresh();
    poller.stop();
    resolvePoll({ value: { status: 'READY' } });
    await flushMicrotasks();

    expect(onValue).not.toHaveBeenCalled();
  });

  it('uses a slower interval while hidden and refreshes when visible again', async () => {
    vi.useFakeTimers();
    let hidden = true;
    const poll = vi.fn().mockResolvedValue({ value: { status: 'READY' as const } });
    const poller = startRevisionPolling({
      poll,
      hasPreparing: () => false,
      onValue: vi.fn(),
      onError: vi.fn(),
      isHidden: () => hidden,
      hiddenDelayMs: 5000,
    });

    poller.refresh();
    expect(poll).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(4999);
    expect(poll).not.toHaveBeenCalled();

    hidden = false;
    poller.refresh();
    await flushMicrotasks();
    expect(poll).toHaveBeenCalledOnce();
    poller.stop();
  });
});
