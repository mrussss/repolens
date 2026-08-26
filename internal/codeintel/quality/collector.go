package quality

import (
	"repolens/internal/codeintel/model"
)

// ComputeQuality aggregates metrics from files, packages, symbols, and relations.
func ComputeQuality(files []*model.CodeFile, symbols []*model.Symbol, relations []*model.SymbolRelation, pkgTotal, pkgTypechecked, pkgFailed int, warnings []string) model.AnalysisQuality {
	q := model.AnalysisQuality{
		FilesTotal:          len(files),
		PackagesTotal:       pkgTotal,
		PackagesTypechecked: pkgTypechecked,
		PackagesFailed:      pkgFailed,
		SymbolsTotal:        len(symbols),
		Warnings:            warnings,
	}

	for _, f := range files {
		if f.ParseStatus == "OK" {
			q.FilesParsed++
		} else if f.ParseStatus == "ERROR" {
			q.FilesFailed++
		}
	}

	for _, r := range relations {
		switch r.ResolutionKind {
		case model.ResolutionKindSemantic:
			q.SemanticRelationsCount++
		case model.ResolutionKindSyntactic:
			q.SyntacticRelationsCount++
		case model.ResolutionKindHeuristic:
			q.HeuristicRelationsCount++
		case model.ResolutionKindUnresolved:
			q.UnresolvedRelationsCount++
		}
	}

	return q
}
