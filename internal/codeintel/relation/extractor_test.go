package relation_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"repolens/internal/codeintel"
	codeintelmodel "repolens/internal/codeintel/model"
)

func TestSemanticMethodResolutionUsesDeclarationReceiver(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "go.mod", "module example.com/root\n\ngo 1.22\n")
	writeFixtureFile(t, root, "methods.go", `package root

type Service struct{}
func (s *Service) Execute() {}
func RunService(s *Service) { s.Execute() }

type ValueService struct{}
func (s ValueService) Execute() {}
func RunValueService(s ValueService) { s.Execute() }

type Stack[T any] struct{}
func (s *Stack[T]) Push(value T) {}
func RunStack(s *Stack[int]) { s.Push(1) }

type Inner struct{}
func (Inner) Promoted() {}
type Outer struct{ Inner }
func RunPromoted(o Outer) { o.Promoted() }
`)

	result, err := codeintel.NewAnalyzer().Analyze(context.Background(), root, codeintel.DefaultBuildContext())
	if err != nil {
		t.Fatalf("Analyze failed: %v", err)
	}

	wantCalls := map[string]string{
		"root.RunService":      "Service.Execute",
		"root.RunValueService": "ValueService.Execute",
		"root.RunStack":        "Stack.Push",
		"root.RunPromoted":     "Inner.Promoted",
	}
	for callerName, targetName := range wantCalls {
		caller := findSymbol(result.Symbols, callerName)
		if caller == nil {
			t.Fatalf("caller symbol %s not found", callerName)
		}
		target := findSymbol(result.Symbols, "root."+targetName)
		if target == nil {
			t.Fatalf("target symbol %s not found", targetName)
		}
		found := false
		for _, relation := range result.Relations {
			if relation.FromSymbolKeyHash != caller.SymbolKeyHash || relation.ReasonCode != "SEMANTIC_METHOD_SELECTION" {
				continue
			}
			if relation.ToSymbolKeyHash != target.SymbolKeyHash {
				t.Errorf("%s resolved to %s, want %s", callerName, relation.TargetQualifiedName, targetName)
			}
			found = true
		}
		if !found {
			t.Errorf("no exact semantic method relation for %s -> %s", callerName, targetName)
		}
	}
}

func TestSemanticMethodResolutionKeepsSameNamedMethodsInTheirPackage(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "go.mod", "module example.com/root\n\ngo 1.22\n")
	writeFixtureFile(t, root, "pkg_a/worker.go", `package pkg_a
type Worker struct{}
func (*Worker) Process() {}
`)
	writeFixtureFile(t, root, "pkg_b/worker.go", `package pkg_b
type Worker struct{}
func (*Worker) Process() {}
`)
	writeFixtureFile(t, root, "caller.go", `package root
import (
    "example.com/root/pkg_a"
    "example.com/root/pkg_b"
)
func Run(a *pkg_a.Worker, b *pkg_b.Worker) { a.Process(); b.Process() }
`)

	result, err := codeintel.NewAnalyzer().Analyze(context.Background(), root, codeintel.DefaultBuildContext())
	if err != nil {
		t.Fatalf("Analyze failed: %v", err)
	}
	caller := findSymbol(result.Symbols, "root.Run")
	if caller == nil {
		t.Fatal("caller symbol root.Run not found")
	}
	want := map[string]bool{
		"example.com/root/pkg_a": false,
		"example.com/root/pkg_b": false,
	}
	for _, relation := range result.Relations {
		if relation.FromSymbolKeyHash == caller.SymbolKeyHash && relation.ReasonCode == "SEMANTIC_METHOD_SELECTION" {
			want[relation.TargetPackagePath] = true
		}
	}
	for packagePath, found := range want {
		if !found {
			t.Errorf("missing semantic Process relation for %s", packagePath)
		}
	}
}

func findSymbol(symbols []*codeintelmodel.Symbol, qualifiedName string) *codeintelmodel.Symbol {
	for _, symbol := range symbols {
		if symbol.PackageName+"."+symbol.ReceiverCanonical+symbolNameSeparator(symbol)+symbol.Name == qualifiedName {
			return symbol
		}
	}
	return nil
}

func symbolNameSeparator(symbol *codeintelmodel.Symbol) string {
	if symbol.ReceiverCanonical == "" {
		return ""
	}
	return "."
}

func writeFixtureFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
