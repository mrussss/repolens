package diagnosis

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"repolens/internal/analysispipeline"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	platformconfig "repolens/internal/platform/config"
	"repolens/internal/platform/metrics"
	"repolens/internal/platform/redaction"
	"repolens/internal/repo"
	"repolens/internal/revision"
)

var ErrInputTooLarge = errors.New("diagnosis input exceeds configured limit")
var ErrRevisionNotReady = analysispipeline.ErrRevisionNotReady

const (
	MaxIssueDescriptionBytes = 64 * 1024
	MaxErrorLogBytes         = 256 * 1024
)

func ValidateInput(input CreateDiagnosisInput) error {
	if len(input.IssueTitle) == 0 || len(input.IssueTitle) > 255 {
		return fmt.Errorf("issue_title must be between 1 and 255 bytes")
	}
	if len(input.IssueDescription) > MaxIssueDescriptionBytes || len(input.ErrorLog) > MaxErrorLogBytes {
		return ErrInputTooLarge
	}
	if len(input.IdempotencyKey) > 128 {
		return fmt.Errorf("idempotency key exceeds maximum length")
	}
	return nil
}

func RedactSecrets(input string) string {
	return redaction.RedactSecrets(input)
}

type CreateDiagnosisInput struct {
	UserID             string
	RepositoryID       string
	AnalysisRevisionID string
	SnapshotID         string
	IssueTitle         string
	IssueDescription   string
	ErrorLog           string
	IdempotencyKey     string
	CodeIndexBuildID   int64
	RetrievalBuildID   int64
}

type ProviderMetadata struct {
	EndpointFingerprint    string
	ConfigFingerprint      string
	NormalizedBaseURL      string
	ModelName              string
	PromptVersion          string
	AgentVersion           string
	AgentConfigHash        string
	Temperature            float64
	IsConfigured           bool
	IsDemo                 bool
	MaxAgentRounds         int
	MaxToolCalls           int
	MaxSearchCalls         int
	MaxRepeatCalls         int
	MaxEvidencePacketBytes int
	MaxToolResultBytes     int
	FinalizationTurns      int
	MaxOutputTokens        int
	ReasoningEffort        string
	ProviderTimeoutSeconds int
	ProviderRetryAttempts  int
}

type ServiceDependencies struct {
	Store          Store
	RepoStore      repo.Store
	Lineage        *analysispipeline.Resolver
	JobStore       *jobs.Store
	ProviderSource func() ProviderMetadata
}

type Service struct {
	store          Store
	repoStore      repo.Store
	lineage        *analysispipeline.Resolver
	jobStore       *jobs.Store
	providerSource func() ProviderMetadata
}

func NewService(deps ServiceDependencies) *Service {
	return &Service{
		store:          deps.Store,
		repoStore:      deps.RepoStore,
		lineage:        deps.Lineage,
		jobStore:       deps.JobStore,
		providerSource: deps.ProviderSource,
	}
}

func (s *Service) Create(ctx context.Context, input CreateDiagnosisInput) (*DiagnosisRun, bool, error) {
	var run *DiagnosisRun
	var reused bool
	err := s.store.WithProviderConfigLock(ctx, func() error {
		var createErr error
		run, reused, createErr = s.create(ctx, input)
		return createErr
	})
	return run, reused, err
}

// create runs while the store's provider-config identity lock is held, so the
// pinned metadata snapshot and the QUEUED row become visible atomically with
// respect to provider identity changes.
func (s *Service) create(ctx context.Context, input CreateDiagnosisInput) (*DiagnosisRun, bool, error) {
	if input.IdempotencyKey == "" {
		input.IdempotencyKey = uuid.New().String()
	}

	if err := ValidateInput(input); err != nil {
		return nil, false, err
	}
	cleanTitle := RedactSecrets(input.IssueTitle)
	cleanDesc := RedactSecrets(input.IssueDescription)
	cleanLog := RedactSecrets(input.ErrorLog)
	if len(cleanTitle) > 255 || len(cleanDesc) > MaxIssueDescriptionBytes || len(cleanLog) > MaxErrorLogBytes {
		return nil, false, ErrInputTooLarge
	}
	var selectedLineage analysispipeline.ResolvedLineage
	if input.AnalysisRevisionID != "" {
		repositoryID, revErr := s.lineage.RepositoryForRevision(ctx, input.AnalysisRevisionID)
		if revErr != nil {
			return nil, false, revErr
		}
		// Authorize before resolving READY state so another user's revision
		// cannot reveal whether any of its derived artifacts are ready.
		if _, repoErr := s.repoStore.GetByIDAndUser(ctx, repositoryID, input.UserID); repoErr != nil {
			return nil, false, revision.ErrNotFound
		}
		if input.RepositoryID != "" && input.RepositoryID != repositoryID {
			return nil, false, codeintelstore.ErrBuildLineageMismatch
		}
		input.RepositoryID = repositoryID
	}
	reqHash := ComputeRequestHashForRevision(input.AnalysisRevisionID, input.RepositoryID, input.SnapshotID, input.IssueTitle, input.IssueDescription, input.ErrorLog, input.CodeIndexBuildID, input.RetrievalBuildID)

	// Check idempotency
	existing, err := s.store.GetByIdempotencyKey(ctx, input.UserID, input.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if existing.IdempotencyRequestHash != reqHash {
			return nil, false, ErrIdempotencyConflict
		}
		// Return existing
		return existing, false, nil
	}

	if input.AnalysisRevisionID != "" {
		lineage, resolveErr := s.lineage.ResolveReadyLineage(ctx, input.AnalysisRevisionID)
		if resolveErr != nil {
			return nil, false, resolveErr
		}
		if lineage.RepositoryID != input.RepositoryID || (input.SnapshotID != "" && input.SnapshotID != lineage.SnapshotID) || (input.CodeIndexBuildID > 0 && input.CodeIndexBuildID != lineage.CodeIndexBuildID) || (input.RetrievalBuildID > 0 && input.RetrievalBuildID != lineage.RetrievalBuildID) {
			return nil, false, codeintelstore.ErrBuildLineageMismatch
		}
		input.SnapshotID = lineage.SnapshotID
		input.CodeIndexBuildID = lineage.CodeIndexBuildID
		input.RetrievalBuildID = lineage.RetrievalBuildID
		selectedLineage = lineage
	} else if input.CodeIndexBuildID <= 0 || input.RetrievalBuildID <= 0 {
		return nil, false, ErrInvalidBuildSelection
	}

	metadata := ProviderMetadata{}
	if s.providerSource != nil {
		metadata = s.providerSource()
	}
	if s.providerSource != nil && !metadata.IsConfigured {
		return nil, false, ErrProviderNotConfigured
	}

	// Validate repository ownership
	r, err := s.repoStore.GetByIDAndUser(ctx, input.RepositoryID, input.UserID)
	if err != nil || r == nil {
		return nil, false, fmt.Errorf("repository %s not found or access denied", input.RepositoryID)
	}

	if input.AnalysisRevisionID == "" {
		lineage, err := s.lineage.ResolveLegacyReady(ctx, input.RepositoryID, input.SnapshotID, input.CodeIndexBuildID, input.RetrievalBuildID)
		if err != nil {
			return nil, false, err
		}
		selectedLineage = lineage
	}

	if metadata.MaxAgentRounds == 0 {
		metadata.MaxAgentRounds = 8
	}
	if metadata.MaxToolCalls == 0 {
		metadata.MaxToolCalls = 12
	}
	if metadata.MaxSearchCalls == 0 {
		metadata.MaxSearchCalls = 3
	}
	if metadata.MaxRepeatCalls == 0 {
		metadata.MaxRepeatCalls = 2
	}
	if metadata.MaxEvidencePacketBytes == 0 {
		metadata.MaxEvidencePacketBytes = 32 * 1024
	}
	if metadata.MaxToolResultBytes == 0 {
		metadata.MaxToolResultBytes = 32 * 1024
	}
	if metadata.FinalizationTurns == 0 {
		metadata.FinalizationTurns = 1
	}
	if metadata.MaxOutputTokens == 0 {
		metadata.MaxOutputTokens = platformconfig.DefaultMaxOutputTokens
	}
	if metadata.ProviderTimeoutSeconds == 0 {
		metadata.ProviderTimeoutSeconds = platformconfig.DefaultProviderTimeoutSeconds
	}
	if metadata.AgentConfigHash == "" {
		metadata.AgentConfigHash = ComputeAgentConfigHashWithGenerationOptions(
			metadata.MaxAgentRounds,
			metadata.MaxToolCalls,
			metadata.MaxSearchCalls,
			metadata.MaxRepeatCalls,
			metadata.MaxEvidencePacketBytes,
			metadata.MaxToolResultBytes,
			metadata.FinalizationTurns,
			metadata.MaxOutputTokens,
			metadata.ProviderTimeoutSeconds,
			metadata.ProviderRetryAttempts,
			metadata.Temperature,
			metadata.ReasoningEffort,
			"json_object",
		)
	}
	run := &DiagnosisRun{
		ID:                          uuid.New().String(),
		UserID:                      input.UserID,
		RepositoryID:                selectedLineage.RepositoryID,
		AnalysisRevisionID:          selectedLineage.RevisionID,
		SnapshotID:                  selectedLineage.SnapshotID,
		CodeIndexBuildID:            selectedLineage.CodeIndexBuildID,
		RetrievalBuildID:            selectedLineage.RetrievalBuildID,
		IssueTitle:                  cleanTitle,
		IssueDescription:            cleanDesc,
		ErrorLog:                    cleanLog,
		Status:                      StatusQueued,
		IdempotencyKey:              input.IdempotencyKey,
		IdempotencyRequestHash:      reqHash,
		Version:                     1,
		ProviderEndpointFingerprint: metadata.EndpointFingerprint,
		ProviderConfigFingerprint:   metadata.ConfigFingerprint,
		NormalizedBaseURL:           metadata.NormalizedBaseURL,
		ModelName:                   metadata.ModelName,
		PromptVersion:               metadata.PromptVersion,
		AgentVersion:                metadata.AgentVersion,
		AgentConfigHash:             metadata.AgentConfigHash,
		MaxAgentRounds:              metadata.MaxAgentRounds,
		MaxToolCalls:                metadata.MaxToolCalls,
		MaxSearchCalls:              metadata.MaxSearchCalls,
		MaxRepeatCalls:              metadata.MaxRepeatCalls,
		MaxEvidencePacketBytes:      metadata.MaxEvidencePacketBytes,
		MaxToolResultBytes:          metadata.MaxToolResultBytes,
		FinalizationTurns:           metadata.FinalizationTurns,
		MaxOutputTokens:             metadata.MaxOutputTokens,
		ReasoningEffort:             metadata.ReasoningEffort,
		ProviderTimeoutSeconds:      metadata.ProviderTimeoutSeconds,
		ProviderRetryAttempts:       metadata.ProviderRetryAttempts,
		Temperature:                 metadata.Temperature,
	}
	if selectedLineage.RevisionID != "" {
		run.PipelineFingerprint = selectedLineage.PipelineFingerprint
	}

	if err := s.store.Create(ctx, run); err != nil {
		return nil, false, fmt.Errorf("failed to create diagnosis run: %w", err)
	}

	metrics.DiagnosisTotal.Inc()
	return run, true, nil
}

func (s *Service) Get(ctx context.Context, id, userID string) (*DiagnosisRun, error) {
	run, err := s.store.GetByIDAndUser(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	if run.Status == StatusFailed {
		run.RetryReason = "当前失败不属于可安全重试的 Provider 临时故障"
	}
	if s.jobStore != nil {
		if job, jobErr := s.jobStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, id); jobErr == nil {
			run.ExecutionGeneration = job.ExecutionGeneration
			if run.Status == StatusFailed && job.Status == jobs.StatusFailed && job.LastErrorCode != nil {
				run.RetryErrorCode = *job.LastErrorCode
				class := jobs.ErrorClass("")
				if job.LastErrorClass != nil {
					class = jobs.ErrorClass(*job.LastErrorClass)
				}
				run.RetryAllowed = jobs.IsRetryableDiagnosisProviderFailure(class, *job.LastErrorCode)
				if run.RetryAllowed {
					run.RetryReason = ""
				}
			}
		}
	}
	return run, nil
}

func (s *Service) List(ctx context.Context, userID string, page, pageSize int) ([]DiagnosisRun, int64, error) {
	return s.store.ListByUser(ctx, userID, page, pageSize)
}

func (s *Service) Cancel(ctx context.Context, id, userID string) error {
	return s.store.RequestCancellation(ctx, id, userID)
}

func (s *Service) Retry(ctx context.Context, id, userID string) error {
	if s.jobStore == nil {
		return errors.New("diagnosis retry is not configured")
	}
	return s.store.WithProviderConfigLock(ctx, func() error {
		run, err := s.Get(ctx, id, userID)
		if err != nil {
			return err
		}
		if s.providerSource != nil {
			metadata := s.providerSource()
			if !metadata.IsConfigured {
				return ErrProviderNotConfigured
			}
			if run.ProviderConfigFingerprint == "" || run.ProviderConfigFingerprint != metadata.ConfigFingerprint {
				return ErrProviderIdentityChanged
			}
		}
		return s.jobStore.RetryDiagnosis(ctx, id)
	})
}

func (s *Service) ListAttempts(ctx context.Context, runID, userID string) ([]DiagnosisAttempt, error) {
	if _, err := s.Get(ctx, runID, userID); err != nil {
		return nil, err
	}
	return s.store.ListAttemptsByRun(ctx, runID)
}

// GetAttemptForRun authorizes the diagnosis before exposing an attempt or any
// trace data attached to it. An attempt ID alone is not a sufficient security
// boundary because trace rows are keyed by attempt rather than user.
func (s *Service) GetAttemptForRun(ctx context.Context, runID, userID, attemptID string) (*DiagnosisAttempt, error) {
	if _, err := s.Get(ctx, runID, userID); err != nil {
		return nil, err
	}
	attempt, err := s.store.GetAttempt(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	if attempt.DiagnosisRunID != runID {
		return nil, ErrAttemptNotFound
	}
	return attempt, nil
}
