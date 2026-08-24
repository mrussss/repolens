package trace

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type StepType string

const (
	StepTypeThinking    StepType = "THINKING"
	StepTypeToolCall    StepType = "TOOL_CALL"
	StepTypeToolResult  StepType = "TOOL_RESULT"
	StepTypeFinalOutput StepType = "FINAL_OUTPUT"
	StepTypeError       StepType = "ERROR"
)

type AgentStep struct {
	ID                string    `gorm:"primaryKey;size:36" json:"id"`
	AttemptID         string    `gorm:"size:36;not null;index" json:"attempt_id"`
	Seq               int       `gorm:"not null" json:"seq"`
	StepType          StepType  `gorm:"size:32;not null" json:"step_type"`
	ToolName          string    `gorm:"size:64" json:"tool_name,omitempty"`
	ToolArgsSummary   string    `gorm:"type:text" json:"tool_args_summary,omitempty"`
	ToolResultSummary string    `gorm:"type:mediumtext" json:"tool_result_summary,omitempty"`
	Status            string    `gorm:"size:32;not null;default:'COMPLETED'" json:"status"`
	LatencyMs         int64     `gorm:"default:0" json:"latency_ms"`
	InputTokens       int       `gorm:"default:0" json:"input_tokens"`
	OutputTokens      int       `gorm:"default:0" json:"output_tokens"`
	ErrorCode         string    `gorm:"size:64" json:"error_code,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

type Store interface {
	Create(ctx context.Context, step *AgentStep) error
	ListByAttempt(ctx context.Context, attemptID string) ([]AgentStep, error)
	ListAfterSeq(ctx context.Context, attemptID string, lastSeq int) ([]AgentStep, error)
}

type GormStore struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *GormStore {
	return &GormStore{db: db}
}

func (s *GormStore) Create(ctx context.Context, step *AgentStep) error {
	if step.ID == "" {
		step.ID = uuid.New().String()
	}
	return s.db.WithContext(ctx).Create(step).Error
}

func (s *GormStore) ListByAttempt(ctx context.Context, attemptID string) ([]AgentStep, error) {
	var steps []AgentStep
	err := s.db.WithContext(ctx).Where("attempt_id = ?", attemptID).Order("seq ASC").Find(&steps).Error
	return steps, err
}

func (s *GormStore) ListAfterSeq(ctx context.Context, attemptID string, lastSeq int) ([]AgentStep, error) {
	var steps []AgentStep
	err := s.db.WithContext(ctx).Where("attempt_id = ? AND seq > ?", attemptID, lastSeq).Order("seq ASC").Find(&steps).Error
	return steps, err
}
