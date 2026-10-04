// @vitest-environment jsdom
import { act } from 'react';
import { createRoot, Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { AnalysisRevision, Repository } from '../types';
import { RepositoriesPage } from './RepositoriesPage';

Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });

const repository: Repository = {
  id: 'repo-1',
  user_id: 'user-1',
  name: 'service',
  git_url: 'https://example.com/service.git',
  default_ref: 'main',
  status: 'ACTIVE',
  created_at: '2026-01-01T00:00:00Z',
};

const preparingRevision: AnalysisRevision = {
  id: 'revision-1', repository_id: repository.id, source_ref: 'main',
  commit_sha: '0123456789012345678901234567890123456789', pipeline_version: 'v2.2',
  pipeline_fingerprint: 'fingerprint', status: 'PREPARING', stage: 'BUILDING_CODE_INDEX',
  execution_generation: 1, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
};

const readyRevision: AnalysisRevision = {
  ...preparingRevision, status: 'READY', stage: 'READY', retrieval_build_id: 1,
};

describe('RepositoriesPage AnalysisRevision polling', () => {
  let container: HTMLDivElement;
  let root: Root;

  beforeEach(() => {
    vi.useFakeTimers();
    container = document.createElement('div');
    document.body.appendChild(container);
    root = createRoot(container);
    vi.spyOn(api, 'listRepositories').mockResolvedValue({ items: [repository], total: 1, page: 1, page_size: 20 });
    vi.spyOn(api, 'listAnalysisRevisions').mockResolvedValue([readyRevision]);
    vi.spyOn(api, 'createRepository').mockResolvedValue(repository);
    vi.spyOn(api, 'createAnalysisRevision').mockResolvedValue({ analysis_revision: preparingRevision, created: true });
    vi.spyOn(api, 'retryAnalysisRevision').mockResolvedValue(preparingRevision);
  });

  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  async function mountAndFlush() {
    await act(async () => {
      root.render(<RepositoriesPage onSelectRepoForDiagnosis={() => undefined} />);
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });
  }

  it('keeps PREPARING through a transient read failure and resumes until READY', async () => {
    let reads = 0;
    vi.mocked(api.listAnalysisRevisions).mockImplementation(async () => {
      reads++;
      if (reads === 1) return [preparingRevision];
      if (reads === 2) throw new Error('temporary network failure');
      return [readyRevision];
    });

    await mountAndFlush();
    expect(container.textContent).toContain('PREPARING');

    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(reads).toBe(2);
    expect(container.textContent).toContain('PREPARING');
    expect(container.textContent).toContain('已保留上次状态');

    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(reads).toBe(3);
    expect(container.textContent).toContain('READY');
    expect(container.textContent).not.toContain('已保留上次状态');
  });

  it('stops polling after a successfully read READY revision', async () => {
    await mountAndFlush();
    expect(api.listAnalysisRevisions).toHaveBeenCalledOnce();

    await act(async () => { await vi.advanceTimersByTimeAsync(30000); });
    expect(api.listAnalysisRevisions).toHaveBeenCalledOnce();
  });

  it('shows a reload action after the first transient failure and recovers on click', async () => {
    vi.mocked(api.listAnalysisRevisions)
      .mockRejectedValueOnce(new Error('temporary network failure'))
      .mockResolvedValueOnce([readyRevision]);

    await mountAndFlush();
    expect(container.textContent).toContain('分析版本刷新失败');
    const reloadButton = [...container.querySelectorAll('button')]
      .find((button) => button.textContent?.includes('重新加载'));
    expect(reloadButton).toBeTruthy();

    await act(async () => {
      reloadButton!.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(api.listAnalysisRevisions).toHaveBeenCalledTimes(2);
    expect(container.textContent).toContain('READY');
    expect(container.textContent).not.toContain('分析版本刷新失败');
  });
  it('opens all 25 repositories with 20 on page one and 5 on page two', async () => {
    const all = Array.from({ length: 25 }, (_, i) => ({ ...repository, id: `repo-${i}`, name: `repository-${i}` }));
    vi.mocked(api.listRepositories).mockImplementation(async (page = 1, pageSize = 20) => ({
      items: all.slice((page - 1) * pageSize, page * pageSize), total: 25, page, page_size: pageSize,
    }));
    await mountAndFlush();
    expect(container.querySelectorAll('h3')).toHaveLength(20);
    expect(container.textContent).toContain('共 25 条');
    const next = [...container.querySelectorAll('button')].find((button) => button.textContent === '下一页')!;
    await act(async () => { next.click(); });
    expect(api.listRepositories).toHaveBeenLastCalledWith(2, 20);
    expect(container.querySelectorAll('h3')).toHaveLength(5);
    expect(container.textContent).toContain('repository-24');
    expect(container.textContent).not.toContain('repository-0');
    expect(next.disabled).toBe(true);
    const previous = [...container.querySelectorAll('button')].find((button) => button.textContent === '上一页')!;
    await act(async () => { previous.click(); });
    expect(container.querySelectorAll('h3')).toHaveLength(20);
  });

  it('discards obsolete page requests and polls revisions only on the current page', async () => {
    let obsoleteResolve!: (value: Awaited<ReturnType<typeof api.listRepositories>>) => void;
    let pageOneReads = 0;
    const second = { ...repository, id: 'repo-21', name: 'second-page' };
    vi.mocked(api.listRepositories).mockImplementation(async (page = 1) => {
      if (page === 1 && ++pageOneReads > 1) return new Promise((resolve) => { obsoleteResolve = resolve; });
      return { items: page === 1 ? [repository] : [second], total: 25, page, page_size: 20 };
    });
    let secondReads = 0;
    vi.mocked(api.listAnalysisRevisions).mockImplementation(async (id) => {
      if (id === second.id && ++secondReads > 1) return [{ ...readyRevision, repository_id: second.id }];
      return [{ ...preparingRevision, repository_id: id }];
    });
    await mountAndFlush();
    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(obsoleteResolve).toBeTypeOf('function');
    const next = [...container.querySelectorAll('button')].find((button) => button.textContent === '下一页')!;
    await act(async () => { next.click(); });
    expect(container.textContent).toContain('second-page');
    const oldReads = vi.mocked(api.listAnalysisRevisions).mock.calls.filter(([id]) => id === repository.id).length;
    await act(async () => {
      obsoleteResolve({ items: [repository], total: 25, page: 1, page_size: 20 });
      await vi.advanceTimersByTimeAsync(1000);
    });
    expect(container.textContent).toContain('second-page');
    expect(container.textContent).toContain('READY');
    expect(container.textContent).toContain('第 2 / 2 页');
    expect(vi.mocked(api.listAnalysisRevisions).mock.calls.filter(([id]) => id === repository.id)).toHaveLength(oldReads);
    expect(api.listRepositories).toHaveBeenLastCalledWith(2, 20);
    expect(secondReads).toBe(2);
  });

  it('does not apply an old page revision response after switching pages', async () => {
    const second = { ...repository, id: 'repo-21', name: 'second-page' };
    let resolveOld!: (value: AnalysisRevision[]) => void;
    let oldReads = 0;
    vi.mocked(api.listRepositories).mockImplementation(async (page = 1) => ({
      items: page === 1 ? [repository] : [second], total: 25, page, page_size: 20,
    }));
    vi.mocked(api.listAnalysisRevisions).mockImplementation(async (id) => {
      if (id === second.id) return [{ ...readyRevision, repository_id: id }];
      if (++oldReads > 1) return new Promise((resolve) => { resolveOld = resolve; });
      return [preparingRevision];
    });
    await mountAndFlush();
    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    await act(async () => { [...container.querySelectorAll('button')].find((button) => button.textContent === '下一页')!.click(); });
    await act(async () => { resolveOld([preparingRevision]); });
    expect(container.textContent).toContain('second-page');
    expect(container.textContent).toContain('READY');
    expect(container.textContent).not.toContain('PREPARING');
    expect(container.textContent).toContain('第 2 / 2 页');
    await act(async () => { await vi.advanceTimersByTimeAsync(30000); });
    expect(oldReads).toBe(2);
  });

});
