package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/provider"
	"repolens/internal/repo"
	"repolens/internal/snapshot"
	"repolens/internal/trace"
)

func setupDemoHandler(t *testing.T) (*gorm.DB, *gin.Engine, *evidence.GormReportStore, *evidence.GormCitationStore) {
	t.Helper()
	manager, diagnosisStore, db, userID := setupProviderConfigGuard(t)
	storeFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	reportStore := evidence.NewReportStore(db)
	citationStore := evidence.NewCitationStore(db)
	handler := provider.NewHandler(manager, repo.NewStore(db), snapshot.NewStore(db), diagnosisStore, reportStore, citationStore, trace.NewStore(db), storeFS).
		WithDemoDependencies(db, codeintelstore.NewStore(db), filepath.Join(t.TempDir(), "indexes"))
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("user_id", userID) })
	router.POST("/demo/trigger", handler.TriggerDemo)
	return db, router, reportStore, citationStore
}

func TestDemoPublishesValidReportWithCanonicalCitations(t *testing.T) {
	db, router, reports, citations := setupDemoHandler(t)
	response := performProviderRequestTo(router, http.MethodPost, "/demo/trigger", `{}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("trigger demo = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		DiagnosisID string `json:"diagnosis_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.DiagnosisID == "" {
		t.Fatalf("decode demo response: %v: %s", err, response.Body.String())
	}
	report, err := reports.GetByRunID(context.Background(), payload.DiagnosisID)
	if err != nil {
		t.Fatal(err)
	}
	if report.ReportStatus != evidence.ReportValid || report.ConclusionKind != evidence.ConclusionRootCause || report.Summary == "" {
		t.Errorf("demo report is not renderable as a valid root cause: status=%s conclusion=%s summary=%q", report.ReportStatus, report.ConclusionKind, report.Summary)
	}
	if report.FindingCount != 1 || report.SupportedFindingCount != 1 || report.ValidCitationCount != 1 || report.InvalidCitationCount != 0 {
		t.Errorf("demo evidence counts are inconsistent: findings=%d supported=%d valid=%d invalid=%d", report.FindingCount, report.SupportedFindingCount, report.ValidCitationCount, report.InvalidCitationCount)
	}
	storedCitations, err := citations.ListByReportID(context.Background(), report.ID)
	if err != nil || len(storedCitations) != 1 {
		t.Fatalf("demo citations = %v, err=%v", storedCitations, err)
	}
	citation := storedCitations[0]
	if citation.ValidationStatus != evidence.CitationValid || citation.ContentHash == "" || citation.Excerpt == "" {
		t.Errorf("demo citation is not validated source evidence: status=%s error=%q", citation.ValidationStatus, citation.ValidationError)
	}
	var findings []evidence.Finding
	if err := json.Unmarshal([]byte(report.FindingsJSON), &findings); err != nil || len(findings) != 1 || len(findings[0].Citations) != 1 {
		t.Fatalf("decode demo findings: %v: %s", err, report.FindingsJSON)
	}
	if got := findings[0].Citations[0]; got.ValidationStatus != evidence.CitationValid || got.ContentHash != citation.ContentHash || got.Excerpt != citation.Excerpt || got.StartLine != citation.StartLine || got.EndLine != citation.EndLine {
		t.Errorf("displayed citation differs from persisted canonical citation: displayed=%+v stored=%+v", got, citation)
	}
	var run diagnosis.DiagnosisRun
	if err := db.First(&run, "id = ?", payload.DiagnosisID).Error; err != nil {
		t.Fatal(err)
	}
	if run.Status != diagnosis.StatusSucceeded || run.FinalAttemptID != report.AttemptID {
		t.Errorf("demo run/report attempt mismatch: %+v", run)
	}
}

func TestDemoPreservesUnrelatedPipelineJobWithCollidingResourceID(t *testing.T) {
	db, router, _, _ := setupDemoHandler(t)
	ctx := context.Background()
	otherBuild, _, err := codeintelstore.NewStore(db).GetOrCreateBuild(ctx, "other-snapshot", "example.com/other", codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	var before jobs.AnalysisJob
	if err := db.Where("job_type = ? AND resource_id = ?", jobs.JobTypeBuildCodeIndex, otherBuild.ID).First(&before).Error; err != nil {
		t.Fatal(err)
	}
	response := performProviderRequestTo(router, http.MethodPost, "/demo/trigger", `{}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("trigger demo = %d: %s", response.Code, response.Body.String())
	}
	var after jobs.AnalysisJob
	if err := db.First(&after, before.ID).Error; err != nil {
		t.Fatalf("demo removed an unrelated pending CodeIndex job whose ID collides with the demo RetrievalBuild: %v", err)
	}
	if after.Status != before.Status || after.ResourceID != before.ResourceID || after.JobType != before.JobType {
		t.Errorf("demo changed unrelated pipeline job: before=%+v after=%+v", before, after)
	}
}

func TestDemoReusesReadySnapshotWithoutRewritingSource(t *testing.T) {
	db, router, _, _ := setupDemoHandler(t)
	firstResponse := performProviderRequestTo(router, http.MethodPost, "/demo/trigger", `{}`)
	if firstResponse.Code != http.StatusCreated {
		t.Fatalf("first demo = %d: %s", firstResponse.Code, firstResponse.Body.String())
	}
	var before snapshot.RepositorySnapshot
	if err := db.First(&before).Error; err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(before.MaterializedPath, "processor.go")
	beforeFile, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	secondResponse := performProviderRequestTo(router, http.MethodPost, "/demo/trigger", `{}`)
	if secondResponse.Code != http.StatusCreated {
		t.Fatalf("repeat demo = %d: %s", secondResponse.Code, secondResponse.Body.String())
	}
	afterFile, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var after snapshot.RepositorySnapshot
	if err := db.First(&after, "id = ?", before.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeFile, afterFile) || !beforeFile.ModTime().Equal(afterFile.ModTime()) || !before.ReadyAt.Equal(*after.ReadyAt) || before.ContentHash != after.ContentHash {
		t.Errorf("repeat demo rewrote its READY snapshot: before=%+v after=%+v", before, after)
	}
}

func TestDemoPersistenceFailureRollsBackAndCanBeRetried(t *testing.T) {
	db, router, _, _ := setupDemoHandler(t)
	const callback = "test:demo_trace_failure"
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "agent_steps" {
			tx.AddError(errors.New("injected demo trace persistence failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	response := performProviderRequestTo(router, http.MethodPost, "/demo/trigger", `{}`)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("failing demo = %d: %s", response.Code, response.Body.String())
	}
	for _, table := range []string{"diagnosis_runs", "diagnosis_attempts", "reports", "citations", "code_index_builds", "retrieval_builds", "analysis_jobs", "repository_snapshots"} {
		var count int64
		if err := db.Table(table).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("failed demo left %d rows in %s", count, table)
		}
	}
	if err := db.Callback().Create().Remove(callback); err != nil {
		t.Fatal(err)
	}
	response = performProviderRequestTo(router, http.MethodPost, "/demo/trigger", `{}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("retry after rolled-back demo = %d: %s", response.Code, response.Body.String())
	}
}
