package main

import "os"

// Shape-aware routing: send a task to the hardware whose measured strengths
// match the task's shape, rather than only to the hardware with the lowest
// hourly rate.
//
// The claim ORDER BY currently prefers the cheapest sufficient class, where
// "cheapest" means cost per HOUR. For batch work that is the wrong denominator.
// Measured on this deployment (evidence/perf/routing-crossover.json):
//
//	                     concurrency 1        concurrency 16
//	Metal M3 Ultra       TTFT   14.4 ms       211 tok/s, TTFT 5002 ms
//	CUDA RTX A5000       TTFT  181.0 ms      1617 tok/s, TTFT  313 ms
//
// Metal answers first-token 12.6x faster because it is local with no network
// hop, and then collapses under concurrency because llama.cpp has no continuous
// batching. CUDA holds latency flat and scales 11.7x.
//
// # A NOTE ON WHOSE COST THIS IS
//
// merc does not rent the hardware in the production supply model. Suppliers own
// their rigs and pay their own electricity; merc pays them per unit of accepted
// work. The $0.27/hr A5000 used to take the measurement above was RENTED TEST
// CAPACITY, not what merc's supply costs.
//
// So the hourly rate is the SUPPLIER's input, not merc's, and the quantity that
// decides routing is throughput per unit of whatever the supplier is paid. A
// class that sustains 1617 tok/s earns its owner far more per kilowatt-hour than
// one that sustains 211, which is why the same hardware that looks expensive by
// the hour can be the one both sides want for batch.
//
// The consequence for ordering is unchanged -- rank by fitness for the shape,
// not by an hourly figure merc does not pay -- but the reasoning must not be
// quoted as merc's own cost per token.
//
// # WHY THIS IS GATED OFF BY DEFAULT
//
// The crossover above is one measurement, taken on two DIFFERENT models at
// different precision (3B Q4_K_M on Metal, 1.5B fp16 on CUDA) with a single
// worker on each side. That is enough to establish the SHAPE of the difference,
// which is an engine and locality property, and not enough to justify inverting
// the cost ordering on a live money path. Turning this on changes which supplier
// gets paid.
//
// The evidence that would justify enabling it is specific: two workers of
// different classes connected simultaneously, serving identical weights at
// identical precision, with per-accepted-token cost measured on both. Until that
// exists, the mechanism ships and the behaviour does not.
//
//	MERC_SHAPE_AWARE_ROUTING=1
type shapePreference int

const (
	// shapeNoPreference leaves the existing cost-first ordering untouched.
	shapeNoPreference shapePreference = iota
	// shapePreferLowLatency favours locally attached hardware: interactive work
	// is dominated by time-to-first-token, where the network hop to a rented pod
	// costs more than the pod's extra throughput can return.
	shapePreferLowLatency
	// shapePreferThroughput favours hardware that scales under concurrency, where
	// cost per token beats cost per hour.
	shapePreferThroughput
)

// shapeAwareRoutingEnabled reports whether measured shape may influence claim
// ordering. Off unless explicitly enabled, because enabling it redirects money.
func shapeAwareRoutingEnabled() bool {
	return os.Getenv("MERC_SHAPE_AWARE_ROUTING") == "1"
}

// preferenceForTier maps a job's service tier to the hardware shape that
// measured better for it.
//
// 'batch' is throughput-bound: it has no interactive user waiting on first
// token, and it is the shape where Metal collapsed to a 5 second TTFT while
// CUDA held 313 ms. Everything else is treated as latency-bound, which is the
// conservative direction: mistakenly optimising a batch job for latency wastes
// some throughput, while mistakenly optimising an interactive request for
// throughput is visible to a user on every request.
func preferenceForTier(tier string) shapePreference {
	if !shapeAwareRoutingEnabled() {
		return shapeNoPreference
	}
	switch tier {
	case "batch":
		return shapePreferThroughput
	case "priority", "interactive", "trusted":
		return shapePreferLowLatency
	default:
		return shapePreferLowLatency
	}
}

// shapePreferenceName is the durable label a claim receipt can show. Empty when
// shape ordering did not apply (flag off).
func shapePreferenceName(pref shapePreference) string {
	switch pref {
	case shapePreferLowLatency:
		return "low_latency"
	case shapePreferThroughput:
		return "throughput"
	default:
		return ""
	}
}

// shapeOrderSQL is the claim ORDER BY term for hardware-shape fitness. It is
// always derived through preferenceForTier so MERC_SHAPE_AWARE_ROUTING is a
// live production path, not an inert helper.
//
// The term is per-job: batch work ranks throughput-strong classes first, every
// other tier ranks low-latency classes first. The claiming worker's hw_class is
// scored against the job's preferred shape so a worker prefers the work it is
// measured good at. When the flag is off both arms collapse to the no-op
// constant and the sort is unchanged.
//
// This is an ordering input only — never a WHERE filter — and must sit below
// cheaper_class_online / cheaper_ask_online so shape never promotes a more
// expensive cost class.
func shapeOrderSQL(hwCol, tierCol string) string {
	batch := shapeRankSQL(hwCol, preferenceForTier("batch"))
	// priority stands for every non-batch tier; preferenceForTier maps them
	// identically when the flag is on.
	latency := shapeRankSQL(hwCol, preferenceForTier("priority"))
	if batch == latency {
		return batch
	}
	return `(CASE WHEN ` + tierCol + ` = 'batch' THEN (` + batch + `) ELSE (` + latency + `) END)`
}

// shapeRankSQL orders hardware classes by fitness for a preference, cheapest
// rank first so it composes with the existing ASC ordering.
//
// Locally attached Apple Silicon wins latency; rented NVIDIA wins throughput. An
// unrecognised class sorts last under both, for the same reason it ranks last on
// cost: what has not been measured must not be preferred by default.
func shapeRankSQL(col string, pref shapePreference) string {
	switch pref {
	case shapePreferLowLatency:
		return `CASE
		          WHEN ` + col + ` LIKE 'apple_silicon%' THEN 0
		          WHEN ` + col + ` LIKE 'nvidia%'        THEN 1
		          ELSE 2
		        END`
	case shapePreferThroughput:
		return `CASE
		          WHEN ` + col + ` LIKE 'nvidia%'        THEN 0
		          WHEN ` + col + ` LIKE 'apple_silicon%' THEN 1
		          ELSE 2
		        END`
	default:
		// Cast, not a bare `0`. Postgres reads a bare integer literal in ORDER BY
		// as a COLUMN POSITION, so `ORDER BY ... 0 ...` fails with "ORDER BY
		// position 0 is not in select list" and every claim errors out. The cast
		// makes it an expression: a constant that sorts every row equally, so the
		// disabled path is a no-op term rather than a syntax error.
		return `0::int`
	}
}
