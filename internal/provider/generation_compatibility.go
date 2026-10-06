package provider

import platformconfig "repolens/internal/platform/config"

const (
	WarningHigherReasoningBudgetRisk = "HIGHER_REASONING_BUDGET_RISK"
	WarningCustomGenerationProfile   = "CUSTOM_GENERATION_PROFILE"
)

// CompatibilityWarning is advisory metadata, never an execution policy.
type CompatibilityWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// AssessGenerationCompatibility describes budget risks without validating
// provider-specific effort values or changing the selected generation settings.
func AssessGenerationCompatibility(reasoningEffort string, maxOutputTokens, providerTimeoutSeconds int) []CompatibilityWarning {
	var warnings []CompatibilityWarning
	if reasoningEffort != "" && reasoningEffort != platformconfig.DefaultReasoningEffort {
		warnings = append(warnings, CompatibilityWarning{
			Code:    WarningHigherReasoningBudgetRisk,
			Message: "Higher reasoning effort may consume more completion budget and increase latency. The connection probe validates request-shape compatibility, not successful completion of a full diagnosis. MODEL_OUTPUT_TRUNCATED or provider timeout can still occur.",
		})
	}
	if reasoningEffort != platformconfig.DefaultReasoningEffort || maxOutputTokens != platformconfig.DefaultMaxOutputTokens || providerTimeoutSeconds != platformconfig.DefaultProviderTimeoutSeconds {
		warnings = append(warnings, CompatibilityWarning{
			Code:    WarningCustomGenerationProfile,
			Message: "This diagnosis generation configuration differs from RepoLens defaults. Test Connection does not prove that this full generation profile will complete successfully on every diagnosis.",
		})
	}
	return warnings
}
