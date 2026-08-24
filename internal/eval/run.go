package eval

import (
	"time"
)

type EvalRun struct {
	ID                string    `json:"id"`
	DatasetVersion    string    `json:"dataset_version"`
	GitCommit         string    `json:"git_commit"`
	SnapshotSHA       string    `json:"snapshot_sha"`
	RetrievalStrategy string    `json:"retrieval_strategy"`
	RetrievalVersion  string    `json:"retrieval_version"`
	IndexVersion      string    `json:"index_version"`
	PromptVersion     string    `json:"prompt_version"`
	AgentVersion      string    `json:"agent_version"`
	Model             string    `json:"model"`
	EmbeddingModel    string    `json:"embedding_model"`
	TotalCases        int       `json:"total_cases"`
	StartedAt         time.Time `json:"started_at"`
	FinishedAt        time.Time `json:"finished_at"`
	Metrics           Metrics   `json:"metrics"`
}
