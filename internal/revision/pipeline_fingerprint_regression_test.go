package revision_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/revision"
)

// This JSON is the production fingerprint input from
// v2.2-pre-refactor-stable (30bceaa9), with the original field order.
// Hashing the frozen input here checks the final refactor without embedding
// an unexplained digest or recomputing the expected value from current code.
const preRefactorPipelineInput = `{"pipeline_version":"v2.2.0","parser_version":"v2.2.1","analyzer_version":"v2.2.0","symbol_schema_version":"v2.1.0","retrieval_version":"v2.2.0","tokenizer_version":"v2.1.0","retrieval_strategy":"symbol_bm25_structural","bm25_k1":"1.2","bm25_b":"0.75","structural_parameters":"symbol-expansion-v1","file_filter_version":"filter-v2.2"}`

// Isolation and runtime fixes change derived artifacts, so old builds must not
// satisfy the new production identity. Freeze the new input independently.
const auditFixPipelineInput = `{"pipeline_version":"v2.2.0","parser_version":"v2.2.2","analyzer_version":"v2.2.1","symbol_schema_version":"v2.1.0","retrieval_version":"v2.2.0","tokenizer_version":"v2.1.0","retrieval_strategy":"symbol_bm25_structural","bm25_k1":"1.2","bm25_b":"0.75","structural_parameters":"symbol-expansion-v1","file_filter_version":"filter-v2.2"}`

func TestPipelineFingerprintMatchesAuditFixProductionInputs(t *testing.T) {
	if revision.PipelineVersion != "v2.2.0" ||
		codeintelmodel.CurrentParserVersion != "v2.2.2" ||
		codeintelmodel.CurrentAnalyzerVersion != "v2.2.1" ||
		codeintelmodel.CurrentSymbolSchemaVersion != "v2.1.0" ||
		codeintelmodel.CurrentRetrievalVersion != "v2.2.0" ||
		codeintelmodel.CurrentTokenizerVersion != "v2.1.0" {
		t.Fatal("production pipeline version inputs differ from the audit fix contract")
	}
	baseline := sha256.Sum256([]byte(auditFixPipelineInput))
	want := hex.EncodeToString(baseline[:])
	if got := revision.ComputePipelineFingerprint(); got != want {
		t.Fatalf("pipeline fingerprint differs from the audit fix contract: got %s, baseline %s", got, want)
	}
	old := sha256.Sum256([]byte(preRefactorPipelineInput))
	if revision.ComputePipelineFingerprint() == hex.EncodeToString(old[:]) {
		t.Fatal("audit fix reused an artifact identity with unsafe metadata or degraded runtime semantics")
	}
}
