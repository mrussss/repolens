package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"repolens/internal/agent"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/llm"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
)

type Runner struct {
	storeFS snapshotstore.SnapshotStore
	cases   []EvalCase
}

func NewRunner(storeFS snapshotstore.SnapshotStore) *Runner {
	return &Runner{
		storeFS: storeFS,
		cases:   make([]EvalCase, 0),
	}
}

func (r *Runner) LoadCasesFromDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			var c EvalCase
			if err := json.Unmarshal(data, &c); err == nil && c.CaseID != "" {
				r.cases = append(r.cases, c)
			}
		}
	}
	return nil
}

func (r *Runner) AddCase(c EvalCase) {
	r.cases = append(r.cases, c)
}

func (r *Runner) RunRetrievalEval(ctx context.Context, strategy string, retriever retrieval.Retriever, chunkStore retrieval.ChunkIndexStore) (*EvalRun, error) {
	run := &EvalRun{
		ID:                uuid.New().String(),
		DatasetVersion:    "v1.0",
		GitCommit:         "local-freeze",
		SnapshotSHA:       "snapshot-base",
		RetrievalStrategy: strategy,
		RetrievalVersion:  "1.1",
		IndexVersion:      "v1",
		PromptVersion:     "v1.1",
		AgentVersion:      "v1.1",
		Model:             "fake-deterministic",
		EmbeddingModel:    "pseudo-embed-128",
		TotalCases:        len(r.cases),
		StartedAt:         time.Now(),
	}

	var results []CaseEvalResult

	for _, c := range r.cases {
		query := c.IssueTitle + " " + c.ErrorLog
		start := time.Now()

		searchRes, err := retriever.Search(ctx, retrieval.SearchRequest{
			SnapshotID: c.SnapshotSHA,
			Query:      query,
			TopK:       10,
		})
		latency := time.Since(start).Milliseconds()

		if err != nil {
			continue
		}

		hit5, hit10, rr := CalculateRetrievalMetrics(searchRes, c.RelevantFiles)

		results = append(results, CaseEvalResult{
			CaseID:         c.CaseID,
			HitAt5:         hit5,
			HitAt10:        hit10,
			ReciprocalRank: rr,
			LatencyMs:      latency,
		})
	}

	run.FinishedAt = time.Now()
	run.Metrics = AggregateMetrics(results)
	return run, nil
}

func (r *Runner) RunEndToEndDiagnosisEval(ctx context.Context, provider llm.Provider, retriever retrieval.Retriever) (*EvalRun, error) {
	run := &EvalRun{
		ID:                uuid.New().String(),
		DatasetVersion:    "v1.0",
		GitCommit:         "local-freeze",
		RetrievalStrategy: "E2E_AGENT",
		RetrievalVersion:  "1.1",
		IndexVersion:      "v1",
		PromptVersion:     "v1.1",
		AgentVersion:      "v1.1",
		Model:             "agent-e2e-pipeline",
		TotalCases:        len(r.cases),
		StartedAt:         time.Now(),
	}

	var results []CaseEvalResult
	validator := evidence.NewCitationValidator(r.storeFS)
	executor := agent.NewAgentRuntimeExecutor(
		provider,
		retriever,
		r.storeFS,
		nil,
		agent.DefaultGuardConfig(),
	)

	for _, c := range r.cases {
		start := time.Now()

		// 1. Evaluate Retrieval on the case query against ground-truth relevant files
		query := c.IssueTitle + " " + c.ErrorLog
		searchRes, _ := retriever.Search(ctx, retrieval.SearchRequest{
			SnapshotID: c.SnapshotSHA,
			Query:      query,
			TopK:       10,
		})
		hit5, hit10, rr := CalculateRetrievalMetrics(searchRes, c.RelevantFiles)

		// 2. Execute full Agent Loop with Tool Calling
		diagRun := &diagnosis.DiagnosisRun{
			ID:               uuid.New().String(),
			RepositoryID:     c.RepositoryName,
			SnapshotID:       c.SnapshotSHA,
			IssueTitle:       c.IssueTitle,
			IssueDescription: c.IssueDescription,
			ErrorLog:         c.ErrorLog,
			Status:           diagnosis.StatusRunning,
		}
		attempt := &diagnosis.DiagnosisAttempt{
			ID:             uuid.New().String(),
			DiagnosisRunID: diagRun.ID,
			AttemptNo:      1,
			Status:         diagnosis.AttemptStatusRunning,
		}

		execRes, err := executor.Execute(ctx, diagRun, attempt)
		latency := time.Since(start).Milliseconds()

		if err != nil || execRes == nil || execRes.Report == nil {
			promptTokens := 0
			compTokens := 0
			if execRes != nil {
				promptTokens = execRes.PromptTokens
				compTokens = execRes.CompletionTokens
			}
			results = append(results, CaseEvalResult{
				CaseID:           c.CaseID,
				HitAt5:           hit5,
				HitAt10:          hit10,
				ReciprocalRank:   rr,
				CitationsTotal:   0,
				CitationsValid:   0,
				RootCauseSuccess: false,
				ForbiddenClaim:   false,
				LatencyMs:        latency,
				PromptTokens:     promptTokens,
				CompletionTokens: compTokens,
			})
			continue
		}

		report := execRes.Report
		rcSuccess, forb := CalculateCaseDiagnosis(report, c)

		citTotal := 0
		citValid := 0
		for _, f := range report.Findings {
			for _, cit := range f.Citations {
				citTotal++
				cit.SnapshotID = c.SnapshotSHA
				validator.Validate(ctx, c.RepositoryName, c.SnapshotSHA, &cit)
				if cit.ValidationStatus == evidence.CitationValid {
					citValid++
				}
			}
		}

		results = append(results, CaseEvalResult{
			CaseID:           c.CaseID,
			HitAt5:           hit5,
			HitAt10:          hit10,
			ReciprocalRank:   rr,
			CitationsTotal:   citTotal,
			CitationsValid:   citValid,
			RootCauseSuccess: rcSuccess,
			ForbiddenClaim:   forb,
			LatencyMs:        latency,
			PromptTokens:     execRes.PromptTokens,
			CompletionTokens: execRes.CompletionTokens,
		})
	}

	run.FinishedAt = time.Now()
	run.Metrics = AggregateMetrics(results)
	return run, nil
}

func PrintComparisonTable(runs []*EvalRun) {
	fmt.Println("\n=========================================================================================================")
	fmt.Printf("%-15s | %-8s | %-8s | %-8s | %-12s | %-12s | %-8s | %-8s\n",
		"Retrieval", "Hit@5", "Hit@10", "MRR", "Cit. Valid", "Root Cause", "P50(ms)", "P95(ms)")
	fmt.Println("---------------------------------------------------------------------------------------------------------")
	for _, r := range runs {
		fmt.Printf("%-15s | %7.1f%% | %7.1f%% | %8.3f | %11.1f%% | %11.1f%% | %7d | %7d\n",
			r.RetrievalStrategy,
			r.Metrics.FileHitAt5*100,
			r.Metrics.FileHitAt10*100,
			r.Metrics.MRR,
			r.Metrics.CitationValidityRate*100,
			r.Metrics.RootCauseSuccessRate*100,
			r.Metrics.P50LatencyMs,
			r.Metrics.P95LatencyMs,
		)
	}
	fmt.Println("=========================================================================================================")
}
