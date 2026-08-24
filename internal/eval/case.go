package eval

type LineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type EvalCase struct {
	CaseID             string               `json:"case_id"`
	DatasetVersion     string               `json:"dataset_version"`
	RepositoryName     string               `json:"repository_name"`
	SnapshotSHA        string               `json:"snapshot_sha"`
	IssueTitle         string               `json:"issue_title"`
	IssueDescription   string               `json:"issue_description"`
	ErrorLog           string               `json:"error_log"`
	ExpectedRootCause  string               `json:"expected_root_cause"`
	RelevantFiles      []string             `json:"relevant_files"`
	RelevantLineRanges map[string]LineRange `json:"relevant_line_ranges,omitempty"`
	ForbiddenClaims    []string             `json:"forbidden_claims,omitempty"`
}
