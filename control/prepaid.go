package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// deferredChargeEnabled restores the pre-prepaid collection model: jobs may
// run against a saved card (or free_credit_usd sandbox grant) and
// chargeOrDeferJob / FormChargeBatch collect after compute is spent.
//
// Rollback: set MERC_DEFERRED_CHARGE=1 (and restart control). Prepaid top-up
// endpoints remain available but job admission no longer requires balance.
func deferredChargeEnabled() bool {
	s := strings.TrimSpace(os.Getenv("MERC_DEFERRED_CHARGE"))
	return s == "1" || strings.EqualFold(s, "true") || strings.EqualFold(s, "yes")
}

// prepaidBalanceRequired is the default collection model: buyers must hold
// prepaid balance before work is accepted. Card-on-file alone is not enough.
func prepaidBalanceRequired() bool {
	return !deferredChargeEnabled()
}

const defaultTopupMinUSD = 25.0

func topupMinUSD() float64 {
	s := strings.TrimSpace(os.Getenv("MERC_TOPUP_MIN_USD"))
	if s == "" {
		return defaultTopupMinUSD
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return defaultTopupMinUSD
	}
	return v
}

type topupRequest struct {
	AmountUSD float64 `json:"amount_usd"`
}

// prepaidTopupOperationKey namespaces the caller's Idempotency-Key under their
// buyer id. Operation keys are a global primary key, so two buyers that both
// pick "1" would otherwise collide on one row: the second would be refused for
// a buyer mismatch, and the refusal would leak that the key is taken.
func prepaidTopupOperationKey(buyerID uuid.UUID, header string) (string, bool) {
	key := strings.TrimSpace(header)
	if key == "" || len(key) > 200 {
		return "", false
	}
	return "topup-" + buyerID.String() + "-" + key, true
}

func (s *Server) handleBillingTopup(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationalControlActive(w, r, controlPayments) {
		return
	}
	if stripeKey() == "" {
		writeErr(w, http.StatusServiceUnavailable, errBillingUnconfigured.Error())
		return
	}
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	// Strict decode like every other money body here: a request carrying a
	// "buyer_id" or a second "amount_usd" must be refused, not silently read as
	// the fields we happen to recognise.
	raw, err := readAndCloseBounded(r.Body, adminActionBodyLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid topup body: "+err.Error())
		return
	}
	var req topupRequest
	if err := decodeStrictJSONObject(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid topup json: "+err.Error())
		return
	}
	minUSD := topupMinUSD()
	if math.IsNaN(req.AmountUSD) || math.IsInf(req.AmountUSD, 0) || req.AmountUSD < minUSD {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("amount_usd must be at least $%.2f (set MERC_TOPUP_MIN_USD to change the floor)", minUSD))
		return
	}
	cents := int64(math.Round(req.AmountUSD * 100))
	if cents <= 0 {
		writeErr(w, http.StatusBadRequest, "amount_usd rounds to a non-positive cent amount")
		return
	}
	opKey, ok := prepaidTopupOperationKey(auth.BuyerID, r.Header.Get("Idempotency-Key"))
	if !ok {
		writeErr(w, http.StatusBadRequest,
			"Idempotency-Key header (1-200 characters) is required so a retried top-up cannot charge the card twice")
		return
	}

	cust, err := ensureStripeCustomer(r.Context(), s.store, auth.BuyerID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	_, pm, err := s.store.GetBillingCustomer(r.Context(), auth.BuyerID)
	if err != nil || strings.TrimSpace(pm) == "" {
		writeErr(w, http.StatusPaymentRequired,
			"save a card via POST /v1/billing/setup before topping up")
		return
	}
	// Arm the durable operation row before any Stripe I/O, the same boundary
	// chargeBuyer crosses in billing.go: whoever wins this insert owns the
	// charge, and every other request for that key is answered from the row.
	arm, err := s.store.BeginPrepaidTopup(r.Context(), opKey, auth.BuyerID, cents)
	if errors.Is(err, errPrepaidTopupConflict) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "recording top-up: "+err.Error())
		return
	}
	switch arm.State {
	case prepaidTopupCredited:
		bal, _ := s.store.BuyerPrepaidBalanceMicros(r.Context(), auth.BuyerID)
		writeJSON(w, http.StatusOK, map[string]any{
			"operation_key":  opKey,
			"payment_intent": arm.PaymentIntent,
			"charge_id":      arm.ChargeID,
			"amount_cents":   cents,
			"currency":       SettlementCurrencyCode(),
			"balance_micros": bal,
			"balance_usd":    microsToUSD(bal),
			"replayed":       true,
		})
		return
	case prepaidTopupInFlight:
		writeErr(w, http.StatusConflict,
			"top-up "+opKey+" already crossed its durable Stripe boundary; check GET /v1/billing/status and retry with a fresh Idempotency-Key")
		return
	}
	charge, err := chargePaymentIntent(r.Context(), cust, pm, cents, SettlementCurrencyCode(), opKey)
	if err != nil {
		log.Printf("billing: top-up charge failed buyer=%s op=%s: %v", auth.BuyerID, opKey, err)
		writeErr(w, http.StatusPaymentRequired, "top-up charge failed: "+err.Error())
		return
	}
	// Credit immediately on synchronous success; webhook is an idempotent safety net.
	if err := s.store.CreditPrepaidTopup(r.Context(), opKey, auth.BuyerID, charge); err != nil {
		log.Printf("billing: top-up credit failed buyer=%s op=%s pi=%s: %v",
			auth.BuyerID, opKey, charge.PaymentIntentID, err)
		writeErr(w, http.StatusInternalServerError, "crediting prepaid balance: "+err.Error())
		return
	}
	bal, _ := s.store.BuyerPrepaidBalanceMicros(r.Context(), auth.BuyerID)
	writeJSON(w, http.StatusOK, map[string]any{
		"operation_key":  opKey,
		"payment_intent": charge.PaymentIntentID,
		"charge_id":      charge.ChargeID,
		"amount_cents":   charge.ReceivedCents,
		"currency":       charge.Currency,
		"balance_micros": bal,
		"balance_usd":    microsToUSD(bal),
	})
}

// handleAdminRefundPrepaid returns a buyer's unreserved prepaid balance to the
// cards that funded it.
//
// Authority is operator, not buyer. A self-serve "refund my whole remainder"
// button is a card-rinse primitive — fund with a stolen card, pull the cash
// back, repeat — and it moves platform cash out over card rails with no reason,
// no correlation reference and no audit row. The other cash-reversing route on
// this surface, POST /admin/realtime/contracts/{id}/refund, is operator-gated
// and audited for the same reason. The buyer's own money-back seam is
// POST /v1/jobs/{id}/dispute, which is bounded to one job's charge.
func (s *Server) handleAdminRefundPrepaid(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationalControlActive(w, r, controlPayments) {
		return
	}
	if stripeKey() == "" {
		writeErr(w, http.StatusServiceUnavailable, errBillingUnconfigured.Error())
		return
	}
	buyerID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	body, err := decodeAdminActionBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json: "+err.Error())
		return
	}
	actor, ok := adminActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authenticated admin identity is required")
		return
	}
	result, err := s.refundPrepaidRemainder(r.Context(), actor, buyerID, body.Reason, body.RequestID)
	if errors.Is(err, errInsufficientPrepaid) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if writeAdminMutationInputOrAuthError(w, err) {
		return
	}
	if err != nil {
		log.Printf("billing: prepaid refund failed buyer=%s ref=%s: %v", buyerID, body.RequestID, err)
		writeErr(w, http.StatusInternalServerError, "refunding prepaid balance: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// refundPrepaidRemainder debits the buyer's *unreserved* balance and records the
// operation durably, then asks Stripe to move the cash.
//
// Both orderings are load-bearing, and the first version had them backwards:
//
//   - The refundable amount is available balance, not gross balance. Refunding
//     gross while jobs are queued or running strips the reservation those jobs
//     were admitted against, so the compute completes and settlement can never
//     debit it — the buyer gets the work and the money.
//   - The debit and the operation row are committed before the provider call. A
//     crash after a Stripe refund but before the ledger write used to hand the
//     cash back while leaving the identical balance spendable.
//
// A provider failure therefore leaves a 'pending' operation, never spendable
// money: the operator replays the same request_id to finish the transfer.
func (s *Server) refundPrepaidRemainder(
	ctx context.Context,
	actor AdminActor,
	buyerID uuid.UUID,
	reason, correlationRef string,
) (map[string]any, error) {
	plan, err := s.store.BeginPrepaidRefund(ctx, actor, buyerID, reason, correlationRef)
	if err != nil {
		return nil, err
	}
	refundIDs := make([]string, 0, len(plan.Slices))
	for i, slice := range plan.Slices {
		if slice.RefundID == "" {
			// Deterministic per-slice key: a replay of this operation makes Stripe
			// return the original refund rather than sending the cash a second time.
			id, err := stripeCreateRefund(ctx, slice.PaymentIntent, slice.Cents,
				plan.OperationKey+"-"+slice.PaymentIntent)
			if err != nil {
				return nil, fmt.Errorf("refund %s is recorded but not transferred for %s; replay the same request_id: %w",
					plan.OperationKey, slice.PaymentIntent, err)
			}
			plan.Slices[i].RefundID = id
		}
		refundIDs = append(refundIDs, plan.Slices[i].RefundID)
	}
	if err := s.store.CompletePrepaidRefund(ctx, plan.Slices); err != nil {
		return nil, err
	}
	newBal, _ := s.store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	return map[string]any{
		"operation_key":   plan.OperationKey,
		"buyer_id":        buyerID,
		"refunded_cents":  plan.Cents,
		"refunded_micros": plan.Cents * microUSDPerCent,
		"stripe_refunds":  refundIDs,
		"balance_micros":  newBal,
		"balance_usd":     microsToUSD(newBal),
		"replayed":        plan.Replayed,
	}, nil
}

func stripeCreateRefund(ctx context.Context, paymentIntent string, cents int64, idemKey string) (string, error) {
	if cents <= 0 || strings.TrimSpace(paymentIntent) == "" {
		return "", fmt.Errorf("invalid refund request")
	}
	form := url.Values{
		"payment_intent": {paymentIntent},
		"amount":         {strconv.FormatInt(cents, 10)},
	}
	out, err := stripeForm(ctx, "refunds", form, idemKey)
	if err != nil {
		return "", err
	}
	id, _ := out["id"].(string)
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("stripe refund: no id in response")
	}
	status, _ := out["status"].(string)
	if status != "succeeded" && status != "pending" {
		return "", fmt.Errorf("stripe refund %s status %q", id, status)
	}
	return id, nil
}

// reconcilePrepaidTopup is the webhook path for payment_intent.succeeded when
// the PI's operation key is a prepaid top-up.
func (s *Store) reconcilePrepaidTopup(ctx context.Context, operationKey string, charge ChargeResult) error {
	buyerID, amountCents, err := s.PrepaidTopupPending(ctx, operationKey)
	if err != nil {
		return err
	}
	if charge.RequestedCents != amountCents || charge.ReceivedCents != amountCents {
		return fmt.Errorf("top-up %s amount mismatch: op=%d charge=%d/%d",
			operationKey, amountCents, charge.RequestedCents, charge.ReceivedCents)
	}
	return s.CreditPrepaidTopup(ctx, operationKey, buyerID, charge)
}
