package diagnosis_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/analysispipeline"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/mysql"
	"repolens/internal/repo"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

func newDiagnosisTestService(db *gorm.DB, store diagnosis.Store, repositories repo.Store, snapshots snapshot.Store) *diagnosis.Service {
	return diagnosis.NewService(diagnosis.ServiceDependencies{
		Store: store, RepoStore: repositories,
		Lineage: analysispipeline.NewResolver(revision.NewStore(db), snapshots, codeintelstore.NewStore(db)),
	})
}

func seedDiagnosisLegacyBuilds(t *testing.T, db *gorm.DB, snapshotID string, codeBuildID, retrievalBuildID int64) {
	t.Helper()
	buildContext := codeintelmodel.DefaultBuildContext()
	build := &codeintelmodel.CodeIndexBuild{
		ID: codeBuildID, SnapshotID: snapshotID, ParserVersion: codeintelmodel.CurrentParserVersion,
		AnalyzerVersion: codeintelmodel.CurrentAnalyzerVersion, SymbolSchemaVersion: codeintelmodel.CurrentSymbolSchemaVersion,
		BuildContextHash: buildContext.BuildContextHash(), ModulePath: "test", GOOS: buildContext.GOOS,
		GOARCH: buildContext.GOARCH, BuildTagsHash: buildContext.BuildTagsHash(), Status: codeintelmodel.BuildStatusReady,
	}
	if err := db.Create(build).Error; err != nil {
		t.Fatal(err)
	}
	retrievalBuild := &codeintelmodel.RetrievalBuild{
		ID: retrievalBuildID, CodeIndexBuildID: codeBuildID, Strategy: "BM25",
		RetrievalVersion: codeintelmodel.CurrentRetrievalVersion, TokenizerVersion: codeintelmodel.CurrentTokenizerVersion,
		ConfigHash: "test", ArtifactPath: "test-index", ArtifactHash: "test-hash", Status: codeintelmodel.BuildStatusReady,
	}
	if err := db.Create(retrievalBuild).Error; err != nil {
		t.Fatal(err)
	}
}

func newDiagnosisTestServiceWithCodeIntel(db *gorm.DB, store diagnosis.Store, repositories repo.Store, snapshots snapshot.Store) *diagnosis.Service {
	return diagnosis.NewService(diagnosis.ServiceDependencies{
		Store: store, RepoStore: repositories,
		Lineage: analysispipeline.NewResolver(revision.NewStore(db), snapshots, codeintelstore.NewStore(db)),
	})
}

func TestDiagnosisRequestUsesSharedV22Fixture(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to locate test file")
	}
	fixturePath := filepath.Join(filepath.Dir(currentFile), "..", "..", "contracts", "v2.2", "diagnosis-create.request.json")
	body, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		AnalysisRevisionID string `json:"analysis_revision_id"`
		IssueTitle         string `json:"issue_title"`
		CodeIndexBuildID   int64  `json:"code_index_build_id"`
		RetrievalBuildID   int64  `json:"retrieval_build_id"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if request.AnalysisRevisionID == "" || request.IssueTitle == "" || request.CodeIndexBuildID != 0 || request.RetrievalBuildID != 0 {
		t.Fatalf("shared fixture does not describe the v2.2 revision request: %+v", request)
	}
}

func seedReadyDiagnosisLineage(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Create(&revision.AnalysisRevision{ID: "private-revision", RepositoryID: "private-repo", CommitSHA: "0123456789012345678901234567890123456789", PipelineVersion: "v2.2", PipelineFingerprint: "fingerprint", SnapshotID: "private-snapshot", CodeIndexBuildID: 101, RetrievalBuildID: 202, Status: revision.StatusReady, Stage: revision.StageReady}).Error; err != nil {
		t.Fatal(err)
	}
	for _, value := range []interface{}{
		&snapshot.RepositorySnapshot{ID: "private-snapshot", RepositoryID: "private-repo", AnalysisRevisionID: "private-revision", CommitSHA: "0123456789012345678901234567890123456789", Status: snapshot.StatusReady},
		&codeintelmodel.CodeIndexBuild{ID: 101, SnapshotID: "private-snapshot", AnalysisRevisionID: "private-revision", Status: codeintelmodel.BuildStatusReady},
		&codeintelmodel.RetrievalBuild{ID: 202, CodeIndexBuildID: 101, AnalysisRevisionID: "private-revision", Status: codeintelmodel.BuildStatusReady},
	} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiagnosisRevisionSubmissionRequiresRepositoryOwnership(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	ctx := context.Background()
	repoStore := repo.NewStore(db)
	if err := repoStore.Create(ctx, &repo.Repository{ID: "private-repo", UserID: "owner", Name: "private", GitURL: "https://github.com/example/private"}); err != nil {
		t.Fatal(err)
	}
	seedReadyDiagnosisLineage(t, db)
	svc := newDiagnosisTestServiceWithCodeIntel(db, diagnosis.NewStore(db), repoStore, snapshot.NewStore(db))
	_, _, err := svc.Create(ctx, diagnosis.CreateDiagnosisInput{
		UserID: "attacker", AnalysisRevisionID: "private-revision", IssueTitle: "should not access", IdempotencyKey: "ownership-key",
	})
	if !errors.Is(err, revision.ErrNotFound) {
		t.Fatalf("cross-user revision submission error = %v, want revision not found", err)
	}
}

func TestDiagnosisRevisionSubmissionUsesResolvedLineage(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	ctx := context.Background()
	repoStore := repo.NewStore(db)
	if err := repoStore.Create(ctx, &repo.Repository{ID: "private-repo", UserID: "owner", Name: "private", GitURL: "https://github.com/example/private"}); err != nil {
		t.Fatal(err)
	}
	seedReadyDiagnosisLineage(t, db)
	svc := newDiagnosisTestServiceWithCodeIntel(db, diagnosis.NewStore(db), repoStore, snapshot.NewStore(db))
	input := diagnosis.CreateDiagnosisInput{
		UserID: "owner", AnalysisRevisionID: "private-revision", IssueTitle: "issue", IdempotencyKey: "resolved-lineage-key",
	}
	run, created, err := svc.Create(ctx, input)
	if err != nil || !created {
		t.Fatalf("Create = created=%v err=%v", created, err)
	}
	if run.RepositoryID != "private-repo" || run.AnalysisRevisionID != "private-revision" || run.SnapshotID != "private-snapshot" || run.CodeIndexBuildID != 101 || run.RetrievalBuildID != 202 || run.PipelineFingerprint != "fingerprint" {
		t.Fatalf("DiagnosisRun did not persist resolved lineage: %+v", run)
	}
	duplicate, created, err := svc.Create(ctx, input)
	if err != nil || created || duplicate.ID != run.ID {
		t.Fatalf("idempotent Create = run=%+v created=%v err=%v", duplicate, created, err)
	}
	input.IdempotencyKey = "mismatched-build-key"
	input.CodeIndexBuildID = 999
	if _, _, err := svc.Create(ctx, input); !errors.Is(err, codeintelstore.ErrBuildLineageMismatch) {
		t.Fatalf("mismatched build error = %v, want lineage mismatch", err)
	}
}

func TestDiagnosisCreateRejectsUnconfiguredProviderWithoutCreatingJob(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	diagStore := diagnosis.NewStore(db)
	svc := diagnosis.NewService(diagnosis.ServiceDependencies{Store: diagStore, RepoStore: repo.NewStore(db), Lineage: analysispipeline.NewResolver(revision.NewStore(db), snapshot.NewStore(db), nil), ProviderSource: func() diagnosis.ProviderMetadata {
		return diagnosis.ProviderMetadata{IsConfigured: false}
	}})

	router := diagnosisHandlerRouter(svc)
	body := []byte(`{"repository_id":"repo","snapshot_id":"snapshot","issue_title":"issue","code_index_build_id":1,"retrieval_build_id":2}`)
	req := httptest.NewRequest(http.MethodPost, "/diagnoses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "new-key")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)

	if response.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want 424: %s", response.Code, response.Body.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["code"] != "PROVIDER_NOT_CONFIGURED" {
		t.Fatalf("error code = %v", payload["code"])
	}
	var runCount, jobCount int64
	if err := db.Model(&diagnosis.DiagnosisRun{}).Count(&runCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Count(&jobCount).Error; err != nil {
		t.Fatal(err)
	}
	if runCount != 0 || jobCount != 0 {
		t.Fatalf("unconfigured request created run/job: %d/%d", runCount, jobCount)
	}
}

func TestDiagnosisCreateRejectsIncompleteBuildSelection(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	svc := newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
	router := diagnosisHandlerRouter(svc)
	for _, body := range []string{
		`{"issue_title":"issue"}`,
		`{"issue_title":"issue","code_index_build_id":1}`,
		`{"issue_title":"issue","retrieval_build_id":2}`,
	} {
		response := performDiagnosisCreate(router, body)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_BUILD_SELECTION") {
			t.Fatalf("body %s -> %d %s, want INVALID_BUILD_SELECTION", body, response.Code, response.Body.String())
		}
	}
}

func TestDiagnosisCreateAcceptsRevisionSelectionAtHandlerBoundary(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	svc := newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
	router := diagnosisHandlerRouter(svc)
	response := performDiagnosisCreate(router, `{"issue_title":"issue","analysis_revision_id":"rev"}`)
	if response.Code == http.StatusBadRequest && strings.Contains(response.Body.String(), "INVALID_BUILD_SELECTION") {
		t.Fatalf("revision selection was rejected by handler: %s", response.Body.String())
	}
}

func TestDiagnosisCreateRejectsMalformedTrailingAndUnknownJSON(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	svc := newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
	router := diagnosisHandlerRouter(svc)
	for _, body := range []string{
		`{"issue_title":"issue","code_index_build_id":1,"retrieval_build_id":2`,
		`{"issue_title":"issue","code_index_build_id":1,"retrieval_build_id":2}{}`,
		`{"issue_title":"issue","code_index_build_id":1,"retrieval_build_id":2,"unknown":true}`,
	} {
		response := performDiagnosisCreate(router, body)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INPUT_INVALID") {
			t.Fatalf("body %s -> %d %s, want INPUT_INVALID", body, response.Code, response.Body.String())
		}
	}
}

func TestDiagnosisCreateRejectsExplicitZeroOrNegativeBuildIDs(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	svc := newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
	router := diagnosisHandlerRouter(svc)
	for _, buildID := range []string{"0", "-1"} {
		body := `{"analysis_revision_id":"rev","issue_title":"issue","code_index_build_id":` + buildID + `}`
		response := performDiagnosisCreate(router, body)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INPUT_INVALID") {
			t.Fatalf("build id %s -> %d %s, want INPUT_INVALID", buildID, response.Code, response.Body.String())
		}
	}
}

func TestGetReportStillReturnsCurrentInvalidFinalReport(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	ctx := context.Background()
	run := &diagnosis.DiagnosisRun{ID: "run-api-invalid-report", UserID: "user", RepositoryID: "repo", SnapshotID: "snapshot", IssueTitle: "issue", Status: diagnosis.StatusFailed, FinalAttemptID: "attempt-api-invalid", IdempotencyKey: "api-invalid-report", IdempotencyRequestHash: "api-invalid-report"}
	if err := db.Create(run).Error; err != nil {
		t.Fatal(err)
	}
	if err := evidence.NewReportStore(db).Create(ctx, &evidence.Report{
		ID: "report-api-invalid", DiagnosisRunID: run.ID, AttemptID: "attempt-api-invalid", ReportStatus: evidence.ReportInvalid,
		FindingsJSON: "[]", RecommendedChecksJSON: "[]", StructuredPayloadJSON: "{}", RawOutput: `{"secret-test-marker-XYZ":"untrusted raw output"}`,
		ParseError: "INVALID_STRUCTURED_REPORT: UNKNOWN_FIELD",
	}); err != nil {
		t.Fatal(err)
	}
	svc := newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
	handler := diagnosis.NewHandler(svc, evidence.NewReportStore(db), evidence.NewCitationStore(db), nil)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(string(logger.UserIDKey), "user"); c.Next() })
	router.GET("/diagnoses/:id/report", handler.GetReport)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/diagnoses/"+run.ID+"/report", nil))
	var payload struct {
		Report evidence.Report `json:"report"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || payload.Report.ReportStatus != evidence.ReportInvalid || !strings.Contains(payload.Report.ParseError, "INVALID_STRUCTURED_REPORT") || strings.Contains(payload.Report.ParseError, "secret-test-marker-XYZ") {
		t.Fatalf("report response = %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(payload.Report.RawOutput, "secret-test-marker-XYZ") {
		t.Fatal("separately stored raw output was not preserved for authorized diagnostics")
	}
}

func TestGetReportDoesNotExposePreviousGenerationAfterManualRetry(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	ctx := context.Background()
	run := &diagnosis.DiagnosisRun{
		ID: "run-report-manual-retry", UserID: "user", RepositoryID: "repo", SnapshotID: "snapshot",
		IssueTitle: "issue", Status: diagnosis.StatusRunning, FinalAttemptID: "",
		IdempotencyKey: "report-manual-retry", IdempotencyRequestHash: "report-manual-retry",
	}
	if err := db.Create(run).Error; err != nil {
		t.Fatal(err)
	}
	if err := evidence.NewReportStore(db).Create(ctx, &evidence.Report{
		ID: "report-prior-generation", DiagnosisRunID: run.ID, AttemptID: "attempt-prior-generation",
		RootCause: "prior generation", FindingsJSON: "[]", RecommendedChecksJSON: "[]",
	}); err != nil {
		t.Fatal(err)
	}
	handler := diagnosis.NewHandler(
		newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db)),
		evidence.NewReportStore(db), evidence.NewCitationStore(db), nil,
	)
	response := httptest.NewRecorder()
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(string(logger.UserIDKey), "user"); c.Next() })
	router.GET("/diagnoses/:id/report", handler.GetReport)
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/diagnoses/"+run.ID+"/report", nil))
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"REPORT_NOT_FOUND"`) || strings.Contains(response.Body.String(), "prior generation") {
		t.Fatalf("report after manual retry = %d %s; want no previous-generation report", response.Code, response.Body.String())
	}
}

func TestGetReportReturnsCurrentFinalAttemptReport(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	ctx := context.Background()
	run := &diagnosis.DiagnosisRun{
		ID: "run-report-current-final", UserID: "user", RepositoryID: "repo", SnapshotID: "snapshot",
		IssueTitle: "issue", Status: diagnosis.StatusSucceeded, FinalAttemptID: "attempt-current-final",
		IdempotencyKey: "report-current-final", IdempotencyRequestHash: "report-current-final",
	}
	if err := db.Create(run).Error; err != nil {
		t.Fatal(err)
	}
	reports := evidence.NewReportStore(db)
	for _, report := range []*evidence.Report{
		{ID: "report-old-final", DiagnosisRunID: run.ID, AttemptID: "attempt-old-final", RootCause: "old report", FindingsJSON: "[]", RecommendedChecksJSON: "[]"},
		{ID: "report-current-final", DiagnosisRunID: run.ID, AttemptID: run.FinalAttemptID, RootCause: "current report", FindingsJSON: "[]", RecommendedChecksJSON: "[]"},
	} {
		if err := reports.Create(ctx, report); err != nil {
			t.Fatal(err)
		}
	}
	handler := diagnosis.NewHandler(
		newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db)),
		reports, evidence.NewCitationStore(db), nil,
	)
	response := httptest.NewRecorder()
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(string(logger.UserIDKey), "user"); c.Next() })
	router.GET("/diagnoses/:id/report", handler.GetReport)
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/diagnoses/"+run.ID+"/report", nil))
	var payload struct {
		Report evidence.Report `json:"report"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || payload.Report.ID != "report-current-final" || payload.Report.AttemptID != run.FinalAttemptID || payload.Report.RootCause != "current report" {
		t.Fatalf("current final report = %d %+v %s", response.Code, payload.Report, response.Body.String())
	}
}

func TestGetReportRejectsMismatchedRunLineage(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	run := &diagnosis.DiagnosisRun{
		ID: "run-report-lineage-owner", UserID: "user", RepositoryID: "repo", SnapshotID: "snapshot",
		IssueTitle: "issue", Status: diagnosis.StatusSucceeded, FinalAttemptID: "attempt-mismatched-lineage",
		IdempotencyKey: "report-lineage-owner", IdempotencyRequestHash: "report-lineage-owner",
	}
	if err := db.Create(run).Error; err != nil {
		t.Fatal(err)
	}
	store := reportStoreStub{report: &evidence.Report{ID: "mismatched-report", DiagnosisRunID: "different-run", AttemptID: run.FinalAttemptID}}
	handler := diagnosis.NewHandler(
		newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db)),
		store, citationStoreStub{}, nil,
	)
	response := httptest.NewRecorder()
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(string(logger.UserIDKey), "user"); c.Next() })
	router.GET("/diagnoses/:id/report", handler.GetReport)
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/diagnoses/"+run.ID+"/report", nil))
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"INTERNAL_ERROR"`) {
		t.Fatalf("mismatched report lineage = %d %s, want internal error", response.Code, response.Body.String())
	}
}

func TestDiagnosisReportDistinguishesMissingRowsFromStoreFailures(t *testing.T) {
	for _, tt := range []struct {
		name        string
		reportErr   error
		citationErr error
		wantStatus  int
	}{
		{name: "missing report", reportErr: gorm.ErrRecordNotFound, wantStatus: http.StatusNotFound},
		{name: "report store failure", reportErr: errors.New("database unavailable"), wantStatus: http.StatusInternalServerError},
		{name: "citation store failure", citationErr: errors.New("database unavailable"), wantStatus: http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := newDiagnosisHandlerTestDB(t)
			run := &diagnosis.DiagnosisRun{ID: "run-report-store-errors", UserID: "user", RepositoryID: "repo", SnapshotID: "snapshot", IssueTitle: "issue", Status: diagnosis.StatusSucceeded, FinalAttemptID: "attempt-report-store-errors", IdempotencyKey: "report-store-errors", IdempotencyRequestHash: "report-store-errors"}
			if err := db.Create(run).Error; err != nil {
				t.Fatal(err)
			}
			svc := newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
			reportStore := reportStoreStub{report: &evidence.Report{ID: "report-for-errors", DiagnosisRunID: run.ID}, err: tt.reportErr}
			citationStore := citationStoreStub{err: tt.citationErr}
			handler := diagnosis.NewHandler(svc, reportStore, citationStore, nil)
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set(string(logger.UserIDKey), "user"); c.Next() })
			router.GET("/diagnoses/:id/report", handler.GetReport)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/diagnoses/"+run.ID+"/report", nil))
			if response.Code != tt.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), tt.wantStatus)
			}
		})
	}
}

type reportStoreStub struct {
	report *evidence.Report
	err    error
}

func (s reportStoreStub) Create(context.Context, *evidence.Report) error { return nil }
func (s reportStoreStub) GetByRunID(context.Context, string) (*evidence.Report, error) {
	return s.report, s.err
}
func (s reportStoreStub) GetByAttemptID(context.Context, string) (*evidence.Report, error) {
	return s.report, s.err
}

type citationStoreStub struct{ err error }

func (s citationStoreStub) CreateBatch(context.Context, []evidence.Citation) error { return nil }
func (s citationStoreStub) ListByReportID(context.Context, string) ([]evidence.Citation, error) {
	return nil, s.err
}

func TestDiagnosisStatusExposesExplicitProviderRetryPolicy(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	ctx := context.Background()
	store := diagnosis.NewStore(db)
	jobsStore := jobs.NewStoreWithDriver(mustSQLDB(t, db), "sqlite3")
	run := &diagnosis.DiagnosisRun{ID: "retry-policy-api", UserID: "user", RepositoryID: "repo", SnapshotID: "snap", IssueTitle: "retry", IdempotencyKey: "retry-policy-key", IdempotencyRequestHash: "retry-policy-hash"}
	if err := store.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&diagnosis.DiagnosisRun{}).Where("id = ?", run.ID).Update("status", diagnosis.StatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, run.ID).Updates(map[string]interface{}{
		"status": jobs.StatusFailed, "last_error_class": jobs.ErrorClassPermanent, "last_error_code": "CHECKPOINT_SAVE_FAILED",
	}).Error; err != nil {
		t.Fatal(err)
	}
	svc := diagnosis.NewService(diagnosis.ServiceDependencies{Store: store, RepoStore: repo.NewStore(db), Lineage: analysispipeline.NewResolver(revision.NewStore(db), snapshot.NewStore(db), nil), JobStore: jobsStore})
	response := httptest.NewRecorder()
	diagnosisHandlerRouter(svc).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/diagnoses/"+run.ID, nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"retry_allowed":true`) || !strings.Contains(response.Body.String(), `"retry_error_code":"CHECKPOINT_SAVE_FAILED"`) {
		t.Fatalf("retry policy API response = %d %s", response.Code, response.Body.String())
	}

	retryResponse := httptest.NewRecorder()
	router := diagnosisHandlerRouter(svc)
	router.ServeHTTP(retryResponse, httptest.NewRequest(http.MethodPost, "/diagnoses/"+run.ID+"/retry", nil))
	if retryResponse.Code != http.StatusAccepted {
		t.Fatalf("explicit retry response = %d %s, want 202", retryResponse.Code, retryResponse.Body.String())
	}
	var retriedJob jobs.AnalysisJob
	if err := db.Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, run.ID).First(&retriedJob).Error; err != nil {
		t.Fatal(err)
	}
	if retriedJob.Status != jobs.StatusPending || retriedJob.ExecutionGeneration != 2 {
		t.Fatalf("explicit retry job = status %s generation %d, want PENDING generation 2", retriedJob.Status, retriedJob.ExecutionGeneration)
	}
}

func TestDiagnosisRetryIsFencedByPinnedProviderIdentity(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	ctx := context.Background()
	store := diagnosis.NewStore(db)
	jobsStore := jobs.NewStoreWithDriver(mustSQLDB(t, db), "sqlite3")
	run := &diagnosis.DiagnosisRun{
		ID: "retry-provider-identity", UserID: "user", RepositoryID: "repo", SnapshotID: "snap",
		IssueTitle: "retry", IdempotencyKey: "retry-provider-identity-key", IdempotencyRequestHash: "retry-provider-identity-hash",
		Status: diagnosis.StatusFailed, ProviderConfigFingerprint: "pinned-endpoint-model-auth",
	}
	if err := store.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&diagnosis.DiagnosisRun{}).Where("id = ?", run.ID).Update("status", diagnosis.StatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, run.ID).Updates(map[string]interface{}{
		"status": jobs.StatusFailed, "last_error_class": jobs.ErrorClassRetryable, "last_error_code": "PROVIDER_TIMEOUT",
	}).Error; err != nil {
		t.Fatal(err)
	}

	metadata := diagnosis.ProviderMetadata{IsConfigured: true, ConfigFingerprint: "changed-endpoint-model-auth"}
	svc := diagnosis.NewService(diagnosis.ServiceDependencies{Store: store, RepoStore: repo.NewStore(db), Lineage: analysispipeline.NewResolver(revision.NewStore(db), snapshot.NewStore(db), nil), JobStore: jobsStore, ProviderSource: func() diagnosis.ProviderMetadata {
		return metadata
	}})
	if err := svc.Retry(ctx, run.ID, "user"); !errors.Is(err, diagnosis.ErrProviderIdentityChanged) {
		t.Fatalf("retry with changed provider identity error = %v, want ErrProviderIdentityChanged", err)
	}
	var job jobs.AnalysisJob
	if err := db.Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, run.ID).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusFailed {
		t.Fatalf("mismatched-identity retry changed job status to %s", job.Status)
	}

	// The config fingerprint intentionally excludes the API key, so rotating
	// only that secret leaves the run retryable under the same provider identity.
	metadata.ConfigFingerprint = "pinned-endpoint-model-auth"
	if err := svc.Retry(ctx, run.ID, "user"); err != nil {
		t.Fatalf("retry after key-only rotation (same identity fingerprint): %v", err)
	}
	if err := db.Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, run.ID).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusPending {
		t.Fatalf("same-identity retry job status = %s, want PENDING", job.Status)
	}
}

func mustSQLDB(t *testing.T, db *gorm.DB) *sql.DB {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	return sqlDB
}

func TestDiagnosisReportAPIExposesDegradedCitationReport(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	ctx := context.Background()
	run := &diagnosis.DiagnosisRun{ID: "run-api-degraded-report", UserID: "user", RepositoryID: "repo", SnapshotID: "snapshot", IssueTitle: "issue", Status: diagnosis.StatusSucceeded, FinalAttemptID: "attempt-api-degraded", IdempotencyKey: "api-degraded-report", IdempotencyRequestHash: "api-degraded-report"}
	if err := db.Create(run).Error; err != nil {
		t.Fatal(err)
	}
	if err := evidence.NewReportStore(db).Create(ctx, &evidence.Report{
		ID: "report-api-degraded", DiagnosisRunID: run.ID, AttemptID: "attempt-api-degraded", ReportStatus: evidence.ReportDegraded,
		FindingsJSON:          `[{"title":"finding","reasoning":"reasoning","citations":[{"evidence_id":"ev-missing","validation_status":"INVALID","validation_error":"EVIDENCE_NOT_FOUND"}]}]`,
		RecommendedChecksJSON: "[]", StructuredPayloadJSON: "{}", InvalidCitationCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := evidence.NewCitationStore(db).CreateBatch(ctx, []evidence.Citation{{
		ID: "citation-api-degraded", ReportID: "report-api-degraded", EvidenceID: "ev-missing",
		SnapshotID: "snapshot", ValidationStatus: evidence.CitationInvalid, ValidationError: "EVIDENCE_NOT_FOUND",
	}}); err != nil {
		t.Fatal(err)
	}
	svc := newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
	handler := diagnosis.NewHandler(svc, evidence.NewReportStore(db), evidence.NewCitationStore(db), nil)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(string(logger.UserIDKey), "user"); c.Next() })
	router.GET("/diagnoses/:id/report", handler.GetReport)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/diagnoses/"+run.ID+"/report", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"report_status":"DEGRADED"`) || !strings.Contains(response.Body.String(), `"validation_status":"INVALID"`) {
		t.Fatalf("report response = %d %s", response.Code, response.Body.String())
	}
}

func TestDiagnosisHandlerGetDistinguishesNotFoundFromStoreFailure(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	svc := newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
	router := diagnosisHandlerRouter(svc)

	response := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/diagnoses/missing", nil)
	router.ServeHTTP(response, req)
	if response.Code != http.StatusNotFound || !bytes.Contains(response.Body.Bytes(), []byte(`"DIAGNOSIS_NOT_FOUND"`)) {
		t.Fatalf("not found response = %d %s", response.Code, response.Body.String())
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/diagnoses/any", nil)
	router.ServeHTTP(response, req)
	if response.Code != http.StatusInternalServerError || !bytes.Contains(response.Body.Bytes(), []byte(`"INTERNAL_ERROR"`)) || bytes.Contains(response.Body.Bytes(), []byte("database")) {
		t.Fatalf("store failure response = %d %s", response.Code, response.Body.String())
	}
}

func TestDiagnosisHandlerPaginationValidation(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	svc := newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
	router := diagnosisHandlerRouter(svc)

	tests := []struct {
		query string
		code  int
	}{
		{query: "", code: http.StatusOK},
		{query: "?page=2&page_size=50", code: http.StatusOK},
		{query: "?page=abc", code: http.StatusBadRequest},
		{query: "?page=0", code: http.StatusBadRequest},
		{query: "?page=-1", code: http.StatusBadRequest},
		{query: "?page_size=abc", code: http.StatusBadRequest},
		{query: "?page_size=0", code: http.StatusBadRequest},
		{query: "?page_size=101", code: http.StatusBadRequest},
	}
	for _, tt := range tests {
		response := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/diagnoses"+tt.query, nil)
		router.ServeHTTP(response, req)
		if response.Code != tt.code {
			t.Errorf("query %q status = %d, want %d: %s", tt.query, response.Code, tt.code, response.Body.String())
		}
		if tt.code == http.StatusBadRequest && !bytes.Contains(response.Body.Bytes(), []byte(`"INVALID_PAGINATION"`)) {
			t.Errorf("query %q missing INVALID_PAGINATION: %s", tt.query, response.Body.String())
		}
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/diagnoses?page=1", nil)
	router.ServeHTTP(response, req)
	if response.Code != http.StatusInternalServerError || !bytes.Contains(response.Body.Bytes(), []byte(`"INTERNAL_ERROR"`)) {
		t.Fatalf("list store failure response = %d %s", response.Code, response.Body.String())
	}
}

func TestDiagnosisAttemptEndpointsEnforceRunAndAttemptOwnership(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	if err := db.Create(&diagnosis.DiagnosisRun{
		ID: "run-owner", UserID: "user", RepositoryID: "repo", SnapshotID: "snapshot",
		IssueTitle: "owner run", IdempotencyKey: "owner-key", IdempotencyRequestHash: "owner-hash",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&diagnosis.DiagnosisRun{
		ID: "run-other", UserID: "user", RepositoryID: "repo", SnapshotID: "snapshot",
		IssueTitle: "other run", IdempotencyKey: "other-key", IdempotencyRequestHash: "other-hash",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&diagnosis.DiagnosisRun{
		ID: "run-private", UserID: "another-user", RepositoryID: "repo", SnapshotID: "snapshot",
		IssueTitle: "private run", IdempotencyKey: "private-key", IdempotencyRequestHash: "private-hash",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&diagnosis.DiagnosisAttempt{
		ID: "attempt-other", DiagnosisRunID: "run-other", AttemptNo: 1,
		Status: diagnosis.AttemptStatusRunning, StartedAt: time.Now().UTC(),
		HeartbeatAt: time.Now().UTC(), DeadlineAt: time.Now().UTC().Add(time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}

	svc := newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
	router := diagnosisHandlerRouter(svc)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/diagnoses/run-private/attempts", nil)
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !bytes.Contains(response.Body.Bytes(), []byte(`"DIAGNOSIS_NOT_FOUND"`)) {
		t.Fatalf("cross-user/missing attempt list response = %d %s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/diagnoses/run-owner/steps?attempt_id=attempt-other", nil)
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !bytes.Contains(response.Body.Bytes(), []byte(`"ATTEMPT_NOT_FOUND"`)) {
		t.Fatalf("cross-run trace response = %d %s", response.Code, response.Body.String())
	}
}

func TestDiagnosisCreateIdempotencyReplayPrecedesProviderCheck(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	ctx := context.Background()
	repoStore := repo.NewStore(db)
	snapshotStore := snapshot.NewStore(db)
	if err := repoStore.Create(ctx, &repo.Repository{ID: "repo-replay", UserID: "user", Name: "replay", GitURL: "https://github.com/example/replay"}); err != nil {
		t.Fatal(err)
	}
	if err := snapshotStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID: "snapshot-replay", RepositoryID: "repo-replay", CommitSHA: "commit-replay",
		MaterializedPath: t.TempDir(), Status: snapshot.StatusReady,
	}); err != nil {
		t.Fatal(err)
	}
	seedDiagnosisLegacyBuilds(t, db, "snapshot-replay", 11, 12)

	configured := true
	svc := diagnosis.NewService(diagnosis.ServiceDependencies{Store: diagnosis.NewStore(db), RepoStore: repoStore, Lineage: analysispipeline.NewResolver(revision.NewStore(db), snapshotStore, codeintelstore.NewStore(db)), ProviderSource: func() diagnosis.ProviderMetadata {
		return diagnosis.ProviderMetadata{IsConfigured: configured}
	}})
	input := diagnosis.CreateDiagnosisInput{
		UserID: "user", RepositoryID: "repo-replay", SnapshotID: "snapshot-replay",
		IssueTitle: "Authorization: Bearer title-secret", IssueDescription: "Authorization: Basic dXNlcjpwYXNz",
		ErrorLog: "authorization:\tBearer log-secret", IdempotencyKey: "replay-key", CodeIndexBuildID: 11, RetrievalBuildID: 12,
	}
	run, created, err := svc.Create(ctx, input)
	if err != nil || !created {
		t.Fatalf("initial create failed: created=%v err=%v", created, err)
	}
	for field, value := range map[string]string{
		"issue title": run.IssueTitle, "issue description": run.IssueDescription, "error log": run.ErrorLog,
	} {
		if strings.Contains(value, "title-secret") || strings.Contains(value, "dXNlcjpwYXNz") || strings.Contains(value, "log-secret") || !strings.Contains(value, "[REDACTED_SECRET]") {
			t.Errorf("%s was not consistently redacted: %q", field, value)
		}
	}
	configured = false
	replayed, created, err := svc.Create(ctx, input)
	if err != nil || created || replayed.ID != run.ID {
		t.Fatalf("replay was not returned before provider check: created=%v run=%v err=%v", created, replayed, err)
	}
	input.IssueTitle = "different issue"
	if _, _, err := svc.Create(ctx, input); err != diagnosis.ErrIdempotencyConflict {
		t.Fatalf("conflict was not returned before provider check: %v", err)
	}
}

func newDiagnosisHandlerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "handler.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func diagnosisHandlerRouter(svc *diagnosis.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(logger.UserIDKey), "user")
		c.Next()
	})
	handler := diagnosis.NewHandler(svc, nil, nil, nil)
	router.POST("/diagnoses", handler.Create)
	router.GET("/diagnoses", handler.List)
	router.GET("/diagnoses/:id", handler.Get)
	router.GET("/diagnoses/:id/attempts", handler.ListAttempts)
	router.GET("/diagnoses/:id/steps", handler.GetSteps)
	router.POST("/diagnoses/:id/retry", handler.Retry)
	return router
}

func performDiagnosisCreate(router *gin.Engine, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/diagnoses", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
