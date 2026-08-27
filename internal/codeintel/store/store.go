package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"repolens/internal/codeintel/model"
	"repolens/internal/jobs"
	"repolens/internal/snapshot"
)

var (
	ErrBuildNotFound        = errors.New("code index build not found")
	ErrBuildLineageMismatch = errors.New("build lineage mismatch: artifacts do not belong to the same snapshot chain")
)

// Store defines database operations for CodeIndexBuild and related entities.
type Store interface {
	GetOrCreateBuild(ctx context.Context, snapshotID, modulePath string, bc model.BuildContext) (*model.CodeIndexBuild, bool, error)
	GetByID(ctx context.Context, id int64) (*model.CodeIndexBuild, error)
	GetBySnapshot(ctx context.Context, snapshotID string) (*model.CodeIndexBuild, error)
	SaveAnalysisResult(ctx context.Context, buildID int64, result *model.AnalysisResult) error
	FinalizeCodeIndexSuccess(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, result *model.AnalysisResult) error
	FailBuild(ctx context.Context, buildID int64, errorCode string) error
	MarkBuildBuilding(ctx context.Context, buildID int64) error
	ListSymbols(ctx context.Context, buildID int64, query string, limit int) ([]*model.Symbol, error)
	GetSymbolByHash(ctx context.Context, buildID int64, symbolKeyHash string) (*model.Symbol, error)
	ListRelationsForSymbol(ctx context.Context, buildID int64, symbolID int64) ([]*model.SymbolRelation, error)
	ListRelatedTests(ctx context.Context, buildID int64, symbolKeyHash string) ([]*model.SymbolRelation, error)

	// RetrievalBuild methods
	GetOrCreateRetrievalBuild(ctx context.Context, codeIndexBuildID int64, strategy string) (*model.RetrievalBuild, bool, error)
	GetRetrievalBuildByID(ctx context.Context, id int64) (*model.RetrievalBuild, error)
	GetRetrievalBuildByCodeIndexBuild(ctx context.Context, codeIndexBuildID int64) (*model.RetrievalBuild, error)
	CompleteRetrievalBuild(ctx context.Context, id int64, artifactPath, artifactHash string, docCount int) error
	FinalizeRetrievalSuccess(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, artifactPath, artifactHash string, docCount int) error
	FailRetrievalBuild(ctx context.Context, id int64, errorCode string) error
	MarkRetrievalBuilding(ctx context.Context, id int64) error

	// Lineage Validation
	ValidateLineage(ctx context.Context, repoID, snapshotID string, codeIndexBuildID, retrievalBuildID int64) error
}

type GormStore struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *GormStore {
	return &GormStore{db: db}
}

func (s *GormStore) GetOrCreateBuild(ctx context.Context, snapshotID, modulePath string, bc model.BuildContext) (*model.CodeIndexBuild, bool, error) {
	ctxHash := bc.BuildContextHash()
	var existing model.CodeIndexBuild

	err := s.db.WithContext(ctx).Where(
		"snapshot_id = ? AND parser_version = ? AND analyzer_version = ? AND symbol_schema_version = ? AND build_context_hash = ?",
		snapshotID, model.CurrentParserVersion, model.CurrentAnalyzerVersion, model.CurrentSymbolSchemaVersion, ctxHash,
	).First(&existing).Error

	if err == nil {
		return &existing, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	// Create new build + AnalysisJob transactionally
	build := &model.CodeIndexBuild{
		SnapshotID:          snapshotID,
		ParserVersion:       model.CurrentParserVersion,
		AnalyzerVersion:     model.CurrentAnalyzerVersion,
		SymbolSchemaVersion: model.CurrentSymbolSchemaVersion,
		BuildContextHash:    ctxHash,
		ModulePath:          modulePath,
		GOOS:                bc.GOOS,
		GOARCH:              bc.GOARCH,
		BuildTagsHash:       bc.BuildTagsHash(),
		Status:              model.BuildStatusCreated,
		CreatedAt:           time.Now().UTC(),
	}

	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(build).Error; err != nil {
			return err
		}

		// Insert corresponding AnalysisJob
		job := &jobs.AnalysisJob{
			JobType:             jobs.JobTypeBuildCodeIndex,
			ResourceID:          fmt.Sprintf("%d", build.ID),
			Status:              jobs.StatusPending,
			ExecutionGeneration: 1,
			AttemptCount:        0,
			MaxAttempts:         3,
			NextRunAt:           time.Now().UTC(),
		}
		return tx.Create(job).Error
	})

	if txErr != nil {
		return nil, false, txErr
	}

	return build, true, nil
}

func (s *GormStore) GetByID(ctx context.Context, id int64) (*model.CodeIndexBuild, error) {
	var build model.CodeIndexBuild
	if err := s.db.WithContext(ctx).First(&build, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrBuildNotFound
		}
		return nil, err
	}
	return &build, nil
}

func (s *GormStore) GetBySnapshot(ctx context.Context, snapshotID string) (*model.CodeIndexBuild, error) {
	var build model.CodeIndexBuild
	err := s.db.WithContext(ctx).
		Where("snapshot_id = ?", snapshotID).
		Order("id DESC").
		First(&build).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrBuildNotFound
		}
		return nil, err
	}
	return &build, nil
}

func (s *GormStore) SaveAnalysisResult(ctx context.Context, buildID int64, res *model.AnalysisResult) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.saveAnalysisResultTx(tx, buildID, res)
	})
}

func (s *GormStore) saveAnalysisResultTx(tx *gorm.DB, buildID int64, res *model.AnalysisResult) error {
	// 1. Batch insert CodeFiles
	fileMap := make(map[string]int64)
	for _, f := range res.Files {
		f.CodeIndexBuildID = buildID
		f.CreatedAt = time.Now().UTC()
		if err := tx.Create(f).Error; err != nil {
			return fmt.Errorf("failed saving code file %s: %w", f.Path, err)
		}
		fileMap[f.Path] = f.ID
	}

	// 2. Batch insert Symbols
	symbolMap := make(map[string]int64)
	for _, sym := range res.Symbols {
		sym.CodeIndexBuildID = buildID
		if fID, ok := fileMap[sym.FilePath]; ok {
			sym.FileID = fID
		}
		if err := tx.Create(sym).Error; err != nil {
			return fmt.Errorf("failed saving symbol %s: %w", sym.Name, err)
		}
		symbolMap[sym.SymbolKeyHash] = sym.ID
	}

	// 3. Batch insert SymbolRelations
	for _, rel := range res.Relations {
		rel.CodeIndexBuildID = buildID
		if rel.FromSymbolKeyHash != "" {
			if fromID, ok := symbolMap[rel.FromSymbolKeyHash]; ok {
				rel.FromSymbolID = &fromID
			}
		}
		if rel.ToSymbolKeyHash != "" {
			if toID, ok := symbolMap[rel.ToSymbolKeyHash]; ok {
				rel.ToSymbolID = &toID
			}
		}
		if fID, ok := fileMap[rel.FilePath]; ok {
			rel.FileID = fID
		}
		if err := tx.Create(rel).Error; err != nil {
			return fmt.Errorf("failed saving symbol relation: %w", err)
		}
	}

	// 4. Update CodeIndexBuild with metrics and READY status
	now := time.Now().UTC()
	result := tx.Model(&model.CodeIndexBuild{}).Where("id = ? AND status = ?", buildID, model.BuildStatusBuilding).Updates(map[string]interface{}{
		"module_path":               res.ModulePath,
		"build_tags_hash":           res.BuildContext.BuildTagsHash(),
		"status":                    model.BuildStatusReady,
		"files_total":               res.Quality.FilesTotal,
		"files_parsed":              res.Quality.FilesParsed,
		"files_failed":              res.Quality.FilesFailed,
		"packages_total":            res.Quality.PackagesTotal,
		"packages_typechecked":      res.Quality.PackagesTypechecked,
		"packages_failed":           res.Quality.PackagesFailed,
		"symbol_count":              len(res.Symbols),
		"semantic_relation_count":   res.Quality.SemanticRelationsCount,
		"syntactic_relation_count":  res.Quality.SyntacticRelationsCount,
		"heuristic_relation_count":  res.Quality.HeuristicRelationsCount,
		"unresolved_relation_count": res.Quality.UnresolvedRelationsCount,
		"ready_at":                  &now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("code index build %d finalize conflict", buildID)
	}
	return nil
}

func (s *GormStore) FinalizeCodeIndexSuccess(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, res *model.AnalysisResult) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireOwnedJob(tx, jobID, workerID, claimToken); err != nil {
			return err
		}
		if err := s.saveAnalysisResultTx(tx, buildID, res); err != nil {
			return err
		}
		return finalizeOwnedJob(tx, jobID, workerID, claimToken)
	})
}

func (s *GormStore) MarkBuildBuilding(ctx context.Context, buildID int64) error {
	result := s.db.WithContext(ctx).Model(&model.CodeIndexBuild{}).
		Where("id = ? AND status IN (?, ?)", buildID, model.BuildStatusCreated, model.BuildStatusBuilding).
		Update("status", model.BuildStatusBuilding)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		build, err := s.GetByID(ctx, buildID)
		if err == nil && build.Status == model.BuildStatusBuilding {
			return nil
		}
		return fmt.Errorf("code index build %d is not claimable", buildID)
	}
	return nil
}

func (s *GormStore) FailBuild(ctx context.Context, buildID int64, errorCode string) error {
	return s.db.WithContext(ctx).Model(&model.CodeIndexBuild{}).Where("id = ? AND status != ?", buildID, model.BuildStatusReady).Updates(map[string]interface{}{
		"status":     model.BuildStatusFailed,
		"error_code": errorCode,
	}).Error
}

func (s *GormStore) MarkRetrievalBuilding(ctx context.Context, id int64) error {
	result := s.db.WithContext(ctx).Model(&model.RetrievalBuild{}).
		Where("id = ? AND status IN (?, ?)", id, model.BuildStatusCreated, model.BuildStatusBuilding).
		Update("status", model.BuildStatusBuilding)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		build, err := s.GetRetrievalBuildByID(ctx, id)
		if err == nil && build.Status == model.BuildStatusBuilding {
			return nil
		}
		return fmt.Errorf("retrieval build %d is not claimable", id)
	}
	return nil
}

func (s *GormStore) ListSymbols(ctx context.Context, buildID int64, query string, limit int) ([]*model.Symbol, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	var symbols []*model.Symbol
	db := s.db.WithContext(ctx).Where("code_index_build_id = ?", buildID)
	if query != "" {
		db = db.Where("name LIKE ? OR qualified_name LIKE ?", "%"+query+"%", "%"+query+"%")
	}
	if err := db.Order("id ASC").Limit(limit).Find(&symbols).Error; err != nil {
		return nil, err
	}
	return symbols, nil
}

func (s *GormStore) GetSymbolByHash(ctx context.Context, buildID int64, symbolKeyHash string) (*model.Symbol, error) {
	var sym model.Symbol
	err := s.db.WithContext(ctx).
		Where("code_index_build_id = ? AND symbol_key_hash = ?", buildID, symbolKeyHash).
		First(&sym).Error
	if err != nil {
		return nil, err
	}
	return &sym, nil
}

func (s *GormStore) ListRelationsForSymbol(ctx context.Context, buildID int64, symbolID int64) ([]*model.SymbolRelation, error) {
	var rels []*model.SymbolRelation
	err := s.db.WithContext(ctx).
		Where("code_index_build_id = ? AND (from_symbol_id = ? OR to_symbol_id = ?)", buildID, symbolID, symbolID).
		Find(&rels).Error
	if err != nil {
		return nil, err
	}
	return rels, nil
}

func (s *GormStore) ListRelatedTests(ctx context.Context, buildID int64, symbolKeyHash string) ([]*model.SymbolRelation, error) {
	var rels []*model.SymbolRelation
	err := s.db.WithContext(ctx).
		Where("code_index_build_id = ? AND relation_type = 'TEST_RELATION' AND from_symbol_key_hash = ?", buildID, symbolKeyHash).
		Find(&rels).Error
	if err != nil {
		return nil, err
	}
	return rels, nil
}

// RetrievalBuild implementation
func (s *GormStore) GetOrCreateRetrievalBuild(ctx context.Context, codeIndexBuildID int64, strategy string) (*model.RetrievalBuild, bool, error) {
	configHash := "config-v2.1"
	var existing model.RetrievalBuild

	err := s.db.WithContext(ctx).Where(
		"code_index_build_id = ? AND strategy = ? AND retrieval_version = ? AND tokenizer_version = ? AND config_hash = ?",
		codeIndexBuildID, strategy, model.CurrentRetrievalVersion, model.CurrentTokenizerVersion, configHash,
	).First(&existing).Error

	if err == nil {
		return &existing, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	build := &model.RetrievalBuild{
		CodeIndexBuildID: codeIndexBuildID,
		Strategy:         strategy,
		RetrievalVersion: model.CurrentRetrievalVersion,
		TokenizerVersion: model.CurrentTokenizerVersion,
		ConfigHash:       configHash,
		ArtifactPath:     "",
		ArtifactHash:     "",
		Status:           model.BuildStatusCreated,
		CreatedAt:        time.Now().UTC(),
	}

	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(build).Error; err != nil {
			return err
		}

		job := &jobs.AnalysisJob{
			JobType:             jobs.JobTypeBuildRetrieval,
			ResourceID:          fmt.Sprintf("%d", build.ID),
			Status:              jobs.StatusPending,
			ExecutionGeneration: 1,
			AttemptCount:        0,
			MaxAttempts:         3,
			NextRunAt:           time.Now().UTC(),
		}
		return tx.Create(job).Error
	})

	if txErr != nil {
		return nil, false, txErr
	}

	return build, true, nil
}

func (s *GormStore) GetRetrievalBuildByID(ctx context.Context, id int64) (*model.RetrievalBuild, error) {
	var build model.RetrievalBuild
	if err := s.db.WithContext(ctx).First(&build, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &build, nil
}

func (s *GormStore) GetRetrievalBuildByCodeIndexBuild(ctx context.Context, codeIndexBuildID int64) (*model.RetrievalBuild, error) {
	var build model.RetrievalBuild
	err := s.db.WithContext(ctx).
		Where("code_index_build_id = ?", codeIndexBuildID).
		Order("id DESC").
		First(&build).Error
	if err != nil {
		return nil, err
	}
	return &build, nil
}

func (s *GormStore) CompleteRetrievalBuild(ctx context.Context, id int64, artifactPath, artifactHash string, docCount int) error {
	if artifactPath == "" || artifactHash == "" {
		return fmt.Errorf("retrieval build %d cannot become READY without an artifact and hash", id)
	}
	now := time.Now().UTC()
	result := s.db.WithContext(ctx).Model(&model.RetrievalBuild{}).Where("id = ? AND status = ?", id, model.BuildStatusBuilding).Updates(map[string]interface{}{
		"status":         model.BuildStatusReady,
		"artifact_path":  artifactPath,
		"artifact_hash":  artifactHash,
		"document_count": docCount,
		"ready_at":       &now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("retrieval build %d finalize conflict", id)
	}
	return nil
}

func (s *GormStore) FinalizeRetrievalSuccess(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, artifactPath, artifactHash string, docCount int) error {
	if artifactPath == "" || artifactHash == "" {
		return fmt.Errorf("retrieval build %d cannot become READY without an artifact and hash", buildID)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireOwnedJob(tx, jobID, workerID, claimToken); err != nil {
			return err
		}
		now := time.Now().UTC()
		result := tx.Model(&model.RetrievalBuild{}).Where("id = ? AND status = ?", buildID, model.BuildStatusBuilding).Updates(map[string]interface{}{
			"status": model.BuildStatusReady, "artifact_path": artifactPath, "artifact_hash": artifactHash,
			"document_count": docCount, "ready_at": &now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("retrieval build %d finalize conflict", buildID)
		}
		return finalizeOwnedJob(tx, jobID, workerID, claimToken)
	})
}

func (s *GormStore) FailRetrievalBuild(ctx context.Context, id int64, errorCode string) error {
	return s.db.WithContext(ctx).Model(&model.RetrievalBuild{}).Where("id = ? AND status != ?", id, model.BuildStatusReady).Updates(map[string]interface{}{
		"status":     model.BuildStatusFailed,
		"error_code": errorCode,
	}).Error
}

func requireOwnedJob(tx *gorm.DB, jobID int64, workerID, claimToken string) error {
	var job jobs.AnalysisJob
	if err := tx.Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ? AND cancel_requested = ?", jobID, jobs.StatusRunning, workerID, claimToken, false).First(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return jobs.ErrOwnershipLost
		}
		return err
	}
	return nil
}

func finalizeOwnedJob(tx *gorm.DB, jobID int64, workerID, claimToken string) error {
	now := time.Now().UTC()
	result := tx.Model(&jobs.AnalysisJob{}).Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ? AND cancel_requested = ?", jobID, jobs.StatusRunning, workerID, claimToken, false).Updates(map[string]interface{}{
		"status": jobs.StatusSucceeded, "finished_at": &now, "updated_at": &now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return jobs.ErrOwnershipLost
	}
	return nil
}

// ValidateLineage verifies the strict lineage invariant:
// snapshot.repository_id == repoID
// code_index_build.snapshot_id == snapshotID
// retrieval_build.code_index_build_id == codeIndexBuildID
func (s *GormStore) ValidateLineage(ctx context.Context, repoID, snapshotID string, codeIndexBuildID, retrievalBuildID int64) error {
	var snap snapshot.RepositorySnapshot
	if err := s.db.WithContext(ctx).First(&snap, "id = ?", snapshotID).Error; err != nil || snap.RepositoryID != repoID || snap.Status != snapshot.StatusReady {
		return fmt.Errorf("%w: snapshot is missing, not READY, or belongs to another repository", ErrBuildLineageMismatch)
	}
	if codeIndexBuildID > 0 {
		var cib model.CodeIndexBuild
		if err := s.db.WithContext(ctx).First(&cib, "id = ?", codeIndexBuildID).Error; err != nil {
			return fmt.Errorf("%w: code_index_build not found", ErrBuildLineageMismatch)
		}
		if cib.SnapshotID != snapshotID {
			return fmt.Errorf("%w: code_index_build.snapshot_id (%s) != requested snapshot_id (%s)", ErrBuildLineageMismatch, cib.SnapshotID, snapshotID)
		}
	}

	if retrievalBuildID > 0 {
		var rb model.RetrievalBuild
		if err := s.db.WithContext(ctx).First(&rb, "id = ?", retrievalBuildID).Error; err != nil {
			return fmt.Errorf("%w: retrieval_build not found", ErrBuildLineageMismatch)
		}
		if codeIndexBuildID > 0 && rb.CodeIndexBuildID != codeIndexBuildID {
			return fmt.Errorf("%w: retrieval_build.code_index_build_id (%d) != code_index_build_id (%d)", ErrBuildLineageMismatch, rb.CodeIndexBuildID, codeIndexBuildID)
		}
	}

	return nil
}
