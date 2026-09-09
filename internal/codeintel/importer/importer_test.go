package importer_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
	"time"

	codeimporter "repolens/internal/codeintel/importer"
)

func TestOfflineImporterAllowsStdlibAndRejectsExternalAndCgo(t *testing.T) {
	fset := token.NewFileSet()
	imp := codeimporter.NewOfflineImporter(fset, "example.com/root", nil)

	if _, err := imp.Import("fmt"); err != nil {
		t.Fatalf("stdlib import fmt failed: %v", err)
	}
	if _, err := imp.Import("net/http"); err != nil {
		t.Fatalf("stdlib import net/http failed: %v", err)
	}
	if _, err := imp.Import("github.com/google/uuid"); !errors.Is(err, codeimporter.ErrExternalDependencyUnresolved) {
		t.Fatalf("external import error = %v, want ErrExternalDependencyUnresolved", err)
	}
	if _, err := imp.Import("C"); !errors.Is(err, codeimporter.ErrUnsupportedCgoImport) {
		t.Fatalf("cgo import error = %v, want ErrUnsupportedCgoImport", err)
	}
}

func TestOfflineImporterCrossPackageDoesNotDeadlock(t *testing.T) {
	fset := token.NewFileSet()
	parse := func(name, source string) *ast.File {
		f, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	pkgB := "example.com/root/pkg_b"
	pkgA := "example.com/root/pkg_a"
	aFile := parse("a.go", "package pkg_a; import \"example.com/root/pkg_b\"; var _ pkg_b.Value")
	imp := codeimporter.NewOfflineImporter(fset, "example.com/root", map[string]*codeimporter.PkgFiles{
		pkgB: {PkgPath: pkgB, PkgName: "pkg_b", Files: []*ast.File{parse("b.go", "package pkg_b; type Value struct { N int }")}},
		pkgA: {PkgPath: pkgA, PkgName: "pkg_a", Files: []*ast.File{aFile}},
	})

	done := make(chan error, 1)
	go func() {
		conf := types.Config{Importer: imp}
		_, err := conf.Check(pkgA, fset, []*ast.File{aFile}, nil)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cross-package offline type check deadlocked")
	}
}
