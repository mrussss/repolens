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

export interface CodeIndexBuild {
  id: number;
  snapshot_id: string;
  parser_version: string;
  analyzer_version: string;
  symbol_schema_version: string;
  build_context_hash: string;
  module_path: string;
  goos: string;
  goarch: string;
  status: 'CREATED' | 'BUILDING' | 'READY' | 'FAILED';
  files_total: number;
  files_parsed: number;
  files_failed: number;
  packages_total: number;
  packages_typechecked: number;
  packages_failed: number;
  symbol_count: number;
  semantic_relation_count: number;
  syntactic_relation_count: number;
  heuristic_relation_count: number;
  unresolved_relation_count: number;
  error_code?: string;
  created_at: string;
  ready_at?: string;
}

export interface RetrievalBuild {
  id: number;
  code_index_build_id: number;
  strategy: string;
  artifact_path?: string;
  artifact_hash?: string;
  doc_count: number;
  status: 'CREATED' | 'BUILDING' | 'READY' | 'FAILED';
  created_at: string;
  ready_at?: string;
}

export interface CodeSymbol {
  id: number;
  code_index_build_id: number;
  file_path: string;
  symbol_key_raw: string;
  symbol_key_hash: string;
  module_path: string;
  package_path: string;
  package_name: string;
  kind: 'FUNCTION' | 'METHOD' | 'TYPE' | 'INTERFACE';
  name: string;
  qualified_name: string;
  receiver_raw?: string;
  receiver_canonical?: string;
  signature: string;
  doc?: string;
  start_line: number;
  start_col: number;
  end_line: number;
  end_col: number;
  exported: boolean;
  content_hash: string;
}

export interface SymbolRelation {
  id: number;
  code_index_build_id: number;
  from_symbol_id?: number;
  from_symbol_key_hash?: string;
  to_symbol_id?: number;
  to_symbol_key_hash?: string;
  relation_type: 'REFERENCE' | 'CALL_CANDIDATE' | 'TEST_RELATION';
  resolution_kind: 'SEMANTIC' | 'SYNTACTIC' | 'HEURISTIC' | 'UNRESOLVED';
  confidence: number;
  reason_code: string;
  reason_detail?: string;
  target_name?: string;
  file_path: string;
  line: number;
  column: number;
}

export interface QualityReport {
  code_index_build_id: number;
  snapshot_id: string;
  files_total: number;
  files_parsed: number;
  files_failed: number;
  parsed_pct: string;
  packages_total: number;
  packages_typechecked: number;
  packages_failed: number;
  typechecked_pct: string;
  symbol_count: number;
  semantic_relation_count: number;
  syntactic_relation_count: number;
  heuristic_relation_count: number;
  unresolved_relation_count: number;
  status: string;
}

export interface DiagnosisRun {
  id: string;
  user_id: string;
  repository_id: string;
  snapshot_id: string;
  code_index_build_id?: number;
  retrieval_build_id?: number;
  issue_title: string;
  issue_description?: string;
  error_log?: string;
  status: 'QUEUED' | 'RUNNING' | 'SUCCEEDED' | 'FAILED' | 'CANCELLED';
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
