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
	loading      map[string]bool           // package states; prevents recursive deadlock
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
		loading:      make(map[string]bool),
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
	if pkg, ok := oi.cache[path]; ok {
		oi.mu.Unlock()
		return pkg, nil
	}
	if oi.loading[path] {
		oi.mu.Unlock()
		return nil, fmt.Errorf("root-module import cycle detected at %q", path)
	}

	// Never hold the cache mutex while types.Check recursively imports another
	// package. sync.Mutex is not re-entrant; the old implementation deadlocked
	// on ordinary root-module cross-package imports.
	if path == oi.modulePath || strings.HasPrefix(path, oi.modulePath+"/") {
		pkgFiles, exists := oi.pkgFilesMap[path]
		oi.mu.Unlock()
		if !exists {
			return nil, fmt.Errorf("root-module package %q not found in snapshot", path)
		}
		oi.mu.Lock()
		oi.loading[path] = true
		oi.mu.Unlock()
		pkg, err := oi.typeCheckPackage(pkgFiles)
		oi.mu.Lock()
		delete(oi.loading, path)
		if err != nil {
			oi.typeErrors[path] = append(oi.typeErrors[path], err.Error())
		}
		if pkg != nil {
			oi.cache[path] = pkg
			oi.mu.Unlock()
			return pkg, nil
		}
		oi.mu.Unlock()
		return nil, fmt.Errorf("failed to type-check package %q: %w", path, err)
	}
	oi.mu.Unlock()

	// Standard library imports are also performed without the cache lock.
	if oi.stdlibImport != nil {
		if impFrom, ok := oi.stdlibImport.(types.ImporterFrom); ok {
			pkg, err := impFrom.ImportFrom(path, srcDir, mode)
			if err == nil {
				oi.mu.Lock()
				oi.cache[path] = pkg
				oi.mu.Unlock()
				return pkg, nil
			}
		} else {
			pkg, err := oi.stdlibImport.Import(path)
			if err == nil {
				oi.mu.Lock()
				oi.cache[path] = pkg
				oi.mu.Unlock()
				return pkg, nil
			}
		}
	}

	// External dependency not present in snapshot -> unresolved (strictly offline).
	return nil, fmt.Errorf("external dependency %q unresolved (offline mode: no network resolution)", path)
}

func (oi *OfflineImporter) typeCheckPackage(pf *PkgFiles) (*types.Package, error) {
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
		oi.mu.Lock()
		oi.typeErrors[pf.PkgPath] = append(oi.typeErrors[pf.PkgPath], typeErrors...)
		oi.mu.Unlock()
	}
	return pkg, err
}

// GetTypeErrors returns any recorded type errors for a package.
func (oi *OfflineImporter) GetTypeErrors(pkgPath string) []string {
	oi.mu.Lock()
	defer oi.mu.Unlock()
	return oi.typeErrors[pkgPath]
}
