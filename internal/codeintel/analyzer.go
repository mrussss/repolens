package codeintel

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"repolens/internal/codeintel/importer"
	"repolens/internal/codeintel/parser"
	"repolens/internal/codeintel/quality"
	"repolens/internal/codeintel/relation"
	"repolens/internal/codeintel/symbol"
	"repolens/internal/codeintel/tests"
)

// Analyzer executes the end-to-end code intelligence analysis pipeline.
type Analyzer struct{}

// NewAnalyzer creates a new Analyzer instance.
func NewAnalyzer() *Analyzer {
	return &Analyzer{}
}

// Analyze runs the complete parsing, symbol extraction, type-checking, relation extraction, and test discovery pipeline.
func (a *Analyzer) Analyze(ctx context.Context, rootPath string, bctx BuildContext) (*AnalysisResult, error) {
	// 1. Discover module
	modInfo, err := parser.DiscoverModule(rootPath)
	if err != nil {
		return nil, fmt.Errorf("failed discovering module at %s: %w", rootPath, err)
	}

	fset := token.NewFileSet()

	// 2. Parse repository files
	parsedFiles, warnings, err := parser.ParseRepository(fset, rootPath, modInfo, bctx)
	if err != nil {
		return nil, fmt.Errorf("failed parsing repository: %w", err)
	}

	var codeFiles []*CodeFile
	var allSymbols []*Symbol
	symbolsByHash := make(map[string]*Symbol)
	symbolsByKey := make(map[string]*Symbol)
	symbolsByFile := make(map[string][]*Symbol)
	importPathMap := make(map[string]map[string]string)
	pkgFilesMap := make(map[string]*importer.PkgFiles)
	testFilesMap := make(map[string]*ast.File)

	for _, pf := range parsedFiles {
		codeFiles = append(codeFiles, pf.CodeFile)
		if pf.AST == nil || pf.CodeFile.ParseStatus != "OK" {
			continue
		}

		filePath := pf.CodeFile.Path
		pkgPath := pf.CodeFile.PackagePath

		if pf.CodeFile.IsTest {
			testFilesMap[filePath] = pf.AST
		}

		// Group files by package for type-checking
		if pkgFilesMap[pkgPath] == nil {
			pkgFilesMap[pkgPath] = &importer.PkgFiles{
				PkgPath: pkgPath,
				PkgName: pf.CodeFile.PackageName,
				Files:   []*ast.File{},
			}
		}
		pkgFilesMap[pkgPath].Files = append(pkgFilesMap[pkgPath].Files, pf.AST)

		// Record imports for this file
		importPathMap[filePath] = make(map[string]string)
		for _, imp := range pf.AST.Imports {
			impPath := strings.Trim(imp.Path.Value, `"`)
			var alias string
			if imp.Name != nil {
				alias = imp.Name.Name
			} else {
				// Default alias is last segment of import path
				parts := strings.Split(impPath, "/")
				alias = parts[len(parts)-1]
			}
			importPathMap[filePath][alias] = impPath
		}

		// 3. Extract symbols
		syms := symbol.ExtractSymbols(fset, pf.AST, filePath, modInfo.ModulePath, pkgPath, pf.Content)
		for _, s := range syms {
			allSymbols = append(allSymbols, s)
			symbolsByHash[s.SymbolKeyHash] = s
			symbolsByKey[s.SymbolKeyRaw] = s
			symbolsByFile[filePath] = append(symbolsByFile[filePath], s)
		}
	}

	// 4. Offline Type-Checking
	offlineImp := importer.NewOfflineImporter(fset, modInfo.ModulePath, pkgFilesMap)
	typeInfoByFile := make(map[string]*types.Info)
	pkgTotal := len(pkgFilesMap)
	pkgTypechecked := 0
	pkgFailed := 0

	for pkgPath, pf := range pkgFilesMap {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		typeInfo := &types.Info{
			Types:      make(map[ast.Expr]types.TypeAndValue),
			Defs:       make(map[*ast.Ident]types.Object),
			Uses:       make(map[*ast.Ident]types.Object),
			Selections: make(map[*ast.SelectorExpr]*types.Selection),
		}

		conf := types.Config{
			Importer: offlineImp,
			Error: func(typeErr error) {
				// Non-fatal type error, recorded in importer
			},
			IgnoreFuncBodies: false,
		}

		_, err := conf.Check(pkgPath, fset, pf.Files, typeInfo)
		if err != nil {
			pkgFailed++
			typeErrs := offlineImp.GetTypeErrors(pkgPath)
			if len(typeErrs) > 0 {
				warnings = append(warnings, fmt.Sprintf("package %s type-check degraded: %s", pkgPath, typeErrs[0]))
			}
		} else {
			pkgTypechecked++
		}

		for _, astFile := range pf.Files {
			// Find filePath for this astFile
			for _, pfItem := range parsedFiles {
				if pfItem.AST == astFile {
					typeInfoByFile[pfItem.CodeFile.Path] = typeInfo
					break
				}
			}
		}
	}

	// 5. Extract Relations
	ectx := &relation.ExtractionContext{
		Fset:           fset,
		ModulePath:     modInfo.ModulePath,
		SymbolsByHash:  symbolsByHash,
		SymbolsByKey:   symbolsByKey,
		SymbolsByFile:  symbolsByFile,
		ImportPathMap:  importPathMap,
		TypeInfoByFile: typeInfoByFile,
	}

	var allRelations []*SymbolRelation
	for _, pf := range parsedFiles {
		if pf.AST == nil || pf.CodeFile.ParseStatus != "OK" {
			continue
		}
		rels := relation.ExtractRelations(ectx, pf.AST, pf.CodeFile.Path, pf.CodeFile.PackagePath)
		allRelations = append(allRelations, rels...)
	}

	// 6. Discover Related Tests
	tctx := &tests.TestDiscoveryContext{
		Fset:           fset,
		Symbols:        allSymbols,
		SymbolsByHash:  symbolsByHash,
		TestFiles:      testFilesMap,
		TypeInfoByFile: typeInfoByFile,
	}
	relatedTests := tests.DiscoverRelatedTests(tctx)

	// 7. Compute Quality
	q := quality.ComputeQuality(codeFiles, allSymbols, allRelations, pkgTotal, pkgTypechecked, pkgFailed, warnings)

	return &AnalysisResult{
		ModulePath:   modInfo.ModulePath,
		BuildContext: bctx,
		Files:        codeFiles,
		Symbols:      allSymbols,
		Relations:    allRelations,
		RelatedTests: relatedTests,
		Quality:      q,
	}, nil
}
