package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	KindBuyerCharge    = "buyer_charge"
	KindSupplierCredit = "supplier_credit"
	KindPlatformTake   = "platform_take"
	KindClawback       = "clawback"
	KindStripeFee      = "stripe_fee"
)

const (
	PayoutPending          = "pending"
	PayoutHeld             = "held"
	PayoutReady            = "ready"
	PayoutAwaitingFunding  = "awaiting_funding"
	PayoutCarried          = "carried"
	PayoutSending          = "sending"
	PayoutOutcomeUnknown   = "outcome_unknown"
	PayoutReleased         = "released"
	PayoutExported         = "exported"
	PayoutClawedBack       = "clawed_back"
	PayoutReversalRequired = "reversal_required"
)

const (
	supplierSettlementPolicyFloorCentCarryV1       = "floor_cent_carry_v1"
	microUSDPerCent                          int64 = 10_000
)

func splitSupplierLiabilityMicros(liabilityMicros int64) (cashCents, remainderMicros int64, err error) {
	if liabilityMicros < 0 {
		return 0, 0, fmt.Errorf("supplier liability must be non-negative, got %d microusd", liabilityMicros)
	}
	cashCents = liabilityMicros / microUSDPerCent
	remainderMicros = liabilityMicros % microUSDPerCent
	return cashCents, remainderMicros, nil
}

const minimumPayoutHold = 24 * time.Hour

func payoutReleaseAt(now time.Time, requestedSecs uint32) time.Time {
	hold := time.Duration(requestedSecs) * time.Second
	if hold < minimumPayoutHold {
		hold = minimumPayoutHold
	}
	return now.Add(hold)
}

var (
	platformTakeRate  = takeRateFromEnv()
	supplierShareRate = 1.0 - platformTakeRate
)

func takeRateFromEnv() float64 {
	const def, lo, hi = 3.0, 1.0, 5.0
	pct := def
	if s := strings.TrimSpace(os.Getenv("CX_PLATFORM_TAKE_PCT")); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			pct = v
		}
	}
	if pct < lo {
		pct = lo
	}
	if pct > hi {
		pct = hi
	}
	return pct / 100.0
}

type LedgerEntry struct {
	Kind         string
	SupplierID   *uuid.UUID
	BuyerID      *uuid.UUID
	TaskID       *uuid.UUID
	AmountUSD    float64
	PayoutStatus string
	ReleaseAt    *time.Time
}

func splitFrozenCharge(buyerID, supplierID, taskID uuid.UUID, buyerCharge, supplierPayout float64, holdSecs uint32, now time.Time) []LedgerEntry {
	platformAmt := buyerCharge - supplierPayout
	release := payoutReleaseAt(now, holdSecs)
	return []LedgerEntry{
		{Kind: KindBuyerCharge, BuyerID: &buyerID, TaskID: &taskID, AmountUSD: -buyerCharge, PayoutStatus: PayoutReleased},
		{Kind: KindSupplierCredit, SupplierID: &supplierID, TaskID: &taskID, AmountUSD: supplierPayout, PayoutStatus: PayoutHeld, ReleaseAt: &release},
		{Kind: KindPlatformTake, TaskID: &taskID, AmountUSD: platformAmt, PayoutStatus: PayoutReleased},
	}
}

func clawbackEntry(supplierID, taskID uuid.UUID, amount float64) LedgerEntry {
	return LedgerEntry{
		Kind:         KindClawback,
		SupplierID:   &supplierID,
		TaskID:       &taskID,
		AmountUSD:    -amount,
		PayoutStatus: PayoutClawedBack,
	}
}

type PayoutResult struct {
	Ref       string
	SentCents int64
	Currency  string
	CashMoved bool
}

type Payout interface {
	Send(ctx context.Context, supplierID uuid.UUID, cents int64, currency, payoutKey string) (PayoutResult, error)
}

var errPayoutUnconfigured = errors.New("payout rail not configured (Stripe Connect/Trolley)  -  Phase 3")

var errPayoutOutcomeUnknown = errors.New("payout provider outcome is unknown")

var errPayoutDefinitelyNotSent = errors.New("payout provider definitely did not send")

func payoutOutcomeUnknown(cause error) error {
	if cause == nil {
		return errPayoutOutcomeUnknown
	}
	return fmt.Errorf("%w: %v", errPayoutOutcomeUnknown, cause)
}

func payoutDefinitelyNotSent(cause error) error {
	if cause == nil {
		return errPayoutDefinitelyNotSent
	}
	return fmt.Errorf("%w: %w", errPayoutDefinitelyNotSent, cause)
}

type stubPayout struct{}

func (stubPayout) Send(_ context.Context, _ uuid.UUID, _ int64, _, _ string) (PayoutResult, error) {
	return PayoutResult{}, payoutDefinitelyNotSent(errPayoutUnconfigured)
}

type StripePayout struct {
	store  *Store
	secret string
	http   *http.Client
}

func newStripePayout(store *Store, secret string) StripePayout {
	return StripePayout{store: store, secret: secret, http: &http.Client{Timeout: 20 * time.Second}}
}

func readStripePayoutResponseBody(r io.Reader) ([]byte, error) {
	body, err := readBoundedRemoteBody(r, stripeAPIResponseMaxBytes)
	if err != nil {
		return nil, payoutOutcomeUnknown(fmt.Errorf("stripe transfer response read: %v", err))
	}
	return body, nil
}

func (p StripePayout) Send(ctx context.Context, supplierID uuid.UUID, cents int64, currency, payoutKey string) (PayoutResult, error) {
	if p.secret == "" {
		return PayoutResult{}, payoutDefinitelyNotSent(errPayoutUnconfigured)
	}
	acct, err := p.store.SupplierStripeAcct(ctx, supplierID)
	if err != nil {
		return PayoutResult{}, payoutDefinitelyNotSent(fmt.Errorf("looking up supplier stripe account: %w", err))
	}
	if acct == "" {
		return PayoutResult{}, payoutDefinitelyNotSent(
			fmt.Errorf("supplier %s has no connected Stripe account (stripe_acct empty)", supplierID))
	}
	if cents <= 0 {
		return PayoutResult{}, payoutDefinitelyNotSent(fmt.Errorf("non-positive payout amount %d cents", cents))
	}
	if currency != "usd" {
		return PayoutResult{}, payoutDefinitelyNotSent(fmt.Errorf("unsupported payout currency %q", currency))
	}
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(cents, 10))
	form.Set("currency", currency)
	form.Set("destination", acct)
	form.Set("transfer_group", "cxpo_"+payoutKey)
	form.Set("metadata[cx_payout_key]", payoutKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.stripe.com/v1/transfers", strings.NewReader(form.Encode()))
	if err != nil {
		return PayoutResult{}, payoutDefinitelyNotSent(err)
	}
	req.Header.Set("Authorization", "Bearer "+p.secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Idempotency-Key", stripeIdempotencyKey(supplierID, cents, payoutKey))
	resp, err := p.http.Do(req)
	if err != nil {
		return PayoutResult{}, payoutOutcomeUnknown(fmt.Errorf("stripe transfer request: %w", err))
	}
	defer resp.Body.Close()
	body, readErr := readStripePayoutResponseBody(resp.Body)
	if readErr != nil {
		return PayoutResult{}, readErr
	}
	if resp.StatusCode/100 != 2 {
		err := fmt.Errorf("stripe transfer failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		if resp.StatusCode >= http.StatusInternalServerError ||
			resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusConflict {
			return PayoutResult{}, payoutOutcomeUnknown(err)
		}
		return PayoutResult{}, payoutDefinitelyNotSent(err)
	}
	var out struct {
		ID       string `json:"id"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.ID == "" {
		return PayoutResult{}, payoutOutcomeUnknown(
			fmt.Errorf("stripe transfer: unparseable success response: %s", strings.TrimSpace(string(body))))
	}
	if out.Amount != cents || out.Currency != currency {
		return PayoutResult{}, payoutOutcomeUnknown(fmt.Errorf(
			"stripe transfer %s amount/currency mismatch: requested=%d usd response=%d %s",
			out.ID, cents, out.Amount, out.Currency))
	}
	return PayoutResult{Ref: out.ID, SentCents: out.Amount, Currency: out.Currency, CashMoved: true}, nil
}

func stripeIdempotencyKey(supplierID uuid.UUID, cents int64, payoutKey string) string {
	key := "cx-" + supplierID.String() + "-" + strconv.FormatInt(cents, 10)
	if payoutKey != "" {
		key += "-" + payoutKey
	}
	return key
}

type ManualExportPayout struct {
	path string
	mu   sync.Mutex
}

func newManualExportPayout(path string) *ManualExportPayout { return &ManualExportPayout{path: path} }

func (p *ManualExportPayout) Send(_ context.Context, supplierID uuid.UUID, cents int64, currency, payoutKey string) (PayoutResult, error) {
	if cents <= 0 {
		return PayoutResult{}, payoutDefinitelyNotSent(fmt.Errorf("non-positive payout amount %d cents", cents))
	}
	if currency != "usd" {
		return PayoutResult{}, payoutDefinitelyNotSent(fmt.Errorf("unsupported payout currency %q", currency))
	}
	if strings.TrimSpace(payoutKey) == "" {
		return PayoutResult{}, payoutDefinitelyNotSent(errors.New("manual export payout key is required"))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, err := os.ReadFile(p.path); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(existing)), "\n") {
			fields := strings.Split(line, ",")
			if len(fields) > 0 && fields[len(fields)-1] == payoutKey {
				expectedAmount := fmt.Sprintf("%.6f", float64(cents)/100)
				if len(fields) != 4 || fields[0] != supplierID.String() || fields[1] != expectedAmount {
					return PayoutResult{}, fmt.Errorf(
						"payout export key %s is already bound to a different instruction", payoutKey)
				}
				return PayoutResult{Ref: "manual-export:" + p.path, Currency: currency, CashMoved: false}, nil
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return PayoutResult{}, payoutDefinitelyNotSent(
			fmt.Errorf("reading payout export %q for idempotency: %w", p.path, err))
	}
	_, statErr := os.Stat(p.path)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return PayoutResult{}, payoutDefinitelyNotSent(fmt.Errorf("stating payout export %q: %w", p.path, statErr))
	}
	f, err := os.OpenFile(p.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return PayoutResult{}, payoutDefinitelyNotSent(fmt.Errorf("opening payout export %q: %w", p.path, err))
	}
	if _, err := fmt.Fprintf(f, "%s,%.6f,%s,%s\n", supplierID, float64(cents)/100,
		time.Now().UTC().Format(time.RFC3339), payoutKey); err != nil {
		_ = f.Close()
		return PayoutResult{}, fmt.Errorf("writing payout export: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return PayoutResult{}, fmt.Errorf("syncing payout export %q: %w", p.path, err)
	}
	if err := f.Close(); err != nil {
		return PayoutResult{}, fmt.Errorf("closing payout export %q: %w", p.path, err)
	}
	if created {
		dir, err := os.Open(filepath.Dir(p.path))
		if err != nil {
			return PayoutResult{}, fmt.Errorf("opening payout export directory: %w", err)
		}
		if err := dir.Sync(); err != nil {
			_ = dir.Close()
			return PayoutResult{}, fmt.Errorf("syncing payout export directory: %w", err)
		}
		if err := dir.Close(); err != nil {
			return PayoutResult{}, fmt.Errorf("closing payout export directory: %w", err)
		}
	}
	return PayoutResult{Ref: "manual-export:" + p.path, Currency: "usd", CashMoved: false}, nil
}
