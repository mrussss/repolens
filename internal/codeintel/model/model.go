package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// SymbolKind represents the type of a code symbol.
type SymbolKind string

const (
	SymbolKindFunction  SymbolKind = "FUNCTION"
	SymbolKindMethod    SymbolKind = "METHOD"
	SymbolKindType      SymbolKind = "TYPE"
	SymbolKindInterface SymbolKind = "INTERFACE"
)

// RelationType represents the category of relationship between code symbols.
type RelationType string

const (
	RelationTypeReference     RelationType = "REFERENCE"
	RelationTypeCallCandidate RelationType = "CALL_CANDIDATE"
	RelationTypeTestRelation  RelationType = "TEST_RELATION"
)

// ResolutionKind represents the certainty level of the relation resolution.
type ResolutionKind string

const (
	ResolutionKindSemantic   ResolutionKind = "SEMANTIC"
	ResolutionKindSyntactic  ResolutionKind = "SYNTACTIC"
	ResolutionKindHeuristic  ResolutionKind = "HEURISTIC"
	ResolutionKindUnresolved ResolutionKind = "UNRESOLVED"
)

// TestRelationReason represents the reason code for related test discovery.
type TestRelationReason string

const (
	TestReasonDirectSemantic  TestRelationReason = "DIRECT_SEMANTIC_USAGE"
	TestReasonDirectSyntactic TestRelationReason = "DIRECT_SYNTACTIC_USAGE"
	TestReasonNameMatch       TestRelationReason = "NAME_MATCH"
	TestReasonSamePackage     TestRelationReason = "SAME_PACKAGE"
)

// BuildContext defines the target build environment constraints.
type BuildContext struct {
	GOOS      string   `json:"goos"`
	GOARCH    string   `json:"goarch"`
	BuildTags []string `json:"build_tags"`
}

// DefaultBuildContext returns the standard default linux/amd64 build context.
func DefaultBuildContext() BuildContext {
	return BuildContext{
		GOOS:      "linux",
		GOARCH:    "amd64",
		BuildTags: []string{},
	}
}

// BuildContextHash computes the SHA256 hash of the build context.
func (bc BuildContext) BuildContextHash() string {
	raw := fmt.Sprintf("%s|%s|%s", bc.GOOS, bc.GOARCH, strings.Join(bc.BuildTags, ","))
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// CodeFile represents a parsed source file within the codebase.
type CodeFile struct {
	ID                     int64     `json:"id,omitempty" gorm:"primaryKey;autoIncrement"`
	CodeIndexBuildID       int64     `json:"code_index_build_id,omitempty" gorm:"not null;index:ix_file_build"`
	Path                   string    `json:"path" gorm:"size:512;not null;index:ix_file_path"`
	PackagePath            string    `json:"package_path" gorm:"size:255;not null"`
	PackageName            string    `json:"package_name" gorm:"size:128;not null"`
	ContentHash            string    `json:"content_hash" gorm:"size:64;not null"`
	LineCount              int       `json:"line_count" gorm:"not null;default:0"`
	SizeBytes              int64     `json:"size_bytes" gorm:"not null;default:0"`
	IsTest                 bool      `json:"is_test" gorm:"not null;default:false"`
	IncludedByBuildContext bool      `json:"included_by_build_context" gorm:"not null;default:true"`
	ParseStatus            string    `json:"parse_status" gorm:"size:32;not null;default:'OK'"` // "OK", "ERROR", "SKIPPED"
	ParseError             string    `json:"parse_error,omitempty" gorm:"type:text"`
	CreatedAt              time.Time `json:"created_at,omitempty"`
}

func (CodeFile) TableName() string {
	return "code_files"
}

// Symbol represents an extracted code symbol (function, method, type, interface).
type Symbol struct {
	ID                int64      `json:"id,omitempty" gorm:"primaryKey;autoIncrement"`
	CodeIndexBuildID  int64      `json:"code_index_build_id,omitempty" gorm:"not null;index:ix_sym_build;index:ix_sym_lookup,priority:1"`
	FileID            int64      `json:"file_id,omitempty" gorm:"not null;index:ix_sym_file"`
	FilePath          string     `json:"file_path" gorm:"size:512;not null"`
	SymbolKeyRaw      string     `json:"symbol_key_raw" gorm:"size:512;not null"`
	SymbolKeyHash     string     `json:"symbol_key_hash" gorm:"size:64;not null;index:ix_sym_lookup,priority:2"`
	ModulePath        string     `json:"module_path" gorm:"size:255;not null"`
	PackagePath       string     `json:"package_path" gorm:"size:255;not null"`
	PackageName       string     `json:"package_name" gorm:"size:128;not null"`
	Kind              SymbolKind `json:"kind" gorm:"size:32;not null"`
	Name              string     `json:"name" gorm:"size:128;not null;index:ix_sym_name"`
	QualifiedName     string     `json:"qualified_name" gorm:"size:255;not null"`
	ReceiverRaw       string     `json:"receiver_raw,omitempty" gorm:"size:128"`
	ReceiverCanonical string     `json:"receiver_canonical,omitempty" gorm:"size:128"`
	Signature         string     `json:"signature" gorm:"type:text"`
	Doc               string     `json:"doc,omitempty" gorm:"type:text"`
	StartLine         int        `json:"start_line" gorm:"not null"`
	StartCol          int        `json:"start_col" gorm:"not null"`
	EndLine           int        `json:"end_line" gorm:"not null"`
	EndCol            int        `json:"end_col" gorm:"not null"`
	Exported          bool       `json:"exported" gorm:"not null;default:false"`
	ContentHash       string     `json:"content_hash" gorm:"size:64;not null"`
	SourceExcerpt     string     `json:"source_excerpt,omitempty" gorm:"-"`
}

func (Symbol) TableName() string {
	return "symbols"
}

// BuildSymbolKey generates the raw symbol key and its SHA256 hash.
// Raw format: module_path|package_path|receiver_canonical|kind|name
func BuildSymbolKey(modulePath, packagePath, receiverCanonical string, kind SymbolKind, name string) (raw string, hash string) {
	raw = fmt.Sprintf("%s|%s|%s|%s|%s", modulePath, packagePath, receiverCanonical, kind, name)
	h := sha256.Sum256([]byte(raw))
	hash = hex.EncodeToString(h[:])
	return raw, hash
}

// CanonicalizeReceiver extracts the base type name from a receiver expression.
// e.g. "*Service" -> "Service", "(s *Service)" -> "Service", "Service" -> "Service".
func CanonicalizeReceiver(recv string) string {
	recv = strings.TrimSpace(recv)
	// Remove outer parens
	for strings.HasPrefix(recv, "(") && strings.HasSuffix(recv, ")") {
		recv = strings.TrimSpace(recv[1 : len(recv)-1])
	}
	if idx := strings.IndexAny(recv, "*["); idx != -1 {
		prefix := strings.TrimSpace(recv[:idx])
		if prefix != "" {
			parts := strings.Fields(prefix)
			if len(parts) >= 1 && strings.HasPrefix(recv[idx:], "*") {
				recv = recv[idx:]
			}
		}
	} else {
		parts := strings.Fields(recv)
		if len(parts) >= 2 {
			recv = parts[len(parts)-1]
		}
	}
	// Strip pointer asterisk and any remaining whitespace
	recv = strings.TrimPrefix(recv, "*")
	recv = strings.TrimSpace(recv)
	// Strip type parameters for generics, e.g. Stack[T] -> Stack
	if idx := strings.Index(recv, "["); idx != -1 {
		recv = strings.TrimSpace(recv[:idx])
	}
	return recv
}

// SymbolRelation represents a relationship between symbols or an unresolved reference.
type SymbolRelation struct {
	ID                  int64          `json:"id,omitempty" gorm:"primaryKey;autoIncrement"`
	CodeIndexBuildID    int64          `json:"code_index_build_id,omitempty" gorm:"not null;index:ix_rel_build"`
	FromSymbolID        *int64         `json:"from_symbol_id,omitempty" gorm:"index:ix_rel_from"`
	FromSymbolKeyHash   string         `json:"from_symbol_key_hash,omitempty" gorm:"size:64"`
	ToSymbolID          *int64         `json:"to_symbol_id,omitempty" gorm:"index:ix_rel_to"`
	ToSymbolKeyHash     string         `json:"to_symbol_key_hash,omitempty" gorm:"size:64"`
	RelationType        RelationType   `json:"relation_type" gorm:"size:32;not null"`
	ResolutionKind      ResolutionKind `json:"resolution_kind" gorm:"size:32;not null"`
	Confidence          float64        `json:"confidence" gorm:"not null;default:1.0"`
	ReasonCode          string         `json:"reason_code" gorm:"size:64;not null"`
	ReasonDetail        string         `json:"reason_detail,omitempty" gorm:"size:255"`
	TargetName          string         `json:"target_name,omitempty" gorm:"size:128"`
	TargetPackagePath   string         `json:"target_package_path,omitempty" gorm:"size:255"`
	TargetQualifiedName string         `json:"target_qualified_name,omitempty" gorm:"size:255"`
	FilePath            string         `json:"file_path" gorm:"size:512;not null"`
	FileID              int64          `json:"file_id,omitempty" gorm:"not null"`
	Line                int            `json:"line" gorm:"not null"`
	Column              int            `json:"column" gorm:"not null"`
}

func (SymbolRelation) TableName() string {
	return "symbol_relations"
}

// RelatedTestDiscovery represents a discovered test symbol linked to a target symbol.
type RelatedTestDiscovery struct {
	TargetSymbolKeyHash string             `json:"target_symbol_key_hash"`
	TargetSymbolName    string             `json:"target_symbol_name"`
	TestSymbolKeyHash   string             `json:"test_symbol_key_hash"`
	TestSymbolName      string             `json:"test_symbol_name"`
	TestFilePath        string             `json:"test_file_path"`
	ReasonCode          TestRelationReason `json:"reason_code"`
	ResolutionKind      ResolutionKind     `json:"resolution_kind"`
	Confidence          float64            `json:"confidence"`
	Explanation         string             `json:"explanation"`
	TestLine            int                `json:"test_line"`
}

// AnalysisQuality captures the completeness and certainty distribution of the analysis.
type AnalysisQuality struct {
	FilesTotal               int      `json:"files_total"`
	FilesParsed              int      `json:"files_parsed"`
	FilesFailed              int      `json:"files_failed"`
	PackagesTotal            int      `json:"packages_total"`
	PackagesTypechecked      int      `json:"packages_typechecked"`
	PackagesFailed           int      `json:"packages_failed"`
	SymbolsTotal             int      `json:"symbols_total"`
	SemanticRelationsCount   int      `json:"semantic_relations_count"`
	SyntacticRelationsCount  int      `json:"syntactic_relations_count"`
	HeuristicRelationsCount  int      `json:"heuristic_relations_count"`
	UnresolvedRelationsCount int      `json:"unresolved_relations_count"`
	Warnings                 []string `json:"warnings"`
}

// AnalysisResult is the comprehensive result of indexing a codebase.
type AnalysisResult struct {
	ModulePath   string                  `json:"module_path"`
	BuildContext BuildContext            `json:"build_context"`
	Files        []*CodeFile             `json:"files"`
	Symbols      []*Symbol               `json:"symbols"`
	Relations    []*SymbolRelation       `json:"relations"`
	RelatedTests []*RelatedTestDiscovery `json:"related_tests"`
	Quality      AnalysisQuality         `json:"quality"`
}
