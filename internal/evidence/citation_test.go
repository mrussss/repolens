package evidence_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"repolens/internal/evidence"
	"repolens/internal/platform/snapshotstore"
)

func TestCitationAcceptsAgentPathField(t *testing.T) {
	var citation evidence.Citation
	if err := json.Unmarshal([]byte(`{"path":"middleware/wrap_writer.go","start_line":10,"end_line":12}`), &citation); err != nil {
		t.Fatalf("failed to decode agent citation: %v", err)
	}
	if citation.FilePath != "middleware/wrap_writer.go" {
		t.Fatalf("file path = %q, want middleware/wrap_writer.go", citation.FilePath)
	}

	var canonical evidence.Citation
	if err := json.Unmarshal([]byte(`{"file_path":"canonical.go","start_line":1,"end_line":1}`), &canonical); err != nil {
		t.Fatalf("failed to decode canonical citation: %v", err)
	}
	if canonical.FilePath != "canonical.go" {
		t.Fatalf("canonical file path = %q, want canonical.go", canonical.FilePath)
	}
}

func TestCitationVerification(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "repolens_cit_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	storeFS := snapshotstore.NewLocalSnapshotStore(tmpDir)
	repoID := "repo-cit-1"
	snapID := "snap-cit-1"

	sourceDir, err := storeFS.EnsureDir(repoID, snapID)
	if err != nil {
		t.Fatalf("failed to create snapshot dir: %v", err)
	}

	testFile := filepath.Join(sourceDir, "worker.go")
	testContent := `package worker

func ProcessTask() error {
    // line 4
    if err != nil {
        return err
    }
    return nil
}
`
	_ = os.WriteFile(testFile, []byte(testContent), 0644)

	validator := evidence.NewCitationValidator(storeFS)
	ctx := context.Background()

	// 1. Valid citation
	validCit := evidence.Citation{
		FilePath:  "worker.go",
		StartLine: 5,
		EndLine:   5,
		Excerpt:   "    if err != nil {",
	}
	validator.Validate(ctx, repoID, snapID, &validCit)
	if validCit.ValidationStatus != evidence.CitationValid {
		t.Errorf("expected VALID status, got %s (err: %s)", validCit.ValidationStatus, validCit.ValidationError)
	}
	if validCit.ContentHash == "" {
		t.Errorf("expected content hash computed")
	}
	hashCitation := evidence.Citation{FilePath: "worker.go", StartLine: 5, EndLine: 5, ContentHash: validCit.ContentHash}
	validator.Validate(ctx, repoID, snapID, &hashCitation)
	if hashCitation.ValidationStatus != evidence.CitationValid {
		t.Errorf("exact range hash was rejected: %s", hashCitation.ValidationError)
	}

	// 2. Non-existent file citation
	invalidCit1 := evidence.Citation{
		FilePath:  "non_existent.go",
		StartLine: 1,
		EndLine:   10,
	}
	validator.Validate(ctx, repoID, snapID, &invalidCit1)
	if invalidCit1.ValidationStatus != evidence.CitationInvalid {
		t.Errorf("expected INVALID status for non-existent file, got %s", invalidCit1.ValidationStatus)
	}

	// 3. Invalid line range (start > end)
	invalidCit2 := evidence.Citation{
		FilePath:  "worker.go",
		StartLine: 8,
		EndLine:   2,
	}
	validator.Validate(ctx, repoID, snapID, &invalidCit2)
	if invalidCit2.ValidationStatus != evidence.CitationInvalid {
		t.Errorf("expected INVALID status for start > end, got %s", invalidCit2.ValidationStatus)
	}

	// 4. Mismatched excerpt
	invalidCit3 := evidence.Citation{
		FilePath:  "worker.go",
		StartLine: 1,
		EndLine:   2,
		Excerpt:   "this text definitely does not exist on lines 1-2",
	}
	validator.Validate(ctx, repoID, snapID, &invalidCit3)
	if invalidCit3.ValidationStatus != evidence.CitationInvalid {
		t.Errorf("expected INVALID status for mismatched excerpt, got %s", invalidCit3.ValidationStatus)
	}

	for name, excerpt := range map[string]string{
		"fabricated prefix": "fabricated prefix\npackage worker",
		"fabricated suffix": "package worker\nfabricated suffix",
	} {
		t.Run(name, func(t *testing.T) {
			citation := evidence.Citation{FilePath: "worker.go", StartLine: 1, EndLine: 1, Excerpt: excerpt}
			validator.Validate(ctx, repoID, snapID, &citation)
			if citation.ValidationStatus != evidence.CitationInvalid {
				t.Fatalf("fabricated excerpt was accepted: %+v", citation)
			}
		})
	}

	wrongSnapshot := evidence.Citation{SnapshotID: "another-snapshot", FilePath: "worker.go", StartLine: 5, EndLine: 5, Excerpt: "    if err != nil {"}
	validator.Validate(ctx, repoID, snapID, &wrongSnapshot)
	if wrongSnapshot.ValidationStatus != evidence.CitationInvalid {
		t.Fatalf("citation from another snapshot was accepted: %+v", wrongSnapshot)
	}

	wrongRange := evidence.Citation{FilePath: "worker.go", StartLine: 1, EndLine: 1, Excerpt: "    if err != nil {"}
	validator.Validate(ctx, repoID, snapID, &wrongRange)
	if wrongRange.ValidationStatus != evidence.CitationInvalid {
		t.Fatalf("citation excerpt from another range was accepted: %+v", wrongRange)
	}

	wrongHash := evidence.Citation{FilePath: "worker.go", StartLine: 5, EndLine: 5, ContentHash: strings.Repeat("0", 64)}
	validator.Validate(ctx, repoID, snapID, &wrongHash)
	if wrongHash.ValidationStatus != evidence.CitationInvalid {
		t.Fatalf("citation with wrong range hash was accepted: %+v", wrongHash)
	}
}
