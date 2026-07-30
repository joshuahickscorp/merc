package main

import (
	"crypto/sha256"
	"encoding/hex"
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
)

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
		// A zero vector is the exact case the mean hid: cosine() reports 0 for
		// it rather than failing, so it used to be a free 0.0 contribution to the
		// average instead of a rejection.
		if obsNorm == 0 {
			out.FailingRowIndex = i
			out.RejectionReason = fmt.Sprintf("row %d is a zero vector", i)
			return out
		}
		if refNorm == 0 {
			out.FailingRowIndex = i
			out.RejectionReason = fmt.Sprintf("governed reference row %d is a zero vector", i)
			return out
		}

		c := cosine(refRows[i], obsRows[i])
		if math.IsNaN(c) {
			out.FailingRowIndex = i
			out.RejectionReason = fmt.Sprintf("row %d cosine is undefined", i)
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
