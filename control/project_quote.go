package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const projectQuoteMaxInputBytes = 64 << 20

type ProjectStepQuote struct {
	StepID                  string   `json:"step_id"`
	QuoteID                 string   `json:"quote_id"`
	PricingDecisionSHA256   string   `json:"pricing_decision_sha256"`
	ExpectedCostNanos       int64    `json:"expected_cost_nanos"`
	MaximumCostNanos        int64    `json:"maximum_cost_nanos"`
	P50Secs                 int      `json:"p50_secs"`
	P90Secs                 int      `json:"p90_secs"`
	Confidence              float64  `json:"confidence"`
	ConfidenceReasons       []string `json:"confidence_reasons"`
	ETAConfidenceBandMethod string   `json:"eta_confidence_band_method"`
}

type ProjectQuote struct {
	Version             int                `json:"version"`
	IRSHA256            string             `json:"ir_sha256"`
	Currency            string             `json:"currency"`
	ExpectedCostNanos   int64              `json:"expected_cost_nanos"`
	MaximumCostNanos    int64              `json:"maximum_cost_nanos"`
	BuyerCeilingNanos   int64              `json:"buyer_ceiling_nanos"`
	CriticalPathP50Secs int                `json:"critical_path_p50_secs"`
	CriticalPathP90Secs int                `json:"critical_path_p90_secs"`
	MinimumConfidence   float64            `json:"minimum_confidence"`
	CalibrationState    string             `json:"calibration_state"`
	Steps               []ProjectStepQuote `json:"steps"`
}

func quoteCompiledProject(c *client, root string, ir ProjectWorkloadIR) (ProjectQuote, error) {
	if !ir.Probe.Executed || ir.Probe.ApprovedIRSHA256 == "" {
		return ProjectQuote{}, errors.New("project quote requires an exact buyer-approved bounded probe")
	}
	if ir.Economics.PricingDecisionSHA256 != "" {
		return ProjectQuote{}, errors.New("unquoted IR already carries pricing authority")
	}
	currency, err := ParseCurrency(ir.Economics.Currency)
	if err != nil {
		return ProjectQuote{}, fmt.Errorf("project currency: %w", err)
	}
	if ir.Economics.MaximumBuyerPriceNanos <= 0 {
		return ProjectQuote{}, errors.New("project buyer ceiling must be positive")
	}
	out := ProjectQuote{
		Version: 1, IRSHA256: ir.IRSHA256, Currency: currency.Code(),
		BuyerCeilingNanos: ir.Economics.MaximumBuyerPriceNanos,
		MinimumConfidence: 1, CalibrationState: "STEP_QUOTES_NOT_PROJECT_OUTCOME_CALIBRATED",
	}
	p50 := make(map[string]int, len(ir.Steps))
	p90 := make(map[string]int, len(ir.Steps))
	for _, step := range ir.Steps {
		if step.RuntimeID == "" || step.ModelID == "" {
			return ProjectQuote{}, fmt.Errorf("step %s has no resolved runtime/model contract", step.ID)
		}
		inputPath, err := exactProjectStepInput(root, step)
		if err != nil {
			return ProjectQuote{}, fmt.Errorf("step %s: %w", step.ID, err)
		}
		input, err := os.ReadFile(inputPath)
		if err != nil {
			return ProjectQuote{}, fmt.Errorf("step %s input: %w", step.ID, err)
		}
		model, ok := runtimeAuthorityModels[step.ModelID]
		if !ok {
			return ProjectQuote{}, fmt.Errorf("step %s model authority disappeared", step.ID)
		}
		request := cliJobSubmit{
			JobType: jobType{Type: model.Job}, Model: modelRef{Kind: model.WireKind, Ref: model.ID},
			Constraints: jobConstraints{}, Verification: verificationPolicy{}, Tier: "batch",
			Input: mustJSON(string(input)),
		}
		blob, err := projectQuoteRequest(c, mustJSON(request))
		if err != nil {
			return ProjectQuote{}, fmt.Errorf("step %s quote: %w", step.ID, err)
		}
		var quote Quote
		if err := json.Unmarshal(blob, &quote); err != nil {
			return ProjectQuote{}, fmt.Errorf("step %s decode quote: %w", step.ID, err)
		}
		if quote.Currency != currency.Code() || quote.Pricing.Currency != currency.Code() ||
			quote.Pricing.FixedPoint == nil || quote.Pricing.FixedPoint.Currency != currency.Code() {
			return ProjectQuote{}, fmt.Errorf("step %s quote currency/fixed-point authority does not match project %s", step.ID, currency.Code())
		}
		if err := ValidateDistributedPricingDecisionSnapshot(
			quote.Pricing, quote.Workload, quote.ComputePlan, quote.Placement, quote.Economics,
		); err != nil {
			return ProjectQuote{}, fmt.Errorf("step %s quote PricingDecision is invalid: %w", step.ID, err)
		}
		if len(quote.Workload.RuntimeCandidates) != 1 ||
			quote.Workload.RuntimeCandidates[0].RuntimeID != step.RuntimeID || quote.Model != step.ModelID {
			return ProjectQuote{}, fmt.Errorf("step %s quote resolved a different runtime/model", step.ID)
		}
		pricingSHA, err := pricingDecisionDigest(quote.Pricing)
		if err != nil {
			return ProjectQuote{}, fmt.Errorf("step %s pricing digest: %w", step.ID, err)
		}
		expected, err := MoneyNanosFromUSDFloat(currency, quote.Cost.ExpectedUSD)
		if err != nil {
			return ProjectQuote{}, err
		}
		maximum, err := MoneyNanosFromUSDFloat(currency, quote.Cost.MaxUSD)
		if err != nil {
			return ProjectQuote{}, err
		}
		if expected.Nanos <= 0 || maximum.Nanos < expected.Nanos ||
			quote.Pricing.FixedPoint.AcceptedCeilingNanos < maximum.Nanos {
			return ProjectQuote{}, fmt.Errorf("step %s quote has inconsistent cost ceiling", step.ID)
		}
		if out.ExpectedCostNanos > int64(^uint64(0)>>1)-expected.Nanos || out.MaximumCostNanos > int64(^uint64(0)>>1)-maximum.Nanos {
			return ProjectQuote{}, errors.New("project quote cost overflow")
		}
		out.ExpectedCostNanos += expected.Nanos
		out.MaximumCostNanos += maximum.Nanos
		if quote.Confidence.Score < out.MinimumConfidence {
			out.MinimumConfidence = quote.Confidence.Score
		}
		p50[step.ID], p90[step.ID] = quote.Time.P50Secs, quote.Time.P90Secs
		out.Steps = append(out.Steps, ProjectStepQuote{
			StepID: step.ID, QuoteID: quote.QuoteID, PricingDecisionSHA256: pricingSHA,
			ExpectedCostNanos: expected.Nanos, MaximumCostNanos: maximum.Nanos,
			P50Secs: quote.Time.P50Secs, P90Secs: quote.Time.P90Secs,
			Confidence: quote.Confidence.Score, ConfidenceReasons: quote.Confidence.Reasons,
			ETAConfidenceBandMethod: quote.Time.ConfidenceBandMethod,
		})
	}
	if out.MaximumCostNanos > out.BuyerCeilingNanos {
		return ProjectQuote{}, fmt.Errorf("project maximum %d nanos exceeds buyer ceiling %d nanos", out.MaximumCostNanos, out.BuyerCeilingNanos)
	}
	out.CriticalPathP50Secs, err = projectCriticalPath(ir.Steps, p50)
	if err != nil {
		return ProjectQuote{}, err
	}
	out.CriticalPathP90Secs, err = projectCriticalPath(ir.Steps, p90)
	if err != nil {
		return ProjectQuote{}, err
	}
	return out, nil
}

func exactProjectStepInput(root string, step ProjectIRStep) (string, error) {
	if len(step.Inputs) != 1 || !strings.HasPrefix(step.Inputs[0], "project://") || step.Inputs[0] == "project://input" {
		return "", errors.New("quotable finite step requires one explicit project://PATH input")
	}
	rel := strings.TrimPrefix(step.Inputs[0], "project://")
	clean := filepath.Clean(rel)
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("input path escapes project root")
	}
	path := filepath.Join(root, clean)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > projectQuoteMaxInputBytes {
		return "", fmt.Errorf("input must be a regular non-symlink file no larger than %d bytes", projectQuoteMaxInputBytes)
	}
	return path, nil
}

func projectQuoteRequest(c *client, body []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, c.base+"/v1/quote", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONRequestBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxJSONRequestBodyBytes {
		return nil, errors.New("quote response exceeds bounded size")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("quote endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func projectCriticalPath(steps []ProjectIRStep, durations map[string]int) (int, error) {
	byID := make(map[string]ProjectIRStep, len(steps))
	for _, step := range steps {
		byID[step.ID] = step
	}
	memo := make(map[string]int, len(steps))
	visiting := make(map[string]bool, len(steps))
	var path func(string) (int, error)
	path = func(id string) (int, error) {
		if value, ok := memo[id]; ok {
			return value, nil
		}
		if visiting[id] {
			return 0, errors.New("project graph cycle during critical-path calculation")
		}
		step, ok := byID[id]
		if !ok {
			return 0, fmt.Errorf("unknown project step %s", id)
		}
		visiting[id] = true
		longest := 0
		for _, dependency := range step.DependsOn {
			value, err := path(dependency)
			if err != nil {
				return 0, err
			}
			if value > longest {
				longest = value
			}
		}
		delete(visiting, id)
		if durations[id] < 0 || longest > int(^uint(0)>>1)-durations[id] {
			return 0, errors.New("project duration overflow")
		}
		memo[id] = longest + durations[id]
		return memo[id], nil
	}
	longest := 0
	for _, step := range steps {
		value, err := path(step.ID)
		if err != nil {
			return 0, err
		}
		if value > longest {
			longest = value
		}
	}
	return longest, nil
}

func writeProjectQuote(w io.Writer, quote ProjectQuote) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(quote)
}
