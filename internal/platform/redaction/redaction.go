package redaction

import "regexp"
import "strings"

var (
	bearerTokenRegex = regexp.MustCompile(`(?i)\b(bearer[\t ]+)([a-zA-Z0-9_\-\.+/]+=*)\b`)
	githubTokenRegex = regexp.MustCompile(`\b(ghp_[a-zA-Z0-9]{36}|github_pat_[a-zA-Z0-9_]{60,})\b`)
	openaiKeyRegex   = regexp.MustCompile(`\b(sk-[a-zA-Z0-9_\-]{20,})\b`)
	awsKeyRegex      = regexp.MustCompile(`\b(AKIA[0-9A-Z]{16})\b`)
	privateKeyRegex  = regexp.MustCompile(`(?s)-----BEGIN\s+([A-Z\s]+)?PRIVATE\s+KEY-----.*?-----END\s+([A-Z\s]+)?PRIVATE\s+KEY-----`)
	envSecretRegex   = regexp.MustCompile(`(?i)\b(password|secret|api_key|access_token|private_key)\s*=\s*['"]?([^'"\s\n]{8,})['"]?`)
	authHeaderRegex  = regexp.MustCompile(`(?im)(\bauthorization[\t ]*:[\t ]*)([a-z][a-z0-9_-]*)([\t ]+)([^\s\r\n]+)`)
	authJSONRegex    = regexp.MustCompile(`(?i)(["']authorization["'][\t ]*:[\t ]*["'])([^"'\r\n]+)`)
)

// RedactSecrets replaces obvious API keys, tokens, and credentials before
// source content is sent to a model or persisted in an agent trace.
func RedactSecrets(text string) string {
	if text == "" {
		return text
	}

	text = privateKeyRegex.ReplaceAllString(text, "[REDACTED_PRIVATE_KEY]")
	text = authHeaderRegex.ReplaceAllString(text, "${1}${2} [REDACTED_SECRET]")
	text = authJSONRegex.ReplaceAllStringFunc(text, func(header string) string {
		parts := authJSONRegex.FindStringSubmatch(header)
		if len(parts) != 3 {
			return header
		}
		fields := strings.Fields(parts[2])
		if len(fields) < 2 {
			return header
		}
		return parts[1] + fields[0] + " [REDACTED_SECRET]"
	})
	text = bearerTokenRegex.ReplaceAllString(text, "${1}[REDACTED_SECRET]")
	text = githubTokenRegex.ReplaceAllString(text, "[REDACTED_GITHUB_TOKEN]")
	text = openaiKeyRegex.ReplaceAllString(text, "[REDACTED_API_KEY]")
	text = awsKeyRegex.ReplaceAllString(text, "[REDACTED_AWS_KEY]")
	text = envSecretRegex.ReplaceAllString(text, "${1}=[REDACTED_SECRET]")
	return text
}
