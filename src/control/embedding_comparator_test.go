package main

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// embedBinaryArtifact builds the compact CXEM float32 artifact the agent emits
// when a job asks for binary output.
//
// It exists in these tests because the binary envelope parser validates SHAPE
// ONLY — magic, version, dimension, count, byte length — and performs no finite
// or nonzero check. The JSON parser does both. So a zero vector, a NaN or an Inf
// cannot reach the comparator as JSON and CAN reach it as binary, which makes
// the comparator's own value checks load-bearing rather than defence in depth.
func embedBinaryArtifact(dim int, vectors [][]float64) []byte {
	out := make([]byte, 0, 16+len(vectors)*dim*4)
	out = append(out, 'C', 'X', 'E', 'M')
	out = binary.LittleEndian.AppendUint32(out, 1)
	out = binary.LittleEndian.AppendUint32(out, uint32(dim))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(vectors)))
	for _, row := range vectors {
		for _, v := range row {
			out = binary.LittleEndian.AppendUint32(out, math.Float32bits(float32(v)))
		}
	}
	return out
}

// embedArtifact builds the JSON shape the agent commits.
func embedArtifact(t *testing.T, dim int, vectors [][]float64) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"job_type": "embed",
		"model":    "all-minilm-l6-v2",
		"dim":      dim,
		"count":    len(vectors),
		"vectors":  vectors,
	})
	must(t, err)
	return body
}

// unitRows builds n distinct, normalized rows of the given dimension.
func unitRows(n, dim int) [][]float64 {
	rows := make([][]float64, n)
	for i := range rows {
		row := make([]float64, dim)
		// A distinct direction per row, so a swap is detectable.
		for j := range row {
			row[j] = math.Sin(float64(i+1) * float64(j+1))
		}
		var norm float64
		for _, v := range row {
			norm += v * v
		}
		norm = math.Sqrt(norm)
		for j := range row {
			row[j] /= norm
		}
		rows[i] = row
	}
	return rows
}

// The defect the per-row floor exists for.
//
// 999 correct rows and one row that is well-formed but WRONG: right width,
// finite, nonzero, pointing somewhere else entirely. The JSON parser accepts it
// — it validates shape and values, not meaning — so it reached the old rule,
// where 999 rows at 1.0 and one at ~0.0 averaged to 0.999 and passed the 0.999
// threshold exactly. The buyer would have received one embedding for the wrong
// text, indistinguishable in the artifact, with a receipt saying it verified.
//
// A zero row would NOT demonstrate this: parseEmbeddingJSONVectors already
// rejects zero vectors, NaN and Inf on the JSON path. Using one here would have
// produced a passing test that proved the parser worked and said nothing about
// the mean. The binary path is where those values actually get through, and it
// has its own case below.
func TestOneWrongRowCannotHideBehindTheMean(t *testing.T) {
	const dim = 8
	rows := unitRows(1000, dim)
	reference := embedArtifact(t, dim, rows)

	corrupted := make([][]float64, len(rows))
	copy(corrupted, rows)
	// A different, entirely valid unit vector: orthogonal-ish to the original.
	wrong := make([]float64, dim)
	for j := range wrong {
		wrong[j] = math.Cos(float64(j+1) * 3.7)
	}
	corrupted[500] = wrong
	observed := embedArtifact(t, dim, corrupted)

	// The old rule's arithmetic, reconstructed so the regression is visible
	// rather than asserted.
	oldMean := 0.0
	for i := range rows {
		oldMean += cosine(rows[i], corrupted[i])
	}
	oldMean /= float64(len(rows))
	if oldMean < embeddingMeanCosineThreshold {
		t.Fatalf("fixture does not reproduce the defect: mean %.6f already below %.3f",
			oldMean, embeddingMeanCosineThreshold)
	}
	t.Logf("old mean-only rule would have scored %.6f and PASSED", oldMean)

	result := CompareEmbeddings(reference, observed)
	if result.Passed {
		t.Fatal("a wrong row hidden among 999 correct ones was accepted")
	}
	if result.FailingRowIndex != 500 {
		t.Errorf("failing row reported as %d, want 500", result.FailingRowIndex)
	}
	if !strings.Contains(result.RejectionReason, "per-row floor") {
		t.Errorf("rejection reason %q does not name the per-row floor", result.RejectionReason)
	}
	if result.ComparatorRevision != embeddingComparatorRevision {
		t.Errorf("comparison recorded revision %q", result.ComparatorRevision)
	}
}

// Values the JSON parser rejects but the BINARY parser lets through.
//
// The binary envelope check validates magic, version, dimension, count and byte
// length, and nothing about the numbers. Without the comparator's own checks a
// NaN or a zero row in a CXEM artifact would reach the cosine arithmetic, where
// a zero row scores 0.0 and averages away and a NaN poisons the total.
func TestBinaryArtifactValueChecksAreLoadBearing(t *testing.T) {
	const dim = 4
	rows := unitRows(4, dim)
	reference := embedBinaryArtifact(dim, rows)

	// Sanity: the binary parser accepts these values, so the comparator is the
	// only thing standing between them and a settlement decision.
	for name, mutate := range map[string]func([][]float64) [][]float64{
		"zero vector": func(v [][]float64) [][]float64 {
			v[1] = make([]float64, dim)
			return v
		},
		"NaN": func(v [][]float64) [][]float64 {
			v[2] = append([]float64(nil), v[2]...)
			v[2][0] = math.NaN()
			return v
		},
		"Inf": func(v [][]float64) [][]float64 {
			v[3] = append([]float64(nil), v[3]...)
			v[3][1] = math.Inf(1)
			return v
		},
	} {
		t.Run(name, func(t *testing.T) {
			copied := make([][]float64, len(rows))
			for i := range rows {
				copied[i] = append([]float64(nil), rows[i]...)
			}
			observed := embedBinaryArtifact(dim, mutate(copied))

			// It decodes: the parser has no opinion about these values.
			if _, ok := parseEmbeddingVectors(observed); !ok {
				t.Skipf("the binary parser now rejects %s upstream; "+
					"this test is obsolete and the comparator check is redundant", name)
			}
			result := CompareEmbeddings(reference, observed)
			if result.Passed {
				t.Fatalf("%s reached settlement authority through the binary path: %+v",
					name, result)
			}
			if result.FailingRowIndex < 0 {
				t.Errorf("%s was rejected without naming a row: %+v", name, result)
			}
		})
	}
}

// The real cross-cell artifacts must still pass, or the tightened contract has
// broken the thing it was meant to govern.
func TestRealCrossCellArtifactsPassTheGovernedComparator(t *testing.T) {
	candle := loadChainArtifact(t, "candle_metal", candleEmbedCell, "hf")
	llama := loadChainArtifact(t, "llama_cpp_metal", llamaEmbedCell, "gguf")

	result := CompareEmbeddings(candle.body, llama.body)
	if !result.Passed {
		t.Fatalf("real llama.cpp output failed the governed comparator: %+v", result)
	}
	t.Logf("mean=%.6f min_row=%.6f rows=%d dim=%d revision=%s",
		result.MeanCosine, result.MinRowCosine, result.ObservedRows,
		result.ObservedDim, result.ComparatorRevision)
	if result.MinRowCosine < result.MeanCosine-1e-9 && result.MinRowCosine < embeddingRowCosineThreshold {
		t.Fatal("the minimum row cosine is below the floor but the comparison passed")
	}
	// Both statistics are recorded, not just the aggregate.
	if result.MeanCosine <= 0 || math.IsInf(result.MinRowCosine, 1) {
		t.Fatalf("comparison did not record both statistics: %+v", result)
	}
	if result.ReferenceSHA256 != candle.sha256 || result.ObservedSHA256 != llama.sha256 {
		t.Fatal("the comparison did not bind the digests of what it compared")
	}
}

func TestGovernedComparatorFailureMatrix(t *testing.T) {
	const dim = 6
	rows := unitRows(5, dim)
	reference := embedArtifact(t, dim, rows)

	mutate := func(f func([][]float64) [][]float64) []byte {
		copied := make([][]float64, len(rows))
		for i := range rows {
			copied[i] = append([]float64(nil), rows[i]...)
		}
		return embedArtifact(t, dim, f(copied))
	}

	for _, tc := range []struct {
		name, wantReason string
		wantRow          int
		observed         []byte
	}{
		{
			name: "swapped rows", wantReason: "below the per-row floor", wantRow: 0,
			// Row identity is positional. Swapping two rows returns every correct
			// vector attached to the wrong input, which is a wrong answer that a
			// set-based check would call correct.
			observed: mutate(func(v [][]float64) [][]float64 {
				v[0], v[1] = v[1], v[0]
				return v
			}),
		},
		{
			name: "duplicate row", wantReason: "below the per-row floor", wantRow: 1,
			observed: mutate(func(v [][]float64) [][]float64 {
				v[1] = append([]float64(nil), v[0]...)
				return v
			}),
		},
		{
			name: "missing row", wantReason: "row count", wantRow: -1,
			observed: mutate(func(v [][]float64) [][]float64 { return v[:len(v)-1] }),
		},
		{
			name: "extra row", wantReason: "row count", wantRow: -1,
			observed: mutate(func(v [][]float64) [][]float64 {
				return append(v, append([]float64(nil), v[0]...))
			}),
		},
		{
			// Rejected by the JSON parser before the comparator sees it, which is
			// correct and worth pinning: the message differs from the comparator's
			// own, and the artifact is refused either way.
			name: "zero vector", wantReason: "does not decode", wantRow: -1,
			observed: mutate(func(v [][]float64) [][]float64 {
				v[2] = make([]float64, dim)
				return v
			}),
		},
		// NaN and Inf are absent here on purpose: encoding/json cannot marshal
		// them, so they cannot exist in a JSON artifact at all, and the JSON
		// parser rejects them anyway. They are exercised through the binary path
		// in TestBinaryArtifactValueChecksAreLoadBearing, which is the only
		// format that can actually carry them.
		{
			// A WELL-FORMED artifact of a different dimension: it declares dim 3
			// and carries 3-wide rows, so it decodes cleanly on its own terms.
			// Only a comparison against the governed reference can see that the
			// width is wrong, which is exactly the check the parser cannot make.
			name: "wrong dimensions", wantReason: "dimensions", wantRow: 0,
			observed: embedArtifact(t, 3, [][]float64{
				{1, 0, 0}, {0, 1, 0}, {0, 0, 1}, {1, 1, 0}, {0, 1, 1},
			}),
		},
		{
			// Also caught upstream by the JSON parser's per-row width check.
			name: "ragged rows", wantReason: "does not decode", wantRow: -1,
			observed: mutate(func(v [][]float64) [][]float64 {
				v[2] = v[2][:dim-1]
				return v
			}),
		},
		{
			name: "wrong corpus", wantReason: "below the per-row floor", wantRow: 0,
			// A well-formed artifact for entirely different inputs. Every
			// structural check passes; only the comparison catches it.
			observed: embedArtifact(t, dim, unitRows(5, dim)[:0:0]),
		},
		{
			name: "not an embedding artifact", wantReason: "does not decode",
			wantRow: -1, observed: []byte(`{"job_type":"batch_infer","completions":[]}`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := tc.observed
			if tc.name == "wrong corpus" {
				// Distinct directions from the reference: sin((i+7)*(j+3)).
				other := make([][]float64, len(rows))
				for i := range other {
					row := make([]float64, dim)
					for j := range row {
						row[j] = math.Sin(float64(i+7) * float64(j+3))
					}
					other[i] = row
				}
				observed = embedArtifact(t, dim, other)
			}
			result := CompareEmbeddings(reference, observed)
			if result.Passed {
				t.Fatalf("%s was accepted: %+v", tc.name, result)
			}
			if !strings.Contains(result.RejectionReason, tc.wantReason) {
				t.Errorf("rejection reason %q does not contain %q",
					result.RejectionReason, tc.wantReason)
			}
			if tc.wantRow >= 0 && result.FailingRowIndex != tc.wantRow {
				t.Errorf("failing row %d, want %d", result.FailingRowIndex, tc.wantRow)
			}
			if result.ComparatorRevision == "" {
				t.Error("a rejection recorded no comparator revision")
			}
		})
	}
}

// Identical artifacts are the trivial pass, and a comparator that failed here
// would be refusing correct work.
func TestGovernedComparatorAcceptsIdenticalArtifacts(t *testing.T) {
	const dim = 4
	body := embedArtifact(t, dim, unitRows(3, dim))
	result := CompareEmbeddings(body, body)
	if !result.Passed {
		t.Fatalf("identical artifacts were rejected: %+v", result)
	}
	if result.FailingRowIndex != -1 {
		t.Errorf("a passing comparison named a failing row %d", result.FailingRowIndex)
	}
	if math.Abs(result.MinRowCosine-1) > 1e-9 || math.Abs(result.MeanCosine-1) > 1e-9 {
		t.Errorf("identical artifacts scored mean=%.9f min=%.9f",
			result.MeanCosine, result.MinRowCosine)
	}
}

// The tightened embedding contract must not have touched byte_exact.
func TestByteExactContractIsUnchangedByTheEmbeddingPolicy(t *testing.T) {
	a := []byte(`{"job_type":"batch_infer","completions":[{"index":0,"text":"x","tokens":1}]}`)
	b := []byte(`{"job_type":"batch_infer","completions":[{"index":0,"text":"y","tokens":1}]}`)
	if !resultsAgree("batch_infer", a, a) {
		t.Fatal("byte_exact rejected identical bytes")
	}
	if resultsAgree("batch_infer", a, b) {
		t.Fatal("byte_exact accepted different bytes")
	}
}
