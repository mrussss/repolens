package diagnosis

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestDecodeCreateDiagnosisRequestIsStrict(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "valid", body: `{"analysis_revision_id":"rev","issue_title":"bug"}`, want: true},
		{name: "revision with optional lineage", body: `{"analysis_revision_id":"rev","repository_id":"repo","snapshot_id":"snap","code_index_build_id":1,"retrieval_build_id":2,"issue_title":"bug"}`, want: true},
		{name: "direct pinned builds", body: `{"repository_id":"repo","snapshot_id":"snap","code_index_build_id":1,"retrieval_build_id":2,"issue_title":"bug"}`, want: true},
		{name: "missing selection", body: `{"issue_title":"bug"}`, want: false},
		{name: "direct missing repository", body: `{"snapshot_id":"snap","code_index_build_id":1,"retrieval_build_id":2,"issue_title":"bug"}`, want: false},
		{name: "direct missing snapshot", body: `{"repository_id":"repo","code_index_build_id":1,"retrieval_build_id":2,"issue_title":"bug"}`, want: false},
		{name: "only one build", body: `{"repository_id":"repo","snapshot_id":"snap","code_index_build_id":1,"issue_title":"bug"}`, want: false},
		{name: "unknown field", body: `{"issue_title":"bug","unexpected":true}`, want: false},
		{name: "trailing json", body: `{"issue_title":"bug"}{}`, want: false},
		{name: "malformed", body: `{"issue_title":`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("POST", "/diagnoses", strings.NewReader(tt.body))
			var request CreateDiagnosisRequest
			err := decodeCreateDiagnosisRequest(ctx, &request)
			if (err == nil) != tt.want {
				t.Fatalf("decode error = %v, want success=%v", err, tt.want)
			}
		})
	}
}

func TestV22DiagnosisSchemaDeclaresExclusiveBuildSelection(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to locate test file")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), "..", "..", "contracts", "v2.2", "diagnosis-create.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		AdditionalProperties bool `json:"additionalProperties"`
		OneOf                []struct {
			Required []string `json:"required"`
		} `json:"oneOf"`
		Properties map[string]struct {
			MinLength *int `json:"minLength"`
			Minimum   *int `json:"minimum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.AdditionalProperties || len(schema.OneOf) != 2 {
		t.Fatalf("schema selection constraints = %+v", schema)
	}
	if schema.Properties["analysis_revision_id"].MinLength == nil || *schema.Properties["analysis_revision_id"].MinLength != 1 {
		t.Fatalf("analysis_revision_id minLength = %+v", schema.Properties["analysis_revision_id"].MinLength)
	}
	if schema.Properties["code_index_build_id"].Minimum == nil || *schema.Properties["code_index_build_id"].Minimum != 1 || schema.Properties["retrieval_build_id"].Minimum == nil || *schema.Properties["retrieval_build_id"].Minimum != 1 {
		t.Fatalf("build ID minimums are not positive: %+v", schema.Properties)
	}
}
