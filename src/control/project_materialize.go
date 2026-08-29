package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// A downstream project step may only consume a result that was retained by the
// buyer's job receipt. Keeping this materially smaller than a project input
// prevents a control-plane downloader from becoming an unbounded data plane.
const projectMaterializeMaxBytes = 64 << 20

// ProjectMaterialization is the buyer-side bridge from a completed Merc job to
// one declared project artifact. It is evidence for artifact hand-off only;
// a separate command re-quotes and submits a dependent step against it.
type ProjectMaterialization struct {
	Version               int    `json:"version"`
	IRSHA256              string `json:"ir_sha256"`
	ProjectID             string `json:"project_id"`
	StepID                string `json:"step_id"`
	JobID                 string `json:"job_id"`
	Output                string `json:"output"`
	Bytes                 int64  `json:"bytes"`
	SHA256                string `json:"sha256"`
	PricingDecisionSHA256 string `json:"pricing_decision_sha256"`
	AuthorityQuoteSHA256  string `json:"authority_quote_sha256"`
	MaterializedAt        string `json:"materialized_at"`
}

// runProjectMaterialize is intentionally a separate command from submit. A
// project cannot continue merely because a job was accepted: the buyer must
// first retrieve a completed, receipt-bound output artifact. A later dependent
// executor will consume this receipt rather than trust a mutable local file.
func runProjectMaterialize(args []string) int {
	fs := flag.NewFlagSet("project materialize", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "", "project directory")
	irPath := fs.String("ir", "", "buyer-approved probed Project IR artifact")
	submissionPath := fs.String("submission", "", "accepted project submission artifact")
	stepID := fs.String("step", "", "completed project step id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" || *irPath == "" || *submissionPath == "" || *stepID == "" {
		fmt.Fprintln(os.Stderr, "project materialize: --root, --ir, --submission and --step are required")
		return 2
	}
	var ir ProjectWorkloadIR
	if err := readProjectArtifact(*irPath, &ir); err != nil {
		fmt.Fprintf(os.Stderr, "project materialize: invalid IR artifact: %v\n", err)
		return 1
	}
	var submission ProjectSubmission
	if err := readProjectArtifact(*submissionPath, &submission); err != nil {
		fmt.Fprintf(os.Stderr, "project materialize: invalid submission artifact: %v\n", err)
		return 1
	}
	result, err := materializeProjectStep(newClient(), *root, ir, submission, *stepID, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "project materialize: %v\n", err)
		return 1
	}
	if err := writeProjectMaterialization(os.Stdout, result); err != nil {
		fmt.Fprintf(os.Stderr, "project materialize: %v\n", err)
		return 1
	}
	return 0
}

func readProjectArtifact(path string, out any) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > projectQuoteArtifactMaxBytes {
		return fmt.Errorf("artifact must be a regular non-symlink file no larger than %d bytes", projectQuoteArtifactMaxBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeStrictJSONObject(raw, out)
}

func materializeProjectStep(c *client, root string, ir ProjectWorkloadIR, submission ProjectSubmission, stepID string, now time.Time) (ProjectMaterialization, error) {
	if ir.IRSHA256 == "" || !validSHA256(ir.IRSHA256) || submission.IRSHA256 != ir.IRSHA256 || submission.Status != "ACCEPTED" {
		return ProjectMaterialization{}, errors.New("materialization requires the exact accepted Project IR submission")
	}
	if _, err := uuid.Parse(submission.ProjectID); err != nil {
		return ProjectMaterialization{}, errors.New("materialization requires the server project order id from the accepted submission")
	}
	var step *ProjectIRStep
	for i := range ir.Steps {
		if ir.Steps[i].ID == stepID {
			step = &ir.Steps[i]
			break
		}
	}
	if step == nil || len(step.Outputs) != 1 || !validProjectArtifactRef(step.Outputs[0]) {
		return ProjectMaterialization{}, errors.New("materialization requires exactly one declared project:// output")
	}
	var submitted *ProjectStepSubmission
	for i := range submission.Steps {
		if submission.Steps[i].StepID == stepID {
			submitted = &submission.Steps[i]
			break
		}
	}
	if submitted == nil || !validSHA256(submitted.PricingDecisionSHA256) || !validSHA256(submitted.AuthorityQuoteSHA256) {
		return ProjectMaterialization{}, errors.New("project submission has no complete authority for the declared step")
	}
	jobID, err := uuid.Parse(submitted.JobID)
	if err != nil {
		return ProjectMaterialization{}, fmt.Errorf("project step job id: %w", err)
	}
	receipt, err := projectReceipt(c, jobID)
	if err != nil {
		return ProjectMaterialization{}, err
	}
	if receipt.JobID != jobID || receipt.Status != "complete" || receipt.AuthorityStatus != "verified" ||
		receipt.Authority.PricingDecisionSHA256 != submitted.PricingDecisionSHA256 {
		return ProjectMaterialization{}, errors.New("completed job receipt does not bind the submitted pricing authority")
	}
	results, err := projectResults(c, jobID)
	if err != nil {
		return ProjectMaterialization{}, err
	}
	if results.JobID != jobID || results.Status != "complete" || results.ResultsExpired || results.ResultsURL == "" {
		return ProjectMaterialization{}, errors.New("completed job has no retained merged result artifact")
	}
	body, err := downloadProjectArtifact(c, results.ResultsURL)
	if err != nil {
		return ProjectMaterialization{}, err
	}
	if err := writeDeclaredProjectOutput(root, step.Outputs[0], body); err != nil {
		return ProjectMaterialization{}, err
	}
	sum := sha256.Sum256(body)
	return ProjectMaterialization{Version: 2, IRSHA256: ir.IRSHA256, ProjectID: submission.ProjectID, StepID: stepID, JobID: jobID.String(),
		Output: step.Outputs[0], Bytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:]),
		PricingDecisionSHA256: submitted.PricingDecisionSHA256, AuthorityQuoteSHA256: submitted.AuthorityQuoteSHA256,
		MaterializedAt: now.UTC().Format(time.RFC3339Nano)}, nil
}

func projectReceipt(c *client, jobID uuid.UUID) (ClearingReceipt, error) {
	var out ClearingReceipt
	if err := projectGetJSON(c, "/v1/jobs/"+jobID.String()+"/receipt", &out); err != nil {
		return ClearingReceipt{}, fmt.Errorf("project job receipt: %w", err)
	}
	return out, nil
}

func projectResults(c *client, jobID uuid.UUID) (JobResults, error) {
	var out JobResults
	if err := projectGetJSON(c, "/v1/jobs/"+jobID.String()+"/results", &out); err != nil {
		return JobResults{}, fmt.Errorf("project job results: %w", err)
	}
	return out, nil
}

func projectGetJSON(c *client, path string, out any) error {
	if c == nil || c.hc == nil {
		return errors.New("project API client is not configured")
	}
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONRequestBodyBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxJSONRequestBodyBytes {
		return errors.New("project API response exceeds bounded size")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("job endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return decodeStrictJSONObject(raw, out)
}

func downloadProjectArtifact(c *client, rawURL string) ([]byte, error) {
	if c == nil || c.hc == nil || strings.TrimSpace(rawURL) == "" {
		return nil, errors.New("project artifact URL is not configured")
	}
	resp, err := c.hc.Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("project artifact download returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, projectMaterializeMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > projectMaterializeMaxBytes {
		return nil, errors.New("project artifact is empty or exceeds the materialization bound")
	}
	return body, nil
}

func writeDeclaredProjectOutput(root, output string, body []byte) error {
	if !validProjectArtifactRef(output) || output == "project://input" {
		return errors.New("project output is not a writable declared artifact")
	}
	rel := filepath.Clean(strings.TrimPrefix(output, "project://"))
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("project output path escapes project root")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	info, err := os.Lstat(absRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("project root must be a real directory")
	}
	target := filepath.Join(absRoot, rel)
	if relative, err := filepath.Rel(absRoot, target); err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("project output path escapes project root")
	}
	if _, err := os.Lstat(target); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("declared project output already exists")
	}
	parent := filepath.Dir(target)
	if err := ensureProjectOutputParent(absRoot, parent); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(parent, ".merc-project-output-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, bytes.NewReader(body)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

func ensureProjectOutputParent(root, parent string) error {
	rel, err := filepath.Rel(root, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("project output parent escapes project root")
	}
	if rel == "." {
		return nil
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0700); err != nil {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("project output parent is not a real directory")
		}
	}
	return nil
}

func writeProjectMaterialization(w io.Writer, result ProjectMaterialization) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
