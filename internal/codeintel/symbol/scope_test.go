package symbol

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

func TestExtractSymbolsPackageScope(t *testing.T) {
	for _, tc := range []struct{ name, source, want string }{
		{"initializer_closures", "var A = func(){type T struct{A int}}\nvar B = func(){type T struct{B string}}", ""},
		{"initializer_iifes", "var A = func()int{type T struct{A int};return 1}()\nvar B = func()int{type T struct{B string};return 2}()", ""},
		{"named_function_bodies", "func A(){type T struct{A int}}\nfunc B(){type T struct{B string}}", "FUNCTION:A,FUNCTION:B"},
		{"shadowed_package_type", "type T struct{A int}\nvar A = func(){type T struct{B string}}", "TYPE:T"},
		{"closures_inside_function", "func A(){_ = func(){type T struct{A int}};_ = func(){type T struct{B string}}}", "FUNCTION:A"},
		{"initializer_interfaces", "var A = func(){type T interface{M()}}\nvar B = func(){type T interface{N()}}", ""},
		{"nested_callback_registration", "func register(f func())bool{return true}\nvar _ = register(func(){type LocalAlias = int;_ = register(func(){type LocalAlias = string})})", "FUNCTION:register"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := "package scope\n" + tc.source + "\n"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "scope.go", source, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = (&types.Config{}).Check("example.com/scope", fset, []*ast.File{file}, nil); err != nil {
				t.Fatalf("fixture must be legal Go: %v", err)
			}
			syms := ExtractSymbols(fset, file, "scope.go", "example.com/scope", "example.com/scope", []byte(source))
			var got []string
			seen := map[string]bool{}
			for _, sym := range syms {
				got = append(got, fmt.Sprintf("%s:%s", sym.Kind, sym.Name))
				if seen[sym.SymbolKeyHash] {
					t.Errorf("duplicate package identity: %s", sym.SymbolKeyRaw)
				}
				seen[sym.SymbolKeyHash] = true
			}
			if strings.Join(got, ",") != tc.want {
				t.Fatalf("package symbols=%v, want %q", got, tc.want)
			}
		})
	}
}

func TestPackageSymbolIdentitiesRemainStable(t *testing.T) {
	const source = `package sample
type (
 Box[T any] struct{ Value T }
 Alias = int
 Reader interface{ Read() int }
)
func Identity[T any](value T) T { return value }
func (b Box[T]) Get() T { return b.Value }
func (b *Box[T]) Set(value T) { b.Value = value }
func init() {}; func init() {}
var _ = func(){type Box struct{};type Alias = string;type Reader interface{Wrong()}}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (&types.Config{}).Check("example.com/sample", fset, []*ast.File{file}, nil); err != nil {
		t.Fatal(err)
	}
	syms := ExtractSymbols(fset, file, "sample.go", "example.com/sample", "example.com/sample", []byte(source))
	want := []string{
		"example.com/sample|example.com/sample||TYPE|Box",
		"example.com/sample|example.com/sample||TYPE|Alias",
		"example.com/sample|example.com/sample||INTERFACE|Reader",
		"example.com/sample|example.com/sample||FUNCTION|Identity",
		"example.com/sample|example.com/sample|Box|METHOD|Get",
		"example.com/sample|example.com/sample|Box|METHOD|Set",
		"example.com/sample|example.com/sample||FUNCTION|init|sample.go:10:1",
		"example.com/sample|example.com/sample||FUNCTION|init|sample.go:10:17",
	}
	if len(syms) != len(want) {
		t.Fatalf("got %d symbols, want %d", len(syms), len(want))
	}
	for i, sym := range syms {
		if sym.SymbolKeyRaw != want[i] || sym.SymbolKeyHash != fmt.Sprintf("%x", sha256.Sum256([]byte(want[i]))) {
			t.Errorf("symbol %d identity: %s / %s", i, sym.SymbolKeyRaw, sym.SymbolKeyHash)
		}
	}
	if syms[1].Signature != "type Alias = int" {
		t.Errorf("alias signature=%q", syms[1].Signature)
	}
	if syms[4].ReceiverCanonical != "Box" || syms[5].ReceiverCanonical != "Box" {
		t.Error("generic value/pointer method receiver identity changed")
	}
}
