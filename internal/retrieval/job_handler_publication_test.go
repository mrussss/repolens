package retrieval

import (
	"context"
	"errors"
	"testing"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/retrieval/bm25"
)

func TestRetrievalHandlerDoesNotMarkBuildReadyAfterPublicationFailure(t *testing.T) {
	publicationErr := errors.New("index sync failed")
	publisher := &failingRetrievalArtifactPublisher{err: publicationErr}
	store := &publicationFailureStore{
		retrievalBuild: &codeintelmodel.RetrievalBuild{ID: 7, CodeIndexBuildID: 11, Strategy: "BM25", Status: codeintelmodel.BuildStatusCreated},
		codeIndexBuild: &codeintelmodel.CodeIndexBuild{ID: 11, Status: codeintelmodel.BuildStatusReady},
		symbols:        []*codeintelmodel.Symbol{{Name: "Handle", FilePath: "main.go", StartLine: 1, EndLine: 1}},
	}
	handler := NewRetrievalJobHandler(store, t.TempDir())
	handler.publisher = publisher
	workerID, claimToken := "publication-worker", "publication-claim"
	job := &jobs.AnalysisJob{
		ID: 91, JobType: jobs.JobTypeBuildRetrieval, ResourceID: "7", ExecutionGeneration: 1,
		WorkerID: &workerID, ClaimToken: &claimToken,
	}

	err := handler.Execute(context.Background(), job)
	if !errors.Is(err, publicationErr) {
		t.Fatalf("Execute error = %v, want publication error %v", err, publicationErr)
	}
	if !publisher.called {
		t.Fatal("retrieval artifact publisher was not called")
	}
	if store.finalizeCalls != 0 || store.retrievalBuild.Status != codeintelmodel.BuildStatusBuilding {
		t.Fatalf("failed publication reached READY finalizer: finalize calls=%d build status=%s", store.finalizeCalls, store.retrievalBuild.Status)
	}
}

type failingRetrievalArtifactPublisher struct {
	err    error
	called bool
}

func (p *failingRetrievalArtifactPublisher) Publish(int64, int64, string, string, *bm25.Index) (string, string, error) {
	p.called = true
	return "", "", p.err
}

type publicationFailureStore struct {
	codeintelstore.Store
	retrievalBuild *codeintelmodel.RetrievalBuild
	codeIndexBuild *codeintelmodel.CodeIndexBuild
	symbols        []*codeintelmodel.Symbol
	finalizeCalls  int
}

func (s *publicationFailureStore) GetRetrievalBuildByID(context.Context, int64) (*codeintelmodel.RetrievalBuild, error) {
	return s.retrievalBuild, nil
}

func (s *publicationFailureStore) GetByID(context.Context, int64) (*codeintelmodel.CodeIndexBuild, error) {
	return s.codeIndexBuild, nil
}

func (s *publicationFailureStore) MarkRetrievalBuilding(context.Context, int64) error {
	s.retrievalBuild.Status = codeintelmodel.BuildStatusBuilding
	return nil
}

func (s *publicationFailureStore) ListAllSymbols(context.Context, int64) ([]*codeintelmodel.Symbol, error) {
	return s.symbols, nil
}

func (s *publicationFailureStore) FinalizeRetrievalSuccess(context.Context, int64, string, string, int64, string, string, int) error {
	s.finalizeCalls++
	s.retrievalBuild.Status = codeintelmodel.BuildStatusReady
	return nil
}
