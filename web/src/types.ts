export interface ProviderStatus {
  base_url?: string;
  model?: string;
  endpoint_fingerprint?: string;
  config_fingerprint?: string;
  is_configured: boolean;
  is_demo: boolean;
  updated_at?: string;
}

export interface Repository {
  id: string;
  user_id: string;
  name: string;
  git_url: string;
  default_ref: string;
  status: string;
  created_at: string;
  snapshots?: Snapshot[];
}

export interface Snapshot {
  id: string;
  repository_id: string;
  commit_sha: string;
  ref: string;
  materialized_path: string;
  status: 'CREATED' | 'MATERIALIZING' | 'READY' | 'FAILED';
  ready_at?: string;
  created_at: string;
}

export interface DiagnosisRun {
  id: string;
  user_id: string;
  repository_id: string;
  snapshot_id: string;
  issue_title: string;
  issue_description?: string;
  error_log?: string;
  status: 'QUEUED' | 'RUNNING' | 'SUCCEEDED' | 'FAILED' | 'RETRY_WAIT' | 'CANCELLED';
  execution_status?: string;
  attempt_count?: number;
  idempotency_key?: string;
  created_at: string;
  updated_at: string;
}

export interface Citation {
  snapshot_id: string;
  file_path: string;
  start_line: number;
  end_line: number;
  excerpt: string;
  reason?: string;
}

export interface Finding {
  title: string;
  reasoning: string;
  citations: Citation[];
}

export interface DiagnosisReport {
  id: string;
  diagnosis_run_id: string;
  attempt_id: string;
  root_cause: string;
  findings: Finding[];
  recommended_checks: string[];
  confidence: number;
  created_at: string;
}

export interface AgentStep {
  id: string;
  attempt_id: string;
  seq: number;
  step_type: string;
  tool_name?: string;
  tool_args_summary?: string;
  tool_result_summary?: string;
  status: string;
  latency_ms: number;
  input_tokens: number;
  output_tokens: number;
  created_at: string;
}
