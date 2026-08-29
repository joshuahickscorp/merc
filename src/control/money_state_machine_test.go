package main

import (
	"math/rand"
	"testing"

	"github.com/google/uuid"
)

// Randomized state-machine testing of the money path.
//
// Table tests check sequences I chose. Fuzzing checks single values. This
// checks ORDERINGS: it drives a random sequence of operations against the real
// store and re-asserts every money invariant after each step, so a bug that
// only appears when a claim lands between two other claims has somewhere to
// show up.
//
// The invariants are the ones that would cost real money if violated:
//
//	I1  absorbed = paid*10000 + carried          (nothing minted, nothing lost)
//	I2  carry is always drained below one cent
//	I3  a claim never reports more cents than the accrual recorded
//	I4  physical tokens never exceed delivered tokens
//	I5  a job's charge never exceeds full-rate billing

type moneyModel struct {
	supplier      uuid.UUID
	absorbedMicro int64 // what the model believes was absorbed
	claimedCents  int64 // what claims have returned
}

func (m *moneyModel) checkInvariants(t *testing.T, step int, acc supplierAccrual) {
	t.Helper()
	if acc.LifetimeAbsorbed != m.absorbedMicro {
		t.Fatalf("step %d: I1 broken -- store absorbed %d, model expected %d",
			step, acc.LifetimeAbsorbed, m.absorbedMicro)
	}
	if got := acc.LifetimePaidCent*microUSDPerCent + acc.AccruedMicros; got != acc.LifetimeAbsorbed {
		t.Fatalf("step %d: I1 broken -- paid %d cents + carry %d != absorbed %d",
			step, acc.LifetimePaidCent, acc.AccruedMicros, acc.LifetimeAbsorbed)
	}
	if acc.AccruedMicros < 0 || acc.AccruedMicros >= microUSDPerCent {
		t.Fatalf("step %d: I2 broken -- carry %d outside [0, one cent)", step, acc.AccruedMicros)
	}
	if m.claimedCents != acc.LifetimePaidCent {
		t.Fatalf("step %d: I3 broken -- claims returned %d cents, accrual recorded %d",
			step, m.claimedCents, acc.LifetimePaidCent)
	}
}

func TestMoneyInvariantsHoldUnderRandomOperationOrderings(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-statemachine")

	const seeds = 6
	const stepsPerSeed = 40

	for seed := 0; seed < seeds; seed++ {
		rng := rand.New(rand.NewSource(int64(9000 + seed)))
		m := &moneyModel{supplier: uuid.New()}

		// Entries seeded but not yet claimed, so the sequence can interleave
		// creation and claiming rather than doing all of one then the other.
		var pending []uuid.UUID

		for step := 0; step < stepsPerSeed; step++ {
			switch op := rng.Intn(10); {

			case op < 5: // create a credit, usually sub-cent
				usd := float64(rng.Intn(900)+1) / 100000.0 // 0.00001 .. 0.009
				if rng.Intn(10) == 0 {
					usd = float64(rng.Intn(300)+1) / 100.0 // occasionally a large one
				}
				f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
					creditUSD: usd, supplierID: m.supplier,
				})
				pending = append(pending, f.entryID)

			case op < 9: // claim a pending entry
				if len(pending) == 0 {
					continue
				}
				i := rng.Intn(len(pending))
				entry := pending[i]
				pending = append(pending[:i], pending[i+1:]...)

				before, err := store.SupplierAccrual(ctx, m.supplier)
				mustf(t, err, "seed %d step %d: read accrual: %v", seed, step)
				out, _, err := store.ClaimPayout(ctx, entry)
				mustf(t, err, "seed %d step %d: claim: %v", seed, step)
				after, err := store.SupplierAccrual(ctx, m.supplier)
				mustf(t, err, "seed %d step %d: re-read accrual: %v", seed, step)

				// The model learns the absorbed amount from the store's own
				// delta, then verifies the delta is internally consistent --
				// so a wrong delta cannot be laundered into the model.
				delta := after.LifetimeAbsorbed - before.LifetimeAbsorbed
				if delta < 0 {
					t.Fatalf("seed %d step %d: absorbed went backwards by %d", seed, step, -delta)
				}
				expectCents := (before.AccruedMicros + delta) / microUSDPerCent
				if out.RequestedCents != expectCents {
					t.Fatalf("seed %d step %d: claim paid %d cents, arithmetic says %d "+
						"(carry-in %d + liability %d)",
						seed, step, out.RequestedCents, expectCents, before.AccruedMicros, delta)
				}
				m.absorbedMicro += delta
				m.claimedCents += out.RequestedCents

			default: // re-claim an already-claimed entry: must not double-absorb
				f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
					creditUSD: 0.004, supplierID: m.supplier,
				})
				before, _ := store.SupplierAccrual(ctx, m.supplier)
				out, _, err := store.ClaimPayout(ctx, f.entryID)
				mustf(t, err, "seed %d step %d: first claim: %v", seed, step)
				mid, _ := store.SupplierAccrual(ctx, m.supplier)
				m.absorbedMicro += mid.LifetimeAbsorbed - before.LifetimeAbsorbed
				m.claimedCents += out.RequestedCents

				// Replay must be inert.
				_, _, _ = store.ClaimPayout(ctx, f.entryID)
				replayed, _ := store.SupplierAccrual(ctx, m.supplier)
				if replayed != mid {
					t.Fatalf("seed %d step %d: replaying a claim changed the accrual %+v -> %+v",
						seed, step, mid, replayed)
				}
			}

			acc, err := store.SupplierAccrual(ctx, m.supplier)
			mustf(t, err, "seed %d step %d: read accrual: %v", seed, step)
			m.checkInvariants(t, step, acc)
		}

		final, _ := store.SupplierAccrual(ctx, m.supplier)
		t.Logf("seed %d: absorbed %d micro-USD, paid %d cents, carried %d, %d entries left unclaimed",
			seed, final.LifetimeAbsorbed, final.LifetimePaidCent, final.AccruedMicros, len(pending))
	}
}

// Billing invariants under randomized class mixes: physical never exceeds
// delivered, and a charge never exceeds full-rate billing, for any combination.
func TestBillingInvariantsHoldForRandomClassMixes(t *testing.T) {
	rng := rand.New(rand.NewSource(4242))
	const price = 0.000036

	for i := 0; i < 20000; i++ {
		acct := TokenAccounting{}
		for _, class := range []string{
			ClassUncachedInput, ClassGeneratedOutput,
			ClassPrefixReusedInput, ClassExactCachedInput, ClassExactResultReuse,
		} {
			if rng.Intn(3) > 0 {
				acct[class] = int64(rng.Intn(1_000_000))
			}
		}
		phys, deliv := acct.PhysicalTokens(), acct.DeliveredTokens()
		if phys > deliv {
			t.Fatalf("iteration %d: I4 broken -- physical %d > delivered %d (%+v)",
				i, phys, deliv, acct)
		}
		charged := PriceAccounting(acct, price)
		ceiling := roundUSD(float64(deliv) / 1000.0 * price)
		if charged > ceiling {
			t.Fatalf("iteration %d: I5 broken -- charged %.10f > full-rate %.10f (%+v)",
				i, charged, ceiling, acct)
		}
		if charged < 0 {
			t.Fatalf("iteration %d: negative charge %.10f", i, charged)
		}
		// Reuse must never be dearer than the same tokens uncached.
		if reused := acct.DeliveredTokens() - acct.PhysicalTokens(); reused > 0 {
			allPhysical := TokenAccounting{ClassUncachedInput: deliv}
			if charged > PriceAccounting(allPhysical, price) {
				t.Fatalf("iteration %d: reuse cost more than computing everything (%+v)", i, acct)
			}
		}
	}
}
