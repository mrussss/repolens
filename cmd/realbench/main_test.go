package main

import "testing"

func TestComparisonCaseSelection(t *testing.T) {
	for _, tc := range []struct {
		id    string
		all   bool
		valid bool
	}{
		{"", true, true}, {" REAL-004 ", false, true}, {"", false, false}, {"REAL-004", true, false}, {" ", false, false},
	} {
		ids, err := comparisonCaseIDs(tc.id, tc.all)
		if (err == nil) != tc.valid {
			t.Fatalf("selection(%q,%t): %v", tc.id, tc.all, err)
		}
		if tc.valid && !tc.all && (len(ids) != 1 || ids[0] != "REAL-004") {
			t.Fatalf("wrong case IDs: %v", ids)
		}
	}
}

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
