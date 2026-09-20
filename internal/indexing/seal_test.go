package indexing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSealSnapshotDoesNotChmodSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(target, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink is unavailable: %v", err)
	}
	if err := sealSnapshot(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("symlink target mode = %o, want 600", info.Mode().Perm())
	}
	linkInfo, err := os.Lstat(filepath.Join(root, "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("snapshot link was replaced or followed")
	}
	_ = os.Chmod(root, 0755)
}
