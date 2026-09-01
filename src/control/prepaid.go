package main

import (
	"context"
	"errors"
	"fmt"
	"log"
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

const defaultTopupMinMajor = "25"

// topupMinMinor is deliberately configured in integer Stripe minor units. A
// deployment that says "USD" while settling CAD is not merely mislabeled: it
// changes the commercial meaning of buyer money. Refuse the retired
// MERC_TOPUP_MIN_USD setting instead of reinterpreting it under another
// currency or quietly falling back to a floor the operator did not choose.
func topupMinMinor(currency Currency) (int64, error) {
	if strings.TrimSpace(os.Getenv("MERC_TOPUP_MIN_USD")) != "" {
		return 0, errors.New("MERC_TOPUP_MIN_USD is obsolete; set MERC_TOPUP_MIN_MINOR in integer settlement minor units")
	}
	raw := strings.TrimSpace(os.Getenv("MERC_TOPUP_MIN_MINOR"))
	if raw == "" {
		return currency.ParseMajorToMinorExact(defaultTopupMinMajor)
	}
	for _, digit := range raw {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("MERC_TOPUP_MIN_MINOR must be a positive integer, got %q", raw)
		}
	}
	minor, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || minor <= 0 {
		return 0, fmt.Errorf("MERC_TOPUP_MIN_MINOR must be a positive integer, got %q", raw)
	}
	return minor, nil
}

func formatMinorAmount(minor int64, currency Currency) string {
	factor, err := currency.MinorUnitsPerMajor()
	if err != nil || factor <= 0 {
		return strconv.FormatInt(minor, 10)
	}
	whole, fraction := minor/factor, minor%factor
	if currency.Exponent() == 0 {
		return strconv.FormatInt(whole, 10)
	}
	return fmt.Sprintf("%d.%0*d", whole, currency.Exponent(), fraction)
}

type topupRequest struct {
	AmountMajor string `json:"amount_major"`
	Currency    string `json:"currency"`
}

// prepaidTopupBodyLimit bounds the buyer-facing top-up body. It is deliberately
// not adminActionBodyLimit: that bound exists for operator mutation bodies with
// a free-text reason, and borrowing it means retuning the operator surface
// silently retunes what an unprivileged buyer may push at the money path. This
// body carries one number.
const prepaidTopupBodyLimit = 1 << 10

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
	// "buyer_id" or a second "amount_major" must be refused, not silently read as
	// the fields we happen to recognise.
	raw, err := readAndCloseBounded(r.Body, prepaidTopupBodyLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid topup body: "+err.Error())
		return
	}
	var req topupRequest
	if err := decodeStrictJSONObject(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid topup json: "+err.Error())
		return
	}
	settlement, err := SettlementCurrency()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "settlement currency is not configured")
		return
	}
	requestedCurrency, err := ParseCurrency(req.Currency)
	if err != nil || !requestedCurrency.Equal(settlement) {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("currency must be the configured settlement currency %s", settlement.Code()))
		return
	}
	minorUnits, err := settlement.ParseMajorToMinorExact(req.AmountMajor)
	if err != nil || minorUnits <= 0 {
		writeErr(w, http.StatusBadRequest, "amount_major must be a positive exact decimal in "+settlement.Code())
		return
	}
	minimumMinor, err := topupMinMinor(settlement)
	if err != nil {
		log.Printf("billing: invalid top-up floor configuration: %v", err)
		writeErr(w, http.StatusServiceUnavailable, "top-up floor configuration is invalid")
		return
	}
	if minorUnits < minimumMinor {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"amount_major must be at least %s %s (set MERC_TOPUP_MIN_MINOR in integer minor units to change the floor)",
			formatMinorAmount(minimumMinor, settlement), settlement.Code()))
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
	arm, err := s.store.BeginPrepaidTopup(r.Context(), opKey, auth.BuyerID, minorUnits)
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
			"operation_key":      opKey,
			"payment_intent":     arm.PaymentIntent,
			"charge_id":          arm.ChargeID,
			"amount_minor_units": minorUnits,
			"currency":           settlement.Code(),
			"balance_micros":     bal,
			"replayed":           true,
		})
		return
	case prepaidTopupInFlight:
		writeErr(w, http.StatusConflict,
			"top-up "+opKey+" already crossed its durable Stripe boundary; GET /v1/billing/status reports the credited balance and any top-up still pending, so retry with a fresh Idempotency-Key only once this one is no longer pending there")
		return
	}
	charge, err := chargePaymentIntent(r.Context(), cust, pm, minorUnits, settlement.Code(), opKey)
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
		"operation_key":      opKey,
		"payment_intent":     charge.PaymentIntentID,
		"charge_id":          charge.ChargeID,
		"amount_minor_units": charge.ReceivedCents,
		"currency":           charge.Currency,
		"balance_micros":     bal,
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
			id, err := stripeCreateRefund(ctx, slice.PaymentIntent, slice.Cents, plan.Currency,
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
	settlement, err := ParseCurrency(plan.Currency)
	if err != nil {
		return nil, err
	}
	refundedMicros, err := settlement.MinorToMicros(plan.Cents)
	if err != nil {
		return nil, err
	}
	newBal, _ := s.store.buyerPrepaidBalanceMicrosInCurrency(ctx, buyerID, plan.Currency)
	return map[string]any{
		"operation_key":        plan.OperationKey,
		"buyer_id":             buyerID,
		"refunded_minor_units": plan.Cents,
		"refunded_micros":      refundedMicros,
		"currency":             settlement.Code(),
		"stripe_refunds":       refundIDs,
		"balance_micros":       newBal,
		"replayed":             plan.Replayed,
	}, nil
}

func stripeCreateRefund(
	ctx context.Context, paymentIntent string, minorUnits int64, currency, idemKey string,
) (string, error) {
	paymentIntent = strings.TrimSpace(paymentIntent)
	currency = strings.TrimSpace(currency)
	if minorUnits <= 0 || !validStripeObjectID(paymentIntent, "pi_") {
		return "", fmt.Errorf("invalid refund request")
	}
	if _, err := ParseCurrency(currency); err != nil {
		return "", fmt.Errorf("invalid refund currency: %w", err)
	}
	form := url.Values{
		"payment_intent": {paymentIntent},
		"amount":         {strconv.FormatInt(minorUnits, 10)},
	}
	out, err := stripeForm(ctx, "refunds", form, idemKey)
	if err != nil {
		return "", err
	}
	return parseStripePrepaidRefundResponse(out, paymentIntent, minorUnits, currency)
}

func parseStripePrepaidRefundResponse(
	out map[string]any, paymentIntent string, minorUnits int64, currency string,
) (string, error) {
	paymentIntent = strings.TrimSpace(paymentIntent)
	wantCurrency, err := ParseCurrency(currency)
	if minorUnits <= 0 || !validStripeObjectID(paymentIntent, "pi_") || err != nil {
		return "", fmt.Errorf("stripe refund response does not match the requested durable slice")
	}
	id, err := parseStripeProviderObjectID(out, "refund", "refund", "re_")
	if err != nil {
		return "", err
	}
	amount, err := stripeIntegerField(out, "amount")
	if err != nil {
		return "", fmt.Errorf("stripe refund %s has an invalid amount: %w", id, err)
	}
	if amount != minorUnits {
		return "", fmt.Errorf("stripe refund %s amount %d does not match requested %d", id, amount, minorUnits)
	}
	returnedCurrency, _ := out["currency"].(string)
	gotCurrency, err := ParseCurrency(returnedCurrency)
	if err != nil || !gotCurrency.Equal(wantCurrency) {
		return "", fmt.Errorf("stripe refund %s currency %q does not match requested %q", id, returnedCurrency, wantCurrency.Code())
	}
	returnedPI, err := stripeExpandableMapID(out, "payment_intent", "payment_intent")
	if err != nil || strings.TrimSpace(returnedPI) != paymentIntent {
		return "", fmt.Errorf("stripe refund %s PaymentIntent does not match requested %s", id, paymentIntent)
	}
	status, _ := out["status"].(string)
	if status != "succeeded" {
		return "", fmt.Errorf("stripe refund %s is not complete (status %q); leave the durable slice pending", id, status)
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
