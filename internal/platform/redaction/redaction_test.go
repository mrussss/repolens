package redaction

import (
	"strings"
	"testing"
)

func TestRedactRawAndEscapedJSONCredentialPairs(t *testing.T) {
	keys := []string{
		"password", "passwd", "pwd", "token", "access_token", "refresh_token",
		"api_key", "apikey", "secret", "client_secret", "authorization",
	}
	for _, key := range keys {
		for _, escaped := range []bool{false, true} {
			name := key + "/raw"
			input := `{"` + key + `" : "credential-value"}`
			if escaped {
				name = key + "/escaped"
				input = strings.ReplaceAll(input, `"`, `\"`)
			}
			t.Run(name, func(t *testing.T) {
				got := RedactSecrets("context=kept " + input)
				if strings.Contains(got, "credential-value") {
					t.Fatalf("credential remained visible: %s", got)
				}
				if !strings.Contains(got, "context=kept") || !strings.Contains(got, "[REDACTED_SECRET]") {
					t.Fatalf("ordinary context or redaction marker missing: %s", got)
				}
			})
		}
	}
}

func TestRedactEmbeddedJSONLogWithMultipleCredentials(t *testing.T) {
	input := `request_body="{\"password\":\"password-secret\",\"access_token\": \"access-secret\"}" response="{\"client_secret\":\"client-secret\"}" note=kept`
	got := RedactSecrets(input)
	for _, secret := range []string{"password-secret", "access-secret", "client-secret"} {
		if strings.Contains(got, secret) {
			t.Errorf("embedded credential %q remained visible: %s", secret, got)
		}
	}
	for _, context := range []string{"request_body=", "response=", "note=kept", "[REDACTED_SECRET]"} {
		if !strings.Contains(got, context) {
			t.Errorf("redaction removed useful context %q: %s", context, got)
		}
	}
}

func TestRedactLeavesOrdinaryJSONIntact(t *testing.T) {
	input := `{"name":"ordinary","password_hint":"not a credential","count":3,"nested":{"enabled":true}}`
	if got := RedactSecrets(input); got != input {
		t.Fatalf("ordinary JSON changed: got %s, want %s", got, input)
	}
}
