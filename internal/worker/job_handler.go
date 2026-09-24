package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"repolens/internal/agent"
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
	evidenceIssuer evidence.EvidenceIssuer
	executor       DiagnosisExecutor
}

func (h *DiagnosisJobHandler) WithEvidenceIssuer(issuer evidence.EvidenceIssuer) *DiagnosisJobHandler {
	h.evidenceIssuer = issuer
	return h
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
	executionGeneration := job.ExecutionGeneration
	if executionGeneration <= 0 {
		executionGeneration = 1
	}
	attemptNo := job.AttemptCount
	if attemptNo <= 0 {
		attemptNo = 1
	}
	now := time.Now().UTC()
	attempt := &diagnosis.DiagnosisAttempt{
		ID:                  uuid.NewString(),
		DiagnosisRunID:      run.ID,
		ExecutionGeneration: executionGeneration,
		AttemptNo:           attemptNo,
		WorkerID:            workerID,
		Status:              diagnosis.AttemptStatusRunning,
		StartedAt:           now,
		HeartbeatAt:         now,
		DeadlineAt:          now.Add(30 * time.Minute),
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
	// Keep Attempt liveness independent of Provider latency. RecoverySweeper
	// uses these heartbeats, and its stale duration is longer than this interval.
	heartbeat := NewHeartbeatEmitter(h.diagnosisStore, attempt.ID, 5*time.Second)
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		heartbeat.Start(heartbeatCtx)
	}()
	defer func() {
		stopHeartbeat()
		heartbeat.Stop()
		<-heartbeatDone
	}()

	if run.CancelRequested || job.CancelRequested {
		log.Info("diagnosis cancellation requested")
		return h.cancelAttempt(ctx, job, run, attempt)
	}

	var result *ExecutionResult
	var execErr error
	if checkpointStore, ok := h.diagnosisStore.(interface {
		GetLatestFinalCheckpoint(context.Context, string, int) (*diagnosis.DiagnosisAttempt, error)
	}); ok {
		checkpoint, checkpointErr := checkpointStore.GetLatestFinalCheckpoint(ctx, run.ID, attempt.ExecutionGeneration)
		if checkpointErr != nil {
			return jobs.NewRetryableError("CHECKPOINT_LOAD_FAILED", "failed loading diagnosis checkpoint", checkpointErr)
		}
		if checkpoint != nil {
			if checkpoint.CheckpointAgentVersion != "" && (checkpoint.CheckpointAgentVersion != run.AgentVersion || checkpoint.CheckpointPromptVersion != run.PromptVersion) {
				versionErr := jobs.NewPermanentError("CHECKPOINT_VERSION_MISMATCH", "diagnosis checkpoint uses an incompatible Agent protocol; explicit retry is required", nil)
				finalizeCtx, cancelFinalize := context.WithTimeout(context.Background(), 10*time.Second)
				if finalizeErr := h.diagnosisStore.FinishAttemptAndRun(finalizeCtx, run.ID, attempt.ID, diagnosis.StatusFailed, diagnosis.AttemptStatusFailedTerminal, 0, 0, 0, "CHECKPOINT_VERSION_MISMATCH", versionErr.Error(), false, 0); finalizeErr != nil {
					log.Error("failed to terminalize incompatible diagnosis checkpoint", "error", finalizeErr)
				}
				cancelFinalize()
				return versionErr
			}
			result = executionResultFromCheckpoint(checkpoint)
			if checkpoint.CheckpointKind == diagnosis.CheckpointKindFinalInvalid {
				message := result.ParseError
				if message == "" {
					message = "agent returned an invalid structured report"
				}
				execErr = fmt.Errorf("%w: %s", agent.ErrInvalidStructuredReport, message)
			}
			if result.ReportDraft != nil && h.evidenceIssuer != nil {
				result.ReportDraft = rebindCheckpointDraft(ctx, h.evidenceIssuer, result.ReportDraft, checkpoint.ID, run, attempt)
				result.Report = resolveCheckpointDraft(ctx, h.evidenceIssuer, result.ReportDraft, run, attempt)
			}
			log.Info("resuming diagnosis from provider checkpoint", "checkpoint_attempt_id", checkpoint.ID)
		}
	}
	if result == nil {
		result, execErr = h.executor.Execute(ctx, run, attempt)
	}
	// A result that claims to be structured must satisfy the same complete
	// contract used by evidence classification. This guard also protects
	// checkpoint/resume and custom executors from bypassing Agent parsing.
	if execErr == nil && result != nil {
		if !result.StructuredReport || result.Report == nil {
			result.StructuredReport = false
			if result.ParseError == "" {
				result.ParseError = "structured report is missing or marked invalid"
			}
			execErr = fmt.Errorf("%w: %s", agent.ErrInvalidStructuredReport, result.ParseError)
		} else if validationErr := evidence.ValidateReportStructure(result.Report); validationErr != nil {
			result.StructuredReport = false
			result.ParseError = validationErr.Error()
			execErr = fmt.Errorf("%w: %v", agent.ErrInvalidStructuredReport, validationErr)
		}
	}
	if result != nil {
		checkpointKind := diagnosis.CheckpointKindPartialProviderFailure
		switch {
		case errors.Is(execErr, agent.ErrInvalidStructuredReport):
			checkpointKind = diagnosis.CheckpointKindFinalInvalid
		case execErr == nil:
			checkpointKind = diagnosis.CheckpointKindFinalValid
		}
		if checkpointErr := h.saveAttemptCheckpoint(attempt, run, result, checkpointKind); checkpointErr != nil {
			log.Error("failed to persist provider checkpoint; refusing automatic provider retry", "error", checkpointErr)
			finalizeCtx, cancelFinalize := context.WithTimeout(context.Background(), 10*time.Second)
			finalizeErr := h.diagnosisStore.FinishAttemptAndRun(finalizeCtx, run.ID, attempt.ID, diagnosis.StatusFailed, diagnosis.AttemptStatusFailedTerminal, result.PromptTokens, result.CompletionTokens, result.ToolCalls, "CHECKPOINT_SAVE_FAILED", "provider checkpoint could not be persisted; explicit diagnosis retry is required", false, 0)
			cancelFinalize()
			if finalizeErr != nil {
				log.Error("failed to terminalize diagnosis after checkpoint failure", "error", finalizeErr)
			}
			return jobs.NewPermanentError("CHECKPOINT_SAVE_FAILED", "provider checkpoint could not be persisted; explicit diagnosis retry is required", checkpointErr)
		}
	}
	if errors.Is(execErr, agent.ErrInvalidStructuredReport) {
		promptTokens, completionTokens, toolCalls := 0, 0, 0
		if result != nil {
			promptTokens, completionTokens, toolCalls = result.PromptTokens, result.CompletionTokens, result.ToolCalls
		}
		message := "INVALID_STRUCTURED_REPORT: INVALID_REPORT_STRUCTURE"
		if result != nil && result.ParseError != "" {
			message = safeStructuredParseMessage(result.ParseError)
		}
		rawOutput := ""
		if result != nil {
			rawOutput = result.RawOutput
		}
		report := &evidence.Report{
			ID:                    uuid.New().String(),
			DiagnosisRunID:        run.ID,
			AttemptID:             attempt.ID,
			ReportStatus:          evidence.ReportInvalid,
			FindingsJSON:          "[]",
			RecommendedChecksJSON: "[]",
			StructuredPayloadJSON: "{}",
			RawOutput:             rawOutput,
			ParseError:            message,
			LimitationsJSON:       "[]",
			CreatedAt:             time.Now().UTC(),
		}
		if finalizer, ok := h.diagnosisStore.(interface {
			FinalizeInvalidStructuredReport(context.Context, int64, string, string, string, string, *evidence.Report, int, int, int, string, string) error
		}); ok {
			if job.WorkerID == nil || job.ClaimToken == nil {
				return jobs.ErrOwnershipLost
			}
			finalizeCtx, cancelFinalize := context.WithTimeout(context.Background(), 10*time.Second)
			finalizeErr := finalizer.FinalizeInvalidStructuredReport(finalizeCtx, job.ID, *job.WorkerID, *job.ClaimToken, run.ID, attempt.ID, report, promptTokens, completionTokens, toolCalls, agent.ErrCodeInvalidStructuredReport, message)
			cancelFinalize()
			if errors.Is(finalizeErr, jobs.ErrAlreadyFinalized) {
				return jobs.ErrAlreadyFinalized
			}
			if errors.Is(finalizeErr, jobs.ErrCancellationRequested) {
				return h.cancelAttempt(ctx, job, run, attempt)
			}
			if finalizeErr != nil {
				h.closeCheckpointAttempt(ctx, run, attempt, "ATOMIC_INVALID_FINALIZE_FAILED", finalizeErr)
				return jobs.NewRetryableError("ATOMIC_INVALID_FINALIZE_FAILED", "failed to atomically finalize invalid structured report", finalizeErr)
			}
			return jobs.NewPermanentError(agent.ErrCodeInvalidStructuredReport, "agent returned an invalid structured report", execErr)
		}
		// Compatibility path for lightweight non-SQL stores.
		if h.reportStore != nil {
			if persistErr := h.reportStore.Create(ctx, report); persistErr != nil {
				return jobs.NewRetryableError("REPORT_PERSIST_FAILED", persistErr.Error(), persistErr)
			}
		}
		_ = h.diagnosisStore.FinishAttemptAndRun(ctx, run.ID, attempt.ID, diagnosis.StatusFailed, diagnosis.AttemptStatusFailedTerminal, promptTokens, completionTokens, toolCalls, agent.ErrCodeInvalidStructuredReport, message, false, 0)
		return jobs.NewPermanentError(agent.ErrCodeInvalidStructuredReport, "agent returned an invalid structured report", execErr)
	}
	if execErr != nil {
		log.Error("agent execution failed", "error", execErr)
		if errors.Is(execErr, agent.ErrModelOutputTruncated) {
			finalizeCtx, cancelFinalize := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelFinalize()
			promptTokens, completionTokens, toolCalls := 0, 0, 0
			if result != nil {
				promptTokens, completionTokens, toolCalls = result.PromptTokens, result.CompletionTokens, result.ToolCalls
			}
			if finalizeErr := h.diagnosisStore.FinishAttemptAndRun(finalizeCtx, run.ID, attempt.ID, diagnosis.StatusFailed, diagnosis.AttemptStatusFailedTerminal, promptTokens, completionTokens, toolCalls, agent.ErrCodeModelOutputTruncated, execErr.Error(), false, 0); finalizeErr != nil {
				log.Error("failed to terminalize truncated diagnosis", "error", finalizeErr)
			}
			return jobs.NewPermanentError(agent.ErrCodeModelOutputTruncated, "agent output was truncated before a tool call or structured report", execErr)
		}
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
		structuredPayloadBytes, _ := json.Marshal(result.Report)
		rep := &evidence.Report{
			ID:                     uuid.New().String(),
			DiagnosisRunID:         run.ID,
			AttemptID:              attempt.ID,
			RootCause:              result.Report.RootCause,
			ConclusionKind:         result.Report.ConclusionKind,
			Summary:                result.Report.Summary,
			FindingsJSON:           string(findingsBytes),
			RecommendedChecksJSON:  string(checksBytes),
			StructuredPayloadJSON:  string(structuredPayloadBytes),
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
				if h.citationVal != nil && cit.EvidenceID == "" {
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
				h.closeCheckpointAttempt(ctx, run, attempt, "ATOMIC_FINALIZE_FAILED", err)
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

// rebindCheckpointDraft carries evidence handles from a provider checkpoint
// into the new attempt that is performing finalization. Evidence handles are
// intentionally attempt-scoped, so reusing the old opaque ID would make a
// valid checkpoint look like a forged citation after a retry.
func rebindCheckpointDraft(ctx context.Context, issuer evidence.EvidenceIssuer, draft *evidence.ReportDraft, sourceAttemptID string, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) *evidence.ReportDraft {
	if draft == nil || issuer == nil || sourceAttemptID == "" || attempt == nil || sourceAttemptID == attempt.ID {
		return draft
	}

	rebound := *draft
	rebound.Findings = make([]evidence.FindingDraft, len(draft.Findings))
	for findingIndex, finding := range draft.Findings {
		rebound.Findings[findingIndex] = finding
		rebound.Findings[findingIndex].Citations = append([]evidence.CitationRef(nil), finding.Citations...)
		for citationIndex, citation := range rebound.Findings[findingIndex].Citations {
			evidenceID := strings.TrimSpace(citation.EvidenceID)
			if evidenceID == "" {
				continue
			}
			item, err := issuer.Resolve(ctx, sourceAttemptID, evidenceID)
			if err != nil || item == nil || item.DiagnosisRunID != run.ID || item.SnapshotID != run.SnapshotID || item.CodeIndexBuildID != run.CodeIndexBuildID {
				continue
			}
			reissued, err := issuer.Issue(ctx, evidence.IssueRequest{
				AttemptID:        attempt.ID,
				DiagnosisRunID:   run.ID,
				RepositoryID:     run.RepositoryID,
				SnapshotID:       item.SnapshotID,
				CodeIndexBuildID: item.CodeIndexBuildID,
				SourceKind:       item.SourceKind,
				SourceStepSeq:    valueOrZero(item.SourceStepSeq),
				RetrievalChunkID: item.RetrievalChunkID,
				FilePath:         item.FilePath,
				StartLine:        item.StartLine,
				EndLine:          item.EndLine,
			})
			if err == nil && reissued != nil {
				rebound.Findings[findingIndex].Citations[citationIndex].EvidenceID = reissued.ID
			}
		}
	}
	return &rebound
}

func valueOrZero(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func resolveCheckpointDraft(ctx context.Context, issuer evidence.EvidenceIssuer, draft *evidence.ReportDraft, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) *evidence.DiagnosisReportData {
	report, err := evidence.ResolveReportDraft(ctx, issuer, draft, evidence.DraftLineage{
		AttemptID:        attempt.ID,
		DiagnosisRunID:   run.ID,
		RepositoryID:     run.RepositoryID,
		SnapshotID:       run.SnapshotID,
		CodeIndexBuildID: run.CodeIndexBuildID,
	})
	if err != nil || report == nil {
		return &evidence.DiagnosisReportData{}
	}
	return report
}

func (h *DiagnosisJobHandler) saveAttemptCheckpoint(attempt *diagnosis.DiagnosisAttempt, run *diagnosis.DiagnosisRun, result *ExecutionResult, kind diagnosis.CheckpointKind) error {
	if attempt == nil || run == nil || result == nil {
		return nil
	}
	parsedReport, _ := json.Marshal(result.Report)
	parsedDraft, _ := json.Marshal(result.ReportDraft)
	checkpoint := diagnosis.AttemptCheckpoint{
		ExecutionGeneration: attempt.ExecutionGeneration,
		Kind:                kind,
		RawOutput:           result.RawOutput,
		ParsedReportJSON:    string(parsedReport),
		ParsedDraftJSON:     string(parsedDraft),
		PromptVersion:       run.PromptVersion,
		AgentVersion:        run.AgentVersion,
		Structured:          result.StructuredReport,
		PromptTokens:        result.PromptTokens,
		CompletionTokens:    result.CompletionTokens,
		CachedPromptTokens:  result.CachedPromptTokens,
		ReasoningTokens:     result.ReasoningTokens,
		ToolCalls:           result.ToolCalls,
		AgentRounds:         result.AgentRounds,
		SearchCalls:         result.SearchCalls,
		ProviderCalls:       result.ProviderCalls,
		FinalizationReason:  result.FinalizationReason,
		FinishReason:        result.FinishReason,
	}
	if kind == diagnosis.CheckpointKindFinalInvalid {
		checkpoint.ErrorCode = agent.ErrCodeInvalidStructuredReport
		checkpoint.ErrorMessage = safeStructuredParseMessage(result.ParseError)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if store, ok := h.diagnosisStore.(interface {
		UpdateAttemptCheckpointWithDraft(context.Context, string, diagnosis.AttemptCheckpoint) error
	}); ok {
		return store.UpdateAttemptCheckpointWithDraft(ctx, attempt.ID, checkpoint)
	}
	if store, ok := h.diagnosisStore.(interface {
		UpdateAttemptCheckpoint(context.Context, string, diagnosis.AttemptCheckpoint) error
	}); ok {
		return store.UpdateAttemptCheckpoint(ctx, attempt.ID, checkpoint)
	}
	return nil
}

func safeStructuredParseMessage(message string) string {
	for _, category := range []string{"UNKNOWN_FIELD", "TRAILING_JSON", "MALFORMED_JSON", "INVALID_FIELD_TYPE", "INVALID_REPORT_STRUCTURE"} {
		if strings.Contains(message, category) {
			return "INVALID_STRUCTURED_REPORT: " + category
		}
	}
	return "INVALID_STRUCTURED_REPORT: INVALID_REPORT_STRUCTURE"
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
		FinishReason:       checkpoint.FinishReason,
		StructuredReport:   checkpoint.StructuredOutputValid,
		FinalizationReason: checkpoint.FinalizationReason,
		ParseError:         checkpoint.CheckpointErrorMessage,
	}
	if checkpoint.ParsedReportJSON != "" {
		var report evidence.DiagnosisReportData
		if err := json.Unmarshal([]byte(checkpoint.ParsedReportJSON), &report); err == nil {
			result.Report = &report
		} else {
			result.ParseError = "checkpoint parsed report is invalid"
		}
	}
	if checkpoint.ParsedReportDraftJSON != "" {
		var draft evidence.ReportDraft
		if err := json.Unmarshal([]byte(checkpoint.ParsedReportDraftJSON), &draft); err == nil {
			result.ReportDraft = &draft
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

// closeCheckpointAttempt prevents a durable provider result from leaving its
// source attempt RUNNING if the atomic finalizer rolls back. The run remains
// RUNNING while the generic worker schedules a same-generation retry, which
// restores this checkpoint without another Provider call.
func (h *DiagnosisJobHandler) closeCheckpointAttempt(ctx context.Context, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt, code string, cause error) {
	if run == nil || attempt == nil {
		return
	}
	getter, ok := h.diagnosisStore.(interface {
		GetAttempt(context.Context, string) (*diagnosis.DiagnosisAttempt, error)
	})
	if !ok {
		return
	}
	finalizeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	saved, err := getter.GetAttempt(finalizeCtx, attempt.ID)
	if err != nil || saved.ProviderCompletedAt == nil || (saved.CheckpointKind != diagnosis.CheckpointKindFinalValid && saved.CheckpointKind != diagnosis.CheckpointKindFinalInvalid) {
		return
	}
	status := diagnosis.AttemptStatusFailedRetryable
	errorCode := code
	retryable := true
	freshRun, runErr := h.diagnosisStore.GetByID(finalizeCtx, run.ID)
	ownershipLost := errors.Is(cause, jobs.ErrOwnershipLost) || errors.Is(cause, jobs.ErrAlreadyFinalized)
	terminalRun := runErr == nil && (freshRun.Status == diagnosis.StatusSucceeded || freshRun.Status == diagnosis.StatusFailed || freshRun.Status == diagnosis.StatusCancelled)
	if ownershipLost || errors.Is(runErr, diagnosis.ErrRunNotFound) || terminalRun {
		status = diagnosis.AttemptStatusAbandoned
		errorCode = "FINALIZATION_OWNERSHIP_LOST"
		retryable = false
	}
	message := "final report checkpoint was saved, but this Attempt no longer owns Run finalization"
	if status == diagnosis.AttemptStatusFailedRetryable {
		message = "final report checkpoint was saved; automatic retry can resume without another Provider call"
	}
	if closeErr := h.diagnosisStore.CloseAttempt(finalizeCtx, run.ID, attempt.ID, saved.ExecutionGeneration, status, errorCode, message, retryable); closeErr != nil && !errors.Is(closeErr, diagnosis.ErrAttemptNotRunning) {
		logger.L(finalizeCtx).Error("failed to close checkpoint Attempt without changing Run state", "attempt_id", attempt.ID, "error", closeErr)
	}
}
