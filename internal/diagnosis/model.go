package diagnosis

import (
	"time"
)

type RunStatus string

const (
	StatusQueued    RunStatus = "QUEUED"
	StatusRunning   RunStatus = "RUNNING"
	StatusSucceeded RunStatus = "SUCCEEDED"
	StatusFailed    RunStatus = "FAILED"
	StatusCancelled RunStatus = "CANCELLED"
)

type DiagnosisRun struct {
	ID                          string    `gorm:"primaryKey;size:36" json:"id"`
	UserID                      string    `gorm:"size:36;not null;index:idx_user_idemp,unique;index:idx_user_created" json:"user_id"`
	RepositoryID                string    `gorm:"size:36;not null;index" json:"repository_id"`
	AnalysisRevisionID          string    `gorm:"size:36;index" json:"analysis_revision_id,omitempty"`
	SnapshotID                  string    `gorm:"size:36;not null;index" json:"snapshot_id"`
	CodeIndexBuildID            int64     `gorm:"not null;index" json:"code_index_build_id"`
	RetrievalBuildID            int64     `gorm:"not null;index" json:"retrieval_build_id"`
	IssueTitle                  string    `gorm:"size:255;not null" json:"issue_title"`
	IssueDescription            string    `gorm:"type:text" json:"issue_description"`
	ErrorLog                    string    `gorm:"type:mediumtext" json:"error_log"`
	Status                      RunStatus `gorm:"size:32;not null;default:'QUEUED';index" json:"status"`
	CancelRequested             bool      `gorm:"default:false;not null" json:"cancel_requested"`
	ProviderEndpointFingerprint string    `gorm:"size:64;not null" json:"provider_endpoint_fingerprint"`
	ProviderConfigFingerprint   string    `gorm:"size:64;not null" json:"provider_config_fingerprint"`
	NormalizedBaseURL           string    `gorm:"size:512;not null" json:"normalized_base_url"`
	ModelName                   string    `gorm:"size:128;not null" json:"model_name"`
	PromptVersion               string    `gorm:"size:64;not null" json:"prompt_version"`
	AgentVersion                string    `gorm:"size:64;not null" json:"agent_version"`
	AgentConfigHash             string    `gorm:"size:64;not null" json:"agent_config_hash"`
	MaxAgentRounds              int       `gorm:"not null;default:8" json:"max_agent_rounds"`
	MaxToolCalls                int       `gorm:"not null;default:12" json:"max_tool_calls"`
	MaxSearchCalls              int       `gorm:"not null;default:3" json:"max_search_calls"`
	MaxRepeatCalls              int       `gorm:"not null;default:2" json:"max_repeat_calls"`
	MaxEvidencePacketBytes      int       `gorm:"not null;default:32768" json:"max_evidence_packet_bytes"`
	MaxToolResultBytes          int       `gorm:"not null;default:32768" json:"max_tool_result_bytes"`
	FinalizationTurns           int       `gorm:"not null;default:1" json:"finalization_turns"`
	MaxOutputTokens             int       `gorm:"not null;default:4096" json:"max_output_tokens"`
	ReasoningEffort             string    `gorm:"size:32;not null;default:''" json:"reasoning_effort"`
	ProviderTimeoutSeconds      int       `gorm:"not null;default:60" json:"provider_timeout_seconds"`
	ProviderRetryAttempts       int       `gorm:"not null;default:0" json:"provider_retry_attempts"`
	PipelineFingerprint         string    `gorm:"size:64;not null;default:''" json:"pipeline_fingerprint,omitempty"`
	Temperature                 float64   `gorm:"not null;default:0" json:"temperature"`
	IdempotencyKey              string    `gorm:"size:128;not null;index:idx_user_idemp,unique" json:"idempotency_key"`
	IdempotencyRequestHash      string    `gorm:"size:64;not null" json:"idempotency_request_hash"`
	FinalAttemptID              string    `gorm:"size:36" json:"final_attempt_id,omitempty"`
	RetryAllowed                bool      `gorm:"-" json:"retry_allowed"`
	RetryReason                 string    `gorm:"-" json:"retry_reason,omitempty"`
	RetryErrorCode              string    `gorm:"-" json:"retry_error_code,omitempty"`
	ExecutionGeneration         int       `gorm:"-" json:"execution_generation,omitempty"`
	Version                     int       `gorm:"default:1;not null" json:"version"`
	CreatedAt                   time.Time `json:"created_at"`
	UpdatedAt                   time.Time `json:"updated_at"`
}
