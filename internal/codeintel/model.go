package codeintel

import (
	"repolens/internal/codeintel/model"
)

type SymbolKind = model.SymbolKind

const (
	SymbolKindFunction  = model.SymbolKindFunction
	SymbolKindMethod    = model.SymbolKindMethod
	SymbolKindType      = model.SymbolKindType
	SymbolKindInterface = model.SymbolKindInterface
)

type RelationType = model.RelationType

const (
	RelationTypeReference     = model.RelationTypeReference
	RelationTypeCallCandidate = model.RelationTypeCallCandidate
	RelationTypeTestRelation  = model.RelationTypeTestRelation
)

type ResolutionKind = model.ResolutionKind

const (
	ResolutionKindSemantic   = model.ResolutionKindSemantic
	ResolutionKindSyntactic  = model.ResolutionKindSyntactic
	ResolutionKindHeuristic  = model.ResolutionKindHeuristic
	ResolutionKindUnresolved = model.ResolutionKindUnresolved
)

type TestRelationReason = model.TestRelationReason

const (
	TestReasonDirectSemantic  = model.TestReasonDirectSemantic
	TestReasonDirectSyntactic = model.TestReasonDirectSyntactic
	TestReasonNameMatch       = model.TestReasonNameMatch
	TestReasonSamePackage     = model.TestReasonSamePackage
)

type BuildContext = model.BuildContext

var DefaultBuildContext = model.DefaultBuildContext

type CodeFile = model.CodeFile
type Symbol = model.Symbol
type SymbolRelation = model.SymbolRelation
type RelatedTestDiscovery = model.RelatedTestDiscovery
type AnalysisQuality = model.AnalysisQuality
type AnalysisResult = model.AnalysisResult

var BuildSymbolKey = model.BuildSymbolKey
var CanonicalizeReceiver = model.CanonicalizeReceiver
