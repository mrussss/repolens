package parser

import (
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"repolens/internal/codeintel/model"
)

func TestParseRepositorySkipsSymlinkAndRecordsWarning(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/test\ngo 1.22\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real.go"), []byte("package test\n\nfunc Real() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(target, []byte("package outside\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "linked.go")); err != nil {
		t.Skipf("symlink is unavailable: %v", err)
	}

	moduleInfo, err := DiscoverModule(root)
	if err != nil {
		t.Fatal(err)
	}
	files, warnings, err := ParseRepository(token.NewFileSet(), root, moduleInfo, model.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].CodeFile.Path != "real.go" {
		t.Fatalf("parsed files = %+v, want only real.go", files)
	}
	foundWarning := false
	for _, warning := range warnings {
		if strings.HasSuffix(warning, "skipped symlink "+filepath.Join(root, "linked.go")+"; symlink targets are not indexed") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatalf("warnings = %v, want symlink warning", warnings)
	}
}

func TestParseRepositoryHonorsFilenameBuildConstraints(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/platform\ngo 1.22\n"), 0600); err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{
		"plain.go":             true,
		"go_release.go":        true,
		"gc_compiler.go":       true,
		"foo_linux.go":         true,
		"foo_windows.go":       false,
		"foo_amd64.go":         true,
		"foo_arm64.go":         false,
		"foo_linux_amd64.go":   true,
		"foo_windows_amd64.go": false,
		"both_linux.go":        false,
		"future_release.go":    false,
	}
	for name := range files {
		content := "package platform\n"
		if name == "both_linux.go" {
			content = "//go:build linux && custom\n\npackage platform\n"
		} else if name == "go_release.go" {
			content = "//go:build go1.1\n\npackage platform\n"
		} else if name == "gc_compiler.go" {
			content = "//go:build gc\n\npackage platform\n"
		} else if name == "future_release.go" {
			content = "//go:build go1.23\n\npackage platform\n"
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	moduleInfo, err := DiscoverModule(root)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, err := ParseRepository(token.NewFileSet(), root, moduleInfo, model.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(parsed))
	for _, file := range parsed {
		got[file.CodeFile.Path] = file.CodeFile.IncludedByBuildContext
	}
	for name, want := range files {
		if got[name] != want {
			t.Errorf("%s included=%t, want %t", name, got[name], want)
		}
	}

	withTag := model.DefaultBuildContext()
	withTag.BuildTags = []string{"custom"}
	parsedWithTag, _, err := ParseRepository(token.NewFileSet(), root, moduleInfo, withTag)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range parsedWithTag {
		if file.CodeFile.Path == "both_linux.go" && !file.CodeFile.IncludedByBuildContext {
			t.Fatal("both_linux.go was excluded despite matching filename and //go:build constraints")
		}
	}
}

func TestMatchesBuildContextUsesTargetToolTagsNotHostTags(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"cgo.go":             "//go:build cgo\n\npackage target\n",
		"not_cgo.go":         "//go:build !cgo\n\npackage target\n",
		"386_sse2.go":        "//go:build 386.sse2\n\npackage target\n",
		"386_387.go":         "//go:build 386.387\n\npackage target\n",
		"amd64_feature.go":   "//go:build amd64.v1\n\npackage target\n",
		"arm_5.go":           "//go:build arm.5\n\npackage target\n",
		"arm_6.go":           "//go:build arm.6\n\npackage target\n",
		"arm_7.go":           "//go:build arm.7\n\npackage target\n",
		"arm_8.go":           "//go:build arm.8\n\npackage target\n",
		"arm64_feature.go":   "//go:build arm64.v8.0\n\npackage target\n",
		"riscv64_feature.go": "//go:build riscv64.rva20u64\n\npackage target\n",
		"mips_hardfloat.go":  "//go:build mips.hardfloat\n\npackage target\n",
		"ppc64_power8.go":    "//go:build ppc64.power8\n\npackage target\n",
		"ppc64_power9.go":    "//go:build ppc64.power9\n\npackage target\n",
		"custom_feature.go":  "//go:build target_custom\n\npackage target\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	targets := []struct {
		name string
		ctx  model.BuildContext
		want map[string]bool
	}{
		{
			name: "amd64",
			ctx:  model.BuildContext{GOOS: "linux", GOARCH: "amd64"},
			want: map[string]bool{
				"cgo.go": false, "not_cgo.go": true, "386_sse2.go": false, "386_387.go": false,
				"amd64_feature.go": true, "arm_5.go": false, "arm_6.go": false, "arm_7.go": false, "arm_8.go": false,
				"arm64_feature.go": false, "riscv64_feature.go": false, "mips_hardfloat.go": false,
				"ppc64_power8.go": false, "ppc64_power9.go": false, "custom_feature.go": false,
			},
		},
		{
			name: "arm64",
			ctx:  model.BuildContext{GOOS: "linux", GOARCH: "arm64"},
			want: map[string]bool{
				"cgo.go": false, "not_cgo.go": true, "386_sse2.go": false, "386_387.go": false,
				"amd64_feature.go": false, "arm_5.go": false, "arm_6.go": false, "arm_7.go": false, "arm_8.go": false,
				"arm64_feature.go": false, "riscv64_feature.go": false, "mips_hardfloat.go": false,
				"ppc64_power8.go": false, "ppc64_power9.go": false, "custom_feature.go": false,
			},
		},
		{
			name: "386",
			ctx:  model.BuildContext{GOOS: "linux", GOARCH: "386"},
			want: map[string]bool{"386_sse2.go": true, "386_387.go": false},
		},
		{
			name: "arm",
			ctx:  model.BuildContext{GOOS: "linux", GOARCH: "arm"},
			want: map[string]bool{"arm_5.go": true, "arm_6.go": true, "arm_7.go": true, "arm_8.go": false},
		},
		{
			name: "mips",
			ctx:  model.BuildContext{GOOS: "linux", GOARCH: "mips"},
			want: map[string]bool{"mips_hardfloat.go": true},
		},
		{
			name: "ppc64",
			ctx:  model.BuildContext{GOOS: "linux", GOARCH: "ppc64"},
			want: map[string]bool{"ppc64_power8.go": true, "ppc64_power9.go": false},
		},
	}
	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			for name, want := range target.want {
				got := matchesBuildContext(dir, name, target.ctx)
				if got != want {
					t.Errorf("target %s: %s included=%t, want %t", target.name, name, got, want)
				}
			}
		})
	}
	withCustom := model.BuildContext{GOOS: "linux", GOARCH: "arm64", BuildTags: []string{" target_custom ", "", "target_custom"}}
	if !matchesBuildContext(dir, "custom_feature.go", withCustom) {
		t.Fatal("explicit custom build tag was not applied")
	}
}
