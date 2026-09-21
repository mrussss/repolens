package diagnosis

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const (
	CurrentPromptVersion = "v2.2-evidence-1"
	CurrentAgentVersion  = "v2.2.1"
)

// ComputeAgentConfigHash returns the stable identity of the agent configuration
// that is persisted on each diagnosis run.
func ComputeAgentConfigHash(maxSteps, maxToolCalls, maxRepeatCalls int, temperature float64) string {
	return ComputeAgentConfigHashWithBounds(maxSteps, maxToolCalls, 3, maxRepeatCalls, 32*1024, 1, temperature)
}

func ComputeAgentConfigHashWithBounds(maxSteps, maxToolCalls, maxSearchCalls, maxRepeatCalls, maxEvidencePacketBytes, finalizationTurns int, temperature float64) string {
	return ComputeAgentConfigHashWithRuntime(maxSteps, maxToolCalls, maxSearchCalls, maxRepeatCalls, maxEvidencePacketBytes, finalizationTurns, 2048, 60, 0, temperature)
}

// ComputeAgentConfigHashWithRuntime includes the execution limits that affect
// reproducibility, including output and provider timeout/retry policy.
func ComputeAgentConfigHashWithRuntime(maxSteps, maxToolCalls, maxSearchCalls, maxRepeatCalls, maxEvidencePacketBytes, finalizationTurns, maxOutputTokens, providerTimeoutSeconds, providerRetryAttempts int, temperature float64) string {
	return ComputeAgentConfigHashWithRuntimeAndToolLimit(maxSteps, maxToolCalls, maxSearchCalls, maxRepeatCalls, maxEvidencePacketBytes, 32*1024, finalizationTurns, maxOutputTokens, providerTimeoutSeconds, providerRetryAttempts, temperature)
}

// ComputeAgentConfigHashWithRuntimeAndToolLimit includes every execution bound
// that is frozen on a diagnosis run, including the maximum single tool result.
func ComputeAgentConfigHashWithRuntimeAndToolLimit(maxSteps, maxToolCalls, maxSearchCalls, maxRepeatCalls, maxEvidencePacketBytes, maxToolResultBytes, finalizationTurns, maxOutputTokens, providerTimeoutSeconds, providerRetryAttempts int, temperature float64) string {
	payload := struct {
		PromptVersion          string  `json:"prompt_version"`
		AgentVersion           string  `json:"agent_version"`
		MaxSteps               int     `json:"max_steps"`
		MaxToolCalls           int     `json:"max_tool_calls"`
		MaxSearchCalls         int     `json:"max_search_calls"`
		MaxRepeatCalls         int     `json:"max_repeat_calls"`
		MaxEvidencePacketBytes int     `json:"max_evidence_packet_bytes"`
		MaxToolResultBytes     int     `json:"max_tool_result_bytes"`
		FinalizationTurns      int     `json:"finalization_turns"`
		MaxOutputTokens        int     `json:"max_output_tokens"`
		ProviderTimeoutSeconds int     `json:"provider_timeout_seconds"`
		ProviderRetryAttempts  int     `json:"provider_retry_attempts"`
		ToolSetVersion         string  `json:"tool_set_version"`
		Temperature            float64 `json:"temperature"`
	}{CurrentPromptVersion, CurrentAgentVersion, maxSteps, maxToolCalls, maxSearchCalls, maxRepeatCalls, maxEvidencePacketBytes, maxToolResultBytes, finalizationTurns, maxOutputTokens, providerTimeoutSeconds, providerRetryAttempts, "v2.2-readonly-tools-evidence", temperature}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ComputeAgentConfigHashWithGenerationOptions extends the stable RealBench
// configuration identity with provider generation compatibility parameters.
// The existing helper remains unchanged for production callers. When the
// options describe the current default request, this helper deliberately
// returns the existing hash so an unset RealBench experiment is identical to
// the current main behavior.
func ComputeAgentConfigHashWithGenerationOptions(maxSteps, maxToolCalls, maxSearchCalls, maxRepeatCalls, maxEvidencePacketBytes, maxToolResultBytes, finalizationTurns, maxOutputTokens, providerTimeoutSeconds, providerRetryAttempts int, temperature float64, reasoningEffort, responseFormat string) string {
	if reasoningEffort == "" && responseFormat == "json_object" {
		return ComputeAgentConfigHashWithRuntimeAndToolLimit(maxSteps, maxToolCalls, maxSearchCalls, maxRepeatCalls, maxEvidencePacketBytes, maxToolResultBytes, finalizationTurns, maxOutputTokens, providerTimeoutSeconds, providerRetryAttempts, temperature)
	}
	payload := struct {
		PromptVersion          string  `json:"prompt_version"`
		AgentVersion           string  `json:"agent_version"`
		MaxSteps               int     `json:"max_steps"`
		MaxToolCalls           int     `json:"max_tool_calls"`
		MaxSearchCalls         int     `json:"max_search_calls"`
		MaxRepeatCalls         int     `json:"max_repeat_calls"`
		MaxEvidencePacketBytes int     `json:"max_evidence_packet_bytes"`
		MaxToolResultBytes     int     `json:"max_tool_result_bytes"`
		FinalizationTurns      int     `json:"finalization_turns"`
		MaxOutputTokens        int     `json:"max_output_tokens"`
		ProviderTimeoutSeconds int     `json:"provider_timeout_seconds"`
		ProviderRetryAttempts  int     `json:"provider_retry_attempts"`
		ToolSetVersion         string  `json:"tool_set_version"`
		Temperature            float64 `json:"temperature"`
		ReasoningEffort        string  `json:"reasoning_effort"`
		ResponseFormat         string  `json:"response_format"`
	}{CurrentPromptVersion, CurrentAgentVersion, maxSteps, maxToolCalls, maxSearchCalls, maxRepeatCalls, maxEvidencePacketBytes, maxToolResultBytes, finalizationTurns, maxOutputTokens, providerTimeoutSeconds, providerRetryAttempts, "v2.2-readonly-tools-evidence", temperature, reasoningEffort, responseFormat}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
