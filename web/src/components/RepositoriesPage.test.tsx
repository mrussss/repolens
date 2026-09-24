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
    vi.spyOn(api, 'listRepositories').mockResolvedValue([repository]);
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
});
