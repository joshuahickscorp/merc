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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var errBillingUnconfigured = fmt.Errorf("billing is not configured (set STRIPE_SECRET_KEY)  -  no charge is made or faked")

var (
	stripeAPIBaseURL = "https://api.stripe.com/v1"
	stripeHTTPClient = newStripeHTTPClient()
)

const stripeAPIResponseMaxBytes int64 = 2 << 20

const stripeWebhookPayloadMaxBytes int64 = 1 << 20

const stripeMaxIdleConnsPerHost = 16

func newStripeHTTPClient() *http.Client {
	// Payment flows can issue a short burst of customer, setup-intent, payment,
	// and refund calls. Keep enough authenticated HTTPS connections warm for
	// that burst without changing the standard transport's proxy/TLS behavior.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = stripeMaxIdleConnsPerHost
	transport.MaxIdleConns = stripeMaxIdleConnsPerHost * 4
	return &http.Client{Transport: transport, Timeout: 20 * time.Second}
}

var errRemoteResponseTooLarge = errors.New("remote response exceeds configured size limit")

// stripeRequestError preserves enough provider response metadata for callers
// to distinguish an explicit request rejection from an ambiguous provider or
// transport failure. A 409, timeout, or 5xx remains ambiguous because Stripe
// may have accepted the idempotent request before returning the error.
type stripeRequestError struct {
	path       string
	statusCode int
	detail     string
}

func (e *stripeRequestError) Error() string {
	return fmt.Sprintf("stripe %s (%d): %s", e.path, e.statusCode, e.detail)
}

func (e *stripeRequestError) definitelyNotSent() bool {
	if e == nil || e.statusCode < http.StatusBadRequest || e.statusCode >= http.StatusInternalServerError {
		return false
	}
	switch e.statusCode {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return false
	default:
		return true
	}
}

func readBoundedRemoteBody(r io.Reader, maxBytes int64) ([]byte, error) {
	if r == nil {
		return nil, errors.New("remote response body is nil")
	}
	if maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return nil, errors.New("remote response size limit is invalid")
	}
	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, errRemoteResponseTooLarge
	}
	return body, nil
}

func readStripeWebhookPayload(r io.Reader) ([]byte, error) {
	return readBoundedRemoteBody(r, stripeWebhookPayloadMaxBytes)
}

func configuredStripeKey() (string, error) {
	path := strings.TrimSpace(os.Getenv(stripeSecretKeyFileEnv))
	if path == "" {
		return strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY")), nil
	}
	if strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY")) != "" {
		return "", fmt.Errorf("%s and STRIPE_SECRET_KEY cannot both be set", stripeSecretKeyFileEnv)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s must be an absolute path", stripeSecretKeyFileEnv)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat Stripe secret key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o027 != 0 {
		return "", errors.New("Stripe secret key must be a regular file, not group-writable, and inaccessible to other users")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Stripe secret key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" || len(key) > maxPaymentProviderSecretBytes {
		return "", fmt.Errorf("Stripe secret key file must contain 1..%d bytes", maxPaymentProviderSecretBytes)
	}
	return key, nil
}

// stripeKey is used only at operation boundaries that immediately call the
// authority loader, which re-reads and validates the configured secret file.
// Startup uses configuredStripeKey directly so file errors are never hidden.
func stripeKey() string {
	key, _ := configuredStripeKey()
	return key
}

func validStripeObjectID(value, prefix string) bool {
	value, prefix = strings.TrimSpace(value), strings.TrimSpace(prefix)
	if prefix == "" || len(value) <= len(prefix) || !strings.HasPrefix(value, prefix) {
		return false
	}
	// Stripe object IDs are opaque to Merc, but they are still placed into
	// URL paths and query parameters.  Keep the accepted opaque suffix to the
	// provider's URL-safe ID alphabet so a legacy or manually seeded row cannot
	// turn a read into a different endpoint or query.
	for i := len(prefix); i < len(value); i++ {
		c := value[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func stripePOSTOperation(path string, form url.Values) (PaymentOperation, int64, string, error) {
	path = strings.Trim(strings.SplitN(path, "?", 2)[0], "/")
	switch path {
	case "customers", "setup_intents", "accounts", "account_links":
		return paymentOperationSetup, 0, "", nil
	case "payment_intents":
		amount, err := strconv.ParseInt(form.Get("amount"), 10, 64)
		if err != nil || amount <= 0 {
			return "", 0, "", errors.New("Stripe payment_intents operation requires a positive integer amount")
		}
		currency := strings.ToLower(strings.TrimSpace(form.Get("currency")))
		if currency == "" {
			return "", 0, "", errors.New("Stripe payment_intents operation requires currency")
		}
		return paymentOperationCharge, amount, currency, nil
	case "refunds":
		amount, err := strconv.ParseInt(form.Get("amount"), 10, 64)
		if err != nil || amount <= 0 {
			return "", 0, "", errors.New("Stripe refund operation requires a positive integer amount")
		}
		return paymentOperationRefund, amount, SettlementCurrencyCode(), nil
	default:
		return "", 0, "", fmt.Errorf("unclassified Stripe POST path %q is refused", path)
	}
}

func stripeForm(ctx context.Context, path string, form url.Values, idemKey string) (map[string]any, error) {
	key := stripeKey()
	operation, amountMinor, currency, err := stripePOSTOperation(path, form)
	if err != nil {
		return nil, err
	}
	if _, err := authorizePaymentOperation(operation, amountMinor, currency, key); err != nil {
		return nil, err
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
	resp, err := doStripeRequest(stripeHTTPClient, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := readBoundedRemoteBody(resp.Body, stripeAPIResponseMaxBytes)
	if readErr != nil {
		return nil, fmt.Errorf("stripe %s response read: %w", path, readErr)
	}
	if resp.StatusCode/100 != 2 {
		return nil, &stripeRequestError{
			path: path, statusCode: resp.StatusCode, detail: strings.TrimSpace(string(body)),
		}
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
	} else if err != nil && !errors.Is(err, errNotFound) {
		return "", err
	}
	out, err := stripeForm(ctx, "customers", url.Values{"metadata[buyer_id]": {buyerID.String()}},
		stripeCustomerIdempotencyKey(buyerID))
	if err != nil {
		return "", err
	}
	cust, err := parseStripeProviderObjectID(out, "customer", "customer", "cus_")
	if err != nil {
		return "", err
	}
	if err := store.UpsertBillingCustomer(ctx, buyerID, cust); err != nil {
		return "", err
	}
	return cust, nil
}

func parseStripeProviderObjectID(out map[string]any, objectName, expectedObject, prefix string) (string, error) {
	objectType, _ := out["object"].(string)
	id, _ := out["id"].(string)
	objectType, id = strings.TrimSpace(objectType), strings.TrimSpace(id)
	if objectType != expectedObject || !validStripeObjectID(id, prefix) {
		return "", fmt.Errorf("stripe %s: provider returned an invalid %s object", objectName, objectName)
	}
	return id, nil
}

func stripeCustomerIdempotencyKey(buyerID uuid.UUID) string {
	return "cx-customer-" + buyerID.String()
}

// setupIntentIdempotencyKey scopes an optional client retry key to the buyer.
// When the client omits one, each request still gets a fresh provider key so
// an intentionally new card setup is not accidentally replayed as an older
// SetupIntent. Clients that retry a lost response should resend their key.
func setupIntentIdempotencyKey(buyerID uuid.UUID, requestKey string) string {
	requestKey = strings.TrimSpace(requestKey)
	if requestKey == "" {
		requestKey = uuid.NewString()
	}
	return "cx-setup-" + buyerID.String() + "-" + requestKey
}

func parseStripeSetupIntentResponse(out map[string]any, customer string) (string, error) {
	object, _ := out["object"].(string)
	id, _ := out["id"].(string)
	returnedCustomer, _ := out["customer"].(string)
	clientSecret, _ := out["client_secret"].(string)
	customer = strings.TrimSpace(customer)
	if !validStripeObjectID(customer, "cus_") || object != "setup_intent" || !validStripeObjectID(id, "seti_") ||
		returnedCustomer != customer || strings.TrimSpace(clientSecret) == "" {
		return "", errors.New("stripe setup_intent: response is not the requested SetupIntent")
	}
	return strings.TrimSpace(clientSecret), nil
}

func setupIntent(ctx context.Context, store *Store, buyerID uuid.UUID, requestKey string) (string, error) {
	cust, err := ensureStripeCustomer(ctx, store, buyerID)
	if err != nil {
		return "", err
	}
	out, err := stripeForm(ctx, "setup_intents", url.Values{"customer": {cust}, "payment_method_types[]": {"card"}}, setupIntentIdempotencyKey(buyerID, requestKey))
	if err != nil {
		return "", err
	}
	return parseStripeSetupIntentResponse(out, cust)
}

type ChargeResult struct {
	PaymentIntentID string
	ChargeID        string
	RequestedCents  int64
	ReceivedCents   int64
	Currency        string
}

func validateChargeResult(charge ChargeResult) error {
	if err := validateChargeResultShape(charge); err != nil {
		return err
	}
	if RequireSettlementCurrency(charge.Currency) != nil {
		return errors.New("charge result has invalid Stripe identities, amount, or settlement currency")
	}
	return nil
}

func validateChargeResultShape(charge ChargeResult) error {
	if !validStripeObjectID(charge.PaymentIntentID, "pi_") ||
		!validStripeObjectID(charge.ChargeID, "ch_") ||
		charge.RequestedCents <= 0 || charge.ReceivedCents != charge.RequestedCents {
		return errors.New("charge result has invalid Stripe identities or amount")
	}
	if _, err := ParseCurrency(charge.Currency); err != nil {
		return errors.New("charge result has invalid settlement currency")
	}
	return nil
}

func normalizeChargeCurrencyForAuthority(charge *ChargeResult, authority string) error {
	if charge == nil {
		return fmt.Errorf("%w: charge result is nil", errCurrencyMismatch)
	}
	want, err := ParseCurrency(authority)
	if err != nil {
		return fmt.Errorf("%w: stored authority currency %q is invalid", errCurrencyMismatch, authority)
	}
	got, err := ParseCurrency(charge.Currency)
	if err != nil || !got.Equal(want) {
		return fmt.Errorf("%w: charge currency %q does not match stored authority %q",
			errCurrencyMismatch, charge.Currency, want.Code())
	}
	charge.Currency = got.Code()
	return nil
}

func stripeIntegerField(out map[string]any, field string) (int64, error) {
	v, ok := out[field].(float64)
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || math.Trunc(v) != v || v > math.MaxInt64 {
		return 0, fmt.Errorf("payment intent: %s must be a non-negative integer", field)
	}
	return int64(v), nil
}

func chargePaymentIntent(ctx context.Context, customer, paymentMethod string, cents int64, currency, idemKey string) (ChargeResult, error) {
	return chargePaymentIntentWithKeys(ctx, customer, paymentMethod, cents, currency, idemKey, idemKey)
}

// chargePaymentIntentWithKeys keeps the durable Merc operation key in Stripe
// metadata while allowing a retry after a provider-confirmed decline to use a
// new Stripe idempotency key. Reusing a key after a cached 4xx would replay the
// same rejection even if the buyer has since replaced the card.
func chargePaymentIntentWithKeys(
	ctx context.Context,
	customer, paymentMethod string,
	cents int64,
	currency, operationKey, providerIdemKey string,
) (ChargeResult, error) {
	customer = strings.TrimSpace(customer)
	paymentMethod = strings.TrimSpace(paymentMethod)
	if !validStripeObjectID(customer, "cus_") ||
		(paymentMethod != "" && !validStripeObjectID(paymentMethod, "pm_")) {
		return ChargeResult{}, errors.New("charge requires valid Stripe customer and payment-method identities")
	}
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
		"metadata[cx_operation_key]": {operationKey},
	}
	if paymentMethod != "" {
		form.Set("payment_method", paymentMethod)
	}
	out, err := stripeForm(ctx, "payment_intents", form, providerIdemKey)
	if err != nil {
		return ChargeResult{}, err
	}
	if raw, exists := out["error"]; exists && raw != nil {
		return ChargeResult{}, fmt.Errorf("payment intent returned an error-shaped 2xx response: %v", raw)
	}
	id, err := parseStripeProviderObjectID(out, "payment intent", "payment_intent", "pi_")
	if err != nil {
		return ChargeResult{}, err
	}
	returnedCustomer, err := stripeExpandableMapID(out, "customer", "customer")
	if err != nil || !validStripeObjectID(returnedCustomer, "cus_") || returnedCustomer != customer {
		return ChargeResult{}, fmt.Errorf("payment intent %s customer does not match the requested customer", id)
	}
	if paymentMethod != "" {
		returnedPaymentMethod, err := stripeExpandableMapID(out, "payment_method", "payment_method")
		if err != nil || !validStripeObjectID(returnedPaymentMethod, "pm_") || returnedPaymentMethod != paymentMethod {
			return ChargeResult{}, fmt.Errorf("payment intent %s payment method does not match the requested payment method", id)
		}
	}
	chargeID := ""
	switch latest := out["latest_charge"].(type) {
	case string:
		chargeID = strings.TrimSpace(latest)
	case map[string]any:
		objectType, _ := latest["object"].(string)
		if strings.TrimSpace(objectType) != "charge" {
			return ChargeResult{}, fmt.Errorf("payment intent %s: successful response has a latest charge with the wrong object kind", id)
		}
		chargeID, _ = latest["id"].(string)
		chargeID = strings.TrimSpace(chargeID)
	}
	if !validStripeObjectID(chargeID, "ch_") {
		return ChargeResult{}, fmt.Errorf("payment intent %s: successful response has an invalid latest charge id", id)
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
		Object         string            `json:"object"`
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
	if strings.TrimSpace(pi.Object) != "payment_intent" {
		return "", ChargeResult{}, false, errors.New("successful PaymentIntent event has the wrong Stripe object kind")
	}
	operationKey := strings.TrimSpace(pi.Metadata["cx_operation_key"])
	if operationKey == "" {
		return "", ChargeResult{}, false, nil
	}
	currency, err := ParseCurrency(pi.Currency)
	if err != nil {
		return "", ChargeResult{}, true, errors.New("owned successful PaymentIntent has an unsupported currency")
	}
	chargeID, err := stripeExpandableID(pi.LatestCharge, "charge")
	if err != nil {
		return "", ChargeResult{}, true, err
	}
	pi.ID, chargeID = strings.TrimSpace(pi.ID), strings.TrimSpace(chargeID)
	if !validStripeObjectID(pi.ID, "pi_") || !validStripeObjectID(chargeID, "ch_") || pi.Status != "succeeded" ||
		pi.Amount <= 0 || pi.AmountReceived != pi.Amount {
		return "", ChargeResult{}, true, errors.New("owned successful PaymentIntent has invalid cash evidence")
	}
	return operationKey, ChargeResult{
		PaymentIntentID: pi.ID, ChargeID: chargeID, RequestedCents: pi.Amount,
		ReceivedCents: pi.AmountReceived, Currency: currency.Code(),
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
	settle, err := SettlementCurrency()
	if err != nil {
		return ChargeResult{}, err
	}
	cents, err := settle.MajorToMinor(usd)
	if err != nil {
		return ChargeResult{}, err
	}
	if cents <= 0 {
		return ChargeResult{}, fmt.Errorf("non-positive charge amount %.6f %s", usd, settle.Code())
	}
	// Refuse before creating the durable outcome_unknown request boundary. The
	// provider call repeats this check immediately before network I/O so an
	// activation that expires between planning and dispatch still fails closed.
	if _, err := authorizePaymentOperation(
		paymentOperationCharge, cents, settle.Code(), stripeKey(),
	); err != nil {
		return ChargeResult{}, err
	}
	cust, err := ensureStripeCustomer(ctx, store, buyerID)
	if err != nil {
		return ChargeResult{}, err
	}
	_, pm, err := store.GetBillingCustomer(ctx, buyerID)
	if err != nil || strings.TrimSpace(pm) == "" {
		return ChargeResult{}, fmt.Errorf("buyer has no saved payment method")
	}
	armed, providerIdemKey, err := store.BeginBuyerChargeOperation(
		ctx, idemKey, sourceKind, sourceID, buyerID, cust, pm, cents, settle.Code(),
	)
	if err != nil {
		return ChargeResult{}, err
	}
	if !armed {
		return ChargeResult{}, fmt.Errorf("%w: operation %s already crossed its durable request boundary",
			errBuyerChargeOutcomeUnknown, idemKey)
	}
	charge, err := chargePaymentIntentWithKeys(
		ctx, cust, pm, cents, settle.Code(), idemKey, providerIdemKey,
	)
	if err != nil {
		var providerErr *stripeRequestError
		if errors.As(err, &providerErr) && providerErr.definitelyNotSent() {
			if ferr := store.MarkBuyerChargeDefinitelyFailed(ctx, idemKey, err); ferr != nil {
				_ = store.NoteBuyerChargeOutcomeUnknown(ctx, idemKey, ferr)
				return ChargeResult{}, fmt.Errorf(
					"%w: operation %s requires Stripe reconciliation after durable failure recording failed: %v",
					errBuyerChargeOutcomeUnknown, idemKey, ferr)
			}
			return ChargeResult{}, fmt.Errorf("%w: operation %s: %v",
				errBuyerChargeDefinitelyFailed, idemKey, err)
		}
		_ = store.NoteBuyerChargeOutcomeUnknown(ctx, idemKey, err)
		return ChargeResult{}, fmt.Errorf("%w: operation %s requires Stripe reconciliation: %v",
			errBuyerChargeOutcomeUnknown, idemKey, err)
	}
	return charge, nil
}

func (s *Server) handleBillingSetup(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationalControlActive(w, r, controlPayments) {
		return
	}
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	requestKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if requestKey != "" && !idempotencyKeyPattern.MatchString(requestKey) {
		writeErr(w, http.StatusBadRequest, "Idempotency-Key must be 8-128 safe ASCII characters")
		return
	}
	cs, err := setupIntent(r.Context(), s.store, auth.BuyerID, requestKey)
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
	// Balance and pending top-ups are here because this is the endpoint the
	// top-up path sends a buyer to when their request is refused for already
	// having crossed the Stripe boundary. Answering only configured/connected/
	// has_card left that buyer with no way to tell whether their deposit landed.
	bal, err := s.store.BuyerPrepaidBalanceMicros(r.Context(), auth.BuyerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reading prepaid balance")
		return
	}
	pendingCount, pendingMinorUnits, err := s.store.PrepaidPendingTopupMinorUnits(r.Context(), auth.BuyerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reading pending top-ups")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":                stripeKey() != "",
		"connected":                 cust != "",
		"has_card":                  pm != "",
		"currency":                  SettlementCurrencyCode(),
		"balance_micros":            bal,
		"pending_topup_count":       pendingCount,
		"pending_topup_minor_units": pendingMinorUnits,
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
type billingPMEventSetter func(context.Context, stripePaymentMethodEvent) error
type stripeCashEventApplier func(context.Context, stripeCashEvent) (stripeCashEventResult, error)
type buyerChargeReconciler func(context.Context, string, ChargeResult) error
type stripeRiskEventRecorder func(context.Context, stripeRiskEvent) (stripeRiskEventResult, error)
type stripePaymentFailureEventRecorder func(context.Context, stripePaymentFailureEvent) (stripePaymentFailureEventResult, error)

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
	handleStripeWebhookWithAllHandlersAtMode(
		w, r, secret, setPM, applyCashEvent, reconcileCharge, false,
	)
}

func handleStripeWebhookWithAllHandlersAtMode(
	w http.ResponseWriter,
	r *http.Request,
	secret string,
	setPM billingPMSetter,
	applyCashEvent stripeCashEventApplier,
	reconcileCharge buyerChargeReconciler,
	expectedLive bool,
) {
	handleStripeWebhookWithAllHandlersAtModeAndRisk(
		w, r, secret, setPM, applyCashEvent, reconcileCharge, expectedLive, nil,
	)
}

func handleStripeWebhookWithAllHandlersAtModeAndRisk(
	w http.ResponseWriter,
	r *http.Request,
	secret string,
	setPM billingPMSetter,
	applyCashEvent stripeCashEventApplier,
	reconcileCharge buyerChargeReconciler,
	expectedLive bool,
	recordRisk stripeRiskEventRecorder,
) {
	handleStripeWebhookWithAllHandlersAtModeAndRiskAndPaymentFailure(
		w, r, secret, setPM, applyCashEvent, reconcileCharge, expectedLive,
		recordRisk, nil,
	)
}

func handleStripeWebhookWithAllHandlersAtModeAndRiskAndPaymentFailure(
	w http.ResponseWriter,
	r *http.Request,
	secret string,
	setPM billingPMSetter,
	applyCashEvent stripeCashEventApplier,
	reconcileCharge buyerChargeReconciler,
	expectedLive bool,
	recordRisk stripeRiskEventRecorder,
	recordPaymentFailure stripePaymentFailureEventRecorder,
) {
	handleStripeWebhookWithAllHandlersAtModeAndRiskAndPaymentFailureAndPMEvent(
		w, r, secret, setPM, applyCashEvent, reconcileCharge, expectedLive,
		recordRisk, recordPaymentFailure, nil,
	)
}

func handleStripeWebhookWithAllHandlersAtModeAndRiskAndPaymentFailureAndPMEvent(
	w http.ResponseWriter,
	r *http.Request,
	secret string,
	setPM billingPMSetter,
	applyCashEvent stripeCashEventApplier,
	reconcileCharge buyerChargeReconciler,
	expectedLive bool,
	recordRisk stripeRiskEventRecorder,
	recordPaymentFailure stripePaymentFailureEventRecorder,
	setPMEvent billingPMEventSetter,
) {
	payload, err := readStripeWebhookPayload(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable Stripe webhook body")
		return
	}
	if !verifyStripeSig(payload, r.Header.Get("Stripe-Signature"), secret) {
		writeErr(w, http.StatusBadRequest, "invalid stripe signature")
		return
	}
	var ev struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Created    int64  `json:"created"`
		APIVersion string `json:"api_version"`
		Livemode   *bool  `json:"livemode"`
		Data       struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		writeErr(w, http.StatusBadRequest, "unparseable webhook body")
		return
	}
	if err := validateStripeEventContract(ev.APIVersion, ev.Livemode, expectedLive); err != nil {
		writeErr(w, http.StatusBadRequest, "stripe webhook contract mismatch")
		return
	}
	switch ev.Type {
	case "setup_intent.succeeded", "payment_method.attached":
		var obj map[string]any
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			writeErr(w, http.StatusBadRequest, "unparseable webhook object")
			return
		}
		cust, pm, err := stripeSavedCardWebhookReferences(ev.Type, obj)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid saved payment-method webhook object")
			return
		}
		if cust != "" && pm != "" {
			var err error
			if setPMEvent != nil {
				hash := sha256.Sum256(payload)
				err = setPMEvent(r.Context(), stripePaymentMethodEvent{
					EventID:       ev.ID,
					EventType:     ev.Type,
					CustomerID:    cust,
					PaymentMethod: pm,
					EventCreated:  ev.Created,
					PayloadSHA256: hex.EncodeToString(hash[:]),
				})
			} else if setPM != nil {
				err = setPM(r.Context(), cust, pm)
			} else {
				err = errors.New("saved payment-method handler unavailable")
			}
			if err != nil {
				// A customer this deployment has never seen is not our event:
				// the Stripe account is shared with fixtures and other tooling.
				// Answering 500 tells Stripe to retry, and no number of retries
				// will make a stranger's customer exist here — it just builds a
				// permanently failing delivery queue. Acknowledge and ignore.
				// A real database fault, or the "matched N rows" invariant
				// violation, still answers 500 so the retry is useful.
				if errors.Is(err, errNotFound) {
					log.Printf("billing webhook: ignoring payment-method update for unknown customer")
					break
				}
				log.Printf("billing webhook: saved payment-method update failed: %v", err)
				writeErr(w, http.StatusInternalServerError, "updating saved payment method")
				return
			}
		}
	case "payment_intent.payment_failed":
		var obj map[string]any
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			writeErr(w, http.StatusBadRequest, "unparseable Stripe payment-failure object")
			return
		}
		failureEvent, err := parseStripePaymentFailureEvent(ev.ID, ev.Created, obj, payload)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid Stripe payment-failure event")
			return
		}
		if recordPaymentFailure == nil {
			writeErr(w, http.StatusInternalServerError, "Stripe payment-failure handler unavailable")
			return
		}
		result, err := recordPaymentFailure(r.Context(), failureEvent)
		if err != nil {
			log.Printf("billing webhook: Stripe payment-failure record failed event=%s pi=%s: %v",
				ev.ID, failureEvent.PaymentIntent, err)
			writeErr(w, http.StatusInternalServerError, "recording Stripe payment failure")
			return
		}
		outcome := "recorded"
		if result.Duplicate {
			outcome = "duplicate"
		}
		w.Header().Set("X-Merc-Stripe-Event-Outcome", outcome)
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
	case "radar.early_fraud_warning.created", "radar.early_fraud_warning.updated":
		var obj map[string]any
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			writeErr(w, http.StatusBadRequest, "unparseable Stripe risk object")
			return
		}
		riskEvent, err := parseStripeRiskEvent(ev.ID, ev.Type, ev.Created, obj, payload)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid Stripe risk event")
			return
		}
		if recordRisk == nil {
			writeErr(w, http.StatusInternalServerError, "Stripe risk-event handler unavailable")
			return
		}
		result, err := recordRisk(r.Context(), riskEvent)
		if err != nil {
			log.Printf("billing webhook: Stripe risk event record failed type=%s event=%s: %v", ev.Type, ev.ID, err)
			writeErr(w, http.StatusInternalServerError, "recording Stripe risk event")
			return
		}
		outcome := "recorded"
		if result.Duplicate {
			outcome = "duplicate"
		}
		w.Header().Set("X-Merc-Stripe-Event-Outcome", outcome)
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
			outcome := "accepted"
			switch {
			case result.Duplicate:
				outcome = "duplicate"
			case cashEvent.EffectRank > 0 && result.CashEffectApplied:
				outcome = "applied"
			case cashEvent.EffectRank > 0:
				outcome = "stale_ignored"
			}
			w.Header().Set("X-Merc-Stripe-Event-Outcome", outcome)
			if result.CurrentCashEffectRank > 0 {
				w.Header().Set("X-Merc-Stripe-Cash-Effect-Rank",
					strconv.Itoa(result.CurrentCashEffectRank))
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

// stripeSavedCardWebhookReferences binds a saved-card mutation to the Stripe
// object kind named by the event. A valid HMAC only proves that Stripe signed
// the bytes; accepting a different provider object with the right customer and
// payment_method fields could still replace the buyer's card. Keep unrelated
// customer-less events acknowledgeable, but fail closed before any local
// payment-method setter sees a malformed identity.
func stripeSavedCardWebhookReferences(eventType string, object map[string]any) (string, string, error) {
	if object == nil {
		return "", "", errors.New("saved-card webhook object is absent")
	}
	objectKind, _ := object["object"].(string)
	objectKind = strings.TrimSpace(objectKind)
	objectID, _ := object["id"].(string)
	objectID = strings.TrimSpace(objectID)
	var pm string
	switch eventType {
	case "setup_intent.succeeded":
		if objectKind != "setup_intent" || !validStripeObjectID(objectID, "seti_") {
			return "", "", errors.New("setup_intent.succeeded object is not a SetupIntent")
		}
		var err error
		pm, err = stripeExpandableMapID(object, "payment_method", "payment_method")
		if err != nil {
			return "", "", fmt.Errorf("decode setup-intent payment method: %w", err)
		}
	case "payment_method.attached":
		if objectKind != "payment_method" || !validStripeObjectID(objectID, "pm_") {
			return "", "", errors.New("payment_method.attached object is not a PaymentMethod")
		}
		pm = objectID
	default:
		return "", "", errors.New("unsupported saved-card event type")
	}
	cust, err := stripeExpandableMapID(object, "customer", "customer")
	if err != nil {
		return "", "", fmt.Errorf("decode saved-card customer: %w", err)
	}
	cust, pm = strings.TrimSpace(cust), strings.TrimSpace(pm)
	if cust != "" && !validStripeObjectID(cust, "cus_") {
		return "", "", errors.New("saved-card customer is not a Customer")
	}
	if !validStripeObjectID(pm, "pm_") {
		return "", "", errors.New("saved-card payment method is not a PaymentMethod")
	}
	return cust, pm, nil
}

func (s *Server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationalControlActive(w, r, controlWebhooks) ||
		!s.requireOperationalControlActive(w, r, controlPayments) {
		return
	}
	authority, err := authorizePaymentOperation(
		paymentOperationWebhook, 0, "", stripeKey(),
	)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "stripe webhook authority is unavailable")
		return
	}
	secret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	if secret == "" {
		writeErr(w, http.StatusServiceUnavailable, "stripe webhooks not configured (set STRIPE_WEBHOOK_SECRET)")
		return
	}
	handleStripeWebhookWithAllHandlersAtModeAndRiskAndPaymentFailureAndPMEvent(
		w, r, secret, s.store.SetBillingPMByCustomer, s.store.ApplyPaymentEventTx,
		s.store.ReconcileBuyerChargeOperation, authority.Mode == PaymentModeLive,
		s.store.ApplyStripeRiskEvent, s.store.ApplyStripePaymentFailureEvent,
		s.store.SetBillingPMByCustomerEvent,
	)
}

func (s *Server) chargeForJob(ctx context.Context, jobID uuid.UUID) {
	paused, err := s.store.OperationalControlPaused(ctx, controlPayments)
	if err != nil || paused {
		log.Printf("billing: charge for job %s deferred while payment processing is paused or unavailable", jobID)
		return
	}
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
	// Processor-fee reconciliation is deliberately off the charge success
	// critical path. The charged job is durable above; the charge-collect sweep
	// fetches the exact settled balance-transaction fee and records it later.
}

func recordStripeFee(ctx context.Context, store *Store, buyerID uuid.UUID, pi string) error {
	pi = strings.TrimSpace(pi)
	if !validStripeObjectID(pi, "pi_") {
		return fmt.Errorf("no payment intent id to fetch a fee for")
	}
	out, err := stripeGet(ctx, "payment_intents/"+url.PathEscape(pi)+"?expand[]=latest_charge.balance_transaction")
	if err != nil {
		return err
	}
	returnedID, err := parseStripeProviderObjectID(out, "payment intent fee", "payment_intent", "pi_")
	if err != nil || returnedID != pi {
		return fmt.Errorf("payment intent fee response has an unexpected object id")
	}
	feeMinorUnits, currency, err := stripePaymentIntentFeeCash(out)
	if err != nil {
		return fmt.Errorf("payment intent %s: %w", pi, err)
	}
	if err := store.InsertStripeFee(ctx, buyerID, pi, feeMinorUnits, currency); err != nil {
		return err
	}
	if _, err := store.AllocateBatchStripeFee(ctx, pi); err != nil {
		return fmt.Errorf("stripe fee recorded for %s but batch allocation is pending: %w", pi, err)
	}
	return nil
}

// stripePaymentIntentFeeCash extracts a settled processor fee as an exact ISO
// minor-unit value.  Stripe returns two currencies here: the PaymentIntent's
// presentment currency and its balance transaction currency.  Recording a fee
// when either is absent or they disagree would make a CAD/JPY receipt appear
// balanced while pricing the processor cost in a different money unit.
func stripePaymentIntentFeeCash(out map[string]any) (int64, string, error) {
	piCurrencyRaw, _ := out["currency"].(string)
	piCurrency, err := ParseCurrency(piCurrencyRaw)
	if err != nil {
		return 0, "", fmt.Errorf("payment intent currency refused: %w", err)
	}
	settlement, err := SettlementCurrency()
	if err != nil || !piCurrency.Equal(settlement) {
		if err == nil {
			err = errCurrencyMismatch
		}
		return 0, "", fmt.Errorf("payment intent currency refused: %w", err)
	}
	lc, ok := out["latest_charge"].(map[string]any)
	if !ok {
		return 0, "", errors.New("latest_charge.balance_transaction is absent (not settled yet?)  -  fee not recorded, never estimated")
	}
	bt, ok := lc["balance_transaction"].(map[string]any)
	if !ok {
		return 0, "", errors.New("latest_charge.balance_transaction is absent (not settled yet?)  -  fee not recorded, never estimated")
	}
	btCurrencyRaw, _ := bt["currency"].(string)
	btCurrency, err := ParseCurrency(btCurrencyRaw)
	if err != nil || !btCurrency.Equal(piCurrency) {
		if err == nil {
			err = errCurrencyMismatch
		}
		return 0, "", fmt.Errorf("balance transaction currency %q does not match payment intent currency %q: %w", btCurrencyRaw, piCurrencyRaw, err)
	}
	feeMinorUnits, err := stripeIntegerField(bt, "fee")
	if err != nil {
		return 0, "", fmt.Errorf("latest_charge.balance_transaction fee: %w", err)
	}
	return feeMinorUnits, piCurrency.Code(), nil
}

func stripeGet(ctx context.Context, path string) (map[string]any, error) {
	key := stripeKey()
	if _, err := authorizePaymentOperation(paymentOperationRead, 0, "", key); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(stripeAPIBaseURL, "/")+"/"+strings.TrimLeft(path, "/"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := doStripeRequest(stripeHTTPClient, req)
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
