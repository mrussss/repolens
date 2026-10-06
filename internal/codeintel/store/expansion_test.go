package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"repolens/internal/codeintel/model"
)

type expansionQueryLog struct {
	logger.Interface
	sql []string
}

func (l *expansionQueryLog) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	l.sql = append(l.sql, sql)
}

func expansionDB(t *testing.T) (*gorm.DB, *GormStore, *expansionQueryLog) {
	t.Helper()
	log := &expansionQueryLog{Interface: logger.Default.LogMode(logger.Silent)}
	db, err := gorm.Open(sqlite.Open("file:"+t.TempDir()+"/expansion.sqlite"), &gorm.Config{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Symbol{}, &model.SymbolRelation{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db, NewStore(db), log
}

func expansionSymbol(t *testing.T, db *gorm.DB, buildID int64, name string) model.Symbol {
	t.Helper()
	raw, hash := model.BuildSymbolKey("example.com/m", "example.com/m", "", model.SymbolKindFunction, name)
	s := model.Symbol{CodeIndexBuildID: buildID, Name: name, SymbolKeyRaw: raw, SymbolKeyHash: hash, FilePath: name + ".go", Kind: model.SymbolKindFunction, StartLine: 1, EndLine: 1}
	if err := db.Create(&s).Error; err != nil {
		t.Fatal(err)
	}
	return s
}

func expansionRelation(t *testing.T, db *gorm.DB, from, to model.Symbol, kind model.RelationType, resolution model.ResolutionKind, confidence float64, reason string) {
	t.Helper()
	r := model.SymbolRelation{CodeIndexBuildID: from.CodeIndexBuildID, FromSymbolID: &from.ID, FromSymbolKeyHash: from.SymbolKeyHash, ToSymbolID: &to.ID, ToSymbolKeyHash: to.SymbolKeyHash, RelationType: kind, ResolutionKind: resolution, Confidence: confidence, ReasonCode: reason}
	if err := db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
}

func TestStructuralExpansionStoreFiltersDirectionConfidenceAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kind       model.RelationType
		resolution model.ResolutionKind
		confidence float64
		reason     string
		reverse    bool
		want       bool
	}{
		{"call95", model.RelationTypeCallCandidate, model.ResolutionKindSemantic, .95, "SEMANTIC_DIRECT_FUNC", false, true},
		{"call100", model.RelationTypeCallCandidate, model.ResolutionKindSemantic, 1, "SEMANTIC_DIRECT_FUNC", false, true},
		{"call94", model.RelationTypeCallCandidate, model.ResolutionKindSemantic, .94, "SEMANTIC_DIRECT_FUNC", false, false},
		{"reverseCall", model.RelationTypeCallCandidate, model.ResolutionKindSemantic, 1, "SEMANTIC_DIRECT_FUNC", true, false},
		{"reference", model.RelationTypeReference, model.ResolutionKindSemantic, 1, "SEMANTIC_TYPE_OR_SYMBOL_REF", false, false},
		{"reverseReference", model.RelationTypeReference, model.ResolutionKindSemantic, 1, "SEMANTIC_TYPE_OR_SYMBOL_REF", true, false},
		{"syntacticCall", model.RelationTypeCallCandidate, model.ResolutionKindSyntactic, 1, "", false, false},
		{"heuristicCall", model.RelationTypeCallCandidate, model.ResolutionKindHeuristic, 1, "", false, false},
		{"unresolvedCall", model.RelationTypeCallCandidate, model.ResolutionKindUnresolved, 1, "", false, false},
		{"test", model.RelationTypeTestRelation, model.ResolutionKindSemantic, 1, string(model.TestReasonDirectSemantic), true, true},
		{"forwardTest", model.RelationTypeTestRelation, model.ResolutionKindSemantic, 1, string(model.TestReasonDirectSemantic), false, false},
		{"weakTestName", model.RelationTypeTestRelation, model.ResolutionKindSemantic, 1, string(model.TestReasonNameMatch), true, false},
		{"weakTestPackage", model.RelationTypeTestRelation, model.ResolutionKindSemantic, 1, string(model.TestReasonSamePackage), true, false},
		{"weakTestSyntactic", model.RelationTypeTestRelation, model.ResolutionKindSemantic, 1, string(model.TestReasonDirectSyntactic), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store, log := expansionDB(t)
			seed := expansionSymbol(t, db, 1, "seed")
			target := expansionSymbol(t, db, 1, "target")
			from, to := seed, target
			if tc.reverse {
				from, to = to, from
			}
			expansionRelation(t, db, from, to, tc.kind, tc.resolution, tc.confidence, tc.reason)
			log.sql = nil
			result, err := store.ListStructuralExpansionCandidates(context.Background(), 1, seed.SymbolKeyHash)
			if err != nil || (len(result.Candidates) == 1) != tc.want || result.SymbolQueries != 1 || result.RelationQueries != 2 || len(log.sql) != 3 {
				t.Fatalf("result=%+v err=%v SQL=%v", result, err, log.sql)
			}
			for _, query := range log.sql[1:] {
				if !strings.Contains(query, "LIMIT 8") || !strings.Contains(query, "target.code_index_build_id = r.code_index_build_id") {
					t.Fatalf("unbounded or unpinned SQL: %s", query)
				}
			}
			if tc.want && result.Candidates[0].Symbol.ID != target.ID {
				t.Fatal("target was not fully joined")
			}
		})
	}
}

func TestStructuralExpansionStoreBoundedDistinctRowsAndStableOrdering(t *testing.T) {
	db, store, log := expansionDB(t)
	seed := expansionSymbol(t, db, 1, "seed")
	for i := 0; i < 30; i++ {
		target := expansionSymbol(t, db, 1, fmt.Sprintf("target%02d", i))
		// Multiple sites of one call must not crowd out the bounded neighbors.
		for j := 0; j < 3; j++ {
			expansionRelation(t, db, seed, target, model.RelationTypeCallCandidate, model.ResolutionKindSemantic, 1, "SEMANTIC_DIRECT_FUNC")
		}
		expansionRelation(t, db, target, seed, model.RelationTypeTestRelation, model.ResolutionKindSemantic, 1, string(model.TestReasonDirectSemantic))
	}
	var previous string
	for i := 0; i < 3; i++ {
		log.sql = nil
		result, err := store.ListStructuralExpansionCandidates(context.Background(), 1, seed.SymbolKeyHash)
		if err != nil || len(result.Candidates) != 16 || result.RelationsExamined != 16 || result.RelationQueries != 2 || len(log.sql) != 3 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		var keys []string
		for n, c := range result.Candidates {
			keys = append(keys, c.Symbol.SymbolKeyHash)
			if n%8 > 0 && result.Candidates[n-1].Symbol.SymbolKeyHash >= c.Symbol.SymbolKeyHash {
				t.Fatal("query ordering or distinct identity failed")
			}
		}
		order := strings.Join(keys, ",")
		if i > 0 && previous != order {
			t.Fatal("nondeterministic bounded selection")
		}
		previous = order
	}
}

func TestStructuralExpansionStoreRejectsUnresolvedCrossBuildAndInconsistentEdges(t *testing.T) {
	db, store, _ := expansionDB(t)
	seed := expansionSymbol(t, db, 1, "seed")
	other := expansionSymbol(t, db, 2, "other")
	// Cross-build target cannot be joined even when its ID is resolved.
	expansionRelation(t, db, seed, other, model.RelationTypeCallCandidate, model.ResolutionKindSemantic, 1, "SEMANTIC_DIRECT_FUNC")
	missing := expansionSymbol(t, db, 1, "missing")
	if err := db.Model(&missing).Update("symbol_key_hash", "").Error; err != nil {
		t.Fatal(err)
	}
	expansionRelation(t, db, seed, missing, model.RelationTypeCallCandidate, model.ResolutionKindSemantic, 1, "SEMANTIC_DIRECT_FUNC")
	broken := model.SymbolRelation{CodeIndexBuildID: 1, FromSymbolID: &seed.ID, FromSymbolKeyHash: seed.SymbolKeyHash, ToSymbolKeyHash: "unresolved", RelationType: model.RelationTypeCallCandidate, ResolutionKind: model.ResolutionKindSemantic, Confidence: 1}
	if err := db.Create(&broken).Error; err != nil {
		t.Fatal(err)
	}
	result, err := store.ListStructuralExpansionCandidates(context.Background(), 1, seed.SymbolKeyHash)
	if err != nil || len(result.Candidates) != 0 {
		t.Fatalf("invalid identity accepted: %+v %v", result, err)
	}
	result, err = store.ListStructuralExpansionCandidates(context.Background(), 1, "absent")
	if err != nil || result.RelationQueries != 0 {
		t.Fatal("missing seed queried graph")
	}
	result, err = store.ListStructuralExpansionCandidates(context.Background(), 0, "")
	if err != nil || result.SymbolQueries != 0 {
		t.Fatal("invalid seed queried graph")
	}
}
