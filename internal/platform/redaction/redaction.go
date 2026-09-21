package redaction

import "regexp"

var (
	bearerTokenRegex = regexp.MustCompile(`(?i)\b(bearer\s+)([a-zA-Z0-9_\-\.]{16,})\b`)
	githubTokenRegex = regexp.MustCompile(`\b(ghp_[a-zA-Z0-9]{36}|github_pat_[a-zA-Z0-9_]{60,})\b`)
	openaiKeyRegex   = regexp.MustCompile(`\b(sk-[a-zA-Z0-9_\-]{20,})\b`)
	awsKeyRegex      = regexp.MustCompile(`\b(AKIA[0-9A-Z]{16})\b`)
	privateKeyRegex  = regexp.MustCompile(`(?s)-----BEGIN\s+([A-Z\s]+)?PRIVATE\s+KEY-----.*?-----END\s+([A-Z\s]+)?PRIVATE\s+KEY-----`)
	envSecretRegex   = regexp.MustCompile(`(?i)\b(password|secret|api_key|access_token|private_key)\s*=\s*['"]?([^'"\s\n]{8,})['"]?`)
	authHeaderRegex  = regexp.MustCompile(`(?i)(authorization:\s*)([^\s\r\n]{10,})`)
)

// RedactSecrets replaces obvious API keys, tokens, and credentials before
// source content is sent to a model or persisted in an agent trace.
func RedactSecrets(text string) string {
	if text == "" {
		return text
	}

	text = privateKeyRegex.ReplaceAllString(text, "[REDACTED_PRIVATE_KEY]")
	text = bearerTokenRegex.ReplaceAllString(text, "${1}[REDACTED_SECRET]")
	text = authHeaderRegex.ReplaceAllString(text, "${1}[REDACTED_SECRET]")
	text = githubTokenRegex.ReplaceAllString(text, "[REDACTED_GITHUB_TOKEN]")
	text = openaiKeyRegex.ReplaceAllString(text, "[REDACTED_API_KEY]")
	text = awsKeyRegex.ReplaceAllString(text, "[REDACTED_AWS_KEY]")
	text = envSecretRegex.ReplaceAllString(text, "${1}=[REDACTED_SECRET]")
	return text
}
