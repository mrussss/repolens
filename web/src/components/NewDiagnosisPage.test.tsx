// @vitest-environment jsdom
import { act } from 'react';
import { webcrypto } from 'node:crypto';
import { createRoot, Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { getStableDiagnosisIdempotencyKey } from '../diagnosisSubmission';
import { AnalysisRevision } from '../types';
import { NewDiagnosisPage } from './NewDiagnosisPage';

Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });

function readyRevision(id: string, repositoryID: string): AnalysisRevision {
  return {
    id, repository_id: repositoryID, source_ref: 'main', commit_sha: 'a'.repeat(40),
    pipeline_version: 'v2.2', pipeline_fingerprint: 'fingerprint', status: 'READY', stage: 'READY',
    execution_generation: 1, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
  };
}

function setInputValue(input: HTMLInputElement | HTMLSelectElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(input), 'value')?.set;
  setter?.call(input, value);
  input.dispatchEvent(new Event(input instanceof HTMLSelectElement ? 'change' : 'input', { bubbles: true }));
}

describe('NewDiagnosisPage submission and revision ownership', () => {
  let container: HTMLDivElement;
  let root: Root;
  let originalCrypto: Crypto;

  beforeEach(() => {
    sessionStorage.clear();
    originalCrypto = window.crypto;
    Object.defineProperty(window, 'crypto', { configurable: true, value: webcrypto });
    container = document.createElement('div');
    document.body.appendChild(container);
    root = createRoot(container);
  });

  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    vi.restoreAllMocks();
    sessionStorage.clear();
    Object.defineProperty(window, 'crypto', { configurable: true, value: originalCrypto });
  });

  it('submits through the native Crypto receiver and clears the successful idempotency key', async () => {
    vi.spyOn(api, 'listAllRepositories').mockResolvedValue([{ id: 'repo-a', name: 'A', git_url: 'https://example.com/a' } as any]);
    vi.spyOn(api, 'listAnalysisRevisions').mockResolvedValue([readyRevision('revision-a', 'repo-a')]);
    const create = vi.spyOn(api, 'createDiagnosis').mockResolvedValue({ diagnosis_run: { id: 'diagnosis-a' } } as any);
    const created = vi.fn();

    // Web Crypto's method requires its Crypto receiver; passing it directly is the audited failure.
    expect(() => getStableDiagnosisIdempotencyKey('payload', sessionStorage, webcrypto.randomUUID)).toThrow();

    await act(async () => {
      root.render(<NewDiagnosisPage initialRepoId="repo-a" initialRevisionId="revision-a" onDiagnosisCreated={created} />);
      await Promise.resolve();
      await Promise.resolve();
    });
    const title = container.querySelector<HTMLInputElement>('input[type="text"]')!;
    await act(async () => setInputValue(title, 'issue'));
    await act(async () => {
      container.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(create).toHaveBeenCalledOnce();
    expect(create.mock.calls[0][0].idempotency_key).toMatch(/^[0-9a-f-]{36}$/i);
    expect(created).toHaveBeenCalledWith('diagnosis-a');
    expect(sessionStorage.getItem('repolens-diagnosis-payload')).toBeNull();
    expect(sessionStorage.getItem('repolens-diagnosis-key')).toBeNull();
  });

  it('reuses a key for the same payload and creates a new key when the payload changes', () => {
    const storage = new Map<string, string>();
    const wrappedStorage = {
      getItem: (key: string) => storage.get(key) ?? null,
      setItem: (key: string, value: string) => { storage.set(key, value); },
    };
    const createKey = () => window.crypto.randomUUID();

    const first = getStableDiagnosisIdempotencyKey('payload-a', wrappedStorage, createKey);
    const replay = getStableDiagnosisIdempotencyKey('payload-a', wrappedStorage, createKey);
    const changed = getStableDiagnosisIdempotencyKey('payload-b', wrappedStorage, createKey);
    expect(replay).toBe(first);
    expect(changed).not.toBe(first);
  });

  it('discards a late revision response from the previously selected repository', async () => {
    let resolveA!: (value: AnalysisRevision[]) => void;
    let resolveB!: (value: AnalysisRevision[]) => void;
    vi.spyOn(api, 'listAllRepositories').mockResolvedValue([
      { id: 'repo-a', name: 'A', git_url: 'https://example.com/a' } as any,
      { id: 'repo-b', name: 'B', git_url: 'https://example.com/b' } as any,
    ]);
    vi.spyOn(api, 'listAnalysisRevisions').mockImplementation((repoID) => new Promise((resolve) => {
      if (repoID === 'repo-a') resolveA = resolve;
      else resolveB = resolve;
    }));

    await act(async () => {
      root.render(<NewDiagnosisPage initialRepoId="repo-a" onDiagnosisCreated={() => undefined} />);
      await Promise.resolve();
    });
    const [repoSelect, revisionSelect] = [...container.querySelectorAll('select')];
    expect(api.listAnalysisRevisions).toHaveBeenCalledWith('repo-a');

    await act(async () => setInputValue(repoSelect, 'repo-b'));
    expect(revisionSelect.value).toBe('');
    expect([...revisionSelect.options].map((option) => option.value)).toEqual(['']);
    expect(api.listAnalysisRevisions).toHaveBeenLastCalledWith('repo-b');

    await act(async () => { resolveB([readyRevision('revision-b', 'repo-b')]); await Promise.resolve(); });
    expect(revisionSelect.value).toBe('revision-b');
    await act(async () => { resolveA([readyRevision('revision-a', 'repo-a')]); await Promise.resolve(); });
    expect(repoSelect.value).toBe('repo-b');
    expect(revisionSelect.value).toBe('revision-b');
    expect([...revisionSelect.options].map((option) => option.value)).toEqual(['', 'revision-b']);
  });

  it('keeps the initial revision only when it belongs to the initial repository', async () => {
    const revisions = vi.spyOn(api, 'listAnalysisRevisions').mockResolvedValue([
      readyRevision('initial-revision', 'repo-a'),
      readyRevision('other-revision', 'repo-a'),
    ]);
    vi.spyOn(api, 'listAllRepositories').mockResolvedValue([{ id: 'repo-a', name: 'A', git_url: 'https://example.com/a' } as any]);
    await act(async () => {
      root.render(<NewDiagnosisPage initialRepoId="repo-a" initialRevisionId="initial-revision" onDiagnosisCreated={() => undefined} />);
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(container.querySelectorAll('select')[1].value).toBe('initial-revision');

    await act(async () => root.unmount());
    root = createRoot(container);
    revisions.mockResolvedValueOnce([
      readyRevision('initial-revision', 'repo-other'),
      readyRevision('other-revision', 'repo-a'),
    ]);
    await act(async () => {
      root.render(<NewDiagnosisPage initialRepoId="repo-a" initialRevisionId="initial-revision" onDiagnosisCreated={() => undefined} />);
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(container.querySelectorAll('select')[1].value).toBe('other-revision');
  });

  it('clears loaded options immediately and never reapplies the initial revision after manual switching', async () => {
    vi.spyOn(api, 'listAllRepositories').mockResolvedValue([
      { id: 'repo-a', name: 'A', git_url: 'https://example.com/a' } as any,
      { id: 'repo-b', name: 'B', git_url: 'https://example.com/b' } as any,
    ]);
    let resolveNext!: (value: AnalysisRevision[]) => void;
    const revisions = vi.spyOn(api, 'listAnalysisRevisions')
      .mockResolvedValueOnce([readyRevision('a-first', 'repo-a'), readyRevision('a-initial', 'repo-a')])
      .mockImplementation(() => new Promise((resolve) => { resolveNext = resolve; }));
    await act(async () => {
      root.render(<NewDiagnosisPage initialRepoId="repo-a" initialRevisionId="a-initial" onDiagnosisCreated={() => undefined} />);
      await Promise.resolve();
      await Promise.resolve();
    });
    const [repoSelect, revisionSelect] = [...container.querySelectorAll('select')];
    expect(revisionSelect.value).toBe('a-initial');
    expect(revisionSelect.options.length).toBe(3);
    await act(async () => setInputValue(repoSelect, 'repo-b'));
    expect(revisionSelect.value).toBe('');
    expect([...revisionSelect.options].map((option) => option.value)).toEqual(['']);
    await act(async () => { resolveNext([readyRevision('b-ready', 'repo-b')]); await Promise.resolve(); });
    expect(revisionSelect.value).toBe('b-ready');
    await act(async () => setInputValue(repoSelect, 'repo-a'));
    expect(revisions).toHaveBeenLastCalledWith('repo-a');
    expect(revisionSelect.value).toBe('');
    await act(async () => { resolveNext([readyRevision('a-first', 'repo-a'), readyRevision('a-initial', 'repo-a')]); await Promise.resolve(); });
    expect(revisionSelect.value).toBe('a-first');
  });

  it('leaves revision selection empty when the current repository has no READY revision', async () => {
    vi.spyOn(api, 'listAllRepositories').mockResolvedValue([{ id: 'repo-a', name: 'A', git_url: 'https://example.com/a' } as any]);
    vi.spyOn(api, 'listAnalysisRevisions').mockResolvedValue([{ ...readyRevision('preparing', 'repo-a'), status: 'PREPARING' }]);
    await act(async () => {
      root.render(<NewDiagnosisPage initialRepoId="repo-a" onDiagnosisCreated={() => undefined} />);
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(container.querySelectorAll('select')[1].value).toBe('');
  });
});
