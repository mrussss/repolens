package integration_real

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

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

func TestRealMySQL_DemoBuildJobsStayInvisibleUntilCleanup(t *testing.T) {
	db, jobStore, cleanup := setupRealMySQL(t)
	defer cleanup()

	// 暂停在同步分析开始之前，让独立连接尝试 claim。此前 Demo 的 Job
	// 已经提交，这里能直接抢到；现在它们应只存在于初始化事务内部。
	sawBuilding := false
	const callback = "test:claim_during_demo_build"
	if err := db.Callback().Update().After("gorm:update").Register(callback, func(tx *gorm.DB) {
		updates, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok || tx.Statement.Table != "code_index_builds" || updates["status"] != codeintelmodel.BuildStatusBuilding {
			return
		}
		sawBuilding = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		claimed, err := jobStore.ClaimJobs(ctx, "competing-demo-worker", 10, time.Minute)
		if err != nil {
			t.Errorf("competing worker claim: %v", err)
			return
		}
		if len(claimed) != 0 {
			t.Errorf("worker claimed %d Demo jobs during synchronous initialization", len(claimed))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Update().Remove(callback) })

	storeFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	reports := evidence.NewReportStore(db)
	citations := evidence.NewCitationStore(db)
	handler := provider.NewHandler(
		provider.NewManager(filepath.Join(t.TempDir(), "provider.json"), "", "", "", "fake"),
		repo.NewStore(db), snapshot.NewStore(db), diagnosis.NewStore(db), reports, citations, trace.NewStore(db), storeFS,
	).WithDemoDependencies(db, codeintelstore.NewStore(db), filepath.Join(t.TempDir(), "indexes"))
	router := gin.New()
	router.POST("/demo/trigger", handler.TriggerDemo)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/demo/trigger", bytes.NewBufferString(`{}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("demo = %d: %s", response.Code, response.Body.String())
	}
	if !sawBuilding {
		t.Fatal("did not exercise the CodeIndex BUILDING boundary")
	}
	var payload struct {
		DiagnosisID string `json:"diagnosis_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	report, err := reports.GetByRunID(context.Background(), payload.DiagnosisID)
	if err != nil {
		t.Fatal(err)
	}
	if report.ReportStatus != evidence.ReportValid || report.ValidCitationCount != 1 || report.CitationCoverage == nil || *report.CitationCoverage != 1 {
		t.Errorf("MySQL Demo report/evidence contract is invalid: %+v", report)
	}
	var remaining int64
	if err := db.Model(&jobs.AnalysisJob{}).Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("Demo leaked %d ordinary build jobs after commit", remaining)
	}
}
