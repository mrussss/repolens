package indexing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCloneDiskLimitCountsIgnoredFilesAndCleansStaging(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")
	for path, content := range map[string]string{
		".git/objects/pack/packed-data": "12345678",
		"node_modules/cache.bin":        "abcdefgh",
		"src/main.go":                   "ok",
	} {
		fullPath := filepath.Join(staging, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	cloner := NewSafeGitClonerWithDiskLimit(nil, 16, 0)
	err := cloner.checkCloneDiskLimit(staging)
	if !errors.Is(err, ErrRepositoryCloneSizeLimit) {
		t.Fatalf("size check error = %v, want REPOSITORY_CLONE_SIZE_LIMIT", err)
	}
	if _, statErr := os.Stat(staging); !os.IsNotExist(statErr) {
		t.Fatalf("over-limit staging directory still exists: stat error=%v", statErr)
	}
}

func TestCloneDiskLimitAllowsExactBoundary(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "source.bin"), []byte("12345678"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewSafeGitClonerWithDiskLimit(nil, 8, 0).checkCloneDiskLimit(staging); err != nil {
		t.Fatalf("exact clone-size boundary rejected: %v", err)
	}
}

func TestActiveCloneDiskGuardStopsGrowingTree(t *testing.T) {
	installFakeGit(t, `#!/bin/sh
set -eu
target="$2"
mkdir -p "$target"
while :; do
  printf '%4096s' x >> "$target/growing.data"
  sleep 0.01
done
`)
	target := filepath.Join(t.TempDir(), "staging")
	const limit = int64(32 << 10)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := NewSafeGitClonerWithDiskLimit(nil, limit, time.Second).runGitCommandWithDiskGuard(ctx, target, "clone", target)
	if !errors.Is(err, ErrRepositoryCloneSizeLimit) {
		t.Fatalf("growing clone error = %v, want REPOSITORY_CLONE_SIZE_LIMIT", err)
	}
	var limitErr *CloneDiskLimitError
	if !errors.As(err, &limitErr) || limitErr.LimitBytes != limit || limitErr.SizeBytes <= limit {
		t.Fatalf("growing clone limit error = %#v, want measured over-limit size", limitErr)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("over-limit clone staging still exists: stat error=%v", statErr)
	}
}

func TestActiveCloneDiskGuardAllowsUnderLimit(t *testing.T) {
	installFakeGit(t, `#!/bin/sh
set -eu
target="$2"
mkdir -p "$target"
printf 'small clone payload' > "$target/source.txt"
`)
	target := filepath.Join(t.TempDir(), "staging")
	out, err := NewSafeGitClonerWithDiskLimit(nil, 1024, time.Second).runGitCommandWithDiskGuard(context.Background(), target, "clone", target)
	if err != nil {
		t.Fatalf("under-limit clone failed: %v; output=%s", err, out)
	}
	if contents, err := os.ReadFile(filepath.Join(target, "source.txt")); err != nil || string(contents) != "small clone payload" {
		t.Fatalf("under-limit clone output = %q err=%v", contents, err)
	}
}

func TestCloneDiskGuardFinalCheckCatchesFastOvershoot(t *testing.T) {
	installFakeGit(t, `#!/bin/sh
set -eu
target="$2"
mkdir -p "$target"
printf '%16384s' x > "$target/fast-overshoot.data"
`)
	target := filepath.Join(t.TempDir(), "staging")
	const limit = int64(8 << 10)
	out, err := NewSafeGitClonerWithDiskLimit(nil, limit, time.Second).runGitCommandWithDiskGuard(context.Background(), target, "clone", target)
	if !errors.Is(err, ErrRepositoryCloneSizeLimit) {
		t.Fatalf("fast overshoot error = %v, want REPOSITORY_CLONE_SIZE_LIMIT; output=%s", err, out)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("fast over-limit clone staging still exists: stat error=%v", statErr)
	}
}

func TestCloneDiskGuardDoesNotFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges")
	}
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(staging, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "large-outside-file")
	if err := os.WriteFile(outside, make([]byte, 32<<10), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(staging, "outside-link")); err != nil {
		t.Fatal(err)
	}
	if err := NewSafeGitClonerWithDiskLimit(nil, 1024, 0).checkCloneDiskLimit(staging); err != nil {
		t.Fatalf("disk guard followed a symlink outside staging: %v", err)
	}
}

func installFakeGit(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake git script requires a POSIX shell")
	}
	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	if err := os.WriteFile(gitPath, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
