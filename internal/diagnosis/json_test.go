package diagnosis

import (
	"net/http/httptest"
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
