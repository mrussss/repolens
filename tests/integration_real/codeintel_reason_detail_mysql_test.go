package integration_real

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"repolens/internal/codeintel"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
)

func TestRealMySQL_RelatedTestExplanationBeyond255Persists(t *testing.T) {
	if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "" {
		t.Skip("skipping real MySQL CodeIndex test (set REPOLENS_REQUIRE_REAL_INTEGRATION=1)")
	}

	db, _, cleanup := setupRealMySQL(t)
	if cleanup != nil {
		defer cleanup()
	}
	ctx := context.Background()
	root := t.TempDir()
	modulePath := "example.com/longreason"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+modulePath+"\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	prodName := "Prod" + strings.Repeat("p", 124)
	testName := "Test" + strings.Repeat("t", 124)
	writeSource := func(name, source string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(source), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	writeSource("service.go", "package longreason\n\nfunc "+prodName+"() {}\n")
	writeSource("service_test.go", "package longreason\n\nfunc "+testName+"() { "+prodName+"() }\n")

	analysis, err := codeintel.NewAnalyzer().Analyze(ctx, root, codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatalf("analyze long-name fixture: %v", err)
	}
	if len(analysis.RelatedTests) == 0 {
		t.Fatal("analyzer did not discover the related test")
	}
	discovery := analysis.RelatedTests[0]
	if len(discovery.Explanation) <= 255 {
		t.Fatalf("fixture explanation length = %d, want >255", len(discovery.Explanation))
	}

	ciStore := codeintelstore.NewStore(db)
	build, _, err := ciStore.GetOrCreateBuild(ctx, "snap-long-reason", analysis.ModulePath, analysis.BuildContext)
	if err != nil {
		t.Fatalf("create CodeIndexBuild: %v", err)
	}
	if err := ciStore.MarkBuildBuilding(ctx, build.ID); err != nil {
		t.Fatalf("mark CodeIndexBuild as BUILDING: %v", err)
	}
	if err := ciStore.SaveAnalysisResult(ctx, build.ID, analysis); err != nil {
		t.Fatalf("persist CodeIndex analysis with long related-test explanation: %v", err)
	}

	relations, err := ciStore.ListRelatedTests(ctx, build.ID, discovery.TargetSymbolKeyHash)
	if err != nil {
		t.Fatalf("read TEST_RELATION: %v", err)
	}
	if len(relations) != 1 {
		t.Fatalf("related TEST_RELATION count = %d, want 1", len(relations))
	}
	if relations[0].RelationType != codeintelmodel.RelationTypeTestRelation || relations[0].ReasonDetail != discovery.Explanation {
		t.Fatalf("read TEST_RELATION did not preserve the full explanation (stored length=%d, expected length=%d)", len(relations[0].ReasonDetail), len(discovery.Explanation))
	}
}
