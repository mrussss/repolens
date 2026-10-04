import { DiagnosisAttempt, DiagnosisRun } from './types';

// A run is only a container for traces: always resolve a concrete attempt.
export function currentTraceAttempt(run: DiagnosisRun, attempts: DiagnosisAttempt[]): string | undefined {
  if (run.status === 'RUNNING' || run.status === 'QUEUED') {
    return attempts.filter((attempt) => attempt.diagnosis_run_id === run.id && attempt.status === 'RUNNING'
      && attempt.execution_generation === (run.execution_generation ?? 1))
      .sort((a, b) => b.attempt_no - a.attempt_no)[0]?.id;
  }
  if (run.final_attempt_id) return run.final_attempt_id;
  if (run.status === 'CANCELLED') {
    return attempts.filter((attempt) => attempt.diagnosis_run_id === run.id
      && attempt.execution_generation === (run.execution_generation ?? 1))
      .sort((a, b) => b.attempt_no - a.attempt_no)[0]?.id;
  }
  return undefined;
}
