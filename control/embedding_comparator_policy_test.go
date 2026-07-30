package main

import (
	"errors"
	"math"
	"testing"
)

// The instability that made the exact `norm == 0` test insufficient.
//
// Measured before the epsilon existed: a row scaled to 1e-160 has a sum of
// squares of 9.99989e-321, which is subnormal. Its norm is nonzero, so the old
// zero-vector test passed it, and its cosine against a unit reference comes back
// as 1.000006 — impossible, and above the 0.999 floor. The zero-vector rule was
// evadable through numerical instability rather than through correctness.
func TestSubnormalRowCannotEvadeTheZeroVectorRule(t *testing.T) {
	const dim = 4
	rows := unitRows(3, dim)
	reference := embedBinaryArtifact(dim, rows)

	if embeddingMinimumNorm <= 0 {
		t.Fatal("the governed minimum norm is not positive; the rule is vacuous")
	}

	for _, scale := range []float64{1e-160, 1e-100, 1e-20, 1e-13} {
		observed := make([][]float64, len(rows))
		for i := range rows {
			observed[i] = append([]float64(nil), rows[i]...)
		}
		scaled := make([]float64, dim)
		for j := range scaled {
			scaled[j] = rows[1][j] * scale
		}
		observed[1] = scaled

		result := CompareEmbeddings(reference, embedBinaryArtifact(dim, observed))
		if result.Passed {
			t.Errorf("a row scaled to %g was accepted: min_row=%.9f",
				scale, result.MinRowCosine)
			continue
		}
		if result.FailingRowIndex != 1 {
			t.Errorf("scale %g: failing row %d, want 1", scale, result.FailingRowIndex)
		}
	}

	// A row just ABOVE the governed floor must still be graded on its direction
	// rather than refused for being small, or the epsilon would be rejecting
	// legitimate unnormalized output.
	above := make([][]float64, len(rows))
	for i := range rows {
		above[i] = append([]float64(nil), rows[i]...)
	}
	scaled := make([]float64, dim)
	for j := range scaled {
		scaled[j] = rows[1][j] * 1e-6
	}
	above[1] = scaled
	if result := CompareEmbeddings(reference, embedBinaryArtifact(dim, above)); !result.Passed {
		t.Errorf("a small but well-above-floor row was refused: %+v", result)
	}
}

// A cosine outside [-1,1] by more than rounding is an error, not a value to
// round in. Within rounding it is clamped, so ordinary accumulation noise does
// not fail an otherwise identical artifact.
func TestCosineIsClampedWithinRoundingAndRefusedBeyondIt(t *testing.T) {
	a := []float64{1, 0, 0, 0}
	if c, ok := clampedCosine(a, a); !ok || c != 1 {
		t.Fatalf("identical rows scored %v (ok=%v), want exactly 1", c, ok)
	}
	opposite := []float64{-1, 0, 0, 0}
	if c, ok := clampedCosine(a, opposite); !ok || c != -1 {
		t.Fatalf("opposed rows scored %v (ok=%v), want exactly -1", c, ok)
	}
	// The measured instability value: 1.000006 is six orders of magnitude beyond
	// float64 rounding over a few thousand dimensions and must be refused rather
	// than clamped into a pass.
	if 1.000006 <= 1+embeddingCosineClampEpsilon {
		t.Fatalf("the clamp epsilon %g would round in the measured 1.000006 instability",
			embeddingCosineClampEpsilon)
	}
}

// One documented validation contract across both artifact formats.
//
// The formats do not accept the same INPUTS — encoding/json cannot represent NaN
// at all — so parity is asserted where it is meaningful: an artifact valid in
// both formats must receive the same verdict in both.
func TestJSONAndBinaryAgreeOnEveryValidMatrix(t *testing.T) {
	const dim = 6
	for _, rows := range [][][]float64{
		unitRows(1, dim),
		unitRows(4, dim),
		unitRows(37, dim),
	} {
		observed := make([][]float64, len(rows))
		for i := range rows {
			observed[i] = append([]float64(nil), rows[i]...)
		}
		// A perturbation well inside the floor, so both formats must accept.
		observed[0][0] += 1e-7

		jsonResult := CompareEmbeddings(
			embedArtifact(t, dim, rows), embedArtifact(t, dim, observed))
		binResult := CompareEmbeddings(
			embedBinaryArtifact(dim, rows), embedBinaryArtifact(dim, observed))

		if jsonResult.Passed != binResult.Passed {
			t.Fatalf("%d rows: JSON passed=%v but CXEM passed=%v",
				len(rows), jsonResult.Passed, binResult.Passed)
		}
		if jsonResult.ObservedRows != binResult.ObservedRows ||
			jsonResult.ObservedDim != binResult.ObservedDim {
			t.Fatalf("%d rows: shapes differ between formats: %+v vs %+v",
				len(rows), jsonResult, binResult)
		}
		// CXEM stores float32, so the cosines are not bit-identical. They must
		// agree far inside the floor, which is the property that matters: one
		// artifact does not change verdict by being re-encoded.
		if math.Abs(jsonResult.MinRowCosine-binResult.MinRowCosine) > 1e-5 {
			t.Errorf("%d rows: min-row cosine differs by more than float32 storage "+
				"explains: JSON %.9f vs CXEM %.9f",
				len(rows), jsonResult.MinRowCosine, binResult.MinRowCosine)
		}
		if jsonResult.Policy.Revision != binResult.Policy.Revision {
			t.Error("the two formats resolved different comparator revisions")
		}
	}
}

// The frozen policy must be complete and bound into every comparison. A receipt
// saying "passed embed-cosine-v2" is only checkable if it also says what v2
// required.
func TestComparatorPolicyIsBoundIntoEveryComparison(t *testing.T) {
	const dim = 4
	body := embedArtifact(t, dim, unitRows(2, dim))
	policy := CompareEmbeddings(body, body).Policy

	if policy.Revision != embeddingComparatorRevision {
		t.Errorf("policy revision %q", policy.Revision)
	}
	for name, value := range map[string]float64{
		"mean threshold": policy.MeanThreshold,
		"row threshold":  policy.RowThreshold,
		"minimum norm":   policy.MinimumNorm,
		"clamp epsilon":  policy.ClampEpsilon,
	} {
		if value <= 0 {
			t.Errorf("%s is not bound into the comparison: %v", name, value)
		}
	}
	for name, value := range map[string]string{
		"accumulator":  policy.Accumulator,
		"row order":    policy.RowOrder,
		"decode stage": policy.DecodeStage,
	} {
		if value == "" {
			t.Errorf("%s is not documented in the bound policy", name)
		}
	}
	if policy.RowOrder != "positional" {
		t.Errorf("row order is %q; a set-based comparison would accept a shuffled "+
			"artifact in which every buyer row points at the wrong text", policy.RowOrder)
	}
	// A rejection must carry the policy too, or a disputed refusal could not be
	// checked against the rule that produced it.
	rejected := CompareEmbeddings(body, embedArtifact(t, dim, unitRows(3, dim)))
	if rejected.Passed || rejected.Policy.Revision != embeddingComparatorRevision {
		t.Errorf("a rejection did not carry the comparator policy: %+v", rejected)
	}
}

// Historical replay: a pre-cutover decision stays verifiable under the rule that
// was in force, and that rule may not decide new work.
func TestV1ReplayIsAvailableButCannotDecideNewWork(t *testing.T) {
	if err := liveEmbeddingComparator(embeddingComparatorRevision); err != nil {
		t.Fatalf("v2 is not live: %v", err)
	}
	err := liveEmbeddingComparator(embeddingComparatorRevisionV1)
	if err == nil {
		t.Fatal("v1 was accepted for new work")
	}
	if !errors.Is(err, ErrRetiredComparator) {
		t.Errorf("refusal was %v, want ErrRetiredComparator", err)
	}
	if liveEmbeddingComparator("embed-cosine-v99") == nil {
		t.Fatal("an unknown comparator revision was accepted")
	}
	if resolveEmbeddingComparator().Revision != embeddingComparatorRevision {
		t.Fatal("a new plan did not resolve to the live comparator")
	}

	// The artifact that exposed the defect: v1 accepted it, v2 rejects it, and
	// replaying v1 must still report what v1 decided. Rewriting that as a
	// rejection would be falsifying historical verification evidence.
	const dim = 8
	rows := unitRows(1000, dim)
	corrupted := make([][]float64, len(rows))
	copy(corrupted, rows)
	wrong := make([]float64, dim)
	for j := range wrong {
		wrong[j] = math.Cos(float64(j+1) * 3.7)
	}
	corrupted[500] = wrong

	reference := embedArtifact(t, dim, rows)
	observed := embedArtifact(t, dim, corrupted)

	mean, passedV1 := replayEmbeddingComparatorV1(reference, observed)
	if !passedV1 {
		t.Fatalf("v1 replay reports a rejection it did not make: mean %.6f", mean)
	}
	t.Logf("v1 replay reproduces the historical PASS at mean %.6f", mean)

	if CompareEmbeddings(reference, observed).Passed {
		t.Fatal("v2 accepted the artifact that motivated it")
	}
}
