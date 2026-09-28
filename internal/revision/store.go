package revision

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/snapshot"
)

var (
	ErrNotFound       = errors.New("analysis revision not found")
	ErrFailedRevision = errors.New("analysis revision already failed; explicit retry is required")
	ErrInvalidState   = errors.New("invalid analysis revision state transition")
	ErrRetryConflict  = errors.New("analysis revision retry conflict")
	ErrLineage        = errors.New("analysis revision lineage is incomplete or inconsistent")
	ErrRefResolution  = errors.New("repository ref could not be resolved")
)

type Store interface {
	GetByID(ctx context.Context, id string) (*AnalysisRevision, error)
	GetByIDAndRepository(ctx context.Context, id, repositoryID string) (*AnalysisRevision, error)
	GetByIdentity(ctx context.Context, repositoryID, commitSHA, pipelineFingerprint string) (*AnalysisRevision, error)
	ListByRepository(ctx context.Context, repositoryID string, limit int) ([]AnalysisRevision, error)
	MarkSnapshotReady(ctx context.Context, id, snapshotID string) error
	MarkCodeIndexReady(ctx context.Context, id string, buildID int64) error
	MarkRetrievalReady(ctx context.Context, id string, buildID int64) error
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

func (s *GormStore) MarkSnapshotReady(ctx context.Context, id, snapshotID string) error {
	var value AnalysisRevision
	if err := s.db.WithContext(ctx).First(&value, "id = ?", id).Error; err != nil {
		return err
	}
	var snap snapshot.RepositorySnapshot
	if err := s.db.WithContext(ctx).First(&snap, "id = ?", snapshotID).Error; err != nil || snap.Status != snapshot.StatusReady || snap.AnalysisRevisionID != id || snap.RepositoryID != value.RepositoryID || snap.CommitSHA != value.CommitSHA {
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
	var snap snapshot.RepositorySnapshot
	if err := s.db.WithContext(ctx).First(&snap, "id = ?", build.SnapshotID).Error; err != nil || snap.AnalysisRevisionID != id || snap.RepositoryID != value.RepositoryID || snap.CommitSHA != value.CommitSHA || snap.Status != snapshot.StatusReady {
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
	if codeBuild.AnalysisRevisionID != id {
		return ErrLineage
	}
	var snap snapshot.RepositorySnapshot
	if err := s.db.WithContext(ctx).First(&snap, "id = ?", codeBuild.SnapshotID).Error; err != nil || snap.AnalysisRevisionID != id || snap.RepositoryID != value.RepositoryID || snap.CommitSHA != value.CommitSHA || snap.Status != snapshot.StatusReady {
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
