import { ProviderStatus, Repository, DiagnosisRun, DiagnosisReport, AgentStep, CodeIndexBuild, CodeSymbol, SymbolRelation, QualityReport } from './types';

const API_BASE = '/api/v1';

async function handleResponse<T>(res: Response): Promise<T> {
  if (!res.ok) {
    let errDetail = `HTTP ${res.status} ${res.statusText}`;
    try {
      const body = await res.json();
      if (body.error) errDetail = body.error;
      else if (body.message) errDetail = body.message;
    } catch {
      // ignore
    }
    throw new Error(errDetail);
  }
  return res.json();
}

export const api = {
  async getProviderStatus(): Promise<ProviderStatus> {
    const res = await fetch(`${API_BASE}/system/provider`);
    return handleResponse<ProviderStatus>(res);
  },

  async saveProviderConfig(data: { base_url: string; model: string; api_key: string }): Promise<{ message: string; status: ProviderStatus }> {
    const res = await fetch(`${API_BASE}/system/provider`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(data),
    });
    return handleResponse(res);
  },

  async testProviderConnection(data: { base_url: string; model: string; api_key: string }): Promise<{ success: boolean; latency_ms: number; message: string }> {
    const res = await fetch(`${API_BASE}/system/provider/test`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(data),
    });
    return handleResponse(res);
  },

  async triggerDemo(): Promise<{ diagnosis_id: string; repository_id: string; report: DiagnosisReport }> {
    const res = await fetch(`${API_BASE}/demo/trigger`, {
      method: 'POST',
    });
    return handleResponse(res);
  },

  async listRepositories(): Promise<Repository[]> {
    const res = await fetch(`${API_BASE}/repositories`);
    return handleResponse<Repository[]>(res);
  },

  async createRepository(data: { name: string; git_url: string; default_ref?: string }): Promise<Repository> {
    const res = await fetch(`${API_BASE}/repositories`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(data),
    });
    return handleResponse<Repository>(res);
  },

  async triggerIndex(repoId: string, ref?: string): Promise<{ snapshot_id: string }> {
    const res = await fetch(`${API_BASE}/repositories/${repoId}/index`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ref: ref || 'main' }),
    });
    return handleResponse(res);
  },

  // Code Intelligence (M5)
  async triggerCodeIndexBuild(snapshotId: string): Promise<{ code_index_build: CodeIndexBuild; status: string }> {
    const res = await fetch(`${API_BASE}/snapshots/${snapshotId}/code-index-builds`, {
      method: 'POST',
    });
    return handleResponse(res);
  },

  async getCodeIndexBuild(id: number): Promise<CodeIndexBuild> {
    const res = await fetch(`${API_BASE}/code-index-builds/${id}`);
    return handleResponse<CodeIndexBuild>(res);
  },

  async getBuildQuality(id: number): Promise<QualityReport> {
    const res = await fetch(`${API_BASE}/code-index-builds/${id}/quality`);
    return handleResponse<QualityReport>(res);
  },

  async listSymbols(buildId: number, query?: string): Promise<{ symbols: CodeSymbol[]; total: number }> {
    const q = query ? `?q=${encodeURIComponent(query)}` : '';
    const res = await fetch(`${API_BASE}/code-index-builds/${buildId}/symbols${q}`);
    return handleResponse(res);
  },

  async getSymbolReferences(symbolId: number, buildId: number): Promise<{ relations: SymbolRelation[]; total: number }> {
    const res = await fetch(`${API_BASE}/symbols/${symbolId}/references?code_index_build_id=${buildId}`);
    return handleResponse(res);
  },

  async getSymbolRelatedTests(symbolId: number, buildId: number, symbolKeyHash: string): Promise<{ related_tests: SymbolRelation[]; total: number }> {
    const res = await fetch(`${API_BASE}/symbols/${symbolId}/tests?code_index_build_id=${buildId}&symbol_key_hash=${symbolKeyHash}`);
    return handleResponse(res);
  },

  // Diagnoses
  async listDiagnoses(): Promise<DiagnosisRun[]> {
    const res = await fetch(`${API_BASE}/diagnoses`);
    return handleResponse<DiagnosisRun[]>(res);
  },

  async getDiagnosis(id: string): Promise<DiagnosisRun> {
    const res = await fetch(`${API_BASE}/diagnoses/${id}`);
    return handleResponse<DiagnosisRun>(res);
  },

  async createDiagnosis(data: {
    repository_id: string;
    snapshot_id: string;
    issue_title: string;
    issue_description?: string;
    error_log?: string;
    idempotency_key?: string;
  }): Promise<{ diagnosis_run: DiagnosisRun; is_duplicate?: boolean }> {
    const res = await fetch(`${API_BASE}/diagnoses`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...(data.idempotency_key ? { 'Idempotency-Key': data.idempotency_key } : {}),
      },
      body: JSON.stringify(data),
    });
    return handleResponse(res);
  },

  async cancelDiagnosis(id: string): Promise<{ message: string }> {
    const res = await fetch(`${API_BASE}/diagnoses/${id}/cancel`, {
      method: 'POST',
    });
    return handleResponse(res);
  },

  async getDiagnosisReport(id: string): Promise<DiagnosisReport> {
    const res = await fetch(`${API_BASE}/diagnoses/${id}/report`);
    return handleResponse<DiagnosisReport>(res);
  },

  async getDiagnosisSteps(id: string): Promise<AgentStep[]> {
    const res = await fetch(`${API_BASE}/diagnoses/${id}/steps`);
    return handleResponse<AgentStep[]>(res);
  },
};
