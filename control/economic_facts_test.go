package main

import (
	"testing"

	"github.com/google/uuid"
)

func TestBatchFeeAllocationConservesAndRejectsInvalidWeights(t *testing.T) {
	for _, tc := range []struct {
		fee     int64
		weights []int64
		valid   bool
	}{{101, []int64{1, 1, 1}, true}, {0, []int64{3}, true}, {-1, []int64{1}, false}, {1, nil, false}, {1, []int64{0}, false}} {
		weights := make([]batchFeeWeight, len(tc.weights))
		for i, weight := range tc.weights {
			weights[i] = batchFeeWeight{JobID: uuid.New(), WeightMicros: weight}
		}
		allocations, err := allocateBatchFeeMicros(tc.fee, weights)
		if (err == nil) != tc.valid {
			t.Fatalf("fee=%d weights=%v err=%v", tc.fee, tc.weights, err)
		}
		var sum int64
		for _, allocation := range allocations {
			sum += allocation.AllocatedMicros
		}
		if err == nil && sum != tc.fee {
			t.Fatalf("fee=%d allocation sum=%d", tc.fee, sum)
		}
	}
}
