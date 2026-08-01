package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

type measuredThroughput struct {
	ModelID        string
	JobType        string
	UnitsPerSec    float64 // tok/s or eps - one unit for one catalogue price, same as estimateJobSettlement
	HWClass        string  // apple_silicon_pro (the only measured reference box; see GPU_CAPABILITY.md)
	SourceCitation string
}

var repricingBenchmarks = []measuredThroughput{
	{
		ModelID:        "all-minilm-l6-v2",
		JobType:        "embed",
		UnitsPerSec:    1967.3141,
		HWClass:        "apple_silicon_pro",
		SourceCitation: "evidence/benchmarks/2026-07-01-m3-pro.json#embed",
	},
	{
		ModelID:        "llama-3.2-1b-instruct-q4",
		JobType:        "batch_infer",
		UnitsPerSec:    138.7,
		HWClass:        "apple_silicon_pro",
		SourceCitation: "evidence/benchmarks/2026-07-01-m3-pro.json#batch_infer",
	},
}

var sustainedWattsByHWClass = map[string]float64{
	"apple_silicon_base":  20.0,
	"apple_silicon_pro":   30.0,
	"apple_silicon_max":   45.0,
	"apple_silicon_ultra": 65.0,
	// CUDA sustained draw under inference load, board power. These are an order
	// of magnitude above Apple Silicon, which is what makes the supplier
	// break-even arithmetic completely different on CUDA supply.
	"nvidia_24gb":  350.0,
	"nvidia_48gb":  300.0,
	"nvidia_80gb":  400.0,
	"nvidia_180gb": 1000.0,
	"cpu":          25.0,
}

const defaultElectricityUSDPerKWh = 0.15

// targetSupplierUSDHr is the cost-plus supplier revenue target used only for
// the diagnostic floor (repriceFromSupplierEconomics). Catalogue prices come
// from pricing/board.json (market board × positioning_multiplier).
const targetSupplierUSDHr = 2.0

type RepriceResult struct {
	ModelID             string  `json:"model_id"`
	JobType             string  `json:"job_type"`
	ReferencePricePer1K float64 `json:"reference_price_per_1k"`
	PricePer1K          float64 `json:"price_per_1k"`
	// SupplierShare is the physical workload's published market term. It is
	// stored on the result, rather than once on the schedule, so a schedule can
	// not silently apply the same take rate to every lane.
	SupplierShare float64 `json:"supplier_share,omitempty"`
	Formula       string  `json:"formula"` // human-readable, cites every real input (proof artifact)
}

const cataloguePriceScheduleVersion = 2

const (
	catalogueReferenceCurrency = "usd"
	priceFXRateEnv             = "MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE"
	priceFXRevisionEnv         = "MERC_PRICE_FX_REVISION"
)

// CataloguePriceSchedule is the all-or-nothing authority applied to models.
// It binds the exact board bytes, selector policy, per-workload supplier shares
// and complete model result set. A model row may point only at a durable
// schedule record.
type CataloguePriceSchedule struct {
	Version               int     `json:"version"`
	ReferenceCurrency     string  `json:"reference_currency"`
	SettlementCurrency    string  `json:"settlement_currency"`
	ReferenceToSettlement float64 `json:"reference_to_settlement_rate"`
	FXRevision            string  `json:"fx_revision"`
	BoardSHA256           string  `json:"board_sha256"`
	BoardSchemaVersion    int     `json:"board_schema_version"`
	BoardFetchedAt        string  `json:"board_fetched_at"`
	PositioningMultiplier float64 `json:"positioning_multiplier"`
	// SupplierShare is only populated on v1 schedules. New schedules carry a
	// per-result share under SupplierSharePolicyRevision.
	SupplierShare               float64         `json:"supplier_share,omitempty"`
	SupplierSharePolicyRevision string          `json:"supplier_share_policy_revision,omitempty"`
	Results                     []RepriceResult `json:"results"`
	SHA256                      string          `json:"sha256"`
}

// repriceFromSupplierEconomics is the cost-plus diagnostic: target supplier
// $/hr on the slowest measured laptop class. Kept for unit tests and for the
// market-gap report; it no longer drives the live catalogue.
func repriceFromSupplierEconomics(b measuredThroughput, supplierShare, electricityUSDPerKWh float64) RepriceResult {
	watts := sustainedWattsByHWClass[b.HWClass]
	if watts <= 0 {
		watts = 30.0 // conservative apple_silicon_pro-equivalent default, never zero
	}
	electricityUSDHr := watts / 1000.0 * electricityUSDPerKWh
	unitsPerHr := b.UnitsPerSec * 3600.0

	denom := unitsPerHr / 1000.0 * supplierShare
	var price float64
	if denom > 0 {
		price = (targetSupplierUSDHr + electricityUSDHr) / denom
	}

	formula := fmt.Sprintf(
		"price_per_1k = (target_supplier_usd_hr=%.2f + electricity_usd_hr=%.4f) / (units_per_hr=%.1f/1000 * supplier_share=%.4f) = %.8f  [source: %s, hw_class=%s, %.0fW @ $%.2f/kWh, platform_take=%.2f%%]",
		targetSupplierUSDHr, electricityUSDHr, unitsPerHr, supplierShare, price,
		b.SourceCitation, b.HWClass, watts, electricityUSDPerKWh, (1-supplierShare)*100,
	)
	return RepriceResult{
		ModelID: b.ModelID, JobType: b.JobType,
		ReferencePricePer1K: price, PricePer1K: price, Formula: formula,
	}
}

// --- market board -----------------------------------------------------------

type priceBoardObservation struct {
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	USDPer1K  float64 `json:"usd_per_1k"`
	USDPer1M  float64 `json:"usd_per_1m"`
	SourceURL string  `json:"source_url"`
	FetchedAt string  `json:"fetched_at"`
	Notes     string  `json:"notes"`
}

type priceBoardClass struct {
	Description  string                  `json:"description"`
	JobType      string                  `json:"job_type"`
	ModelIDs     []string                `json:"model_ids"`
	Observations []priceBoardObservation `json:"observations"`
}

type priceBoard struct {
	SchemaVersion         int                        `json:"schema_version"`
	Unit                  string                     `json:"unit"`
	FetchedAt             string                     `json:"fetched_at"`
	PositioningMultiplier float64                    `json:"positioning_multiplier"`
	Classes               map[string]priceBoardClass `json:"classes"`
}

// The board is loaded once per process and then reused, so /version can name a
// single answer to "which prices are you serving from".
//
// A FAILURE is deliberately not cached. sync.Once cached both outcomes, which
// made the price authority a function of whoever called first: main() resolves
// at startup in production, but in any other process the first caller wins, and
// under `go test` that is whichever test the -run filter happens to order first.
// A test that sets MERC_ENV=production for its own reasons therefore poisoned
// the board permanently and every later test that priced anything failed with a
// production refusal it never asked for. Caching the failure also buys nothing
// real: main() log.Fatalf's on a load error, so the process it matters in never
// gets a second call, and every other caller would rather retry than be told
// about a resolution that is no longer the one in effect.
var (
	priceBoardMu     sync.Mutex
	priceBoardCached *priceBoard
	priceBoardSHA256 string
)

// priceBoardSource records WHERE the loaded board came from, so /version can say
// so. A deployment that cannot answer "which price board are you serving from"
// cannot be audited after the fact.
var priceBoardSource string

func loadPriceBoard() (*priceBoard, error) {
	priceBoardMu.Lock()
	defer priceBoardMu.Unlock()
	if priceBoardCached != nil {
		return priceBoardCached, nil
	}
	var priceBoardErr error
	func() {
		resolved, err := resolvePriceBoard(os.Getenv("MERC_ENV"))
		if err != nil {
			priceBoardErr = err
			return
		}
		path := resolved.Path
		raw, err := os.ReadFile(path)
		if err != nil {
			priceBoardErr = fmt.Errorf("read price board %s (%s): %w",
				path, resolved.Source, err)
			return
		}
		// Before parsing. Bytes the operator did not approve must not be given the
		// chance to be well-formed.
		digest, err := verifyPriceBoardDigest(raw, resolved.ExpectedDigest)
		if err != nil {
			priceBoardErr = err
			return
		}
		priceBoardSource = resolved.Source
		var b priceBoard
		if err := json.Unmarshal(raw, &b); err != nil {
			priceBoardErr = fmt.Errorf("parse price board %s: %w", path, err)
			return
		}
		if b.SchemaVersion != 1 {
			priceBoardErr = fmt.Errorf("price board schema_version must be 1, got %d", b.SchemaVersion)
			return
		}
		if b.Unit != "usd_per_1k_units" {
			priceBoardErr = fmt.Errorf("price board unit must be usd_per_1k_units, got %q", b.Unit)
			return
		}
		if b.PositioningMultiplier <= 0 || math.IsNaN(b.PositioningMultiplier) || math.IsInf(b.PositioningMultiplier, 0) {
			priceBoardErr = fmt.Errorf("price board positioning_multiplier must be finite and > 0, got %v", b.PositioningMultiplier)
			return
		}
		if len(b.Classes) == 0 {
			priceBoardErr = fmt.Errorf("price board has no classes")
			return
		}
		// Parse the timestamp here so a malformed date is caught at load, but do
		// not age-filter: the board is data, and the read-only price page should
		// render what it says. Staleness is enforced where new pricing authority
		// is minted, in BuildCataloguePriceSchedule.
		if _, err := parseBoardTimestamp(b.FetchedAt); err != nil {
			priceBoardErr = fmt.Errorf("price board fetched_at is unusable: %w", err)
			return
		}
		priceBoardSHA256 = digest
		priceBoardCached = &b
	}()
	return priceBoardCached, priceBoardErr
}

// repriceFromMarketBoard prices one catalogue model from the sole governed
// selector: confidence-weighted median evidence times the positioning
// multiplier. Do not reintroduce an alternate min/mean derivation here: a
// price board is authority only when every published row follows one policy.
func repriceFromMarketBoard(modelID, jobType string, board *priceBoard) (RepriceResult, bool) {
	for className, class := range board.Classes {
		match := false
		for _, id := range class.ModelIDs {
			if id == modelID {
				match = true
				break
			}
		}
		if !match {
			// Fall back to job_type match only when model_ids is empty.
			if len(class.ModelIDs) == 0 && class.JobType == jobType {
				match = true
			}
		}
		if !match {
			continue
		}
		minPx, chosen, gerr := confidenceWeightedMedianUSDPer1K(className, class)
		if gerr != nil {
			// Ungoverned class: leave the existing catalogue price alone rather
			// than replacing it with something the evidence cannot support.
			continue
		}
		provider, model, source := chosen.Provider, chosen.Model, chosen.Source
		price := minPx * board.PositioningMultiplier
		formula := fmt.Sprintf(
			"price_per_1k = confidence_weighted_median(board[%s])×positioning_multiplier = %.8f×%.4f = %.8f  [median_obs=%s/%s source=%s fetched_at=%s]",
			className, minPx, board.PositioningMultiplier, price, provider, model, source, board.FetchedAt,
		)
		jt := jobType
		if class.JobType != "" {
			jt = class.JobType
		}
		return RepriceResult{
			ModelID: modelID, JobType: jt,
			ReferencePricePer1K: price, PricePer1K: price, Formula: formula,
		}, true
	}
	return RepriceResult{}, false
}

func catalogueFXAuthority(referenceCurrency, settlementCurrency string) (float64, string, error) {
	if referenceCurrency == settlementCurrency {
		return 1, "identity-" + referenceCurrency, nil
	}
	rawRate := strings.TrimSpace(os.Getenv(priceFXRateEnv))
	rawRevision := strings.TrimSpace(os.Getenv(priceFXRevisionEnv))
	if rawRate == "" || rawRevision == "" {
		return 0, "", fmt.Errorf(
			"%s and %s are required when catalogue reference currency %s differs from settlement currency %s",
			priceFXRateEnv, priceFXRevisionEnv, referenceCurrency, settlementCurrency,
		)
	}
	rate, err := strconv.ParseFloat(rawRate, 64)
	if err != nil || rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 0, "", fmt.Errorf("%s must be a finite positive number, got %q", priceFXRateEnv, rawRate)
	}
	if len(rawRevision) > 128 || strings.ContainsAny(rawRevision, "\r\n\t") {
		return 0, "", fmt.Errorf("%s must be a single non-empty line of at most 128 bytes", priceFXRevisionEnv)
	}
	return rate, rawRevision, nil
}

func ceilPricePer1K(value float64) float64 {
	const scale = 100000000
	return math.Ceil(value*scale) / scale
}

func cataloguePriceScheduleDigest(schedule CataloguePriceSchedule) (string, error) {
	payload := schedule
	payload.SHA256 = ""
	payload.Results = append([]RepriceResult(nil), schedule.Results...)
	sort.Slice(payload.Results, func(i, j int) bool {
		if payload.Results[i].ModelID == payload.Results[j].ModelID {
			return payload.Results[i].JobType < payload.Results[j].JobType
		}
		return payload.Results[i].ModelID < payload.Results[j].ModelID
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal catalogue price schedule: %w", err)
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum[:]), nil
}

func validateCataloguePriceSchedule(schedule CataloguePriceSchedule) error {
	if schedule.Version != 1 && schedule.Version != cataloguePriceScheduleVersion {
		return fmt.Errorf("unsupported catalogue price schedule version %d", schedule.Version)
	}
	if schedule.ReferenceCurrency != catalogueReferenceCurrency ||
		!finiteNonNegative(schedule.ReferenceToSettlement) || schedule.ReferenceToSettlement <= 0 ||
		strings.TrimSpace(schedule.FXRevision) == "" || len(schedule.FXRevision) > 128 ||
		strings.ContainsAny(schedule.FXRevision, "\r\n\t") ||
		!digestPattern.MatchString(schedule.BoardSHA256) ||
		schedule.BoardSchemaVersion != 1 ||
		strings.TrimSpace(schedule.BoardFetchedAt) == "" ||
		!finiteNonNegative(schedule.PositioningMultiplier) || schedule.PositioningMultiplier <= 0 ||
		len(schedule.Results) != len(repricingBenchmarks) {
		return fmt.Errorf("catalogue price schedule metadata is incomplete or invalid")
	}
	switch schedule.Version {
	case 1:
		if !finiteNonNegative(schedule.SupplierShare) || schedule.SupplierShare <= 0 || schedule.SupplierShare > 1 ||
			schedule.SupplierSharePolicyRevision != "" {
			return fmt.Errorf("legacy catalogue price schedule has invalid supplier share authority")
		}
	case cataloguePriceScheduleVersion:
		if schedule.SupplierShare != 0 || schedule.SupplierSharePolicyRevision != supplierSharePolicyRevision {
			return fmt.Errorf("catalogue price schedule must bind %s per-workload supplier shares", supplierSharePolicyRevision)
		}
	}
	settlement, err := ParseCurrency(schedule.SettlementCurrency)
	if err != nil || settlement.Code() != schedule.SettlementCurrency {
		return fmt.Errorf("catalogue price schedule settlement currency is invalid")
	}
	if schedule.ReferenceCurrency == schedule.SettlementCurrency {
		if schedule.ReferenceToSettlement != 1 || schedule.FXRevision != "identity-"+schedule.ReferenceCurrency {
			return fmt.Errorf("identity catalogue FX authority is invalid")
		}
	} else if strings.HasPrefix(schedule.FXRevision, "identity-") {
		return fmt.Errorf("cross-currency catalogue schedule cannot use identity FX authority")
	}
	expected := make(map[string]string, len(repricingBenchmarks))
	for _, b := range repricingBenchmarks {
		expected[b.ModelID] = b.JobType
	}
	seen := make(map[string]bool, len(schedule.Results))
	for _, result := range schedule.Results {
		if seen[result.ModelID] || expected[result.ModelID] != result.JobType ||
			!finiteNonNegative(result.ReferencePricePer1K) || result.ReferencePricePer1K <= 0 ||
			!finiteNonNegative(result.PricePer1K) || result.PricePer1K <= 0 ||
			strings.TrimSpace(result.Formula) == "" ||
			!strings.Contains(result.Formula, "board_sha256="+schedule.BoardSHA256) ||
			!strings.Contains(result.Formula, "fx_revision="+schedule.FXRevision) {
			return fmt.Errorf("invalid catalogue price result for model %q", result.ModelID)
		}
		if schedule.Version == 1 {
			if result.SupplierShare != 0 {
				return fmt.Errorf("legacy catalogue price result for model %q carries a per-workload share", result.ModelID)
			}
		} else if err := validateSupplierSharePolicy(result.JobType, result.ModelID, result.SupplierShare); err != nil {
			return fmt.Errorf("invalid catalogue supplier share for model %q: %w", result.ModelID, err)
		} else if !strings.Contains(result.Formula, "supplier_share_policy="+supplierSharePolicyRevision) ||
			!strings.Contains(result.Formula, fmt.Sprintf("supplier_share=%.8f", result.SupplierShare)) {
			return fmt.Errorf("catalogue price result for model %q lacks supplier-share provenance", result.ModelID)
		}
		wantPrice := ceilPricePer1K(result.ReferencePricePer1K * schedule.ReferenceToSettlement)
		if math.Abs(result.PricePer1K-wantPrice) > 0.0000000001 {
			return fmt.Errorf("catalogue price result for model %q is inconsistent with FX authority", result.ModelID)
		}
		seen[result.ModelID] = true
	}
	digest, err := cataloguePriceScheduleDigest(schedule)
	if err != nil {
		return err
	}
	if schedule.SHA256 == "" || schedule.SHA256 != digest {
		return fmt.Errorf("catalogue price schedule digest mismatch")
	}
	return nil
}

// BuildCataloguePriceSchedule derives one complete, canonical schedule from the
// exact governed board bytes. Any unpriced or underwater measured model blocks
// the entire schedule; boot never applies a profitable subset and leaves the
// rest on stale terms. Each result receives its own reviewed physical-workload
// share; there is intentionally no process-wide take-rate input.
func BuildCataloguePriceSchedule() (CataloguePriceSchedule, error) {
	loaded, err := loadPriceBoard()
	if err != nil {
		return CataloguePriceSchedule{}, err
	}
	// Publication is where market evidence becomes pricing authority, so it is
	// where age is enforced. Work on a copy: dropping stale rows must not mutate
	// the cached board that the read-only price page renders.
	board, err := boardAsOfPublication(loaded, priceBoardNow())
	if err != nil {
		return CataloguePriceSchedule{}, err
	}
	settlement, err := SettlementCurrency()
	if err != nil {
		return CataloguePriceSchedule{}, fmt.Errorf("build catalogue price schedule: %w", err)
	}
	settlementCurrency := settlement.Code()
	fxRate, fxRevision, err := catalogueFXAuthority(catalogueReferenceCurrency, settlementCurrency)
	if err != nil {
		return CataloguePriceSchedule{}, err
	}
	out := make([]RepriceResult, 0, len(repricingBenchmarks))
	for _, b := range repricingBenchmarks {
		r, ok := repriceFromMarketBoard(b.ModelID, b.JobType, board)
		if !ok {
			return CataloguePriceSchedule{}, fmt.Errorf(
				"price board has no governed price for measured model %s", b.ModelID,
			)
		}
		referencePrice := r.PricePer1K
		supplierShare, serr := supplierShareForWorkload(b.JobType, b.ModelID)
		if serr != nil {
			return CataloguePriceSchedule{}, serr
		}
		if gerr := governPublishedPrice(b, referencePrice, supplierShare); gerr != nil {
			return CataloguePriceSchedule{}, gerr
		}
		r.ReferencePricePer1K = referencePrice
		r.PricePer1K = ceilPricePer1K(referencePrice * fxRate)
		r.SupplierShare = supplierShare
		r.Formula += fmt.Sprintf(
			" board_sha256=%s reference_price_per_1k=%.8f reference_currency=%s reference_to_settlement_rate=%.12g fx_revision=%s settlement_price_per_1k=%.8f settlement_currency=%s supplier_share_policy=%s supplier_share=%.8f",
			priceBoardSHA256, r.ReferencePricePer1K, catalogueReferenceCurrency,
			fxRate, fxRevision, r.PricePer1K, settlementCurrency,
			supplierSharePolicyRevision, supplierShare,
		)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelID < out[j].ModelID })
	schedule := CataloguePriceSchedule{
		Version:                     cataloguePriceScheduleVersion,
		ReferenceCurrency:           catalogueReferenceCurrency,
		SettlementCurrency:          settlementCurrency,
		ReferenceToSettlement:       fxRate,
		FXRevision:                  fxRevision,
		BoardSHA256:                 priceBoardSHA256,
		BoardSchemaVersion:          board.SchemaVersion,
		BoardFetchedAt:              board.FetchedAt,
		PositioningMultiplier:       board.PositioningMultiplier,
		SupplierSharePolicyRevision: supplierSharePolicyRevision,
		Results:                     out,
	}
	schedule.SHA256, err = cataloguePriceScheduleDigest(schedule)
	if err != nil {
		return CataloguePriceSchedule{}, err
	}
	if err := validateCataloguePriceSchedule(schedule); err != nil {
		return CataloguePriceSchedule{}, err
	}
	return schedule, nil
}

// RepriceCatalogueFromSupplierEconomics keeps the historical diagnostic API
// for reports and unit tests. Startup uses the error-returning schedule builder
// and therefore cannot silently retain a stale partial catalogue.
func RepriceCatalogueFromSupplierEconomics() []RepriceResult {
	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		log.Printf("catalogue price schedule rejected: %v", err)
		return nil
	}
	return append([]RepriceResult(nil), schedule.Results...)
}

// CostFloorVsMarket reports, for each measured model, the cost-plus floor
// versus the market-board catalogue price. Used in tests and operator reports.
type CostFloorVsMarket struct {
	ModelID          string
	JobType          string
	CostPlusPer1K    float64
	MarketBoardPer1K float64
	GapRatio         float64 // cost_plus / market; >1 means cost basis above market
}

func CompareCostFloorToMarketBoard(supplierShare float64) []CostFloorVsMarket {
	board, err := loadPriceBoard()
	if err != nil {
		return nil
	}
	var out []CostFloorVsMarket
	for _, b := range repricingBenchmarks {
		cost := repriceFromSupplierEconomics(b, supplierShare, defaultElectricityUSDPerKWh)
		mkt, ok := repriceFromMarketBoard(b.ModelID, b.JobType, board)
		if !ok || mkt.PricePer1K <= 0 {
			continue
		}
		out = append(out, CostFloorVsMarket{
			ModelID:          b.ModelID,
			JobType:          b.JobType,
			CostPlusPer1K:    cost.PricePer1K,
			MarketBoardPer1K: mkt.PricePer1K,
			GapRatio:         cost.PricePer1K / mkt.PricePer1K,
		})
	}
	return out
}

// SupplierViabilityAtMarket answers the question the price board cannot: at the
// catalogue price we actually charge, does the supplier clear their own power
// bill?
//
// Moving from cost-plus to a market board fixed a price that was ~460x above the
// market and, in the same stroke, cut supplier gross by the same factor. On the
// hardware the control plane currently admits (Apple Silicon, 138.7 tok/s
// measured) the two numbers land within a rounding error of each other: about
// $0.00436/hr of gross against $0.0045/hr of electricity. A marketplace whose
// suppliers pay to participate has no supply side, and that fact must be visible
// in an operator report rather than discovered by a supplier reading their
// electricity bill.
//
// This is a report, not a guard. Refusing to price at market would simply hide
// the problem behind an uncompetitive catalogue; the honest response is to make
// both facts legible at once and let the operator decide which side to fix.
type SupplierViabilityAtMarket struct {
	ModelID              string  `json:"model_id"`
	JobType              string  `json:"job_type"`
	SupplierShare        float64 `json:"supplier_share"`
	HWClass              string  `json:"hw_class"`
	MeasuredUnitsPerSec  float64 `json:"measured_units_per_sec"`
	CataloguePricePer1K  float64 `json:"catalogue_price_per_1k"`
	SupplierGrossUSDHr   float64 `json:"supplier_gross_usd_hr"`
	ElectricityUSDHr     float64 `json:"electricity_usd_hr"`
	NetUSDHr             float64 `json:"net_usd_hr"`
	BreakEvenUnitsPerSec float64 `json:"break_even_units_per_sec"`
	Viable               bool    `json:"viable"`
}

// SupplierViabilityReport evaluates every measured model against the catalogue
// price the buyer is actually charged, under the same physical-workload share
// policy that publication binds into the catalogue.
func SupplierViabilityReport() []SupplierViabilityAtMarket {
	board, err := loadPriceBoard()
	if err != nil {
		return nil
	}
	var out []SupplierViabilityAtMarket
	for _, b := range repricingBenchmarks {
		mkt, ok := repriceFromMarketBoard(b.ModelID, b.JobType, board)
		if !ok || mkt.PricePer1K <= 0 {
			continue
		}
		supplierShare, err := supplierShareForWorkload(b.JobType, b.ModelID)
		if err != nil {
			continue
		}
		watts := sustainedWattsByHWClass[b.HWClass]
		if watts <= 0 {
			watts = 30.0
		}
		elec := watts / 1000.0 * defaultElectricityUSDPerKWh
		unitsPerHr := b.UnitsPerSec * 3600.0
		gross := unitsPerHr / 1000.0 * mkt.PricePer1K * supplierShare
		var breakEven float64
		if perUnitHr := mkt.PricePer1K / 1000.0 * supplierShare; perUnitHr > 0 {
			breakEven = elec / perUnitHr / 3600.0
		}
		out = append(out, SupplierViabilityAtMarket{
			ModelID: b.ModelID, JobType: b.JobType, SupplierShare: supplierShare, HWClass: b.HWClass,
			MeasuredUnitsPerSec:  b.UnitsPerSec,
			CataloguePricePer1K:  mkt.PricePer1K,
			SupplierGrossUSDHr:   roundUSD(gross),
			ElectricityUSDHr:     roundUSD(elec),
			NetUSDHr:             roundUSD(gross - elec),
			BreakEvenUnitsPerSec: breakEven,
			Viable:               gross > elec,
		})
	}
	return out
}

const actualUSDBasisQuoteDerivedSettlement = "quote_derived_per_task_buyer_charge_settlement"

type PriceTuningBlockReason string

const priceTuningBlockedNoIndependentTelemetry PriceTuningBlockReason = "independent_execution_cost_telemetry_unavailable"

type CostDriftRow struct {
	JobType           string                 `json:"job_type"`
	ModelRef          string                 `json:"model_ref"`
	Samples           int                    `json:"samples"`             // quote-bound, terminal jobs behind this rollup
	AvgQuotedUSD      float64                `json:"avg_quoted_usd"`      // mean quotes.cost_expected_usd
	AvgActualUSD      float64                `json:"avg_actual_usd"`      // mean quote-derived jobs.actual_usd settlement
	DriftRatio        float64                `json:"drift_ratio"`         // settled charge / quoted charge; not a cost-overrun ratio
	DriftPct          float64                `json:"drift_pct"`           // (drift_ratio - 1) * 100, signed charge-realization difference
	ActualUSDBasis    string                 `json:"actual_usd_basis"`    // explicitly names the source semantics of AvgActualUSD
	UsingForTuning    bool                   `json:"using_for_tuning"`    // always false until independent economic telemetry exists
	TuningBlockReason PriceTuningBlockReason `json:"tuning_block_reason"` // machine-readable fail-closed reason
}

func finalizeCostDriftRow(d CostDriftRow) CostDriftRow {
	if d.AvgQuotedUSD > 0 {
		d.DriftRatio = d.AvgActualUSD / d.AvgQuotedUSD
		d.DriftPct = (d.DriftRatio - 1) * 100
	}
	d.ActualUSDBasis = actualUSDBasisQuoteDerivedSettlement
	d.UsingForTuning = false
	d.TuningBlockReason = priceTuningBlockedNoIndependentTelemetry
	return d
}

func (s *Store) CostDriftRollup(ctx context.Context) ([]CostDriftRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT j.job_type,
		        COALESCE(j.model_ref,''),
		        COUNT(*),
		        COALESCE(AVG(q.cost_expected_usd),0),
		        COALESCE(AVG(j.actual_usd),0)
		   FROM jobs j
		   JOIN quotes q ON q.id = j.quote_id
		  WHERE j.quote_id IS NOT NULL
		    AND j.status IN ('complete','failed')
		    AND q.cost_expected_usd > 0
		  GROUP BY j.job_type, j.model_ref
		  ORDER BY COUNT(*) DESC, j.job_type, j.model_ref`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostDriftRow
	for rows.Next() {
		var d CostDriftRow
		if err := rows.Scan(&d.JobType, &d.ModelRef, &d.Samples, &d.AvgQuotedUSD, &d.AvgActualUSD); err != nil {
			return nil, err
		}
		out = append(out, finalizeCostDriftRow(d))
	}
	return out, rows.Err()
}

func (s *Store) ApplyRepricing(ctx context.Context, schedule CataloguePriceSchedule) (updated int, err error) {
	if err := validateCataloguePriceSchedule(schedule); err != nil {
		return 0, fmt.Errorf("refusing invalid catalogue price schedule: %w", err)
	}
	if err := RequireSettlementCurrency(schedule.SettlementCurrency); err != nil {
		return 0, fmt.Errorf("refusing catalogue schedule for another settlement authority: %w", err)
	}
	scheduleJSON, err := json.Marshal(schedule)
	if err != nil {
		return 0, fmt.Errorf("marshal catalogue price schedule: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('merc-catalogue-price-schedule-v1',0))`); err != nil {
		return 0, fmt.Errorf("lock catalogue price authority: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO catalogue_price_schedules (
		  sha256,version,reference_currency,settlement_currency,
		  reference_to_settlement_rate,fx_revision,board_sha256,board_schema_version,
		  board_fetched_at,positioning_multiplier,supplier_share,schedule_json
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (sha256) DO NOTHING`,
		schedule.SHA256, schedule.Version, schedule.ReferenceCurrency, schedule.SettlementCurrency,
		schedule.ReferenceToSettlement, schedule.FXRevision,
		schedule.BoardSHA256, schedule.BoardSchemaVersion, schedule.BoardFetchedAt,
		schedule.PositioningMultiplier, nullIfZero(schedule.SupplierShare), scheduleJSON,
	); err != nil {
		return 0, fmt.Errorf("record catalogue price schedule: %w", err)
	}

	for _, result := range schedule.Results {
		var priorPrice, priorReferencePrice float64
		var priorSource, priorFormula, priorSchedule, priorCurrency string
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(price_per_1k,0),COALESCE(price_source,''),
			       COALESCE(price_formula,''),COALESCE(price_schedule_sha256,''),
			       COALESCE(price_currency,''),COALESCE(price_reference_per_1k,0)
			  FROM models WHERE id=$1 FOR UPDATE`,
			result.ModelID,
		).Scan(
			&priorPrice, &priorSource, &priorFormula, &priorSchedule,
			&priorCurrency, &priorReferencePrice,
		); err != nil {
			return 0, fmt.Errorf("lock catalogue model %s: %w", result.ModelID, err)
		}
		switch priorSource {
		case "", "seed", "measured_supplier_economics", "market_board":
		default:
			return 0, fmt.Errorf("model %s has operator-owned price source %q", result.ModelID, priorSource)
		}
		if priorSchedule == schedule.SHA256 {
			if math.Abs(priorPrice-result.PricePer1K) > 0.0000000001 ||
				math.Abs(priorReferencePrice-result.ReferencePricePer1K) > 0.0000000001 ||
				priorSource != "market_board" || priorFormula != result.Formula ||
				priorCurrency != schedule.SettlementCurrency {
				return 0, fmt.Errorf("model %s conflicts with its recorded price schedule", result.ModelID)
			}
			continue
		}
		// The skip above is keyed on models.price_schedule_sha256, but this row is
		// keyed on (schedule_sha256, model_id). Anything that resets model price
		// state without clearing the history - a re-seed, or a restore that brings
		// back one table and not the other - makes the skip miss while the row
		// still exists, and the control plane then cannot start at all. Recovery
		// is exactly when that must not happen.
		//
		// An existing row describing the same transition is therefore not an
		// error; one describing a different outcome for the same schedule still is.
		var recordedPrice, recordedReference, recordedSupplierShare float64
		var recordedCurrency, recordedFormula string
		switch err := tx.QueryRow(ctx, `
			SELECT price_per_1k,reference_price_per_1k,price_currency,price_formula,
			       COALESCE(supplier_share,0)
			  FROM model_price_history WHERE schedule_sha256=$1 AND model_id=$2`,
			schedule.SHA256, result.ModelID,
		).Scan(&recordedPrice, &recordedReference, &recordedCurrency, &recordedFormula, &recordedSupplierShare); {
		case errors.Is(err, pgx.ErrNoRows):
			if _, err := tx.Exec(ctx, `
				INSERT INTO model_price_history (
				  schedule_sha256,model_id,prior_price_per_1k,prior_price_source,
				  reference_price_per_1k,reference_currency,price_per_1k,
				  price_currency,price_formula,supplier_share
				) VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10)`,
				schedule.SHA256, result.ModelID, priorPrice, priorSource,
				result.ReferencePricePer1K, schedule.ReferenceCurrency,
				result.PricePer1K, schedule.SettlementCurrency, result.Formula,
				nullIfZero(result.SupplierShare),
			); err != nil {
				return 0, fmt.Errorf("record price history for %s: %w", result.ModelID, err)
			}
		case err != nil:
			return 0, fmt.Errorf("read recorded price history for %s: %w", result.ModelID, err)
		default:
			if math.Abs(recordedPrice-result.PricePer1K) > 0.0000000001 ||
				math.Abs(recordedReference-result.ReferencePricePer1K) > 0.0000000001 ||
				recordedCurrency != schedule.SettlementCurrency ||
				recordedFormula != result.Formula ||
				math.Abs(recordedSupplierShare-result.SupplierShare) > 0.0000000001 {
				return 0, fmt.Errorf(
					"model %s already has a different recorded outcome for schedule %s",
					result.ModelID, schedule.SHA256)
			}
		}
		tag, err := tx.Exec(ctx, `
			UPDATE models
			   SET price_per_1k=$2,price_source='market_board',price_formula=$3,
			       price_reference_currency=$4,price_schedule_sha256=$5,
			       price_schedule_version=$6,price_currency=$7,
			       price_reference_per_1k=$8
			 WHERE id=$1`,
			result.ModelID, result.PricePer1K, result.Formula,
			schedule.ReferenceCurrency, schedule.SHA256, schedule.Version,
			schedule.SettlementCurrency, result.ReferencePricePer1K,
		)
		if err != nil {
			return 0, fmt.Errorf("apply repricing for %s: %w", result.ModelID, err)
		}
		if tag.RowsAffected() != 1 {
			return 0, fmt.Errorf("apply repricing for %s changed %d rows", result.ModelID, tag.RowsAffected())
		}
		updated++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return updated, nil
}

// nullIfZero keeps v1 append-only schedule rows readable while ensuring a v2
// schedule never records a made-up process-wide supplier share in the legacy
// column. The v2 authority lives on each model_price_history result.
func nullIfZero(value float64) any {
	if value == 0 {
		return nil
	}
	return value
}
