package evidence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Finding struct {
	Title     string     `json:"title"`
	Reasoning string     `json:"reasoning"`
	Citations []Citation `json:"citations,omitempty"`
}

type ConclusionKind string

const (
	ConclusionRootCause            ConclusionKind = "ROOT_CAUSE"
	ConclusionInsufficientEvidence ConclusionKind = "INSUFFICIENT_EVIDENCE"
)

type ReportStatus string

const (
	ReportValid                ReportStatus = "VALID"
	ReportDegraded             ReportStatus = "DEGRADED"
	ReportInsufficientEvidence ReportStatus = "INSUFFICIENT_EVIDENCE"
	ReportInvalid              ReportStatus = "INVALID"
)

type DiagnosisReportData struct {
	ConclusionKind         ConclusionKind `json:"conclusion_kind"`
	Summary                string         `json:"summary"`
	RootCause              string         `json:"root_cause"`
	Findings               []Finding      `json:"findings"`
	RecommendedChecks      []string       `json:"recommended_checks"`
	Confidence             float64        `json:"confidence"`
	ModelClaimedConfidence *float64       `json:"model_claimed_confidence,omitempty"`
	Limitations            []string       `json:"limitations"`
}

type Report struct {
	ID                      string         `gorm:"primaryKey;size:36" json:"id"`
	DiagnosisRunID          string         `gorm:"size:36;not null;index" json:"diagnosis_run_id"`
	AttemptID               string         `gorm:"size:36;not null;index" json:"attempt_id"`
	RootCause               string         `gorm:"type:text;not null" json:"root_cause"`
	ConclusionKind          ConclusionKind `gorm:"size:32;not null;default:''" json:"conclusion_kind"`
	ReportStatus            ReportStatus   `gorm:"size:32;not null;default:'INVALID'" json:"report_status"`
	Summary                 string         `gorm:"type:text" json:"summary,omitempty"`
	FindingsJSON            string         `gorm:"type:text;not null" json:"findings_json"`
	RecommendedChecksJSON   string         `gorm:"type:text" json:"recommended_checks_json"`
	Confidence              float64        `gorm:"default:0.0" json:"confidence"`
	RawOutput               string         `gorm:"type:mediumtext" json:"raw_output,omitempty"`
	ParseError              string         `gorm:"type:text" json:"parse_error,omitempty"`
	LimitationsJSON         string         `gorm:"type:text" json:"limitations_json,omitempty"`
	ModelClaimedConfidence  *float64       `json:"model_claimed_confidence,omitempty"`
	FindingCount            int            `gorm:"not null;default:0" json:"finding_count"`
	SupportedFindingCount   int            `gorm:"not null;default:0" json:"supported_finding_count"`
	UnsupportedFindingCount int            `gorm:"not null;default:0" json:"unsupported_finding_count"`
	ValidCitationCount      int            `gorm:"not null;default:0" json:"valid_citation_count"`
	InvalidCitationCount    int            `gorm:"not null;default:0" json:"invalid_citation_count"`
	CitationCoverage        *float64       `json:"citation_coverage,omitempty"`
	FinalizationReason      string         `gorm:"size:64" json:"finalization_reason,omitempty"`
	CreatedAt               time.Time      `json:"created_at"`
}

type ReportQuality struct {
	Status                  ReportStatus
	FindingCount            int
	SupportedFindingCount   int
	UnsupportedFindingCount int
	ValidCitationCount      int
	InvalidCitationCount    int
	CitationCoverage        *float64
}

func ClassifyReport(data *DiagnosisReportData, structured bool) (ReportQuality, error) {
	if !structured || data == nil {
		return ReportQuality{Status: ReportInvalid}, errors.New("structured report is invalid")
	}
	if data.ConclusionKind == "" && data.RootCause != "" {
		// Compatibility for v2.1 Scripted/Fake providers. New provider output
		// is prompted to emit the explicit field.
		data.ConclusionKind = ConclusionRootCause
	}
	if data.ConclusionKind != ConclusionRootCause && data.ConclusionKind != ConclusionInsufficientEvidence {
		return ReportQuality{Status: ReportInvalid}, errors.New("conclusion_kind is invalid")
	}
	if data.ConclusionKind == ConclusionInsufficientEvidence {
		if len(data.Limitations) == 0 && len(data.RecommendedChecks) == 0 {
			return ReportQuality{Status: ReportInvalid}, errors.New("insufficient evidence report needs limitations or next checks")
		}
		return ReportQuality{Status: ReportInsufficientEvidence}, nil
	}
	if data.Summary == "" || data.RootCause == "" || len(data.Findings) == 0 {
		return ReportQuality{Status: ReportInvalid}, fmt.Errorf("root cause report is missing required fields")
	}
	quality := ReportQuality{Status: ReportValid, FindingCount: len(data.Findings)}
	for _, finding := range data.Findings {
		if len(finding.Citations) == 0 {
			quality.UnsupportedFindingCount++
			continue
		}
		hasValid := false
		for _, citation := range finding.Citations {
			switch citation.ValidationStatus {
			case CitationValid:
				quality.ValidCitationCount++
				hasValid = true
			case CitationInvalid:
				quality.InvalidCitationCount++
			}
		}
		if hasValid {
			quality.SupportedFindingCount++
		} else {
			quality.UnsupportedFindingCount++
		}
	}
	if quality.FindingCount > 0 {
		coverage := float64(quality.SupportedFindingCount) / float64(quality.FindingCount)
		quality.CitationCoverage = &coverage
	}
	if quality.UnsupportedFindingCount > 0 || quality.InvalidCitationCount > 0 {
		quality.Status = ReportDegraded
	}
	return quality, nil
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
