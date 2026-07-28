package main

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
)

// The catalogue used to price a class as min(observations). That is the most
// fragile selector available: one stale, promotional, mistyped or third-party
// observation drags every supplier underwater with no guard. The board shipped
// with exactly that hazard -- the $0.01/1M row that set the price for the whole
// infer_small class cited a competitor's marketing blog rather than the named
// vendor's own pricing page.
//
// Prices are now a confidence-weighted median over normalised observations, and
// a price that cannot pay its supply side is refused outright.

// observationConfidence scores how much a single observation should count.
// A vendor quoting its own list price is the strong case; anything else is
// hearsay until corroborated.
func observationConfidence(provider, sourceURL string) float64 {
	u, err := url.Parse(strings.TrimSpace(sourceURL))
	if err != nil || u.Host == "" {
		return 0.2 // unattributed
	}
	host := strings.ToLower(u.Host)
	host = strings.TrimPrefix(host, "www.")
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		return 0.2
	}
	// Vendor-owned: the host contains the provider name (openai.com,
	// docs.voyageai.com, together.ai, ...).
	if strings.Contains(host, p) {
		return 1.0
	}
	// Someone else's page describing this provider's prices.
	return 0.35
}

// normalisedObservation is one board row reduced to usd_per_1k with a weight.
type normalisedObservation struct {
	Provider   string
	Model      string
	Source     string
	USDPer1K   float64
	Confidence float64
}

// normaliseObservations converts every usable row to usd_per_1k and scores it.
func normaliseObservations(class priceBoardClass) []normalisedObservation {
	var out []normalisedObservation
	for _, obs := range class.Observations {
		p := obs.USDPer1K
		if p <= 0 && obs.USDPer1M > 0 {
			p = obs.USDPer1M / 1000.0
		}
		if p <= 0 || math.IsNaN(p) || math.IsInf(p, 0) {
			continue
		}
		out = append(out, normalisedObservation{
			Provider:   obs.Provider,
			Model:      obs.Model,
			Source:     obs.SourceURL,
			USDPer1K:   p,
			Confidence: observationConfidence(obs.Provider, obs.SourceURL),
		})
	}
	return out
}

// Governance floors. A class priced from too little evidence, or from evidence
// that is entirely hearsay, is not priced at all -- the previous catalogue price
// stands rather than being replaced by something unaccountable.
const (
	minBoardObservations    = 3
	minBoardTotalConfidence = 1.0
)

// Age is the third way an observation stops being evidence, after count and
// attribution. A row keeps its vendor-owned confidence of 1.0 forever, so
// without this a board fetched once would price the catalogue indefinitely and
// a single old row could still carry a class. Stale rows are dropped at load,
// which lets the count and confidence floors above do the rest: a class that
// falls below them is refused, and the previous catalogue price stands.
const (
	// An observation older than this is not current market evidence.
	maxObservationAge = 45 * 24 * time.Hour
	// A board whose own fetch is older than this cannot be published from at
	// all, however fresh individual rows claim to be.
	maxBoardAge = 60 * 24 * time.Hour
)

// Boards carry plain dates; observations sometimes carry a full timestamp.
var boardTimestampLayouts = []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02"}

func parseBoardTimestamp(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("timestamp is empty")
	}
	for _, layout := range boardTimestampLayouts {
		if at, err := time.Parse(layout, trimmed); err == nil {
			return at.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("timestamp %q is not RFC3339 or YYYY-MM-DD", trimmed)
}

// priceBoardNow is the clock publication ages the board against. It is a seam
// so tests that exercise the committed board can pin a date near its fetch:
// a wall-clock comparison against fixture data would otherwise turn every such
// test into a dated failure that arrives with no code change.
var priceBoardNow = func() time.Time { return time.Now().UTC() }

// boardAsOfPublication returns a copy of the board with stale rows removed, or
// an error if the board as a whole is too old to mint pricing authority from.
// The copy matters: the cached board backs the read-only price page, which
// should keep rendering what the board actually says.
func boardAsOfPublication(board *priceBoard, now time.Time) (*priceBoard, error) {
	boardAt, err := parseBoardTimestamp(board.FetchedAt)
	if err != nil {
		return nil, fmt.Errorf("price board fetched_at is unusable: %w", err)
	}
	copied := *board
	copied.Classes = make(map[string]priceBoardClass, len(board.Classes))
	for name, class := range board.Classes {
		copied.Classes[name] = class
	}
	if err := dropStaleObservations(&copied, boardAt, now); err != nil {
		return nil, err
	}
	return &copied, nil
}

// dropStaleObservations removes rows that are no longer current evidence, and
// refuses a board that is wholly stale. Observations without their own
// fetched_at inherit the board's, so an undated row cannot outlive the board.
func dropStaleObservations(board *priceBoard, boardAt, now time.Time) error {
	if now.Sub(boardAt) > maxBoardAge {
		return fmt.Errorf(
			"price board fetched_at %s is %.0f days old (ceiling %.0f days); refusing to price from it",
			boardAt.Format("2006-01-02"), now.Sub(boardAt).Hours()/24, maxBoardAge.Hours()/24)
	}
	if boardAt.After(now.Add(24 * time.Hour)) {
		return fmt.Errorf("price board fetched_at %s is in the future", boardAt.Format("2006-01-02"))
	}
	for name, class := range board.Classes {
		kept := make([]priceBoardObservation, 0, len(class.Observations))
		for _, obs := range class.Observations {
			at := boardAt
			if strings.TrimSpace(obs.FetchedAt) != "" {
				parsed, err := parseBoardTimestamp(obs.FetchedAt)
				if err != nil {
					// An unparseable date is not a fresh date.
					continue
				}
				at = parsed
			}
			if now.Sub(at) > maxObservationAge || at.After(now.Add(24*time.Hour)) {
				continue
			}
			kept = append(kept, obs)
		}
		class.Observations = kept
		board.Classes[name] = class
	}
	return nil
}

type errBoardUngoverned struct {
	Class      string
	Count      int
	Confidence float64
}

func (e errBoardUngoverned) Error() string {
	return fmt.Sprintf(
		"price board class %q is ungoverned: %d usable observation(s), total confidence %.2f "+
			"(need >=%d observations and >=%.2f confidence); refusing to reprice",
		e.Class, e.Count, e.Confidence, minBoardObservations, minBoardTotalConfidence)
}

// confidenceWeightedMedianUSDPer1K is the governed price selector.
//
// Sorting by price and walking the cumulative weight to the halfway point gives
// the observation a majority of the evidence sits at or below. Unlike min() a
// single outlier cannot move it, and unlike a plain median a blog post does not
// count the same as a vendor's own price sheet.
func confidenceWeightedMedianUSDPer1K(className string, class priceBoardClass) (
	price float64, chosen normalisedObservation, err error,
) {
	obs := normaliseObservations(class)
	var total float64
	for _, o := range obs {
		total += o.Confidence
	}
	if len(obs) < minBoardObservations || total < minBoardTotalConfidence {
		return 0, normalisedObservation{}, errBoardUngoverned{
			Class: className, Count: len(obs), Confidence: total,
		}
	}
	sort.Slice(obs, func(i, j int) bool { return obs[i].USDPer1K < obs[j].USDPer1K })

	half := total / 2
	var run float64
	for _, o := range obs {
		run += o.Confidence
		if run >= half {
			return o.USDPer1K, o, nil
		}
	}
	last := obs[len(obs)-1]
	return last.USDPer1K, last, nil
}

// contributionMargins reports what a price actually leaves for each side, per
// supplier-hour, using the same measured throughput and power figures the
// viability report uses.
type contributionMargins struct {
	SupplierGrossUSDHr float64
	ElectricityUSDHr   float64
	SupplierNetUSDHr   float64
	CXGrossUSDHr       float64
}

func (m contributionMargins) supplierNegative() bool { return m.SupplierNetUSDHr < 0 }
func (m contributionMargins) cxNegative() bool       { return m.CXGrossUSDHr < 0 }

// errNegativeContribution refuses a public price that cannot pay its own supply
// side. Publishing such a price is a promise the platform cannot keep: the
// supplier pays for electricity out of pocket on every job they accept.
type errNegativeContribution struct {
	ModelID string
	Price   float64
	Margins contributionMargins
}

func (e errNegativeContribution) Error() string {
	return fmt.Sprintf(
		"refusing to publish %s at $%.8f/1k: supplier net $%.6f/hr (gross $%.6f - electricity $%.6f), "+
			"CX gross $%.6f/hr; catalogue publication cannot be subsidised",
		e.ModelID, e.Price, e.Margins.SupplierNetUSDHr, e.Margins.SupplierGrossUSDHr,
		e.Margins.ElectricityUSDHr, e.Margins.CXGrossUSDHr)
}

// marginsForPrice computes what a candidate catalogue price leaves each side,
// reusing the same measured throughput and sustained-power figures the
// viability report uses so the two can never disagree.
func marginsForPrice(b measuredThroughput, pricePer1K, supplierShare float64) contributionMargins {
	watts := sustainedWattsByHWClass[b.HWClass]
	if watts <= 0 {
		watts = 30.0
	}
	elec := watts / 1000.0 * defaultElectricityUSDPerKWh
	unitsPerHr := b.UnitsPerSec * 3600.0
	revenue := unitsPerHr / 1000.0 * pricePer1K
	gross := revenue * supplierShare
	return contributionMargins{
		SupplierGrossUSDHr: gross,
		ElectricityUSDHr:   elec,
		SupplierNetUSDHr:   gross - elec,
		CXGrossUSDHr:       revenue - gross,
	}
}

// governPublishedPrice is the gate for item 13: a public price whose supplier
// or platform contribution is negative is refused. A process environment
// string is not durable treasury authority, so there is deliberately no bypass
// here. Explicit subsidy funds may satisfy already-created supplier liabilities
// through the separately audited payout path; they cannot publish an
// unsustainable catalogue promise.
//
// Returns the reason it was blocked, or "" when the price may be published.
func governPublishedPrice(b measuredThroughput, pricePer1K, supplierShare float64) error {
	m := marginsForPrice(b, pricePer1K, supplierShare)
	if !m.supplierNegative() && !m.cxNegative() {
		return nil
	}
	return errNegativeContribution{ModelID: b.ModelID, Price: pricePer1K, Margins: m}
}
