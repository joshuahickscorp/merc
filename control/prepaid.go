package main

import (
	"context"
	"encoding/json"
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

func (s *Server) handleBillingTopup(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationalControlActive(w, r, controlPayments) {
		return
	}
	if stripeKey() == "" {
		writeErr(w, http.StatusServiceUnavailable, errBillingUnconfigured.Error())
		return
	}
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	var req topupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	// Stable per-request operation key when client supplies Idempotency-Key;
	// otherwise a fresh key (Stripe still de-dupes within its window on PI create).
	opKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if opKey == "" {
		opKey = "topup-" + uuid.NewString()
	} else if !strings.HasPrefix(opKey, "topup-") {
		opKey = "topup-" + opKey
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
	if err := s.store.BeginPrepaidTopup(r.Context(), opKey, auth.BuyerID, cents); err != nil {
		writeErr(w, http.StatusInternalServerError, "recording top-up: "+err.Error())
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

func (s *Server) handleBillingRefund(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationalControlActive(w, r, controlPayments) {
		return
	}
	if stripeKey() == "" {
		writeErr(w, http.StatusServiceUnavailable, errBillingUnconfigured.Error())
		return
	}
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	// Full unspent refund (remainder). Optional amount_usd may be added later.
	result, err := s.refundPrepaidRemainder(r.Context(), auth.BuyerID)
	if err != nil {
		if errors.Is(err, errInsufficientPrepaid) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		log.Printf("billing: prepaid refund failed buyer=%s: %v", auth.BuyerID, err)
		writeErr(w, http.StatusInternalServerError, "refunding prepaid balance: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) refundPrepaidRemainder(ctx context.Context, buyerID uuid.UUID) (map[string]any, error) {
	bal, err := s.store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	if err != nil {
		return nil, err
	}
	if bal <= 0 {
		return nil, errInsufficientPrepaid
	}
	// Stripe refunds are integer cents; floor leftover micro-USD below 1¢ stays
	// as dust on the balance until another top-up/debit absorbs it.
	cents := bal / microUSDPerCent
	if cents <= 0 {
		return nil, fmt.Errorf("prepaid balance %d micro-USD is below one cent and cannot be refunded via card rails", bal)
	}
	refundMicros := cents * microUSDPerCent
	topups, err := s.store.ListRefundableTopups(ctx, buyerID)
	if err != nil {
		return nil, err
	}
	remainingCents := cents
	var refundIDs []string
	for _, t := range topups {
		if remainingCents <= 0 {
			break
		}
		slice := t.AmountCents
		if slice > remainingCents {
			slice = remainingCents
		}
		refID, err := stripeCreateRefund(ctx, t.PaymentIntent, slice, "prepaid-refund-"+buyerID.String()+"-"+t.PaymentIntent)
		if err != nil {
			return nil, err
		}
		refundIDs = append(refundIDs, refID)
		remainingCents -= slice
	}
	if remainingCents > 0 {
		return nil, fmt.Errorf("could not cover full refund: %d cents remain after top-up refunds", remainingCents)
	}
	opKey := "prepaid-refund-" + uuid.NewString()
	if err := s.store.DebitPrepaidRefund(ctx, opKey, buyerID, refundMicros, strings.Join(refundIDs, ",")); err != nil {
		return nil, err
	}
	newBal, _ := s.store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	return map[string]any{
		"operation_key":   opKey,
		"refunded_cents":  cents,
		"refunded_micros": refundMicros,
		"stripe_refunds":  refundIDs,
		"balance_micros":  newBal,
		"balance_usd":     microsToUSD(newBal),
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
