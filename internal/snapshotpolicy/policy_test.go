package snapshotpolicy_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"repolens/internal/snapshotpolicy"
)

func TestSharedVisibilityPolicy(t *testing.T) {
	tests := []struct {
		path    string
		allowed bool
	}{
		{path: ".env", allowed: false},
		{path: ".env.staging", allowed: false},
		{path: ".ENV.PRODUCTION", allowed: false},
		{path: "secret.pem", allowed: false},
		{path: "foo.key", allowed: false},
		{path: "secrets.json", allowed: false},
		{path: "src/secret.JSON", allowed: false},
		{path: "src/credential.json", allowed: false},
		{path: "src/credentials.JSON", allowed: false},
		{path: "src/service-account.json", allowed: false},
		{path: "src/service_account.JSON", allowed: false},
		{path: "config.json", allowed: true},
		{path: "dist/main.go", allowed: false},
		{path: "build/main.go", allowed: false},
		{path: "vendor/main.go", allowed: false},
		{path: "src/main.go", allowed: true},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			decisions := []snapshotpolicy.Decision{
				snapshotpolicy.CanMaterialize(test.path, 0),
				snapshotpolicy.CanIndex(test.path, 0),
				snapshotpolicy.CanReadByAgent(test.path, 0),
				snapshotpolicy.CanIssueEvidence(test.path, 0),
			}
			for i, decision := range decisions {
				if decision.Allowed != test.allowed {
					t.Fatalf("policy consumer %d allowed=%v, want %v (decision=%+v)", i, decision.Allowed, test.allowed, decision)
				}
			}
		})
	}
}

func TestManifestDefinesSnapshotAndAgentReadUniverse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo", "snapshot", "executions", "gen-1", "claim", "source")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := snapshotpolicy.NewManifest("snapshot", "commit", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []snapshotpolicy.FileEntry{
		snapshotpolicy.FileEntryFor("src/main.go", []byte("package main\n")),
	})
	if err := snapshotpolicy.WriteManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	loaded, err := snapshotpolicy.LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := loaded.AllowedPaths(), []string{"src/main.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest paths = %v, want %v", got, want)
	}
	for _, test := range []struct {
		path    string
		allowed bool
	}{{"src/main.go", true}, {"dist/main.go", false}, {"README.md", false}, {".env.staging", false}} {
		decision := snapshotpolicy.CanReadFromSnapshot(root, test.path)
		if decision.Allowed != test.allowed {
			t.Errorf("snapshot manifest visibility for %q = %v, want %v (%s)", test.path, decision.Allowed, test.allowed, decision.Reason)
		}
	}
}

func TestImmutableSnapshotFailsClosedWhenManifestIsMissing(t *testing.T) {
	immutableRoot := filepath.Join(t.TempDir(), "repo", "snapshot", "executions", "gen-1", "claim", "source")
	if decision := snapshotpolicy.CanReadFromSnapshot(immutableRoot, "src/main.go"); decision.Allowed {
		t.Fatal("immutable snapshot without a manifest was allowed to expose a file")
	}
	legacyRoot := filepath.Join(t.TempDir(), "repo", "snapshot", "source")
	if decision := snapshotpolicy.CanReadFromSnapshot(legacyRoot, "src/main.go"); !decision.Allowed {
		t.Fatalf("legacy snapshot without a manifest did not use shared policy: %+v", decision)
	}
}
