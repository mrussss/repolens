import { ProviderStatus, Repository, Snapshot, DiagnosisRun, DiagnosisReport, AgentStep, CodeIndexBuild, CodeSymbol, SymbolRelation, QualityReport, RetrievalBuild } from './types';

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
    const res = await fetch(`${API_BASE}/settings/provider`);
    return handleResponse<ProviderStatus>(res);
  },

  async saveProviderConfig(data: { base_url: string; model: string; api_key: string }): Promise<{ message: string; status: ProviderStatus }> {
    const res = await fetch(`${API_BASE}/settings/provider`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(data),
    });
    return handleResponse(res);
  },

  async testProviderConnection(data: { base_url: string; model: string; api_key: string }): Promise<{ success: boolean; latency_ms: number; message: string }> {
    const res = await fetch(`${API_BASE}/settings/provider/test`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(data),
    });
    return handleResponse(res);
  },

  async triggerDemo(): Promise<{ diagnosis_id: string; repository_id: string; report: DiagnosisReport }> {
    const res = await fetch(`${API_BASE}/demo/trigger`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
    return handleResponse(res);
  },

  async listRepositories(): Promise<Repository[]> {
    const res = await fetch(`${API_BASE}/repositories`);
    const body = await handleResponse<{ repositories: Repository[] }>(res);
    return body.repositories || [];
  },

  async createRepository(data: { name: string; git_url: string; default_ref?: string }): Promise<Repository> {
    const res = await fetch(`${API_BASE}/repositories`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(data),
    });
    const body = await handleResponse<{ repository: Repository }>(res);
    return body.repository;
  },

  async triggerIndex(repoId: string, ref?: string): Promise<{ snapshot: Snapshot; message: string }> {
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
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
    return handleResponse(res);
  },

  async getCodeIndexBuild(id: number): Promise<CodeIndexBuild> {
    const res = await fetch(`${API_BASE}/code-index-builds/${id}`);
    return handleResponse<CodeIndexBuild>(res);
  },

  async triggerRetrievalBuild(buildId: number): Promise<{ retrieval_build: RetrievalBuild; status: string }> {
    const res = await fetch(`${API_BASE}/code-index-builds/${buildId}/retrieval-builds`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
    return handleResponse(res);
  },

  async getRetrievalBuild(id: number): Promise<RetrievalBuild> {
    const res = await fetch(`${API_BASE}/retrieval-builds/${id}`);
    return handleResponse<RetrievalBuild>(res);
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
    const body = await handleResponse<{ diagnosis_runs: DiagnosisRun[] }>(res);
    return body.diagnosis_runs || [];
  },

  async getDiagnosis(id: string): Promise<DiagnosisRun> {
    const res = await fetch(`${API_BASE}/diagnoses/${id}`);
    const body = await handleResponse<{ diagnosis_run: DiagnosisRun }>(res);
    return body.diagnosis_run;
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
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
    return handleResponse(res);
  },

  async getDiagnosisReport(id: string): Promise<DiagnosisReport> {
    const res = await fetch(`${API_BASE}/diagnoses/${id}/report`);
    const body = await handleResponse<{ report: any; citations: any[] }>(res);
    let findings = [];
    let recommendedChecks = [];
    try { findings = JSON.parse(body.report.findings_json || '[]'); } catch {}
    try { recommendedChecks = JSON.parse(body.report.recommended_checks_json || '[]'); } catch {}
    return { ...body.report, findings, recommended_checks: recommendedChecks } as DiagnosisReport;
  },

  async getDiagnosisSteps(id: string): Promise<AgentStep[]> {
    const res = await fetch(`${API_BASE}/diagnoses/${id}/steps`);
    const body = await handleResponse<{ steps: AgentStep[] }>(res);
    return body.steps || [];
  },
};
