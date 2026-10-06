package model

// Fixed experimental budgets are part of RetrievalConfigHash. They are not
// runtime knobs. RelationLimit bounds materialized rows per allowed direction,
// including lookahead past targets already in the lexical pool or artifact gaps.
const (
	StructuralV2SeedBudget      = 8
	StructuralV2PerSeedBudget   = 1
	StructuralV2ExpansionBudget = 4
	StructuralV2Depth           = 1
	StructuralV2MinConfidence   = 0.95
	StructuralV2RelationLimit   = 8
	ExpansionForward            = "FORWARD"
	ExpansionReverse            = "REVERSE"
)

// StructuralExpansionCandidate is a resolved symbol joined to a permitted edge.
// It is a query projection, not a new persisted entity or graph schema.
type StructuralExpansionCandidate struct {
	Symbol         Symbol         `gorm:"embedded"`
	RelationType   RelationType   `json:"relation_type"`
	Direction      string         `json:"direction"`
	ResolutionKind ResolutionKind `json:"resolution_kind"`
	Confidence     float64        `json:"confidence"`
	ReasonCode     string         `json:"reason_code"`
}

type StructuralExpansionCandidates struct {
	Candidates        []StructuralExpansionCandidate
	SymbolQueries     int
	RelationQueries   int
	RelationsExamined int // Returned distinct rows, not database-internal scan work.
}
