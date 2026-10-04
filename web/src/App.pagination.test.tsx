// @vitest-environment jsdom
import { act } from 'react';
import { createRoot } from 'react-dom/client';
import { describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { api } from './api';
import { DiagnosisRun } from './types';

vi.mock('./components/SetupPage', () => ({ SetupPage: () => <div>Setup</div> }));
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });

describe('Diagnosis History pagination', () => {
  it('opens 20 diagnoses on page one and the remaining 5 on page two', async () => {
    const all = Array.from({ length: 25 }, (_, i) => ({ id: `diag-${i}`, issue_title: `diagnosis-${i}`, repository_id: 'repository', status: 'SUCCEEDED', created_at: '2026-01-01T00:00:00Z' } as DiagnosisRun));
    const list = vi.spyOn(api, 'listDiagnoses').mockImplementation(async (page = 1, pageSize = 20) => ({
      items: all.slice((page - 1) * pageSize, page * pageSize), total: 25, page, page_size: pageSize,
    }));
    const container = document.createElement('div');
    document.body.append(container);
    const root = createRoot(container);
    try {
      await act(async () => { root.render(<App />); });
      const history = [...container.querySelectorAll('button')].find((button) => button.textContent?.includes('历史'))!;
      await act(async () => { history.click(); });
      expect(container.querySelectorAll('h3')).toHaveLength(20);
      expect(container.textContent).toContain('共 25 条');
      const next = [...container.querySelectorAll('button')].find((button) => button.textContent === '下一页')!;
      await act(async () => { next.click(); });
      expect(list).toHaveBeenLastCalledWith(2, 20);
      expect(container.querySelectorAll('h3')).toHaveLength(5);
      expect(container.textContent).toContain('diagnosis-24');
      expect(container.textContent).not.toContain('diagnosis-0');
      expect(next.disabled).toBe(true);
      await act(async () => { [...container.querySelectorAll('button')].find((button) => button.textContent === '上一页')!.click(); });
      expect(container.querySelectorAll('h3')).toHaveLength(20);
    } finally {
      await act(async () => { root.unmount(); });
      container.remove();
      vi.restoreAllMocks();
    }
  });
});
