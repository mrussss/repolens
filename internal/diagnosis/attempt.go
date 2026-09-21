package diagnosis

import (
	"time"
)

type AttemptStatus string

const (
	AttemptStatusRunning         AttemptStatus = "RUNNING"
	AttemptStatusSucceeded       AttemptStatus = "SUCCEEDED"
	AttemptStatusFailedRetryable AttemptStatus = "FAILED_RETRYABLE"
	AttemptStatusFailedTerminal  AttemptStatus = "FAILED_TERMINAL"
	AttemptStatusCancelled       AttemptStatus = "CANCELLED"
	AttemptStatusAbandoned       AttemptStatus = "ABANDONED"
)

type DiagnosisAttempt struct {
	ID                      string        `gorm:"primaryKey;size:36" json:"id"`
	DiagnosisRunID          string        `gorm:"size:36;not null;index" json:"diagnosis_run_id"`
	AttemptNo               int           `gorm:"not null" json:"attempt_no"`
	WorkerID                string        `gorm:"size:64;not null;index" json:"worker_id"`
	Status                  AttemptStatus `gorm:"size:32;not null;default:'RUNNING';index" json:"status"`
	StartedAt               time.Time     `gorm:"not null" json:"started_at"`
	HeartbeatAt             time.Time     `gorm:"not null;index" json:"heartbeat_at"`
	DeadlineAt              time.Time     `gorm:"not null;index" json:"deadline_at"`
	FinishedAt              *time.Time    `json:"finished_at,omitempty"`
	ErrorCode               string        `gorm:"size:64" json:"error_code,omitempty"`
	ErrorMessage            string        `gorm:"type:text" json:"error_message,omitempty"`
	Retryable               bool          `gorm:"default:false" json:"retryable"`
	Model                   string        `gorm:"size:64" json:"model,omitempty"`
	PromptTokens            int           `gorm:"default:0" json:"prompt_tokens"`
	CompletionTokens        int           `gorm:"default:0" json:"completion_tokens"`
	ToolCalls               int           `gorm:"default:0" json:"tool_calls"`
	CachedPromptTokens      int           `gorm:"default:0" json:"cached_prompt_tokens"`
	ReasoningTokens         int           `gorm:"default:0" json:"reasoning_tokens"`
	AgentRounds             int           `gorm:"default:0" json:"agent_rounds"`
	SearchCalls             int           `gorm:"default:0" json:"search_calls"`
	ProviderCalls           int           `gorm:"default:0" json:"provider_calls"`
	RawOutput               string        `gorm:"type:mediumtext" json:"raw_output,omitempty"`
	ParsedReportJSON        string        `gorm:"type:mediumtext" json:"parsed_report_json,omitempty"`
	ParsedReportDraftJSON   string        `gorm:"type:mediumtext" json:"parsed_report_draft_json,omitempty"`
	CheckpointPromptVersion string        `gorm:"size:64" json:"checkpoint_prompt_version,omitempty"`
	CheckpointAgentVersion  string        `gorm:"size:64" json:"checkpoint_agent_version,omitempty"`
	StructuredOutputValid   bool          `gorm:"default:false" json:"structured_output_valid"`
	ProviderCompletedAt     *time.Time    `json:"provider_completed_at,omitempty"`
	FinalizationReason      string        `gorm:"size:64" json:"finalization_reason,omitempty"`
	CreatedAt               time.Time     `json:"created_at"`
	UpdatedAt               time.Time     `json:"updated_at"`
}
