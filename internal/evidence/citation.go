package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"repolens/internal/platform/snapshotstore"
)

type CitationStatus string

const (
	CitationValid     CitationStatus = "VALID"
	CitationInvalid   CitationStatus = "INVALID"
	CitationUnchecked CitationStatus = "UNCHECKED"
)

type Citation struct {
	ID               string         `gorm:"primaryKey;size:36" json:"id"`
	ReportID         string         `gorm:"size:36;index" json:"report_id,omitempty"`
	SnapshotID       string         `gorm:"size:36;not null;index" json:"snapshot_id"`
	CodeIndexBuildID int64          `gorm:"not null;default:0;index" json:"code_index_build_id"`
	FilePath         string         `gorm:"size:255;not null" json:"file_path"`
	StartLine        int            `gorm:"not null" json:"start_line"`
	EndLine          int            `gorm:"not null" json:"end_line"`
	Excerpt          string         `gorm:"type:text" json:"excerpt,omitempty"`
	Reason           string         `gorm:"size:255" json:"reason,omitempty"`
	ContentHash      string         `gorm:"size:64" json:"content_hash,omitempty"`
	ValidationStatus CitationStatus `gorm:"size:32;not null;default:'UNCHECKED'" json:"validation_status"`
	ValidationError  string         `gorm:"size:255" json:"validation_error,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
}

type CitationStore interface {
	CreateBatch(ctx context.Context, citations []Citation) error
	ListByReportID(ctx context.Context, reportID string) ([]Citation, error)
}

type GormCitationStore struct {
	db *gorm.DB
}

func NewCitationStore(db *gorm.DB) *GormCitationStore {
	return &GormCitationStore{db: db}
}

func (s *GormCitationStore) CreateBatch(ctx context.Context, citations []Citation) error {
	if len(citations) == 0 {
		return nil
	}
	for i := range citations {
		if citations[i].ID == "" {
			citations[i].ID = uuid.New().String()
		}
	}
	return s.db.WithContext(ctx).Create(&citations).Error
}

func (s *GormCitationStore) ListByReportID(ctx context.Context, reportID string) ([]Citation, error) {
	var list []Citation
	err := s.db.WithContext(ctx).Where("report_id = ?", reportID).Find(&list).Error
	return list, err
}

type CitationValidator struct {
	store snapshotstore.SnapshotStore
}

func NewCitationValidator(store snapshotstore.SnapshotStore) *CitationValidator {
	return &CitationValidator{store: store}
}

func (v *CitationValidator) Validate(ctx context.Context, repoID, snapshotID string, c *Citation) {
	if c.FilePath == "" {
		c.ValidationStatus = CitationInvalid
		c.ValidationError = "file_path is empty"
		return
	}

	if c.StartLine <= 0 || c.EndLine < c.StartLine {
		c.ValidationStatus = CitationInvalid
		c.ValidationError = fmt.Sprintf("invalid line range: %d to %d", c.StartLine, c.EndLine)
		return
	}

	if !v.store.FileExists(repoID, snapshotID, c.FilePath) {
		c.ValidationStatus = CitationInvalid
		c.ValidationError = fmt.Sprintf("file %s does not exist in snapshot", c.FilePath)
		return
	}

	actualContent, err := v.store.ReadFile(ctx, repoID, snapshotID, c.FilePath, c.StartLine, c.EndLine)
	if err != nil {
		c.ValidationStatus = CitationInvalid
		c.ValidationError = fmt.Sprintf("failed to read file lines: %v", err)
		return
	}

	h := sha256.Sum256([]byte(actualContent))
	c.ContentHash = hex.EncodeToString(h[:])

	if c.Excerpt != "" {
		normExcerpt := strings.TrimSpace(strings.ReplaceAll(c.Excerpt, "\r\n", "\n"))
		normActual := strings.TrimSpace(strings.ReplaceAll(actualContent, "\r\n", "\n"))
		if !strings.Contains(normActual, normExcerpt) && !strings.Contains(normExcerpt, normActual) {
			c.ValidationStatus = CitationInvalid
			c.ValidationError = "excerpt does not match actual file lines in snapshot"
			return
		}
	}

	c.ValidationStatus = CitationValid
	c.ValidationError = ""
}
