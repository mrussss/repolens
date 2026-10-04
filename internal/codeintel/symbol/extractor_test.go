package symbol

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"repolens/internal/codeintel/model"
)

func TestExtractSymbols_PreservesLongDocsWithoutEmbeddingThemInSignatures(t *testing.T) {
	var source strings.Builder
	source.WriteString("package sample\n\n// Box documentation must not become its signature.\ntype Box[T any] struct{}\n\n")
	for i := 0; i < 720; i++ {
		fmt.Fprintf(&source, "// doc-row-%04d %s\n", i, strings.Repeat("x", 96))
	}
	source.WriteString("func (b *Box[T]) Value() T { var zero T; return zero }\n\n")
	source.WriteString("// Generic documentation must not become its signature.\nfunc Identity[T any](value T) T { return value }\n")
	contents := source.String()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", contents, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	symbols := ExtractSymbols(fset, file, "sample.go", "example.com/sample", "example.com/sample", []byte(contents))
	byName := make(map[string]*model.Symbol, len(symbols))
	for _, sym := range symbols {
		byName[sym.Name] = sym
	}

	box := byName["Box"]
	if box == nil || strings.Contains(box.Signature, "Box documentation") {
		t.Fatalf("type signature includes its doc or is missing: %+v", box)
	}
	method := byName["Value"]
	if method == nil {
		t.Fatal("method symbol missing")
	}
	if len(method.Doc) <= 65535 {
		t.Fatalf("method doc bytes=%d; want complete doc above TEXT capacity", len(method.Doc))
	}
	if !strings.Contains(method.Doc, "doc-row-0000") || !strings.Contains(method.Doc, "doc-row-0719") {
		t.Fatal("long method documentation was truncated")
	}
	if strings.Contains(method.Signature, "doc-row-") || !strings.Contains(method.Signature, "(b *Box[T]) Value() T") {
		t.Fatalf("method signature lost its receiver or includes documentation: %q", method.Signature)
	}
	function := byName["Identity"]
	if function == nil {
		t.Fatal("generic function symbol missing")
	}
	if strings.Contains(function.Signature, "Generic documentation") || !strings.Contains(function.Signature, "Identity[T any](value T) T") {
		t.Fatalf("generic function signature=%q", function.Signature)
	}

	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "Value" {
			if fn.Doc == nil || fn.Body == nil {
				t.Fatal("extractor did not restore the parsed method AST")
			}
			return
		}
	}
	t.Fatal("parsed method declaration missing after extraction")
}
