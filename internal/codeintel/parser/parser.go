package parser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	"repolens/internal/codeintel/model"
)

// ParsedFile contains the parsed AST along with file metadata and raw content.
type ParsedFile struct {
	CodeFile *model.CodeFile
	AST      *ast.File
	FileSet  *token.FileSet
	Content  []byte
}

// ModuleInfo contains Go module information for a codebase.
type ModuleInfo struct {
	ModulePath string
	RootPath   string
	GoVersion  string
	NestedMods []string
}

// DiscoverModule locates the root go.mod and any nested go.mod files.
func DiscoverModule(rootPath string) (*ModuleInfo, error) {
	rootPath, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	rootGoMod := filepath.Join(rootPath, "go.mod")
	data, err := os.ReadFile(rootGoMod)
	if err != nil {
		return nil, fmt.Errorf("go.mod not found at root (%s): %w", rootGoMod, err)
	}

	mf, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to parse root go.mod: %w", err)
	}

	modulePath := ""
	if mf.Module != nil {
		modulePath = mf.Module.Mod.Path
	}
	goVersion := ""
	if mf.Go != nil {
		goVersion = mf.Go.Version
	}

	info := &ModuleInfo{
		ModulePath: modulePath,
		RootPath:   rootPath,
		GoVersion:  goVersion,
		NestedMods: []string{},
	}

	// Walk to discover nested go.mod
	err = filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" && path != rootGoMod {
			rel, _ := filepath.Rel(rootPath, path)
			info.NestedMods = append(info.NestedMods, rel)
		}
		return nil
	})

	return info, err
}

// ParseRepository walks the repository root and parses all relevant Go files matching the build context.
func ParseRepository(fset *token.FileSet, rootPath string, moduleInfo *ModuleInfo, bctx model.BuildContext) ([]*ParsedFile, []string, error) {
	var warnings []string
	if len(moduleInfo.NestedMods) > 0 {
		warnings = append(warnings, fmt.Sprintf("found %d nested go.mod files (%s); nested modules excluded from root module semantic analysis",
			len(moduleInfo.NestedMods), strings.Join(moduleInfo.NestedMods, ", ")))
	}

	nestedDirs := make(map[string]bool)
	for _, nmod := range moduleInfo.NestedMods {
		nestedDirs[filepath.Dir(filepath.Join(rootPath, nmod))] = true
	}

	var parsedFiles []*ParsedFile

	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}

		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata" {
				return filepath.SkipDir
			}
			// If this directory is inside a nested module, skip semantic parsing
			if nestedDirs[path] && path != rootPath {
				return filepath.SkipDir
			}
			return nil
		}

		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}

		relPath, err := filepath.Rel(rootPath, path)
		if err != nil {
			relPath = path
		}
		relPath = filepath.ToSlash(relPath)

		content, err := os.ReadFile(path)
		if err != nil {
			parsedFiles = append(parsedFiles, &ParsedFile{
				CodeFile: &model.CodeFile{
					Path:                   relPath,
					ParseStatus:            "ERROR",
					ParseError:             fmt.Sprintf("read error: %v", err),
					IncludedByBuildContext: true,
				},
			})
			return nil
		}

		contentHash := sha256.Sum256(content)
		contentHashHex := hex.EncodeToString(contentHash[:])
		lineCount := bytes.Count(content, []byte("\n"))
		if len(content) > 0 && !bytes.HasSuffix(content, []byte("\n")) {
			lineCount++
		}
		isTest := strings.HasSuffix(d.Name(), "_test.go")

		// Determine package path
		dirRel := filepath.Dir(relPath)
		var pkgPath string
		if dirRel == "." || dirRel == "" {
			pkgPath = moduleInfo.ModulePath
		} else {
			pkgPath = moduleInfo.ModulePath + "/" + dirRel
		}

		// Check build constraints
		included := matchesBuildContext(content, bctx)

		codeFile := &model.CodeFile{
			Path:                   relPath,
			PackagePath:            pkgPath,
			ContentHash:            contentHashHex,
			LineCount:              lineCount,
			SizeBytes:              int64(len(content)),
			IsTest:                 isTest,
			IncludedByBuildContext: included,
		}

		if !included {
			codeFile.ParseStatus = "SKIPPED"
			parsedFiles = append(parsedFiles, &ParsedFile{
				CodeFile: codeFile,
				Content:  content,
				FileSet:  fset,
			})
			return nil
		}

		// Parse AST
		astFile, parseErr := parser.ParseFile(fset, path, content, parser.ParseComments)
		if parseErr != nil {
			codeFile.ParseStatus = "ERROR"
			codeFile.ParseError = parseErr.Error()
			codeFile.PackageName = ""
		} else {
			codeFile.ParseStatus = "OK"
			codeFile.PackageName = astFile.Name.Name
		}

		parsedFiles = append(parsedFiles, &ParsedFile{
			CodeFile: codeFile,
			AST:      astFile,
			FileSet:  fset,
			Content:  content,
		})

		return nil
	})

	if err != nil {
		return nil, warnings, fmt.Errorf("failed walking repository: %w", err)
	}

	return parsedFiles, warnings, nil
}

// matchesBuildContext evaluates //go:build and // +build constraint comments.
func matchesBuildContext(content []byte, bctx model.BuildContext) bool {
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			// Stop searching once package declaration is reached
			break
		}
		if strings.HasPrefix(line, "//go:build ") || strings.HasPrefix(line, "// +build ") {
			expr, err := constraint.Parse(line)
			if err != nil {
				continue
			}
			isTag := func(tag string) bool {
				if tag == bctx.GOOS || tag == bctx.GOARCH {
					return true
				}
				for _, t := range bctx.BuildTags {
					if t == tag {
						return true
					}
				}
				return false
			}
			return expr.Eval(isTag)
		}
	}
	return true
}
