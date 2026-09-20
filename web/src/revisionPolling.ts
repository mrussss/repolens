import { AnalysisRevision } from './types';

export function shouldContinueRevisionPolling(revisions: AnalysisRevision[]): boolean {
  return revisions.some((revision) => revision.status === 'PREPARING');
}
