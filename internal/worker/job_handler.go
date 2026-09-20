package worker

import (
	"context"
	"encoding/json"
	"errors"
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
		Status:         diagnosis.AttemptStatusRunning,
		StartedAt:      time.Now().UTC(),
		HeartbeatAt:    time.Now().UTC(),
		DeadlineAt:     time.Now().UTC().Add(30 * time.Minute),
	}

	if starter, ok := h.diagnosisStore.(interface {
		StartAttempt(context.Context, string, *diagnosis.DiagnosisAttempt) error
	}); ok {
		if err := starter.StartAttempt(ctx, run.ID, attempt); err != nil {
			return jobs.NewRetryableError("START_ATTEMPT_FAILED", err.Error(), err)
		}
	} else {
		// Compatibility path for lightweight unit stores.
		if err := h.diagnosisStore.FinishAttemptAndRun(ctx, run.ID, attempt.ID, diagnosis.StatusRunning, diagnosis.AttemptStatusRunning, 0, 0, 0, "", "", false, 0); err != nil {
			return jobs.NewRetryableError("START_ATTEMPT_FAILED", err.Error(), err)
		}
	}
	if run.CancelRequested || job.CancelRequested {
		log.Info("diagnosis cancellation requested")
		return h.cancelAttempt(ctx, job, run, attempt)
	}

	var result *ExecutionResult
	var execErr error
	if checkpointStore, ok := h.diagnosisStore.(interface {
		GetLatestCheckpoint(context.Context, string) (*diagnosis.DiagnosisAttempt, error)
	}); ok {
		checkpoint, checkpointErr := checkpointStore.GetLatestCheckpoint(ctx, run.ID)
		if checkpointErr != nil {
			return jobs.NewRetryableError("CHECKPOINT_LOAD_FAILED", "failed loading diagnosis checkpoint", checkpointErr)
		}
		if checkpoint != nil {
			result = executionResultFromCheckpoint(checkpoint)
			log.Info("resuming diagnosis from provider checkpoint", "checkpoint_attempt_id", checkpoint.ID)
		}
	}
	if result == nil {
		result, execErr = h.executor.Execute(ctx, run, attempt)
	}
	if result != nil {
		parsedReport, _ := json.Marshal(result.Report)
		if checkpoint, ok := h.diagnosisStore.(interface {
			UpdateAttemptCheckpoint(context.Context, string, string, string, bool, int, int, int, int, int, int, int, int, string) error
		}); ok {
			checkpointCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = checkpoint.UpdateAttemptCheckpoint(checkpointCtx, attempt.ID, result.RawOutput, string(parsedReport), result.StructuredReport, result.PromptTokens, result.CompletionTokens, result.CachedPromptTokens, result.ReasoningTokens, result.ToolCalls, result.AgentRounds, result.SearchCalls, result.ProviderCalls, result.FinalizationReason)
			cancel()
		}
	}
	if execErr != nil {
		log.Error("agent execution failed", "error", execErr)
		if progressErr, ok := execErr.(interface {
			Progressed() bool
		}); ok && progressErr.Progressed() && result != nil {
			finalizeCtx, cancelFinalize := context.WithTimeout(context.Background(), 10*time.Second)
			_ = h.diagnosisStore.FinishAttemptAndRun(finalizeCtx, run.ID, attempt.ID, diagnosis.StatusFailed, diagnosis.AttemptStatusFailedTerminal, result.PromptTokens, result.CompletionTokens, result.ToolCalls, "PROVIDER_PROGRESS_ABORTED", execErr.Error(), false, 0)
			cancelFinalize()
			terminal := jobs.NewPermanentError("PROVIDER_PROGRESS_ABORTED", "provider failed after agent progress; explicit diagnosis retry is required", execErr)
			return terminal
		}
		errClass, errCode := jobs.ClassifyError(execErr)
		if errors.Is(execErr, context.Canceled) || errClass == jobs.ErrorClassCancelled {
			return h.cancelAttempt(ctx, job, run, attempt)
		}
		isTerminal := (errClass == jobs.ErrorClassPermanent) || (job.AttemptCount >= job.MaxAttempts)

		var newAttemptStatus diagnosis.AttemptStatus
		if isTerminal {
			newAttemptStatus = diagnosis.AttemptStatusFailedTerminal
		} else {
			// Retry belongs to AnalysisJob. Diagnosis remains RUNNING while
			// the job is in RETRY_WAIT.
			newAttemptStatus = diagnosis.AttemptStatusFailedRetryable
		}

		// Diagnosis remains RUNNING until the Job Store terminalizes it.
		_ = h.diagnosisStore.FinishAttempt(ctx, run.ID, attempt.ID, newAttemptStatus, 0, 0, 0, errCode, execErr.Error(), !isTerminal)
		return execErr
	}
	if result == nil || result.Report == nil {
		err := jobs.NewRetryableError("EMPTY_AGENT_OUTPUT", "agent did not return a structured diagnosis report", nil)
		h.failDiagnosisIfTerminal(ctx, job, run, attempt, err)
		return err
	}

	// Process Report & Citations
	if result != nil && result.Report != nil {
		findingsBytes, _ := json.Marshal(result.Report.Findings)
		checksBytes, _ := json.Marshal(result.Report.RecommendedChecks)
		limitationsBytes, _ := json.Marshal(result.Report.Limitations)
		rep := &evidence.Report{
			ID:                     uuid.New().String(),
			DiagnosisRunID:         run.ID,
			AttemptID:              attempt.ID,
			RootCause:              result.Report.RootCause,
			ConclusionKind:         result.Report.ConclusionKind,
			Summary:                result.Report.Summary,
			FindingsJSON:           string(findingsBytes),
			RecommendedChecksJSON:  string(checksBytes),
			Confidence:             result.Report.Confidence,
			ModelClaimedConfidence: result.Report.ModelClaimedConfidence,
			RawOutput:              result.RawOutput,
			ParseError:             result.ParseError,
			LimitationsJSON:        string(limitationsBytes),
			FinalizationReason:     result.FinalizationReason,
			CreatedAt:              time.Now().UTC(),
		}
		var allCitations []evidence.Citation
		for findingIndex := range result.Report.Findings {
			for citationIndex := range result.Report.Findings[findingIndex].Citations {
				cit := result.Report.Findings[findingIndex].Citations[citationIndex]
				cit.ReportID = rep.ID
				cit.SnapshotID = run.SnapshotID
				cit.CodeIndexBuildID = run.CodeIndexBuildID
				cit.CreatedAt = time.Now().UTC()
				if h.citationVal != nil {
					h.citationVal.Validate(ctx, run.RepositoryID, run.SnapshotID, &cit)
				}
				result.Report.Findings[findingIndex].Citations[citationIndex] = cit
				allCitations = append(allCitations, cit)
			}
		}
		if validatedFindings, marshalErr := json.Marshal(result.Report.Findings); marshalErr == nil {
			rep.FindingsJSON = string(validatedFindings)
		}
		quality, qualityErr := evidence.ClassifyReport(result.Report, result.StructuredReport)
		if qualityErr != nil && result.ParseError == "" {
			result.ParseError = qualityErr.Error()
		}
		rep.ParseError = result.ParseError
		rep.ReportStatus = quality.Status
		rep.FindingCount = quality.FindingCount
		rep.SupportedFindingCount = quality.SupportedFindingCount
		rep.UnsupportedFindingCount = quality.UnsupportedFindingCount
		rep.ValidCitationCount = quality.ValidCitationCount
		rep.InvalidCitationCount = quality.InvalidCitationCount
		rep.CitationCoverage = quality.CitationCoverage

		promptTokens := 0
		completionTokens := 0
		toolCalls := 0
		if result != nil {
			promptTokens, completionTokens, toolCalls = result.PromptTokens, result.CompletionTokens, result.ToolCalls
		}
		if finalizer, ok := h.diagnosisStore.(interface {
			FinalizeSuccess(context.Context, int64, string, string, string, string, *evidence.Report, []evidence.Citation, int, int, int) error
		}); ok {
			if job.ClaimToken == nil || job.WorkerID == nil {
				return jobs.ErrOwnershipLost
			}
			if err := finalizer.FinalizeSuccess(ctx, job.ID, *job.WorkerID, *job.ClaimToken, run.ID, attempt.ID, rep, allCitations, promptTokens, completionTokens, toolCalls); err != nil {
				if errors.Is(err, jobs.ErrCancellationRequested) {
					return h.cancelAttempt(ctx, job, run, attempt)
				}
				h.failDiagnosisIfTerminal(ctx, job, run, attempt, err)
				return jobs.NewRetryableError("ATOMIC_FINALIZE_FAILED", err.Error(), err)
			}
			log.Info("diagnosis job completed successfully")
			return nil
		}
		// Compatibility fallback for non-SQL test stores.
		if err := h.reportStore.Create(ctx, rep); err != nil {
			h.failDiagnosisIfTerminal(ctx, job, run, attempt, err)
			return jobs.NewRetryableError("REPORT_PERSIST_FAILED", err.Error(), err)
		}
		if err := h.citationStore.CreateBatch(ctx, allCitations); err != nil {
			h.failDiagnosisIfTerminal(ctx, job, run, attempt, err)
			return jobs.NewRetryableError("CITATION_PERSIST_FAILED", err.Error(), err)
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

func executionResultFromCheckpoint(checkpoint *diagnosis.DiagnosisAttempt) *ExecutionResult {
	result := &ExecutionResult{
		RawOutput:          checkpoint.RawOutput,
		PromptTokens:       checkpoint.PromptTokens,
		CompletionTokens:   checkpoint.CompletionTokens,
		CachedPromptTokens: checkpoint.CachedPromptTokens,
		ReasoningTokens:    checkpoint.ReasoningTokens,
		ToolCalls:          checkpoint.ToolCalls,
		AgentRounds:        checkpoint.AgentRounds,
		SearchCalls:        checkpoint.SearchCalls,
		ProviderCalls:      checkpoint.ProviderCalls,
		StructuredReport:   checkpoint.StructuredOutputValid,
		FinalizationReason: checkpoint.FinalizationReason,
	}
	if checkpoint.ParsedReportJSON != "" {
		var report evidence.DiagnosisReportData
		if err := json.Unmarshal([]byte(checkpoint.ParsedReportJSON), &report); err == nil {
			result.Report = &report
		} else {
			result.ParseError = "checkpoint parsed report is invalid"
		}
	}
	if result.Report == nil {
		result.Report = &evidence.DiagnosisReportData{}
	}
	return result
}

func (h *DiagnosisJobHandler) failDiagnosisIfTerminal(ctx context.Context, job *jobs.AnalysisJob, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt, err error) {
	if job == nil || job.AttemptCount < job.MaxAttempts {
		return
	}
	class, code := jobs.ClassifyError(err)
	if class == jobs.ErrorClassOwnershipLost {
		return
	}
	_ = h.diagnosisStore.FinishAttempt(ctx, run.ID, attempt.ID, diagnosis.AttemptStatusFailedTerminal, 0, 0, 0, code, err.Error(), false)
}

func (h *DiagnosisJobHandler) cancelAttempt(ctx context.Context, job *jobs.AnalysisJob, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) error {
	// The execution context is expected to be cancelled when this path is
	// reached after an agent stops. Terminal state persistence must therefore
	// use an independent, bounded context so the atomic business/job finalize
	// transaction can still complete.
	finalizeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if finalizer, ok := h.diagnosisStore.(interface {
		FinalizeCancellation(context.Context, int64, string, string, string, string) error
	}); ok && job.WorkerID != nil && job.ClaimToken != nil {
		if err := finalizer.FinalizeCancellation(finalizeCtx, job.ID, *job.WorkerID, *job.ClaimToken, run.ID, attempt.ID); err != nil {
			return err
		}
		return jobs.NewPermanentError("CANCELLED", "diagnosis was cancelled", context.Canceled)
	}
	_ = h.diagnosisStore.FinishAttemptAndRun(finalizeCtx, run.ID, attempt.ID, diagnosis.StatusCancelled, diagnosis.AttemptStatusCancelled, 0, 0, 0, "CANCELLED", "User requested cancellation", false, 0)
	return jobs.NewPermanentError("CANCELLED", "diagnosis was cancelled", context.Canceled)
}
