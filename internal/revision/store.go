package revision

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/jobs"
	"repolens/internal/snapshot"
)

var (
	ErrNotFound       = errors.New("analysis revision not found")
	ErrFailedRevision = errors.New("analysis revision already failed; explicit retry is required")
	ErrInvalidState   = errors.New("invalid analysis revision state transition")
	ErrLineage        = errors.New("analysis revision lineage is incomplete or inconsistent")
)

type PrepareSpec struct {
	RepositoryID        string
	SourceRef           string
	CommitSHA           string
	PipelineVersion     string
	PipelineFingerprint string
	SnapshotBasePath    string
	ModulePath          string
}

type Store interface {
	GetByID(ctx context.Context, id string) (*AnalysisRevision, error)
	GetByIDAndRepository(ctx context.Context, id, repositoryID string) (*AnalysisRevision, error)
	GetByIdentity(ctx context.Context, repositoryID, commitSHA, pipelineFingerprint string) (*AnalysisRevision, error)
	ListByRepository(ctx context.Context, repositoryID string, limit int) ([]AnalysisRevision, error)
	CreatePreparation(ctx context.Context, spec PrepareSpec) (*AnalysisRevision, error)
	Retry(ctx context.Context, id string) (*AnalysisRevision, error)
	MarkSnapshotReady(ctx context.Context, id, snapshotID string) error
	MarkCodeIndexReady(ctx context.Context, id string, buildID int64) error
	MarkRetrievalReady(ctx context.Context, id string, buildID int64) error
	MarkFailed(ctx context.Context, id string, stage Stage, code, message string) error
}

type GormStore struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *GormStore { return &GormStore{db: db} }

func (s *GormStore) GetByID(ctx context.Context, id string) (*AnalysisRevision, error) {
	var value AnalysisRevision
	if err := s.db.WithContext(ctx).First(&value, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &value, nil
}

func (s *GormStore) GetByIDAndRepository(ctx context.Context, id, repositoryID string) (*AnalysisRevision, error) {
	var value AnalysisRevision
	if err := s.db.WithContext(ctx).Where("id = ? AND repository_id = ?", id, repositoryID).First(&value).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &value, nil
}

func (s *GormStore) GetByIdentity(ctx context.Context, repositoryID, commitSHA, pipelineFingerprint string) (*AnalysisRevision, error) {
	var value AnalysisRevision
	if err := s.db.WithContext(ctx).Where("repository_id = ? AND commit_sha = ? AND pipeline_fingerprint = ?", repositoryID, commitSHA, pipelineFingerprint).First(&value).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &value, nil
}

func (s *GormStore) ListByRepository(ctx context.Context, repositoryID string, limit int) ([]AnalysisRevision, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var values []AnalysisRevision
	if err := s.db.WithContext(ctx).Where("repository_id = ?", repositoryID).Order("created_at DESC").Limit(limit).Find(&values).Error; err != nil {
		return nil, err
	}
	return values, nil
}

func (s *GormStore) CreatePreparation(ctx context.Context, spec PrepareSpec) (*AnalysisRevision, error) {
	if spec.RepositoryID == "" || len(spec.CommitSHA) != 40 || spec.PipelineFingerprint == "" {
		return nil, fmt.Errorf("invalid analysis revision preparation identity")
	}
	now := time.Now().UTC()
	revision := &AnalysisRevision{
		ID:                  uuid.New().String(),
		RepositoryID:        spec.RepositoryID,
		SourceRef:           spec.SourceRef,
		CommitSHA:           spec.CommitSHA,
		PipelineVersion:     spec.PipelineVersion,
		PipelineFingerprint: spec.PipelineFingerprint,
		Status:              StatusPreparing,
		Stage:               StageMaterializing,
		ExecutionGeneration: 1,
		Version:             1,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	snapshotID := uuid.New().String()
	snap := &snapshot.RepositorySnapshot{
		ID:                 snapshotID,
		RepositoryID:       spec.RepositoryID,
		AnalysisRevisionID: revision.ID,
		CommitSHA:          spec.CommitSHA,
		Ref:                spec.SourceRef,
		RequestedRef:       spec.SourceRef,
		MaterializedPath:   filepath.Join(spec.SnapshotBasePath, spec.RepositoryID, snapshotID, "source"),
		Status:             snapshot.StatusMaterializing,
		CreatedAt:          now,
	}
	bc := codeintelmodel.DefaultBuildContext()
	build := &codeintelmodel.CodeIndexBuild{
		AnalysisRevisionID:  revision.ID,
		SnapshotID:          snap.ID,
		ParserVersion:       codeintelmodel.CurrentParserVersion,
		AnalyzerVersion:     codeintelmodel.CurrentAnalyzerVersion,
		SymbolSchemaVersion: codeintelmodel.CurrentSymbolSchemaVersion,
		BuildContextHash:    bc.BuildContextHash(),
		ModulePath:          spec.ModulePath,
		GOOS:                bc.GOOS,
		GOARCH:              bc.GOARCH,
		BuildTagsHash:       bc.BuildTagsHash(),
		Status:              codeintelmodel.BuildStatusCreated,
		CreatedAt:           now,
	}
	retrievalBuild := &codeintelmodel.RetrievalBuild{
		AnalysisRevisionID: revision.ID,
		CodeIndexBuildID:   build.ID,
		// The current production adapter is Pure Go BM25 plus structural
		// expansion; BM25 remains the persisted build strategy for v2.1
		// compatibility and avoids creating a duplicate derived build.
		Strategy:         "BM25",
		RetrievalVersion: codeintelmodel.CurrentRetrievalVersion,
		TokenizerVersion: codeintelmodel.CurrentTokenizerVersion,
		ConfigHash:       "config-v2.1",
		Status:           codeintelmodel.BuildStatusCreated,
		CreatedAt:        now,
	}
	revision.SnapshotID = snap.ID
	revision.CodeIndexBuildID = 0
	revision.RetrievalBuildID = 0

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(revision).Error; err != nil {
			return err
		}
		if err := tx.Create(snap).Error; err != nil {
			return err
		}
		if err := tx.Create(build).Error; err != nil {
			return err
		}
		retrievalBuild.CodeIndexBuildID = build.ID
		if err := tx.Create(retrievalBuild).Error; err != nil {
			return err
		}
		revision.CodeIndexBuildID = build.ID
		revision.RetrievalBuildID = retrievalBuild.ID
		if err := tx.Model(&AnalysisRevision{}).Where("id = ?", revision.ID).Updates(map[string]interface{}{
			"code_index_build_id": build.ID,
			"retrieval_build_id":  retrievalBuild.ID,
		}).Error; err != nil {
			return err
		}
		jobsToCreate := []*jobs.AnalysisJob{
			{JobType: jobs.JobTypeMaterializeSnapshot, ResourceID: snap.ID},
			{JobType: jobs.JobTypeBuildCodeIndex, ResourceID: fmt.Sprintf("%d", build.ID)},
			{JobType: jobs.JobTypeBuildRetrieval, ResourceID: fmt.Sprintf("%d", retrievalBuild.ID)},
		}
		for _, job := range jobsToCreate {
			job.Status = jobs.StatusPending
			job.ExecutionGeneration = 1
			job.MaxAttempts = 3
			job.NextRunAt = now
			if err := createJobGorm(tx, job); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return revision, nil
}

func createJobGorm(tx *gorm.DB, job *jobs.AnalysisJob) error {
	return tx.Create(job).Error
}

func (s *GormStore) Retry(ctx context.Context, id string) (*AnalysisRevision, error) {
	var value AnalysisRevision
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ?", id).First(&value).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if value.Status != StatusFailed {
			return ErrInvalidState
		}
		value.Status = StatusPreparing
		value.Stage = StageMaterializing
		value.ErrorCode = ""
		value.ErrorMessage = ""
		value.ExecutionGeneration++
		value.Version++
		value.UpdatedAt = time.Now().UTC()
		if err := tx.Model(&AnalysisRevision{}).Where("id = ? AND status = ?", id, StatusFailed).Updates(map[string]interface{}{
			"status": StatusPreparing, "stage": StageMaterializing, "error_code": "", "error_message": "",
			"execution_generation": value.ExecutionGeneration, "version": value.Version,
		}).Error; err != nil {
			return err
		}
		var snap snapshot.RepositorySnapshot
		if err := tx.First(&snap, "id = ?", value.SnapshotID).Error; err != nil {
			return err
		}
		var build codeintelmodel.CodeIndexBuild
		if err := tx.First(&build, "id = ?", value.CodeIndexBuildID).Error; err != nil {
			return err
		}
		var rb codeintelmodel.RetrievalBuild
		if err := tx.First(&rb, "id = ?", value.RetrievalBuildID).Error; err != nil {
			return err
		}
		if snap.Status != snapshot.StatusReady {
			_ = tx.Model(&snapshot.RepositorySnapshot{}).Where("id = ?", snap.ID).Updates(map[string]interface{}{"status": snapshot.StatusMaterializing, "error_code": ""})
			_ = tx.Model(&codeintelmodel.CodeIndexBuild{}).Where("id = ?", build.ID).Updates(map[string]interface{}{"status": codeintelmodel.BuildStatusCreated, "error_code": ""})
			_ = tx.Model(&codeintelmodel.RetrievalBuild{}).Where("id = ?", rb.ID).Updates(map[string]interface{}{"status": codeintelmodel.BuildStatusCreated, "error_code": ""})
		} else if build.Status != codeintelmodel.BuildStatusReady {
			value.Stage = StageBuildingCode
			_ = tx.Model(&AnalysisRevision{}).Where("id = ?", id).Updates(map[string]interface{}{"stage": StageBuildingCode})
			_ = tx.Model(&codeintelmodel.CodeIndexBuild{}).Where("id = ?", build.ID).Updates(map[string]interface{}{"status": codeintelmodel.BuildStatusCreated, "error_code": ""})
			_ = tx.Model(&codeintelmodel.RetrievalBuild{}).Where("id = ?", rb.ID).Updates(map[string]interface{}{"status": codeintelmodel.BuildStatusCreated, "error_code": ""})
		} else if rb.Status != codeintelmodel.BuildStatusReady {
			value.Stage = StageBuildingSearch
			_ = tx.Model(&AnalysisRevision{}).Where("id = ?", id).Updates(map[string]interface{}{"stage": StageBuildingSearch})
			_ = tx.Model(&codeintelmodel.RetrievalBuild{}).Where("id = ?", rb.ID).Updates(map[string]interface{}{"status": codeintelmodel.BuildStatusCreated, "error_code": ""})
		} else {
			value.Status = StatusReady
			value.Stage = StageReady
			now := time.Now().UTC()
			value.ReadyAt = &now
			_ = tx.Model(&AnalysisRevision{}).Where("id = ?", id).Updates(map[string]interface{}{"status": StatusReady, "stage": StageReady, "ready_at": now})
		}
		for _, item := range []struct {
			typ      jobs.JobType
			resource string
		}{
			{jobs.JobTypeMaterializeSnapshot, snap.ID},
			{jobs.JobTypeBuildCodeIndex, fmt.Sprintf("%d", build.ID)},
			{jobs.JobTypeBuildRetrieval, fmt.Sprintf("%d", rb.ID)},
		} {
			if err := tx.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", item.typ, item.resource).Updates(map[string]interface{}{
				"status": jobs.StatusPending, "execution_generation": value.ExecutionGeneration, "attempt_count": 0,
				"next_run_at": time.Now().UTC(), "worker_id": nil, "claim_token": nil, "lease_until": nil,
				"cancel_requested": false, "terminal_reason": nil, "last_error_class": nil, "last_error_code": nil,
				"last_error_message": nil, "finished_at": nil,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *GormStore) MarkSnapshotReady(ctx context.Context, id, snapshotID string) error {
	var snap snapshot.RepositorySnapshot
	if err := s.db.WithContext(ctx).First(&snap, "id = ?", snapshotID).Error; err != nil || snap.Status != snapshot.StatusReady {
		return ErrLineage
	}
	now := time.Now().UTC()
	result := s.db.WithContext(ctx).Model(&AnalysisRevision{}).Where("id = ? AND status = ? AND snapshot_id = ?", id, StatusPreparing, snapshotID).Updates(map[string]interface{}{"stage": StageBuildingCode, "version": gorm.Expr("version + 1"), "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLineage
	}
	return nil
}

func (s *GormStore) MarkCodeIndexReady(ctx context.Context, id string, buildID int64) error {
	var value AnalysisRevision
	if err := s.db.WithContext(ctx).First(&value, "id = ?", id).Error; err != nil {
		return err
	}
	var build codeintelmodel.CodeIndexBuild
	if err := s.db.WithContext(ctx).First(&build, "id = ?", buildID).Error; err != nil || build.Status != codeintelmodel.BuildStatusReady || build.SnapshotID != value.SnapshotID || build.AnalysisRevisionID != id {
		return ErrLineage
	}
	result := s.db.WithContext(ctx).Model(&AnalysisRevision{}).Where("id = ? AND status = ? AND code_index_build_id = ?", id, StatusPreparing, buildID).Updates(map[string]interface{}{"stage": StageBuildingSearch, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLineage
	}
	return nil
}

func (s *GormStore) MarkRetrievalReady(ctx context.Context, id string, buildID int64) error {
	var value AnalysisRevision
	if err := s.db.WithContext(ctx).First(&value, "id = ?", id).Error; err != nil {
		return err
	}
	var retrievalBuild codeintelmodel.RetrievalBuild
	if err := s.db.WithContext(ctx).First(&retrievalBuild, "id = ?", buildID).Error; err != nil || retrievalBuild.Status != codeintelmodel.BuildStatusReady || retrievalBuild.AnalysisRevisionID != id || retrievalBuild.CodeIndexBuildID != value.CodeIndexBuildID {
		return ErrLineage
	}
	var codeBuild codeintelmodel.CodeIndexBuild
	if err := s.db.WithContext(ctx).First(&codeBuild, "id = ?", value.CodeIndexBuildID).Error; err != nil || codeBuild.Status != codeintelmodel.BuildStatusReady || codeBuild.SnapshotID != value.SnapshotID {
		return ErrLineage
	}
	now := time.Now().UTC()
	result := s.db.WithContext(ctx).Model(&AnalysisRevision{}).Where("id = ? AND status = ? AND retrieval_build_id = ?", id, StatusPreparing, buildID).Updates(map[string]interface{}{"status": StatusReady, "stage": StageReady, "ready_at": now, "version": gorm.Expr("version + 1"), "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLineage
	}
	return nil
}

func (s *GormStore) MarkFailed(ctx context.Context, id string, stage Stage, code, message string) error {
	result := s.db.WithContext(ctx).Model(&AnalysisRevision{}).Where("id = ? AND status = ?", id, StatusPreparing).Updates(map[string]interface{}{"status": StatusFailed, "stage": stage, "error_code": code, "error_message": message, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrInvalidState
	}
	return nil
}
