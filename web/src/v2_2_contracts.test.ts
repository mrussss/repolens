import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { buildDiagnosisRequest } from './api';
import { getStableDiagnosisIdempotencyKey } from './diagnosisSubmission';
import { shouldContinueRevisionPolling } from './revisionPolling';
import { isInvalidReport } from './reportStatus';
import { getProviderTestAlert } from './providerCompatibility';

function storage(initial: Record<string, string> = {}) {
  const values = new Map(Object.entries(initial));
  return {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => values.set(key, value),
  };
}

describe('v2.2 API and UI contracts', () => {
  it('keeps the shared diagnosis request fixture on the revision payload path', () => {
    const fixture = JSON.parse(readFileSync(fileURLToPath(new URL('../../contracts/v2.2/diagnosis-create.request.json', import.meta.url)), 'utf8'));
    expect(fixture.analysis_revision_id).toBeTruthy();
    expect(fixture).not.toHaveProperty('code_index_build_id');
    expect(fixture).not.toHaveProperty('retrieval_build_id');
  });

  it('serializes only analysis_revision_id for the new diagnosis path', () => {
    const request = buildDiagnosisRequest({
      repository_id: 'repo-1',
      analysis_revision_id: 'revision-1',
      issue_title: 'bug',
      idempotency_key: 'key-1',
    });
    expect(request.payload).toEqual({ repository_id: 'repo-1', analysis_revision_id: 'revision-1', issue_title: 'bug' });
    expect(request.headers).toEqual({ 'Idempotency-Key': 'key-1' });
    expect(request.payload).not.toHaveProperty('code_index_build_id');
    expect(request.payload).not.toHaveProperty('retrieval_build_id');
  });

  it('reuses an idempotency key for the same failed submission', () => {
    const state = storage();
    const first = getStableDiagnosisIdempotencyKey('same-payload', state, () => 'key-1');
    const second = getStableDiagnosisIdempotencyKey('same-payload', state, () => 'key-2');
    expect(first).toBe('key-1');
    expect(second).toBe('key-1');
  });

  it('stops revision polling after the terminal state', () => {
    expect(shouldContinueRevisionPolling([{ status: 'PREPARING' } as any])).toBe(true);
    expect(shouldContinueRevisionPolling([{ status: 'READY' } as any])).toBe(false);
    expect(shouldContinueRevisionPolling([{ status: 'FAILED' } as any])).toBe(false);
  });

  it('does not treat an INVALID report as a green conclusion', () => {
    expect(isInvalidReport({ report_status: 'INVALID' } as any)).toBe(true);
    expect(isInvalidReport({ report_status: 'VALID' } as any)).toBe(false);
  });

  it('labels an observed first tool call without claiming a full round trip', () => {
    expect(getProviderTestAlert({
      success: true,
      latency_ms: 120,
      message: 'ok',
      compatibility: {
        probe_max_output_tokens: 256,
        production_max_output_tokens: 4096,
        reasoning_effort: 'low',
        response_format: 'json_object',
        tools: true,
        probe_status: 'CONFIRMED',
        tool_call_observed: true,
      },
    })).toEqual({
      className: 'alert-success',
      message: '✓ 已观测到目标工具调用（未验证完整工具往返），延迟 120ms',
    });
  });

  it('keeps providers without an observed tool call uncertain', () => {
    expect(getProviderTestAlert({
      success: true,
      latency_ms: 240,
      message: 'ok',
      compatibility: {
        probe_max_output_tokens: 256,
        production_max_output_tokens: 4096,
        reasoning_effort: 'low',
        response_format: 'json_object',
        tools: true,
        probe_status: 'UNCERTAIN',
        tool_call_observed: false,
      },
    })).toEqual({
      className: 'alert-warning',
      message: '✓ 连接成功，但工具调用能力尚未确认，延迟 240ms',
    });
  });
});
