package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/worker"
)

type oversizedReportExecutor struct{ field string }

func (e oversizedReportExecutor) Execute(context.Context, diagnosis.DiagnosisExecutionSpec, *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	report := &evidence.DiagnosisReportData{ConclusionKind: evidence.ConclusionRootCause, Summary: "summary", RootCause: "cause", Findings: []evidence.Finding{{Title: "title", Reasoning: "reason"}}}
	result := &worker.ExecutionResult{Report: report, StructuredReport: true, RawOutput: "{}"}
	if e.field == "raw" {
		result.RawOutput = strings.Repeat("x", evidence.MaxReportBytes+1)
	} else if e.field == "post-citation" {
		report.Findings = nil
		for i := 0; i < 8; i++ {
			finding := evidence.Finding{Title: "title", Reasoning: "reason"}
			for j := 0; j < 16; j++ {
				finding.Citations = append(finding.Citations, evidence.Citation{FilePath: "foo.go", Excerpt: strings.Repeat("x", 32768), ValidationStatus: evidence.CitationValid})
			}
			report.Findings = append(report.Findings, finding)
		}
		encoded, _ := json.Marshal(report)
		excess := len(encoded) - evidence.MaxReportBytes
		last := &report.Findings[7].Citations[15]
		last.Excerpt = last.Excerpt[:len(last.Excerpt)-excess]
	} else {
		for i := 0; i < 16; i++ {
			report.Findings[0].Citations = append(report.Findings[0].Citations, evidence.Citation{FilePath: "foo.go", Excerpt: strings.Repeat("\x00", 32768)})
		}
		report.Findings = append(report.Findings, report.Findings[0])
	}
	return result, nil
}

func TestOversizedReportFailsPermanentlyBeforeDatabasePayloadWrite(t *testing.T) {
	for _, field := range []string{"raw", "resolved-report", "post-citation"} {
		t.Run(field, func(t *testing.T) {
			db, jobStore := setupTestEnvironment(t)
			ctx := context.Background()
			store := diagnosis.NewStore(db)
			run := &diagnosis.DiagnosisRun{ID: "capacity-run", UserID: "user", RepositoryID: "repo", SnapshotID: "snapshot", IssueTitle: "oversized report", IdempotencyKey: "capacity-key", IdempotencyRequestHash: "capacity-hash"}
			if err := store.Create(ctx, run); err != nil {
				t.Fatal(err)
			}
			claimed, err := jobStore.ClaimJobs(ctx, "capacity-worker", 1, time.Minute)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim: %+v %v", claimed, err)
			}
			startClaimedJob(t, jobStore, claimed[0], "capacity-worker")
			handler := worker.NewDiagnosisJobHandler(store, evidence.NewReportStore(db), evidence.NewCitationStore(db), nil, oversizedReportExecutor{field: field})
			if err := handler.Execute(ctx, claimed[0]); !errors.Is(err, jobs.ErrAlreadyFinalized) {
				t.Fatalf("execute: %v", err)
			}
			attempts, err := store.ListAttemptsByRun(ctx, run.ID)
			expectedRaw := ""
			if field == "post-citation" {
				expectedRaw = "{}"
			}
			if err != nil || len(attempts) != 1 || attempts[0].ErrorCode != evidence.ReportTooLargeCode || attempts[0].Status != diagnosis.AttemptStatusFailedTerminal || attempts[0].RawOutput != expectedRaw {
				t.Fatalf("attempt=%+v err=%v", attempts, err)
			}
			job, err := jobStore.GetJobByID(ctx, claimed[0].ID)
			if err != nil || job.Status != jobs.StatusFailed || job.TerminalReason == nil || *job.TerminalReason != jobs.TerminalReasonPermanent || job.LastErrorCode == nil || *job.LastErrorCode != evidence.ReportTooLargeCode {
				t.Fatalf("job=%+v err=%v", job, err)
			}
			var reportCount int64
			if err := db.Model(&evidence.Report{}).Count(&reportCount).Error; err != nil || reportCount != 0 {
				t.Fatalf("oversized report persisted: count=%d err=%v", reportCount, err)
			}
		})
	}
}
