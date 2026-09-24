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
  visibilityChanged: () => void;
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
  let hasSuccessfulPoll = false;
  let terminal = false;
  let refreshPending = false;
  let visibilityRefreshPending = false;

  const clearTimer = () => {
    if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }
  };

  const schedule = (delay: number) => {
    if (stopped || terminal || timer !== undefined) return;
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

  const poll = async (force = false) => {
    if (stopped || inFlight || (terminal && !force)) return;
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
      hasSuccessfulPoll = true;
      hasPreparing = options.hasPreparing(result.value);
      terminal = !hasPreparing;
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
          terminal = false;
          clearTimer();
          void poll(true);
        } else if (visibilityRefreshPending) {
          visibilityRefreshPending = false;
          if (!terminal && (hasPreparing || !hasSuccessfulPoll)) {
            clearTimer();
            void poll();
          }
        }
      }
    }
  };

  return {
    refresh: () => {
      if (stopped) return;
      terminal = false;
      if (inFlight) {
        refreshPending = true;
        return;
      }
      clearTimer();
      void poll(true);
    },
    visibilityChanged: () => {
      if (stopped || terminal) return;
      if (inFlight) {
        visibilityRefreshPending = true;
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
