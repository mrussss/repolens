package evidence

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"repolens/internal/platform/redaction"
	"repolens/internal/platform/snapshotstore"
)

type SourceKind string

const (
	SourceInitialRetrieval SourceKind = "INITIAL_RETRIEVAL"
	SourceSearchCode       SourceKind = "SEARCH_CODE"
	SourceReadFile         SourceKind = "READ_FILE"
)

var (
	ErrEvidenceNotFound          = errors.New("EVIDENCE_NOT_FOUND")
	ErrEvidenceAttemptMismatch   = errors.New("EVIDENCE_ATTEMPT_MISMATCH")
	ErrEvidenceLineageMismatch   = errors.New("EVIDENCE_LINEAGE_MISMATCH")
	ErrEvidenceSourceUnavailable = errors.New("EVIDENCE_SOURCE_UNAVAILABLE")
	ErrEvidenceHashMismatch      = errors.New("EVIDENCE_CONTENT_HASH_MISMATCH")
)

// AttemptEvidenceItem is the server-owned identity of a source range exposed
// to one diagnosis attempt. DisplayExcerpt may be redacted; RawContentHash is
// always calculated from the unredacted canonical snapshot range.
type AttemptEvidenceItem struct {
	ID               string     `gorm:"primaryKey;size:64" json:"evidence_id"`
	AttemptID        string     `gorm:"size:36;not null;index:idx_attempt_evidence;index:uq_attempt_evidence_identity,priority:1" json:"attempt_id"`
	DiagnosisRunID   string     `gorm:"size:36;not null;index" json:"diagnosis_run_id"`
	SnapshotID       string     `gorm:"size:64;not null;index:idx_snapshot_evidence;index:uq_attempt_evidence_identity,priority:2" json:"snapshot_id"`
	CodeIndexBuildID int64      `gorm:"not null;default:0;index" json:"code_index_build_id"`
	SourceKind       SourceKind `gorm:"size:32;not null" json:"source_kind"`
	SourceStepSeq    *int       `json:"source_step_seq,omitempty"`
	RetrievalChunkID string     `gorm:"size:128" json:"retrieval_chunk_id,omitempty"`
	FilePath         string     `gorm:"size:512;not null;index:idx_snapshot_evidence;index:uq_attempt_evidence_identity,priority:3" json:"path"`
	StartLine        int        `gorm:"not null;index:uq_attempt_evidence_identity,priority:4" json:"start_line"`
	EndLine          int        `gorm:"not null;index:uq_attempt_evidence_identity,priority:5" json:"end_line"`
	TotalLines       int        `gorm:"not null;default:0" json:"total_lines"`
	RawContentHash   string     `gorm:"size:64;not null;index:uq_attempt_evidence_identity,priority:6" json:"content_hash"`
	DisplayExcerpt   string     `gorm:"type:mediumtext;not null" json:"content"`
	RedactionApplied bool       `gorm:"not null;default:false" json:"redaction_applied"`
	Truncated        bool       `gorm:"not null;default:false" json:"truncated"`
	CreatedAt        time.Time  `json:"created_at"`
}

func (AttemptEvidenceItem) TableName() string { return "attempt_evidence_items" }

type IssueRequest struct {
	AttemptID        string
	DiagnosisRunID   string
	RepositoryID     string
	SnapshotID       string
	CodeIndexBuildID int64
	SourceKind       SourceKind
	SourceStepSeq    int
	RetrievalChunkID string
	FilePath         string
	StartLine        int
	EndLine          int
	MaxBytes         int
}

type EvidenceStore interface {
	Create(ctx context.Context, item *AttemptEvidenceItem) error
	GetByAttemptAndID(ctx context.Context, attemptID, evidenceID string) (*AttemptEvidenceItem, error)
	FindCanonical(ctx context.Context, attemptID, snapshotID, filePath string, startLine, endLine int, rawHash string) (*AttemptEvidenceItem, error)
}

type GormEvidenceStore struct {
	db *gorm.DB
}

func NewEvidenceStore(db *gorm.DB) *GormEvidenceStore { return &GormEvidenceStore{db: db} }

func (s *GormEvidenceStore) Create(ctx context.Context, item *AttemptEvidenceItem) error {
	if item.ID == "" {
		item.ID = "ev_" + uuid.NewString()
	}
	return s.db.WithContext(ctx).Create(item).Error
}

func (s *GormEvidenceStore) GetByAttemptAndID(ctx context.Context, attemptID, evidenceID string) (*AttemptEvidenceItem, error) {
	var item AttemptEvidenceItem
	if err := s.db.WithContext(ctx).Where("attempt_id = ? AND id = ?", attemptID, evidenceID).First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrEvidenceNotFound
		}
		return nil, err
	}
	return &item, nil
}

func (s *GormEvidenceStore) FindCanonical(ctx context.Context, attemptID, snapshotID, filePath string, startLine, endLine int, rawHash string) (*AttemptEvidenceItem, error) {
	var item AttemptEvidenceItem
	err := s.db.WithContext(ctx).Where("attempt_id = ? AND snapshot_id = ? AND file_path = ? AND start_line = ? AND end_line = ? AND raw_content_hash = ?", attemptID, snapshotID, filePath, startLine, endLine, rawHash).First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *GormEvidenceStore) ListByAttempt(ctx context.Context, attemptID string) ([]AttemptEvidenceItem, error) {
	var items []AttemptEvidenceItem
	err := s.db.WithContext(ctx).Where("attempt_id = ?", attemptID).Order("created_at ASC, id ASC").Find(&items).Error
	return items, err
}

func (s *GormEvidenceStore) FindByID(ctx context.Context, evidenceID string) (*AttemptEvidenceItem, error) {
	var item AttemptEvidenceItem
	if err := s.db.WithContext(ctx).Where("id = ?", evidenceID).First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrEvidenceNotFound
		}
		return nil, err
	}
	return &item, nil
}

type EvidenceIssuer interface {
	Issue(ctx context.Context, req IssueRequest) (*AttemptEvidenceItem, error)
	Resolve(ctx context.Context, attemptID, evidenceID string) (*AttemptEvidenceItem, error)
}

type EvidenceIssuerService struct {
	db      *gorm.DB
	storeFS snapshotstore.SnapshotStore
	store   EvidenceStore
}

func NewEvidenceIssuer(db *gorm.DB, storeFS snapshotstore.SnapshotStore) *EvidenceIssuerService {
	return &EvidenceIssuerService{db: db, storeFS: storeFS, store: NewEvidenceStore(db)}
}

func NewEvidenceIssuerWithStore(storeFS snapshotstore.SnapshotStore, store EvidenceStore) *EvidenceIssuerService {
	return &EvidenceIssuerService{storeFS: storeFS, store: store}
}

func (s *EvidenceIssuerService) Issue(ctx context.Context, req IssueRequest) (*AttemptEvidenceItem, error) {
	if err := validateIssueRequest(req); err != nil {
		return nil, err
	}
	if err := s.validateLineage(ctx, req); err != nil {
		return nil, err
	}
	if s.storeFS == nil || s.store == nil {
		return nil, ErrEvidenceSourceUnavailable
	}

	fileRange, err := s.storeFS.ReadFileRange(ctx, req.RepositoryID, req.SnapshotID, req.FilePath, req.StartLine, req.EndLine, req.MaxBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEvidenceSourceUnavailable, err)
	}
	rawHash := sha256Hex(fileRange.Content)
	if existing, findErr := s.store.FindCanonical(ctx, req.AttemptID, req.SnapshotID, fileRange.Path, fileRange.StartLine, fileRange.EndLine, rawHash); findErr != nil {
		return nil, findErr
	} else if existing != nil {
		return existing, nil
	}

	display := redaction.RedactSecrets(fileRange.Content)
	item := &AttemptEvidenceItem{
		ID:               "ev_" + uuid.NewString(),
		AttemptID:        req.AttemptID,
		DiagnosisRunID:   req.DiagnosisRunID,
		SnapshotID:       req.SnapshotID,
		CodeIndexBuildID: req.CodeIndexBuildID,
		SourceKind:       req.SourceKind,
		RetrievalChunkID: req.RetrievalChunkID,
		FilePath:         fileRange.Path,
		StartLine:        fileRange.StartLine,
		EndLine:          fileRange.EndLine,
		TotalLines:       fileRange.TotalLines,
		RawContentHash:   rawHash,
		DisplayExcerpt:   display,
		RedactionApplied: display != fileRange.Content,
		Truncated:        fileRange.Truncated,
		CreatedAt:        time.Now().UTC(),
	}
	if req.SourceStepSeq > 0 {
		seq := req.SourceStepSeq
		item.SourceStepSeq = &seq
	}
	if err := s.store.Create(ctx, item); err != nil {
		if existing, findErr := s.store.FindCanonical(ctx, req.AttemptID, req.SnapshotID, fileRange.Path, fileRange.StartLine, fileRange.EndLine, rawHash); findErr == nil && existing != nil {
			return existing, nil
		}
		return nil, fmt.Errorf("persist evidence item: %w", err)
	}
	return item, nil
}

func (s *EvidenceIssuerService) Resolve(ctx context.Context, attemptID, evidenceID string) (*AttemptEvidenceItem, error) {
	if s.store == nil || attemptID == "" || evidenceID == "" {
		return nil, ErrEvidenceNotFound
	}
	item, err := s.store.GetByAttemptAndID(ctx, attemptID, evidenceID)
	if err != nil {
		if errors.Is(err, ErrEvidenceNotFound) {
			if finder, ok := s.store.(interface {
				FindByID(context.Context, string) (*AttemptEvidenceItem, error)
			}); ok {
				foreign, findErr := finder.FindByID(ctx, evidenceID)
				if findErr == nil && foreign != nil && foreign.AttemptID != attemptID {
					return nil, ErrEvidenceAttemptMismatch
				}
			}
		}
		return nil, err
	}
	return item, nil
}

func (s *EvidenceIssuerService) ListByAttempt(ctx context.Context, attemptID string) ([]AttemptEvidenceItem, error) {
	lister, ok := s.store.(interface {
		ListByAttempt(context.Context, string) ([]AttemptEvidenceItem, error)
	})
	if !ok {
		return nil, ErrEvidenceSourceUnavailable
	}
	return lister.ListByAttempt(ctx, attemptID)
}

// FindByID is intentionally separate from Resolve. It is used by evaluation
// diagnostics to distinguish an unknown opaque handle from a handle issued to
// another attempt; normal report resolution remains attempt-scoped.
func (s *EvidenceIssuerService) FindByID(ctx context.Context, evidenceID string) (*AttemptEvidenceItem, error) {
	lister, ok := s.store.(interface {
		FindByID(context.Context, string) (*AttemptEvidenceItem, error)
	})
	if !ok {
		return nil, ErrEvidenceSourceUnavailable
	}
	return lister.FindByID(ctx, evidenceID)
}

func (s *EvidenceIssuerService) Verify(ctx context.Context, item *AttemptEvidenceItem, lineage DraftLineage) error {
	if item == nil {
		return ErrEvidenceNotFound
	}
	if item.AttemptID != lineage.AttemptID {
		return ErrEvidenceAttemptMismatch
	}
	if item.DiagnosisRunID != lineage.DiagnosisRunID || item.SnapshotID != lineage.SnapshotID || item.CodeIndexBuildID != lineage.CodeIndexBuildID {
		return ErrEvidenceLineageMismatch
	}
	if s.storeFS == nil {
		return ErrEvidenceSourceUnavailable
	}
	fileRange, err := s.storeFS.ReadFileRange(ctx, lineage.RepositoryID, item.SnapshotID, item.FilePath, item.StartLine, item.EndLine, 0)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrEvidenceSourceUnavailable, err)
	}
	if sha256Hex(fileRange.Content) != item.RawContentHash || fileRange.StartLine != item.StartLine || fileRange.EndLine != item.EndLine {
		return ErrEvidenceHashMismatch
	}
	return nil
}

func (s *EvidenceIssuerService) validateLineage(ctx context.Context, req IssueRequest) error {
	if s.db == nil {
		return nil
	}
	var row struct {
		DiagnosisRunID   string
		RepositoryID     string
		SnapshotID       string
		CodeIndexBuildID int64
	}
	err := s.db.WithContext(ctx).Table("diagnosis_attempts AS a").Select("a.diagnosis_run_id, d.repository_id, d.snapshot_id, d.code_index_build_id").Joins("JOIN diagnosis_runs AS d ON d.id = a.diagnosis_run_id").Where("a.id = ?", req.AttemptID).Scan(&row).Error
	if err != nil {
		return fmt.Errorf("validate evidence lineage: %w", err)
	}
	if row.DiagnosisRunID == "" {
		return ErrEvidenceNotFound
	}
	if row.DiagnosisRunID != req.DiagnosisRunID || row.RepositoryID != req.RepositoryID || row.SnapshotID != req.SnapshotID || row.CodeIndexBuildID != req.CodeIndexBuildID {
		return ErrEvidenceLineageMismatch
	}
	return nil
}

func validateIssueRequest(req IssueRequest) error {
	if req.AttemptID == "" || req.DiagnosisRunID == "" || req.RepositoryID == "" || req.SnapshotID == "" || req.FilePath == "" {
		return ErrEvidenceSourceUnavailable
	}
	switch req.SourceKind {
	case SourceInitialRetrieval, SourceSearchCode, SourceReadFile:
	default:
		return fmt.Errorf("unsupported evidence source kind %q", req.SourceKind)
	}
	return nil
}

func sha256Hex(content string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
}
