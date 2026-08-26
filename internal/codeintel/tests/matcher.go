package tests

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"repolens/internal/codeintel/model"
)

// TestDiscoveryContext holds symbols and ASTs to correlate production code with test functions.
type TestDiscoveryContext struct {
	Fset           *token.FileSet
	Symbols        []*model.Symbol
	SymbolsByHash  map[string]*model.Symbol
	TestFiles      map[string]*ast.File
	TypeInfoByFile map[string]*types.Info
}

// DiscoverRelatedTests searches for test functions related to production symbols across four signal levels.
func DiscoverRelatedTests(tctx *TestDiscoveryContext) []*model.RelatedTestDiscovery {
	var discoveries []*model.RelatedTestDiscovery

	// 1. Separate production symbols and test symbols
	var prodSymbols []*model.Symbol
	var testSymbols []*model.Symbol

	for _, sym := range tctx.Symbols {
		if strings.HasSuffix(sym.FilePath, "_test.go") || strings.HasPrefix(sym.Name, "Test") || strings.HasPrefix(sym.Name, "Benchmark") || strings.HasPrefix(sym.Name, "Example") {
			testSymbols = append(testSymbols, sym)
		} else {
			prodSymbols = append(prodSymbols, sym)
		}
	}

	if len(testSymbols) == 0 || len(prodSymbols) == 0 {
		return nil
	}

	// Index test symbols by name and package
	type testFuncInfo struct {
		symbol   *model.Symbol
		astFunc  *ast.FuncDecl
		filePath string
		typeInfo *types.Info
	}
	var testFuncs []testFuncInfo

	for _, testSym := range testSymbols {
		astFile := tctx.TestFiles[testSym.FilePath]
		if astFile == nil {
			continue
		}
		for _, decl := range astFile.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == testSym.Name {
				testFuncs = append(testFuncs, testFuncInfo{
					symbol:   testSym,
					astFunc:  fd,
					filePath: testSym.FilePath,
					typeInfo: tctx.TypeInfoByFile[testSym.FilePath],
				})
			}
		}
	}

	// For each production symbol, find matching test functions
	for _, prodSym := range prodSymbols {
		seenTests := make(map[string]bool)

		for _, tf := range testFuncs {
			if seenTests[tf.symbol.SymbolKeyHash] {
				continue
			}

			// Signal 1: DIRECT_SEMANTIC_USAGE
			if hasDirectSemanticUsage(tf.astFunc, tf.typeInfo, prodSym) {
				discoveries = append(discoveries, &model.RelatedTestDiscovery{
					TargetSymbolKeyHash: prodSym.SymbolKeyHash,
					TargetSymbolName:    prodSym.Name,
					TestSymbolKeyHash:   tf.symbol.SymbolKeyHash,
					TestSymbolName:      tf.symbol.Name,
					TestFilePath:        tf.filePath,
					ReasonCode:          model.TestReasonDirectSemantic,
					ResolutionKind:      model.ResolutionKindSemantic,
					Confidence:          1.0,
					Explanation:         fmt.Sprintf("Test %s directly calls or references %s resolved via types", tf.symbol.Name, prodSym.QualifiedName),
					TestLine:            tf.symbol.StartLine,
				})
				seenTests[tf.symbol.SymbolKeyHash] = true
				continue
			}

			// Signal 2: DIRECT_SYNTACTIC_USAGE
			if hasDirectSyntacticUsage(tf.astFunc, prodSym) {
				discoveries = append(discoveries, &model.RelatedTestDiscovery{
					TargetSymbolKeyHash: prodSym.SymbolKeyHash,
					TargetSymbolName:    prodSym.Name,
					TestSymbolKeyHash:   tf.symbol.SymbolKeyHash,
					TestSymbolName:      tf.symbol.Name,
					TestFilePath:        tf.filePath,
					ReasonCode:          model.TestReasonDirectSyntactic,
					ResolutionKind:      model.ResolutionKindSyntactic,
					Confidence:          0.85,
					Explanation:         fmt.Sprintf("Test %s contains AST identifier reference matching %s", tf.symbol.Name, prodSym.Name),
					TestLine:            tf.symbol.StartLine,
				})
				seenTests[tf.symbol.SymbolKeyHash] = true
				continue
			}

			// Signal 3: NAME_MATCH
			if isNameMatch(tf.symbol.Name, prodSym) {
				discoveries = append(discoveries, &model.RelatedTestDiscovery{
					TargetSymbolKeyHash: prodSym.SymbolKeyHash,
					TargetSymbolName:    prodSym.Name,
					TestSymbolKeyHash:   tf.symbol.SymbolKeyHash,
					TestSymbolName:      tf.symbol.Name,
					TestFilePath:        tf.filePath,
					ReasonCode:          model.TestReasonNameMatch,
					ResolutionKind:      model.ResolutionKindHeuristic,
					Confidence:          0.70,
					Explanation:         fmt.Sprintf("Test name %s follows standard Go test naming convention for %s", tf.symbol.Name, prodSym.Name),
					TestLine:            tf.symbol.StartLine,
				})
				seenTests[tf.symbol.SymbolKeyHash] = true
				continue
			}

			// Signal 4: SAME_PACKAGE
			if isSamePackage(tf.symbol.PackagePath, prodSym.PackagePath) {
				discoveries = append(discoveries, &model.RelatedTestDiscovery{
					TargetSymbolKeyHash: prodSym.SymbolKeyHash,
					TargetSymbolName:    prodSym.Name,
					TestSymbolKeyHash:   tf.symbol.SymbolKeyHash,
					TestSymbolName:      tf.symbol.Name,
					TestFilePath:        tf.filePath,
					ReasonCode:          model.TestReasonSamePackage,
					ResolutionKind:      model.ResolutionKindHeuristic,
					Confidence:          0.40,
					Explanation:         fmt.Sprintf("Test %s resides in the same package (%s) as %s", tf.symbol.Name, prodSym.PackageName, prodSym.Name),
					TestLine:            tf.symbol.StartLine,
				})
				seenTests[tf.symbol.SymbolKeyHash] = true
				continue
			}
		}
	}

	return discoveries
}

func hasDirectSemanticUsage(funcDecl *ast.FuncDecl, typeInfo *types.Info, prodSym *model.Symbol) bool {
	if funcDecl.Body == nil || typeInfo == nil {
		return false
	}
	found := false
	ast.Inspect(funcDecl.Body, func(n ast.Node) bool {
		if found || n == nil {
			return !found
		}
		if ident, ok := n.(*ast.Ident); ok {
			if obj, ok := typeInfo.Uses[ident]; ok && obj != nil {
				if obj.Name() == prodSym.Name {
					if obj.Pkg() != nil && obj.Pkg().Path() == prodSym.PackagePath {
						found = true
						return false
					}
				}
			}
		}
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if selection, ok := typeInfo.Selections[sel]; ok && selection != nil {
				if selection.Obj().Name() == prodSym.Name {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

func hasDirectSyntacticUsage(funcDecl *ast.FuncDecl, prodSym *model.Symbol) bool {
	if funcDecl.Body == nil {
		return false
	}
	found := false
	ast.Inspect(funcDecl.Body, func(n ast.Node) bool {
		if found || n == nil {
			return !found
		}
		if ident, ok := n.(*ast.Ident); ok {
			if ident.Name == prodSym.Name {
				found = true
				return false
			}
		}
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if sel.Sel.Name == prodSym.Name {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func isNameMatch(testName string, prodSym *model.Symbol) bool {
	prefixes := []string{"Test", "Benchmark", "Example"}
	for _, pfx := range prefixes {
		if strings.HasPrefix(testName, pfx) {
			rest := strings.TrimPrefix(testName, pfx)
			// Matches TestFoo, TestFoo_Bar, TestService_Foo
			if rest == prodSym.Name {
				return true
			}
			if strings.HasPrefix(rest, prodSym.Name+"_") || strings.HasPrefix(rest, prodSym.Name) {
				return true
			}
			if prodSym.ReceiverCanonical != "" {
				target := prodSym.ReceiverCanonical + "_" + prodSym.Name
				if rest == target || strings.HasPrefix(rest, target+"_") {
					return true
				}
				target2 := prodSym.ReceiverCanonical + prodSym.Name
				if rest == target2 {
					return true
				}
			}
		}
	}
	return false
}

func isSamePackage(testPkgPath, prodPkgPath string) bool {
	cleanTest := strings.TrimSuffix(testPkgPath, "_test")
	cleanProd := strings.TrimSuffix(prodPkgPath, "_test")
	return cleanTest == cleanProd
}
