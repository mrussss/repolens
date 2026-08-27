package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Env                string
	HTTPPort           string
	DBDriver           string // "mysql" or "sqlite"
	DSN                string
	SnapshotBasePath   string
	AllowHosts         []string
	MaxRepoSizeMB      int64
	MaxFileCount       int
	MaxFileSizeKB      int64
	ProviderType       string // "fake", "openai"
	ProviderAPIKey     string
	ProviderBaseURL    string
	ProviderModel      string
	ProviderAuthMode   string
	ProviderSecretPath string
	RetrievalStrategy  string // "bm25", "symbol_bm25_structural"
}

func Load() *Config {
	return &Config{
		Env:                getEnv("ENV", "development"),
		HTTPPort:           getEnv("HTTP_PORT", "8080"),
		DBDriver:           getEnv("DB_DRIVER", "sqlite"),
		DSN:                getEnv("DB_DSN", "repolens.db"),
		SnapshotBasePath:   getEnv("SNAPSHOT_BASE_PATH", "/data/repositories"),
		AllowHosts:         splitHosts(getEnv("GIT_ALLOWED_HOSTS", "github.com")),
		MaxRepoSizeMB:      getEnvInt64("MAX_REPO_SIZE_MB", 50),
		MaxFileCount:       getEnvInt("MAX_FILE_COUNT", 2000),
		MaxFileSizeKB:      getEnvInt64("MAX_FILE_SIZE_KB", 512),
		ProviderType:       getEnv("REPOLENS_PROVIDER_TYPE", "fake"),
		ProviderAPIKey:     getEnv("REPOLENS_PROVIDER_API_KEY", ""),
		ProviderBaseURL:    getEnv("REPOLENS_PROVIDER_BASE_URL", "https://api.openai.com/v1"),
		ProviderModel:      getEnv("REPOLENS_PROVIDER_MODEL", "gpt-4o"),
		ProviderAuthMode:   getEnv("REPOLENS_PROVIDER_AUTH_MODE", "bearer"),
		ProviderSecretPath: getEnv("PROVIDER_SECRET_PATH", ""),
		RetrievalStrategy:  getEnv("RETRIEVAL_STRATEGY", "symbol_bm25_structural"),
	}
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
