package diagnosis_test

import (
	"testing"

	"repolens/internal/diagnosis"
)

func TestAgentConfigHashIncludesSingleToolResultLimit(t *testing.T) {
	withoutLargeResult := diagnosis.ComputeAgentConfigHashWithRuntimeAndToolLimit(8, 12, 3, 2, 32768, 16384, 1, 2048, 60, 0, 0.1)
	withLargeResult := diagnosis.ComputeAgentConfigHashWithRuntimeAndToolLimit(8, 12, 3, 2, 32768, 32768, 1, 2048, 60, 0, 0.1)
	if withoutLargeResult == withLargeResult {
		t.Fatal("agent config hash did not change when max tool result bytes changed")
	}
}
