package config

import (
	"os"
	"strconv"
)

type Config struct {
	Env               string
	HTTPPort          string
	DBDriver          string // "mysql" or "sqlite"
	DSN               string
	SnapshotBasePath  string
	AllowHosts        []string
	MaxRepoSizeMB     int64
	MaxFileCount      int
	MaxFileSizeKB     int64
	LLMProvider       string // "fake", "openai"
	LLMAPIKey         string
	LLMBaseURL        string
	LLMModel          string
	RetrievalStrategy string // "bm25" (default), "hybrid"
	ESURL             string
	ESIndexName       string
	EmbeddingProvider string // "local", "openai"
	EmbeddingAPIKey   string
	EmbeddingBaseURL  string
	EmbeddingModel    string
	EmbeddingDim      int
}

func Load() *Config {
	return &Config{
		Env:               getEnv("ENV", "development"),
		HTTPPort:          getEnv("HTTP_PORT", "8080"),
		DBDriver:          getEnv("DB_DRIVER", "sqlite"),
		DSN:               getEnv("DB_DSN", "repolens.db"),
		SnapshotBasePath:  getEnv("SNAPSHOT_BASE_PATH", "/data/repositories"),
		AllowHosts:        []string{"github.com", "gitlab.com"},
		MaxRepoSizeMB:     getEnvInt64("MAX_REPO_SIZE_MB", 50),
		MaxFileCount:      getEnvInt("MAX_FILE_COUNT", 2000),
		MaxFileSizeKB:     getEnvInt64("MAX_FILE_SIZE_KB", 512),
		LLMProvider:       getEnv("LLM_PROVIDER", "fake"),
		LLMAPIKey:         getEnv("LLM_API_KEY", ""),
		LLMBaseURL:        getEnv("LLM_BASE_URL", "https://api.openai.com/v1"),
		LLMModel:          getEnv("LLM_MODEL", "gpt-4o"),
		RetrievalStrategy: getEnv("RETRIEVAL_STRATEGY", "bm25"),
		ESURL:             getEnv("ES_URL", "http://localhost:9200"),
		ESIndexName:       getEnv("ES_INDEX_NAME", "repolens_chunks"),
		EmbeddingProvider: getEnv("EMBEDDING_PROVIDER", "local"),
		EmbeddingAPIKey:   getEnv("EMBEDDING_API_KEY", ""),
		EmbeddingBaseURL:  getEnv("EMBEDDING_BASE_URL", "https://api.openai.com/v1"),
		EmbeddingModel:    getEnv("EMBEDDING_MODEL", "text-embedding-3-small"),
		EmbeddingDim:      getEnvInt("EMBEDDING_DIM", 128),
	}
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
