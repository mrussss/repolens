package diagnosis

import (
	"context"
	"fmt"
	"regexp"

	"github.com/google/uuid"

	"repolens/internal/outbox"
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
}

type Service struct {
	store         Store
	repoStore     repo.Store
	snapshotStore snapshot.Store
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

	reqHash := ComputeRequestHash(input.RepositoryID, input.SnapshotID, input.IssueTitle, input.IssueDescription, input.ErrorLog)

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

	// Secret redaction
	cleanDesc := RedactSecrets(input.IssueDescription)
	cleanLog := RedactSecrets(input.ErrorLog)

	run := &DiagnosisRun{
		ID:                     uuid.New().String(),
		UserID:                 input.UserID,
		RepositoryID:           input.RepositoryID,
		SnapshotID:             input.SnapshotID,
		IssueTitle:             input.IssueTitle,
		IssueDescription:       cleanDesc,
		ErrorLog:               cleanLog,
		Status:                 StatusQueued,
		IdempotencyKey:         input.IdempotencyKey,
		IdempotencyRequestHash: reqHash,
		Version:                1,
	}

	outboxEvt := &outbox.OutboxEvent{}
	if err := s.store.CreateWithOutbox(ctx, run, outboxEvt); err != nil {
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
