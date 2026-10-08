// @vitest-environment jsdom
import { act } from 'react';
import { createRoot, Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { CodeIndexBuild, CodeSymbol, QualityReport, Repository, RetrievalBuild, Snapshot, SymbolRelation } from '../types';
import { CodeIntelPage } from './CodeIntelPage';

Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: Error) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}
const snapshot = (id: string): Snapshot => ({ id, repository_id: 'repo-a', commit_sha: 'aaaaaaaa', ref: 'main', materialized_path: '', status: 'READY', created_at: '' });
const repoA: Repository = { id: 'repo-a', user_id: 'user', name: 'A', git_url: '', default_ref: 'main', status: 'ACTIVE', created_at: '', snapshots: [snapshot('snap-a'), snapshot('snap-a2')] };
const repoB: Repository = { ...repoA, id: 'repo-b', name: 'B', snapshots: [] };
const build = (id: number, status: CodeIndexBuild['status'] = 'READY') => ({ id, status } as CodeIndexBuild);
const retrieval = (id: number, status: RetrievalBuild['status'] = 'READY') => ({ id, status } as RetrievalBuild);
const symbol = (id: number, name: string) => ({ id, name, symbol_key_hash: `hash-${id}`, file_path: 'main.go', start_line: 2, end_line: 2, package_path: 'a', kind: 'FUNCTION', signature: `func ${name}()` } as CodeSymbol);
const symbolA = symbol(1, 'OnlyInRepoA');
const symbolB = symbol(2, 'SecondSymbol');
const relation = (id: number, name: string): SymbolRelation => ({ id, code_index_build_id: 11, target_name: name, relation_type: 'REFERENCE', resolution_kind: 'SEMANTIC', confidence: 1, reason_code: 'SEMANTIC', file_path: 'main.go', line: 2, column: 1 });

describe('CodeIntelPage context isolation', () => {
  let container: HTMLDivElement;
  let root: Root;
  beforeEach(() => {
    vi.useFakeTimers();
    container = document.createElement('div'); document.body.appendChild(container); root = createRoot(container);
    vi.spyOn(api, 'listAllRepositories').mockResolvedValue([repoA, repoB]);
    vi.spyOn(api, 'triggerCodeIndexBuild').mockResolvedValue({ code_index_build: build(11), status: 'READY' });
    vi.spyOn(api, 'triggerRetrievalBuild').mockResolvedValue({ retrieval_build: retrieval(22), status: 'READY' });
    vi.spyOn(api, 'getCodeIndexBuild').mockResolvedValue(build(11));
    vi.spyOn(api, 'getRetrievalBuild').mockResolvedValue(retrieval(22));
    vi.spyOn(api, 'getBuildQuality').mockResolvedValue({ code_index_build_id: 11, status: 'READY', parsed_pct: '100%', semantic_relation_count: 0, syntactic_relation_count: 0 } as QualityReport);
    vi.spyOn(api, 'listSymbols').mockResolvedValue({ symbols: [symbolA, symbolB], total: 2 });
    vi.spyOn(api, 'getSymbolReferences').mockImplementation(async (id) => ({ relations: [relation(id, `Ref-${id}`)], total: 1 }));
    vi.spyOn(api, 'getSymbolRelatedTests').mockImplementation(async (id) => ({ related_tests: [relation(id + 10, `Test-${id}`)], total: 1 }));
  });
  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove(); vi.restoreAllMocks(); vi.useRealTimers();
  });
  async function change(index: number, value: string) {
    await act(async () => {
      const select = container.querySelectorAll('select')[index];
      select.value = value; select.dispatchEvent(new Event('change', { bubbles: true }));
    });
  }
  function loadButton() { return [...container.querySelectorAll('button')].find(b => b.textContent?.includes('加载 / 刷新'))!; }
  async function load() { await act(async () => { loadButton().click(); }); }
  async function mount() { await act(async () => { root.render(<CodeIntelPage />); }); await change(1, 'snap-a'); }
  async function selectSymbol(name: string) {
    await act(async () => {
      const label = [...container.querySelectorAll('span')].find(span => span.textContent === name)!;
      label.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });
  }
  function expectCleared() {
    for (const old of ['OnlyInRepoA', '构建 #11', '100%', 'Ref-1', 'Test-1']) expect(container.textContent).not.toContain(old);
  }

  it.each(['repo', 'snapshot'])('clears every downstream value immediately on %s changes', async (kind) => {
    await mount(); await load();
    expect(container.textContent).toContain('OnlyInRepoA');
    expect(container.textContent).toContain('Ref-1');
    expect(container.textContent).toContain('Test-1');
    await change(kind === 'repo' ? 0 : 1, kind === 'repo' ? 'repo-b' : 'snap-a2');
    expectCleared();
    expect(container.querySelector('button[type="submit"]')?.hasAttribute('disabled')).toBe(true);
  });

  it('drops an in-flight build trigger after switching repositories', async () => {
    const pending = deferred<Awaited<ReturnType<typeof api.triggerCodeIndexBuild>>>();
    vi.mocked(api.triggerCodeIndexBuild).mockReturnValueOnce(pending.promise);
    await mount(); await load(); await change(0, 'repo-b');
    await act(async () => { pending.resolve({ code_index_build: build(11), status: 'READY' }); });
    expectCleared();
    expect(api.triggerRetrievalBuild).not.toHaveBeenCalled();
    expect(api.getBuildQuality).not.toHaveBeenCalled();
  });

  it('drops late build details across repositories', async () => {
    const pending = deferred<QualityReport>();
    vi.mocked(api.getBuildQuality).mockReturnValueOnce(pending.promise);
    await mount(); await load(); await change(0, 'repo-b');
    await act(async () => { pending.resolve({ code_index_build_id: 11, status: 'READY', parsed_pct: '100%' } as QualityReport); });
    expectCleared();
    expect(api.getSymbolReferences).not.toHaveBeenCalled();
  });

  it('keeps the newer snapshot details when older details arrive last', async () => {
    const pending = deferred<QualityReport>();
    vi.mocked(api.getBuildQuality).mockReturnValueOnce(pending.promise).mockResolvedValueOnce({ code_index_build_id: 33, status: 'READY', semantic_relation_count: 0, syntactic_relation_count: 0 } as QualityReport);
    vi.mocked(api.triggerCodeIndexBuild).mockResolvedValueOnce({ code_index_build: build(11), status: 'READY' }).mockResolvedValueOnce({ code_index_build: build(33), status: 'READY' });
    vi.mocked(api.listSymbols).mockResolvedValueOnce({ symbols: [symbolA], total: 1 }).mockResolvedValueOnce({ symbols: [symbol(3, 'NewSnapshot')], total: 1 });
    await mount(); await load(); await change(1, 'snap-a2'); await load();
    await act(async () => { pending.resolve({ code_index_build_id: 11, status: 'READY', parsed_pct: '100%' } as QualityReport); });
    expect(container.textContent).toContain('NewSnapshot');
    expect(container.textContent).toContain('构建 #33');
    expectCleared();
  });

  it('keeps symbol B relations when symbol A succeeds late', async () => {
    const refs = deferred<Awaited<ReturnType<typeof api.getSymbolReferences>>>();
    const tests = deferred<Awaited<ReturnType<typeof api.getSymbolRelatedTests>>>();
    vi.mocked(api.getSymbolReferences).mockReturnValueOnce(refs.promise);
    vi.mocked(api.getSymbolRelatedTests).mockReturnValueOnce(tests.promise);
    await mount(); await load(); await selectSymbol('SecondSymbol');
    expect(container.textContent).toContain('Ref-2');
    expect(container.textContent).toContain('Test-2');
    await act(async () => { refs.resolve({ relations: [relation(1, 'Ref-1')], total: 1 }); tests.resolve({ related_tests: [relation(11, 'Test-1')], total: 1 }); });
    expect([...container.querySelectorAll('h2')].some(h => h.textContent === 'SecondSymbol')).toBe(true);
    expect(container.textContent).toContain('Ref-2');
    expect(container.textContent).toContain('Test-2');
    expect(container.textContent).not.toContain('Ref-1');
    expect(container.textContent).not.toContain('Test-1');
  });

  it('does not let stale errors or finally clear a newer symbol loading state', async () => {
    const old = deferred<Awaited<ReturnType<typeof api.getSymbolReferences>>>();
    const next = deferred<Awaited<ReturnType<typeof api.getSymbolReferences>>>();
    vi.mocked(api.getSymbolReferences).mockReturnValueOnce(old.promise).mockReturnValueOnce(next.promise);
    await mount(); await load(); await selectSymbol('SecondSymbol');
    await act(async () => { old.reject(new Error('obsolete symbol error')); });
    expect(container.textContent).not.toContain('obsolete symbol error');
    expect(container.textContent).not.toContain('Ref-2');
    expect(container.querySelectorAll('.spin')).toHaveLength(1);
    await act(async () => { next.resolve({ relations: [relation(2, 'Ref-2')], total: 1 }); });
    expect(container.textContent).toContain('Ref-2');
    expect(container.querySelectorAll('.spin')).toHaveLength(0);
  });

  it.each(['code', 'retrieval'])('stops %s polling when its snapshot is obsolete', async (kind) => {
    if (kind === 'code') {
      vi.mocked(api.triggerCodeIndexBuild).mockResolvedValueOnce({ code_index_build: build(11, 'BUILDING'), status: 'READY' });
      vi.mocked(api.getCodeIndexBuild).mockResolvedValue(build(11, 'BUILDING'));
    } else {
      vi.mocked(api.triggerRetrievalBuild).mockResolvedValueOnce({ retrieval_build: retrieval(22, 'BUILDING'), status: 'READY' });
      vi.mocked(api.getRetrievalBuild).mockResolvedValue(retrieval(22, 'BUILDING'));
    }
    await mount(); await load(); await change(1, 'snap-a2');
    await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
    expect(kind === 'code' ? api.getCodeIndexBuild : api.getRetrievalBuild).toHaveBeenCalledTimes(1);
    expectCleared();
    expect(container.textContent).not.toContain('stale');
    expect(loadButton().disabled).toBe(false);
  });

  it.each(['success', 'error'])('drops stale search %s without clearing a newer build loading state', async (outcome) => {
    await mount(); await load();
    const pendingSearch = deferred<Awaited<ReturnType<typeof api.listSymbols>>>();
    const pendingBuild = deferred<Awaited<ReturnType<typeof api.triggerCodeIndexBuild>>>();
    vi.mocked(api.listSymbols).mockReturnValueOnce(pendingSearch.promise);
    await act(async () => { container.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); });
    await change(1, 'snap-a2');
    vi.mocked(api.triggerCodeIndexBuild).mockReturnValueOnce(pendingBuild.promise);
    await load();
    await act(async () => {
      if (outcome === 'error') pendingSearch.reject(new Error('obsolete search error'));
      else pendingSearch.resolve({ symbols: [symbolA], total: 1 });
    });
    expect(loadButton().disabled).toBe(true);
    expect(container.textContent).not.toContain('obsolete search error');
    expectCleared();
    await act(async () => { pendingBuild.resolve({ code_index_build: build(33), status: 'READY' }); });
    expect(loadButton().disabled).toBe(false);
  });
});
