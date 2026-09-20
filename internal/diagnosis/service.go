package diagnosis

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"gorm.io/gorm"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/metrics"
	"repolens/internal/repo"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

var (
	secretPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|auth|bearer)\s*[:=]\s*['"]?([a-zA-Z0-9_\-\.]{8,})['"]?`),
		regexp.MustCompile(`(?i)ghp_[a-zA-Z0-9]{36}`),
		regexp.MustCompile(`(?i)glpat-[a-zA-Z0-9\-_]{20}`),
		regexp.MustCompile(`(?i)sk-[a-zA-Z0-9]{32,}`),
		regexp.MustCompile(`(?i)AKIA[0-9A-Z]{16}`),
	}
)

var ErrInputTooLarge = errors.New("diagnosis input exceeds configured limit")
var ErrRevisionNotReady = errors.New("analysis revision is not ready")

func ValidateInput(input CreateDiagnosisInput) error {
	if len(input.IssueTitle) == 0 || len(input.IssueTitle) > 255 {
		return fmt.Errorf("issue_title must be between 1 and 255 bytes")
	}
	if len(input.IssueDescription) > 64*1024 || len(input.ErrorLog) > 256*1024 {
		return ErrInputTooLarge
	}
	if len(input.IdempotencyKey) > 128 {
		return fmt.Errorf("idempotency key exceeds maximum length")
	}
	return nil
}

func revisionPipelineFingerprint(revisionID string) string {
	if revisionID == "" {
		return ""
	}
	return revision.ComputePipelineFingerprint()
}

func RedactSecrets(input string) string {
	redacted := input
	for _, p := range secretPatterns {
		redacted = p.ReplaceAllStringFunc(redacted, func(match string) string {
			return "[REDACTED_SECRET]"
		})
	}
	return redacted
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
	FinalizationTurns      int
	MaxOutputTokens        int
	ProviderTimeoutSeconds int
	ProviderRetryAttempts  int
}

type Service struct {
	store                  Store
	repoStore              repo.Store
	snapshotStore          snapshot.Store
	codeIntelStore         codeintelstore.Store
	revisionStore          revision.Store
	jobStore               *jobs.Store
	providerMetadata       ProviderMetadata
	providerMetadataSet    bool
	providerMetadataSource func() ProviderMetadata
}

func (s *Service) WithCodeIntelStore(store codeintelstore.Store) *Service {
	s.codeIntelStore = store
	return s
}

func (s *Service) WithRevisionStore(store revision.Store) *Service {
	s.revisionStore = store
	return s
}

func (s *Service) WithJobStore(store *jobs.Store) *Service {
	s.jobStore = store
	return s
}

func (s *Service) WithProviderMetadata(metadata ProviderMetadata) *Service {
	s.providerMetadata = metadata
	s.providerMetadataSet = true
	return s
}

func (s *Service) WithProviderMetadataSource(source func() ProviderMetadata) *Service {
	s.providerMetadataSource = source
	return s
}

func NewService(store Store, repoStore repo.Store, snapshotStore snapshot.Store) *Service {
	return &Service{
		store:         store,
		repoStore:     repoStore,
		snapshotStore: snapshotStore,
	}
}

func (s *Service) Create(ctx context.Context, input CreateDiagnosisInput) (*DiagnosisRun, bool, error) {
	if input.IdempotencyKey == "" {
		input.IdempotencyKey = uuid.New().String()
	}

	if err := ValidateInput(input); err != nil {
		return nil, false, err
	}
	if input.AnalysisRevisionID != "" {
		if s.revisionStore == nil {
			return nil, false, fmt.Errorf("analysis revision store is not configured")
		}
		rev, revErr := s.revisionStore.GetByID(ctx, input.AnalysisRevisionID)
		if revErr != nil {
			return nil, false, revErr
		}
		// A revision lookup by ID is not an authorization check. Resolve its
		// repository through the user-scoped store before using its lineage.
		if _, repoErr := s.repoStore.GetByIDAndUser(ctx, rev.RepositoryID, input.UserID); repoErr != nil {
			return nil, false, revision.ErrNotFound
		}
		if input.RepositoryID != "" && input.RepositoryID != rev.RepositoryID {
			return nil, false, codeintelstore.ErrBuildLineageMismatch
		}
		input.RepositoryID = rev.RepositoryID
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
		if s.revisionStore == nil {
			return nil, false, fmt.Errorf("analysis revision store is not configured")
		}
		rev, revErr := s.revisionStore.GetByIDAndRepository(ctx, input.AnalysisRevisionID, input.RepositoryID)
		if revErr != nil {
			return nil, false, revErr
		}
		if rev.Status != revision.StatusReady {
			return nil, false, ErrRevisionNotReady
		}
		if (input.SnapshotID != "" && input.SnapshotID != rev.SnapshotID) || (input.CodeIndexBuildID > 0 && input.CodeIndexBuildID != rev.CodeIndexBuildID) || (input.RetrievalBuildID > 0 && input.RetrievalBuildID != rev.RetrievalBuildID) {
			return nil, false, codeintelstore.ErrBuildLineageMismatch
		}
		input.SnapshotID = rev.SnapshotID
		input.CodeIndexBuildID = rev.CodeIndexBuildID
		input.RetrievalBuildID = rev.RetrievalBuildID
	} else if input.CodeIndexBuildID <= 0 || input.RetrievalBuildID <= 0 {
		return nil, false, ErrInvalidBuildSelection
	}

	metadata := s.providerMetadata
	if s.providerMetadataSource != nil {
		metadata = s.providerMetadataSource()
	}
	if (s.providerMetadataSet || s.providerMetadataSource != nil) && !metadata.IsConfigured {
		return nil, false, ErrProviderNotConfigured
	}

	// Validate repository ownership
	r, err := s.repoStore.GetByIDAndUser(ctx, input.RepositoryID, input.UserID)
	if err != nil || r == nil {
		return nil, false, fmt.Errorf("repository %s not found or access denied", input.RepositoryID)
	}

	// Validate snapshot
	snap, err := s.snapshotStore.GetByID(ctx, input.SnapshotID)
	if err != nil || snap == nil {
		return nil, false, fmt.Errorf("snapshot %s not found", input.SnapshotID)
	}
	if snap.Status != snapshot.StatusReady {
		return nil, false, fmt.Errorf("snapshot %s is not READY (current status: %s)", input.SnapshotID, snap.Status)
	}

	codeIndexBuildID := input.CodeIndexBuildID
	retrievalBuildID := input.RetrievalBuildID
	if s.codeIntelStore != nil {
		if err := s.codeIntelStore.ValidateLineage(ctx, input.RepositoryID, input.SnapshotID, codeIndexBuildID, retrievalBuildID); err != nil {
			return nil, false, err
		}
		cib, err := s.codeIntelStore.GetByID(ctx, codeIndexBuildID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, false, fmt.Errorf("%w: code index build %d", ErrBuildNotReady, codeIndexBuildID)
			}
			return nil, false, fmt.Errorf("failed to load code index build %d: %w", codeIndexBuildID, err)
		}
		if cib == nil {
			return nil, false, fmt.Errorf("%w: code index build %d", ErrBuildNotReady, codeIndexBuildID)
		}
		if cib.Status != codeintelmodel.BuildStatusReady {
			return nil, false, fmt.Errorf("%w: code index build %d is %s", ErrBuildNotReady, codeIndexBuildID, cib.Status)
		}
		rb, err := s.codeIntelStore.GetRetrievalBuildByID(ctx, retrievalBuildID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, false, fmt.Errorf("%w: retrieval build %d", ErrBuildNotReady, retrievalBuildID)
			}
			return nil, false, fmt.Errorf("failed to load retrieval build %d: %w", retrievalBuildID, err)
		}
		if rb == nil {
			return nil, false, fmt.Errorf("%w: retrieval build %d", ErrBuildNotReady, retrievalBuildID)
		}
		if rb.Status != codeintelmodel.BuildStatusReady {
			return nil, false, fmt.Errorf("%w: retrieval build %d is %s", ErrBuildNotReady, retrievalBuildID, rb.Status)
		}
	}

	cleanDesc := RedactSecrets(input.IssueDescription)
	cleanLog := RedactSecrets(input.ErrorLog)

	if metadata.AgentConfigHash == "" {
		metadata.AgentConfigHash = ComputeAgentConfigHashWithRuntime(8, 12, 3, 2, 32*1024, 1, 2048, 60, 0, metadata.Temperature)
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
	if metadata.FinalizationTurns == 0 {
		metadata.FinalizationTurns = 1
	}
	if metadata.MaxOutputTokens == 0 {
		metadata.MaxOutputTokens = 2048
	}
	if metadata.ProviderTimeoutSeconds == 0 {
		metadata.ProviderTimeoutSeconds = 60
	}
	run := &DiagnosisRun{
		ID:                          uuid.New().String(),
		UserID:                      input.UserID,
		RepositoryID:                input.RepositoryID,
		AnalysisRevisionID:          input.AnalysisRevisionID,
		SnapshotID:                  input.SnapshotID,
		CodeIndexBuildID:            codeIndexBuildID,
		RetrievalBuildID:            retrievalBuildID,
		IssueTitle:                  input.IssueTitle,
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
		FinalizationTurns:           metadata.FinalizationTurns,
		MaxOutputTokens:             metadata.MaxOutputTokens,
		ProviderTimeoutSeconds:      metadata.ProviderTimeoutSeconds,
		ProviderRetryAttempts:       metadata.ProviderRetryAttempts,
		PipelineFingerprint:         revisionPipelineFingerprint(input.AnalysisRevisionID),
		Temperature:                 metadata.Temperature,
	}

	if err := s.store.Create(ctx, run); err != nil {
		return nil, false, fmt.Errorf("failed to create diagnosis run: %w", err)
	}

	metrics.DiagnosisTotal.Inc()
	return run, true, nil
}

func (s *Service) Get(ctx context.Context, id, userID string) (*DiagnosisRun, error) {
	return s.store.GetByIDAndUser(ctx, id, userID)
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
	if _, err := s.Get(ctx, id, userID); err != nil {
		return err
	}
	return s.jobStore.RetryDiagnosis(ctx, id)
}

func (s *Service) ListAttempts(ctx context.Context, runID string) ([]DiagnosisAttempt, error) {
	return s.store.ListAttemptsByRun(ctx, runID)
}
