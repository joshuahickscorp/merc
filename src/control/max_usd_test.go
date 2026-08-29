package main

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCreateJobRejectsInvalidMaxUSDBeforeSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name string
		max  float64
	}{
		{name: "negative", max: -0.01},
		{name: "positive infinity", max: math.Inf(1)},
		{name: "negative infinity", max: math.Inf(-1)},
		{name: "not a number", max: math.NaN()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, herr := (&Server{}).createJob(context.Background(), uuid.New(), jobSubmit{MaxUSD: tc.max})
			if herr == nil || herr.status != http.StatusBadRequest ||
				!strings.Contains(herr.msg, "max_usd") {
				t.Fatalf("invalid max_usd=%v result=%v, want an early 400", tc.max, herr)
			}
		})
	}
}
