package diagnosis_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"repolens/internal/diagnosis"
	"repolens/internal/platform/logger"
	"repolens/internal/repo"
	"repolens/internal/snapshot"
	"repolens/internal/trace"
)

func TestAttemptScopedTraceDuringRunningAndFinalStates(t *testing.T) {
	for _, status := range []diagnosis.RunStatus{diagnosis.StatusRunning, diagnosis.StatusSucceeded, diagnosis.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			db := newDiagnosisHandlerTestDB(t)
			run := &diagnosis.DiagnosisRun{ID: "trace-run", UserID: "user", RepositoryID: "repo", SnapshotID: "snapshot", Status: status, IssueTitle: "trace", ExecutionGeneration: 2, IdempotencyKey: "trace-key", IdempotencyRequestHash: "trace-hash"}
			if status != diagnosis.StatusRunning {
				run.FinalAttemptID = "current-attempt"
			}
			if err := db.Create(run).Error; err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"old-attempt", "current-attempt", "foreign-attempt"} {
				runID, generation, attemptNo := run.ID, 2, 1
				if id == "old-attempt" {
					generation = 1
				}
				if id == "foreign-attempt" {
					runID = "foreign-run"
				}
				now := time.Now().UTC()
				attempt := &diagnosis.DiagnosisAttempt{ID: id, DiagnosisRunID: runID, ExecutionGeneration: generation, AttemptNo: attemptNo, Status: diagnosis.AttemptStatusRunning, StartedAt: now, HeartbeatAt: now, DeadlineAt: now.Add(time.Minute)}
				if err := db.Create(attempt).Error; err != nil {
					t.Fatal(err)
				}
				if err := trace.NewStore(db).Create(context.Background(), &trace.AgentStep{ID: "step-" + id, AttemptID: id, Seq: 1, StepType: trace.StepTypeThinking}); err != nil {
					t.Fatal(err)
				}
			}
			svc := newDiagnosisTestService(db, diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set(string(logger.UserIDKey), "user") })
			router.GET("/diagnoses/:id/steps", diagnosis.NewHandler(svc, nil, nil, trace.NewStore(db)).GetSteps)
			url := "/diagnoses/trace-run/steps"
			if status == diagnosis.StatusRunning {
				url += "?attempt_id=current-attempt"
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, url, nil))
			var payload struct {
				Steps []trace.AgentStep `json:"steps"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || len(payload.Steps) != 1 || payload.Steps[0].AttemptID != "current-attempt" {
				t.Fatalf("current trace: %d %s", response.Code, response.Body.String())
			}
			response = httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/diagnoses/trace-run/steps?attempt_id=foreign-attempt", nil))
			if response.Code != http.StatusNotFound {
				t.Fatalf("foreign trace accepted: %d %s", response.Code, response.Body.String())
			}
		})
	}
}
