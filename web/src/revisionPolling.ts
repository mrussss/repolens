import { AnalysisRevision } from './types';

export function shouldContinueRevisionPolling(revisions: AnalysisRevision[]): boolean {
  return revisions.some((revision) => revision.status === 'PREPARING');
}

export type RevisionRead<T> =
  | { repositoryId: string; revisions: T[] }
  | { repositoryId: string; failed: true };

export function mergeRevisionReads<T>(
  previous: Record<string, T[]>,
  reads: RevisionRead<T>[],
): Record<string, T[]> {
  return Object.fromEntries(reads.map((read) => [
    read.repositoryId,
    'failed' in read ? previous[read.repositoryId] ?? [] : read.revisions,
  ]));
}

export interface RevisionPollResult<T> {
  value: T;
  error?: string;
}

export interface RevisionPollingOptions<T> {
  poll: () => Promise<RevisionPollResult<T>>;
  hasPreparing: (value: T) => boolean;
  onValue: (value: T) => void;
  onError: (error: string | null) => void;
  onPollStart?: () => void;
  onPollEnd?: () => void;
  isHidden?: () => boolean;
  baseDelayMs?: number;
  maxDelayMs?: number;
  hiddenDelayMs?: number;
}

export interface RevisionPoller {
  refresh: () => void;
  stop: () => void;
}

export function startRevisionPolling<T>(options: RevisionPollingOptions<T>): RevisionPoller {
  const baseDelayMs = options.baseDelayMs ?? 1000;
  const maxDelayMs = options.maxDelayMs ?? 5000;
  const hiddenDelayMs = options.hiddenDelayMs ?? 5000;
  const isHidden = options.isHidden ?? (() => typeof document !== 'undefined' && document.hidden);

  let stopped = false;
  let inFlight = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let delayMs = baseDelayMs;
  let hasPreparing = false;
  let refreshPending = false;

  const clearTimer = () => {
    if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }
  };

  const schedule = (delay: number) => {
    if (stopped || timer !== undefined) return;
    timer = setTimeout(() => {
      timer = undefined;
      void poll();
    }, delay);
  };

  const scheduleNext = () => {
    const nextDelay = delayMs;
    delayMs = Math.min(maxDelayMs, delayMs * 2);
    schedule(isHidden() ? hiddenDelayMs : nextDelay);
  };

  const poll = async () => {
    if (stopped || inFlight) return;
    if (isHidden()) {
      schedule(hiddenDelayMs);
      return;
    }

    inFlight = true;
    options.onPollStart?.();
    try {
      const result = await options.poll();
      if (stopped) return;
      options.onValue(result.value);
      options.onError(result.error ?? null);
      hasPreparing = options.hasPreparing(result.value);
      if (hasPreparing) scheduleNext();
    } catch (error) {
      if (stopped) return;
      options.onError(error instanceof Error && error.message ? error.message : '刷新分析版本失败，请检查网络后重新加载。');
      if (hasPreparing) scheduleNext();
    } finally {
      inFlight = false;
      if (!stopped) {
        options.onPollEnd?.();
        if (refreshPending) {
          refreshPending = false;
          clearTimer();
          void poll();
        }
      }
    }
  };

  return {
    refresh: () => {
      if (stopped) return;
      if (inFlight) {
        refreshPending = true;
        return;
      }
      clearTimer();
      void poll();
    },
    stop: () => {
      stopped = true;
      clearTimer();
    },
  };
}
