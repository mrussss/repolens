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
		citationVal:    citationVal,
		executor:       executor,
	}
}

// Execute processes a RUN_DIAGNOSIS job.
func (h *DiagnosisJobHandler) Execute(ctx context.Context, job *jobs.AnalysisJob) error {
	if errors.Is(context.Cause(ctx), jobs.ErrWorkerShutdown) {
		return jobs.ErrWorkerShutdown
	}
	if errors.Is(context.Cause(ctx), jobs.ErrOwnershipLost) {
		return jobs.ErrOwnershipLost
	}
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

	if err := h.diagnosisStore.StartAttempt(ctx, run.ID, attempt); err != nil {
		return jobs.NewRetryableError("START_ATTEMPT_FAILED", err.Error(), err)
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
				finalizeErr := h.finalizeDiagnosisFailure(finalizeCtx, job, run, attempt, jobs.ErrorClassPermanent, "CHECKPOINT_VERSION_MISMATCH", versionErr.Error(), nil)
				if finalizeErr != nil && !errors.Is(finalizeErr, jobs.ErrAlreadyFinalized) {
					log.Error("failed to terminalize incompatible diagnosis checkpoint", "error", finalizeErr)
				}
				cancelFinalize()
				if finalizeErr == nil || errors.Is(finalizeErr, jobs.ErrAlreadyFinalized) {
					return jobs.ErrAlreadyFinalized
				}
				return jobs.NewRetryableError("CHECKPOINT_VERSION_MISMATCH_FINALIZE_FAILED", "failed to finalize incompatible diagnosis checkpoint", finalizeErr)
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
			finalizeErr := h.finalizeDiagnosisFailure(finalizeCtx, job, run, attempt, jobs.ErrorClassPermanent, "CHECKPOINT_SAVE_FAILED", "provider checkpoint could not be persisted; explicit diagnosis retry is required", result)
			cancelFinalize()
			if finalizeErr != nil && !errors.Is(finalizeErr, jobs.ErrAlreadyFinalized) {
				log.Error("failed to terminalize diagnosis after checkpoint failure", "error", finalizeErr)
				return jobs.NewRetryableError("CHECKPOINT_SAVE_FAILED_FINALIZE_FAILED", "failed to finalize diagnosis after checkpoint failure", finalizeErr)
			}
			return jobs.ErrAlreadyFinalized
		}
	}
	if cause := context.Cause(ctx); errors.Is(cause, jobs.ErrWorkerShutdown) || errors.Is(cause, jobs.ErrOwnershipLost) {
		return cause
	} else if errors.Is(cause, jobs.ErrUserCancellation) || run.CancelRequested || job.CancelRequested {
		return h.cancelAttempt(ctx, job, run, attempt)
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
			rawOutput = agent.RedactSecrets(result.RawOutput)
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
		if job.WorkerID == nil || job.ClaimToken == nil {
			return jobs.ErrOwnershipLost
		}
		finalizeCtx, cancelFinalize := context.WithTimeout(context.Background(), 10*time.Second)
		finalizeErr := h.diagnosisStore.FinalizeInvalidStructuredReport(finalizeCtx, job.ID, *job.WorkerID, *job.ClaimToken, run.ID, attempt.ID, report, promptTokens, completionTokens, toolCalls, agent.ErrCodeInvalidStructuredReport, message)
		cancelFinalize()
		if errors.Is(finalizeErr, jobs.ErrAlreadyFinalized) {
			return jobs.ErrAlreadyFinalized
		}
		if errors.Is(finalizeErr, jobs.ErrCancellationRequested) {
			return h.cancelAttempt(ctx, job, run, attempt)
		}
		if finalizeErr != nil {
			return h.handleFinalizerFailure(ctx, job, run, attempt, "ATOMIC_INVALID_FINALIZE_FAILED", finalizeErr)
		}
		return jobs.ErrAlreadyFinalized
	}
	if execErr != nil {
		log.Error("agent execution failed", "error", execErr)
		if cause := context.Cause(ctx); errors.Is(cause, jobs.ErrWorkerShutdown) || errors.Is(cause, jobs.ErrOwnershipLost) {
			return cause
		} else if errors.Is(cause, jobs.ErrUserCancellation) || run.CancelRequested || job.CancelRequested {
			return h.cancelAttempt(ctx, job, run, attempt)
		}
		errorCode := ""
		if errors.Is(execErr, agent.ErrModelOutputTruncated) {
			execErr = jobs.NewPermanentError(agent.ErrCodeModelOutputTruncated, "agent output was truncated before a tool call or structured report", execErr)
			errorCode = agent.ErrCodeModelOutputTruncated
		}
		if progressErr, ok := execErr.(interface {
			Progressed() bool
		}); ok && progressErr.Progressed() && result != nil {
			execErr = jobs.NewPermanentError("PROVIDER_PROGRESS_ABORTED", "provider failed after agent progress; explicit diagnosis retry is required", execErr)
			errorCode = "PROVIDER_PROGRESS_ABORTED"
		}
		var codedError interface {
			ErrorCode() string
			Permanent() bool
		}
		if errors.As(execErr, &codedError) && codedError.Permanent() {
			errorCode = codedError.ErrorCode()
			execErr = jobs.NewPermanentError(errorCode, errorCode, execErr)
		}
		errClass, errCode := jobs.ClassifyError(execErr)
		if errorCode != "" {
			errCode = errorCode
		}
		if errClass == jobs.ErrorClassOwnershipLost {
			return jobs.ErrOwnershipLost
		}
		if errClass == jobs.ErrorClassCancelled && errors.Is(execErr, jobs.ErrUserCancellation) {
			return h.cancelAttempt(ctx, job, run, attempt)
		}
		isTerminal := (errClass == jobs.ErrorClassPermanent) || (job.AttemptCount >= job.MaxAttempts)
		counts := result
		if isTerminal {
			return h.finalizeDiagnosisFailure(ctx, job, run, attempt, errClass, errCode, execErr.Error(), counts)
		}
		if err := h.finalizeRetryableAttempt(ctx, job, run, attempt, errCode, execErr.Error(), counts); err != nil {
			if errors.Is(err, jobs.ErrCancellationRequested) {
				return h.cancelAttempt(ctx, job, run, attempt)
			}
			return jobs.NewRetryableError("ATTEMPT_RETRY_FINALIZE_FAILED", "failed to close diagnosis attempt before retry", err)
		}
		return execErr
	}
	if result == nil || result.Report == nil {
		err := jobs.NewRetryableError("EMPTY_AGENT_OUTPUT", "agent did not return a structured diagnosis report", nil)
		if job.AttemptCount >= job.MaxAttempts {
			class, code := jobs.ClassifyError(err)
			return h.finalizeDiagnosisFailure(ctx, job, run, attempt, class, code, err.Error(), result)
		}
		if finalizeErr := h.finalizeRetryableAttempt(ctx, job, run, attempt, "EMPTY_AGENT_OUTPUT", err.Error(), result); finalizeErr != nil {
			if errors.Is(finalizeErr, jobs.ErrCancellationRequested) {
				return h.cancelAttempt(ctx, job, run, attempt)
			}
			return jobs.NewRetryableError("ATTEMPT_RETRY_FINALIZE_FAILED", "failed to close diagnosis attempt before retry", finalizeErr)
		}
		return err
	}
	if cause := context.Cause(ctx); errors.Is(cause, jobs.ErrWorkerShutdown) || errors.Is(cause, jobs.ErrOwnershipLost) {
		return cause
	} else if errors.Is(cause, jobs.ErrUserCancellation) || run.CancelRequested || job.CancelRequested {
		return h.cancelAttempt(ctx, job, run, attempt)
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
			RawOutput:              agent.RedactSecrets(result.RawOutput),
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
		if job.ClaimToken == nil || job.WorkerID == nil {
			return jobs.ErrOwnershipLost
		}
		finalizeCtx, cancelFinalize := context.WithTimeout(context.Background(), 10*time.Second)
		finalizeErr := h.diagnosisStore.FinalizeSuccess(finalizeCtx, job.ID, *job.WorkerID, *job.ClaimToken, run.ID, attempt.ID, rep, allCitations, promptTokens, completionTokens, toolCalls)
		cancelFinalize()
		if errors.Is(finalizeErr, jobs.ErrCancellationRequested) {
			return h.cancelAttempt(ctx, job, run, attempt)
		}
		if errors.Is(finalizeErr, jobs.ErrAlreadyFinalized) {
			return jobs.ErrAlreadyFinalized
		}
		if finalizeErr != nil {
			return h.handleFinalizerFailure(ctx, job, run, attempt, "ATOMIC_FINALIZE_FAILED", finalizeErr)
		}
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
		RawOutput:           agent.RedactSecrets(result.RawOutput),
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

func (h *DiagnosisJobHandler) cancelAttempt(ctx context.Context, job *jobs.AnalysisJob, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) error {
	// The execution context is expected to be cancelled when this path is
	// reached after an agent stops. Terminal state persistence must therefore
	// use an independent, bounded context so the atomic business/job finalize
	// transaction can still complete.
	finalizeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if job == nil || job.WorkerID == nil || job.ClaimToken == nil {
		return jobs.ErrOwnershipLost
	}
	if err := h.diagnosisStore.FinalizeCancellation(finalizeCtx, job.ID, *job.WorkerID, *job.ClaimToken, run.ID, attempt.ID); err != nil {
		return err
	}
	return jobs.ErrAlreadyFinalized
}

func (h *DiagnosisJobHandler) finalizeDiagnosisFailure(ctx context.Context, job *jobs.AnalysisJob, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt, class jobs.ErrorClass, code, message string, result *ExecutionResult) error {
	if cause := context.Cause(ctx); errors.Is(cause, jobs.ErrWorkerShutdown) || errors.Is(cause, jobs.ErrOwnershipLost) {
		return cause
	} else if errors.Is(cause, jobs.ErrUserCancellation) || (job != nil && job.CancelRequested) || (run != nil && run.CancelRequested) {
		return h.cancelAttempt(ctx, job, run, attempt)
	}
	if job == nil || job.WorkerID == nil || job.ClaimToken == nil || run == nil || attempt == nil {
		return jobs.ErrOwnershipLost
	}
	promptTokens, completionTokens, toolCalls := executionCounts(result)
	finalizeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := h.diagnosisStore.FinalizeDiagnosisFailure(finalizeCtx, job.ID, *job.WorkerID, *job.ClaimToken,
		job.ExecutionGeneration, run.ID, attempt.ID, class, code, message, promptTokens, completionTokens, toolCalls)
	if errors.Is(err, jobs.ErrCancellationRequested) {
		return h.cancelAttempt(ctx, job, run, attempt)
	}
	if err != nil {
		return jobs.NewRetryableError("ATOMIC_FAILURE_FINALIZE_FAILED", "failed to atomically finalize diagnosis failure", err)
	}
	return jobs.ErrAlreadyFinalized
}

func (h *DiagnosisJobHandler) finalizeRetryableAttempt(ctx context.Context, job *jobs.AnalysisJob, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt, code, message string, result *ExecutionResult) error {
	if cause := context.Cause(ctx); errors.Is(cause, jobs.ErrWorkerShutdown) || errors.Is(cause, jobs.ErrOwnershipLost) {
		return cause
	} else if errors.Is(cause, jobs.ErrUserCancellation) || (job != nil && job.CancelRequested) || (run != nil && run.CancelRequested) {
		return jobs.ErrCancellationRequested
	}
	if job == nil || job.WorkerID == nil || job.ClaimToken == nil || run == nil || attempt == nil {
		return jobs.ErrOwnershipLost
	}
	promptTokens, completionTokens, toolCalls := executionCounts(result)
	finalizeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.diagnosisStore.FinalizeRetryableAttempt(finalizeCtx, job.ID, *job.WorkerID, *job.ClaimToken,
		job.ExecutionGeneration, run.ID, attempt.ID, code, message, promptTokens, completionTokens, toolCalls)
}

func (h *DiagnosisJobHandler) handleFinalizerFailure(ctx context.Context, job *jobs.AnalysisJob, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt, code string, cause error) error {
	if errors.Is(cause, jobs.ErrAlreadyFinalized) || errors.Is(cause, jobs.ErrOwnershipLost) {
		return cause
	}
	if errors.Is(cause, jobs.ErrCancellationRequested) || errors.Is(context.Cause(ctx), jobs.ErrUserCancellation) {
		return h.cancelAttempt(ctx, job, run, attempt)
	}
	if err := h.finalizeRetryableAttempt(ctx, job, run, attempt, code, cause.Error(), nil); err != nil {
		if errors.Is(err, jobs.ErrCancellationRequested) {
			return h.cancelAttempt(ctx, job, run, attempt)
		}
		return jobs.NewRetryableError(code, "diagnosis finalization failed and the attempt could not be closed", errors.Join(cause, err))
	}
	return jobs.NewRetryableError(code, "diagnosis finalization failed", cause)
}

func executionCounts(result *ExecutionResult) (int, int, int) {
	if result == nil {
		return 0, 0, 0
	}
	return result.PromptTokens, result.CompletionTokens, result.ToolCalls
}
