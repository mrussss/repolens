package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"repolens/internal/codeintel/model"
	"repolens/internal/codeintel/symbol"
)

type testImporter struct{ pkg *types.Package }

func (i testImporter) Import(path string) (*types.Package, error) { return i.pkg, nil }

func TestDirectSemanticUsageUsesCanonicalDeclaredIdentity(t *testing.T) {
	fset := token.NewFileSet()
	external, err := parser.ParseFile(fset, "external.go", `package other; type A struct{}; func (A) Run(){}`, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := new(types.Config).Check("example.com/m/other", fset, []*ast.File{external}, nil)
	if err != nil {
		t.Fatal(err)
	}
	source := `package p
import "example.com/m/other"
type A struct{}
func (A) Run(){}
type B struct{}
func (*B) Run(){}
type Box[T any] struct{}
func (*Box[T]) Run(){}
type Embedded struct{ A }
func Run(){}
func TestA(){ A{}.Run() }
func TestPointer(){ a := &A{}; a.Run() }
func TestB(){ b := B{}; b.Run() }
func TestGeneric(){ b := Box[int]{}; b.Run() }
func TestPromoted(){ Embedded{}.Run() }
func TestExpression(){ A.Run(A{}) }
func TestFunction(){ Run() }
func TestExternal(){ other.A{}.Run() }
func TestShadow(){ Run := func(){}; Run() }
func TestRun(){}
func TestUnrelated(){}
`
	file, err := parser.ParseFile(fset, "p_test.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	_, err = (&types.Config{Importer: testImporter{other}}).Check("example.com/m", fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatal(err)
	}
	symbols := symbol.ExtractSymbols(fset, file, "p.go", "example.com/m", "example.com/m", []byte(source))
	funcs := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			funcs[fn.Name.Name] = fn
		}
	}
	expected := map[string]string{"TestA": "A", "TestPointer": "A", "TestB": "B", "TestGeneric": "Box", "TestPromoted": "A", "TestExpression": "A", "TestFunction": "", "TestExternal": "external", "TestShadow": "none", "TestRun": "none", "TestUnrelated": "none"}
	for name, receiver := range expected {
		t.Run(name, func(t *testing.T) {
			for _, sym := range symbols {
				if sym.Name != "Run" {
					continue
				}
				want := sym.ReceiverCanonical == receiver
				if got := hasDirectSemanticUsage(funcs[name], info, sym); got != want {
					t.Fatalf("target %s: direct=%v want=%v", sym.SymbolKeyRaw, got, want)
				}
			}
		})
	}
	// Exercise the public discovery path too: a mismatching receiver may
	// retain syntactic evidence, but can never receive semantic confidence.
	var discoverySymbols []*model.Symbol
	for _, sym := range symbols {
		if sym.Name == "Run" {
			discoverySymbols = append(discoverySymbols, sym)
		}
	}
	testRaw, testHash := model.BuildSymbolKey("example.com/m", "example.com/m", "", model.SymbolKindFunction, "TestA")
	discoverySymbols = append(discoverySymbols, &model.Symbol{FilePath: "p_test.go", Name: "TestA", PackagePath: "example.com/m", SymbolKeyRaw: testRaw, SymbolKeyHash: testHash})
	discoveries := DiscoverRelatedTests(&TestDiscoveryContext{Fset: fset, Symbols: discoverySymbols, TestFiles: map[string]*ast.File{"p_test.go": file}, TypeInfoByFile: map[string]*types.Info{"p_test.go": info}})
	if len(discoveries) != 4 {
		t.Fatalf("expected all four Run declarations, got %+v", discoveries)
	}
	for _, discovery := range discoveries {
		var targetSymbol *model.Symbol
		for _, sym := range discoverySymbols {
			if sym.SymbolKeyHash == discovery.TargetSymbolKeyHash {
				targetSymbol = sym
			}
		}
		if targetSymbol.ReceiverCanonical == "A" {
			if discovery.ReasonCode != model.TestReasonDirectSemantic || discovery.ResolutionKind != model.ResolutionKindSemantic || discovery.Confidence != 1 {
				t.Fatalf("A.Run lost semantic evidence: %+v", discovery)
			}
		} else if discovery.ReasonCode != model.TestReasonDirectSyntactic || discovery.ResolutionKind != model.ResolutionKindSyntactic || discovery.Confidence >= 1 {
			t.Fatalf("mismatched receiver was promoted: %+v", discovery)
		}
	}

	// The external selection must match its own package, never a local A.Run.
	raw, hash := model.BuildSymbolKey("example.com/m", "example.com/m/other", "A", model.SymbolKindMethod, "Run")
	target := &model.Symbol{ModulePath: "example.com/m", PackagePath: "example.com/m/other", Kind: model.SymbolKindMethod, Name: "Run", ReceiverCanonical: "A", SymbolKeyRaw: raw, SymbolKeyHash: hash}
	if !hasDirectSemanticUsage(funcs["TestExternal"], info, target) {
		t.Fatal("external identity did not resolve")
	}
	target.SymbolKeyHash = "wrong-declaration"
	if hasDirectSemanticUsage(funcs["TestExternal"], info, target) {
		t.Fatal("inconsistent symbol identity accepted")
	}
	// Preserve the lower confidence signals for tests with no semantic usage.
	var prod *model.Symbol
	for _, sym := range symbols {
		if sym.Name == "Run" && sym.Kind == model.SymbolKindFunction {
			prod = sym
			break
		}
	}
	for _, tc := range []struct {
		name   string
		reason model.TestRelationReason
	}{{"TestRun", model.TestReasonNameMatch}, {"TestUnrelated", model.TestReasonSamePackage}} {
		testRaw, testHash := model.BuildSymbolKey("example.com/m", "example.com/m", "", model.SymbolKindFunction, tc.name)
		discoveries := DiscoverRelatedTests(&TestDiscoveryContext{Fset: fset, Symbols: []*model.Symbol{prod, {FilePath: "p_test.go", Name: tc.name, PackagePath: "example.com/m", SymbolKeyRaw: testRaw, SymbolKeyHash: testHash}}, TestFiles: map[string]*ast.File{"p_test.go": file}, TypeInfoByFile: map[string]*types.Info{"p_test.go": info}})
		if len(discoveries) != 1 || discoveries[0].ReasonCode != tc.reason || discoveries[0].ResolutionKind != model.ResolutionKindHeuristic {
			t.Fatalf("%s lost heuristic signal: %+v", tc.name, discoveries)
		}
	}
}
