package importer

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/token"
	"go/types"
	"strings"
	"sync"
)

// PkgFiles represents a collection of AST files belonging to a package.
type PkgFiles struct {
	PkgPath string
	PkgName string
	Files   []*ast.File
}

// OfflineImporter implements types.Importer and types.ImporterFrom for offline analysis.
type OfflineImporter struct {
	mu           sync.Mutex
	fset         *token.FileSet
	modulePath   string
	pkgFilesMap  map[string]*PkgFiles      // pkgPath -> PkgFiles
	cache        map[string]*types.Package // pkgPath -> *types.Package
	typeErrors   map[string][]string       // pkgPath -> errors
	stdlibImport types.Importer
}

// NewOfflineImporter constructs a new OfflineImporter.
func NewOfflineImporter(fset *token.FileSet, modulePath string, pkgFilesMap map[string]*PkgFiles) *OfflineImporter {
	return &OfflineImporter{
		fset:         fset,
		modulePath:   modulePath,
		pkgFilesMap:  pkgFilesMap,
		cache:        make(map[string]*types.Package),
		typeErrors:   make(map[string][]string),
		stdlibImport: importer.Default(),
	}
}

// Import imports the package with the given import path.
func (oi *OfflineImporter) Import(path string) (*types.Package, error) {
	return oi.ImportFrom(path, "", 0)
}

// ImportFrom imports the package with the given import path from dir.
func (oi *OfflineImporter) ImportFrom(path, srcDir string, mode types.ImportMode) (*types.Package, error) {
	oi.mu.Lock()
	defer oi.mu.Unlock()

	// 1. Check cache
	if pkg, ok := oi.cache[path]; ok {
		return pkg, nil
	}

	// 2. Check if it's within the root module
	if strings.HasPrefix(path, oi.modulePath) {
		pkgFiles, exists := oi.pkgFilesMap[path]
		if !exists {
			return nil, fmt.Errorf("root-module package %q not found in snapshot", path)
		}

		// Type-check this package in offline mode
		pkg, err := oi.typeCheckPackageLocked(pkgFiles)
		if err != nil {
			// Record error but return package if partially created
			oi.typeErrors[path] = append(oi.typeErrors[path], err.Error())
		}
		if pkg != nil {
			oi.cache[path] = pkg
			return pkg, nil
		}
		return nil, fmt.Errorf("failed to type-check package %q: %w", path, err)
	}

	// 3. Check if it's a standard library package
	if oi.stdlibImport != nil {
		if impFrom, ok := oi.stdlibImport.(types.ImporterFrom); ok {
			pkg, err := impFrom.ImportFrom(path, srcDir, mode)
			if err == nil {
				oi.cache[path] = pkg
				return pkg, nil
			}
		} else {
			pkg, err := oi.stdlibImport.Import(path)
			if err == nil {
				oi.cache[path] = pkg
				return pkg, nil
			}
		}
	}

	// 4. External dependency not present in snapshot -> unresolved (strictly offline)
	return nil, fmt.Errorf("external dependency %q unresolved (offline mode: no network resolution)", path)
}

// typeCheckPackageLocked typechecks a root-module package while holding the mutex.
func (oi *OfflineImporter) typeCheckPackageLocked(pf *PkgFiles) (*types.Package, error) {
	var typeErrors []string
	conf := types.Config{
		Importer: oi,
		Error: func(err error) {
			typeErrors = append(typeErrors, err.Error())
		},
		IgnoreFuncBodies: false,
	}

	info := &types.Info{}
	pkg, err := conf.Check(pf.PkgPath, oi.fset, pf.Files, info)
	if err != nil && len(typeErrors) > 0 {
		oi.typeErrors[pf.PkgPath] = append(oi.typeErrors[pf.PkgPath], typeErrors...)
	}
	return pkg, err
}

// GetTypeErrors returns any recorded type errors for a package.
func (oi *OfflineImporter) GetTypeErrors(pkgPath string) []string {
	oi.mu.Lock()
	defer oi.mu.Unlock()
	return oi.typeErrors[pkgPath]
}
