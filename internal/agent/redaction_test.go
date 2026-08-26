package agent_test

import (
	"strings"
	"testing"

	"repolens/internal/agent"
)

func TestRedactSecrets(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains string
		redacted string
	}{
		{
			name:     "Bearer token",
			input:    "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.xyz1234567890",
			contains: "Bearer [REDACTED_SECRET]",
			redacted: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		},
		{
			name:     "GitHub Token",
			input:    "const token = 'ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';",
			contains: "[REDACTED_GITHUB_TOKEN]",
			redacted: "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		},
		{
			name:     "OpenAI API Key",
			input:    "LLM_KEY=sk-abcdefghijklmnopqrstuvwxyz1234567890",
			contains: "[REDACTED_API_KEY]",
			redacted: "sk-abcdefghijklmnopqrstuvwxyz1234567890",
		},
		{
			name:     "AWS Access Key",
			input:    "export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
			contains: "[REDACTED_AWS_KEY]",
			redacted: "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:     "Private Key Block",
			input:    "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA0\n-----END RSA PRIVATE KEY-----",
			contains: "[REDACTED_PRIVATE_KEY]",
			redacted: "MIIEowIBAAKCAQEA0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := agent.RedactSecrets(tt.input)
			if !strings.Contains(output, tt.contains) {
				t.Errorf("expected output to contain %q, got %q", tt.contains, output)
			}
			if strings.Contains(output, tt.redacted) {
				t.Errorf("expected secret %q to be redacted from output: %q", tt.redacted, output)
			}
		})
	}
}
