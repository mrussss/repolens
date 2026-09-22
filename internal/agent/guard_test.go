package agent

import "testing"

func TestDefaultGuardConfigUsesProductionMaxOutputTokens(t *testing.T) {
	if got := DefaultGuardConfig().MaxOutputTokens; got != 4096 {
		t.Fatalf("default max output tokens = %d, want 4096", got)
	}
}
