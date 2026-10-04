package parser

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"repolens/internal/snapshotpolicy"
)

func TestModuleDiscoveryCanonicalBoundary(t *testing.T) {
	for _, kind := range []string{"regular", "outside-link", "inside-link", "parent-link", "root-link", "fifo", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			canonical := []byte("module example.com/canonical\ngo 1.22\n")
			path := filepath.Join(root, "go.mod")
			switch kind {
			case "regular":
				if err := os.WriteFile(path, canonical, 0600); err != nil {
					t.Fatal(err)
				}
			case "outside-link", "inside-link":
				target := filepath.Join(root, "actual.mod")
				if kind == "outside-link" {
					target = filepath.Join(t.TempDir(), "outside.mod")
				}
				if err := os.WriteFile(target, []byte("module example.com/forbidden\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "parent-link":
				target := t.TempDir()
				if err := os.WriteFile(filepath.Join(target, "go.mod"), canonical, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
					t.Fatal(err)
				}
				if _, err := allowedPath(root, "linked/go.mod", []string{"linked/go.mod"}); err == nil {
					t.Fatal("accepted symlink parent")
				}
				return
			case "root-link":
				target := t.TempDir()
				if err := os.WriteFile(filepath.Join(target, "go.mod"), canonical, 0600); err != nil {
					t.Fatal(err)
				}
				root = filepath.Join(t.TempDir(), "source")
				if err := os.Symlink(target, root); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(path, []byte("module example.com/forbidden\n//"+strings.Repeat("x", maxModuleBytes)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			info, err := DiscoverModuleWithAllowedFiles(root, []string{"go.mod"})
			if kind == "regular" {
				if err != nil || info == nil || info.ModulePath != "example.com/canonical" {
					t.Fatalf("legal module: %+v %v", info, err)
				}
			} else if err == nil || info != nil {
				t.Fatalf("non-canonical metadata participated in discovery: %+v %v", info, err)
			}
		})
	}
}

func TestModuleDiscoveryHonorsManifestAndNestedAllowlist(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	mod := []byte("module example.com/canonical\ngo 1.22\n")
	for _, name := range []string{"go.mod", "nested/go.mod"} {
		if err := os.WriteFile(filepath.Join(root, name), mod, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := DiscoverModuleWithAllowedFiles(root, []string{}); err == nil {
		t.Fatal("module missing from allowlist was read")
	}
	info, err := DiscoverModuleWithAllowedFiles(root, []string{"go.mod"})
	if err != nil || len(info.NestedMods) != 0 {
		t.Fatalf("excluded nested metadata affected analysis: %+v %v", info, err)
	}
	info, err = DiscoverModuleWithAllowedFiles(root, []string{"go.mod", "nested/go.mod"})
	if err != nil || len(info.NestedMods) != 1 {
		t.Fatalf("canonical nested module was not discovered: %+v %v", info, err)
	}
	if err := os.Remove(filepath.Join(root, "nested/go.mod")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "go.mod"), filepath.Join(root, "nested/go.mod")); err != nil {
		t.Fatal(err)
	}
	info, err = DiscoverModuleWithAllowedFiles(root, []string{"go.mod", "nested/go.mod"})
	if err != nil || len(info.NestedMods) != 0 {
		t.Fatalf("linked nested metadata affected analysis: %+v %v", info, err)
	}
	manifest := snapshotpolicy.NewManifest("snapshot", "commit", strings.Repeat("a", 64), []snapshotpolicy.FileEntry{})
	if err := snapshotpolicy.WriteManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	for _, allowlist := range [][]string{nil, {"go.mod"}} {
		if _, err := DiscoverModuleWithAllowedFiles(root, allowlist); err == nil {
			t.Fatal("read go.mod absent from manifest")
		}
	}
}
