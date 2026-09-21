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
	if _, err := issuer.Resolve(context.Background(), "attempt-b", first.ID); !errors.Is(err, evidence.ErrEvidenceAttemptMismatch) {
		t.Fatalf("cross-attempt resolve error = %v, want attempt mismatch", err)
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

func TestEvidenceIssuerDoesNotReuseAcrossSnapshotsAndRejectsUnsafeRanges(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	for _, snapshotID := range []string{"snap-a", "snap-b"} {
		root, err := store.EnsureDir("repo", snapshotID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "scope.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	issuer := evidence.NewEvidenceIssuerWithStore(store, evidence.NewEvidenceStore(db))
	base := evidence.IssueRequest{
		AttemptID: "attempt", DiagnosisRunID: "run", RepositoryID: "repo", SnapshotID: "snap-a", CodeIndexBuildID: 1,
		SourceKind: evidence.SourceReadFile, FilePath: "main.go", StartLine: 1, EndLine: 2,
	}
	first, err := issuer.Issue(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	otherSnapshot := base
	otherSnapshot.SnapshotID = "snap-b"
	second, err := issuer.Issue(context.Background(), otherSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("evidence ID was reused across snapshots")
	}

	for _, test := range []struct {
		name  string
		path  string
		start int
	}{
		{name: "start beyond file", path: "main.go", start: 99},
		{name: "path escape", path: "../main.go", start: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.FilePath = test.path
			request.StartLine = test.start
			if _, err := issuer.Issue(context.Background(), request); !errors.Is(err, evidence.ErrEvidenceSourceUnavailable) {
				t.Fatalf("issue error = %v, want source unavailable", err)
			}
		})
	}

	root, err := store.EnsureDir("repo", "snap-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "main.go"), filepath.Join(root, "link.go")); err == nil {
		request := base
		request.FilePath = "link.go"
		if _, err := issuer.Issue(context.Background(), request); !errors.Is(err, evidence.ErrEvidenceSourceUnavailable) {
			t.Fatalf("symlink issue error = %v, want source unavailable", err)
		}
	}
}

func TestEvidenceIssuerDoesNotReturnIDWhenPersistenceFails(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	issuer := evidence.NewEvidenceIssuerWithStore(store, failingEvidenceStore{})
	item, err := issuer.Issue(context.Background(), evidence.IssueRequest{
		AttemptID: "attempt", DiagnosisRunID: "run", RepositoryID: "repo", SnapshotID: "snap", CodeIndexBuildID: 1,
		SourceKind: evidence.SourceReadFile, FilePath: "main.go", StartLine: 1, EndLine: 1,
	})
	if item != nil || err == nil {
		t.Fatalf("persistence failure returned item=%+v err=%v", item, err)
	}
}

func TestEvidenceIssuerRejectsOverlongCanonicalLine(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "long.go"), []byte("123456789\n"), 0600); err != nil {
		t.Fatal(err)
	}
	issuer := evidence.NewEvidenceIssuerWithStore(store, &emptyEvidenceStore{})
	_, err = issuer.Issue(context.Background(), evidence.IssueRequest{
		AttemptID: "attempt", DiagnosisRunID: "run", RepositoryID: "repo", SnapshotID: "snap", CodeIndexBuildID: 1,
		SourceKind: evidence.SourceReadFile, FilePath: "long.go", StartLine: 1, EndLine: 1, MaxBytes: 4,
	})
	if !errors.Is(err, snapshotstore.ErrLineTooLong) {
		t.Fatalf("overlong line error = %v, want ErrLineTooLong", err)
	}
}

func TestResolveReportDraftValidatesEvidenceScopeAndHash(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\nfunc main() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "resolve.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	issuer := evidence.NewEvidenceIssuerWithStore(store, evidence.NewEvidenceStore(db))
	item, err := issuer.Issue(context.Background(), evidence.IssueRequest{
		AttemptID: "attempt-a", DiagnosisRunID: "run-a", RepositoryID: "repo", SnapshotID: "snap", CodeIndexBuildID: 1,
		SourceKind: evidence.SourceReadFile, FilePath: "main.go", StartLine: 1, EndLine: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Issue(context.Background(), evidence.IssueRequest{
		AttemptID: "attempt-b", DiagnosisRunID: "run-a", RepositoryID: "repo", SnapshotID: "snap", CodeIndexBuildID: 1,
		SourceKind: evidence.SourceReadFile, FilePath: "main.go", StartLine: 1, EndLine: 2,
	}); err != nil {
		t.Fatal(err)
	}

	draft := &evidence.ReportDraft{
		ConclusionKind: evidence.ConclusionRootCause, Summary: "summary", RootCause: "root cause",
		Findings: []evidence.FindingDraft{{Title: "finding", Reasoning: "reasoning", Citations: []evidence.CitationRef{{EvidenceID: item.ID, Reason: "supports"}}}},
	}
	report, err := evidence.ResolveReportDraft(context.Background(), issuer, draft, evidence.DraftLineage{
		AttemptID: "attempt-a", DiagnosisRunID: "run-a", RepositoryID: "repo", SnapshotID: "snap", CodeIndexBuildID: 1,
	})
	if err != nil || report.Findings[0].Citations[0].ValidationStatus != evidence.CitationValid {
		t.Fatalf("valid resolution = %+v err=%v", report, err)
	}
	otherSnapshotReport, err := evidence.ResolveReportDraft(context.Background(), issuer, draft, evidence.DraftLineage{
		AttemptID: "attempt-a", DiagnosisRunID: "run-a", RepositoryID: "repo", SnapshotID: "snap-other", CodeIndexBuildID: 1,
	})
	if err != nil || otherSnapshotReport.Findings[0].Citations[0].ValidationError != "EVIDENCE_LINEAGE_MISMATCH" {
		t.Fatalf("other-snapshot resolution = %+v err=%v", otherSnapshotReport, err)
	}

	foreignDraft := *draft
	foreignDraft.Findings = append([]evidence.FindingDraft(nil), draft.Findings...)
	foreignDraft.Findings[0].Citations = []evidence.CitationRef{{EvidenceID: item.ID, Reason: "foreign"}}
	foreignReport, err := evidence.ResolveReportDraft(context.Background(), issuer, &foreignDraft, evidence.DraftLineage{
		AttemptID: "attempt-b", DiagnosisRunID: "run-a", RepositoryID: "repo", SnapshotID: "snap", CodeIndexBuildID: 1,
	})
	if err != nil || foreignReport.Findings[0].Citations[0].ValidationError != "EVIDENCE_ATTEMPT_MISMATCH" {
		t.Fatalf("foreign resolution = %+v err=%v", foreignReport, err)
	}

	mutatedLineageDraft := &evidence.ReportDraft{
		ConclusionKind: evidence.ConclusionRootCause, Summary: "summary", RootCause: "root cause",
		Findings: []evidence.FindingDraft{{Title: "finding", Reasoning: "reasoning", Citations: []evidence.CitationRef{{EvidenceID: item.ID}}}},
	}
	if err := os.WriteFile(path, []byte("package main\nfunc changed() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mutatedReport, err := evidence.ResolveReportDraft(context.Background(), issuer, mutatedLineageDraft, evidence.DraftLineage{
		AttemptID: "attempt-a", DiagnosisRunID: "run-a", RepositoryID: "repo", SnapshotID: "snap", CodeIndexBuildID: 1,
	})
	if err != nil || mutatedReport.Findings[0].Citations[0].ValidationError != "EVIDENCE_CONTENT_HASH_MISMATCH" {
		t.Fatalf("mutated resolution = %+v err=%v", mutatedReport, err)
	}
}

type failingEvidenceStore struct{}

func (failingEvidenceStore) Create(context.Context, *evidence.AttemptEvidenceItem) error {
	return errors.New("database write failed")
}
func (failingEvidenceStore) GetByAttemptAndID(context.Context, string, string) (*evidence.AttemptEvidenceItem, error) {
	return nil, errors.New("database lookup failed")
}
func (failingEvidenceStore) FindCanonical(context.Context, string, string, string, int, int, string) (*evidence.AttemptEvidenceItem, error) {
	return nil, nil
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

func TestResolveReportDraftKeepsSourceErrorsStable(t *testing.T) {
	draft := &evidence.ReportDraft{
		ConclusionKind: evidence.ConclusionRootCause, Summary: "summary", RootCause: "root cause",
		Findings: []evidence.FindingDraft{{Title: "finding", Reasoning: "reasoning", Citations: []evidence.CitationRef{{EvidenceID: "ev_source", Reason: "support"}}}},
	}
	report, err := evidence.ResolveReportDraft(context.Background(), unavailableEvidenceIssuer{}, draft, evidence.DraftLineage{AttemptID: "attempt", SnapshotID: "snap"})
	if err != nil || report.Findings[0].Citations[0].ValidationError != "EVIDENCE_SOURCE_UNAVAILABLE" {
		t.Fatalf("source error resolution = %+v err=%v", report, err)
	}
}

type unavailableEvidenceIssuer struct{}

func (unavailableEvidenceIssuer) Issue(context.Context, evidence.IssueRequest) (*evidence.AttemptEvidenceItem, error) {
	return nil, evidence.ErrEvidenceSourceUnavailable
}
func (unavailableEvidenceIssuer) Resolve(context.Context, string, string) (*evidence.AttemptEvidenceItem, error) {
	return nil, errors.New("database path /secret/provider.json: permission denied")
}

type emptyEvidenceStore struct{}

func (*emptyEvidenceStore) Create(context.Context, *evidence.AttemptEvidenceItem) error { return nil }
func (*emptyEvidenceStore) GetByAttemptAndID(context.Context, string, string) (*evidence.AttemptEvidenceItem, error) {
	return nil, evidence.ErrEvidenceNotFound
}
func (*emptyEvidenceStore) FindCanonical(context.Context, string, string, string, int, int, string) (*evidence.AttemptEvidenceItem, error) {
	return nil, nil
}
