package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
)

// The governed embedding equivalence comparator.
//
// The rule it replaces was `meanCosine(a, b) >= 0.999`, and a mean hides exactly
// the failure this contract exists to catch. cosine() returns 0 for a zero-norm
// row rather than failing, so 999 perfect rows and one zeroed row average to
// 0.999000 and PASS at the 0.999 threshold. The larger the batch, the more
// corruption one row can carry: at 10,000 rows a completely destroyed row moves
// the mean by 0.0001. A buyer receiving that artifact has one useless embedding
// among thousands and a receipt saying it was verified.
//
// So equivalence is now structural first and statistical second. Every check
// below can fail on its own, and the per-row floor is what makes a localized
// error unhideable.
//
// Byte-exact is untouched. These are two different contracts sold at two
// different quality tiers, and the embedding policy must never become a way to
// get byte-exact work graded loosely.

// embeddingComparatorRevision names this comparator in receipts.
//
// A threshold or rule change takes a new revision rather than editing this one,
// because a receipt that recorded "passed under embed-cosine-v2" must keep
// meaning what it said. v1 was the bare mean.
const embeddingComparatorRevision = "embed-cosine-v2"

const (
	// embeddingMeanCosineThreshold is the aggregate floor, carried forward from
	// v1 so a previously-passing artifact does not start failing on aggregate.
	embeddingMeanCosineThreshold = 0.999
	// embeddingRowCosineThreshold is the floor EVERY row must clear on its own.
	// Set equal to the mean floor deliberately: the tier being sold is numerical
	// equivalence, and a row that is not equivalent is not equivalent regardless
	// of how many of its neighbours are. Measured cross-cell agreement is
	// 0.999998 minimum, which clears this by three orders of magnitude.
	embeddingRowCosineThreshold = 0.999
	// embeddingMinimumNorm is the governed floor on a row's Euclidean norm, and
	// it exists because `norm == 0` is not the same test as "not a zero vector".
	//
	// Measured: a row scaled to 1e-160 has a sum of squares of 9.99989e-321,
	// which is SUBNORMAL. Precision collapses there, and the cosine against a
	// unit reference comes back as 1.000006 — mathematically impossible, and
	// comfortably above the 0.999 floor. So a supplier could emit a row that is
	// numerically indistinguishable from zero and have it graded a perfect match,
	// evading the zero-vector rule through instability rather than through
	// correctness.
	//
	// 1e-12 sits far above the subnormal band where that happens and far below
	// any real embedding: a normalized row has norm 1.0, and even an
	// unnormalized MiniLM row is order 1. Nothing legitimate is near this.
	embeddingMinimumNorm = 1e-12
	// embeddingCosineClampEpsilon bounds how far outside [-1,1] a computed
	// cosine may drift before it is treated as an error rather than rounded in.
	// Ordinary float64 accumulation over a few thousand dimensions lands within
	// a few ULPs; 1e-6 is generous for that and still catches the 1.000006 case
	// above, which is six orders of magnitude worse than rounding.
	embeddingCosineClampEpsilon = 1e-6
)

// embeddingComparatorPolicy is the frozen parameter set a receipt binds.
//
// Recorded as data rather than left implicit in constants, because a receipt
// that says "passed embed-cosine-v2" is only checkable if the reader can see
// what v2 required. Changing any value here takes a new revision.
type embeddingComparatorPolicy struct {
	Revision string `json:"revision"`
	// MeanThreshold and RowThreshold are the two cosine floors.
	MeanThreshold float64 `json:"mean_threshold"`
	RowThreshold  float64 `json:"row_threshold"`
	// MinimumNorm is the governed epsilon below which a row is a zero vector,
	// however it was spelled.
	MinimumNorm float64 `json:"minimum_norm"`
	// ClampEpsilon is how far outside [-1,1] a cosine may drift before it is an
	// error. Within it, the value is clamped.
	ClampEpsilon float64 `json:"clamp_epsilon"`
	// Accumulator names the arithmetic the mean is summed in. float64 over
	// float64 row cosines: stated so a future change to compensated summation is
	// a revision rather than a silent difference in the fifth decimal.
	Accumulator string `json:"accumulator"`
	// RowOrder is positional: row i of the observed artifact is compared against
	// row i of the reference, and no reordering is attempted. Embeddings are
	// returned in input order, so a set-based comparison would call a shuffled
	// artifact correct while every buyer row pointed at the wrong text.
	RowOrder string `json:"row_order"`
	// DecodeStage documents where each class of failure is caught, so the stage
	// and reason are deterministic rather than incidental. JSON and CXEM do not
	// accept the same inputs: encoding/json cannot represent NaN or Inf at all,
	// and the JSON vector parser additionally rejects zero rows and ragged
	// widths, while the CXEM envelope check validates magic, version, dimension,
	// count and byte length ONLY. A zero row therefore fails at decode as JSON
	// and at comparison as CXEM. Both are refusals; the receipt records which.
	DecodeStage string `json:"decode_stage"`
}

func embeddingPolicyV2() embeddingComparatorPolicy {
	return embeddingComparatorPolicy{
		Revision:      embeddingComparatorRevision,
		MeanThreshold: embeddingMeanCosineThreshold,
		RowThreshold:  embeddingRowCosineThreshold,
		MinimumNorm:   embeddingMinimumNorm,
		ClampEpsilon:  embeddingCosineClampEpsilon,
		Accumulator:   "float64",
		RowOrder:      "positional",
		DecodeStage: "malformed bytes fail at decode; semantically invalid vectors fail at " +
			"comparison; JSON additionally rejects zero rows, ragged widths and " +
			"non-finite values at decode because CXEM cannot",
	}
}

// clampedCosine returns the cosine of two rows, refusing a value that has
// drifted implausibly far outside [-1,1] and rounding one that has not.
//
// Without this a subnormal-norm row scores 1.000006 and passes every floor. The
// minimum-norm check below catches that case first; this is the second line, for
// any other route to an out-of-range value.
func clampedCosine(a, b []float64) (float64, bool) {
	c := cosine(a, b)
	if math.IsNaN(c) {
		return 0, false
	}
	if c > 1 {
		if c > 1+embeddingCosineClampEpsilon {
			return c, false
		}
		return 1, true
	}
	if c < -1 {
		if c < -1-embeddingCosineClampEpsilon {
			return c, false
		}
		return -1, true
	}
	return c, true
}

// EmbeddingComparison is the full record of one equivalence decision.
//
// Every field is persisted rather than reduced to a boolean, because "it failed"
// is not a diagnosis. FailingRowIndex in particular is what turns a rejection
// into something a supplier can act on.
type EmbeddingComparison struct {
	ComparatorRevision string  `json:"comparator_revision"`
	Passed             bool    `json:"passed"`
	ExpectedRows       int     `json:"expected_rows"`
	ObservedRows       int     `json:"observed_rows"`
	ExpectedDim        int     `json:"expected_dim"`
	ObservedDim        int     `json:"observed_dim"`
	MeanCosine         float64 `json:"mean_cosine"`
	MinRowCosine       float64 `json:"minimum_row_cosine"`
	// FailingRowIndex is the first row that failed, or -1 when none did.
	FailingRowIndex int    `json:"failing_row_index"`
	RejectionReason string `json:"rejection_reason,omitempty"`
	// The digests of what was actually compared, so a receipt names the bytes
	// rather than the intent. ReferenceSHA256 is the governed answer; a receipt
	// that omitted it could not distinguish "matched the approved reference" from
	// "matched something".
	ReferenceSHA256 string  `json:"reference_sha256"`
	ObservedSHA256  string  `json:"observed_sha256"`
	MeanThreshold   float64 `json:"mean_threshold"`
	RowThreshold    float64 `json:"row_threshold"`
	// Policy is the complete frozen parameter set, so a receipt is checkable
	// without the reader having to know which constants were in force.
	Policy embeddingComparatorPolicy `json:"policy"`
}

// CompareEmbeddings grades an observed artifact against a governed reference.
//
// `reference` is the approved answer and `observed` is the candidate. The
// direction matters for the row/dimension counts reported: expected comes from
// the reference, observed from the candidate, and a receipt that swapped them
// would blame the wrong side.
func CompareEmbeddings(reference, observed []byte) EmbeddingComparison {
	refSum, obsSum := sha256.Sum256(reference), sha256.Sum256(observed)
	out := EmbeddingComparison{
		ComparatorRevision: embeddingComparatorRevision,
		FailingRowIndex:    -1,
		ReferenceSHA256:    hex.EncodeToString(refSum[:]),
		ObservedSHA256:     hex.EncodeToString(obsSum[:]),
		MeanThreshold:      embeddingMeanCosineThreshold,
		RowThreshold:       embeddingRowCosineThreshold,
		Policy:             embeddingPolicyV2(),
	}

	refRows, okRef := parseEmbeddingVectors(reference)
	obsRows, okObs := parseEmbeddingVectors(observed)
	if !okRef {
		out.RejectionReason = "governed reference artifact does not decode as embeddings"
		return out
	}
	if !okObs {
		out.RejectionReason = "observed artifact does not decode as embeddings"
		return out
	}

	out.ExpectedRows, out.ObservedRows = len(refRows), len(obsRows)
	if len(refRows) > 0 {
		out.ExpectedDim = len(refRows[0])
	}
	if len(obsRows) > 0 {
		out.ObservedDim = len(obsRows[0])
	}

	if out.ExpectedRows == 0 {
		out.RejectionReason = "governed reference contains no rows"
		return out
	}
	// Exact cardinality. A short artifact is not a partially correct one: the
	// buyer asked for N embeddings and rows are positional, so a missing row
	// silently re-indexes every row after it.
	if out.ObservedRows != out.ExpectedRows {
		out.RejectionReason = fmt.Sprintf(
			"row count %d does not match the governed reference's %d",
			out.ObservedRows, out.ExpectedRows)
		return out
	}

	sum := 0.0
	out.MinRowCosine = math.Inf(1)
	for i := range refRows {
		// Dimensions are checked per row, not once from row zero. A ragged
		// artifact whose first row is the right width would otherwise pass the
		// shape check and fail somewhere less legible.
		if len(refRows[i]) != out.ExpectedDim {
			out.FailingRowIndex = i
			out.RejectionReason = fmt.Sprintf(
				"governed reference row %d has %d dimensions, want %d",
				i, len(refRows[i]), out.ExpectedDim)
			return out
		}
		if len(obsRows[i]) != out.ExpectedDim {
			out.FailingRowIndex = i
			out.ObservedDim = len(obsRows[i])
			out.RejectionReason = fmt.Sprintf(
				"row %d has %d dimensions, want %d", i, len(obsRows[i]), out.ExpectedDim)
			return out
		}

		var refNorm, obsNorm float64
		for j := range obsRows[i] {
			// Finite values only, on both sides. NaN would poison every
			// comparison downstream, and Inf makes a norm meaningless.
			if math.IsNaN(obsRows[i][j]) || math.IsInf(obsRows[i][j], 0) {
				out.FailingRowIndex = i
				out.RejectionReason = fmt.Sprintf(
					"row %d contains a non-finite value at index %d", i, j)
				return out
			}
			if math.IsNaN(refRows[i][j]) || math.IsInf(refRows[i][j], 0) {
				out.FailingRowIndex = i
				out.RejectionReason = fmt.Sprintf(
					"governed reference row %d contains a non-finite value", i)
				return out
			}
			refNorm += refRows[i][j] * refRows[i][j]
			obsNorm += obsRows[i][j] * obsRows[i][j]
		}
		// The governed epsilon, not `== 0`. A row scaled to 1e-160 has a
		// subnormal sum of squares, a nonzero norm, and a cosine of 1.000006
		// against a unit reference — it would pass every floor while being
		// numerically indistinguishable from zero.
		if math.Sqrt(obsNorm) < embeddingMinimumNorm {
			out.FailingRowIndex = i
			out.RejectionReason = fmt.Sprintf(
				"row %d norm %.3g is below the governed minimum %.3g; it is a zero vector",
				i, math.Sqrt(obsNorm), embeddingMinimumNorm)
			return out
		}
		if math.Sqrt(refNorm) < embeddingMinimumNorm {
			out.FailingRowIndex = i
			out.RejectionReason = fmt.Sprintf(
				"governed reference row %d is a zero vector", i)
			return out
		}

		c, ok := clampedCosine(refRows[i], obsRows[i])
		if !ok {
			out.FailingRowIndex = i
			out.RejectionReason = fmt.Sprintf(
				"row %d cosine %.9f is undefined or implausibly outside [-1,1]", i, c)
			return out
		}
		sum += c
		if c < out.MinRowCosine {
			out.MinRowCosine = c
			if c < embeddingRowCosineThreshold {
				// Keep scanning is tempting, but the first failing row is the
				// one worth reporting and stopping keeps a 100k-row artifact
				// from costing a full pass to reject.
				out.FailingRowIndex = i
				out.MeanCosine = sum / float64(i+1)
				out.RejectionReason = fmt.Sprintf(
					"row %d cosine %.6f is below the per-row floor %.3f",
					i, c, embeddingRowCosineThreshold)
				return out
			}
		}
	}

	out.MeanCosine = sum / float64(len(refRows))
	if out.MeanCosine < embeddingMeanCosineThreshold {
		out.RejectionReason = fmt.Sprintf("mean cosine %.6f is below %.3f",
			out.MeanCosine, embeddingMeanCosineThreshold)
		return out
	}
	out.Passed = true
	return out
}

// ── historical replay ──────────────────────────────────────────────────────

// embeddingComparatorRevisionV1 is the mean-only rule v2 replaced.
//
// It exists for REPLAY ONLY. A verification decision recorded before the cutover
// was taken under this rule, and re-deriving it under v2 would report that a
// correctly-decided historical job now fails — which is not a finding about that
// job, it is a category error about which rule was in force.
//
// Nothing selects it for new work. resolveEmbeddingComparator refuses to hand it
// back for a live plan, so the only way to reach replayEmbeddingComparatorV1 is
// to explicitly name a stored revision.
const embeddingComparatorRevisionV1 = "embed-cosine-v1"

// ErrRetiredComparator is returned when live work asks for a retired revision.
var ErrRetiredComparator = errors.New("comparator revision is retired and may not decide new work")

// resolveEmbeddingComparator returns the comparator a NEW verification plan must
// use. It is deliberately not parameterised by anything a caller controls: after
// the cutover there is one live rule, and "which comparator should this job use"
// is not a question a job gets to answer.
func resolveEmbeddingComparator() embeddingComparatorPolicy { return embeddingPolicyV2() }

// liveEmbeddingComparator reports whether a revision may decide new work.
func liveEmbeddingComparator(revision string) error {
	switch revision {
	case embeddingComparatorRevision:
		return nil
	case embeddingComparatorRevisionV1:
		return fmt.Errorf("%w: %s was replaced by %s because a mean alone accepts one "+
			"materially wrong row among many", ErrRetiredComparator,
			embeddingComparatorRevisionV1, embeddingComparatorRevision)
	default:
		return fmt.Errorf("unknown comparator revision %q", revision)
	}
}

// replayEmbeddingComparatorV1 re-derives a pre-cutover decision under the rule
// that was actually in force when it was taken.
//
// This is the v1 algorithm verbatim: parse both sides, require equal non-zero row
// counts, average the per-row cosines, compare against the mean threshold. No
// per-row floor, no minimum norm, no clamp — because it did not have them, and a
// replay that added them would not be a replay.
func replayEmbeddingComparatorV1(reference, observed []byte) (mean float64, passed bool) {
	refRows, okRef := parseEmbeddingVectors(reference)
	obsRows, okObs := parseEmbeddingVectors(observed)
	if !okRef || !okObs || len(refRows) == 0 || len(refRows) != len(obsRows) {
		return 0, false
	}
	var total float64
	for i := range refRows {
		c := cosine(refRows[i], obsRows[i])
		if math.IsNaN(c) {
			return 0, false
		}
		total += c
	}
	mean = total / float64(len(refRows))
	return mean, mean >= embeddingMeanCosineThreshold
}
