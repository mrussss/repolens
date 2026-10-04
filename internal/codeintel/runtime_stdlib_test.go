package codeintel_test

import (
	"context"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"repolens/internal/codeintel"
	offline "repolens/internal/codeintel/importer"
	codeintelmodel "repolens/internal/codeintel/model"
)

// This test also runs as a compiled test binary inside the production runtime.
func TestRuntimeStdlibAndOfflineBoundary(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/demo\ngo 1.22\n")
	write("hello.go", "package demo\nimport \"fmt\"\nfunc Hello() string { return fmt.Sprintf(\"%s\", \"hello\") }\n")
	result, err := codeintel.NewAnalyzer().Analyze(context.Background(), root, codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	if result.Quality.PackagesTypechecked != 1 || result.Quality.PackagesFailed != 0 {
		t.Fatalf("stdlib type-check degraded: %+v", result.Quality)
	}
	imp := offline.NewOfflineImporter(token.NewFileSet(), "example.com/demo", nil)
	if pkg, err := imp.Import("fmt"); err != nil || pkg.Scope().Lookup("Sprintf") == nil {
		t.Fatalf("fmt missing: %v %v", pkg, err)
	}
	// This dependency exists in the builder's module cache but is never eligible.
	write("hello.go", "package demo\nimport \"github.com/google/uuid\"\nfunc Hello() string { return uuid.NewString() }\n")
	result, err = codeintel.NewAnalyzer().Analyze(context.Background(), root, codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	if result.Quality.PackagesFailed != 1 || result.Quality.PackagesTypechecked != 0 {
		t.Fatalf("external dependency resolved from environment: %+v", result.Quality)
	}
	if !strings.Contains(strings.Join(result.Quality.Warnings, "\n"), offline.ErrExternalDependencyUnresolved.Error()) {
		t.Fatalf("missing offline diagnostic: %+v", result.Quality)
	}
}

func TestAnalyzerRejectsSymlinkModuleBeforeTypeAnalysis(t *testing.T) {
	for _, inside := range []bool{false, true} {
		root := t.TempDir()
		targetRoot := root
		if !inside {
			targetRoot = t.TempDir()
		}
		target := filepath.Join(targetRoot, "metadata.mod")
		if err := os.WriteFile(target, []byte("module example.com/forbidden\ngo 1.22\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "go.mod")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package demo\nfunc Hello() {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, allowed := range [][]string{nil, {"go.mod", "source.go"}} {
			result, err := codeintel.NewAnalyzer().AnalyzeWithAllowedFiles(context.Background(), root, allowed, codeintelmodel.DefaultBuildContext())
			if err == nil || result != nil {
				t.Fatalf("external metadata participated in analysis: result=%+v err=%v", result, err)
			}
		}
	}
}
