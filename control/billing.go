package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var errBillingUnconfigured = fmt.Errorf("billing is not configured (set STRIPE_SECRET_KEY)  -  no charge is made or faked")

var (
	stripeAPIBaseURL = "https://api.stripe.com/v1"
	stripeHTTPClient = &http.Client{Timeout: 20 * time.Second}
)

const stripeAPIResponseMaxBytes int64 = 2 << 20

func stripeKey() string { return os.Getenv("STRIPE_SECRET_KEY") }

func stripeForm(ctx context.Context, path string, form url.Values, idemKey string) (map[string]any, error) {
	key := stripeKey()
	if key == "" {
		return nil, errBillingUnconfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(stripeAPIBaseURL, "/")+"/"+strings.TrimLeft(path, "/"), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := stripeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := readBoundedRemoteBody(resp.Body, stripeAPIResponseMaxBytes)
	if readErr != nil {
		return nil, fmt.Errorf("stripe %s response read: %w", path, readErr)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("stripe %s (%d): %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("stripe %s: unparseable response", path)
	}
	return out, nil
}

func ensureStripeCustomer(ctx context.Context, store *Store, buyerID uuid.UUID) (string, error) {
	if cust, _, err := store.GetBillingCustomer(ctx, buyerID); err == nil && cust != "" {
		return cust, nil
	}
	out, err := stripeForm(ctx, "customers", url.Values{"metadata[buyer_id]": {buyerID.String()}}, "")
	if err != nil {
		return "", err
	}
	cust, _ := out["id"].(string)
	if cust == "" {
		return "", fmt.Errorf("stripe customer: no id in response")
	}
	if err := store.UpsertBillingCustomer(ctx, buyerID, cust); err != nil {
		return "", err
	}
	return cust, nil
}

func setupIntent(ctx context.Context, store *Store, buyerID uuid.UUID) (string, error) {
	cust, err := ensureStripeCustomer(ctx, store, buyerID)
	if err != nil {
		return "", err
	}
	out, err := stripeForm(ctx, "setup_intents", url.Values{"customer": {cust}, "payment_method_types[]": {"card"}}, "")
	if err != nil {
		return "", err
	}
	cs, _ := out["client_secret"].(string)
	if cs == "" {
		return "", fmt.Errorf("stripe setup_intent: no client_secret in response")
	}
	return cs, nil
}

type ChargeResult struct {
	PaymentIntentID string
	ChargeID        string
	RequestedCents  int64
	ReceivedCents   int64
	Currency        string
}

func stripeIntegerField(out map[string]any, field string) (int64, error) {
	v, ok := out[field].(float64)
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || math.Trunc(v) != v || v > math.MaxInt64 {
		return 0, fmt.Errorf("payment intent: %s must be a non-negative integer", field)
	}
	return int64(v), nil
}

func chargePaymentIntent(ctx context.Context, customer, paymentMethod string, cents int64, currency, idemKey string) (ChargeResult, error) {
	if cents <= 0 {
		return ChargeResult{}, fmt.Errorf("non-positive charge amount %d cents", cents)
	}
	if currency == "" {
		return ChargeResult{}, fmt.Errorf("charge currency is required")
	}
	form := url.Values{
		"amount":                     {strconv.FormatInt(cents, 10)},
		"currency":                   {currency},
		"customer":                   {customer},
		"confirm":                    {"true"},
		"off_session":                {"true"},
		"expand[]":                   {"latest_charge"},
		"metadata[cx_operation_key]": {idemKey},
	}
	if paymentMethod != "" {
		form.Set("payment_method", paymentMethod)
	}
	out, err := stripeForm(ctx, "payment_intents", form, idemKey)
	if err != nil {
		return ChargeResult{}, err
	}
	if raw, exists := out["error"]; exists && raw != nil {
		return ChargeResult{}, fmt.Errorf("payment intent returned an error-shaped 2xx response: %v", raw)
	}
	id, _ := out["id"].(string)
	if strings.TrimSpace(id) == "" {
		return ChargeResult{}, fmt.Errorf("payment intent: successful response has no id")
	}
	chargeID := ""
	switch latest := out["latest_charge"].(type) {
	case string:
		chargeID = strings.TrimSpace(latest)
	case map[string]any:
		chargeID, _ = latest["id"].(string)
		chargeID = strings.TrimSpace(chargeID)
	}
	if chargeID == "" {
		return ChargeResult{}, fmt.Errorf("payment intent %s: successful response has no latest charge id", id)
	}
	status, _ := out["status"].(string)
	if status != "succeeded" {
		if status == "" {
			status = "missing"
		}
		return ChargeResult{}, fmt.Errorf("payment intent %s is %s, not succeeded", id, status)
	}
	gotCurrency, _ := out["currency"].(string)
	if gotCurrency != currency {
		return ChargeResult{}, fmt.Errorf("payment intent %s currency %q does not match requested %q", id, gotCurrency, currency)
	}
	requested, err := stripeIntegerField(out, "amount")
	if err != nil {
		return ChargeResult{}, err
	}
	received, err := stripeIntegerField(out, "amount_received")
	if err != nil {
		return ChargeResult{}, err
	}
	if requested != cents || received != cents {
		return ChargeResult{}, fmt.Errorf(
			"payment intent %s amount mismatch: requested=%d response_amount=%d amount_received=%d",
			id, cents, requested, received)
	}
	return ChargeResult{
		PaymentIntentID: id,
		ChargeID:        chargeID,
		RequestedCents:  cents,
		ReceivedCents:   received,
		Currency:        currency,
	}, nil
}

func parseStripeSucceededPaymentIntent(object json.RawMessage) (string, ChargeResult, bool, error) {
	var pi struct {
		ID             string            `json:"id"`
		LatestCharge   json.RawMessage   `json:"latest_charge"`
		Status         string            `json:"status"`
		Amount         int64             `json:"amount"`
		AmountReceived int64             `json:"amount_received"`
		Currency       string            `json:"currency"`
		Metadata       map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(object, &pi); err != nil {
		return "", ChargeResult{}, false, err
	}
	operationKey := strings.TrimSpace(pi.Metadata["cx_operation_key"])
	if operationKey == "" {
		return "", ChargeResult{}, false, nil
	}
	chargeID, err := stripeExpandableID(pi.LatestCharge)
	if err != nil {
		return "", ChargeResult{}, true, err
	}
	pi.ID, chargeID = strings.TrimSpace(pi.ID), strings.TrimSpace(chargeID)
	if pi.ID == "" || chargeID == "" || pi.Status != "succeeded" ||
		pi.Amount <= 0 || pi.AmountReceived != pi.Amount || pi.Currency != "usd" {
		return "", ChargeResult{}, true, errors.New("owned successful PaymentIntent has invalid cash evidence")
	}
	return operationKey, ChargeResult{
		PaymentIntentID: pi.ID, ChargeID: chargeID, RequestedCents: pi.Amount,
		ReceivedCents: pi.AmountReceived, Currency: pi.Currency,
	}, true, nil
}

func chargeBuyer(
	ctx context.Context,
	store *Store,
	buyerID uuid.UUID,
	usd float64,
	idemKey, sourceKind string,
	sourceID uuid.UUID,
) (ChargeResult, error) {
	cents := int64(math.Round(usd * 100))
	if cents <= 0 {
		return ChargeResult{}, fmt.Errorf("non-positive charge amount %.6f USD", usd)
	}
	cust, err := ensureStripeCustomer(ctx, store, buyerID)
	if err != nil {
		return ChargeResult{}, err
	}
	_, pm, err := store.GetBillingCustomer(ctx, buyerID)
	if err != nil || strings.TrimSpace(pm) == "" {
		return ChargeResult{}, fmt.Errorf("buyer has no saved payment method")
	}
	armed, err := store.BeginBuyerChargeOperation(
		ctx, idemKey, sourceKind, sourceID, buyerID, cust, pm, cents, "usd",
	)
	if err != nil {
		return ChargeResult{}, err
	}
	if !armed {
		return ChargeResult{}, fmt.Errorf("%w: operation %s already crossed its durable request boundary",
			errBuyerChargeOutcomeUnknown, idemKey)
	}
	charge, err := chargePaymentIntent(ctx, cust, pm, cents, "usd", idemKey)
	if err != nil {
		_ = store.NoteBuyerChargeOutcomeUnknown(ctx, idemKey, err)
		return ChargeResult{}, fmt.Errorf("%w: operation %s requires Stripe reconciliation: %v",
			errBuyerChargeOutcomeUnknown, idemKey, err)
	}
	return charge, nil
}

func (s *Server) handleBillingSetup(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	cs, err := setupIntent(r.Context(), s.store, auth.BuyerID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"client_secret": cs})
}

func (s *Server) handleBillingStatus(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	cust, pm, err := s.store.GetBillingCustomer(r.Context(), auth.BuyerID)
	if err != nil && !errors.Is(err, errNotFound) {
		writeErr(w, http.StatusInternalServerError, "reading billing status")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": stripeKey() != "",
		"connected":  err == nil && cust != "",
		"has_card":   pm != "",
	})
}

const stripeSigTolerance = 5 * time.Minute

func verifyStripeSig(payload []byte, sigHeader, secret string) bool {
	return verifyStripeSigAt(payload, sigHeader, secret, time.Now())
}

func verifyStripeSigAt(payload []byte, sigHeader, secret string, now time.Time) bool {
	var t string
	var v1s []string
	for _, part := range strings.Split(sigHeader, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if kv[0] == "t" {
			t = kv[1]
		} else if kv[0] == "v1" {
			v1s = append(v1s, kv[1])
		}
	}
	if t == "" || len(v1s) == 0 {
		return false
	}
	tsSecs, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return false
	}
	age := now.Sub(time.Unix(tsSecs, 0))
	if age < 0 {
		age = -age
	}
	if age > stripeSigTolerance {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "." + string(payload)))
	expected := []byte(hex.EncodeToString(mac.Sum(nil)))
	valid := false
	for _, candidate := range v1s {
		if hmac.Equal(expected, []byte(candidate)) {
			valid = true
		}
	}
	return valid
}

type billingPMSetter func(context.Context, string, string) error
type stripeCashEventApplier func(context.Context, stripeCashEvent) (stripeCashEventResult, error)
type buyerChargeReconciler func(context.Context, string, ChargeResult) error

func handleStripeWebhookWithSetter(
	w http.ResponseWriter,
	r *http.Request,
	secret string,
	setPM billingPMSetter,
) {
	handleStripeWebhookWithHandlers(w, r, secret, setPM, nil)
}

func handleStripeWebhookWithHandlers(
	w http.ResponseWriter,
	r *http.Request,
	secret string,
	setPM billingPMSetter,
	applyCashEvent stripeCashEventApplier,
) {
	handleStripeWebhookWithAllHandlers(w, r, secret, setPM, applyCashEvent, nil)
}

func handleStripeWebhookWithAllHandlers(
	w http.ResponseWriter,
	r *http.Request,
	secret string,
	setPM billingPMSetter,
	applyCashEvent stripeCashEventApplier,
	reconcileCharge buyerChargeReconciler,
) {
	payload, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if !verifyStripeSig(payload, r.Header.Get("Stripe-Signature"), secret) {
		writeErr(w, http.StatusBadRequest, "invalid stripe signature")
		return
	}
	var ev struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Created int64  `json:"created"`
		Data    struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		writeErr(w, http.StatusBadRequest, "unparseable webhook body")
		return
	}
	switch ev.Type {
	case "setup_intent.succeeded", "payment_method.attached":
		var obj map[string]any
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			writeErr(w, http.StatusBadRequest, "unparseable webhook object")
			return
		}
		cust, _ := obj["customer"].(string)
		pm, _ := obj["payment_method"].(string)
		if pm == "" {
			pm, _ = obj["id"].(string) // payment_method.attached: the object IS the PM
		}
		if cust != "" && pm != "" {
			if err := setPM(r.Context(), cust, pm); err != nil {
				log.Printf("billing webhook: saved payment-method update failed: %v", err)
				writeErr(w, http.StatusInternalServerError, "updating saved payment method")
				return
			}
		}
	case "payment_intent.succeeded":
		operationKey, charge, owned, err := parseStripeSucceededPaymentIntent(ev.Data.Object)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid successful PaymentIntent event")
			return
		}
		if owned {
			if reconcileCharge == nil {
				writeErr(w, http.StatusInternalServerError, "buyer charge reconciliation unavailable")
				return
			}
			if err := reconcileCharge(r.Context(), operationKey, charge); err != nil {
				log.Printf("billing webhook: buyer charge reconciliation failed operation=%s pi=%s: %v",
					operationKey, charge.PaymentIntentID, err)
				writeErr(w, http.StatusInternalServerError, "reconciling successful buyer charge")
				return
			}
		}
	default:
		if isStripeCashEventType(ev.Type) {
			cashEvent, err := parseStripeCashEvent(ev.ID, ev.Type, ev.Created, ev.Data.Object, payload)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "invalid Stripe cash event")
				return
			}
			if applyCashEvent == nil {
				writeErr(w, http.StatusInternalServerError, "Stripe cash-event handler unavailable")
				return
			}
			result, err := applyCashEvent(r.Context(), cashEvent)
			if err != nil {
				log.Printf("billing webhook: cash event apply failed type=%s event=%s: %v", ev.Type, ev.ID, err)
				writeErr(w, http.StatusInternalServerError, "applying Stripe cash event")
				return
			}
			if result.CompromisedFundingRows > 0 || result.ReversalRequiredRows > 0 {
				log.Printf("billing webhook: funding compromised type=%s event=%s funding_rows=%d reversal_rows=%d unavailable_cents=%d",
					ev.Type, ev.ID, result.CompromisedFundingRows, result.ReversalRequiredRows, result.UnavailableCents)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	if secret == "" {
		writeErr(w, http.StatusServiceUnavailable, "stripe webhooks not configured (set STRIPE_WEBHOOK_SECRET)")
		return
	}
	handleStripeWebhookWithAllHandlers(
		w, r, secret, s.store.SetBillingPMByCustomer, s.store.ApplyPaymentEventTx,
		s.store.ReconcileBuyerChargeOperation,
	)
}

func (s *Server) chargeForJob(ctx context.Context, jobID uuid.UUID) {
	chargeOrDeferJob(ctx, s.store, jobID)
}

func chargeOrDeferJob(ctx context.Context, store *Store, jobID uuid.UUID) {
	if stripeKey() == "" {
		return
	}
	buyerID, usd, err := store.JobChargeInfo(ctx, jobID)
	if err != nil || usd <= 0 {
		return
	}
	if st, serr := store.JobChargeStatus(ctx, jobID); serr != nil || st != "not_attempted" {
		return
	}
	if shouldDeferCharge(usd, chargeMinUSD()) {
		if _, derr := store.MarkJobDeferred(ctx, jobID); derr != nil {
			log.Printf("billing: deferring sub-threshold charge for job %s: %v (stays owed in the ledger)", jobID, derr)
		}
		return
	}
	cust, pm, err := store.GetBillingCustomer(ctx, buyerID)
	if err != nil || cust == "" || pm == "" {
		_ = store.SetChargeStatus(ctx, jobID, "no_payment_method")
		_ = store.InsertJobEvent(ctx, jobID, nil, "charge_failed",
			"Job complete but no saved payment method · amount is owed and will be charged once a card is on file", nil)
		return // no saved card -> nothing to charge off-session (still owed in the ledger)
	}
	if ferr := store.FreezeChargeAmount(ctx, jobID, usd); ferr != nil {
		log.Printf("billing: freezing charge amount for job %s: %v (charge deferred to the sweep)", jobID, ferr)
		return
	}
	charge, err := chargeBuyer(ctx, store, buyerID, usd, "job-"+jobID.String(), "job", jobID)
	if err != nil {
		if !errors.Is(err, errBuyerChargeOutcomeUnknown) {
			_ = store.SetChargeStatus(ctx, jobID, "failed")
		}
		_ = store.InsertJobEvent(ctx, jobID, nil, "charge_failed",
			"Charge for this job failed · amount is owed and will be reconciled", nil)
		log.Printf("billing: charge for job %s failed or is outcome_unknown (owed, will reconcile without a blind retry): %v", jobID, err)
		return
	}
	if serr := store.SetJobCharged(ctx, jobID, charge); serr != nil {
		log.Printf("billing: marking job %s charged (pi %s): %v", jobID, charge.PaymentIntentID, serr)
		return
	}
	if ferr := recordStripeFee(ctx, store, buyerID, charge.PaymentIntentID); ferr != nil {
		log.Printf("billing: stripe fee for job %s (pi %s) not recorded yet: %v (backfilled by the charge-collect sweep)", jobID, charge.PaymentIntentID, ferr)
	}
}

func recordStripeFee(ctx context.Context, store *Store, buyerID uuid.UUID, pi string) error {
	if pi == "" {
		return fmt.Errorf("no payment intent id to fetch a fee for")
	}
	out, err := stripeGet(ctx, "payment_intents/"+pi+"?expand[]=latest_charge.balance_transaction")
	if err != nil {
		return err
	}
	lc, _ := out["latest_charge"].(map[string]any)
	bt, _ := lc["balance_transaction"].(map[string]any)
	feeCents, ok := bt["fee"].(float64) // JSON number; Stripe fees are integer cents
	if !ok {
		return fmt.Errorf("payment intent %s: latest_charge.balance_transaction.fee absent (not settled yet?)  -  fee not recorded, never estimated", pi)
	}
	if err := store.InsertStripeFee(ctx, buyerID, pi, feeCents/100); err != nil {
		return err
	}
	if _, err := store.AllocateBatchStripeFee(ctx, pi); err != nil {
		return fmt.Errorf("stripe fee recorded for %s but batch allocation is pending: %w", pi, err)
	}
	return nil
}

func stripeGet(ctx context.Context, path string) (map[string]any, error) {
	key := stripeKey()
	if key == "" {
		return nil, errBillingUnconfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(stripeAPIBaseURL, "/")+"/"+strings.TrimLeft(path, "/"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := stripeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := readBoundedRemoteBody(resp.Body, stripeAPIResponseMaxBytes)
	if readErr != nil {
		return nil, fmt.Errorf("stripe GET %s response read: %w", path, readErr)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("stripe GET %s (%d): %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("stripe GET %s: unparseable response", path)
	}
	return out, nil
}
