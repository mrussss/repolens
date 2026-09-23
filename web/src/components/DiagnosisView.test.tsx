// @vitest-environment jsdom
import { act } from 'react';
import { createRoot, Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { DiagnosisRun } from '../types';
import { DiagnosisView } from './DiagnosisView';

Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });

const failedRun = {
  id: 'diagnosis-retry', issue_title: 'retry test', status: 'FAILED', final_attempt_id: 'attempt-1',
  retry_allowed: true, retry_error_code: 'PROVIDER_TIMEOUT',
} as DiagnosisRun;
const queuedRun = { ...failedRun, status: 'QUEUED', final_attempt_id: undefined, retry_allowed: false } as DiagnosisRun;
const runningRun = { ...queuedRun, status: 'RUNNING', execution_generation: 2 } as DiagnosisRun;
const succeededGenerationTwo = { ...runningRun, status: 'SUCCEEDED', final_attempt_id: 'attempt-gen2' } as DiagnosisRun;

describe('DiagnosisView retry polling', () => {
  let container: HTMLDivElement;
  let root: Root;
  let currentRun: DiagnosisRun;

  beforeEach(() => {
    vi.useFakeTimers();
    currentRun = failedRun;
    container = document.createElement('div');
    document.body.appendChild(container);
    root = createRoot(container);
    vi.spyOn(api, 'getDiagnosis').mockImplementation(async () => currentRun);
    vi.spyOn(api, 'getDiagnosisAttempts').mockResolvedValue([]);
    vi.spyOn(api, 'getDiagnosisReport').mockResolvedValue({ report_status: 'INVALID', parse_error: 'INVALID_STRUCTURED_REPORT: UNKNOWN_FIELD', raw_output: 'secret-test-marker raw output', findings: [] } as any);
    vi.spyOn(api, 'getDiagnosisSteps').mockResolvedValue([]);
    vi.spyOn(api, 'retryDiagnosis').mockImplementation(async () => { currentRun = queuedRun; return { message: 'queued' }; });
  });

  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('clears terminal report and resumes polling after a retry', async () => {
    await act(async () => {
      root.render(<DiagnosisView diagnosisId={failedRun.id} onBack={() => undefined} />);
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(container.textContent).toContain('INVALID');
    expect(container.textContent).toContain('INVALID_STRUCTURED_REPORT: UNKNOWN_FIELD');
    expect(container.textContent).not.toContain('secret-test-marker');
    const retryButton = [...container.querySelectorAll('button')].find((button) => button.textContent?.includes('重试诊断'));
    expect(retryButton).toBeTruthy();

    await act(async () => {
      retryButton!.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(api.retryDiagnosis).toHaveBeenCalledOnce();
    expect(container.textContent).not.toContain('INVALID');
    expect(container.textContent).toContain('排队中');
    const callsBeforeTimer = vi.mocked(api.getDiagnosis).mock.calls.length;
    await act(async () => { await vi.advanceTimersByTimeAsync(2100); });
    expect(vi.mocked(api.getDiagnosis).mock.calls.length).toBeGreaterThan(callsBeforeTimer);
  });

  it('shows a provider failure without a report and hides retry when disallowed', async () => {
    currentRun = { ...failedRun, retry_allowed: false, retry_reason: '本次失败不可安全重试' } as DiagnosisRun;
    vi.mocked(api.getDiagnosisReport).mockRejectedValue(new Error('report not found'));
    await act(async () => {
      root.render(<DiagnosisView diagnosisId={failedRun.id} onBack={() => undefined} />);
      await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
    });
    expect(container.textContent).toContain('本次执行未生成可展示的诊断报告');
    expect(container.textContent).toContain('PROVIDER_TIMEOUT');
    expect(container.textContent).toContain('本次失败不可安全重试');
    expect([...container.querySelectorAll('button')].some((button) => button.textContent?.includes('重试诊断'))).toBe(false);
  });

  it('replaces the previous generation report with the terminal retry result', async () => {
    let queuedReads = 0;
    let runningReads = 0;
    vi.mocked(api.getDiagnosis).mockImplementation(async () => {
      if (currentRun.status === 'QUEUED' && ++queuedReads > 2) currentRun = runningRun;
      else if (currentRun.status === 'RUNNING' && ++runningReads > 0) currentRun = succeededGenerationTwo;
      return currentRun;
    });
    vi.mocked(api.getDiagnosisReport)
      .mockResolvedValueOnce({ report_status: 'INVALID', summary: 'generation one output', findings: [] } as any)
      .mockResolvedValueOnce({ report_status: 'VALID', summary: 'generation two output', findings: [] } as any);
    await act(async () => {
      root.render(<DiagnosisView diagnosisId={failedRun.id} onBack={() => undefined} />);
      await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
    });
    expect(container.textContent).toContain('generation one output');
    const retryButton = [...container.querySelectorAll('button')].find((button) => button.textContent?.includes('重试诊断'))!;
    await act(async () => {
      retryButton.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      await Promise.resolve(); await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
    });
    expect(container.textContent).not.toContain('generation one output');
    await act(async () => { await vi.advanceTimersByTimeAsync(2100); });
    await act(async () => { await vi.advanceTimersByTimeAsync(4100); });
    expect(container.textContent).toContain('generation two output');
    expect(container.textContent).not.toContain('generation one output');
  });
});
