package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

type EvalRun struct {
	ID                  string    `json:"id"`
	DatasetVersion      string    `json:"dataset_version"`
	GitCommit           string    `json:"git_commit"`
	SnapshotSHA         string    `json:"snapshot_sha"`
	RetrievalStrategy   string    `json:"retrieval_strategy"`
	RetrievalVersion    string    `json:"retrieval_version"`
	IndexVersion        string    `json:"index_version"`
	PromptVersion       string    `json:"prompt_version"`
	AgentVersion        string    `json:"agent_version"`
	Model               string    `json:"model"`
	DatasetManifestHash string    `json:"dataset_manifest_hash"`
	AgentConfigHash     string    `json:"agent_config_hash"`
	EmbeddingModel      string    `json:"embedding_model"`
	TotalCases          int       `json:"total_cases"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at"`
	Metrics             Metrics   `json:"metrics"`
}

// DatasetManifestHash is the reproducibility fingerprint for the exact case
// split consumed by an EvalRun. Case order is normalized before hashing.
func DatasetManifestHash(cases []EvalCase) string {
	ordered := append([]EvalCase(nil), cases...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].CaseID < ordered[j].CaseID })
	data, _ := json.Marshal(ordered)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
