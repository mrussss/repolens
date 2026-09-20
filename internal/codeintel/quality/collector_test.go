package quality

import "testing"

func TestComputeQualityCountsSkippedSymlinks(t *testing.T) {
	result := ComputeQuality(nil, nil, nil, 0, 0, 0, []string{
		"skipped symlink /workspace/link.go; symlink targets are not indexed",
		"warning unrelated to symlinks",
		"skipped symlink /workspace/link-test.go; symlink targets are not indexed",
	})
	if result.SymlinksSkipped != 2 {
		t.Fatalf("SymlinksSkipped = %d, want 2", result.SymlinksSkipped)
	}
	if len(result.Warnings) != 3 {
		t.Fatalf("warnings were not preserved: %#v", result.Warnings)
	}
}
