package evidence_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/evidence"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
)

func TestEvidenceIssuerIsAttemptScopedAndRedactsDisplayContent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "evidence.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	content := "package demo\n// api_key=sk-abcdefghijklmnopqrstuvwxyz\nfunc Handle() {}\n"
	if err := os.WriteFile(filepath.Join(root, "handler.go"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	issuer := evidence.NewEvidenceIssuerWithStore(store, evidence.NewEvidenceStore(db))
	req := evidence.IssueRequest{
		AttemptID: "attempt-a", DiagnosisRunID: "run-a", RepositoryID: "repo", SnapshotID: "snap", CodeIndexBuildID: 7,
		SourceKind: evidence.SourceReadFile, FilePath: "handler.go", StartLine: 1, EndLine: 2,
	}
	first, err := issuer.Issue(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := issuer.Issue(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.ID != second.ID {
		t.Fatalf("same canonical range was not idempotent: first=%q second=%q", first.ID, second.ID)
	}
	if first.DisplayExcerpt == first.RawContentHash || first.DisplayExcerpt == content || !first.RedactionApplied {
		t.Fatalf("expected redacted display excerpt: %+v", first)
	}
	if first.RawContentHash == "" || first.DisplayExcerpt == "" {
		t.Fatalf("missing evidence content metadata: %+v", first)
	}

	other := req
	other.AttemptID = "attempt-b"
	third, err := issuer.Issue(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Fatal("evidence ID crossed attempt scope")
	}
	if _, err := issuer.Resolve(context.Background(), "attempt-b", first.ID); !errors.Is(err, evidence.ErrEvidenceNotFound) {
		t.Fatalf("cross-attempt resolve error = %v, want not found", err)
	}
}

func TestEvidenceIssuerDetectsSnapshotMutation(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "mutation.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\nfunc main() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	issuer := evidence.NewEvidenceIssuerWithStore(store, evidence.NewEvidenceStore(db))
	item, err := issuer.Issue(context.Background(), evidence.IssueRequest{
		AttemptID: "attempt", DiagnosisRunID: "run", RepositoryID: "repo", SnapshotID: "snap", CodeIndexBuildID: 1,
		SourceKind: evidence.SourceInitialRetrieval, FilePath: "main.go", StartLine: 1, EndLine: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package main\nfunc changed() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err = issuer.Verify(context.Background(), item, evidence.DraftLineage{
		AttemptID: "attempt", DiagnosisRunID: "run", RepositoryID: "repo", SnapshotID: "snap", CodeIndexBuildID: 1,
	})
	if !errors.Is(err, evidence.ErrEvidenceHashMismatch) {
		t.Fatalf("mutation verification error = %v, want hash mismatch", err)
	}
}

func TestResolveReportDraftDegradesUnknownEvidenceButKeepsReport(t *testing.T) {
	draft := &evidence.ReportDraft{
		ConclusionKind: evidence.ConclusionRootCause, Summary: "summary", RootCause: "root cause",
		Findings: []evidence.FindingDraft{{Title: "finding", Reasoning: "reasoning", Citations: []evidence.CitationRef{{EvidenceID: "ev_missing", Reason: "support"}}}},
	}
	issuer := evidence.NewEvidenceIssuerWithStore(nil, &emptyEvidenceStore{})
	report, err := evidence.ResolveReportDraft(context.Background(), issuer, draft, evidence.DraftLineage{AttemptID: "attempt", RepositoryID: "repo", SnapshotID: "snap"})
	if err != nil {
		t.Fatal(err)
	}
	quality, err := evidence.ClassifyReport(report, true)
	if err != nil || quality.Status != evidence.ReportDegraded || report.Findings[0].Citations[0].ValidationError != "EVIDENCE_NOT_FOUND" {
		t.Fatalf("report quality=%+v err=%v report=%+v", quality, err, report)
	}
}

type emptyEvidenceStore struct{}

func (*emptyEvidenceStore) Create(context.Context, *evidence.AttemptEvidenceItem) error { return nil }
func (*emptyEvidenceStore) GetByAttemptAndID(context.Context, string, string) (*evidence.AttemptEvidenceItem, error) {
	return nil, evidence.ErrEvidenceNotFound
}
func (*emptyEvidenceStore) FindCanonical(context.Context, string, string, string, int, int, string) (*evidence.AttemptEvidenceItem, error) {
	return nil, nil
}
