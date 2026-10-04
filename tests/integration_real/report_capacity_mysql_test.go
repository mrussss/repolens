package integration_real

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/platform/mysql"
)

func TestRealMySQL_ReportCapacityMigrationAndByteBoundaries(t *testing.T) {
	all := filepath.Join("..", "..", "migrations")
	before := t.TempDir()
	entries, err := os.ReadDir(all)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= "018_" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(all, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(before, entry.Name()), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, _, cleanup := setupRealMySQL(t, before)
	defer cleanup()
	ctx := context.Background()
	old := &evidence.Report{ID: "old-report", DiagnosisRunID: "old-run", AttemptID: "old-attempt", RootCause: "preserved", FindingsJSON: "[]"}
	if err := evidence.NewReportStore(db).Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	migrationDB := &mysql.DB{GormDB: db, SqlDB: sqlDB}
	// Resume after the first widening DDL committed without a migration record.
	if err := db.Exec("ALTER TABLE reports MODIFY COLUMN findings_json MEDIUMTEXT NOT NULL").Error; err != nil {
		t.Fatal(err)
	}
	if err := mysql.ApplyMigrations(migrationDB, all); err != nil {
		t.Fatal(err)
	}
	if err := mysql.ApplyMigrations(migrationDB, all); err != nil {
		t.Fatalf("repeat migrations: %v", err)
	}
	for _, col := range []string{"findings_json", "recommended_checks_json", "limitations_json", "structured_payload_json", "raw_output"} {
		var kind string
		if err := db.Raw("SELECT DATA_TYPE FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='reports' AND column_name=?", col).Scan(&kind).Error; err != nil || kind != "mediumtext" {
			t.Fatalf("%s=%s err=%v", col, kind, err)
		}
	}
	for _, col := range []string{"parsed_report_json", "parsed_report_draft_json", "raw_output"} {
		var kind string
		if err := db.Raw("SELECT DATA_TYPE FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='diagnosis_attempts' AND column_name=?", col).Scan(&kind).Error; err != nil || kind != "mediumtext" {
			t.Fatalf("attempt %s=%s err=%v", col, kind, err)
		}
	}
	saved, err := evidence.NewReportStore(db).GetByAttemptID(ctx, old.AttemptID)
	if err != nil || saved.RootCause != old.RootCause {
		t.Fatalf("upgrade lost existing report: %+v %v", saved, err)
	}
	boundary := &evidence.Report{ID: "boundary-report", DiagnosisRunID: "boundary-run", AttemptID: "boundary-attempt", FindingsJSON: "[]", RawOutput: strings.Repeat("x", evidence.MaxReportBytes)}
	if err := evidence.NewReportStore(db).Create(ctx, boundary); err != nil {
		t.Fatalf("boundary save: %v", err)
	}
	saved, err = evidence.NewReportStore(db).GetByAttemptID(ctx, boundary.AttemptID)
	if err != nil || len(saved.RawOutput) != evidence.MaxReportBytes {
		t.Fatalf("boundary round trip: %v", err)
	}
	// An INSERT trigger distinguishes pre-write validation from a MySQL rejection.
	if err := db.Exec(`CREATE TRIGGER reject_report_write BEFORE INSERT ON reports FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='unexpected DB write'`).Error; err != nil {
		t.Fatal(err)
	}
	boundary.ID = "oversized-report"
	boundary.RawOutput += "x"
	err = evidence.NewReportStore(db).Create(ctx, boundary)
	if !errors.Is(err, evidence.ErrReportTooLarge) || !strings.Contains(err.Error(), evidence.ReportTooLargeCode) {
		t.Fatalf("must reject before INSERT with stable code: %v", err)
	}
}

func TestRealMySQL_LargeValidReportAtomicFinalization(t *testing.T) {
	for _, name := range []string{"70KB", "serialized-boundary"} {
		t.Run(name, func(t *testing.T) {
			db, jobStore, cleanup := setupRealMySQL(t)
			defer cleanup()
			ctx := context.Background()
			store := diagnosis.NewStore(db)
			run := f01DiagnosisRun("http://unused.example")
			if err := store.Create(ctx, run); err != nil {
				t.Fatal(err)
			}
			claimed, err := jobStore.ClaimJobs(ctx, "capacity-worker", 1, time.Minute)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim: %+v %v", claimed, err)
			}
			job := claimed[0]
			attemptNo, err := jobStore.MarkExecutionStarted(ctx, job.ID, *job.WorkerID, *job.ClaimToken, job.ExecutionGeneration)
			if err != nil {
				t.Fatal(err)
			}
			attempt := &diagnosis.DiagnosisAttempt{ID: "capacity-attempt", DiagnosisRunID: run.ID, ExecutionGeneration: job.ExecutionGeneration, AttemptNo: attemptNo, WorkerID: *job.WorkerID, Status: diagnosis.AttemptStatusRunning, StartedAt: time.Now().UTC(), HeartbeatAt: time.Now().UTC(), DeadlineAt: time.Now().UTC().Add(time.Minute)}
			if err := store.StartAttemptWithClaim(ctx, job.ID, *job.WorkerID, *job.ClaimToken, job.ExecutionGeneration, run.ID, attempt); err != nil {
				t.Fatal(err)
			}
			data := &evidence.DiagnosisReportData{ConclusionKind: evidence.ConclusionRootCause, Summary: "summary", RootCause: "cause"}
			if name == "70KB" {
				for i := 0; i < 9; i++ {
					data.RecommendedChecks = append(data.RecommendedChecks, strings.Repeat("c", 8192))
					data.Limitations = append(data.Limitations, strings.Repeat("l", 8192))
				}
			}
			count, excerpts := 1, 3
			excerptBytes := 24000
			if name == "serialized-boundary" {
				count, excerpts, excerptBytes = 8, 16, 32768
			}
			for i := 0; i < count; i++ {
				finding := evidence.Finding{Title: "title", Reasoning: "reason"}
				for j := 0; j < excerpts; j++ {
					finding.Citations = append(finding.Citations, evidence.Citation{ReportID: "capacity-report", SnapshotID: run.SnapshotID, FilePath: "foo.go", StartLine: 1, EndLine: 1, Excerpt: strings.Repeat("x", excerptBytes), ValidationStatus: evidence.CitationValid})
				}
				data.Findings = append(data.Findings, finding)
			}
			encoded, _ := json.Marshal(data)
			if name == "serialized-boundary" {
				excess := len(encoded) - evidence.MaxReportBytes
				last := &data.Findings[count-1].Citations[excerpts-1]
				if excess <= 0 || excess >= len(last.Excerpt) {
					t.Fatalf("unexpected excess=%d", excess)
				}
				last.Excerpt = last.Excerpt[:len(last.Excerpt)-excess]
				encoded, _ = json.Marshal(data)
				if len(encoded) != evidence.MaxReportBytes {
					t.Fatalf("boundary=%d", len(encoded))
				}
			}
			quality, err := evidence.ClassifyReport(data, true)
			if err != nil || quality.Status != evidence.ReportValid {
				t.Fatalf("legal report rejected: %+v %v", quality, err)
			}
			findings, _ := json.Marshal(data.Findings)
			if len(findings) <= 65535 {
				t.Fatalf("fixture does not exceed old TEXT limit: %d", len(findings))
			}
			checkpoint := diagnosis.AttemptCheckpoint{ExecutionGeneration: job.ExecutionGeneration, Kind: diagnosis.CheckpointKindFinalValid, Structured: true, RawOutput: string(encoded), ParsedReportJSON: string(encoded), ParsedDraftJSON: `{}`}
			if err := store.UpdateAttemptCheckpointWithClaim(ctx, job.ID, *job.WorkerID, *job.ClaimToken, job.ExecutionGeneration, attemptNo, run.ID, attempt.ID, checkpoint, true); err != nil {
				t.Fatalf("checkpoint save: %v", err)
			}
			checkpoint.ParsedReportJSON = strings.Repeat("x", evidence.MaxReportBytes+1)
			if err := store.UpdateAttemptCheckpointWithClaim(ctx, job.ID, *job.WorkerID, *job.ClaimToken, job.ExecutionGeneration, attemptNo, run.ID, attempt.ID, checkpoint, true); !errors.Is(err, evidence.ErrReportTooLarge) {
				t.Fatalf("checkpoint bound: %v", err)
			}
			checks, _ := json.Marshal(data.RecommendedChecks)
			limitations, _ := json.Marshal(data.Limitations)
			rep := &evidence.Report{ID: "capacity-report", DiagnosisRunID: run.ID, AttemptID: attempt.ID, RootCause: data.RootCause, Summary: data.Summary, ConclusionKind: data.ConclusionKind, ReportStatus: quality.Status, FindingsJSON: string(findings), RecommendedChecksJSON: string(checks), LimitationsJSON: string(limitations), StructuredPayloadJSON: string(encoded), RawOutput: string(encoded)}
			var citations []evidence.Citation
			for _, finding := range data.Findings {
				citations = append(citations, finding.Citations...)
			}
			if err := store.FinalizeSuccess(ctx, job.ID, *job.WorkerID, *job.ClaimToken, run.ID, attempt.ID, rep, citations, 1, 1, 0); err != nil {
				t.Fatalf("atomic finalization of %d bytes: %v", len(encoded), err)
			}
			savedRun, err := store.GetByID(ctx, run.ID)
			if err != nil || savedRun.Status != diagnosis.StatusSucceeded || savedRun.FinalAttemptID != attempt.ID {
				t.Fatalf("run=%+v err=%v", savedRun, err)
			}
			savedReport, err := evidence.NewReportStore(db).GetByAttemptID(ctx, attempt.ID)
			if err != nil || savedReport.FindingsJSON != rep.FindingsJSON || savedReport.StructuredPayloadJSON != string(encoded) || savedReport.RecommendedChecksJSON != string(checks) || savedReport.LimitationsJSON != string(limitations) {
				t.Fatalf("report round trip: %v", err)
			}
			savedAttempt, err := store.GetAttempt(ctx, attempt.ID)
			if err != nil || savedAttempt.ParsedReportJSON != string(encoded) || savedAttempt.Status != diagnosis.AttemptStatusSucceeded {
				t.Fatalf("attempt round trip: %v", err)
			}
			savedJob, err := jobStore.GetJobByID(ctx, job.ID)
			if err != nil || savedJob.Status != jobs.StatusSucceeded {
				t.Fatalf("job=%+v err=%v", savedJob, err)
			}
		})
	}
}
