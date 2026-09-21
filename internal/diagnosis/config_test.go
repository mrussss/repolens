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

func TestAgentConfigHashIncludesRealBenchGenerationOptions(t *testing.T) {
	base := diagnosis.ComputeAgentConfigHashWithRuntimeAndToolLimit(8, 12, 3, 2, 32768, 32768, 1, 4096, 180, 0, 0.1)
	defaultOptions := diagnosis.ComputeAgentConfigHashWithGenerationOptions(8, 12, 3, 2, 32768, 32768, 1, 4096, 180, 0, 0.1, "", "json_object")
	lowReasoning := diagnosis.ComputeAgentConfigHashWithGenerationOptions(8, 12, 3, 2, 32768, 32768, 1, 4096, 180, 0, 0.1, "low", "json_object")
	noResponseFormat := diagnosis.ComputeAgentConfigHashWithGenerationOptions(8, 12, 3, 2, 32768, 32768, 1, 4096, 180, 0, 0.1, "low", "none")
	if defaultOptions != base {
		t.Fatal("unset RealBench generation options changed the existing hash")
	}
	if lowReasoning == base || noResponseFormat == lowReasoning || noResponseFormat == base {
		t.Fatal("generation experiment options were not separated in the config hash")
	}
}
