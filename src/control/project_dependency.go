package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ProjectDependentQuote is a single, ready downstream step. It does not
// pretend the project was quoted all at once: the order's current reservation
// is observed at quote time, and enforced again by the server at submission.
type ProjectDependentQuote struct {
	Version              int                      `json:"version"`
	IRSHA256             string                   `json:"ir_sha256"`
	ProjectID            string                   `json:"project_id"`
	Currency             string                   `json:"currency"`
	BuyerCeilingNanos    int64                    `json:"buyer_ceiling_nanos"`
	ServerReservedNanos  int64                    `json:"server_reserved_nanos"`
	ServerRemainingNanos int64                    `json:"server_remaining_nanos"`
	Step                 ProjectStepQuote         `json:"step"`
	Materializations     []ProjectMaterialization `json:"materializations"`
	CalibrationState     string                   `json:"calibration_state"`
}

// ProjectInitialQuote is the reviewed authority for every dependency-free root
// in a declared graph. It deliberately does not price a future dependent step:
// that input does not yet exist and must be materialized, receipt-bound, and
// quoted later by ProjectDependentQuote.
type ProjectInitialQuote struct {
	Version             int                `json:"version"`
	IRSHA256            string             `json:"ir_sha256"`
	Currency            string             `json:"currency"`
	BuyerCeilingNanos   int64              `json:"buyer_ceiling_nanos"`
	ExpectedCostNanos   int64              `json:"expected_cost_nanos"`
	MaximumCostNanos    int64              `json:"maximum_cost_nanos"`
	CriticalPathP50Secs int                `json:"critical_path_p50_secs"`
	CriticalPathP90Secs int                `json:"critical_path_p90_secs"`
	MinimumConfidence   float64            `json:"minimum_confidence"`
	Steps               []ProjectStepQuote `json:"steps"`
	CalibrationState    string             `json:"calibration_state"`
}

func initialProjectRoots(ir ProjectWorkloadIR) ([]ProjectIRStep, error) {
	if err := validateProjectArtifactDataflow(ir.Steps); err != nil {
		return nil, fmt.Errorf("project artifact dataflow: %w", err)
	}
	roots := make([]ProjectIRStep, 0, len(ir.Steps))
	for _, step := range ir.Steps {
		if len(step.DependsOn) == 0 {
			roots = append(roots, step)
		}
	}
	if len(roots) == 0 {
		return nil, errors.New("declared project graph has no dependency-free root step")
	}
	return roots, nil
}

// quoteInitialProjectRoots quotes only inputs that exist in the buyer's
// project today. This is intentionally separate from quoteCompiledProject:
// making a quote for a downstream output before it is verified would be a
// second, fictional input authority.
func quoteInitialProjectRoots(c *client, root string, ir ProjectWorkloadIR) (ProjectInitialQuote, error) {
	if ir.IRSHA256 == "" || !validSHA256(ir.IRSHA256) || !ir.Probe.Executed ||
		ir.Probe.ApprovedIRSHA256 == "" || ir.Economics.PricingDecisionSHA256 != "" {
		return ProjectInitialQuote{}, errors.New("initial-root quote requires the exact buyer-approved, unquoted Project IR")
	}
	roots, err := initialProjectRoots(ir)
	if err != nil {
		return ProjectInitialQuote{}, err
	}
	rootIR := ir
	rootIR.Steps = roots
	quoted, err := quoteCompiledProject(c, root, rootIR)
	if err != nil {
		return ProjectInitialQuote{}, err
	}
	return ProjectInitialQuote{
		Version: 1, IRSHA256: ir.IRSHA256, Currency: quoted.Currency,
		BuyerCeilingNanos: quoted.BuyerCeilingNanos, ExpectedCostNanos: quoted.ExpectedCostNanos,
		MaximumCostNanos: quoted.MaximumCostNanos, CriticalPathP50Secs: quoted.CriticalPathP50Secs,
		CriticalPathP90Secs: quoted.CriticalPathP90Secs, MinimumConfidence: quoted.MinimumConfidence,
		Steps: quoted.Steps, CalibrationState: quoted.CalibrationState,
	}, nil
}

func validateInitialProjectQuoteForSubmit(root string, ir ProjectWorkloadIR, artifact ProjectInitialQuote, now time.Time) ([]projectPreparedSubmission, error) {
	if artifact.Version != 1 || artifact.IRSHA256 != ir.IRSHA256 || artifact.Currency != ir.Economics.Currency ||
		artifact.BuyerCeilingNanos != ir.Economics.MaximumBuyerPriceNanos ||
		artifact.CalibrationState != "STEP_QUOTES_NOT_PROJECT_OUTCOME_CALIBRATED" {
		return nil, errors.New("initial-root quote does not bind the exact approved project")
	}
	roots, err := initialProjectRoots(ir)
	if err != nil {
		return nil, err
	}
	rootIR := ir
	rootIR.Steps = roots
	quote := ProjectQuote{Version: 2, IRSHA256: artifact.IRSHA256, Currency: artifact.Currency,
		ExpectedCostNanos: artifact.ExpectedCostNanos, MaximumCostNanos: artifact.MaximumCostNanos,
		BuyerCeilingNanos: artifact.BuyerCeilingNanos, CriticalPathP50Secs: artifact.CriticalPathP50Secs,
		CriticalPathP90Secs: artifact.CriticalPathP90Secs, MinimumConfidence: artifact.MinimumConfidence,
		CalibrationState: artifact.CalibrationState, Steps: artifact.Steps}
	return validateProjectQuoteForSubmit(root, rootIR, quote, now)
}

func createBoundProjectOrder(c *client, ir ProjectWorkloadIR, currency string, ceiling int64) (ProjectOrder, error) {
	order, err := createProjectOrder(c, projectOrderCreateRequest{
		IRSHA256: ir.IRSHA256, Currency: currency, BuyerCeilingNanos: ceiling,
	})
	if err != nil {
		return ProjectOrder{}, fmt.Errorf("create project order: %w", err)
	}
	if _, err := uuid.Parse(order.ID); err != nil || order.IRSHA256 != ir.IRSHA256 ||
		order.Currency != currency || order.BuyerCeilingNanos != ceiling ||
		order.Status != "OPEN" || order.ReservedNanos < 0 || order.RemainingNanos < 0 ||
		order.ReservedNanos+order.RemainingNanos != order.BuyerCeilingNanos {
		return ProjectOrder{}, errors.New("project order response does not bind the approved project ceiling")
	}
	return order, nil
}

// submitInitialProjectRoots creates the one project order before it submits any
// root. Later steps consume its server-side slots rather than introducing a
// second project price ledger.
func submitInitialProjectRoots(c *client, root string, ir ProjectWorkloadIR, artifact ProjectInitialQuote, now time.Time) (ProjectSubmission, error) {
	result := ProjectSubmission{Version: 2, IRSHA256: ir.IRSHA256, Currency: artifact.Currency,
		BuyerCeilingNanos: artifact.BuyerCeilingNanos, Status: "REFUSED", ExecutionMode: "INITIAL_MATERIALIZED_ROOTS"}
	prepared, err := validateInitialProjectQuoteForSubmit(root, ir, artifact, now)
	if err != nil {
		return result, err
	}
	order, err := createBoundProjectOrder(c, ir, artifact.Currency, artifact.BuyerCeilingNanos)
	if err != nil {
		return result, err
	}
	result.ProjectID, result.Status = order.ID, "SUBMITTING"
	for _, item := range prepared {
		key := "project-root:" + ir.IRSHA256[:24] + ":" + strings.TrimPrefix(item.quoted.QuoteID, "q_")
		result.AttemptedStepID, result.AttemptedKey = item.step.ID, key
		item.request.ProjectID, item.request.ProjectStepID = order.ID, item.step.ID
		response, replayed, submitErr := projectSubmitRequest(c, mustJSON(item.request), key)
		if submitErr != nil {
			result.Status = "INDETERMINATE"
			if len(result.Steps) > 0 {
				result.Status = "PARTIAL"
			}
			return result, fmt.Errorf("root step %s submit failed after %d accepted step(s): %w", item.step.ID, len(result.Steps), submitErr)
		}
		if response.JobID == uuid.Nil {
			result.Status = "INDETERMINATE"
			if len(result.Steps) > 0 {
				result.Status = "PARTIAL"
			}
			return result, fmt.Errorf("root step %s submit response omitted job_id", item.step.ID)
		}
		result.Steps = append(result.Steps, ProjectStepSubmission{StepID: item.step.ID, JobID: response.JobID.String(),
			QuoteID: item.quoted.QuoteID, PricingDecisionSHA256: item.quoted.PricingDecisionSHA256,
			AuthorityQuoteSHA256: item.quoted.AuthorityQuoteSHA256, IdempotencyKey: key, IdempotentReplay: replayed})
	}
	result.Status, result.AttemptedStepID, result.AttemptedKey = "ACCEPTED", "", ""
	return result, nil
}

func findProjectIRStep(ir ProjectWorkloadIR, stepID string) (ProjectIRStep, bool) {
	for _, step := range ir.Steps {
		if step.ID == stepID {
			return step, true
		}
	}
	return ProjectIRStep{}, false
}

func projectOrderForClient(c *client, id string) (ProjectOrder, error) {
	if _, err := uuid.Parse(id); err != nil {
		return ProjectOrder{}, errors.New("project id must be a UUID")
	}
	var order ProjectOrder
	if err := projectGetJSON(c, "/v1/projects/"+id, &order); err != nil {
		return ProjectOrder{}, fmt.Errorf("project order: %w", err)
	}
	if order.ID != id || !validSHA256(order.IRSHA256) || order.BuyerCeilingNanos <= 0 ||
		order.ReservedNanos < 0 || order.RemainingNanos < 0 ||
		order.ReservedNanos+order.RemainingNanos != order.BuyerCeilingNanos {
		return ProjectOrder{}, errors.New("project order response has invalid fixed-point reservation authority")
	}
	return order, nil
}

func validateProjectOrderForIR(order ProjectOrder, projectID string, ir ProjectWorkloadIR) error {
	if order.ID != projectID || order.IRSHA256 != ir.IRSHA256 || order.Currency != ir.Economics.Currency ||
		order.BuyerCeilingNanos != ir.Economics.MaximumBuyerPriceNanos || order.Status != "OPEN" {
		return errors.New("project order does not bind the exact approved IR, currency, and open buyer ceiling")
	}
	return nil
}

// validateDependentMaterializations proves the buyer's local downstream input
// is both byte-identical to its hand-off receipt and traceable to a server-side
// accepted project slot. The byte check alone would allow an edited receipt;
// the order check alone would allow an edited local artifact. Both are needed.
func validateDependentMaterializations(
	c *client, root string, ir ProjectWorkloadIR, order ProjectOrder,
	step ProjectIRStep, materializations []ProjectMaterialization,
) error {
	if len(step.DependsOn) == 0 {
		return errors.New("dependent step must name at least one upstream step")
	}
	if len(materializations) != len(step.DependsOn) {
		return fmt.Errorf("dependent step needs exactly %d materialization receipt(s)", len(step.DependsOn))
	}
	if err := validateProjectArtifactDataflow(ir.Steps); err != nil {
		return fmt.Errorf("project artifact dataflow: %w", err)
	}
	reserved := make(map[string]ProjectOrderStep, len(order.Steps))
	for _, item := range order.Steps {
		reserved[item.StepID] = item
	}
	seen := make(map[string]bool, len(materializations))
	for _, materialization := range materializations {
		if materialization.Version != 2 || materialization.ProjectID != order.ID ||
			materialization.IRSHA256 != ir.IRSHA256 || !projectStepIDPattern.MatchString(materialization.StepID) ||
			!validProjectArtifactRef(materialization.Output) || materialization.Output == "project://input" ||
			materialization.Bytes <= 0 || materialization.Bytes > projectMaterializeMaxBytes ||
			!validSHA256(materialization.SHA256) || !validSHA256(materialization.PricingDecisionSHA256) ||
			!validSHA256(materialization.AuthorityQuoteSHA256) {
			return errors.New("materialization receipt is incomplete or does not bind this project order")
		}
		if _, err := time.Parse(time.RFC3339Nano, materialization.MaterializedAt); err != nil {
			return errors.New("materialization receipt has an invalid timestamp")
		}
		if seen[materialization.StepID] {
			return fmt.Errorf("duplicate materialization receipt for %s", materialization.StepID)
		}
		seen[materialization.StepID] = true
		upstream, found := findProjectIRStep(ir, materialization.StepID)
		if !found || len(upstream.Outputs) != 1 || upstream.Outputs[0] != materialization.Output {
			return fmt.Errorf("materialization output does not match declared upstream step %s", materialization.StepID)
		}
		serverStep, accepted := reserved[materialization.StepID]
		if !accepted || serverStep.JobID != materialization.JobID ||
			serverStep.PricingDecisionSHA256 != materialization.PricingDecisionSHA256 {
			return fmt.Errorf("materialization %s is not an accepted server project step", materialization.StepID)
		}
		jobID, err := uuid.Parse(materialization.JobID)
		if err != nil {
			return fmt.Errorf("materialization job id: %w", err)
		}
		receipt, err := projectReceipt(c, jobID)
		if err != nil || receipt.JobID != jobID || receipt.Status != "complete" || receipt.AuthorityStatus != "verified" ||
			receipt.Authority.PricingDecisionSHA256 != materialization.PricingDecisionSHA256 {
			return errors.New("materialization no longer has a completed, verified project receipt")
		}
		path, err := exactProjectArtifactPath(root, materialization.Output)
		if err != nil {
			return fmt.Errorf("materialized %s: %w", materialization.Output, err)
		}
		body, err := os.ReadFile(path)
		if err != nil || int64(len(body)) != materialization.Bytes {
			return fmt.Errorf("materialized %s bytes changed after receipt", materialization.Output)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != materialization.SHA256 {
			return fmt.Errorf("materialized %s hash changed after receipt", materialization.Output)
		}
	}
	for _, dependency := range step.DependsOn {
		if !seen[dependency] {
			return fmt.Errorf("dependent step is missing materialization for %s", dependency)
		}
	}
	return nil
}

func quoteDependentProjectStep(
	c *client, root string, ir ProjectWorkloadIR, projectID, stepID string, materializations []ProjectMaterialization,
) (ProjectDependentQuote, error) {
	if ir.IRSHA256 == "" || !validSHA256(ir.IRSHA256) || !ir.Probe.Executed ||
		ir.Probe.ApprovedIRSHA256 == "" || ir.Economics.PricingDecisionSHA256 != "" {
		return ProjectDependentQuote{}, errors.New("dependent quote requires the exact buyer-approved, unquoted Project IR")
	}
	step, found := findProjectIRStep(ir, stepID)
	if !found || len(step.DependsOn) == 0 {
		return ProjectDependentQuote{}, errors.New("quote-step requires a declared dependent project step")
	}
	order, err := projectOrderForClient(c, projectID)
	if err != nil {
		return ProjectDependentQuote{}, err
	}
	if err := validateProjectOrderForIR(order, projectID, ir); err != nil {
		return ProjectDependentQuote{}, err
	}
	if err := validateDependentMaterializations(c, root, ir, order, step, materializations); err != nil {
		return ProjectDependentQuote{}, err
	}
	// quoteCompiledProject is reused on a one-step copy so the exact same quote
	// validation and no-second-pricing-authority rule applies. Dependencies are
	// erased only in the copy after their materialization receipts were checked;
	// the immutable original IR remains the identity stored in the output.
	ready := step
	ready.DependsOn = nil
	quoteIR := ir
	quoteIR.Steps = []ProjectIRStep{ready}
	quoted, err := quoteCompiledProject(c, root, quoteIR)
	if err != nil {
		return ProjectDependentQuote{}, err
	}
	if len(quoted.Steps) != 1 || quoted.Steps[0].StepID != step.ID || quoted.MaximumCostNanos > order.RemainingNanos {
		return ProjectDependentQuote{}, fmt.Errorf("dependent step maximum %d nanos exceeds current project remaining %d nanos",
			quoted.MaximumCostNanos, order.RemainingNanos)
	}
	return ProjectDependentQuote{
		Version: 1, IRSHA256: ir.IRSHA256, ProjectID: projectID, Currency: quoted.Currency,
		BuyerCeilingNanos: quoted.BuyerCeilingNanos, ServerReservedNanos: order.ReservedNanos,
		ServerRemainingNanos: order.RemainingNanos, Step: quoted.Steps[0],
		Materializations: append([]ProjectMaterialization(nil), materializations...),
		CalibrationState: "STEP_QUOTES_NOT_PROJECT_OUTCOME_CALIBRATED",
	}, nil
}

func validateDependentQuoteForSubmit(
	c *client, root string, ir ProjectWorkloadIR, artifact ProjectDependentQuote, now time.Time,
) (projectPreparedSubmission, error) {
	if artifact.Version != 1 || artifact.IRSHA256 != ir.IRSHA256 || artifact.Currency != ir.Economics.Currency ||
		artifact.BuyerCeilingNanos != ir.Economics.MaximumBuyerPriceNanos ||
		artifact.CalibrationState != "STEP_QUOTES_NOT_PROJECT_OUTCOME_CALIBRATED" ||
		artifact.ProjectID == "" {
		return projectPreparedSubmission{}, errors.New("dependent quote does not bind the exact approved project")
	}
	step, found := findProjectIRStep(ir, artifact.Step.StepID)
	if !found || len(step.DependsOn) == 0 {
		return projectPreparedSubmission{}, errors.New("dependent quote step is not a declared downstream step")
	}
	order, err := projectOrderForClient(c, artifact.ProjectID)
	if err != nil {
		return projectPreparedSubmission{}, err
	}
	if err := validateProjectOrderForIR(order, artifact.ProjectID, ir); err != nil {
		return projectPreparedSubmission{}, err
	}
	if err := validateDependentMaterializations(c, root, ir, order, step, artifact.Materializations); err != nil {
		return projectPreparedSubmission{}, err
	}
	ready := step
	ready.DependsOn = nil
	readyIR := ir
	readyIR.Steps = []ProjectIRStep{ready}
	projectQuote := ProjectQuote{Version: 2, IRSHA256: ir.IRSHA256, Currency: artifact.Currency,
		ExpectedCostNanos: artifact.Step.ExpectedCostNanos, MaximumCostNanos: artifact.Step.MaximumCostNanos,
		BuyerCeilingNanos: artifact.BuyerCeilingNanos, CriticalPathP50Secs: artifact.Step.P50Secs,
		CriticalPathP90Secs: artifact.Step.P90Secs, MinimumConfidence: artifact.Step.Confidence,
		CalibrationState: artifact.CalibrationState, Steps: []ProjectStepQuote{artifact.Step}}
	prepared, err := validateProjectQuoteForSubmit(root, readyIR, projectQuote, now)
	if err != nil || len(prepared) != 1 {
		if err == nil {
			err = errors.New("dependent quote did not produce exactly one firm request")
		}
		return projectPreparedSubmission{}, err
	}
	if prepared[0].quoted.MaximumCostNanos > order.RemainingNanos {
		return projectPreparedSubmission{}, fmt.Errorf("dependent step maximum %d nanos exceeds current project remaining %d nanos",
			prepared[0].quoted.MaximumCostNanos, order.RemainingNanos)
	}
	return prepared[0], nil
}

func submitDependentProjectStep(
	c *client, root string, ir ProjectWorkloadIR, artifact ProjectDependentQuote, now time.Time,
) (ProjectSubmission, error) {
	result := ProjectSubmission{Version: 2, IRSHA256: ir.IRSHA256, ProjectID: artifact.ProjectID,
		Currency: artifact.Currency, BuyerCeilingNanos: artifact.BuyerCeilingNanos,
		Status: "REFUSED", ExecutionMode: "DEPENDENT_MATERIALIZED_STEP"}
	prepared, err := validateDependentQuoteForSubmit(c, root, ir, artifact, now)
	if err != nil {
		return result, err
	}
	idempotencyKey := "project-step:" + artifact.ProjectID + ":" + prepared.step.ID + ":" + strings.TrimPrefix(prepared.quoted.QuoteID, "q_")
	if len(idempotencyKey) > 128 {
		return result, errors.New("dependent project idempotency key exceeds the admission bound")
	}
	result.Status, result.AttemptedStepID, result.AttemptedKey = "SUBMITTING", prepared.step.ID, idempotencyKey
	prepared.request.ProjectID, prepared.request.ProjectStepID = artifact.ProjectID, prepared.step.ID
	response, replayed, err := projectSubmitRequest(c, mustJSON(prepared.request), idempotencyKey)
	if err != nil {
		result.Status = "INDETERMINATE"
		return result, fmt.Errorf("dependent step %s submit: %w", prepared.step.ID, err)
	}
	if response.JobID == uuid.Nil {
		result.Status = "INDETERMINATE"
		return result, errors.New("dependent step submit response omitted job_id")
	}
	result.Status, result.AttemptedStepID, result.AttemptedKey = "ACCEPTED", "", ""
	result.Steps = []ProjectStepSubmission{{StepID: prepared.step.ID, JobID: response.JobID.String(),
		QuoteID: prepared.quoted.QuoteID, PricingDecisionSHA256: prepared.quoted.PricingDecisionSHA256,
		AuthorityQuoteSHA256: prepared.quoted.AuthorityQuoteSHA256, IdempotencyKey: idempotencyKey,
		IdempotentReplay: replayed}}
	return result, nil
}
