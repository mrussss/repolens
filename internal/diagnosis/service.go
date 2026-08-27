package diagnosis

import (
	"context"
	"fmt"
	"regexp"

	"github.com/google/uuid"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/platform/metrics"
	"repolens/internal/repo"
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
	UserID           string
	RepositoryID     string
	SnapshotID       string
	IssueTitle       string
	IssueDescription string
	ErrorLog         string
	IdempotencyKey   string
	CodeIndexBuildID int64
	RetrievalBuildID int64
}

type ProviderMetadata struct {
	EndpointFingerprint string
	ConfigFingerprint   string
	NormalizedBaseURL   string
	ModelName           string
	PromptVersion       string
	AgentVersion        string
	AgentConfigHash     string
	Temperature         float64
}

type Service struct {
	store                  Store
	repoStore              repo.Store
	snapshotStore          snapshot.Store
	codeIntelStore         codeintelstore.Store
	providerMetadata       ProviderMetadata
	providerMetadataSource func() ProviderMetadata
}

func (s *Service) WithCodeIntelStore(store codeintelstore.Store) *Service {
	s.codeIntelStore = store
	return s
}

func (s *Service) WithProviderMetadata(metadata ProviderMetadata) *Service {
	s.providerMetadata = metadata
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

	reqHash := ComputeRequestHash(input.RepositoryID, input.SnapshotID, input.IssueTitle, input.IssueDescription, input.ErrorLog, input.CodeIndexBuildID, input.RetrievalBuildID)

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
		if codeIndexBuildID == 0 {
			build, err := s.codeIntelStore.GetBySnapshot(ctx, input.SnapshotID)
			if err != nil {
				return nil, false, fmt.Errorf("code index build for snapshot %s is not ready: %w", input.SnapshotID, err)
			}
			codeIndexBuildID = build.ID
		}
		if retrievalBuildID == 0 {
			rb, err := s.codeIntelStore.GetRetrievalBuildByCodeIndexBuild(ctx, codeIndexBuildID)
			if err != nil {
				return nil, false, fmt.Errorf("retrieval build for code index build %d is not ready: %w", codeIndexBuildID, err)
			}
			retrievalBuildID = rb.ID
		}
		if err := s.codeIntelStore.ValidateLineage(ctx, input.RepositoryID, input.SnapshotID, codeIndexBuildID, retrievalBuildID); err != nil {
			return nil, false, err
		}
		cib, err := s.codeIntelStore.GetByID(ctx, codeIndexBuildID)
		if err != nil || cib.Status != codeintelmodel.BuildStatusReady {
			return nil, false, fmt.Errorf("code index build %d is not READY", codeIndexBuildID)
		}
		rb, err := s.codeIntelStore.GetRetrievalBuildByID(ctx, retrievalBuildID)
		if err != nil || rb.Status != codeintelmodel.BuildStatusReady {
			return nil, false, fmt.Errorf("retrieval build %d is not READY", retrievalBuildID)
		}
	} else {
		// Isolated unit fixtures may omit derived builds. Production wires the
		// CodeIntelStore and always takes the pinned path above.
		codeIndexBuildID = 0
		retrievalBuildID = 0
	}

	cleanDesc := RedactSecrets(input.IssueDescription)
	cleanLog := RedactSecrets(input.ErrorLog)

	metadata := s.providerMetadata
	if s.providerMetadataSource != nil {
		metadata = s.providerMetadataSource()
	}
	if metadata.AgentConfigHash == "" {
		metadata.AgentConfigHash = ComputeAgentConfigHash(8, 12, 2, metadata.Temperature)
	}
	run := &DiagnosisRun{
		ID:                          uuid.New().String(),
		UserID:                      input.UserID,
		RepositoryID:                input.RepositoryID,
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

func (s *Service) ListAttempts(ctx context.Context, runID string) ([]DiagnosisAttempt, error) {
	return s.store.ListAttemptsByRun(ctx, runID)
}
