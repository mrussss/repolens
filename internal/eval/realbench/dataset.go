package realbench

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var fullSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
var caseIDPattern = regexp.MustCompile(`^REAL-[0-9]{3}$`)

// Manifest identifies the frozen RealBench dataset split.
type Manifest struct {
	DatasetVersion string   `json:"dataset_version"`
	Cases          []string `json:"cases"`
	ManifestHash   string   `json:"manifest_hash"`
}

type Repository struct {
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
}

// Input is the only case data that the retrieval or Agent path may receive.
type Input struct {
	CaseID           string     `json:"case_id"`
	DatasetVersion   string     `json:"dataset_version"`
	Repository       Repository `json:"repository"`
	BuggyCommitSHA   string     `json:"buggy_commit_sha"`
	IssueTitle       string     `json:"issue_title"`
	IssueDescription string     `json:"issue_description"`
	ErrorLog         string     `json:"error_log"`
}

type LineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type Provenance struct {
	IssueURL     string `json:"issue_url"`
	FixPRURL     string `json:"fix_pr_url,omitempty"`
	FixCommitURL string `json:"fix_commit_url"`
}

// GroundTruth is evaluator-only data. It is deliberately loaded separately
// from Input and is never passed to the production retrieval or Agent path.
type GroundTruth struct {
	CaseID             string               `json:"case_id"`
	FixCommitSHA       string               `json:"fix_commit_sha"`
	ExpectedRootCause  string               `json:"expected_root_cause"`
	PrimaryFiles       []string             `json:"primary_relevant_files"`
	SupportingFiles    []string             `json:"supporting_files,omitempty"`
	RelevantSymbols    []string             `json:"relevant_symbols,omitempty"`
	RelevantLineRanges map[string]LineRange `json:"relevant_line_ranges,omitempty"`
	Provenance         Provenance           `json:"provenance"`
	CurationNotes      string               `json:"curation_notes"`
}

type InputCase struct {
	Input   Input
	CaseDir string
}

type Dataset struct {
	Root     string
	Manifest Manifest
	Inputs   []InputCase
}

func LoadInputs(root string) (*Dataset, error) {
	manifest, err := readManifest(root)
	if err != nil {
		return nil, err
	}
	if err := validateManifestHeader(manifest); err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(manifest.Cases))
	inputs := make([]InputCase, 0, len(manifest.Cases))
	for _, caseID := range manifest.Cases {
		if seen[caseID] {
			return nil, fmt.Errorf("duplicate case id %q in manifest", caseID)
		}
		seen[caseID] = true
		caseDir := filepath.Join(root, caseID)
		var input Input
		if err := readJSON(filepath.Join(caseDir, "input.json"), &input); err != nil {
			return nil, fmt.Errorf("%s input: %w", caseID, err)
		}
		if input.CaseID != caseID {
			return nil, fmt.Errorf("%s input: case_id %q does not match directory/manifest case %q", caseID, input.CaseID, caseID)
		}
		if err := validateInput(manifest, input); err != nil {
			return nil, fmt.Errorf("%s input: %w", caseID, err)
		}
		inputs = append(inputs, InputCase{Input: input, CaseDir: caseDir})
	}
	return &Dataset{Root: root, Manifest: manifest, Inputs: inputs}, nil
}

func (d *Dataset) Input(caseID string) (InputCase, bool) {
	for _, c := range d.Inputs {
		if c.Input.CaseID == caseID {
			return c, true
		}
	}
	return InputCase{}, false
}

func (d *Dataset) LoadGroundTruth(caseID string) (GroundTruth, error) {
	caseInput, ok := d.Input(caseID)
	if !ok {
		return GroundTruth{}, fmt.Errorf("case %s is not in dataset", caseID)
	}
	var truth GroundTruth
	if err := readJSON(filepath.Join(caseInput.CaseDir, "ground_truth.json"), &truth); err != nil {
		return GroundTruth{}, fmt.Errorf("%s ground truth: %w", caseID, err)
	}
	if truth.CaseID != caseID {
		return GroundTruth{}, fmt.Errorf("ground truth case_id %q does not match requested case %q", truth.CaseID, caseID)
	}
	if err := validateGroundTruth(d.Manifest, truth); err != nil {
		return GroundTruth{}, fmt.Errorf("%s ground truth: %w", caseID, err)
	}
	return truth, nil
}

// Validate performs the complete offline schema, pairing, provenance, and
// manifest hash validation. It never performs network access.
func Validate(root string) (string, error) {
	dataset, err := LoadInputs(root)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("read dataset root: %w", err)
	}
	manifestCases := make(map[string]bool, len(dataset.Manifest.Cases))
	for _, id := range dataset.Manifest.Cases {
		manifestCases[id] = true
	}
	for _, entry := range entries {
		if entry.IsDir() && !manifestCases[entry.Name()] {
			return "", fmt.Errorf("case directory %q is not listed in manifest", entry.Name())
		}
	}

	for _, inputCase := range dataset.Inputs {
		truth, err := dataset.LoadGroundTruth(inputCase.Input.CaseID)
		if err != nil {
			return "", err
		}
		if err := validateNoLeakage(inputCase.Input, truth); err != nil {
			return "", fmt.Errorf("%s leakage check: %w", inputCase.Input.CaseID, err)
		}
	}

	hash, err := ComputeManifestHash(root, dataset.Manifest.Cases)
	if err != nil {
		return "", err
	}
	if dataset.Manifest.ManifestHash == "" {
		return hash, errors.New("manifest_hash is missing; use the computed hash to freeze the dataset")
	}
	if !strings.EqualFold(dataset.Manifest.ManifestHash, hash) {
		return hash, fmt.Errorf("manifest hash mismatch: manifest=%s computed=%s", dataset.Manifest.ManifestHash, hash)
	}
	return hash, nil
}

func ComputeManifestHash(root string, caseIDs []string) (string, error) {
	ordered := append([]string(nil), caseIDs...)
	sort.Strings(ordered)
	records := make([]struct {
		CaseID string      `json:"case_id"`
		Input  Input       `json:"input"`
		Truth  GroundTruth `json:"ground_truth"`
	}, 0, len(ordered))
	for _, caseID := range ordered {
		var input Input
		var truth GroundTruth
		caseDir := filepath.Join(root, caseID)
		if err := readJSON(filepath.Join(caseDir, "input.json"), &input); err != nil {
			return "", fmt.Errorf("%s input: %w", caseID, err)
		}
		if err := readJSON(filepath.Join(caseDir, "ground_truth.json"), &truth); err != nil {
			return "", fmt.Errorf("%s ground truth: %w", caseID, err)
		}
		records = append(records, struct {
			CaseID string      `json:"case_id"`
			Input  Input       `json:"input"`
			Truth  GroundTruth `json:"ground_truth"`
		}{caseID, input, truth})
	}
	data, err := json.Marshal(records)
	if err != nil {
		return "", fmt.Errorf("marshal manifest hash input: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func readManifest(root string) (Manifest, error) {
	var manifest Manifest
	if err := readJSON(filepath.Join(root, "manifest.json"), &manifest); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	return manifest, nil
}

func readJSON(path string, target interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func validateManifestHeader(manifest Manifest) error {
	if manifest.DatasetVersion == "" {
		return errors.New("dataset_version is required")
	}
	if len(manifest.Cases) == 0 {
		return errors.New("cases must not be empty")
	}
	for _, caseID := range manifest.Cases {
		if !caseIDPattern.MatchString(caseID) {
			return fmt.Errorf("invalid case id %q", caseID)
		}
	}
	return nil
}

func validateInput(manifest Manifest, input Input) error {
	if !caseIDPattern.MatchString(input.CaseID) {
		return fmt.Errorf("invalid case_id %q", input.CaseID)
	}
	if input.DatasetVersion != manifest.DatasetVersion {
		return fmt.Errorf("dataset_version %q does not match manifest %q", input.DatasetVersion, manifest.DatasetVersion)
	}
	if input.Repository.FullName == "" || input.Repository.CloneURL == "" {
		return errors.New("repository.full_name and repository.clone_url are required")
	}
	if err := validateGitHubURL(input.Repository.CloneURL); err != nil {
		return fmt.Errorf("repository.clone_url: %w", err)
	}
	if !fullSHA.MatchString(input.BuggyCommitSHA) {
		return fmt.Errorf("buggy_commit_sha must be a full 40-character SHA")
	}
	if strings.TrimSpace(input.IssueTitle) == "" || strings.TrimSpace(input.IssueDescription) == "" {
		return errors.New("issue_title and issue_description are required")
	}
	if strings.TrimSpace(input.ErrorLog) == "" {
		return errors.New("error_log is required")
	}
	return nil
}

func validateGroundTruth(manifest Manifest, truth GroundTruth) error {
	if !caseIDPattern.MatchString(truth.CaseID) {
		return fmt.Errorf("invalid case_id %q", truth.CaseID)
	}
	if !fullSHA.MatchString(truth.FixCommitSHA) {
		return errors.New("fix_commit_sha must be a full 40-character SHA")
	}
	if strings.TrimSpace(truth.ExpectedRootCause) == "" {
		return errors.New("expected_root_cause is required")
	}
	if len(truth.PrimaryFiles) == 0 {
		return errors.New("primary_relevant_files must not be empty")
	}
	if hasDuplicates(truth.PrimaryFiles) || hasDuplicates(truth.SupportingFiles) {
		return errors.New("relevant file lists must not contain duplicates")
	}
	for _, file := range append(append([]string{}, truth.PrimaryFiles...), truth.SupportingFiles...) {
		if err := validateRelativePath(file); err != nil {
			return err
		}
	}
	for file, lineRange := range truth.RelevantLineRanges {
		if err := validateRelativePath(file); err != nil {
			return err
		}
		if !containsString(append(append([]string{}, truth.PrimaryFiles...), truth.SupportingFiles...), file) {
			return fmt.Errorf("line range file %q is not listed as primary or supporting", file)
		}
		if lineRange.Start <= 0 || lineRange.End < lineRange.Start {
			return fmt.Errorf("invalid line range for %s: %d-%d", file, lineRange.Start, lineRange.End)
		}
	}
	if err := validateGitHubURL(truth.Provenance.IssueURL); err != nil {
		return fmt.Errorf("provenance.issue_url: %w", err)
	}
	if truth.Provenance.FixPRURL != "" {
		if err := validateGitHubURL(truth.Provenance.FixPRURL); err != nil {
			return fmt.Errorf("provenance.fix_pr_url: %w", err)
		}
	}
	if err := validateGitHubURL(truth.Provenance.FixCommitURL); err != nil {
		return fmt.Errorf("provenance.fix_commit_url: %w", err)
	}
	if strings.TrimSpace(truth.CurationNotes) == "" {
		return errors.New("curation_notes is required")
	}
	if truth.CaseID != "" && !containsCase(manifest.Cases, truth.CaseID) {
		return fmt.Errorf("case_id %q is not listed in manifest", truth.CaseID)
	}
	return nil
}

func containsCase(cases []string, wanted string) bool {
	for _, caseID := range cases {
		if caseID == wanted {
			return true
		}
	}
	return false
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validateNoLeakage(input Input, truth GroundTruth) error {
	inputBytes, err := json.Marshal(input)
	if err != nil {
		return err
	}
	inputText := string(inputBytes)
	for _, sentinel := range []string{"DO_NOT_LEAK_GROUND_TRUTH", "DO_NOT_LEAK_SECRET_GROUND_TRUTH"} {
		if strings.Contains(inputText, sentinel) {
			return fmt.Errorf("ground-truth sentinel %q appears in input", sentinel)
		}
	}
	return nil
}

func validateGitHubURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || strings.ToLower(u.Scheme) != "https" {
		return errors.New("must be an HTTPS URL")
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && !strings.HasSuffix(host, ".github.com") {
		return fmt.Errorf("host %q is not github.com", host)
	}
	if u.Path == "" || u.Path == "/" {
		return errors.New("path is required")
	}
	return nil
}

func validateRelativePath(path string) error {
	clean := filepath.Clean(path)
	if path == "" || clean == "." || filepath.IsAbs(path) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid relative file path %q", path)
	}
	return nil
}

func hasDuplicates(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}
