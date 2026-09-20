package revision

import (
	"strings"
	"testing"
)

func TestPublicRevisionRedactsInternalFailureDetails(t *testing.T) {
	value := &AnalysisRevision{
		ID:           "revision-1",
		Status:       StatusFailed,
		Stage:        StageMaterializing,
		ErrorMessage: "git clone failed: /home/user/.repolens/repositories/revision-1/source.tmp: permission denied",
	}
	public := publicRevision(value)
	if public == value {
		t.Fatal("public revision must be a copy")
	}
	if strings.Contains(public.ErrorMessage, "/home/") || strings.Contains(public.ErrorMessage, "permission denied") || strings.Contains(public.ErrorMessage, "source.tmp") {
		t.Fatalf("public failure details leaked: %q", public.ErrorMessage)
	}
	if public.ErrorMessage == "" {
		t.Fatal("public revision should retain a stable failure summary")
	}
}
