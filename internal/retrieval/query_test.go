package retrieval_test

import (
	"strings"
	"testing"

	"repolens/internal/retrieval"
)

func TestBuildQueryBoundsErrorLogAndEvidencePacket(t *testing.T) {
	log := "panic: DO_NOT_FORWARD_THIS_FULL_LOG\n" + strings.Repeat("noise ", 500) + " internal/worker/runner.go:42 nil pointer"
	query := retrieval.BuildQuery("worker crash", "request hangs", log)
	if strings.Contains(query, "noise noise noise noise noise noise") || len(query) > 2048 {
		t.Fatalf("unbounded/full log entered query: %q", query)
	}
	packet := retrieval.BuildEvidencePacket([]retrieval.SearchResult{{Path: "worker.go", StartLine: 1, EndLine: 3, Score: 1.2, Snippet: "package worker"}}, 256)
	if !strings.Contains(packet, "worker.go") || len(packet) > 256 {
		t.Fatalf("unexpected evidence packet: %q", packet)
	}
}
