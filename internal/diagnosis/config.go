package diagnosis

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ComputeAgentConfigHash returns the stable identity of the agent configuration
// that is persisted on each diagnosis run.
func ComputeAgentConfigHash(maxSteps, maxToolCalls, maxRepeatCalls int, temperature float64) string {
	return ComputeAgentConfigHashWithBounds(maxSteps, maxToolCalls, 3, maxRepeatCalls, 32*1024, 1, temperature)
}

func ComputeAgentConfigHashWithBounds(maxSteps, maxToolCalls, maxSearchCalls, maxRepeatCalls, maxEvidencePacketBytes, finalizationTurns int, temperature float64) string {
	payload := struct {
		PromptVersion          string  `json:"prompt_version"`
		AgentVersion           string  `json:"agent_version"`
		MaxSteps               int     `json:"max_steps"`
		MaxToolCalls           int     `json:"max_tool_calls"`
		MaxSearchCalls         int     `json:"max_search_calls"`
		MaxRepeatCalls         int     `json:"max_repeat_calls"`
		MaxEvidencePacketBytes int     `json:"max_evidence_packet_bytes"`
		FinalizationTurns      int     `json:"finalization_turns"`
		ToolSetVersion         string  `json:"tool_set_version"`
		Temperature            float64 `json:"temperature"`
	}{"v2.2", "v2.2", maxSteps, maxToolCalls, maxSearchCalls, maxRepeatCalls, maxEvidencePacketBytes, finalizationTurns, "v2.2-readonly-tools", temperature}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
