package evidence

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

// CitationRef is the only citation shape accepted from the Agent protocol.
// The legacy fields are decode-only compatibility for old checkpoints and
// providers; they are never used to construct a canonical citation.
type CitationRef struct {
	EvidenceID string `json:"evidence_id"`
	Reason     string `json:"reason"`

	LegacyPath      string `json:"path,omitempty"`
	LegacyStartLine int    `json:"start_line,omitempty"`
	LegacyEndLine   int    `json:"end_line,omitempty"`
	LegacyExcerpt   string `json:"excerpt,omitempty"`
}

type FindingDraft struct {
	Title     string        `json:"title"`
	Reasoning string        `json:"reasoning"`
	Citations []CitationRef `json:"citations"`
}

type ReportDraft struct {
	ConclusionKind         ConclusionKind `json:"conclusion_kind"`
	Summary                string         `json:"summary"`
	RootCause              string         `json:"root_cause"`
	Findings               []FindingDraft `json:"findings"`
	RecommendedChecks      []string       `json:"recommended_checks"`
	ConfirmedFacts         []string       `json:"confirmed_facts"`
	Limitations            []string       `json:"limitations"`
	ModelClaimedConfidence *float64       `json:"model_claimed_confidence"`

	// Confidence is accepted only for old v2.2 checkpoints. New prompts never
	// mention it and persistence uses ModelClaimedConfidence.
	LegacyConfidence *float64 `json:"confidence,omitempty"`
}

type DraftLineage struct {
	AttemptID        string
	DiagnosisRunID   string
	RepositoryID     string
	SnapshotID       string
	CodeIndexBuildID int64
}

type EvidenceVerifier interface {
	Verify(ctx context.Context, item *AttemptEvidenceItem, lineage DraftLineage) error
}

// ValidateReportDraftStructure applies the same structural rules used after
// evidence resolution. Empty or unknown evidence IDs are intentionally not a
// structural error: they become INVALID citations during resolution and keep
// the report's existing DEGRADED semantics.
func ValidateReportDraftStructure(draft *ReportDraft) error {
	if draft == nil {
		return errors.New("report draft is nil")
	}
	confidence := draft.ModelClaimedConfidence
	if confidence == nil {
		confidence = draft.LegacyConfidence
	}
	data := &DiagnosisReportData{
		ConclusionKind:         draft.ConclusionKind,
		Summary:                draft.Summary,
		RootCause:              draft.RootCause,
		RecommendedChecks:      append([]string(nil), draft.RecommendedChecks...),
		ConfirmedFacts:         append([]string(nil), draft.ConfirmedFacts...),
		Limitations:            append([]string(nil), draft.Limitations...),
		ModelClaimedConfidence: confidence,
		Findings:               make([]Finding, 0, len(draft.Findings)),
	}
	for _, finding := range draft.Findings {
		resolved := Finding{Title: finding.Title, Reasoning: finding.Reasoning, Citations: make([]Citation, 0, len(finding.Citations))}
		for _, ref := range finding.Citations {
			resolved.Citations = append(resolved.Citations, Citation{
				EvidenceID: ref.EvidenceID,
				// Legacy location fields are decode-only and never canonical. Apply
				// the canonical reason normalization before structural validation.
				Reason: limitCitationReason(ref.Reason),
			})
		}
		data.Findings = append(data.Findings, resolved)
	}
	return ValidateReportStructure(data)
}

// ResolveReportDraft converts model-owned evidence references into canonical
// Citation records. Unknown, cross-scope, or stale IDs remain visible as
// INVALID citations so the report can be persisted as DEGRADED.
func ResolveReportDraft(ctx context.Context, issuer EvidenceIssuer, draft *ReportDraft, lineage DraftLineage) (*DiagnosisReportData, error) {
	if draft == nil {
		return nil, errors.New("report draft is nil")
	}
	confidence := draft.ModelClaimedConfidence
	if confidence == nil {
		confidence = draft.LegacyConfidence
	}
	report := &DiagnosisReportData{
		ConclusionKind:         draft.ConclusionKind,
		Summary:                draft.Summary,
		RootCause:              draft.RootCause,
		RecommendedChecks:      append([]string(nil), draft.RecommendedChecks...),
		ConfirmedFacts:         append([]string(nil), draft.ConfirmedFacts...),
		Limitations:            append([]string(nil), draft.Limitations...),
		ModelClaimedConfidence: confidence,
		Findings:               make([]Finding, 0, len(draft.Findings)),
	}
	if err := ValidateReportDraftStructure(draft); err != nil {
		return nil, err
	}
	for _, finding := range draft.Findings {
		resolved := Finding{Title: finding.Title, Reasoning: finding.Reasoning, Citations: make([]Citation, 0, len(finding.Citations))}
		for _, ref := range finding.Citations {
			citation := Citation{
				EvidenceID:       strings.TrimSpace(ref.EvidenceID),
				Reason:           limitCitationReason(ref.Reason),
				SnapshotID:       lineage.SnapshotID,
				CodeIndexBuildID: lineage.CodeIndexBuildID,
				ValidationStatus: CitationInvalid,
			}
			if citation.EvidenceID == "" {
				citation.ValidationError = "EVIDENCE_ID_EMPTY"
				resolved.Citations = append(resolved.Citations, citation)
				continue
			}
			if issuer == nil {
				citation.ValidationError = "EVIDENCE_SOURCE_UNAVAILABLE"
				resolved.Citations = append(resolved.Citations, citation)
				continue
			}
			item, err := issuer.Resolve(ctx, lineage.AttemptID, citation.EvidenceID)
			if err != nil {
				citation.ValidationError = evidenceResolveError(err)
				resolved.Citations = append(resolved.Citations, citation)
				continue
			}
			if item.AttemptID != lineage.AttemptID {
				citation.ValidationError = "EVIDENCE_ATTEMPT_MISMATCH"
				resolved.Citations = append(resolved.Citations, citation)
				continue
			}
			if item.DiagnosisRunID != lineage.DiagnosisRunID || item.SnapshotID != lineage.SnapshotID || item.CodeIndexBuildID != lineage.CodeIndexBuildID {
				citation.ValidationError = "EVIDENCE_LINEAGE_MISMATCH"
				resolved.Citations = append(resolved.Citations, citation)
				continue
			}
			if verifier, ok := issuer.(EvidenceVerifier); ok {
				if verifyErr := verifier.Verify(ctx, item, lineage); verifyErr != nil {
					citation.ValidationError = evidenceResolveError(verifyErr)
					resolved.Citations = append(resolved.Citations, citation)
					continue
				}
			}
			citation.FilePath = item.FilePath
			citation.StartLine = item.StartLine
			citation.EndLine = item.EndLine
			citation.Excerpt = item.DisplayExcerpt
			citation.ContentHash = item.RawContentHash
			citation.ValidationStatus = CitationValid
			citation.ValidationError = ""
			resolved.Citations = append(resolved.Citations, citation)
		}
		report.Findings = append(report.Findings, resolved)
	}
	return report, nil
}

func limitCitationReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) > 2048 {
		cut := 2048
		for cut > 0 && !utf8.ValidString(reason[:cut]) {
			cut--
		}
		return reason[:cut]
	}
	return reason
}

func evidenceResolveError(err error) string {
	switch {
	case errors.Is(err, ErrEvidenceNotFound):
		return "EVIDENCE_NOT_FOUND"
	case errors.Is(err, ErrEvidenceAttemptMismatch):
		return "EVIDENCE_ATTEMPT_MISMATCH"
	case errors.Is(err, ErrEvidenceLineageMismatch):
		return "EVIDENCE_LINEAGE_MISMATCH"
	case errors.Is(err, ErrEvidenceHashMismatch):
		return "EVIDENCE_CONTENT_HASH_MISMATCH"
	case errors.Is(err, ErrEvidenceSourceUnavailable):
		return "EVIDENCE_SOURCE_UNAVAILABLE"
	default:
		return "EVIDENCE_SOURCE_UNAVAILABLE"
	}
}
