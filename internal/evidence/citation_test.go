package evidence_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"repolens/internal/evidence"
	"repolens/internal/platform/snapshotstore"
)

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
		StartLine: 3,
		EndLine:   5,
		Excerpt:   "if err != nil",
	}
	validator.Validate(ctx, repoID, snapID, &validCit)
	if validCit.ValidationStatus != evidence.CitationValid {
		t.Errorf("expected VALID status, got %s (err: %s)", validCit.ValidationStatus, validCit.ValidationError)
	}
	if validCit.ContentHash == "" {
		t.Errorf("expected content hash computed")
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
}
