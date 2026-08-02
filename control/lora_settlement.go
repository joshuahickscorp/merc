package main

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Outcome-aware settlement for LoRA training.
//
// Training is the first thing merc sells where the buyer cannot tell from the
// artifact whether they got what they paid for. An embedding is right or wrong;
// a fine-tuned adapter is a file that always exists and may be worthless. So
// "did this work?" has to be answered by measurement, by someone with no stake
// in the answer, before anyone is paid.
//
// # Who carries the risk of a training run that does not help
//
// Someone must. The three candidates are all wrong on their own:
//
//   - The supplier, paid only on success. They burned real GPU hours on data
//     they did not choose and cannot inspect. A supplier who can lose a day's
//     revenue to somebody else's bad dataset will not take training work, and
//     merc's own pricing governance already refuses prices that do not cover a
//     supplier's electricity -- paying zero for real compute is the same
//     violation, just conditional.
//   - The buyer, paying regardless. Then settlement is not outcome-aware; it is
//     ordinary compute billing with a nicer name.
//   - merc, refunding the buyer while paying the supplier. That is merc paying
//     for every bad dataset anyone uploads, and it is unbounded.
//
// So the price splits. A COMPUTE FLOOR covers the supplier for work actually
// performed and is owed however the evaluation turns out -- the buyer consumed
// real hardware on their own data. An OUTCOME BONUS sits on top and is owed only
// if an independent evaluation says the adapter beat the baseline by the agreed
// margin. The buyer knows both numbers before the run starts.
//
// merc's own contribution is taken from the floor, not the bonus, so a failed
// run is not a loss merc has to absorb, and merc never has an incentive to
// prefer one outcome over the other.

const (
	// A supplier's compute floor as a share of the quoted maximum. The rest is
	// contingent. Set here rather than per-request so a buyer cannot quote a
	// floor so thin that the supplier is effectively working on spec.
	loraComputeFloorShareNanos int64 = 600_000_000

	// merc's take, applied to the floor only.
	loraPlatformShareOfFloorNanos int64 = 200_000_000

	// The supplier's share of the bonus when the run succeeds. The remainder is
	// merc's, and it is the only place merc's revenue depends on the outcome --
	// deliberately small, because merc decides who evaluates.
	loraSupplierShareOfBonusNanos int64 = 850_000_000
)

var (
	errLoRANotIndependent   = errors.New("LoRA evaluation is not independent")
	errLoRAAlreadySettled   = errors.New("LoRA run is already settled")
	errLoRAEvaluationShape  = errors.New("invalid LoRA evaluation")
	errLoRANegativeMargin   = errors.New("LoRA settlement would leave merc a negative contribution")
	errLoRAHeldOutContested = errors.New("LoRA baseline and candidate were not scored on the same held-out set")
	errLoRAQuoteTooSmall    = errors.New("LoRA quote is too small to pay anyone")
)

// minLoRAQuoteMicros is the smallest quote whose floor still leaves the
// supplier AND merc at least one ledger micro after integer truncation.
//
// Derived, not hardcoded: the shares above are the only inputs, so changing one
// cannot silently reintroduce the hole this guards. Below this, floor * share
// truncates to zero and the supplier is owed nothing for compute it really
// performed -- the exact outcome the floor exists to prevent, arriving through
// arithmetic rather than policy. Found by probing the settlement at sub-cent
// quotes: at 1 ledger micro the floor was 0 and every party settled at zero.
var minLoRAQuoteMicros = func() int64 {
	for total := int64(1); total < 1_000_000; total++ {
		floor, err := mulDiv(total, loraComputeFloorShareNanos, NanosPerMajorUnit, false)
		if err != nil {
			panic(err)
		}
		platform, err := mulDiv(floor, loraPlatformShareOfFloorNanos, NanosPerMajorUnit, false)
		if err != nil {
			panic(err)
		}
		if floor-platform >= 1 && platform >= 1 && total-floor >= 1 {
			return total
		}
	}
	panic("no LoRA quote can pay both parties; the share constants are inconsistent")
}()

// loraEvaluation is what an independent evaluator reports. Both scores come
// from the SAME held-out set, scored by the SAME evaluator, in one run: a
// baseline measured elsewhere, or earlier, or by someone else, is not a
// comparison -- it is two numbers that happen to be adjacent.
type loraEvaluation struct {
	RunID uuid.UUID
	// TrainerSupplierID trained the adapter. EvaluatorSupplierID scored it.
	TrainerSupplierID   uuid.UUID
	EvaluatorSupplierID uuid.UUID
	// The account each supplier belongs to. Two worker ids owned by one account
	// are not two opinions.
	TrainerAccountID   uuid.UUID
	EvaluatorAccountID uuid.UUID

	// HeldOutSetSHA256 identifies the evaluation data. It must match what the
	// job reserved, and the trainer must never have received it.
	HeldOutSetSHA256  string
	ReservedSetSHA256 string
	// TrainerSawHeldOutSet records whether the held-out set leaked into the
	// training shard. If it did, the comparison is meaningless in the direction
	// that pays out.
	TrainerSawHeldOutSet bool

	// Higher is better for both. Same metric, same set, same evaluator.
	BaselineScore  float64
	CandidateScore float64
	// RequiredImprovement is the margin agreed with the buyer up front, as a
	// fraction of the baseline (0.05 = the adapter must be 5% better).
	RequiredImprovement float64
}

// loraExactSettlement is the pre-ledger authority. Shares are applied in one
// integer expression; micro projection happens once at the external ledger
// boundary. This prevents a small quote from being rounded before the floor or
// outcome share is computed.
type loraExactSettlement struct {
	BuyerDebitNanos      int64
	SupplierPayableNanos int64
	PlatformNanos        int64
	FloorNanos           int64
	BonusNanos           int64
}

func settleLoRAExact(totalNanos int64, succeeded bool) (loraExactSettlement, error) {
	if totalNanos <= 0 {
		return loraExactSettlement{}, errLoRAQuoteTooSmall
	}
	floor, err := mulDiv(totalNanos, loraComputeFloorShareNanos, NanosPerMajorUnit, false)
	if err != nil {
		return loraExactSettlement{}, err
	}
	bonus := totalNanos - floor
	platformFloor, err := mulDiv(floor, loraPlatformShareOfFloorNanos, NanosPerMajorUnit, false)
	if err != nil {
		return loraExactSettlement{}, err
	}
	out := loraExactSettlement{FloorNanos: floor, BonusNanos: bonus}
	if succeeded {
		supplierBonus, e := mulDiv(bonus, loraSupplierShareOfBonusNanos, NanosPerMajorUnit, false)
		if e != nil {
			return loraExactSettlement{}, e
		}
		out.BuyerDebitNanos = totalNanos
		out.SupplierPayableNanos = floor - platformFloor + supplierBonus
		out.PlatformNanos = platformFloor + bonus - supplierBonus
	} else {
		out.BuyerDebitNanos = floor
		out.SupplierPayableNanos = floor - platformFloor
		out.PlatformNanos = platformFloor
	}
	if out.BuyerDebitNanos != out.SupplierPayableNanos+out.PlatformNanos || out.SupplierPayableNanos <= 0 || out.PlatformNanos <= 0 {
		return loraExactSettlement{}, errLoRANegativeMargin
	}
	return out, nil
}

// loraSettlement is the outcome decision. Nanos are the economic authority;
// micros are the one external-ledger projection. Currency is carried with both
// so this result can never be mistaken for a USD amount in a CAD deployment.
type loraSettlement struct {
	Currency              string
	Succeeded             bool
	Reason                string
	BuyerDebitNanos       int64
	SupplierPayableNanos  int64
	PlatformNanos         int64
	FloorNanos            int64
	BonusNanos            int64
	BuyerDebitMicros      int64
	SupplierPayableMicros int64
	PlatformMicros        int64
	FloorMicros           int64
	BonusMicros           int64
	ImprovementFraction   float64
}

// validateLoRAIndependence is the gate that decides whether an evaluation may
// move money at all.
//
// A trainer scoring its own adapter is not a check, it is a claim. merc already
// treats a same-supplier redundancy match as unverified for inference
// (Verification.SameSupplier); training is the same principle with a larger
// payout attached, so the rule is stricter: not the same worker, and not the
// same account behind two workers.
func validateLoRAIndependence(eval loraEvaluation) error {
	if eval.TrainerSupplierID == uuid.Nil || eval.EvaluatorSupplierID == uuid.Nil {
		return fmt.Errorf("%w: both a trainer and an evaluator must be named", errLoRAEvaluationShape)
	}
	if eval.TrainerSupplierID == eval.EvaluatorSupplierID {
		return fmt.Errorf("%w: the trainer scored its own adapter", errLoRANotIndependent)
	}
	if eval.TrainerAccountID != uuid.Nil && eval.TrainerAccountID == eval.EvaluatorAccountID {
		return fmt.Errorf("%w: trainer and evaluator are two workers on one account",
			errLoRANotIndependent)
	}
	return nil
}

// validateLoRAEvaluation checks the comparison is one a payout can rest on.
func validateLoRAEvaluation(eval loraEvaluation) error {
	if err := validateLoRAIndependence(eval); err != nil {
		return err
	}
	if !validSHA256(eval.HeldOutSetSHA256) || !validSHA256(eval.ReservedSetSHA256) {
		// The short display commitments below are safe only after this exact
		// shape gate. An evaluator controls these values, so accepting a merely
		// non-empty string would let a malformed cross-set comparison panic the
		// settlement path instead of producing a deterministic refusal.
		return fmt.Errorf("%w: held-out and reserved sets must be SHA-256 identities", errLoRAEvaluationShape)
	}
	if eval.HeldOutSetSHA256 != eval.ReservedSetSHA256 {
		return fmt.Errorf("%w: scored on %s, the job reserved %s",
			errLoRAHeldOutContested, eval.HeldOutSetSHA256[:8], eval.ReservedSetSHA256[:8])
	}
	if eval.TrainerSawHeldOutSet {
		// Not merely suspicious: an adapter trained on the evaluation set scores
		// well by memorising it, which is the exact failure the held-out set
		// exists to detect. Paying a bonus on it pays for overfitting.
		return fmt.Errorf("%w: the held-out set leaked into the training shard, so a "+
			"score improvement measures memorisation rather than generalisation",
			errLoRAEvaluationShape)
	}
	if !moneyUSDInDomain(eval.RequiredImprovement) || eval.RequiredImprovement < 0 {
		return fmt.Errorf("%w: required improvement must be finite and non-negative", errLoRAEvaluationShape)
	}
	if eval.BaselineScore <= 0 {
		// A zero or negative baseline makes "5% better" undefined, and dividing
		// by it silently produces Inf that compares greater than any threshold.
		return fmt.Errorf("%w: baseline score must be positive to express an improvement "+
			"margin against it, got %v", errLoRAEvaluationShape, eval.BaselineScore)
	}
	if !moneyUSDInDomain(eval.BaselineScore) || !moneyUSDInDomain(eval.CandidateScore) {
		return fmt.Errorf("%w: scores must be finite", errLoRAEvaluationShape)
	}
	return nil
}

// settleLoRARun computes a currency-bound outcome settlement. quotedMaximum is
// the buyer's exact ceiling; it is never exceeded in either outcome.
func settleLoRARun(eval loraEvaluation, quotedMaximum MoneyNanos) (loraSettlement, error) {
	if err := validateLoRAEvaluation(eval); err != nil {
		return loraSettlement{}, err
	}
	if !quotedMaximum.Currency.Valid() || quotedMaximum.Nanos <= 0 {
		return loraSettlement{}, fmt.Errorf("%w: quoted maximum must be a positive currency-bound amount, got %s",
			errLoRAEvaluationShape, quotedMaximum)
	}

	totalMicros, err := LedgerMicrosFromNanos(quotedMaximum)
	if err != nil {
		return loraSettlement{}, err
	}
	if totalMicros < minLoRAQuoteMicros {
		// Refused rather than settled at zero. A run this cheap cannot pay the
		// supplier for the hardware it burns, and settling it anyway would mean
		// merc accepted work it knew it could not pay for.
		return loraSettlement{}, fmt.Errorf(
			"%w: %d ledger micros quoted, the minimum that still pays the supplier and merc "+
				"at least one ledger micro each is %d",
			errLoRAQuoteTooSmall, totalMicros, minLoRAQuoteMicros)
	}
	improvement := (eval.CandidateScore - eval.BaselineScore) / eval.BaselineScore
	succeeded := improvement >= eval.RequiredImprovement
	exact, err := settleLoRAExact(quotedMaximum.Nanos, succeeded)
	if err != nil {
		return loraSettlement{}, err
	}
	// The external ledger is micro-major-units. Project the complete buyer
	// amount once, then allocate its integer remainder to Merc; this preserves
	// buyer = supplier + platform without turning a supplier share into a second
	// independently rounded price authority.
	buyerMicros := totalMicros
	supplierMicros, err := LedgerMicrosFromNanos(MoneyNanos{Currency: quotedMaximum.Currency, Nanos: exact.SupplierPayableNanos})
	if err != nil || supplierMicros <= 0 || supplierMicros >= buyerMicros {
		return loraSettlement{}, errLoRAQuoteTooSmall
	}
	settlement := loraSettlement{
		Currency:             quotedMaximum.Currency.Code(),
		Succeeded:            succeeded,
		BuyerDebitNanos:      exact.BuyerDebitNanos,
		SupplierPayableNanos: exact.SupplierPayableNanos,
		PlatformNanos:        exact.PlatformNanos,
		FloorNanos:           exact.FloorNanos,
		BonusNanos:           exact.BonusNanos,
		ImprovementFraction:  improvement,
	}

	if !succeeded {
		// The supplier is paid for the compute it actually performed; the buyer
		// pays only the floor. Nobody is charged for the bonus, so nobody has to
		// argue about whether it was earned.
		settlement.Reason = fmt.Sprintf(
			"adapter improved the held-out score by %.2f%%, below the agreed %.2f%%; "+
				"compute floor settled, outcome bonus not charged",
			improvement*100, eval.RequiredImprovement*100)
		floorMicros, e := LedgerMicrosFromNanos(MoneyNanos{Currency: quotedMaximum.Currency, Nanos: exact.BuyerDebitNanos})
		if e != nil || floorMicros <= 0 || supplierMicros >= floorMicros {
			return loraSettlement{}, errLoRAQuoteTooSmall
		}
		settlement.BuyerDebitMicros = floorMicros
		settlement.FloorMicros = floorMicros
		settlement.BonusMicros = totalMicros - floorMicros
		settlement.SupplierPayableMicros = supplierMicros
		settlement.PlatformMicros = floorMicros - supplierMicros
	} else {
		settlement.Reason = fmt.Sprintf(
			"adapter improved the held-out score by %.2f%%, meeting the agreed %.2f%%",
			improvement*100, eval.RequiredImprovement*100)
		settlement.BuyerDebitMicros = buyerMicros
		floorMicros, e := LedgerMicrosFromNanos(MoneyNanos{Currency: quotedMaximum.Currency, Nanos: exact.FloorNanos})
		if e != nil || floorMicros <= 0 || floorMicros >= buyerMicros {
			return loraSettlement{}, errLoRAQuoteTooSmall
		}
		settlement.FloorMicros = floorMicros
		settlement.BonusMicros = buyerMicros - floorMicros
		settlement.SupplierPayableMicros = supplierMicros
		settlement.PlatformMicros = buyerMicros - supplierMicros
	}

	// Conservation, checked rather than assumed: what the buyer is debited is
	// exactly what the supplier is owed plus what merc keeps. A rounding split
	// that loses a ledger micro here is money that exists on one side of the ledger
	// and not the other.
	if settlement.BuyerDebitMicros != settlement.SupplierPayableMicros+settlement.PlatformMicros {
		return loraSettlement{}, fmt.Errorf(
			"LoRA settlement does not conserve: buyer %d != supplier %d + platform %d",
			settlement.BuyerDebitMicros, settlement.SupplierPayableMicros, settlement.PlatformMicros)
	}
	if settlement.SupplierPayableMicros < 0 {
		return loraSettlement{}, fmt.Errorf(
			"LoRA settlement would owe the supplier a negative amount (%d ledger micros)",
			settlement.SupplierPayableMicros)
	}
	if settlement.PlatformMicros < 0 {
		return loraSettlement{}, fmt.Errorf("%w: %d ledger micros",
			errLoRANegativeMargin, settlement.PlatformMicros)
	}
	if settlement.BuyerDebitMicros > totalMicros {
		return loraSettlement{}, fmt.Errorf(
			"LoRA settlement would charge %d ledger micros against a quoted maximum of %d",
			settlement.BuyerDebitMicros, totalMicros)
	}
	return settlement, nil
}
