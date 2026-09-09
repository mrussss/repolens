package config

import "testing"

func TestProviderTimeoutSecondsUsesPositiveValueOrDefault(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "unset", value: "", want: 60},
		{name: "positive", value: "15", want: 15},
		{name: "zero", value: "0", want: 60},
		{name: "negative", value: "-1", want: 60},
		{name: "invalid", value: "not-a-number", want: 60},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("REPOLENS_PROVIDER_TIMEOUT_SECONDS", tt.value)
			if got := Load().ProviderTimeoutSeconds; got != tt.want {
				t.Fatalf("timeout = %d, want %d", got, tt.want)
			}
		})
	}
}
