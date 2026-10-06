package store

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"repolens/internal/codeintel/model"
)

// ListStructuralExpansionCandidates makes at most one symbol lookup and two
// targeted, limited relation queries. Existing endpoint-ID indexes are usable;
// neither all relations nor ListRelatedTests are loaded into memory.
func (s *GormStore) ListStructuralExpansionCandidates(ctx context.Context, buildID int64, seedHash string) (model.StructuralExpansionCandidates, error) {
	var result model.StructuralExpansionCandidates
	if buildID <= 0 || seedHash == "" {
		return result, nil
	}
	result.SymbolQueries++
	seed, err := s.GetSymbolByHash(ctx, buildID, seedHash)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if seed.ID <= 0 || seed.SymbolKeyRaw == "" || seed.Name == "" {
		return result, nil
	}
	for _, direction := range []string{model.ExpansionForward, model.ExpansionReverse} {
		endpoint, targetEndpoint, seedKey, targetKey := "from_symbol_id", "to_symbol_id", "from_symbol_key_hash", "to_symbol_key_hash"
		relationType := model.RelationTypeCallCandidate
		if direction == model.ExpansionReverse {
			endpoint, targetEndpoint, seedKey, targetKey = "to_symbol_id", "from_symbol_id", "to_symbol_key_hash", "from_symbol_key_hash"
			relationType = model.RelationTypeTestRelation
		}
		query := s.db.WithContext(ctx).Table("symbol_relations AS r").
			Select("DISTINCT target.*, r.relation_type, r.resolution_kind, r.confidence, r.reason_code, '"+direction+"' AS direction").
			Joins("JOIN symbols AS target ON target.id = r."+targetEndpoint+" AND target.symbol_key_hash = r."+targetKey+" AND target.code_index_build_id = r.code_index_build_id").
			Where("r.code_index_build_id = ? AND r."+endpoint+" = ? AND r."+seedKey+" = ?", buildID, seed.ID, seedHash).
			Where("r.relation_type = ? AND r.resolution_kind = ? AND r.confidence >= ?", relationType, model.ResolutionKindSemantic, model.StructuralV2MinConfidence).
			Where("target.symbol_key_hash <> '' AND target.symbol_key_raw <> '' AND target.name <> '' AND target.file_path <> '' AND target.start_line > 0 AND target.end_line >= target.start_line")
		if direction == model.ExpansionReverse {
			query = query.Where("r.reason_code = ?", model.TestReasonDirectSemantic)
		}
		var candidates []model.StructuralExpansionCandidate
		result.RelationQueries++
		err := query.Order("target.symbol_key_hash ASC, r.confidence DESC, r.reason_code ASC").
			Limit(model.StructuralV2RelationLimit).Scan(&candidates).Error
		if err != nil {
			return result, err
		}
		result.RelationsExamined += len(candidates)
		result.Candidates = append(result.Candidates, candidates...)
	}
	return result, nil
}
