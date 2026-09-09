package relation

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"repolens/internal/codeintel/model"
)

// ExtractionContext provides the symbol registry and file metadata needed for relation extraction.
type ExtractionContext struct {
	Fset           *token.FileSet
	ModulePath     string
	SymbolsByHash  map[string]*model.Symbol
	SymbolsByKey   map[string]*model.Symbol // rawKey -> Symbol
	SymbolsByFile  map[string][]*model.Symbol
	ImportPathMap  map[string]map[string]string // filePath -> (importNameOrAlias -> importPath)
	TypeInfoByFile map[string]*types.Info
}

// ExtractRelations analyzes function/method bodies in parsed AST files to identify relations.
func ExtractRelations(ectx *ExtractionContext, astFile *ast.File, filePath, packagePath string) []*model.SymbolRelation {
	if astFile == nil {
		return nil
	}

	var relations []*model.SymbolRelation
	typeInfo := ectx.TypeInfoByFile[filePath]
	fileSymbols := ectx.SymbolsByFile[filePath]

	// Map AST func declarations to our Symbol objects
	for _, decl := range astFile.Decls {
		funcDecl, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || funcDecl.Body == nil {
			continue
		}

		callerSym := findEnclosingSymbol(ectx.Fset, funcDecl, fileSymbols)
		var callerHash string
		if callerSym != nil {
			callerHash = callerSym.SymbolKeyHash
		}

		// Inspect the function body for calls, references, and selector expressions
		ast.Inspect(funcDecl.Body, func(n ast.Node) bool {
			if n == nil {
				return true
			}

			switch expr := n.(type) {
			case *ast.CallExpr:
				rel := extractCallRelation(ectx, expr, funcDecl, callerHash, filePath, packagePath, typeInfo)
				if rel != nil {
					relations = append(relations, rel)
				}

			case *ast.SelectorExpr:
				// If not already the Fun part of a CallExpr, check if it's a type/value reference
				rel := extractSelectorReference(ectx, expr, callerHash, filePath, packagePath, typeInfo)
				if rel != nil {
					relations = append(relations, rel)
				}
			}

			return true
		})
	}

	return relations
}

func extractCallRelation(ectx *ExtractionContext, call *ast.CallExpr, funcDecl *ast.FuncDecl, callerHash, filePath, packagePath string, typeInfo *types.Info) *model.SymbolRelation {
	pos := ectx.Fset.Position(call.Pos())

	// 1. Check if semantic resolution via go/types is available
	if typeInfo != nil {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if selection, ok := typeInfo.Selections[sel]; ok {
				targetObj := selection.Obj()
				if targetFunc, isMethod := targetObj.(*types.Func); isMethod {
					// Use the declaration receiver, not selection.Recv(). The latter
					// describes the expression receiver and may be a promoted or
					// instantiated type rather than the declaring type.
					receiverName, hasReceiver := declaredReceiverName(selection)
					targetPkgPath := ""
					if targetFunc.Pkg() != nil {
						targetPkgPath = targetFunc.Pkg().Path()
					}
					targetName := targetFunc.Name()
					if hasReceiver {
						rawKey, _ := model.BuildSymbolKey(ectx.ModulePath, targetPkgPath, receiverName, model.SymbolKindMethod, targetName)
						if targetSym, exists := ectx.SymbolsByKey[rawKey]; exists {
							return &model.SymbolRelation{
								FromSymbolKeyHash:   callerHash,
								ToSymbolKeyHash:     targetSym.SymbolKeyHash,
								RelationType:        model.RelationTypeCallCandidate,
								ResolutionKind:      model.ResolutionKindSemantic,
								Confidence:          1.0,
								ReasonCode:          "SEMANTIC_METHOD_SELECTION",
								ReasonDetail:        fmt.Sprintf("Call to method %s on declaring receiver %s resolved via go/types", targetName, receiverName),
								TargetName:          targetName,
								TargetPackagePath:   targetPkgPath,
								TargetQualifiedName: fmt.Sprintf("%s.%s", receiverName, targetName),
								FilePath:            filePath,
								Line:                pos.Line,
								Column:              pos.Column,
							}
						}
					}

					// go/types resolved a method, but the corresponding root-module
					// symbol is absent. Do not downgrade this fact to a syntactic
					// receiver candidate.
					reasonCode := "SEMANTIC_TARGET_NOT_INDEXED"
					if targetPkgPath != "" && targetPkgPath != ectx.ModulePath && !strings.HasPrefix(targetPkgPath, ectx.ModulePath+"/") {
						reasonCode = "UNRESOLVED_EXTERNAL_METHOD"
					}
					qualifiedName := targetName
					if receiverName != "" {
						qualifiedName = receiverName + "." + targetName
					}
					return &model.SymbolRelation{
						FromSymbolKeyHash:   callerHash,
						RelationType:        model.RelationTypeCallCandidate,
						ResolutionKind:      model.ResolutionKindUnresolved,
						Confidence:          0,
						ReasonCode:          reasonCode,
						ReasonDetail:        fmt.Sprintf("go/types resolved method %s but the target symbol is not indexed", qualifiedName),
						TargetName:          targetName,
						TargetPackagePath:   targetPkgPath,
						TargetQualifiedName: qualifiedName,
						FilePath:            filePath,
						Line:                pos.Line,
						Column:              pos.Column,
					}
				}
			}

			if usesObj, ok := typeInfo.Uses[sel.Sel]; ok {
				targetPkgPath := ""
				if usesObj.Pkg() != nil {
					targetPkgPath = usesObj.Pkg().Path()
				}
				targetName := usesObj.Name()
				// Package-level function call
				rawKey, _ := model.BuildSymbolKey(ectx.ModulePath, targetPkgPath, "", model.SymbolKindFunction, targetName)
				if targetSym, exists := ectx.SymbolsByKey[rawKey]; exists {
					return &model.SymbolRelation{
						FromSymbolKeyHash:   callerHash,
						ToSymbolKeyHash:     targetSym.SymbolKeyHash,
						RelationType:        model.RelationTypeCallCandidate,
						ResolutionKind:      model.ResolutionKindSemantic,
						Confidence:          1.0,
						ReasonCode:          "SEMANTIC_PACKAGE_FUNC_CALL",
						ReasonDetail:        fmt.Sprintf("Call to func %s in pkg %s resolved via go/types", targetName, targetPkgPath),
						TargetName:          targetName,
						TargetPackagePath:   targetPkgPath,
						TargetQualifiedName: targetSym.QualifiedName,
						FilePath:            filePath,
						Line:                pos.Line,
						Column:              pos.Column,
					}
				}
			}
		}

		if ident, ok := call.Fun.(*ast.Ident); ok {
			if usesObj, ok := typeInfo.Uses[ident]; ok {
				targetPkgPath := ""
				if usesObj.Pkg() != nil {
					targetPkgPath = usesObj.Pkg().Path()
				}
				targetName := usesObj.Name()
				rawKey, _ := model.BuildSymbolKey(ectx.ModulePath, targetPkgPath, "", model.SymbolKindFunction, targetName)
				if targetSym, exists := ectx.SymbolsByKey[rawKey]; exists {
					return &model.SymbolRelation{
						FromSymbolKeyHash:   callerHash,
						ToSymbolKeyHash:     targetSym.SymbolKeyHash,
						RelationType:        model.RelationTypeCallCandidate,
						ResolutionKind:      model.ResolutionKindSemantic,
						Confidence:          1.0,
						ReasonCode:          "SEMANTIC_DIRECT_FUNC_CALL",
						ReasonDetail:        fmt.Sprintf("Direct call to local func %s resolved via go/types", targetName),
						TargetName:          targetName,
						TargetPackagePath:   targetPkgPath,
						TargetQualifiedName: targetSym.QualifiedName,
						FilePath:            filePath,
						Line:                pos.Line,
						Column:              pos.Column,
					}
				}
			}
		}
	}

	// 2. Syntactic / Unresolved extraction
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		targetName := fun.Sel.Name
		if xIdent, ok := fun.X.(*ast.Ident); ok {
			pkgAlias := xIdent.Name
			// Check if pkgAlias corresponds to an import
			imports := ectx.ImportPathMap[filePath]
			importPath, hasImport := imports[pkgAlias]
			if hasImport {
				// If importPath is inside root module, try syntactic resolution
				if strings.HasPrefix(importPath, ectx.ModulePath) {
					rawKey, _ := model.BuildSymbolKey(ectx.ModulePath, importPath, "", model.SymbolKindFunction, targetName)
					if targetSym, exists := ectx.SymbolsByKey[rawKey]; exists {
						return &model.SymbolRelation{
							FromSymbolKeyHash:   callerHash,
							ToSymbolKeyHash:     targetSym.SymbolKeyHash,
							RelationType:        model.RelationTypeCallCandidate,
							ResolutionKind:      model.ResolutionKindSyntactic,
							Confidence:          0.8,
							ReasonCode:          "SYNTACTIC_IMPORTED_PACKAGE_CALL",
							ReasonDetail:        fmt.Sprintf("Call to %s.%s matched via AST import alias", pkgAlias, targetName),
							TargetName:          targetName,
							TargetPackagePath:   importPath,
							TargetQualifiedName: targetSym.QualifiedName,
							FilePath:            filePath,
							Line:                pos.Line,
							Column:              pos.Column,
						}
					}
				}

				// External unresolved package call
				return &model.SymbolRelation{
					FromSymbolKeyHash:   callerHash,
					ToSymbolKeyHash:     "",
					RelationType:        model.RelationTypeCallCandidate,
					ResolutionKind:      model.ResolutionKindUnresolved,
					Confidence:          0.5,
					ReasonCode:          "UNRESOLVED_EXTERNAL_CALL",
					ReasonDetail:        fmt.Sprintf("Call to %s.%s in external package %s", pkgAlias, targetName, importPath),
					TargetName:          targetName,
					TargetPackagePath:   importPath,
					TargetQualifiedName: fmt.Sprintf("%s.%s", pkgAlias, targetName),
					FilePath:            filePath,
					Line:                pos.Line,
					Column:              pos.Column,
				}
			}

			// Receiver variable call (e.g. `s.DoSomething()`)
			return &model.SymbolRelation{
				FromSymbolKeyHash:   callerHash,
				ToSymbolKeyHash:     "",
				RelationType:        model.RelationTypeCallCandidate,
				ResolutionKind:      model.ResolutionKindSyntactic,
				Confidence:          0.6,
				ReasonCode:          "SYNTACTIC_RECEIVER_CALL",
				ReasonDetail:        fmt.Sprintf("Method call .%s() on receiver variable %s", targetName, pkgAlias),
				TargetName:          targetName,
				TargetPackagePath:   packagePath,
				TargetQualifiedName: fmt.Sprintf("%s.%s", pkgAlias, targetName),
				FilePath:            filePath,
				Line:                pos.Line,
				Column:              pos.Column,
			}
		}

	case *ast.Ident:
		targetName := fun.Name
		// Check same-package function
		rawKey, _ := model.BuildSymbolKey(ectx.ModulePath, packagePath, "", model.SymbolKindFunction, targetName)
		if targetSym, exists := ectx.SymbolsByKey[rawKey]; exists {
			return &model.SymbolRelation{
				FromSymbolKeyHash:   callerHash,
				ToSymbolKeyHash:     targetSym.SymbolKeyHash,
				RelationType:        model.RelationTypeCallCandidate,
				ResolutionKind:      model.ResolutionKindSyntactic,
				Confidence:          0.75,
				ReasonCode:          "SYNTACTIC_SAME_PKG_FUNC_CALL",
				ReasonDetail:        fmt.Sprintf("Call to local function %s", targetName),
				TargetName:          targetName,
				TargetPackagePath:   packagePath,
				TargetQualifiedName: targetSym.QualifiedName,
				FilePath:            filePath,
				Line:                pos.Line,
				Column:              pos.Column,
			}
		}
	}

	return nil
}

func declaredReceiverName(selection *types.Selection) (string, bool) {
	if selection == nil {
		return "", false
	}
	fn, ok := selection.Obj().(*types.Func)
	if !ok {
		return "", false
	}

	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return "", false
	}

	t := types.Unalias(sig.Recv().Type())
	if ptr, ok := t.(*types.Pointer); ok {
		t = types.Unalias(ptr.Elem())
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj() == nil {
		return "", false
	}
	return named.Obj().Name(), true
}

func extractSelectorReference(ectx *ExtractionContext, sel *ast.SelectorExpr, callerHash, filePath, packagePath string, typeInfo *types.Info) *model.SymbolRelation {
	pos := ectx.Fset.Position(sel.Pos())
	targetName := sel.Sel.Name

	if typeInfo != nil {
		if usesObj, ok := typeInfo.Uses[sel.Sel]; ok {
			targetPkgPath := ""
			if usesObj.Pkg() != nil {
				targetPkgPath = usesObj.Pkg().Path()
			}
			// Look for Type, Interface, or Function
			for _, kind := range []model.SymbolKind{model.SymbolKindType, model.SymbolKindInterface, model.SymbolKindFunction} {
				rawKey, _ := model.BuildSymbolKey(ectx.ModulePath, targetPkgPath, "", kind, targetName)
				if targetSym, exists := ectx.SymbolsByKey[rawKey]; exists {
					return &model.SymbolRelation{
						FromSymbolKeyHash:   callerHash,
						ToSymbolKeyHash:     targetSym.SymbolKeyHash,
						RelationType:        model.RelationTypeReference,
						ResolutionKind:      model.ResolutionKindSemantic,
						Confidence:          1.0,
						ReasonCode:          "SEMANTIC_TYPE_OR_SYMBOL_REF",
						ReasonDetail:        fmt.Sprintf("Reference to %s in pkg %s resolved via go/types", targetName, targetPkgPath),
						TargetName:          targetName,
						TargetPackagePath:   targetPkgPath,
						TargetQualifiedName: targetSym.QualifiedName,
						FilePath:            filePath,
						Line:                pos.Line,
						Column:              pos.Column,
					}
				}
			}
		}
	}

	return nil
}

func findEnclosingSymbol(fset *token.FileSet, funcDecl *ast.FuncDecl, fileSymbols []*model.Symbol) *model.Symbol {
	startPos := fset.Position(funcDecl.Pos())
	for _, sym := range fileSymbols {
		if sym.StartLine == startPos.Line && sym.Name == funcDecl.Name.Name {
			return sym
		}
	}
	return nil
}
