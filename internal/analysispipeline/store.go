package analysispipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	sqlite3 "github.com/mattn/go-sqlite3"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/jobs"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
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

type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) CreatePreparation(ctx context.Context, spec PrepareSpec) (*revision.AnalysisRevision, error) {
	if spec.RepositoryID == "" || len(spec.CommitSHA) != 40 || spec.PipelineFingerprint == "" {
		return nil, fmt.Errorf("invalid analysis revision preparation identity")
	}
	now := time.Now().UTC()
	prepared := &revision.AnalysisRevision{
		ID:                  uuid.New().String(),
		RepositoryID:        spec.RepositoryID,
		SourceRef:           spec.SourceRef,
		CommitSHA:           spec.CommitSHA,
		PipelineVersion:     spec.PipelineVersion,
		PipelineFingerprint: spec.PipelineFingerprint,
		Status:              revision.StatusPreparing,
		Stage:               revision.StageMaterializing,
		ExecutionGeneration: 1,
		Version:             1,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	snapshotID := uuid.New().String()
	snap := &snapshot.RepositorySnapshot{
		ID:                 snapshotID,
		RepositoryID:       spec.RepositoryID,
		AnalysisRevisionID: prepared.ID,
		CommitSHA:          spec.CommitSHA,
		Ref:                spec.SourceRef,
		RequestedRef:       spec.SourceRef,
		MaterializedPath:   filepath.Join(spec.SnapshotBasePath, spec.RepositoryID, snapshotID, "source"),
		Status:             snapshot.StatusMaterializing,
		CreatedAt:          now,
	}
	prepared.SnapshotID = snap.ID

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(prepared).Error; err != nil {
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
	return prepared, nil
}

func createJobGorm(tx *gorm.DB, job *jobs.AnalysisJob) error {
	return tx.Create(job).Error
}

func (s *Store) RetryPreparation(ctx context.Context, id string) (*revision.AnalysisRevision, error) {
	maxAttempts := 1
	isSQLite := s.db.Dialector.Name() == "sqlite"
	if isSQLite {
		maxAttempts = 8
	}

	for attempt := 0; ; attempt++ {
		value, err := s.retryPreparationOnce(ctx, id)
		if err == nil || !isSQLite || !isSQLiteWriterContention(err) || attempt+1 >= maxAttempts {
			return value, err
		}

		delay := 5 * time.Millisecond << attempt
		if delay > 40*time.Millisecond {
			delay = 40 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func isSQLiteWriterContention(err error) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code == sqlite3.ErrBusy {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "database is locked")
}

func (s *Store) retryPreparationOnce(ctx context.Context, id string) (*revision.AnalysisRevision, error) {
	var value revision.AnalysisRevision
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(&value).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return revision.ErrNotFound
			}
			return err
		}
		if value.Status != revision.StatusFailed {
			if value.Status == revision.StatusPreparing {
				return revision.ErrRetryConflict
			}
			return revision.ErrInvalidState
		}
		now := time.Now().UTC()
		expectedVersion := value.Version
		expectedGeneration := value.ExecutionGeneration
		newGeneration := expectedGeneration + 1
		newVersion := expectedVersion + 1
		result := tx.Model(&revision.AnalysisRevision{}).Where("id = ? AND status = ? AND version = ? AND execution_generation = ?", id, revision.StatusFailed, expectedVersion, expectedGeneration).Updates(map[string]interface{}{
			"status": revision.StatusPreparing, "stage": revision.StageMaterializing, "error_code": "", "error_message": "",
			"ready_at": nil, "execution_generation": newGeneration, "version": newVersion, "updated_at": now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return revision.ErrRetryConflict
		}
		value.Status = revision.StatusPreparing
		value.Stage = revision.StageMaterializing
		value.ErrorCode = ""
		value.ErrorMessage = ""
		value.ReadyAt = nil
		value.ExecutionGeneration = newGeneration
		value.Version = newVersion
		value.UpdatedAt = now
		var snap snapshot.RepositorySnapshot
		if err := tx.First(&snap, "id = ?", value.SnapshotID).Error; err != nil {
			return err
		}
		if snap.AnalysisRevisionID != value.ID || snap.RepositoryID != value.RepositoryID || snap.CommitSHA != value.CommitSHA {
			return revision.ErrLineage
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
			value.Stage = revision.StageBuildingCode
			if err := tx.Model(&revision.AnalysisRevision{}).Where("id = ?", id).Updates(map[string]interface{}{
				"stage": revision.StageBuildingCode, "code_index_build_id": build.ID, "updated_at": now,
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
			return revision.ErrLineage
		}
		if build.Status != codeintelmodel.BuildStatusReady {
			value.Stage = revision.StageBuildingCode
			if err := resetCodeBuildTx(tx, build.ID); err != nil {
				return err
			}
			if err := tx.Model(&revision.AnalysisRevision{}).Where("id = ?", id).Updates(map[string]interface{}{"stage": revision.StageBuildingCode, "updated_at": now}).Error; err != nil {
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
			value.Stage = revision.StageBuildingSearch
			if err := tx.Model(&revision.AnalysisRevision{}).Where("id = ?", id).Updates(map[string]interface{}{
				"stage": revision.StageBuildingSearch, "retrieval_build_id": rb.ID, "updated_at": now,
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
			return revision.ErrLineage
		}
		if rb.Status != codeintelmodel.BuildStatusReady {
			value.Stage = revision.StageBuildingSearch
			if err := resetRetrievalBuildTx(tx, rb.ID); err != nil {
				return err
			}
			if err := tx.Model(&revision.AnalysisRevision{}).Where("id = ?", id).Updates(map[string]interface{}{"stage": revision.StageBuildingSearch, "updated_at": now}).Error; err != nil {
				return err
			}
			return resetJobTx(tx, jobs.JobTypeBuildRetrieval, fmt.Sprintf("%d", rb.ID), value.ExecutionGeneration, now)
		}

		value.Status = revision.StatusReady
		value.Stage = revision.StageReady
		value.ReadyAt = &now
		if err := tx.Model(&revision.AnalysisRevision{}).Where("id = ?", id).Updates(map[string]interface{}{
			"status": revision.StatusReady, "stage": revision.StageReady, "ready_at": now, "updated_at": now,
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
		"status": jobs.StatusPending, "execution_generation": generation, "attempt_count": 0, "execution_started": false,
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
		if build.AnalysisRevisionID != "" && build.AnalysisRevisionID != revisionID {
			return nil, revision.ErrLineage
		}
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
	const configHash = "config-v2.2"
	var build codeintelmodel.RetrievalBuild
	err := tx.Where("code_index_build_id = ? AND strategy = ? AND retrieval_version = ? AND tokenizer_version = ? AND config_hash = ?", codeIndexBuildID, strategy, codeintelmodel.CurrentRetrievalVersion, codeintelmodel.CurrentTokenizerVersion, configHash).First(&build).Error
	if err == nil {
		if build.AnalysisRevisionID != "" && build.AnalysisRevisionID != revisionID {
			return nil, revision.ErrLineage
		}
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
