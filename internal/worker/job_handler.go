package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
)

// DiagnosisJobHandler implements jobs.Handler for RUN_DIAGNOSIS jobs.
type DiagnosisJobHandler struct {
	diagnosisStore diagnosis.Store
	reportStore    evidence.ReportStore
	citationStore  evidence.CitationStore
	citationVal    *evidence.CitationValidator
	executor       DiagnosisExecutor
}

// NewDiagnosisJobHandler constructs a new DiagnosisJobHandler.
func NewDiagnosisJobHandler(
	diagnosisStore diagnosis.Store,
	reportStore evidence.ReportStore,
	citationStore evidence.CitationStore,
	citationVal *evidence.CitationValidator,
	executor DiagnosisExecutor,
) *DiagnosisJobHandler {
	return &DiagnosisJobHandler{
		diagnosisStore: diagnosisStore,
		reportStore:    reportStore,
		citationStore:  citationStore,
		citationVal:    citationVal,
		executor:       executor,
	}
}

// Execute processes a RUN_DIAGNOSIS job.
func (h *DiagnosisJobHandler) Execute(ctx context.Context, job *jobs.AnalysisJob) error {
	runID := job.ResourceID
	log := logger.L(ctx).With("diagnosis_id", runID, "job_id", job.ID, "attempt", job.AttemptCount)

	run, err := h.diagnosisStore.GetByID(ctx, runID)
	if err != nil {
		return jobs.NewPermanentError("DIAGNOSIS_NOT_FOUND", fmt.Sprintf("diagnosis run %s not found: %v", runID, err), err)
	}

	if run.Status == diagnosis.StatusSucceeded || run.Status == diagnosis.StatusFailed || run.Status == diagnosis.StatusCancelled {
		log.Info("diagnosis run already terminal", "status", run.Status)
		return nil
	}

	workerID := "worker"
	if job.WorkerID != nil {
		workerID = *job.WorkerID
	}
	attempt := &diagnosis.DiagnosisAttempt{
		ID:             fmt.Sprintf("job-%d-%d", job.ID, job.AttemptCount),
		DiagnosisRunID: run.ID,
		AttemptNo:      job.AttemptCount,
		WorkerID:       workerID,
	}

	if run.CancelRequested || job.CancelRequested {
		log.Info("diagnosis cancellation requested")
		_ = h.diagnosisStore.FinishAttemptAndRun(ctx, run.ID, attempt.ID, diagnosis.StatusCancelled, diagnosis.AttemptStatusCancelled, 0, 0, 0, "CANCELLED", "User requested cancellation", false, 0)
		return jobs.NewPermanentError("CANCELLED", "diagnosis was cancelled", context.Canceled)
	}

	// Update status to RUNNING
	_ = h.diagnosisStore.FinishAttemptAndRun(ctx, run.ID, attempt.ID, diagnosis.StatusRunning, diagnosis.AttemptStatusRunning, 0, 0, 0, "", "", false, 0)

	result, execErr := h.executor.Execute(ctx, run, attempt)
	if execErr != nil {
		log.Error("agent execution failed", "error", execErr)
		errClass, errCode := jobs.ClassifyError(execErr)
		isTerminal := (errClass == jobs.ErrorClassPermanent) || (job.AttemptCount >= job.MaxAttempts)

		var newRunStatus diagnosis.RunStatus
		var newAttemptStatus diagnosis.AttemptStatus
		if isTerminal {
			newRunStatus = diagnosis.StatusFailed
			newAttemptStatus = diagnosis.AttemptStatusFailedTerminal
		} else {
			newRunStatus = diagnosis.StatusRetryWait
			newAttemptStatus = diagnosis.AttemptStatusFailedRetryable
		}

		_ = h.diagnosisStore.FinishAttemptAndRun(
			ctx, run.ID, attempt.ID, newRunStatus, newAttemptStatus,
			0, 0, 0, errCode, execErr.Error(),
			!isTerminal, 5*time.Second,
		)
		return execErr
	}

	// Process Report & Citations
	if result != nil && result.Report != nil {
		findingsBytes, _ := json.Marshal(result.Report.Findings)
		checksBytes, _ := json.Marshal(result.Report.RecommendedChecks)

		rep := &evidence.Report{
			ID:                    uuid.New().String(),
			DiagnosisRunID:        run.ID,
			AttemptID:             attempt.ID,
			RootCause:             result.Report.RootCause,
			FindingsJSON:          string(findingsBytes),
			RecommendedChecksJSON: string(checksBytes),
			Confidence:            result.Report.Confidence,
			CreatedAt:             time.Now().UTC(),
		}
		if err := h.reportStore.Create(ctx, rep); err != nil {
			log.Error("failed saving report", "error", err)
		}

		var allCitations []evidence.Citation
		for _, f := range result.Report.Findings {
			for _, cit := range f.Citations {
				cit.ReportID = rep.ID
				cit.SnapshotID = run.SnapshotID
				cit.CreatedAt = time.Now().UTC()
				if h.citationVal != nil {
					h.citationVal.Validate(ctx, run.RepositoryID, run.SnapshotID, &cit)
				}
				allCitations = append(allCitations, cit)
			}
		}
		if len(allCitations) > 0 {
			if err := h.citationStore.CreateBatch(ctx, allCitations); err != nil {
				log.Error("failed saving citations", "error", err)
			}
		}
	}

	promptTokens := 0
	completionTokens := 0
	toolCalls := 0
	if result != nil {
		promptTokens = result.PromptTokens
		completionTokens = result.CompletionTokens
		toolCalls = result.ToolCalls
	}

	// Finalize status to SUCCEEDED
	if err := h.diagnosisStore.FinishAttemptAndRun(
		ctx, run.ID, attempt.ID, diagnosis.StatusSucceeded, diagnosis.AttemptStatusSucceeded,
		promptTokens, completionTokens, toolCalls,
		"", "", false, 0,
	); err != nil {
		log.Error("failed marking diagnosis run succeeded", "error", err)
		return jobs.NewRetryableError("UPDATE_STATUS_FAILED", "failed updating diagnosis run status", err)
	}

	log.Info("diagnosis job completed successfully")
	return nil
}
