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
	ID                     int64     `json:"id,omitempty"`
	CodeIndexBuildID       int64     `json:"code_index_build_id,omitempty"`
	Path                   string    `json:"path"`
	PackagePath            string    `json:"package_path"`
	PackageName            string    `json:"package_name"`
	ContentHash            string    `json:"content_hash"`
	LineCount              int       `json:"line_count"`
	SizeBytes              int64     `json:"size_bytes"`
	IsTest                 bool      `json:"is_test"`
	IncludedByBuildContext bool      `json:"included_by_build_context"`
	ParseStatus            string    `json:"parse_status"` // "OK", "ERROR", "SKIPPED"
	ParseError             string    `json:"parse_error,omitempty"`
	CreatedAt              time.Time `json:"created_at,omitempty"`
}

// Symbol represents an extracted code symbol (function, method, type, interface).
type Symbol struct {
	ID                int64      `json:"id,omitempty"`
	CodeIndexBuildID  int64      `json:"code_index_build_id,omitempty"`
	FileID            int64      `json:"file_id,omitempty"`
	FilePath          string     `json:"file_path"`
	SymbolKeyRaw      string     `json:"symbol_key_raw"`
	SymbolKeyHash     string     `json:"symbol_key_hash"`
	ModulePath        string     `json:"module_path"`
	PackagePath       string     `json:"package_path"`
	PackageName       string     `json:"package_name"`
	Kind              SymbolKind `json:"kind"`
	Name              string     `json:"name"`
	QualifiedName     string     `json:"qualified_name"`
	ReceiverRaw       string     `json:"receiver_raw,omitempty"`
	ReceiverCanonical string     `json:"receiver_canonical,omitempty"`
	Signature         string     `json:"signature"`
	Doc               string     `json:"doc,omitempty"`
	StartLine         int        `json:"start_line"`
	StartCol          int        `json:"start_col"`
	EndLine           int        `json:"end_line"`
	EndCol            int        `json:"end_col"`
	Exported          bool       `json:"exported"`
	ContentHash       string     `json:"content_hash"`
	SourceExcerpt     string     `json:"source_excerpt,omitempty"`
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
	ID                  int64          `json:"id,omitempty"`
	CodeIndexBuildID    int64          `json:"code_index_build_id,omitempty"`
	FromSymbolID        *int64         `json:"from_symbol_id,omitempty"`
	FromSymbolKeyHash   string         `json:"from_symbol_key_hash,omitempty"`
	ToSymbolID          *int64         `json:"to_symbol_id,omitempty"`
	ToSymbolKeyHash     string         `json:"to_symbol_key_hash,omitempty"`
	RelationType        RelationType   `json:"relation_type"`
	ResolutionKind      ResolutionKind `json:"resolution_kind"`
	Confidence          float64        `json:"confidence"`
	ReasonCode          string         `json:"reason_code"`
	ReasonDetail        string         `json:"reason_detail,omitempty"`
	TargetName          string         `json:"target_name,omitempty"`
	TargetPackagePath   string         `json:"target_package_path,omitempty"`
	TargetQualifiedName string         `json:"target_qualified_name,omitempty"`
	FilePath            string         `json:"file_path"`
	FileID              int64          `json:"file_id,omitempty"`
	Line                int            `json:"line"`
	Column              int            `json:"column"`
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
