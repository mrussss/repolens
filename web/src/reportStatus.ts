import { DiagnosisReport } from './types';

export function isInvalidReport(report: DiagnosisReport | null): boolean {
  return report?.report_status === 'INVALID';
}
