package analysispipeline_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/analysispipeline"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/platform/mysql"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

const testCommitSHA = "0123456789012345678901234567890123456789"

func readyLineageDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "lineage.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	for _, value := range []interface{}{
		&revision.AnalysisRevision{
			ID: "revision-1", RepositoryID: "repo-1", CommitSHA: testCommitSHA,
			PipelineVersion: "v2.2.0", PipelineFingerprint: "fingerprint-1",
			SnapshotID: "snapshot-1", CodeIndexBuildID: 10, RetrievalBuildID: 20,
			Status: revision.StatusReady, Stage: revision.StageReady,
		},
		&snapshot.RepositorySnapshot{
			ID: "snapshot-1", RepositoryID: "repo-1", AnalysisRevisionID: "revision-1",
			CommitSHA: testCommitSHA, MaterializedPath: t.TempDir(), Status: snapshot.StatusReady,
		},
		&codeintelmodel.CodeIndexBuild{
			ID: 10, SnapshotID: "snapshot-1", AnalysisRevisionID: "revision-1",
			ParserVersion:       codeintelmodel.CurrentParserVersion,
			AnalyzerVersion:     codeintelmodel.CurrentAnalyzerVersion,
			SymbolSchemaVersion: codeintelmodel.CurrentSymbolSchemaVersion,
			BuildContextHash:    "build-context-1", Status: codeintelmodel.BuildStatusReady,
		},
		&codeintelmodel.RetrievalBuild{
			ID: 20, CodeIndexBuildID: 10, AnalysisRevisionID: "revision-1",
			Strategy: "BM25", RetrievalVersion: codeintelmodel.CurrentRetrievalVersion,
			TokenizerVersion: codeintelmodel.CurrentTokenizerVersion, ConfigHash: "config-1",
			Status: codeintelmodel.BuildStatusReady,
		},
	} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func setLineageField(t *testing.T, db *gorm.DB, model interface{}, id interface{}, field string, value interface{}) {
	t.Helper()
	if err := db.Model(model).Where("id = ?", id).Update(field, value).Error; err != nil {
		t.Fatal(err)
	}
}

func TestResolveReadyLineage(t *testing.T) {
	tests := []struct {
		name       string
		change     func(*testing.T, *gorm.DB)
		wantErr    error
		wantString string
	}{
		{name: "ready"},
		{name: "revision not ready", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &revision.AnalysisRevision{}, "revision-1", "status", revision.StatusPreparing)
		}, wantErr: analysispipeline.ErrRevisionNotReady},
		{name: "revision stage not ready", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &revision.AnalysisRevision{}, "revision-1", "stage", revision.StageBuildingSearch)
		}, wantErr: analysispipeline.ErrRevisionNotReady},
		{name: "missing pipeline identity", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &revision.AnalysisRevision{}, "revision-1", "pipeline_fingerprint", "")
		}, wantErr: codeintelstore.ErrBuildLineageMismatch},
		{name: "missing commit identity", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &revision.AnalysisRevision{}, "revision-1", "commit_sha", "")
		}, wantErr: codeintelstore.ErrBuildLineageMismatch},
		{name: "missing build identity", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &revision.AnalysisRevision{}, "revision-1", "code_index_build_id", 0)
		}, wantErr: codeintelstore.ErrBuildLineageMismatch},
		{name: "snapshot not ready", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &snapshot.RepositorySnapshot{}, "snapshot-1", "status", snapshot.StatusMaterializing)
		}, wantString: "not READY"},
		{name: "snapshot repository mismatch", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &snapshot.RepositorySnapshot{}, "snapshot-1", "repository_id", "repo-other")
		}, wantErr: codeintelstore.ErrBuildLineageMismatch},
		{name: "snapshot revision mismatch", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &snapshot.RepositorySnapshot{}, "snapshot-1", "analysis_revision_id", "revision-other")
		}, wantErr: codeintelstore.ErrBuildLineageMismatch},
		{name: "snapshot commit mismatch", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &snapshot.RepositorySnapshot{}, "snapshot-1", "commit_sha", "other")
		}, wantErr: codeintelstore.ErrBuildLineageMismatch},
		{name: "code index not ready", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &codeintelmodel.CodeIndexBuild{}, 10, "status", codeintelmodel.BuildStatusBuilding)
		}, wantErr: analysispipeline.ErrBuildNotReady},
		{name: "code index snapshot mismatch", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &codeintelmodel.CodeIndexBuild{}, 10, "snapshot_id", "snapshot-other")
		}, wantErr: codeintelstore.ErrBuildLineageMismatch},
		{name: "code index revision mismatch", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &codeintelmodel.CodeIndexBuild{}, 10, "analysis_revision_id", "revision-other")
		}, wantErr: codeintelstore.ErrBuildLineageMismatch},
		{name: "retrieval not ready", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &codeintelmodel.RetrievalBuild{}, 20, "status", codeintelmodel.BuildStatusBuilding)
		}, wantErr: analysispipeline.ErrBuildNotReady},
		{name: "retrieval code index mismatch", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &codeintelmodel.RetrievalBuild{}, 20, "code_index_build_id", 99)
		}, wantErr: codeintelstore.ErrBuildLineageMismatch},
		{name: "retrieval revision mismatch", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &codeintelmodel.RetrievalBuild{}, 20, "analysis_revision_id", "revision-other")
		}, wantErr: codeintelstore.ErrBuildLineageMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := readyLineageDB(t)
			if tt.change != nil {
				tt.change(t, db)
			}
			resolver := analysispipeline.NewResolver(revision.NewStore(db), snapshot.NewStore(db), codeintelstore.NewStore(db))
			lineage, err := resolver.ResolveReadyLineage(context.Background(), "revision-1")
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantString != "" && (err == nil || !strings.Contains(err.Error(), tt.wantString)) {
				t.Fatalf("error = %v, want text %q", err, tt.wantString)
			}
			if tt.wantErr != nil || tt.wantString != "" {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if lineage.RepositoryID != "repo-1" || lineage.RevisionID != "revision-1" || lineage.CommitSHA != testCommitSHA || lineage.SnapshotID != "snapshot-1" || lineage.CodeIndexBuildID != 10 || lineage.RetrievalBuildID != 20 || lineage.PipelineVersion != "v2.2.0" || lineage.PipelineFingerprint != "fingerprint-1" {
				t.Fatalf("unexpected resolved lineage: %+v", lineage)
			}
		})
	}
}

func TestResolveReadyLineageMissingRevision(t *testing.T) {
	db := readyLineageDB(t)
	resolver := analysispipeline.NewResolver(revision.NewStore(db), snapshot.NewStore(db), codeintelstore.NewStore(db))
	_, err := resolver.ResolveReadyLineage(context.Background(), "missing")
	if !errors.Is(err, revision.ErrNotFound) {
		t.Fatalf("error = %v, want revision not found", err)
	}
}

func TestResolveLegacyReadyRequiresCompleteReadyLineage(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*testing.T, *gorm.DB)
		wantErr bool
	}{
		{name: "ready"},
		{name: "snapshot not ready", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &snapshot.RepositorySnapshot{}, "snapshot-1", "status", snapshot.StatusMaterializing)
		}, wantErr: true},
		{name: "snapshot repository mismatch", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &snapshot.RepositorySnapshot{}, "snapshot-1", "repository_id", "other-repo")
		}, wantErr: true},
		{name: "code index mismatch", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &codeintelmodel.CodeIndexBuild{}, 10, "snapshot_id", "other-snapshot")
		}, wantErr: true},
		{name: "code index not ready", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &codeintelmodel.CodeIndexBuild{}, 10, "status", codeintelmodel.BuildStatusBuilding)
		}, wantErr: true},
		{name: "retrieval mismatch", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &codeintelmodel.RetrievalBuild{}, 20, "code_index_build_id", 99)
		}, wantErr: true},
		{name: "retrieval not ready", change: func(t *testing.T, db *gorm.DB) {
			setLineageField(t, db, &codeintelmodel.RetrievalBuild{}, 20, "status", codeintelmodel.BuildStatusBuilding)
		}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := readyLineageDB(t)
			if tt.change != nil {
				tt.change(t, db)
			}
			resolver := analysispipeline.NewResolver(revision.NewStore(db), snapshot.NewStore(db), codeintelstore.NewStore(db))
			lineage, err := resolver.ResolveLegacyReady(context.Background(), "repo-1", "snapshot-1", 10, 20)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("invalid legacy lineage was accepted: %+v", lineage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if lineage.RepositoryID != "repo-1" || lineage.RevisionID != "" || lineage.SnapshotID != "snapshot-1" || lineage.CodeIndexBuildID != 10 || lineage.RetrievalBuildID != 20 || lineage.CommitSHA != testCommitSHA {
				t.Fatalf("legacy lineage = %+v", lineage)
			}
		})
	}
}

func TestResolveLegacyReadyRejectsMissingCodeIntelValidator(t *testing.T) {
	db := readyLineageDB(t)
	resolver := analysispipeline.NewResolver(revision.NewStore(db), snapshot.NewStore(db), nil)
	if lineage, err := resolver.ResolveLegacyReady(context.Background(), "repo-1", "snapshot-1", 10, 20); err == nil {
		t.Fatalf("legacy lineage was accepted without CodeIntel validation: %+v", lineage)
	}
}
