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

  it('retries a transient diagnosis-status failure instead of stopping the poll loop', async () => {
    let reads = 0;
    vi.mocked(api.getDiagnosis).mockImplementation(async () => {
      reads += 1;
      if (reads === 1) throw new Error('temporary network failure');
      return runningRun;
    });

    await act(async () => {
      root.render(<DiagnosisView diagnosisId={failedRun.id} onBack={() => undefined} />);
      await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
    });
    expect(container.textContent).toContain('temporary network failure');
    await act(async () => { await vi.advanceTimersByTimeAsync(2100); });
    expect(vi.mocked(api.getDiagnosis).mock.calls.length).toBeGreaterThanOrEqual(2);
    expect(container.textContent).toContain('运行中');
    expect(container.textContent).not.toContain('temporary network failure');
  });

  it('keeps retrying terminal report and steps reads after transient server errors', async () => {
    const succeeded = { ...succeededGenerationTwo, status: 'SUCCEEDED' } as DiagnosisRun;
    vi.mocked(api.getDiagnosis).mockResolvedValue(succeeded);
    let reportReads = 0;
    vi.mocked(api.getDiagnosisReport).mockImplementation(async () => {
      reportReads += 1;
      if (reportReads === 1) throw Object.assign(new Error('report temporarily unavailable'), { status: 503 });
      return { report_status: 'VALID', summary: 'recovered terminal report', findings: [] } as any;
    });
    let stepReads = 0;
    vi.mocked(api.getDiagnosisSteps).mockImplementation(async () => {
      stepReads += 1;
      if (stepReads === 1) throw Object.assign(new Error('trace temporarily unavailable'), { status: 503 });
      return [];
    });

    await act(async () => {
      root.render(<DiagnosisView diagnosisId={failedRun.id} onBack={() => undefined} />);
      await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
    });
    expect(container.textContent).toContain('trace temporarily unavailable');
    expect(vi.mocked(api.getDiagnosisSteps)).toHaveBeenCalledOnce();
    await act(async () => { await vi.advanceTimersByTimeAsync(2100); });
    expect(reportReads).toBeGreaterThanOrEqual(2);
    expect(stepReads).toBeGreaterThanOrEqual(2);
    expect(container.textContent).toContain('recovered terminal report');
    expect(container.textContent).not.toContain('temporarily unavailable');
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
