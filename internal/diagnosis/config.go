package diagnosis

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ComputeAgentConfigHash returns the stable identity of the agent configuration
// that is persisted on each diagnosis run.
func ComputeAgentConfigHash(maxSteps, maxToolCalls, maxRepeatCalls int, temperature float64) string {
	payload := struct {
		PromptVersion  string  `json:"prompt_version"`
		AgentVersion   string  `json:"agent_version"`
		MaxSteps       int     `json:"max_steps"`
		MaxToolCalls   int     `json:"max_tool_calls"`
		MaxRepeatCalls int     `json:"max_repeat_calls"`
		ToolSetVersion string  `json:"tool_set_version"`
		Temperature    float64 `json:"temperature"`
	}{"v2.1", "v2.1", maxSteps, maxToolCalls, maxRepeatCalls, "v2.1-readonly-tools", temperature}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
