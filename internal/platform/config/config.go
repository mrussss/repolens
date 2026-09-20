package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Env                    string
	HTTPPort               string
	DBDriver               string // "mysql" or "sqlite"
	DSN                    string
	SnapshotBasePath       string
	AllowHosts             []string
	MaxRepoSizeMB          int64
	MaxFileCount           int
	MaxFileSizeKB          int64
	ProviderType           string // "fake", "openai"
	ProviderAPIKey         string
	ProviderBaseURL        string
	ProviderModel          string
	ProviderAuthMode       string
	ProviderTimeoutSeconds int
	ProviderRetryAttempts  int
	ProviderSecretPath     string
	RetrievalStrategy      string // "bm25", "symbol_bm25_structural"
}

func Load() *Config {
	return &Config{
		Env:                    getEnv("ENV", "development"),
		HTTPPort:               getEnv("HTTP_PORT", "8080"),
		DBDriver:               getEnv("DB_DRIVER", "sqlite"),
		DSN:                    getEnv("DB_DSN", "repolens.db"),
		SnapshotBasePath:       getEnv("SNAPSHOT_BASE_PATH", defaultSnapshotBasePath()),
		AllowHosts:             splitHosts(getEnv("GIT_ALLOWED_HOSTS", "github.com")),
		MaxRepoSizeMB:          getEnvInt64("MAX_REPO_SIZE_MB", 50),
		MaxFileCount:           getEnvInt("MAX_FILE_COUNT", 2000),
		MaxFileSizeKB:          getEnvInt64("MAX_FILE_SIZE_KB", 512),
		ProviderType:           getEnv("REPOLENS_PROVIDER_TYPE", "fake"),
		ProviderAPIKey:         getEnv("REPOLENS_PROVIDER_API_KEY", ""),
		ProviderBaseURL:        getEnv("REPOLENS_PROVIDER_BASE_URL", "https://api.openai.com/v1"),
		ProviderModel:          getEnv("REPOLENS_PROVIDER_MODEL", "gpt-4o"),
		ProviderAuthMode:       getEnv("REPOLENS_PROVIDER_AUTH_MODE", "bearer"),
		ProviderTimeoutSeconds: getEnvPositiveInt("REPOLENS_PROVIDER_TIMEOUT_SECONDS", 60),
		ProviderRetryAttempts:  getEnvNonNegativeInt("REPOLENS_PROVIDER_RETRY_ATTEMPTS", 0),
		ProviderSecretPath:     getEnv("PROVIDER_SECRET_PATH", defaultProviderSecretPath()),
		RetrievalStrategy:      getEnv("RETRIEVAL_STRATEGY", "symbol_bm25_structural"),
	}
}

func defaultSnapshotBasePath() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(".", ".repolens", "repositories")
	}
	return filepath.Join(home, ".repolens", "repositories")
}

// DefaultSnapshotBasePath returns the writable local snapshot directory used
// when an embedding application does not provide an explicit path.
func DefaultSnapshotBasePath() string {
	return defaultSnapshotBasePath()
}

func defaultProviderSecretPath() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(".", ".repolens", "secrets", "provider.json")
	}
	return filepath.Join(home, ".repolens", "secrets", "provider.json")
}

func getEnvPositiveInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil && i > 0 {
			return i
		}
	}
	return defaultVal
}

func getEnvNonNegativeInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil && i >= 0 {
			return i
		}
	}
	return defaultVal
}

func splitHosts(raw string) []string {
	var hosts []string
	for _, host := range strings.Split(raw, ",") {
		if host = strings.TrimSpace(host); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}

func getEnvInt64(key string, defaultVal int64) int64 {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			return i
		}
	}
	return defaultVal
}
