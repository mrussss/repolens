// @vitest-environment jsdom
import { act } from 'react';
import { createRoot, Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, ProviderCompatibility } from '../api';
import { SetupPage } from './SetupPage';

Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });

const compatibility: ProviderCompatibility = {
  probe_max_output_tokens: 256, production_max_output_tokens: 4096,
  production_timeout_seconds: 60, reasoning_effort: 'medium',
  response_format: 'json_object', tools: true, probe_status: 'CONFIRMED', tool_call_observed: true,
};

describe('SetupPage generation warnings', () => {
  let container: HTMLDivElement;
  let root: Root;

  beforeEach(() => {
    container = document.createElement('div'); document.body.appendChild(container);
    root = createRoot(container);
    vi.spyOn(api, 'getProviderStatus').mockResolvedValue({ is_configured: true, base_url: 'http://localhost:8000/v1', model: 'test', auth_mode: 'none', is_demo: false } as any);
  });
  afterEach(async () => { await act(async () => root.unmount()); container.remove(); vi.restoreAllMocks(); });

  async function testConnection() {
    await act(async () => { root.render(<SetupPage onDemoStarted={() => {}} onConfigSaved={() => {}} />); });
    const button = Array.from(container.querySelectorAll('button')).find(b => b.textContent?.includes('测试连接'))!;
    await act(async () => button.click());
  }

  it('shows warnings while keeping a confirmed connection successful', async () => {
    const test = vi.spyOn(api, 'testProviderConnection').mockResolvedValue({ success: true, latency_ms: 20, message: 'ok', compatibility: { ...compatibility, warnings: [{ code: 'HIGHER_REASONING_BUDGET_RISK', message: 'Higher reasoning may exhaust the budget.' }, { code: 'CUSTOM_GENERATION_PROFILE', message: 'Custom profile is not a full diagnosis guarantee.' }] } });
    await testConnection();
    expect(container.querySelector('.alert-success')?.textContent).toContain('已观测到目标工具调用');
    expect(container.querySelector('[aria-label="Generation configuration warnings"]')?.textContent).toContain('Higher reasoning may exhaust the budget.');
    expect(container.querySelectorAll('[aria-label="Generation configuration warnings"] li')).toHaveLength(2);
    expect(container.querySelector('.alert-danger')).toBeNull();
    expect(test).toHaveBeenCalledTimes(1);
    expect(test.mock.calls[0][0]).not.toHaveProperty('reasoning_effort');
  });

  it.each([undefined, []])('adds no warning UI for warnings=%s', async (warnings) => {
    vi.spyOn(api, 'testProviderConnection').mockResolvedValue({ success: true, latency_ms: 20, message: 'ok', compatibility: { ...compatibility, warnings } });
    await testConnection();
    expect(container.querySelector('[aria-label="Generation configuration warnings"]')).toBeNull();
    expect(container.querySelector('.alert-success')).not.toBeNull();
    expect(container.querySelector('.alert-danger')).toBeNull();
  });

  it('preserves failure status while showing advisory warnings from the error response', async () => {
    vi.spyOn(api, 'testProviderConnection').mockRejectedValue(Object.assign(new Error('Capability unsupported'), { code: 'PROVIDER_CAPABILITY_UNSUPPORTED', latency_ms: 5, compatibility: { ...compatibility, probe_status: 'UNCERTAIN', tool_call_observed: false, warnings: [{ code: 'CUSTOM_GENERATION_PROFILE', message: 'Custom profile warning.' }] } }));
    await testConnection();
    expect(container.querySelector('.alert-danger')?.textContent).toContain('PROVIDER_CAPABILITY_UNSUPPORTED');
    expect(container.querySelector('.alert-success')).toBeNull();
    expect(container.querySelector('[aria-label="Generation configuration warnings"]')?.textContent).toContain('Custom profile warning.');
  });
});
