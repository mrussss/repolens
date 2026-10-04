import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from './api';

afterEach(() => { vi.unstubAllGlobals(); });

describe('paginated API and attempt identity', () => {
  it('preserves all list metadata and sends the requested page', async () => {
    const fetch = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ repositories: [{ id: 'repo-21' }], total: 25, page: 2, page_size: 20 })))
      .mockResolvedValueOnce(new Response(JSON.stringify({ diagnosis_runs: [{ id: 'diag-21' }], total: 25, page: 2, page_size: 20 })));
    vi.stubGlobal('fetch', fetch);
    expect(await api.listRepositories(2, 20)).toEqual({ items: [{ id: 'repo-21' }], total: 25, page: 2, page_size: 20 });
    expect(await api.listDiagnoses(2, 20)).toEqual({ items: [{ id: 'diag-21' }], total: 25, page: 2, page_size: 20 });
    expect(fetch.mock.calls.map(([url]) => url)).toEqual(['/api/v1/repositories?page=2&page_size=20', '/api/v1/diagnoses?page=2&page_size=20']);
  });

  it('makes repositories beyond page one available to all repository selectors', async () => {
    const all = Array.from({ length: 25 }, (_, i) => ({ id: `repo-${i}` }));
    const fetch = vi.fn().mockImplementation(async (url: string) => {
      const page = Number(new URL(url, 'http://localhost').searchParams.get('page'));
      return new Response(JSON.stringify({ repositories: all.slice((page - 1) * 20, page * 20), total: 25, page, page_size: 20 }));
    });
    vi.stubGlobal('fetch', fetch);
    expect(await api.listAllRepositories()).toEqual(all);
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it('encodes the specific attempt ID in trace requests', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({ steps: [{ id: 'step-1' }] })));
    vi.stubGlobal('fetch', fetch);
    expect(await api.getDiagnosisSteps('diagnosis', 'attempt / 2')).toEqual([{ id: 'step-1' }]);
    expect(fetch).toHaveBeenCalledWith('/api/v1/diagnoses/diagnosis/steps?attempt_id=attempt%20%2F%202');
  });
});
