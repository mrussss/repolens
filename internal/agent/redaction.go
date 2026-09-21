package agent

import "repolens/internal/platform/redaction"

// RedactSecrets scans a string and replaces obvious API keys, tokens, and credentials with [REDACTED_SECRET].
func RedactSecrets(text string) string {
	return redaction.RedactSecrets(text)
}
