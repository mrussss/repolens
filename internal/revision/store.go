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
	ErrRefResolution  = errors.New("repository ref could not be resolved")
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
	revision.SnapshotID = snap.ID

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(revision).Error; err != nil {
			return err
		}
		if err := tx.Create(snap).Error; err != nil {
			return err
		}
		job := &jobs.AnalysisJob{
			JobType:             jobs.JobTypeMaterializeSnapshot,
			ResourceID:          snap.ID,
			Status:              jobs.StatusPending,
			ExecutionGeneration: 1,
			MaxAttempts:         3,
			NextRunAt:           now,
		}
		if err := createJobGorm(tx, job); err != nil {
			return err
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
		now := time.Now().UTC()
		value.Status = StatusPreparing
		value.Stage = StageMaterializing
		value.ErrorCode = ""
		value.ErrorMessage = ""
		value.ReadyAt = nil
		value.ExecutionGeneration++
		value.Version++
		value.UpdatedAt = now
		if err := tx.Model(&AnalysisRevision{}).Where("id = ? AND status = ?", id, StatusFailed).Updates(map[string]interface{}{
			"status": StatusPreparing, "stage": StageMaterializing, "error_code": "", "error_message": "",
			"ready_at": nil, "execution_generation": value.ExecutionGeneration, "version": value.Version,
		}).Error; err != nil {
			return err
		}
		var snap snapshot.RepositorySnapshot
		if err := tx.First(&snap, "id = ?", value.SnapshotID).Error; err != nil {
			return err
		}
		if snap.AnalysisRevisionID != value.ID || snap.RepositoryID != value.RepositoryID || snap.CommitSHA != value.CommitSHA {
			return ErrLineage
		}
		if snap.Status != snapshot.StatusReady {
			if err := tx.Model(&snapshot.RepositorySnapshot{}).Where("id = ?", snap.ID).Updates(map[string]interface{}{"status": snapshot.StatusMaterializing, "error_code": "", "ready_at": nil}).Error; err != nil {
				return err
			}
			if value.CodeIndexBuildID > 0 {
				if err := resetCodeBuildTx(tx, value.CodeIndexBuildID); err != nil {
					return err
				}
			}
			if value.RetrievalBuildID > 0 {
				if err := resetRetrievalBuildTx(tx, value.RetrievalBuildID); err != nil {
					return err
				}
			}
			return resetJobTx(tx, jobs.JobTypeMaterializeSnapshot, snap.ID, value.ExecutionGeneration, now)
		}

		if value.CodeIndexBuildID == 0 {
			modulePath, err := repositoryModulePathTx(tx, value.RepositoryID)
			if err != nil {
				return err
			}
			build, err := ensureCodeIndexBuildTx(tx, value.ID, snap.ID, modulePath, now)
			if err != nil {
				return err
			}
			value.CodeIndexBuildID = build.ID
			value.Stage = StageBuildingCode
			if err := tx.Model(&AnalysisRevision{}).Where("id = ?", id).Updates(map[string]interface{}{
				"stage": StageBuildingCode, "code_index_build_id": build.ID, "updated_at": now,
			}).Error; err != nil {
				return err
			}
			return resetJobTx(tx, jobs.JobTypeBuildCodeIndex, fmt.Sprintf("%d", build.ID), value.ExecutionGeneration, now)
		}

		var build codeintelmodel.CodeIndexBuild
		if err := tx.First(&build, "id = ?", value.CodeIndexBuildID).Error; err != nil {
			return err
		}
		if build.SnapshotID != value.SnapshotID || build.AnalysisRevisionID != value.ID {
			return ErrLineage
		}
		if build.Status != codeintelmodel.BuildStatusReady {
			value.Stage = StageBuildingCode
			if err := resetCodeBuildTx(tx, build.ID); err != nil {
				return err
			}
			if err := tx.Model(&AnalysisRevision{}).Where("id = ?", id).Updates(map[string]interface{}{"stage": StageBuildingCode, "updated_at": now}).Error; err != nil {
				return err
			}
			return resetJobTx(tx, jobs.JobTypeBuildCodeIndex, fmt.Sprintf("%d", build.ID), value.ExecutionGeneration, now)
		}

		if value.RetrievalBuildID == 0 {
			rb, err := ensureRetrievalBuildTx(tx, value.ID, build.ID, now)
			if err != nil {
				return err
			}
			value.RetrievalBuildID = rb.ID
			value.Stage = StageBuildingSearch
			if err := tx.Model(&AnalysisRevision{}).Where("id = ?", id).Updates(map[string]interface{}{
				"stage": StageBuildingSearch, "retrieval_build_id": rb.ID, "updated_at": now,
			}).Error; err != nil {
				return err
			}
			return resetJobTx(tx, jobs.JobTypeBuildRetrieval, fmt.Sprintf("%d", rb.ID), value.ExecutionGeneration, now)
		}

		var rb codeintelmodel.RetrievalBuild
		if err := tx.First(&rb, "id = ?", value.RetrievalBuildID).Error; err != nil {
			return err
		}
		if rb.CodeIndexBuildID != value.CodeIndexBuildID || rb.AnalysisRevisionID != value.ID {
			return ErrLineage
		}
		if rb.Status != codeintelmodel.BuildStatusReady {
			value.Stage = StageBuildingSearch
			if err := resetRetrievalBuildTx(tx, rb.ID); err != nil {
				return err
			}
			if err := tx.Model(&AnalysisRevision{}).Where("id = ?", id).Updates(map[string]interface{}{"stage": StageBuildingSearch, "updated_at": now}).Error; err != nil {
				return err
			}
			return resetJobTx(tx, jobs.JobTypeBuildRetrieval, fmt.Sprintf("%d", rb.ID), value.ExecutionGeneration, now)
		}

		value.Status = StatusReady
		value.Stage = StageReady
		value.ReadyAt = &now
		if err := tx.Model(&AnalysisRevision{}).Where("id = ?", id).Updates(map[string]interface{}{
			"status": StatusReady, "stage": StageReady, "ready_at": now, "updated_at": now,
		}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func resetJobTx(tx *gorm.DB, jobType jobs.JobType, resourceID string, generation int, now time.Time) error {
	updates := map[string]interface{}{
		"status": jobs.StatusPending, "execution_generation": generation, "attempt_count": 0,
		"next_run_at": now, "worker_id": nil, "claim_token": nil, "lease_until": nil,
		"cancel_requested": false, "terminal_reason": nil, "last_error_class": nil,
		"last_error_code": nil, "last_error_message": nil, "finished_at": nil,
	}
	result := tx.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobType, resourceID).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return createJobGorm(tx, &jobs.AnalysisJob{
			JobType: jobType, ResourceID: resourceID, Status: jobs.StatusPending,
			ExecutionGeneration: generation, MaxAttempts: 3, NextRunAt: now,
		})
	}
	return nil
}

func resetCodeBuildTx(tx *gorm.DB, buildID int64) error {
	return tx.Model(&codeintelmodel.CodeIndexBuild{}).Where("id = ?", buildID).Updates(map[string]interface{}{
		"status": codeintelmodel.BuildStatusCreated, "error_code": "", "ready_at": nil,
	}).Error
}

func resetRetrievalBuildTx(tx *gorm.DB, buildID int64) error {
	return tx.Model(&codeintelmodel.RetrievalBuild{}).Where("id = ?", buildID).Updates(map[string]interface{}{
		"status": codeintelmodel.BuildStatusCreated, "error_code": "", "artifact_path": "", "artifact_hash": "", "ready_at": nil,
	}).Error
}

func repositoryModulePathTx(tx *gorm.DB, repositoryID string) (string, error) {
	var modulePath string
	if err := tx.Table("repositories").Select("name").Where("id = ?", repositoryID).Scan(&modulePath).Error; err != nil {
		return "", err
	}
	if modulePath == "" {
		modulePath = repositoryID
	}
	return modulePath, nil
}

func ensureCodeIndexBuildTx(tx *gorm.DB, revisionID, snapshotID, modulePath string, now time.Time) (*codeintelmodel.CodeIndexBuild, error) {
	bc := codeintelmodel.DefaultBuildContext()
	var build codeintelmodel.CodeIndexBuild
	err := tx.Where("snapshot_id = ? AND parser_version = ? AND analyzer_version = ? AND symbol_schema_version = ? AND build_context_hash = ?", snapshotID, codeintelmodel.CurrentParserVersion, codeintelmodel.CurrentAnalyzerVersion, codeintelmodel.CurrentSymbolSchemaVersion, bc.BuildContextHash()).First(&build).Error
	if err == nil {
		if build.AnalysisRevisionID == "" {
			if err := tx.Model(&codeintelmodel.CodeIndexBuild{}).Where("id = ?", build.ID).Update("analysis_revision_id", revisionID).Error; err != nil {
				return nil, err
			}
			build.AnalysisRevisionID = revisionID
		}
		return &build, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	build = codeintelmodel.CodeIndexBuild{
		AnalysisRevisionID: revisionID, SnapshotID: snapshotID,
		ParserVersion: codeintelmodel.CurrentParserVersion, AnalyzerVersion: codeintelmodel.CurrentAnalyzerVersion,
		SymbolSchemaVersion: codeintelmodel.CurrentSymbolSchemaVersion, BuildContextHash: bc.BuildContextHash(),
		ModulePath: modulePath, GOOS: bc.GOOS, GOARCH: bc.GOARCH, BuildTagsHash: bc.BuildTagsHash(),
		Status: codeintelmodel.BuildStatusCreated, CreatedAt: now,
	}
	if err := tx.Create(&build).Error; err != nil {
		return nil, err
	}
	return &build, nil
}

func ensureRetrievalBuildTx(tx *gorm.DB, revisionID string, codeIndexBuildID int64, now time.Time) (*codeintelmodel.RetrievalBuild, error) {
	const strategy = "BM25"
	const configHash = "config-v2.1"
	var build codeintelmodel.RetrievalBuild
	err := tx.Where("code_index_build_id = ? AND strategy = ? AND retrieval_version = ? AND tokenizer_version = ? AND config_hash = ?", codeIndexBuildID, strategy, codeintelmodel.CurrentRetrievalVersion, codeintelmodel.CurrentTokenizerVersion, configHash).First(&build).Error
	if err == nil {
		if build.AnalysisRevisionID == "" {
			if err := tx.Model(&codeintelmodel.RetrievalBuild{}).Where("id = ?", build.ID).Update("analysis_revision_id", revisionID).Error; err != nil {
				return nil, err
			}
			build.AnalysisRevisionID = revisionID
		}
		return &build, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	build = codeintelmodel.RetrievalBuild{
		AnalysisRevisionID: revisionID, CodeIndexBuildID: codeIndexBuildID, Strategy: strategy,
		RetrievalVersion: codeintelmodel.CurrentRetrievalVersion, TokenizerVersion: codeintelmodel.CurrentTokenizerVersion,
		ConfigHash: configHash, Status: codeintelmodel.BuildStatusCreated, CreatedAt: now,
	}
	if err := tx.Create(&build).Error; err != nil {
		return nil, err
	}
	return &build, nil
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
