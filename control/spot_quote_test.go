package main

import (
	"strings"
	"testing"
)

func spotQuoteTestRequest(t *testing.T) SpotQuoteRequest {
	t.Helper()
	return SpotQuoteRequest{
		Preference: workloadObjectiveBalanced,
		Schedule:   testEconomicSchedule(),
		Workload: SpotQuoteWorkload{
			InputRecords:         8,
			InputBytes:           4096,
			SettlementInputUnits: 1000,
			CataloguePricePer1K:  1.0,
			SupplierShare:        0.97,
		},
	}
}

func spotQuotePlan(primary, redundancy, honeypot, extra int, bytes int64, units, base float64) (ComputePlan, int) {
	total := primary + redundancy + honeypot
	split := 1
	if primary > 0 {
		split = 8 / primary
		if split < 1 {
			split = 1
		}
	}
	plan := ComputePlan{
		ExecutionMode:           computeExecutionDistributed,
		InputRecords:            8,
		InputBytes:              bytes,
		SettlementInputUnits:    units,
		SplitSize:               split,
		PrimaryTasks:            primary,
		RedundancyTasks:         redundancy,
		HoneypotTasks:           honeypot,
		TotalInitialTasks:       total,
		BaseComputeUSD:          base,
		ETAP50Secs:              30,
		ETAP90Secs:              45,
		ETAWorstCaseSecs:        120,
		ETASource:               "planner",
		ETAConfidenceBandMethod: etaBandMethodPlannerConservativeBound,
	}
	return plan, extra
}

func TestSpotQuoteNumbersStayOrderedAcrossPlans(t *testing.T) {
	zero := 0
	four := 4
	table := []struct {
		name       string
		primary    int
		redundancy int
		honeypot   int
		extra      *int
		bytes      int64
		units      float64
		base       float64
		maxPrice   float64
		// A plan that cannot clear its own economics is REFUSED, and refusing is
		// the point of this producer, not a failure of it. The table therefore
		// states which rows are expected to quote and which to refuse, and both
		// are asserted. Expecting every row to quote hid the refusal path.
		wantRefused bool
	}{
		{name: "single-task-no-extra", primary: 1, extra: &zero, bytes: 128, units: 100, base: 1.0, wantRefused: true},
		{name: "single-task-honeypot-no-extra", primary: 1, honeypot: 1, extra: &zero, bytes: 128, units: 100, base: 1.0},
		{name: "multi-with-default-reserve", primary: 4, redundancy: 1, honeypot: 1, bytes: 512, units: 1000, base: 1.0},
		{name: "zero-bytes", primary: 2, redundancy: 1, honeypot: 1, extra: &four, bytes: 0, units: 50, base: 2.0},
		{name: "tiny-units-min-billable", primary: 1, honeypot: 1, bytes: 16, units: 1, base: 0.000018},
		{name: "heavy-verification", primary: 1, redundancy: 8, honeypot: 2, bytes: 256, units: 200, base: 1.0},
		{name: "buyer-max-above-reserved", primary: 2, redundancy: 1, honeypot: 1, bytes: 256, units: 200, base: 1.0, maxPrice: 100},
		{name: "one-record-geometry", primary: 1, redundancy: 0, honeypot: 1, extra: &zero, bytes: 32, units: 8, base: 0.5},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			req := spotQuoteTestRequest(t)
			req.MaxPriceUSD = tc.maxPrice
			req.ExtraTaskReserve = tc.extra
			plan, extra := spotQuotePlan(tc.primary, tc.redundancy, tc.honeypot, 0, tc.bytes, tc.units, tc.base)
			if tc.extra != nil {
				extra = *tc.extra
				req.ExtraTaskReserve = &extra
			}
			got := PriceSpotQuotePlan(req, plan)
			if tc.wantRefused {
				if got.Status == spotQuoteQuoted {
					t.Fatalf("plan quoted at %+v, but its economics do not clear and it must be refused",
						got.Numbers)
				}
				if got.Refusal == nil || got.Refusal.Term == "" {
					t.Fatalf("refusal must name the term that failed, got %+v", got.Refusal)
				}
				return
			}
			if got.Status != spotQuoteQuoted {
				t.Fatalf("quoted plan refused: %+v", got.Refusal)
			}
			n := got.Numbers
			if n.EstimateUSD > n.LikelyRangeHighUSD {
				t.Fatalf("estimate $%.6f exceeds likely-range high $%.6f",
					n.EstimateUSD, n.LikelyRangeHighUSD)
			}
			if n.LikelyRangeHighUSD > n.MaximumAuthorizedChargeUSD {
				t.Fatalf("likely-range high $%.6f exceeds maximum authorized charge $%.6f",
					n.LikelyRangeHighUSD, n.MaximumAuthorizedChargeUSD)
			}
			if n.LikelyRangeLowUSD > n.EstimateUSD {
				t.Fatalf("likely-range low $%.6f exceeds estimate $%.6f",
					n.LikelyRangeLowUSD, n.EstimateUSD)
			}
			if !n.IsBuyerAcceptedBound() {
				t.Fatalf("ceiling is not the bound the buyer accepted: %+v", n)
			}
			if tc.extra != nil && *tc.extra == 0 && tc.maxPrice == 0 &&
				n.EstimateUSD != n.LikelyRangeHighUSD {
				t.Fatalf("degenerate extra-reserve 0: estimate $%.6f != likely-range high $%.6f",
					n.EstimateUSD, n.LikelyRangeHighUSD)
			}
			if tc.maxPrice > 0 && n.MaximumAuthorizedChargeUSD != roundEconomicUSD(tc.maxPrice) {
				t.Fatalf("buyer max $%.6f quoted as ceiling $%.6f",
					tc.maxPrice, n.MaximumAuthorizedChargeUSD)
			}
			if n.CeilingBasis != spotCeilingBuyerMaxPrice &&
				n.CeilingBasis != spotCeilingPlanReservedCharge {
				t.Fatalf("unknown ceiling basis %q", n.CeilingBasis)
			}
		})
	}
}

func TestSpotQuoteRefusesNamingTheUnacceptableTerm(t *testing.T) {
	req := spotQuoteTestRequest(t)
	req.Preference = workloadObjectiveFastest
	req.Workload.StartupUSD = 10_000
	got := ProduceSpotQuote(req)
	if got.Status != spotQuoteRefused || got.Refusal == nil {
		t.Fatalf("expected a refused quote, got %+v", got)
	}
	if got.Refusal.Term != spotTermStartup {
		t.Fatalf("refusal named %q, want %s (the term that made the economics unacceptable); reason=%q",
			got.Refusal.Term, spotTermStartup, got.Refusal.Reason)
	}
	if !strings.Contains(got.Refusal.Reason, spotTermStartup) &&
		!strings.Contains(strings.ToLower(got.Refusal.Reason), "startup") {
		t.Fatalf("refusal reason does not name the term: %q", got.Refusal.Reason)
	}

	// A buyer max below the executable charge is also a refusal, named from
	// the economic block rather than silently loss-led.
	capped := spotQuoteTestRequest(t)
	capped.MaxPriceUSD = 0.000001
	plan, _ := spotQuotePlan(1, 0, 1, 0, 64, 100, 1.0)
	blocked := PriceSpotQuotePlan(capped, plan)
	if blocked.Status != spotQuoteRefused || blocked.Refusal == nil {
		t.Fatalf("lossy buyer max was not refused: %+v", blocked)
	}
	if blocked.Refusal.Term == "" {
		t.Fatal("refusal did not name a term")
	}
}

func TestSpotQuotePreferenceChangesPlanNotCeilingSemantics(t *testing.T) {
	const buyerMax = 50.0
	base := spotQuoteTestRequest(t)
	base.MaxPriceUSD = buyerMax

	var quotes []SpotQuote
	for _, pref := range []string{
		workloadObjectiveCheapest,
		workloadObjectiveBalanced,
		workloadObjectiveFastest,
	} {
		req := base
		req.Preference = pref
		got := ProduceSpotQuote(req)
		if got.Status != spotQuoteQuoted {
			t.Fatalf("%s refused: %+v", pref, got.Refusal)
		}
		if !got.Numbers.IsBuyerAcceptedBound() {
			t.Fatalf("%s ceiling is not the bound the buyer accepted: %+v", pref, got.Numbers)
		}
		if got.Numbers.CeilingBasis != spotCeilingBuyerMaxPrice {
			t.Fatalf("%s ceiling basis %q, want %s (buyer-named max; preference must not change ceiling semantics)",
				pref, got.Numbers.CeilingBasis, spotCeilingBuyerMaxPrice)
		}
		if got.Numbers.MaximumAuthorizedChargeUSD != roundEconomicUSD(buyerMax) {
			t.Fatalf("%s maximum authorized charge $%.6f, want the buyer-accepted bound $%.6f",
				pref, got.Numbers.MaximumAuthorizedChargeUSD, buyerMax)
		}
		quotes = append(quotes, got)
	}

	cheap, balanced, fastest := quotes[0], quotes[1], quotes[2]
	if cheap.Plan.SplitSize == balanced.Plan.SplitSize &&
		cheap.Plan.PrimaryTasks == balanced.Plan.PrimaryTasks &&
		cheap.Plan.TotalInitialTasks == balanced.Plan.TotalInitialTasks &&
		cheap.Plan.ETAP50Secs == balanced.Plan.ETAP50Secs {
		t.Fatalf("CHEAPEST and BALANCED selected the same plan: %+v", cheap.Plan)
	}
	if balanced.Plan.SplitSize == fastest.Plan.SplitSize &&
		balanced.Plan.PrimaryTasks == fastest.Plan.PrimaryTasks &&
		balanced.Plan.TotalInitialTasks == fastest.Plan.TotalInitialTasks &&
		balanced.Plan.ETAP50Secs == fastest.Plan.ETAP50Secs {
		t.Fatalf("BALANCED and FASTEST selected the same plan: %+v", balanced.Plan)
	}
	if cheap.Plan.SplitSize == fastest.Plan.SplitSize &&
		cheap.Plan.PrimaryTasks == fastest.Plan.PrimaryTasks &&
		cheap.Plan.TotalInitialTasks == fastest.Plan.TotalInitialTasks &&
		cheap.Plan.ETAP50Secs == fastest.Plan.ETAP50Secs {
		t.Fatalf("CHEAPEST and FASTEST selected the same plan: %+v", cheap.Plan)
	}
	if cheap.Numbers.EstimateUSD == balanced.Numbers.EstimateUSD &&
		balanced.Numbers.EstimateUSD == fastest.Numbers.EstimateUSD {
		t.Fatalf("preference changed the plan but not the estimate: cheap=$%.6f balanced=$%.6f fastest=$%.6f",
			cheap.Numbers.EstimateUSD, balanced.Numbers.EstimateUSD, fastest.Numbers.EstimateUSD)
	}
	// Ceiling semantics: same bound, same basis, for every preference.
	for _, q := range quotes {
		if q.Numbers.MaximumAuthorizedChargeUSD != cheap.Numbers.MaximumAuthorizedChargeUSD ||
			q.Numbers.CeilingBasis != cheap.Numbers.CeilingBasis {
			t.Fatalf("preference %s changed ceiling semantics: got charge=$%.6f basis=%s, want charge=$%.6f basis=%s",
				q.Preference, q.Numbers.MaximumAuthorizedChargeUSD, q.Numbers.CeilingBasis,
				cheap.Numbers.MaximumAuthorizedChargeUSD, cheap.Numbers.CeilingBasis)
		}
	}
}

func TestSpotQuoteCostTermsAreSeparatelyReadable(t *testing.T) {
	req := spotQuoteTestRequest(t)
	req.Preference = workloadObjectiveFastest
	got := ProduceSpotQuote(req)
	if got.Status != spotQuoteQuoted {
		t.Fatalf("FASTEST quote refused: %+v", got.Refusal)
	}
	if len(spotQuoteTermNames) != 10 {
		t.Fatalf("term catalogue has %d names, want 10 inspectable cost terms", len(spotQuoteTermNames))
	}
	seen := map[string]PricingCostComponent{}
	for _, name := range spotQuoteTermNames {
		comp, ok := got.Terms.Component(name)
		if !ok {
			t.Fatalf("term %s is not separately readable", name)
		}
		if strings.TrimSpace(comp.Basis) == "" {
			t.Fatalf("term %s has no basis; a blended number is not auditable", name)
		}
		if comp.Status != pricingCostModeled &&
			comp.Status != pricingCostNotApplicable &&
			comp.Status != pricingCostUnknown {
			t.Fatalf("term %s has invalid status %q", name, comp.Status)
		}
		seen[name] = comp
		t.Logf("term %-18s status=%-16s amount=$%.6f basis=%s",
			name, comp.Status, comp.Amount, comp.Basis)
	}
	if seen[spotTermCompute].Amount == seen[spotTermSupplierFloor].Amount &&
		seen[spotTermCompute].Basis == seen[spotTermSupplierFloor].Basis {
		t.Fatal("compute and supplier floor are not distinct inspectable terms")
	}
	if got.Terms.SupplierFloor.Status != pricingCostModeled || got.Terms.SupplierFloor.Amount <= 0 {
		t.Fatalf("supplier floor is not a separately modeled amount: %+v", got.Terms.SupplierFloor)
	}
	if got.Terms.Payment.Status != pricingCostModeled {
		t.Fatalf("payment costs are not separately readable: %+v", got.Terms.Payment)
	}
	if got.Terms.MercContribution.Status != pricingCostModeled || got.Terms.MercContribution.Amount <= 0 {
		t.Fatalf("Merc contribution is not a separately modeled positive amount: %+v", got.Terms.MercContribution)
	}
}
