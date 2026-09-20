package snapshot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/jobs"
)

type Store interface {
	Create(ctx context.Context, s *RepositorySnapshot) error
	GetByID(ctx context.Context, id string) (*RepositorySnapshot, error)
	GetLatestReady(ctx context.Context, repoID string) (*RepositorySnapshot, error)
	GetByCommit(ctx context.Context, repoID, commitSHA string) (*RepositorySnapshot, error)
	UpdateStatus(ctx context.Context, id string, expectedOldStatus, newStatus SnapshotStatus, readyAt *time.Time) error
}

// MaterializationFinalizer is implemented by the SQL-backed store.  It keeps
// the identity fields and READY transition in one conditional update so a
// partially materialized directory can never be advertised as a snapshot.
type MaterializationFinalizer interface {
	FinalizeMaterialization(ctx context.Context, id, commitSHA, contentHash string, fileCount int, totalBytes int64, readyAt time.Time) error
	FailMaterialization(ctx context.Context, id, errorCode string) error
}

type ClaimedMaterializationFinalizer interface {
	FinalizeSnapshotSuccess(ctx context.Context, jobID int64, workerID, claimToken, snapshotID, commitSHA, contentHash string, fileCount int, totalBytes int64, readyAt time.Time) error
}

type ClaimedMaterializationRevisionFinalizer interface {
	FinalizeSnapshotSuccessWithRevision(ctx context.Context, jobID int64, workerID, claimToken, snapshotID, revisionID, modulePath, commitSHA, contentHash string, fileCount int, totalBytes int64, readyAt time.Time) error
}

type GormStore struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *GormStore {
	return &GormStore{db: db}
}

func (s *GormStore) Create(ctx context.Context, snap *RepositorySnapshot) error {
	if snap.ID == "" {
		snap.ID = uuid.New().String()
	}
	return s.db.WithContext(ctx).Create(snap).Error
}

func (s *GormStore) GetByID(ctx context.Context, id string) (*RepositorySnapshot, error) {
	var snap RepositorySnapshot
	if err := s.db.WithContext(ctx).First(&snap, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *GormStore) GetLatestReady(ctx context.Context, repoID string) (*RepositorySnapshot, error) {
	var snap RepositorySnapshot
	if err := s.db.WithContext(ctx).Where("repository_id = ? AND status = ?", repoID, StatusReady).Order("created_at DESC").First(&snap).Error; err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *GormStore) GetByCommit(ctx context.Context, repoID, commitSHA string) (*RepositorySnapshot, error) {
	var snap RepositorySnapshot
	if err := s.db.WithContext(ctx).Where("repository_id = ? AND commit_sha = ?", repoID, commitSHA).First(&snap).Error; err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *GormStore) UpdateStatus(ctx context.Context, id string, expectedOldStatus, newStatus SnapshotStatus, readyAt *time.Time) error {
	if expectedOldStatus == StatusReady || (expectedOldStatus != StatusCreated && expectedOldStatus != StatusMaterializing) {
		return fmt.Errorf("snapshot %s has immutable or invalid source state %s", id, expectedOldStatus)
	}
	if expectedOldStatus == StatusCreated && newStatus != StatusMaterializing || expectedOldStatus == StatusMaterializing && newStatus != StatusReady && newStatus != StatusFailed {
		return fmt.Errorf("invalid snapshot transition %s -> %s", expectedOldStatus, newStatus)
	}
	updates := map[string]interface{}{
		"status": newStatus,
	}
	if readyAt != nil {
		updates["ready_at"] = readyAt
	}

	result := s.db.WithContext(ctx).Model(&RepositorySnapshot{}).
		Where("id = ? AND status = ?", id, expectedOldStatus).
		Updates(updates)

	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("snapshot %s status transition conflict: expected %s", id, expectedOldStatus)
	}
	return nil
}

func (s *GormStore) FinalizeMaterialization(ctx context.Context, id, commitSHA, contentHash string, fileCount int, totalBytes int64, readyAt time.Time) error {
	if commitSHA == "" || commitSHA == "pending" || contentHash == "" {
		return fmt.Errorf("snapshot %s cannot become READY without exact commit and content hash", id)
	}
	result := s.db.WithContext(ctx).Model(&RepositorySnapshot{}).
		Where("id = ? AND status = ?", id, StatusMaterializing).
		Updates(map[string]interface{}{
			"commit_sha":   commitSHA,
			"content_hash": contentHash,
			"file_count":   fileCount,
			"total_bytes":  totalBytes,
			"status":       StatusReady,
			"ready_at":     readyAt,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("snapshot %s materialization finalize conflict", id)
	}
	return nil
}

func (s *GormStore) FinalizeSnapshotSuccess(ctx context.Context, jobID int64, workerID, claimToken, snapshotID, commitSHA, contentHash string, fileCount int, totalBytes int64, readyAt time.Time) error {
	if commitSHA == "" || commitSHA == "pending" || contentHash == "" {
		return fmt.Errorf("snapshot %s cannot become READY without exact commit and content hash", snapshotID)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job jobs.AnalysisJob
		if err := tx.Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ? AND cancel_requested = ?", jobID, jobs.StatusRunning, workerID, claimToken, false).First(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return jobs.ErrOwnershipLost
			}
			return err
		}
		result := tx.Model(&RepositorySnapshot{}).Where("id = ? AND status = ?", snapshotID, StatusMaterializing).Updates(map[string]interface{}{
			"commit_sha": commitSHA, "content_hash": contentHash, "file_count": fileCount,
			"total_bytes": totalBytes, "status": StatusReady, "ready_at": readyAt,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("snapshot %s materialization finalize conflict", snapshotID)
		}
		jobResult := tx.Model(&jobs.AnalysisJob{}).Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ? AND cancel_requested = ?", jobID, jobs.StatusRunning, workerID, claimToken, false).Updates(map[string]interface{}{
			"status": jobs.StatusSucceeded, "finished_at": readyAt, "updated_at": readyAt,
		})
		if jobResult.Error != nil {
			return jobResult.Error
		}
		if jobResult.RowsAffected != 1 {
			return jobs.ErrOwnershipLost
		}
		return nil
	})
}

// FinalizeSnapshotSuccessWithRevision atomically publishes a snapshot,
// advances its AnalysisRevision, and completes the claimed job.
func (s *GormStore) FinalizeSnapshotSuccessWithRevision(ctx context.Context, jobID int64, workerID, claimToken, snapshotID, revisionID, modulePath, commitSHA, contentHash string, fileCount int, totalBytes int64, readyAt time.Time) error {
	if commitSHA == "" || commitSHA == "pending" || contentHash == "" {
		return fmt.Errorf("snapshot %s cannot become READY without exact commit and content hash", snapshotID)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job jobs.AnalysisJob
		if err := tx.Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ? AND cancel_requested = ?", jobID, jobs.StatusRunning, workerID, claimToken, false).First(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return jobs.ErrOwnershipLost
			}
			return err
		}
		result := tx.Model(&RepositorySnapshot{}).Where("id = ? AND status = ? AND analysis_revision_id = ?", snapshotID, StatusMaterializing, revisionID).Updates(map[string]interface{}{
			"commit_sha": commitSHA, "content_hash": contentHash, "file_count": fileCount,
			"total_bytes": totalBytes, "status": StatusReady, "ready_at": readyAt,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("snapshot %s materialization finalize conflict", snapshotID)
		}

		var existingCodeIndexID sql.NullInt64
		if err := tx.Raw("SELECT code_index_build_id FROM analysis_revisions WHERE id = ? AND status = ? AND snapshot_id = ?", revisionID, "PREPARING", snapshotID).Scan(&existingCodeIndexID).Error; err != nil {
			return err
		}
		codeIndexID := int64(0)
		if existingCodeIndexID.Valid {
			codeIndexID = existingCodeIndexID.Int64
		}
		if codeIndexID != 0 {
			var existingBuild codeintelmodel.CodeIndexBuild
			if err := tx.First(&existingBuild, "id = ?", codeIndexID).Error; err != nil {
				return err
			}
			if existingBuild.AnalysisRevisionID != revisionID || existingBuild.SnapshotID != snapshotID {
				return fmt.Errorf("analysis revision lineage is incomplete or inconsistent")
			}
		}
		if codeIndexID == 0 {
			bc := codeintelmodel.DefaultBuildContext()
			build := &codeintelmodel.CodeIndexBuild{
				AnalysisRevisionID:  revisionID,
				SnapshotID:          snapshotID,
				ParserVersion:       codeintelmodel.CurrentParserVersion,
				AnalyzerVersion:     codeintelmodel.CurrentAnalyzerVersion,
				SymbolSchemaVersion: codeintelmodel.CurrentSymbolSchemaVersion,
				BuildContextHash:    bc.BuildContextHash(),
				ModulePath:          modulePath,
				GOOS:                bc.GOOS,
				GOARCH:              bc.GOARCH,
				BuildTagsHash:       bc.BuildTagsHash(),
				Status:              codeintelmodel.BuildStatusCreated,
				CreatedAt:           readyAt,
			}
			if err := tx.Create(build).Error; err != nil {
				return err
			}
			codeIndexID = build.ID
		}

		codeIndexJob := &jobs.AnalysisJob{}
		jobLookup := tx.Where("job_type = ? AND resource_id = ?", jobs.JobTypeBuildCodeIndex, fmt.Sprintf("%d", codeIndexID)).First(codeIndexJob)
		if errors.Is(jobLookup.Error, gorm.ErrRecordNotFound) {
			if err := tx.Create(&jobs.AnalysisJob{
				JobType: jobs.JobTypeBuildCodeIndex, ResourceID: fmt.Sprintf("%d", codeIndexID),
				Status: jobs.StatusPending, ExecutionGeneration: job.ExecutionGeneration, MaxAttempts: 3, NextRunAt: readyAt,
			}).Error; err != nil {
				return err
			}
		} else if jobLookup.Error != nil {
			return jobLookup.Error
		}

		revisionResult := tx.Table("analysis_revisions").Where("id = ? AND status = ? AND snapshot_id = ?", revisionID, "PREPARING", snapshotID).Updates(map[string]interface{}{
			"stage": "BUILDING_CODE_INDEX", "code_index_build_id": codeIndexID, "version": gorm.Expr("version + 1"), "updated_at": readyAt,
		})
		if revisionResult.Error != nil {
			return revisionResult.Error
		}
		if revisionResult.RowsAffected != 1 {
			return fmt.Errorf("analysis revision %s lineage transition conflict", revisionID)
		}
		jobResult := tx.Model(&jobs.AnalysisJob{}).Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ? AND cancel_requested = ?", jobID, jobs.StatusRunning, workerID, claimToken, false).Updates(map[string]interface{}{
			"status": jobs.StatusSucceeded, "finished_at": readyAt, "updated_at": readyAt,
		})
		if jobResult.Error != nil {
			return jobResult.Error
		}
		if jobResult.RowsAffected != 1 {
			return jobs.ErrOwnershipLost
		}
		return nil
	})
}

func (s *GormStore) FailMaterialization(ctx context.Context, id, errorCode string) error {
	result := s.db.WithContext(ctx).Model(&RepositorySnapshot{}).
		Where("id = ? AND status = ?", id, StatusMaterializing).
		Updates(map[string]interface{}{"status": StatusFailed, "error_code": errorCode})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("snapshot %s failure transition conflict", id)
	}
	return nil
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) Create(ctx context.Context, repoID, commitSHA, ref, materializedPath string) (*RepositorySnapshot, error) {
	snap := &RepositorySnapshot{
		ID:               uuid.New().String(),
		RepositoryID:     repoID,
		CommitSHA:        commitSHA,
		Ref:              ref,
		MaterializedPath: materializedPath,
		Status:           StatusCreated,
	}
	if err := s.store.Create(ctx, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func (s *Service) Get(ctx context.Context, id string) (*RepositorySnapshot, error) {
	snap, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("snapshot not found")
		}
		return nil, err
	}
	return snap, nil
}
