package parser

import (
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"repolens/internal/codeintel/model"
)

func TestParseRepositorySkipsSymlinkAndRecordsWarning(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/test\ngo 1.22\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real.go"), []byte("package test\n\nfunc Real() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(target, []byte("package outside\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "linked.go")); err != nil {
		t.Skipf("symlink is unavailable: %v", err)
	}

	moduleInfo, err := DiscoverModule(root)
	if err != nil {
		t.Fatal(err)
	}
	files, warnings, err := ParseRepository(token.NewFileSet(), root, moduleInfo, model.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].CodeFile.Path != "real.go" {
		t.Fatalf("parsed files = %+v, want only real.go", files)
	}
	foundWarning := false
	for _, warning := range warnings {
		if strings.HasSuffix(warning, "skipped symlink "+filepath.Join(root, "linked.go")+"; symlink targets are not indexed") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatalf("warnings = %v, want symlink warning", warnings)
	}
}
