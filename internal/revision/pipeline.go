package revision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const PipelineVersion = "v2.2.0"

// PipelineFingerprint is deliberately based on every setting that changes a
// derived analysis artifact. Keep the input as a typed struct so adding a
// field is an explicit code review decision rather than a string convention.
type pipelineFingerprintInput struct {
	PipelineVersion      string `json:"pipeline_version"`
	ParserVersion        string `json:"parser_version"`
	AnalyzerVersion      string `json:"analyzer_version"`
	SymbolSchemaVersion  string `json:"symbol_schema_version"`
	RetrievalVersion     string `json:"retrieval_version"`
	TokenizerVersion     string `json:"tokenizer_version"`
	RetrievalStrategy    string `json:"retrieval_strategy"`
	BM25K1               string `json:"bm25_k1"`
	BM25B                string `json:"bm25_b"`
	StructuralParameters string `json:"structural_parameters"`
	FileFilterVersion    string `json:"file_filter_version"`
}

func ComputePipelineFingerprint() string {
	input := pipelineFingerprintInput{
		PipelineVersion:      PipelineVersion,
		ParserVersion:        "v2.1.0",
		AnalyzerVersion:      "v2.1.0",
		SymbolSchemaVersion:  "v2.1.0",
		RetrievalVersion:     "v2.1.0",
		TokenizerVersion:     "v2.1.0",
		RetrievalStrategy:    "symbol_bm25_structural",
		BM25K1:               "1.2",
		BM25B:                "0.75",
		StructuralParameters: "symbol-expansion-v1",
		FileFilterVersion:    "filter-v2.1",
	}
	raw, _ := json.Marshal(input)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
