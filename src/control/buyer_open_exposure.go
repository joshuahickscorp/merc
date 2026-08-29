package main

import (
	"fmt"
)

// buyer_open_exposure.go — singular open-exposure definition for buyer funding.
//
// Three rails used to compute "what is already held against this buyer's funds"
// independently and under-held (realtime omitted service leases and prepaid
// batch residual; free credit used estimated not reserved and did not serialise).
// The SQL below is the one definition. Each call site embeds it and adds only
// the non-overlapping site-specific siblings documented on that site.
//
// Parameter contract for every fragment:
//   buyerExpr    — SQL expression for the buyer UUID (e.g. "$1" or "b.id")
//   currencyExpr — SQL expression for the settlement currency code
//                  (e.g. "$2" or "'usd'"). Canonical uses
//                  pricing_decision->>'currency' for leases and j.currency for
//                  jobs; realtime threads SettlementCurrencyCode() as $2; free
//                  credit is USD-only and passes the literal 'usd'.
//
// All amounts are integer micros. Envelope/orphan nanos round UP to micros
// ((n + 999) / 1000) — never to-nearest.

// sqlOpenPrepaidJobResidualMicros is the prepaid-job arm of open exposure:
// residual of reserved_buyer_charge_usd net of prepaid_debit for live
// prepaid_required jobs in queued/running/verifying.
func sqlOpenPrepaidJobResidualMicros(buyerExpr, currencyExpr string) string {
	return fmt.Sprintf(`
		SELECT COALESCE(SUM(GREATEST(0::bigint,
		  (p.reserved_buyer_charge_usd * 1000000)::bigint - COALESCE((
		    SELECT SUM((-le.amount_usd * 1000000)::bigint)
		      FROM ledger_entries le
		     WHERE le.kind='prepaid_debit'
		       AND le.currency=%[2]s
		       AND (le.task_id IN (SELECT id FROM tasks WHERE job_id=j.id)
		            OR le.payout_ref='prepaid-sla-' || j.id::text)
		  ), 0)
		)), 0)::bigint
		  FROM jobs j
		  JOIN job_economic_plans p ON p.job_id=j.id
		 WHERE j.buyer_id=%[1]s AND j.currency=%[2]s AND j.prepaid_required
		   AND j.status IN ('queued','running','verifying')`, buyerExpr, currencyExpr)
}

// sqlOpenServiceLeaseReservedMicros holds reserved_buyer_micros for leases in
// ACTIVE / UPGRADING / FAILOVER_REQUIRED.
func sqlOpenServiceLeaseReservedMicros(buyerExpr, currencyExpr string) string {
	return fmt.Sprintf(`
		SELECT COALESCE(SUM(l.reserved_buyer_micros),0)::bigint
		  FROM service_leases l
		 WHERE l.buyer_id=%[1]s AND l.pricing_decision->>'currency'=%[2]s
		   AND l.state IN ('ACTIVE','UPGRADING','FAILOVER_REQUIRED')`, buyerExpr, currencyExpr)
}

// sqlOpenActiveEnvelopeRemainingMicros holds (cap - spent) ceiled to micros for
// ACTIVE execution envelopes.
func sqlOpenActiveEnvelopeRemainingMicros(buyerExpr, currencyExpr string) string {
	return fmt.Sprintf(`
		SELECT COALESCE(SUM(((e.cap_nanos - e.spent_nanos) + 999) / 1000),0)::bigint
		  FROM execution_envelopes e
		 WHERE e.buyer_id=%[1]s AND e.currency=%[2]s AND e.state='ACTIVE'`, buyerExpr, currencyExpr)
}

// sqlOpenOrphanEnvelopeSpendMicros holds still-RESERVED envelope spends bound
// to EXECUTING contracts whose envelope is no longer ACTIVE. Sibling of the
// realtime EXECUTING fallback: when the ACTIVE-envelope term drops to zero on
// expiry, this term keeps the in-flight cash held.
func sqlOpenOrphanEnvelopeSpendMicros(buyerExpr, currencyExpr string) string {
	return fmt.Sprintf(`
		SELECT COALESCE(SUM(((s.reserved_nanos + 999) / 1000)),0)::bigint
		  FROM execution_contracts c
		  JOIN execution_envelope_spends s ON s.contract_id = c.id
		 WHERE c.buyer_id=%[1]s AND c.currency=%[2]s AND c.state='EXECUTING' AND s.state='RESERVED'
		   AND NOT EXISTS (
		     SELECT 1 FROM execution_envelope_spends s2
		       JOIN execution_envelopes e ON e.id = s2.envelope_id
		      WHERE s2.contract_id=c.id AND e.currency=%[2]s AND e.state='ACTIVE'
		   )`, buyerExpr, currencyExpr)
}

// sqlBuyerOpenExposureMicros is the full canonical open-exposure sum in integer
// micros. prepaidOpenReservationMicros is exactly this expression.
//
// Terms (mutually exclusive by construction):
//  1. prepaid job residual (reserved − prepaid_debit)
//  2. service lease reserved_buyer_micros
//  3. ACTIVE envelope remaining (cap − spent)
//  4. orphaned envelope spends on EXECUTING contracts
func sqlBuyerOpenExposureMicros(buyerExpr, currencyExpr string) string {
	return fmt.Sprintf(`(
		(%s) + (%s) + (%s) + (%s)
	)`,
		sqlOpenPrepaidJobResidualMicros(buyerExpr, currencyExpr),
		sqlOpenServiceLeaseReservedMicros(buyerExpr, currencyExpr),
		sqlOpenActiveEnvelopeRemainingMicros(buyerExpr, currencyExpr),
		sqlOpenOrphanEnvelopeSpendMicros(buyerExpr, currencyExpr),
	)
}

// sqlOpenNonPrepaidJobResidualMicros is a site-specific sibling for free credit
// and realtime. Free-credit / deferred-card batch jobs are not prepaid_required,
// so they are outside the canonical prepaid residual term — but they still
// commit reserved buyer charge against free credit. Prefer reserved residual
// when a plan exists; fall back to estimated micros for plan-less fixtures.
//
// Not embedded in sqlBuyerOpenExposureMicros: prepaid refund/available must
// not treat free-credit jobs as holding prepaid cash.
func sqlOpenNonPrepaidJobResidualMicros(buyerExpr, currencyExpr string) string {
	return fmt.Sprintf(`
		SELECT COALESCE(SUM(
		  CASE
		    WHEN p.job_id IS NOT NULL THEN GREATEST(0::bigint,
		      (p.reserved_buyer_charge_usd * 1000000)::bigint - COALESCE((
		        SELECT SUM((-le.amount_usd * 1000000)::bigint)
		          FROM ledger_entries le
		         WHERE le.kind='prepaid_debit'
		           AND le.currency=%[2]s
		           AND (le.task_id IN (SELECT id FROM tasks WHERE job_id=j.id)
		                OR le.payout_ref='prepaid-sla-' || j.id::text)
		      ), 0))
		    ELSE GREATEST(0::bigint, (COALESCE(j.estimated_usd,0) * 1000000)::bigint)
		  END
		), 0)::bigint
		  FROM jobs j
		  LEFT JOIN job_economic_plans p ON p.job_id=j.id
		 WHERE j.buyer_id=%[1]s AND j.currency=%[2]s AND NOT j.prepaid_required
		   AND j.status IN ('queued','running','verifying')`, buyerExpr, currencyExpr)
}

// sqlOpenNonEnvelopeExecutingCeilingMicros holds EXECUTING realtime ceilings
// that have no envelope spend row at all. Envelope-backed EXECUTING work is
// held exclusively by sqlBuyerOpenExposureMicros (ACTIVE residual or orphan
// spend) — counting ceilings here as well would double-hold.
//
// Hold amount is ceil(accepted_ceiling_nanos → micros), matching admission.
// maximum_price_usd is the to-nearest ledger projection and is only a fallback
// for legacy rows without fixed_point ceilings.
func sqlOpenNonEnvelopeExecutingCeilingMicros(buyerExpr, currencyExpr string) string {
	return fmt.Sprintf(`
		SELECT COALESCE(SUM(
		  CASE
		    WHEN COALESCE((c.pricing_decision #>> '{fixed_point,accepted_ceiling_nanos}')::bigint, 0) > 0
		    THEN (((c.pricing_decision #>> '{fixed_point,accepted_ceiling_nanos}')::bigint + 999) / 1000)
		    ELSE ROUND(c.maximum_price_usd * 1000000)::bigint
		  END
		), 0)::bigint
		  FROM execution_contracts c
		 WHERE c.buyer_id=%[1]s AND c.currency=%[2]s AND c.state='EXECUTING'
		   AND NOT EXISTS (
		     SELECT 1 FROM execution_envelope_spends s WHERE s.contract_id=c.id
		   )`, buyerExpr, currencyExpr)
}

// sqlBuyerCommittedMoneyMicros is open exposure plus the free-credit/realtime
// siblings (non-prepaid job residual + non-envelope EXECUTING). Used by
// evaluateRealtimeBuyerFunding and BuyerFreeCreditRemaining so those rails
// hold everything the canonical authority holds, plus free-credit batch and
// pure-realtime EXECUTING that prepaid open-reservation deliberately leaves
// to the funding-check path.
//
// Non-overlapping composition:
//
//	shared open exposure
//	  = prepaid residual + leases + ACTIVE envelopes + orphan envelope spends
//	+ non-prepaid job residual   (free-credit batch; not prepaid cash)
//	+ non-envelope EXECUTING     (no envelope spend; envelope work is in shared)
func sqlBuyerCommittedMoneyMicros(buyerExpr, currencyExpr string) string {
	return fmt.Sprintf(`(
		%s
		+ (%s)
		+ (%s)
	)`,
		sqlBuyerOpenExposureMicros(buyerExpr, currencyExpr),
		sqlOpenNonPrepaidJobResidualMicros(buyerExpr, currencyExpr),
		sqlOpenNonEnvelopeExecutingCeilingMicros(buyerExpr, currencyExpr),
	)
}
