package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"repolens/internal/codeintel/model"
	"repolens/internal/jobs"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

var (
	ErrBuildNotFound        = errors.New("code index build not found")
	ErrBuildLineageMismatch = errors.New("build lineage mismatch: artifacts do not belong to the same snapshot chain")
	ErrSymbolNotFound       = errors.New("code symbol not found")
)

// Store defines database operations for CodeIndexBuild and related entities.
type Store interface {
	GetOrCreateBuild(ctx context.Context, snapshotID, modulePath string, bc model.BuildContext) (*model.CodeIndexBuild, bool, error)
	GetByID(ctx context.Context, id int64) (*model.CodeIndexBuild, error)
	GetBySnapshot(ctx context.Context, snapshotID string) (*model.CodeIndexBuild, error)
	SaveAnalysisResult(ctx context.Context, buildID int64, result *model.AnalysisResult) error
	FinalizeCodeIndexSuccess(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, result *model.AnalysisResult) error
	FinalizeCodeIndexSuccessWithRevision(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, revisionID string, result *model.AnalysisResult) error
	FinalizeLegacyCodeIndexSuccessWithRetrievalHandoff(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, result *model.AnalysisResult) error
	FinalizeLegacySnapshotSuccessWithCodeIndexHandoff(ctx context.Context, jobID int64, workerID, claimToken, snapshotID, materializedPath, modulePath, commitSHA, contentHash string, fileCount int, totalBytes int64, readyAt time.Time) error
	MarkBuildBuilding(ctx context.Context, buildID int64) error
	ListSymbols(ctx context.Context, buildID int64, query string, limit int) ([]*model.Symbol, error)
	ListAllSymbols(ctx context.Context, buildID int64) ([]*model.Symbol, error)
	GetSymbolByID(ctx context.Context, id int64) (*model.Symbol, error)
	GetSymbolByHash(ctx context.Context, buildID int64, symbolKeyHash string) (*model.Symbol, error)
	ListRelationsForSymbol(ctx context.Context, buildID int64, symbolID int64) ([]*model.SymbolRelation, error)
	ListRelatedTests(ctx context.Context, buildID int64, symbolKeyHash string) ([]*model.SymbolRelation, error)

	// RetrievalBuild methods
	GetOrCreateRetrievalBuild(ctx context.Context, codeIndexBuildID int64, strategy string) (*model.RetrievalBuild, bool, error)
	GetRetrievalBuildByID(ctx context.Context, id int64) (*model.RetrievalBuild, error)
	GetRetrievalBuildByCodeIndexBuild(ctx context.Context, codeIndexBuildID int64) (*model.RetrievalBuild, error)
	CompleteRetrievalBuild(ctx context.Context, id int64, artifactPath, artifactHash string, docCount int) error
	FinalizeRetrievalSuccess(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, artifactPath, artifactHash string, docCount int) error
	FinalizeRetrievalSuccessWithRevision(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, revisionID, artifactPath, artifactHash string, docCount int) error
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
	contextHash := bc.BuildContextHash()
	if existing, err := s.getBuildByIdentity(ctx, snapshotID, contextHash); err == nil {
		if err := s.ensureAnalysisJob(ctx, jobs.JobTypeBuildCodeIndex, strconv.FormatInt(existing.ID, 10), 1, time.Now().UTC()); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	var build *model.CodeIndexBuild
	var created bool
	now := time.Now().UTC()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var txErr error
		build, created, txErr = s.createBuildTx(tx, snapshotID, modulePath, bc, 1, now)
		return txErr
	})
	if err == nil {
		return build, created, nil
	}

	// A concurrent request may have won the unique build identity. Reuse it
	// only after its corresponding Job is present as well.
	winner, lookupErr := s.getBuildByIdentity(ctx, snapshotID, bc.BuildContextHash())
	if lookupErr != nil {
		return nil, false, err
	}
	if ensureErr := s.ensureAnalysisJob(ctx, jobs.JobTypeBuildCodeIndex, strconv.FormatInt(winner.ID, 10), 1, now); ensureErr != nil {
		return nil, false, ensureErr
	}
	return winner, false, nil
}

func (s *GormStore) getOrCreateBuildTx(tx *gorm.DB, snapshotID, modulePath string, bc model.BuildContext, generation int, createdAt time.Time) (*model.CodeIndexBuild, bool, error) {
	contextHash := bc.BuildContextHash()
	var build model.CodeIndexBuild
	err := tx.Where(
		"snapshot_id = ? AND parser_version = ? AND analyzer_version = ? AND symbol_schema_version = ? AND build_context_hash = ?",
		snapshotID, model.CurrentParserVersion, model.CurrentAnalyzerVersion, model.CurrentSymbolSchemaVersion, contextHash,
	).First(&build).Error
	if err == nil {
		if err := ensureAnalysisJobTx(tx, jobs.JobTypeBuildCodeIndex, strconv.FormatInt(build.ID, 10), generation, createdAt); err != nil {
			return nil, false, err
		}
		return &build, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}
	return s.createBuildTx(tx, snapshotID, modulePath, bc, generation, createdAt)
}

func (s *GormStore) createBuildTx(tx *gorm.DB, snapshotID, modulePath string, bc model.BuildContext, generation int, createdAt time.Time) (*model.CodeIndexBuild, bool, error) {
	contextHash := bc.BuildContextHash()
	var build model.CodeIndexBuild
	buildTags := append([]string{}, bc.BuildTags...)
	tagsJSON, err := json.Marshal(buildTags)
	if err != nil {
		return nil, false, fmt.Errorf("encode code index build tags: %w", err)
	}
	build = model.CodeIndexBuild{
		SnapshotID:          snapshotID,
		ParserVersion:       model.CurrentParserVersion,
		AnalyzerVersion:     model.CurrentAnalyzerVersion,
		SymbolSchemaVersion: model.CurrentSymbolSchemaVersion,
		BuildContextHash:    contextHash,
		ModulePath:          modulePath,
		GOOS:                bc.GOOS,
		GOARCH:              bc.GOARCH,
		BuildTagsHash:       bc.BuildTagsHash(),
		BuildTagsJSON:       string(tagsJSON),
		Status:              model.BuildStatusCreated,
		CreatedAt:           createdAt,
	}
	if err := tx.Create(&build).Error; err != nil {
		return nil, false, err
	}
	if err := ensureAnalysisJobTx(tx, jobs.JobTypeBuildCodeIndex, strconv.FormatInt(build.ID, 10), generation, createdAt); err != nil {
		return nil, false, err
	}
	return &build, true, nil
}

func (s *GormStore) ensureAnalysisJob(ctx context.Context, jobType jobs.JobType, resourceID string, generation int, nextRunAt time.Time) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return ensureAnalysisJobTx(tx, jobType, resourceID, generation, nextRunAt)
	})
}

func ensureAnalysisJobTx(tx *gorm.DB, jobType jobs.JobType, resourceID string, generation int, nextRunAt time.Time) error {
	var existing jobs.AnalysisJob
	err := tx.Where("job_type = ? AND resource_id = ?", jobType, resourceID).First(&existing).Error
	if err == nil {
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if generation < 1 {
		generation = 1
	}
	return tx.Create(&jobs.AnalysisJob{
		JobType:             jobType,
		ResourceID:          resourceID,
		Status:              jobs.StatusPending,
		ExecutionGeneration: generation,
		MaxAttempts:         3,
		NextRunAt:           nextRunAt,
	}).Error
}

func (s *GormStore) getBuildByIdentity(ctx context.Context, snapshotID, contextHash string) (*model.CodeIndexBuild, error) {
	var build model.CodeIndexBuild
	err := s.db.WithContext(ctx).Where(
		"snapshot_id = ? AND parser_version = ? AND analyzer_version = ? AND symbol_schema_version = ? AND build_context_hash = ?",
		snapshotID, model.CurrentParserVersion, model.CurrentAnalyzerVersion, model.CurrentSymbolSchemaVersion, contextHash,
	).First(&build).Error
	if err != nil {
		return nil, err
	}
	return &build, nil
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
	warnings, err := json.Marshal(res.Quality.Warnings)
	if err != nil {
		return fmt.Errorf("failed encoding analysis quality warnings: %w", err)
	}
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
	testRelationKeys := make(map[string]struct{})
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
		if rel.RelationType == model.RelationTypeTestRelation {
			testRelationKeys[relatedTestKey(rel.FromSymbolKeyHash, rel.ToSymbolKeyHash)] = struct{}{}
		}
	}

	// Persist test discovery in the same transaction as files, symbols, and
	// ordinary relations so a CodeIndexBuild cannot become READY with only a
	// partial test relation graph.
	for _, discovery := range res.RelatedTests {
		key := relatedTestKey(discovery.TargetSymbolKeyHash, discovery.TestSymbolKeyHash)
		if _, exists := testRelationKeys[key]; exists {
			continue
		}
		fromID, fromOK := symbolMap[discovery.TargetSymbolKeyHash]
		toID, toOK := symbolMap[discovery.TestSymbolKeyHash]
		fileID, fileOK := fileMap[discovery.TestFilePath]
		if !fromOK || !toOK || !fileOK {
			return fmt.Errorf("related test discovery references missing target, test, or file: target=%s test=%s file=%s", discovery.TargetSymbolKeyHash, discovery.TestSymbolKeyHash, discovery.TestFilePath)
		}
		relation := &model.SymbolRelation{
			CodeIndexBuildID:  buildID,
			FromSymbolID:      &fromID,
			FromSymbolKeyHash: discovery.TargetSymbolKeyHash,
			ToSymbolID:        &toID,
			ToSymbolKeyHash:   discovery.TestSymbolKeyHash,
			RelationType:      model.RelationTypeTestRelation,
			ResolutionKind:    discovery.ResolutionKind,
			Confidence:        discovery.Confidence,
			ReasonCode:        string(discovery.ReasonCode),
			ReasonDetail:      discovery.Explanation,
			TargetName:        discovery.TestSymbolName,
			FilePath:          discovery.TestFilePath,
			FileID:            fileID,
			Line:              discovery.TestLine,
			Column:            1,
		}
		if err := tx.Create(relation).Error; err != nil {
			return fmt.Errorf("failed saving related test %s for symbol %s: %w", discovery.TestSymbolName, discovery.TargetSymbolName, err)
		}
		testRelationKeys[key] = struct{}{}
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
		"symlinks_skipped":          res.Quality.SymlinksSkipped,
		"quality_warnings_json":     string(warnings),
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

func relatedTestKey(targetHash, testHash string) string {
	return targetHash + "\x00" + testHash
}

// FinalizeLegacySnapshotSuccessWithCodeIndexHandoff keeps the legacy Snapshot
// publication, CodeIndexBuild identity, downstream Job, and current Job
// success in one claim-fenced transaction.
func (s *GormStore) FinalizeLegacySnapshotSuccessWithCodeIndexHandoff(ctx context.Context, jobID int64, workerID, claimToken, snapshotID, materializedPath, modulePath, commitSHA, contentHash string, fileCount int, totalBytes int64, readyAt time.Time) error {
	if materializedPath == "" || commitSHA == "" || commitSHA == "pending" || contentHash == "" {
		return fmt.Errorf("snapshot %s cannot become READY without exact commit and content hash", snapshotID)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		owned, err := requireOwnedPipelineJobTx(tx, jobID, workerID, claimToken, jobs.JobTypeMaterializeSnapshot, snapshotID)
		if err != nil {
			return err
		}
		result := tx.Model(&snapshot.RepositorySnapshot{}).
			Where("id = ? AND status = ? AND analysis_revision_id = ''", snapshotID, snapshot.StatusMaterializing).
			Updates(map[string]interface{}{
				"materialized_path": materializedPath,
				"commit_sha":        commitSHA,
				"content_hash":      contentHash,
				"file_count":        fileCount,
				"total_bytes":       totalBytes,
				"status":            snapshot.StatusReady,
				"ready_at":          readyAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("legacy snapshot %s materialization finalize conflict", snapshotID)
		}
		if _, _, err := s.getOrCreateBuildTx(tx, snapshotID, modulePath, model.DefaultBuildContext(), owned.ExecutionGeneration, readyAt); err != nil {
			return fmt.Errorf("create legacy CodeIndexBuild handoff: %w", err)
		}
		return finalizeOwnedJob(tx, jobID, workerID, claimToken)
	})
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

// FinalizeLegacyCodeIndexSuccessWithRetrievalHandoff atomically persists a
// legacy analysis result, creates or reuses its RetrievalBuild and Job, and
// completes the claimed BUILD_CODE_INDEX Job.
func (s *GormStore) FinalizeLegacyCodeIndexSuccessWithRetrievalHandoff(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, res *model.AnalysisResult) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		owned, err := requireOwnedPipelineJobTx(tx, jobID, workerID, claimToken, jobs.JobTypeBuildCodeIndex, strconv.FormatInt(buildID, 10))
		if err != nil {
			return err
		}
		var build model.CodeIndexBuild
		if err := tx.Where("id = ? AND (analysis_revision_id = '' OR analysis_revision_id IS NULL) AND status = ?", buildID, model.BuildStatusBuilding).First(&build).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("legacy code index build %d is not BUILDING", buildID)
			}
			return err
		}
		if err := s.saveAnalysisResultTx(tx, buildID, res); err != nil {
			return err
		}
		if _, _, err := s.getOrCreateRetrievalBuildTx(tx, buildID, model.ProductionRetrievalStrategy, owned.ExecutionGeneration, time.Now().UTC()); err != nil {
			return fmt.Errorf("create legacy RetrievalBuild handoff: %w", err)
		}
		return finalizeOwnedJob(tx, jobID, workerID, claimToken)
	})
}

// FinalizeCodeIndexSuccessWithRevision atomically publishes the code index,
// advances its product revision, and completes the claimed job.
func (s *GormStore) FinalizeCodeIndexSuccessWithRevision(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, revisionID string, res *model.AnalysisResult) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireOwnedJob(tx, jobID, workerID, claimToken); err != nil {
			return err
		}
		var productRevision revision.AnalysisRevision
		if err := tx.Where("id = ? AND status = ? AND code_index_build_id = ?", revisionID, revision.StatusPreparing, buildID).First(&productRevision).Error; err != nil {
			return revision.ErrLineage
		}
		var codeBuild model.CodeIndexBuild
		if err := tx.First(&codeBuild, "id = ?", buildID).Error; err != nil {
			return err
		}
		if codeBuild.AnalysisRevisionID != revisionID || codeBuild.SnapshotID != productRevision.SnapshotID || codeBuild.Status != model.BuildStatusBuilding {
			return revision.ErrLineage
		}
		if err := s.saveAnalysisResultTx(tx, buildID, res); err != nil {
			return err
		}
		now := time.Now().UTC()
		retrievalBuildID := productRevision.RetrievalBuildID
		if retrievalBuildID == 0 {
			retrievalBuild := &model.RetrievalBuild{
				AnalysisRevisionID: revisionID,
				CodeIndexBuildID:   buildID,
				Strategy:           model.ProductionRetrievalStrategy,
				RetrievalVersion:   model.CurrentRetrievalVersion,
				TokenizerVersion:   model.CurrentTokenizerVersion,
				ConfigHash:         model.RetrievalConfigHash(model.ProductionRetrievalStrategy),
				Status:             model.BuildStatusCreated,
				CreatedAt:          now,
			}
			if err := tx.Create(retrievalBuild).Error; err != nil {
				return err
			}
			retrievalBuildID = retrievalBuild.ID
		} else {
			var retrievalBuild model.RetrievalBuild
			if err := tx.First(&retrievalBuild, "id = ?", retrievalBuildID).Error; err != nil {
				return err
			}
			if retrievalBuild.CodeIndexBuildID != buildID || (retrievalBuild.AnalysisRevisionID != "" && retrievalBuild.AnalysisRevisionID != revisionID) {
				return revision.ErrLineage
			}
			if retrievalBuild.AnalysisRevisionID == "" {
				if err := tx.Model(&model.RetrievalBuild{}).Where("id = ?", retrievalBuildID).Update("analysis_revision_id", revisionID).Error; err != nil {
					return err
				}
			}
		}

		retrievalJob := &jobs.AnalysisJob{}
		jobLookup := tx.Where("job_type = ? AND resource_id = ?", jobs.JobTypeBuildRetrieval, fmt.Sprintf("%d", retrievalBuildID)).First(retrievalJob)
		if errors.Is(jobLookup.Error, gorm.ErrRecordNotFound) {
			if err := tx.Create(&jobs.AnalysisJob{
				JobType: jobs.JobTypeBuildRetrieval, ResourceID: fmt.Sprintf("%d", retrievalBuildID),
				Status: jobs.StatusPending, ExecutionGeneration: productRevision.ExecutionGeneration,
				MaxAttempts: 3, NextRunAt: now,
			}).Error; err != nil {
				return err
			}
		} else if jobLookup.Error != nil {
			return jobLookup.Error
		}
		result := tx.Model(&revision.AnalysisRevision{}).
			Where("id = ? AND status = ? AND code_index_build_id = ?", revisionID, revision.StatusPreparing, buildID).
			Updates(map[string]interface{}{"stage": revision.StageBuildingSearch, "retrieval_build_id": retrievalBuildID, "version": gorm.Expr("version + 1"), "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return revision.ErrLineage
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

// ListAllSymbols returns every symbol in deterministic insertion order for
// internal consumers that must process a complete CodeIndexBuild.
func (s *GormStore) ListAllSymbols(ctx context.Context, buildID int64) ([]*model.Symbol, error) {
	var symbols []*model.Symbol
	if err := s.db.WithContext(ctx).
		Where("code_index_build_id = ?", buildID).
		Order("id ASC").
		Find(&symbols).Error; err != nil {
		return nil, err
	}
	return symbols, nil
}

func (s *GormStore) GetSymbolByID(ctx context.Context, id int64) (*model.Symbol, error) {
	var symbol model.Symbol
	if err := s.db.WithContext(ctx).First(&symbol, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSymbolNotFound
		}
		return nil, err
	}
	return &symbol, nil
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
	configHash := model.RetrievalConfigHash(strategy)
	var existing model.RetrievalBuild
	err := s.db.WithContext(ctx).Where(
		"code_index_build_id = ? AND strategy = ? AND retrieval_version = ? AND tokenizer_version = ? AND config_hash = ?",
		codeIndexBuildID, strategy, model.CurrentRetrievalVersion, model.CurrentTokenizerVersion, configHash,
	).First(&existing).Error
	if err == nil {
		if err := s.ensureAnalysisJob(ctx, jobs.JobTypeBuildRetrieval, strconv.FormatInt(existing.ID, 10), 1, time.Now().UTC()); err != nil {
			return nil, false, err
		}
		return &existing, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	var build *model.RetrievalBuild
	var created bool
	now := time.Now().UTC()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var txErr error
		build, created, txErr = s.createRetrievalBuildTx(tx, codeIndexBuildID, strategy, 1, now)
		return txErr
	})
	if err == nil {
		return build, created, nil
	}

	var winner model.RetrievalBuild
	lookupErr := s.db.WithContext(ctx).Where(
		"code_index_build_id = ? AND strategy = ? AND retrieval_version = ? AND tokenizer_version = ? AND config_hash = ?",
		codeIndexBuildID, strategy, model.CurrentRetrievalVersion, model.CurrentTokenizerVersion, configHash,
	).First(&winner).Error
	if lookupErr != nil {
		return nil, false, err
	}
	if ensureErr := s.ensureAnalysisJob(ctx, jobs.JobTypeBuildRetrieval, strconv.FormatInt(winner.ID, 10), 1, now); ensureErr != nil {
		return nil, false, ensureErr
	}
	return &winner, false, nil
}

func (s *GormStore) getOrCreateRetrievalBuildTx(tx *gorm.DB, codeIndexBuildID int64, strategy string, generation int, createdAt time.Time) (*model.RetrievalBuild, bool, error) {
	configHash := model.RetrievalConfigHash(strategy)
	var build model.RetrievalBuild
	err := tx.Where(
		"code_index_build_id = ? AND strategy = ? AND retrieval_version = ? AND tokenizer_version = ? AND config_hash = ?",
		codeIndexBuildID, strategy, model.CurrentRetrievalVersion, model.CurrentTokenizerVersion, configHash,
	).First(&build).Error
	if err == nil {
		if err := ensureAnalysisJobTx(tx, jobs.JobTypeBuildRetrieval, strconv.FormatInt(build.ID, 10), generation, createdAt); err != nil {
			return nil, false, err
		}
		return &build, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}
	return s.createRetrievalBuildTx(tx, codeIndexBuildID, strategy, generation, createdAt)
}

func (s *GormStore) createRetrievalBuildTx(tx *gorm.DB, codeIndexBuildID int64, strategy string, generation int, createdAt time.Time) (*model.RetrievalBuild, bool, error) {
	configHash := model.RetrievalConfigHash(strategy)
	var build model.RetrievalBuild
	build = model.RetrievalBuild{
		CodeIndexBuildID: codeIndexBuildID,
		Strategy:         strategy,
		RetrievalVersion: model.CurrentRetrievalVersion,
		TokenizerVersion: model.CurrentTokenizerVersion,
		ConfigHash:       configHash,
		ArtifactPath:     "",
		ArtifactHash:     "",
		Status:           model.BuildStatusCreated,
		CreatedAt:        createdAt,
	}
	if err := tx.Create(&build).Error; err != nil {
		return nil, false, err
	}
	if err := ensureAnalysisJobTx(tx, jobs.JobTypeBuildRetrieval, strconv.FormatInt(build.ID, 10), generation, createdAt); err != nil {
		return nil, false, err
	}
	return &build, true, nil
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

// FinalizeRetrievalSuccessWithRevision atomically publishes the retrieval
// artifact, advances the product revision, and completes the claimed job.
func (s *GormStore) FinalizeRetrievalSuccessWithRevision(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, revisionID string, artifactPath, artifactHash string, docCount int) error {
	if artifactPath == "" || artifactHash == "" {
		return fmt.Errorf("retrieval build %d cannot become READY without an artifact and hash", buildID)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireOwnedJob(tx, jobID, workerID, claimToken); err != nil {
			return err
		}
		var productRevision revision.AnalysisRevision
		if err := tx.Where("id = ? AND status = ? AND retrieval_build_id = ?", revisionID, revision.StatusPreparing, buildID).First(&productRevision).Error; err != nil {
			return revision.ErrLineage
		}
		var retrievalBuild model.RetrievalBuild
		if err := tx.First(&retrievalBuild, "id = ?", buildID).Error; err != nil {
			return err
		}
		if retrievalBuild.AnalysisRevisionID != revisionID || retrievalBuild.CodeIndexBuildID != productRevision.CodeIndexBuildID || retrievalBuild.Status != model.BuildStatusBuilding {
			return revision.ErrLineage
		}
		var codeBuild model.CodeIndexBuild
		if err := tx.First(&codeBuild, "id = ?", productRevision.CodeIndexBuildID).Error; err != nil {
			return err
		}
		if codeBuild.AnalysisRevisionID != revisionID || codeBuild.SnapshotID != productRevision.SnapshotID || codeBuild.Status != model.BuildStatusReady {
			return revision.ErrLineage
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
		revisionResult := tx.Model(&revision.AnalysisRevision{}).
			Where("id = ? AND status = ? AND retrieval_build_id = ?", revisionID, revision.StatusPreparing, buildID).
			Updates(map[string]interface{}{"status": revision.StatusReady, "stage": revision.StageReady, "ready_at": now, "version": gorm.Expr("version + 1"), "updated_at": now})
		if revisionResult.Error != nil {
			return revisionResult.Error
		}
		if revisionResult.RowsAffected != 1 {
			return revision.ErrLineage
		}
		return finalizeOwnedJob(tx, jobID, workerID, claimToken)
	})
}

func requireOwnedJob(tx *gorm.DB, jobID int64, workerID, claimToken string) error {
	var job jobs.AnalysisJob
	if err := tx.Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ? AND execution_started = ? AND cancel_requested = ?", jobID, jobs.StatusRunning, workerID, claimToken, true, false).First(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return jobs.ErrOwnershipLost
		}
		return err
	}
	return nil
}

func requireOwnedPipelineJobTx(tx *gorm.DB, jobID int64, workerID, claimToken string, jobType jobs.JobType, resourceID string) (*jobs.AnalysisJob, error) {
	var job jobs.AnalysisJob
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND job_type = ? AND resource_id = ? AND status = ? AND worker_id = ? AND claim_token = ? AND execution_started = ? AND cancel_requested = ?",
		jobID, jobType, resourceID, jobs.StatusRunning, workerID, claimToken, true, false,
	).First(&job).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, jobs.ErrOwnershipLost
		}
		return nil, err
	}
	return &job, nil
}

func finalizeOwnedJob(tx *gorm.DB, jobID int64, workerID, claimToken string) error {
	now := time.Now().UTC()
	result := tx.Model(&jobs.AnalysisJob{}).Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ? AND execution_started = ? AND cancel_requested = ?", jobID, jobs.StatusRunning, workerID, claimToken, true, false).Updates(map[string]interface{}{
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
