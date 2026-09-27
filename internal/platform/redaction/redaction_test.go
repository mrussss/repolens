package redaction

import (
	"strings"
	"testing"
)

func TestRedactJSONCredentialValues(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "password compact", input: `{"password":"supersecret123"}`},
		{name: "password whitespace", input: `{"password" :  "supersecret123"}`},
		{name: "case insensitive", input: `{"PASSWORD":"supersecret123"}`},
		{name: "passwd", input: `{"passwd":"supersecret123"}`},
		{name: "pwd", input: `{"pwd":"supersecret123"}`},
		{name: "token", input: `{"token":"supersecret123"}`},
		{name: "access token", input: `{"access_token":"supersecret123"}`},
		{name: "refresh token", input: `{"refresh_token":"supersecret123"}`},
		{name: "api key", input: `{"api_key":"supersecret123"}`},
		{name: "apikey", input: `{"apikey":"supersecret123"}`},
		{name: "secret", input: `{"secret":"supersecret123"}`},
		{name: "client secret", input: `{"client_secret":"supersecret123"}`},
		{name: "escaped string value", input: `{"token":"secret\\\"value"}`},
		{name: "authorization", input: `{"authorization":"supersecret123"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := RedactSecrets(test.input)
			if strings.Contains(got, "supersecret123") || strings.Contains(got, "secret\\\"value") {
				t.Fatalf("credential remained visible: %s", got)
			}
			if !strings.Contains(got, `"[REDACTED_SECRET]"`) {
				t.Fatalf("redacted JSON value was not preserved as a string: %s", got)
			}
		})
	}
}

func TestRedactLeavesOrdinaryJSONIntact(t *testing.T) {
	input := `{"name":"ordinary","password_hint":"not a credential","count":3,"nested":{"enabled":true}}`
	if got := RedactSecrets(input); got != input {
		t.Fatalf("ordinary JSON changed: got %s, want %s", got, input)
	}
}
