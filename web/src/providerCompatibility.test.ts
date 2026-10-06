import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from './api';

afterEach(() => { vi.unstubAllGlobals(); });

describe('provider compatibility error metadata', () => {
  it('keeps a connection failure rejected while retaining safe warnings and latency', async () => {
    const compatibility = { production_timeout_seconds: 60, warnings: [{ code: 'CUSTOM_GENERATION_PROFILE', message: 'Advisory only.' }] };
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ success: false, code: 'PROVIDER_CAPABILITY_UNSUPPORTED', error: 'Unsupported request shape', latency_ms: 12, compatibility }), { status: 502 })));
    await expect(api.testProviderConnection({ base_url: 'http://localhost/v1', model: 'test', auth_mode: 'none' })).rejects.toMatchObject({ status: 502, code: 'PROVIDER_CAPABILITY_UNSUPPORTED', latency_ms: 12, compatibility });
  });
});
