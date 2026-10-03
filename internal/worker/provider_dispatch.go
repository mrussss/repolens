package worker

import (
	"context"

	"repolens/internal/diagnosis"
)

type diagnosisProviderDispatchGuard struct {
	store      diagnosis.Store
	jobID      int64
	workerID   string
	claimToken string
	generation int
	runID      string
	attemptID  string
	attemptNo  int
}

func (g diagnosisProviderDispatchGuard) BeginProviderDispatch(ctx context.Context) error {
	if err := g.store.BeginProviderDispatch(ctx, g.jobID, g.workerID, g.claimToken, g.generation, g.runID, g.attemptID, g.attemptNo); err != nil {
		return err
	}
	return g.store.CheckProviderDispatchAuthority(ctx, g.jobID, g.workerID, g.claimToken, g.generation, g.runID, g.attemptID, g.attemptNo)
}

func (g diagnosisProviderDispatchGuard) ProviderDispatchSucceeded(ctx context.Context) error {
	return g.store.ProviderDispatchSucceeded(ctx, g.jobID, g.workerID, g.claimToken, g.generation, g.runID, g.attemptID, g.attemptNo)
}

func (g diagnosisProviderDispatchGuard) ProviderDispatchFailedDefinitely(ctx context.Context) (bool, error) {
	return g.store.ProviderDispatchFailedDefinitely(ctx, g.jobID, g.workerID, g.claimToken, g.generation, g.runID, g.attemptID, g.attemptNo)
}
