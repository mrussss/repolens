package main

import "testing"

func TestResolveDatasetRoot(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{version: "v1", want: "testdata/realbench/v1"},
		{version: "v2", want: "testdata/realbench/v2"},
		{version: "realbench-v2", want: "testdata/realbench/v2"},
	}
	for _, test := range tests {
		got, err := resolveDatasetRoot(test.version, "")
		if err != nil || got != test.want {
			t.Fatalf("resolveDatasetRoot(%q) = %q, %v; want %q", test.version, got, err, test.want)
		}
	}
	if got, err := resolveDatasetRoot("v2", "/custom/dataset"); err != nil || got != "/custom/dataset" {
		t.Fatalf("explicit dataset root = %q, %v", got, err)
	}
	if _, err := resolveDatasetRoot("v3", ""); err == nil {
		t.Fatal("expected unsupported dataset version error")
	}
}
