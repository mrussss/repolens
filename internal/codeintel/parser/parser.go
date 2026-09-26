package parser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
		if d.Type()&os.ModeSymlink != 0 {
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
		if d.Type()&os.ModeSymlink != 0 {
			warnings = append(warnings, fmt.Sprintf("skipped symlink %s; symlink targets are not indexed", path))
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
		included := matchesBuildContext(filepath.Dir(path), d.Name(), bctx)

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
	applyExternalTestPackagePaths(parsedFiles)

	return parsedFiles, warnings, nil
}

// applyExternalTestPackagePaths gives a Go external test package its own
// package identity. A package p_test file in a directory containing package p
// is compiled as a separate package that imports p; retaining the directory's
// base import path would merge its symbols and type-check files with package p.
func applyExternalTestPackagePaths(files []*ParsedFile) {
	productionPackageNames := make(map[string]string)
	for _, file := range files {
		if file == nil || file.CodeFile == nil || file.CodeFile.IsTest || file.AST == nil || file.CodeFile.ParseStatus != "OK" {
			continue
		}
		if _, exists := productionPackageNames[file.CodeFile.PackagePath]; !exists {
			productionPackageNames[file.CodeFile.PackagePath] = file.CodeFile.PackageName
		}
	}

	for _, file := range files {
		if file == nil || file.CodeFile == nil || !file.CodeFile.IsTest || file.AST == nil || file.CodeFile.ParseStatus != "OK" {
			continue
		}
		productionName, exists := productionPackageNames[file.CodeFile.PackagePath]
		if exists && file.CodeFile.PackageName == productionName+"_test" {
			file.CodeFile.PackagePath += "_test"
		}
	}
}

// matchesBuildContext delegates filename and source-comment semantics to the
// standard Go build matcher. MatchFile only reads the file; it never executes
// repository code.
func matchesBuildContext(dir, name string, bctx model.BuildContext) bool {
	ctx := build.Context{
		GOOS:        bctx.GOOS,
		GOARCH:      bctx.GOARCH,
		Compiler:    "gc",
		CgoEnabled:  false,
		BuildTags:   normalizedBuildTags(bctx.BuildTags),
		ReleaseTags: pinnedGoReleaseTags(),
		ToolTags:    targetToolTags(bctx.GOARCH),
	}
	included, err := ctx.MatchFile(dir, name)
	return err == nil && included
}

// pinnedGoReleaseTags makes parser inclusion independent of the Go toolchain
// installed on a worker. v2.2 build identity is based on Go 1.22 semantics;
// bump CurrentParserVersion whenever this supported release-tag set changes.
func pinnedGoReleaseTags() []string {
	tags := make([]string, 0, 22)
	for minor := 1; minor <= 22; minor++ {
		tags = append(tags, fmt.Sprintf("go1.%d", minor))
	}
	return tags
}

func normalizedBuildTags(tags []string) []string {
	normalized := make([]string, 0, len(tags))
	for _, tag := range tags {
		if tag = strings.TrimSpace(tag); tag != "" {
			normalized = append(normalized, tag)
		}
	}
	sort.Strings(normalized)
	return normalized
}

// targetToolTags derives compiler and architecture tags from the requested
// target, without inheriting host toolchain experiments or feature settings.
// BuildContext currently represents the default target feature level for each
// architecture.
func targetToolTags(goarch string) []string {
	tags := []string{"gc"}
	switch goarch {
	case "386":
		tags = append(tags, "386.sse2")
	case "amd64":
		tags = append(tags, "amd64.v1")
	case "arm":
		// The Go 1.22 cross-compilation default is GOARM=7, which
		// defines the compatible arm.5, arm.6, and arm.7 tags.
		tags = append(tags, "arm.5", "arm.6", "arm.7")
	case "mips", "mipsle":
		tags = append(tags, goarch+".hardfloat")
	case "mips64", "mips64le":
		tags = append(tags, goarch+".hardfloat")
	case "ppc64":
		tags = append(tags, "ppc64.power8")
	case "ppc64le":
		tags = append(tags, "ppc64le.power8")
	}
	return tags
}
