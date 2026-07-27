package main

import (
	"errors"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func validLoRAEval() loraEvaluation {
	const set = "aa11bb22cc33dd44ee55ff66aa11bb22cc33dd44ee55ff66aa11bb22cc33dd44"
	return loraEvaluation{
		RunID:               uuid.New(),
		TrainerSupplierID:   uuid.New(),
		EvaluatorSupplierID: uuid.New(),
		TrainerAccountID:    uuid.New(),
		EvaluatorAccountID:  uuid.New(),
		HeldOutSetSHA256:    set,
		ReservedSetSHA256:   set,
		BaselineScore:       0.700,
		CandidateScore:      0.770, // exactly +10%
		RequiredImprovement: 0.05,
	}
}

// A trainer scoring its own adapter is not a check, it is a claim -- and it is
// a claim attached to a payout.
func TestLoRAEvaluationMustBeIndependent(t *testing.T) {
	t.Run("same worker", func(t *testing.T) {
		eval := validLoRAEval()
		eval.EvaluatorSupplierID = eval.TrainerSupplierID
		if _, err := settleLoRARun(eval, 10.0); !errors.Is(err, errLoRANotIndependent) {
			t.Fatalf("trainer scored its own adapter and settlement returned %v", err)
		}
	})

	// The subtle one: two distinct worker ids owned by one account are not two
	// opinions, and this is the shape a supplier would actually use to grade
	// their own work.
	t.Run("two workers, one account", func(t *testing.T) {
		eval := validLoRAEval()
		shared := uuid.New()
		eval.TrainerAccountID = shared
		eval.EvaluatorAccountID = shared
		_, err := settleLoRARun(eval, 10.0)
		if !errors.Is(err, errLoRANotIndependent) {
			t.Fatalf("one account graded itself through two workers and settlement returned %v", err)
		}
		if !strings.Contains(err.Error(), "one account") {
			t.Fatalf("refusal did not explain the account link: %v", err)
		}
	})

	t.Run("independent evaluation settles", func(t *testing.T) {
		if _, err := settleLoRARun(validLoRAEval(), 10.0); err != nil {
			t.Fatalf("an independent evaluation was refused: %v", err)
		}
	})
}

// The comparison itself has to be one a payout can rest on.
func TestLoRAEvaluationRejectsUnsoundComparisons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*loraEvaluation)
		want   error
	}{
		{"scored a different set than the job reserved", func(e *loraEvaluation) {
			e.HeldOutSetSHA256 = strings.Repeat("9", 64)
		}, errLoRAHeldOutContested},
		{"held-out set leaked into training", func(e *loraEvaluation) {
			e.TrainerSawHeldOutSet = true
		}, errLoRAEvaluationShape},
		{"no held-out set named", func(e *loraEvaluation) {
			e.HeldOutSetSHA256, e.ReservedSetSHA256 = "", ""
		}, errLoRAEvaluationShape},
		{"zero baseline", func(e *loraEvaluation) { e.BaselineScore = 0 }, errLoRAEvaluationShape},
		{"negative baseline", func(e *loraEvaluation) { e.BaselineScore = -1 }, errLoRAEvaluationShape},
		{"NaN candidate", func(e *loraEvaluation) { e.CandidateScore = math.NaN() }, errLoRAEvaluationShape},
		{"infinite candidate", func(e *loraEvaluation) { e.CandidateScore = math.Inf(1) }, errLoRAEvaluationShape},
		{"negative required improvement", func(e *loraEvaluation) {
			e.RequiredImprovement = -0.5
		}, errLoRAEvaluationShape},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eval := validLoRAEval()
			tc.mutate(&eval)
			_, err := settleLoRARun(eval, 10.0)
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
			}
		})
	}
}

// A zero baseline makes "5% better" undefined; dividing by it yields +Inf,
// which compares greater than every threshold and would pay a full bonus for a
// model that learned nothing. This is the specific failure the guard prevents.
func TestLoRAZeroBaselineCannotManufactureSuccess(t *testing.T) {
	eval := validLoRAEval()
	eval.BaselineScore = 0
	eval.CandidateScore = 0.0001
	eval.RequiredImprovement = 1000.0
	if _, err := settleLoRARun(eval, 10.0); err == nil {
		t.Fatal("a zero baseline produced a settlement; +Inf improvement would clear any threshold")
	}
}

func TestLoRASettlementSplitsRiskAsDesigned(t *testing.T) {
	const quoted = 10.0
	total := usdToMicros(quoted)

	t.Run("failure charges the floor only", func(t *testing.T) {
		eval := validLoRAEval()
		eval.CandidateScore = eval.BaselineScore * 1.01 // +1%, below the agreed 5%
		s, err := settleLoRARun(eval, quoted)
		if err != nil {
			t.Fatalf("settle: %v", err)
		}
		if s.Succeeded {
			t.Fatal("a 1% improvement met a 5% requirement")
		}
		if s.BuyerDebitMicros != s.FloorMicros {
			t.Fatalf("buyer charged %d on a failed run, floor is %d", s.BuyerDebitMicros, s.FloorMicros)
		}
		// The supplier still burned real GPU hours on data it did not choose.
		if s.SupplierPayableMicros <= 0 {
			t.Fatalf("supplier owed %d for compute actually performed", s.SupplierPayableMicros)
		}
		if s.BuyerDebitMicros >= total {
			t.Fatalf("a failed run charged %d of a %d maximum", s.BuyerDebitMicros, total)
		}
	})

	t.Run("success charges the full quoted maximum", func(t *testing.T) {
		s, err := settleLoRARun(validLoRAEval(), quoted)
		if err != nil {
			t.Fatalf("settle: %v", err)
		}
		if !s.Succeeded {
			t.Fatalf("a 10%% improvement missed a 5%% requirement (measured %.4f)",
				s.ImprovementFraction)
		}
		if s.BuyerDebitMicros != total {
			t.Fatalf("successful run charged %d, quoted maximum is %d", s.BuyerDebitMicros, total)
		}
	})

	t.Run("the boundary is inclusive", func(t *testing.T) {
		eval := validLoRAEval()
		eval.BaselineScore = 1.0
		eval.CandidateScore = 1.05
		eval.RequiredImprovement = 0.05
		s, err := settleLoRARun(eval, quoted)
		if err != nil {
			t.Fatalf("settle: %v", err)
		}
		if !s.Succeeded {
			t.Fatal("exactly meeting the agreed margin was treated as failure")
		}
	})

	// merc must never prefer one outcome. If failure paid merc more than
	// success, merc would be choosing evaluators with a thumb on the scale.
	t.Run("merc never earns more from failure", func(t *testing.T) {
		fail := validLoRAEval()
		fail.CandidateScore = fail.BaselineScore
		failed, err := settleLoRARun(fail, quoted)
		if err != nil {
			t.Fatalf("settle: %v", err)
		}
		ok, err := settleLoRARun(validLoRAEval(), quoted)
		if err != nil {
			t.Fatalf("settle: %v", err)
		}
		if failed.PlatformMicros > ok.PlatformMicros {
			t.Fatalf("merc keeps %d on failure but %d on success -- merc is paid to fail runs",
				failed.PlatformMicros, ok.PlatformMicros)
		}
	})
}

// Money conservation under randomised inputs. Examples prove the cases someone
// thought of; this covers the rounding splits nobody did.
func TestLoRASettlementConservesMoney(t *testing.T) {
	rng := rand.New(rand.NewSource(20260727))
	for i := 0; i < 20000; i++ {
		eval := validLoRAEval()
		eval.BaselineScore = 0.001 + rng.Float64()*100
		// Span both outcomes, including exact-boundary and regression cases.
		eval.CandidateScore = eval.BaselineScore * (0.5 + rng.Float64()*1.5)
		eval.RequiredImprovement = rng.Float64() * 0.5
		quoted := microsToUSD(minLoRAQuoteMicros) + rng.Float64()*5000

		s, err := settleLoRARun(eval, quoted)
		if err != nil {
			t.Fatalf("iteration %d: settle(%v, %v): %v", i, eval.CandidateScore, quoted, err)
		}

		if s.BuyerDebitMicros != s.SupplierPayableMicros+s.PlatformMicros {
			t.Fatalf("iteration %d: buyer %d != supplier %d + platform %d",
				i, s.BuyerDebitMicros, s.SupplierPayableMicros, s.PlatformMicros)
		}
		// <= 0, not < 0. The first version of this assertion used < 0 and let
		// a settlement through that owed the supplier ZERO micro-USD for real
		// compute -- the floor had truncated away at a tiny quote.
		if s.SupplierPayableMicros <= 0 || s.PlatformMicros <= 0 {
			t.Fatalf("iteration %d: a party settled at or below zero for real compute: "+
				"supplier %d platform %d (quoted %v)",
				i, s.SupplierPayableMicros, s.PlatformMicros, quoted)
		}
		if total := usdToMicros(quoted); s.BuyerDebitMicros > total {
			t.Fatalf("iteration %d: charged %d over a quoted maximum of %d",
				i, s.BuyerDebitMicros, total)
		}
		if s.FloorMicros+s.BonusMicros != usdToMicros(quoted) {
			t.Fatalf("iteration %d: floor %d + bonus %d != quoted %d -- a micro-USD was "+
				"created or lost splitting the price",
				i, s.FloorMicros, s.BonusMicros, usdToMicros(quoted))
		}
		// A failed run must never cost the buyer more than a successful one.
		if !s.Succeeded && s.BuyerDebitMicros > s.FloorMicros {
			t.Fatalf("iteration %d: failed run charged %d above the floor %d",
				i, s.BuyerDebitMicros, s.FloorMicros)
		}
	}
}

// The supplier's floor must actually cover something. A floor that rounds to
// zero is the "paid nothing for real compute" outcome the split exists to
// prevent, arriving through arithmetic instead of policy.
func TestLoRAFloorSurvivesSmallQuotes(t *testing.T) {
	eval := validLoRAEval()
	eval.CandidateScore = eval.BaselineScore // failure
	// Down to the derived minimum, and one micro-USD either side of it.
	quotes := []float64{0.01, 0.001, 0.0001, microsToUSD(minLoRAQuoteMicros)}
	for _, quoted := range quotes {
		s, err := settleLoRARun(eval, quoted)
		if err != nil {
			t.Fatalf("quoted %v: %v", quoted, err)
		}
		if s.SupplierPayableMicros <= 0 {
			t.Fatalf("quoted $%v: supplier owed %d micro-USD for real compute -- the floor "+
				"rounded away", quoted, s.SupplierPayableMicros)
		}
	}

	// Below the minimum the run is refused, not settled at zero. Accepting it
	// would mean merc took work it already knew it could not pay for.
	if _, err := settleLoRARun(eval, microsToUSD(minLoRAQuoteMicros-1)); !errors.Is(err, errLoRAQuoteTooSmall) {
		t.Fatalf("a quote below the floor minimum settled instead of being refused: %v", err)
	}
}
