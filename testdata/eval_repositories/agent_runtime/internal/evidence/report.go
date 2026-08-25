package evidence

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Finding struct {
	Title     string     `json:"title"`
	Reasoning string     `json:"reasoning"`
	Citations []Citation `json:"citations,omitempty"`
}

type DiagnosisReportData struct {
	Summary           string    `json:"summary"`
	RootCause         string    `json:"root_cause"`
	Findings          []Finding `json:"findings"`
	RecommendedChecks []string  `json:"recommended_checks"`
	Confidence        float64   `json:"confidence"`
}

type Report struct {
	ID                    string    `gorm:"primaryKey;size:36" json:"id"`
	DiagnosisRunID        string    `gorm:"size:36;not null;index" json:"diagnosis_run_id"`
	AttemptID             string    `gorm:"size:36;not null;index" json:"attempt_id"`
	RootCause             string    `gorm:"type:text;not null" json:"root_cause"`
	FindingsJSON          string    `gorm:"type:text;not null" json:"findings_json"`
	RecommendedChecksJSON string    `gorm:"type:text" json:"recommended_checks_json"`
	Confidence            float64   `gorm:"default:0.0" json:"confidence"`
	CreatedAt             time.Time `json:"created_at"`
}

type ReportStore interface {
	Create(ctx context.Context, report *Report) error
	GetByRunID(ctx context.Context, runID string) (*Report, error)
	GetByAttemptID(ctx context.Context, attemptID string) (*Report, error)
}

type GormReportStore struct {
	db *gorm.DB
}

func NewReportStore(db *gorm.DB) *GormReportStore {
	return &GormReportStore{db: db}
}

func (s *GormReportStore) Create(ctx context.Context, report *Report) error {
	if report.ID == "" {
		report.ID = uuid.New().String()
	}
	return s.db.WithContext(ctx).Create(report).Error
}

func (s *GormReportStore) GetByRunID(ctx context.Context, runID string) (*Report, error) {
	var rep Report
	if err := s.db.WithContext(ctx).Where("diagnosis_run_id = ?", runID).Order("created_at DESC").First(&rep).Error; err != nil {
		return nil, err
	}
	return &rep, nil
}

func (s *GormReportStore) GetByAttemptID(ctx context.Context, attemptID string) (*Report, error) {
	var rep Report
	if err := s.db.WithContext(ctx).Where("attempt_id = ?", attemptID).First(&rep).Error; err != nil {
		return nil, err
	}
	return &rep, nil
}
