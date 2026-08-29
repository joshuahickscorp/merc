package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Randomized state-machine testing of the BUYER funding path.
//
// TestMoneyInvariantsHoldUnderRandomOperationOrderings does this for the
// supplier side — accrual, carry, claim. The buyer side had no equivalent, and
// the buyer side is where every money defect in this programme actually lived:
// realtime funding under-held open prepaid exposure; prepaid admission skipped
// the buyer advisory lock; prepaid refund ignored the EXECUTING hold; an upheld
// dispute left cash that a job still needed; the restore convention was a
// string one caller forgot. Five defects, one class, no ordering test.
//
// Each was an UNDER-HOLD: the store believed less cash was committed than
// really was, so it let a later operation spend money that was already owed.
// An under-hold is invisible to any assertion phrased in the store's own terms
// — balance minus what-the-store-holds is still non-negative when the store
// holds too little. So the model here tracks accepted liability INDEPENDENTLY,
// from what each operation was told to accept, and the invariant compares the
// store's belief against the model's:
//
//	B1  balance >= model liability          — the cash to meet what was accepted is still there
//	B2  store-held >= model liability       — the store holds at least what is owed (catches under-hold)
//	B3  balance == deposits - settled       — nothing minted, nothing lost
//	B4  a refund never pays out cash that B1 needs
//
// B2 is the one that matters. It is stated so that a wrong `held` cannot
// launder itself into the model, the same way the supplier model refuses to
// learn its expectation from the value under test.
type buyerFundingModel struct {
	buyerID uuid.UUID
	// depositedMicros is every micro of cash put in.
	depositedMicros int64
	// settledMicros is every micro irreversibly spent (task debits).
	settledMicros int64
	// refundedMicros is every micro paid back out to the card.
	refundedMicros int64
	// liability maps an open obligation to the micros still owed against it.
	// An obligation leaves only when it settles or is cancelled.
	liability map[string]int64
}

func (m *buyerFundingModel) owed() int64 {
	var total int64
	for _, v := range m.liability {
		total += v
	}
	return total
}

// check re-asserts every invariant against freshly read store state.
func (m *buyerFundingModel) check(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool, seed, step int, op string) {
	t.Helper()
	where := fmt.Sprintf("seed %d step %d after %s", seed, step, op)

	balance, err := store.BuyerPrepaidBalanceMicros(ctx, m.buyerID)
	mustf(t, err, where+": read balance: %v")

	currency := SettlementCurrencyCode()
	held, err := prepaidOpenReservationMicrosInCurrency(ctx, pool, m.buyerID, currency)
	mustf(t, err, where+": read open reservation: %v")
	var executingHold int64
	if err := pool.QueryRow(ctx,
		`SELECT (`+sqlOpenNonEnvelopeExecutingCeilingMicros("$1", "$2")+`)`,
		m.buyerID, currency,
	).Scan(&executingHold); err != nil {
		t.Fatalf("%s: read executing hold: %v", where, err)
	}
	storeHeld := held + executingHold

	owed := m.owed()

	// B1 — the cash needed to meet everything already accepted is still here.
	if balance < owed {
		t.Fatalf("%s: B1 broken — balance %d micros is below accepted liability %d micros; "+
			"work was admitted against cash that is no longer there", where, balance, owed)
	}

	// B2 — the store must hold at least what is owed. A store that holds less
	// will admit the next request against cash that is already committed.
	if storeHeld < owed {
		t.Fatalf("%s: B2 broken (UNDER-HOLD) — store holds %d micros (open %d + executing %d) "+
			"against accepted liability of %d micros; the next admission would oversubscribe by %d",
			where, storeHeld, held, executingHold, owed, owed-storeHeld)
	}

	// B3 — conservation. Every micro is deposited, settled, refunded, or still on the balance.
	if want := m.depositedMicros - m.settledMicros - m.refundedMicros; balance != want {
		t.Fatalf("%s: B3 broken — balance %d micros, but deposits %d - settled %d - refunded %d = %d; "+
			"%d micros were minted or lost", where, balance,
			m.depositedMicros, m.settledMicros, m.refundedMicros, want, balance-want)
	}
}

// settlementMicrosForTest converts refunded card minor units back to micros
// using the settlement currency's own exponent, so the model never assumes
// cents.
func settlementMicrosForTest(cents int64) (int64, error) {
	settlement, err := ParseCurrency(SettlementCurrencyCode())
	if err != nil {
		return 0, err
	}
	return settlement.MinorToMicros(cents)
}

func TestBuyerFundingInvariantsHoldUnderRandomOperationOrderings(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	actor := insertTestAdminActor(t, pool, ctx)

	const seeds = 4
	const stepsPerSeed = 30

	// Which arms actually executed. A randomized test whose refund arm never
	// fired is a green result about three operations, not five, and it would
	// read exactly like the real thing.
	arms := map[string]int{}

	for seed := 0; seed < seeds; seed++ {
		rng := rand.New(rand.NewSource(int64(41000 + seed)))

		buyerID := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
			buyerID, buyerID.String()+"@buyer-sm.invalid"); err != nil {
			t.Fatal(err)
		}
		m := &buyerFundingModel{buyerID: buyerID, liability: map[string]int64{}}

		// Deposits go through the real top-up path, not SeedPrepaidBalance.
		//
		// SeedPrepaidBalance credits a balance with no charge behind it, and the
		// refund rail correctly refuses to pay a card what it cannot trace to an
		// unrefunded top-up ("refund would exceed funded value"). Seeding
		// therefore makes the refund arm unreachable, and an arm that never runs
		// proves nothing while still reporting green.
		topup := func(step int, cents int64) {
			t.Helper()
			key := fmt.Sprintf("sm-topup-%d-%d-%s", seed, step, buyerID)
			_, err := store.BeginPrepaidTopup(ctx, key, buyerID, cents)
			mustf(t, err, "begin topup: %v")
			must(t, store.CreditPrepaidTopup(ctx, key, buyerID, ChargeResult{
				PaymentIntentID: "pi_" + key, ChargeID: "ch_" + key,
				RequestedCents: cents, ReceivedCents: cents, Currency: SettlementCurrencyCode(),
			}))
			micros, cerr := settlementMicrosForTest(cents)
			mustf(t, cerr, "topup minor->micros: %v")
			m.depositedMicros += micros
			arms["topup"]++
		}

		// Start funded so the first operations have something to work against.
		topup(-1, 2_000) // $20
		m.check(t, ctx, store, pool, seed, -1, "opening deposit")

		// Open jobs available to settle later, so the sequence interleaves
		// admission and settlement rather than doing all of one then the other.
		type openJob struct {
			jobID, taskID uuid.UUID
			reservedUSD   float64
			reservedMicro int64
		}
		var open []openJob

		for step := 0; step < stepsPerSeed; step++ {
			switch op := rng.Intn(10); {

			case op < 2: // top up
				topup(step, int64(rng.Intn(5)+1)*100)
				m.check(t, ctx, store, pool, seed, step, "topup")

			case op < 6: // admit a prepaid batch job through the real funding check
				// Reserved is what the buyer becomes liable for; estimated is
				// the smaller figure that under-holding code used instead.
				reservedUSD := float64(rng.Intn(200)+50) / 100.0 // $0.50 .. $2.49
				reservedMicro := int64(reservedUSD * 1_000_000)
				estimatedUSD := reservedUSD / 2

				tx, err := pool.Begin(ctx)
				mustf(t, err, "begin admit: %v")
				err = reservePrepaidForJobTx(ctx, tx, buyerID, reservedMicro)
				if errors.Is(err, errInsufficientPrepaid) {
					_ = tx.Rollback(ctx)
					// A refusal must be truthful: the buyer really must not
					// have had room. Model-side room is balance minus owed.
					balance, berr := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
					mustf(t, berr, "read balance after refusal: %v")
					if room := balance - m.owed(); room >= reservedMicro {
						t.Fatalf("seed %d step %d: admission refused %d micros while %d micros were "+
							"genuinely free (balance %d, accepted liability %d) — an OVER-hold that "+
							"refuses work the buyer paid for", seed, step, reservedMicro, room, balance, m.owed())
					}
					m.check(t, ctx, store, pool, seed, step, "admission refused")
					continue
				}
				mustf(t, err, "admit: %v")

				jobID, taskID := uuid.New(), uuid.New()
				if _, err := tx.Exec(ctx, `
					INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,task_count,prepaid_required,estimated_usd,currency)
					VALUES ($1,$2,'running','embed','sm',1,true,$3,'usd')`,
					jobID, buyerID, estimatedUSD); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `
					INSERT INTO job_economic_plans
					  (job_id,plan_version,schedule_version,plan_json,initial_task_count,
					   buyer_charge_per_task_usd,supplier_payout_per_task_usd,
					   initial_buyer_charge_usd,reserved_buyer_charge_usd,currency)
					VALUES ($1,1,'test','{"schedule":{"currency":"usd"}}',1,$2,$3,$2,$4,'usd')`,
					jobID, estimatedUSD, estimatedUSD/2, reservedUSD); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `
					INSERT INTO tasks (id,job_id,status,input_ref,result_key,visible_at)
					VALUES ($1,$2,'running','in','rk',now())`, taskID, jobID); err != nil {
					t.Fatal(err)
				}
				mustf(t, tx.Commit(ctx), "commit admit: %v")

				m.liability[jobID.String()] = reservedMicro
				open = append(open, openJob{jobID: jobID, taskID: taskID,
					reservedUSD: reservedUSD, reservedMicro: reservedMicro})
				arms["admit"]++
				m.check(t, ctx, store, pool, seed, step, "admit")

			case op < 8: // settle one open job's task
				if len(open) == 0 {
					continue
				}
				i := rng.Intn(len(open))
				job := open[i]
				open = append(open[:i], open[i+1:]...)

				// Settle at or below the reservation, as production does.
				debitMicros := job.reservedMicro
				if rng.Intn(2) == 0 {
					debitMicros = job.reservedMicro / 2
				}
				if debitMicros <= 0 {
					continue
				}
				must(t, store.DebitPrepaidForTask(ctx, buyerID, job.taskID, debitMicros))
				if _, err := pool.Exec(ctx,
					`UPDATE jobs SET status='complete' WHERE id=$1`, job.jobID); err != nil {
					t.Fatal(err)
				}
				m.settledMicros += debitMicros
				delete(m.liability, job.jobID.String())
				arms["settle"]++
				m.check(t, ctx, store, pool, seed, step, "settle")

			case op < 9: // replay a settlement: must be inert
				if len(open) == 0 {
					continue
				}
				// Pop it. A task this op has already debited must not be
				// eligible again, or the model would count a debit the store
				// correctly refused to take twice — and B3 would fail on the
				// model rather than on the store.
				i := rng.Intn(len(open))
				job := open[i]
				open = append(open[:i], open[i+1:]...)
				debit := job.reservedMicro / 2
				if debit <= 0 {
					continue
				}
				must(t, store.DebitPrepaidForTask(ctx, buyerID, job.taskID, debit))
				before, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
				mustf(t, err, "balance before replay: %v")
				// Same task, same idempotency surface: a second debit must not
				// take the money twice.
				_ = store.DebitPrepaidForTask(ctx, buyerID, job.taskID, debit)
				after, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
				mustf(t, err, "balance after replay: %v")
				if after != before {
					t.Fatalf("seed %d step %d: replaying a task debit moved the balance %d -> %d; "+
						"a retried settlement charged the buyer twice", seed, step, before, after)
				}
				m.settledMicros += debit
				// The job keeps its liability: it is partially settled, and the
				// remainder is still owed.
				if rest := m.liability[job.jobID.String()] - debit; rest > 0 {
					m.liability[job.jobID.String()] = rest
				} else {
					delete(m.liability, job.jobID.String())
				}
				arms["settlement replay"]++
				m.check(t, ctx, store, pool, seed, step, "settlement replay")

			default: // refund whatever is genuinely free
				plan, err := store.BeginPrepaidRefund(ctx, actor, buyerID, "sm refund",
					fmt.Sprintf("INC-sm-%d-%d-%s", seed, step, uuid.NewString()[:8]))
				if errors.Is(err, errInsufficientPrepaid) {
					m.check(t, ctx, store, pool, seed, step, "refund refused")
					continue
				}
				if err != nil {
					t.Logf("seed %d step %d: refund declined: %v", seed, step, err)
					m.check(t, ctx, store, pool, seed, step, "refund declined")
					continue
				}
				refunded, cerr := settlementMicrosForTest(plan.Cents)
				mustf(t, cerr, "refund minor->micros: %v")
				m.refundedMicros += refunded
				arms["refund"]++
				m.check(t, ctx, store, pool, seed, step, "refund")
			}
		}

		balance, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
		mustf(t, err, "final balance: %v")
		t.Logf("seed %d: deposited %d, settled %d, refunded %d, balance %d, %d obligations still open (%d micros owed)",
			seed, m.depositedMicros, m.settledMicros, m.refundedMicros, balance, len(m.liability), m.owed())
	}

	for _, arm := range []string{"topup", "admit", "settle", "settlement replay", "refund"} {
		if arms[arm] == 0 {
			t.Fatalf("the %q operation never executed across %d seeds x %d steps — this run proves "+
				"nothing about it, and a green result here would be a false one (arm counts: %v)",
				arm, seeds, stepsPerSeed, arms)
		}
	}
	t.Logf("arm coverage: %v", arms)
}
