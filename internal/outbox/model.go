package outbox

import (
	"time"
)

type EventStatus string

const (
	StatusPending   EventStatus = "PENDING"
	StatusPublished EventStatus = "PUBLISHED"
	StatusFailed    EventStatus = "FAILED"
)

const (
	AggregateDiagnosisRun   = "DIAGNOSIS_RUN"
	AggregateRepositoryIndex = "REPOSITORY_INDEX"

	EventDiagnosisRequested       = "DIAGNOSIS_REQUESTED"
	EventDiagnosisRetryRequested  = "DIAGNOSIS_RETRY_REQUESTED"
	EventRepositoryIndexRequested = "REPOSITORY_INDEX_REQUESTED"
)

type OutboxEvent struct {
	ID            string      `gorm:"primaryKey;size:36" json:"id"`
	AggregateType string      `gorm:"size:64;not null;index" json:"aggregate_type"`
	AggregateID   string      `gorm:"size:36;not null;index" json:"aggregate_id"`
	EventType     string      `gorm:"size:64;not null;index" json:"event_type"`
	Payload       string      `gorm:"type:text;not null" json:"payload"`
	Status        EventStatus `gorm:"size:32;not null;default:'PENDING';index" json:"status"`
	RetryCount    int         `gorm:"default:0" json:"retry_count"`
	AvailableAt   time.Time   `gorm:"not null;index" json:"available_at"`
	CreatedAt     time.Time   `json:"created_at"`
	PublishedAt   *time.Time  `json:"published_at,omitempty"`
}
