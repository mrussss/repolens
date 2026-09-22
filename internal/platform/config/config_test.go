package config

import (
	"path/filepath"
	"testing"
)

func TestLoadUsesWritableLocalRuntimePathsByDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SNAPSHOT_BASE_PATH", "")
	t.Setenv("PROVIDER_SECRET_PATH", "")

	cfg := Load()
	if got, want := cfg.SnapshotBasePath, filepath.Join(home, ".repolens", "repositories"); got != want {
		t.Fatalf("snapshot base path = %q, want %q", got, want)
	}
	if got, want := cfg.ProviderSecretPath, filepath.Join(home, ".repolens", "secrets", "provider.json"); got != want {
		t.Fatalf("provider secret path = %q, want %q", got, want)
	}
}

func TestLoadKeepsExplicitRuntimePaths(t *testing.T) {
	t.Setenv("SNAPSHOT_BASE_PATH", "/custom/repos")
	t.Setenv("PROVIDER_SECRET_PATH", "/custom/provider.json")

	cfg := Load()
	if cfg.SnapshotBasePath != "/custom/repos" || cfg.ProviderSecretPath != "/custom/provider.json" {
		t.Fatalf("explicit paths were changed: snapshot=%q provider=%q", cfg.SnapshotBasePath, cfg.ProviderSecretPath)
	}
}

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

func TestProductionAgentDefaults(t *testing.T) {
	t.Setenv("REPOLENS_MAX_OUTPUT_TOKENS", "")
	t.Setenv("REPOLENS_REASONING_EFFORT", "")
	cfg := Load()
	if DefaultMaxOutputTokens != 4096 || cfg.MaxOutputTokens != DefaultMaxOutputTokens {
		t.Fatalf("max output tokens = %d, want 4096", cfg.MaxOutputTokens)
	}
	if DefaultReasoningEffort != "low" || cfg.ReasoningEffort != DefaultReasoningEffort {
		t.Fatalf("reasoning effort = %q, want low", cfg.ReasoningEffort)
	}
}

func TestProductionAgentConfigCanBeOverridden(t *testing.T) {
	t.Setenv("REPOLENS_MAX_OUTPUT_TOKENS", "8192")
	t.Setenv("REPOLENS_REASONING_EFFORT", "medium")
	cfg := Load()
	if cfg.MaxOutputTokens != 8192 || cfg.ReasoningEffort != "medium" {
		t.Fatalf("production agent config = %+v", cfg)
	}
}

func TestProviderRetryAttemptsAllowsZeroAndRejectsNegative(t *testing.T) {
	tests := []struct {
		value string
		want  int
	}{
		{value: "", want: 0},
		{value: "2", want: 2},
		{value: "-1", want: 0},
		{value: "invalid", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("REPOLENS_PROVIDER_RETRY_ATTEMPTS", tt.value)
			if got := Load().ProviderRetryAttempts; got != tt.want {
				t.Fatalf("provider retry attempts = %d, want %d", got, tt.want)
			}
		})
	}
}
