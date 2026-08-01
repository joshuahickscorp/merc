package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

const projectQuoteArtifactMaxBytes = 32 << 20

type projectPreparedSubmission struct {
	step    ProjectIRStep
	quoted  ProjectStepQuote
	request cliJobSubmit
}

type ProjectStepSubmission struct {
	StepID                string `json:"step_id"`
	JobID                 string `json:"job_id"`
	QuoteID               string `json:"quote_id"`
	PricingDecisionSHA256 string `json:"pricing_decision_sha256"`
	AuthorityQuoteSHA256  string `json:"authority_quote_sha256"`
	IdempotencyKey        string `json:"idempotency_key"`
	IdempotentReplay      bool   `json:"idempotent_replay"`
}

type ProjectSubmission struct {
	Version           int                     `json:"version"`
	IRSHA256          string                  `json:"ir_sha256"`
	Currency          string                  `json:"currency"`
	BuyerCeilingNanos int64                   `json:"buyer_ceiling_nanos"`
	Status            string                  `json:"status"`
	ExecutionMode     string                  `json:"execution_mode"`
	AttemptedStepID   string                  `json:"attempted_step_id,omitempty"`
	AttemptedKey      string                  `json:"attempted_idempotency_key,omitempty"`
	Steps             []ProjectStepSubmission `json:"steps"`
}

// validateProjectQuoteForSubmit turns a reviewed quote artifact into requests
// without deriving price a second time. Every economic value is rechecked
// against the full server-issued authority snapshot before a network mutation.
func validateProjectQuoteForSubmit(root string, ir ProjectWorkloadIR, artifact ProjectQuote, now time.Time) ([]projectPreparedSubmission, error) {
	if artifact.Version != 2 || artifact.IRSHA256 != ir.IRSHA256 {
		return nil, errors.New("project quote does not bind the exact approved probed IR")
	}
	if artifact.CalibrationState != "STEP_QUOTES_NOT_PROJECT_OUTCOME_CALIBRATED" {
		return nil, errors.New("project quote calibration state changed")
	}
	if artifact.Currency != ir.Economics.Currency || artifact.BuyerCeilingNanos != ir.Economics.MaximumBuyerPriceNanos {
		return nil, errors.New("project quote currency or buyer ceiling changed")
	}
	if len(artifact.Steps) != len(ir.Steps) {
		return nil, errors.New("project quote step set changed")
	}
	currency, err := ParseCurrency(artifact.Currency)
	if err != nil {
		return nil, err
	}
	var expectedTotal, maximumTotal int64
	p50 := make(map[string]int, len(ir.Steps))
	p90 := make(map[string]int, len(ir.Steps))
	minimumConfidence := float64(1)
	prepared := make([]projectPreparedSubmission, 0, len(ir.Steps))
	for i, step := range ir.Steps {
		if len(step.DependsOn) != 0 {
			return nil, fmt.Errorf("step %s has dependencies; project submit currently supports only independent finite steps", step.ID)
		}
		quoted := artifact.Steps[i]
		q := quoted.Authority
		if quoted.StepID != step.ID || q.QuoteID != quoted.QuoteID || q.QuoteID == "" {
			return nil, fmt.Errorf("step %s quote identity changed", step.ID)
		}
		if _, err := quoteIDToUUID(q.QuoteID); err != nil {
			return nil, fmt.Errorf("step %s: %w", step.ID, err)
		}
		if !q.ExpiresAt.After(now) {
			return nil, fmt.Errorf("step %s quote expired; request and review a new project quote", step.ID)
		}
		authoritySHA, err := canonicalDigest("project step authority quote", q)
		if err != nil || authoritySHA != quoted.AuthorityQuoteSHA256 {
			return nil, fmt.Errorf("step %s authority quote digest mismatch", step.ID)
		}
		pricingSHA, err := pricingDecisionDigest(q.Pricing)
		if err != nil || pricingSHA != quoted.PricingDecisionSHA256 {
			return nil, fmt.Errorf("step %s PricingDecision digest mismatch", step.ID)
		}
		if err := ValidateDistributedPricingDecisionSnapshot(q.Pricing, q.Workload, q.ComputePlan, q.Placement, q.Economics); err != nil {
			return nil, fmt.Errorf("step %s PricingDecision is invalid: %w", step.ID, err)
		}
		if q.Currency != currency.Code() || q.Pricing.Currency != currency.Code() || q.Pricing.FixedPoint == nil ||
			q.Pricing.FixedPoint.Currency != currency.Code() || q.Tier != "batch" {
			return nil, fmt.Errorf("step %s quote lost currency-bound batch authority", step.ID)
		}
		if len(q.Workload.RuntimeCandidates) != 1 || q.Workload.RuntimeCandidates[0].RuntimeID != step.RuntimeID || q.Model != step.ModelID {
			return nil, fmt.Errorf("step %s quote resolved a different runtime/model", step.ID)
		}
		expected, err := MoneyNanosFromUSDFloat(currency, q.Cost.ExpectedUSD)
		if err != nil {
			return nil, err
		}
		maximum, err := MoneyNanosFromUSDFloat(currency, q.Cost.MaxUSD)
		if err != nil {
			return nil, err
		}
		if expected.Nanos != quoted.ExpectedCostNanos || maximum.Nanos != quoted.MaximumCostNanos ||
			expected.Nanos <= 0 || maximum.Nanos < expected.Nanos ||
			maximum.Nanos <= 0 || q.Pricing.FixedPoint.AcceptedCeilingNanos < maximum.Nanos {
			return nil, fmt.Errorf("step %s quote cost authority changed", step.ID)
		}
		wireMaximum := maximum.USDFloat()
		roundTrip, err := MoneyNanosFromUSDFloat(currency, wireMaximum)
		if err != nil || roundTrip.Nanos != maximum.Nanos {
			return nil, fmt.Errorf("step %s exact maximum cannot cross the legacy job wire without drift", step.ID)
		}
		if expectedTotal > int64(^uint64(0)>>1)-expected.Nanos || maximumTotal > int64(^uint64(0)>>1)-maximum.Nanos {
			return nil, errors.New("project quote cost overflow")
		}
		expectedTotal += expected.Nanos
		maximumTotal += maximum.Nanos
		if q.Time.P50Secs != quoted.P50Secs || q.Time.P90Secs != quoted.P90Secs ||
			q.Confidence.Score != quoted.Confidence || !slices.Equal(q.Confidence.Reasons, quoted.ConfidenceReasons) ||
			q.Time.ConfidenceBandMethod != quoted.ETAConfidenceBandMethod {
			return nil, fmt.Errorf("step %s time/confidence authority changed", step.ID)
		}
		p50[step.ID], p90[step.ID] = quoted.P50Secs, quoted.P90Secs
		if quoted.Confidence < minimumConfidence {
			minimumConfidence = quoted.Confidence
		}
		inputPath, err := exactProjectStepInput(root, step)
		if err != nil {
			return nil, fmt.Errorf("step %s: %w", step.ID, err)
		}
		input, err := os.ReadFile(inputPath)
		if err != nil {
			return nil, err
		}
		inputDigest := sha256.Sum256(input)
		if q.InputSHA256 != hex.EncodeToString(inputDigest[:]) {
			return nil, fmt.Errorf("step %s input changed since quote", step.ID)
		}
		model, ok := runtimeAuthorityModels[step.ModelID]
		if !ok || model.Job != q.JobType || model.WireKind != q.Workload.Binding.Model.Kind {
			return nil, fmt.Errorf("step %s runtime/model authority disappeared", step.ID)
		}
		prepared = append(prepared, projectPreparedSubmission{step: step, quoted: quoted, request: cliJobSubmit{
			JobType: jobType{Type: model.Job}, Model: modelRef{Kind: model.WireKind, Ref: model.ID},
			Constraints: jobConstraints{}, Verification: verificationPolicy{}, Tier: "batch",
			Input: mustJSON(string(input)), MaxUSD: wireMaximum, QuoteID: q.QuoteID, FirmQuote: true,
		}})
	}
	criticalP50, err := projectCriticalPath(ir.Steps, p50)
	if err != nil {
		return nil, err
	}
	criticalP90, err := projectCriticalPath(ir.Steps, p90)
	if err != nil {
		return nil, err
	}
	if expectedTotal != artifact.ExpectedCostNanos || maximumTotal != artifact.MaximumCostNanos ||
		maximumTotal > artifact.BuyerCeilingNanos || criticalP50 != artifact.CriticalPathP50Secs ||
		criticalP90 != artifact.CriticalPathP90Secs || minimumConfidence != artifact.MinimumConfidence {
		return nil, errors.New("project quote aggregate authority changed")
	}
	return prepared, nil
}

func submitCompiledProject(c *client, root string, ir ProjectWorkloadIR, artifact ProjectQuote, now time.Time) (ProjectSubmission, error) {
	prepared, err := validateProjectQuoteForSubmit(root, ir, artifact, now)
	result := ProjectSubmission{Version: 1, IRSHA256: ir.IRSHA256, Currency: artifact.Currency,
		BuyerCeilingNanos: artifact.BuyerCeilingNanos, Status: "REFUSED", ExecutionMode: "INDEPENDENT_FINITE_STEPS"}
	if err != nil {
		return result, err
	}
	result.Status = "SUBMITTING"
	for _, item := range prepared {
		idempotencyKey := "project:" + ir.IRSHA256[:24] + ":" + strings.TrimPrefix(item.quoted.QuoteID, "q_")
		result.AttemptedStepID = item.step.ID
		result.AttemptedKey = idempotencyKey
		response, replayed, err := projectSubmitRequest(c, mustJSON(item.request), idempotencyKey)
		if err != nil {
			result.Status = "INDETERMINATE"
			if len(result.Steps) > 0 {
				result.Status = "PARTIAL"
			}
			return result, fmt.Errorf("step %s submit failed after %d accepted step(s): %w", item.step.ID, len(result.Steps), err)
		}
		if response.JobID.String() == "00000000-0000-0000-0000-000000000000" {
			result.Status = "INDETERMINATE"
			if len(result.Steps) > 0 {
				result.Status = "PARTIAL"
			}
			return result, fmt.Errorf("step %s submit response omitted job_id", item.step.ID)
		}
		result.Steps = append(result.Steps, ProjectStepSubmission{StepID: item.step.ID,
			JobID: response.JobID.String(), QuoteID: item.quoted.QuoteID,
			PricingDecisionSHA256: item.quoted.PricingDecisionSHA256,
			AuthorityQuoteSHA256:  item.quoted.AuthorityQuoteSHA256,
			IdempotencyKey:        idempotencyKey, IdempotentReplay: replayed})
	}
	result.Status = "ACCEPTED"
	result.AttemptedStepID = ""
	result.AttemptedKey = ""
	return result, nil
}

func projectSubmitRequest(c *client, body []byte, idempotencyKey string) (JobSubmitResponse, bool, error) {
	req, err := http.NewRequest(http.MethodPost, c.base+"/v1/jobs", bytes.NewReader(body))
	if err != nil {
		return JobSubmitResponse{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return JobSubmitResponse{}, false, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONRequestBodyBytes+1))
	if err != nil {
		return JobSubmitResponse{}, false, err
	}
	if len(raw) > maxJSONRequestBodyBytes {
		return JobSubmitResponse{}, false, errors.New("submit response exceeds bounded size")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return JobSubmitResponse{}, false, fmt.Errorf("job endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out JobSubmitResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return JobSubmitResponse{}, false, err
	}
	return out, strings.EqualFold(resp.Header.Get("Idempotent-Replayed"), "true"), nil
}
