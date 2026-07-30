package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	errSimConflict       = errors.New("simulator idempotency conflict")
	errSimInvalidState   = errors.New("simulator invalid state transition")
	errSimRefundExceeded = errors.New("simulator refund exceeds captured amount")
	errSimSignature      = errors.New("simulator webhook signature rejected")
)

type simulatedPaymentIntent struct {
	ID             string `json:"id"`
	AmountCents    int64  `json:"amount_cents"`
	State          string `json:"state"`
	RefundedCents  int64  `json:"refunded_cents"`
	DisputeOpen    bool   `json:"dispute_open"`
	TransferCents  int64  `json:"transfer_cents"`
	PayoutState    string `json:"payout_state"`
	WebhookState   string `json:"webhook_state"`
	Transition     int64  `json:"transition"`
	AttemptBinding string `json:"attempt_binding"`
}

type simulatorBinding struct {
	Fingerprint string
	IntentID    string
}

type deterministicStripeSimulator struct {
	mu            sync.Mutex
	seed          int64
	nextID        uint64
	intents       map[string]*simulatedPaymentIntent
	idempotency   map[string]simulatorBinding
	effects       map[string]struct{}
	webhooks      map[string]string
	ledger        []int64
	discrepancies []string
}

func newDeterministicStripeSimulator(seed int64) *deterministicStripeSimulator {
	return &deterministicStripeSimulator{
		seed: seed, intents: map[string]*simulatedPaymentIntent{},
		idempotency: map[string]simulatorBinding{}, effects: map[string]struct{}{},
		webhooks: map[string]string{},
	}
}

func (s *deterministicStripeSimulator) pair(amount int64) {
	s.ledger = append(s.ledger, -amount, amount)
}

func (s *deterministicStripeSimulator) once(effect string, amount int64) bool {
	if _, exists := s.effects[effect]; exists {
		return false
	}
	s.effects[effect] = struct{}{}
	s.pair(amount)
	return true
}

func (s *deterministicStripeSimulator) createPaymentIntent(key string, amount int64, attempt string, persist bool) (simulatedPaymentIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if amount <= 0 || strings.TrimSpace(key) == "" || strings.TrimSpace(attempt) == "" {
		return simulatedPaymentIntent{}, errors.New("simulator requires positive amount, idempotency key, and attempt binding")
	}
	fingerprint := fmt.Sprintf("%d:%s", amount, attempt)
	if bound, ok := s.idempotency[key]; ok {
		if bound.Fingerprint != fingerprint {
			return simulatedPaymentIntent{}, errSimConflict
		}
		return *s.intents[bound.IntentID], nil
	}
	if !persist { // deterministic timeout before the provider durably accepts the request
		return simulatedPaymentIntent{}, contextDeadlineError{}
	}
	s.nextID++
	id := fmt.Sprintf("pi_sim_%016x", uint64(s.seed)^s.nextID)
	intent := &simulatedPaymentIntent{ID: id, AmountCents: amount, State: "created", WebhookState: "created", AttemptBinding: attempt, Transition: 1}
	s.intents[id] = intent
	s.idempotency[key] = simulatorBinding{Fingerprint: fingerprint, IntentID: id}
	return *intent, nil
}

type contextDeadlineError struct{}

func (contextDeadlineError) Error() string   { return "simulated timeout before persistence" }
func (contextDeadlineError) Timeout() bool   { return true }
func (contextDeadlineError) Temporary() bool { return true }

func (s *deterministicStripeSimulator) transition(id, effect, from, to string, amount int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	intent := s.intents[id]
	if intent == nil {
		return errors.New("simulator intent not found")
	}
	if intent.State == to {
		if _, exists := s.effects[effect]; exists {
			return nil
		}
		return errSimInvalidState
	}
	if intent.State != from {
		return errSimInvalidState
	}
	if s.once(effect, amount) {
		intent.State = to
		intent.Transition++
	}
	return nil
}

func (s *deterministicStripeSimulator) authorize(id string) error {
	return s.transition(id, "authorize:"+id, "created", "authorized", s.amount(id))
}

func (s *deterministicStripeSimulator) capture(id string) error {
	return s.transition(id, "capture:"+id, "authorized", "captured", s.amount(id))
}

func (s *deterministicStripeSimulator) decline(id string) error {
	return s.transition(id, "decline:"+id, "created", "declined", 0)
}

func (s *deterministicStripeSimulator) amount(id string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.intents[id] == nil {
		return 0
	}
	return s.intents[id].AmountCents
}

func (s *deterministicStripeSimulator) refund(id, effect string, amount int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	intent := s.intents[id]
	if intent == nil || intent.State != "captured" || amount <= 0 {
		return errSimInvalidState
	}
	if _, exists := s.effects[effect]; exists {
		return nil
	}
	if intent.RefundedCents+amount > intent.AmountCents {
		return errSimRefundExceeded
	}
	s.once(effect, amount)
	intent.RefundedCents += amount
	intent.Transition++
	return nil
}

func (s *deterministicStripeSimulator) dispute(id, effect string, open bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	intent := s.intents[id]
	if intent == nil || intent.State != "captured" {
		return errSimInvalidState
	}
	if open && intent.PayoutState == "released" {
		return errSimInvalidState
	}
	if _, exists := s.effects[effect]; exists {
		return nil
	}
	if intent.DisputeOpen == open {
		return errSimInvalidState
	}
	s.once(effect, intent.AmountCents-intent.RefundedCents)
	intent.DisputeOpen = open
	intent.Transition++
	return nil
}

func (s *deterministicStripeSimulator) transfer(id, effect, attempt string, amount int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	intent := s.intents[id]
	if intent == nil || intent.State != "captured" || intent.DisputeOpen ||
		intent.AttemptBinding != attempt || amount <= 0 {
		return errSimInvalidState
	}
	if _, exists := s.effects[effect]; exists {
		return nil
	}
	if intent.TransferCents+amount > intent.AmountCents-intent.RefundedCents {
		return errSimInvalidState
	}
	if s.once(effect, amount) {
		intent.TransferCents += amount
		intent.PayoutState = "held"
		intent.Transition++
	}
	return nil
}

func (s *deterministicStripeSimulator) payout(id, effect, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	intent := s.intents[id]
	if intent == nil || intent.TransferCents <= 0 || intent.DisputeOpen {
		return errSimInvalidState
	}
	if intent.PayoutState == to {
		if _, exists := s.effects[effect]; exists {
			return nil
		}
		return errSimInvalidState
	}
	valid := (intent.PayoutState == "held" && (to == "released" || to == "failed")) ||
		(intent.PayoutState == "released" && to == "reversed")
	if !valid {
		return errSimInvalidState
	}
	if s.once(effect, intent.TransferCents) {
		intent.PayoutState = to
		intent.Transition++
	}
	return nil
}

func (s *deterministicStripeSimulator) applyWebhookState(id, state string) error {
	rank := map[string]int{"created": 0, "authorized": 1, "captured": 2, "refunded": 3}
	want, valid := rank[state]
	if !valid {
		return errSimInvalidState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	intent := s.intents[id]
	if intent == nil {
		return errSimInvalidState
	}
	current, valid := rank[intent.WebhookState]
	if !valid {
		return errSimInvalidState
	}
	if want <= current {
		return nil
	}
	intent.WebhookState = state
	return nil
}

func (s *deterministicStripeSimulator) snapshot(id string) (simulatedPaymentIntent, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	intent := s.intents[id]
	if intent == nil {
		return simulatedPaymentIntent{}, 0, errSimInvalidState
	}
	return *intent, len(s.ledger), nil
}

func simulatedWebhookSignature(secret string, timestamp int64, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", timestamp)
	mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", timestamp, hex.EncodeToString(mac.Sum(nil)))
}

func (s *deterministicStripeSimulator) acceptWebhook(eventID, signature, secret string, payload []byte, now int64) (bool, error) {
	parts := strings.Split(signature, ",")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "t=") || !strings.HasPrefix(parts[1], "v1=") {
		return false, errSimSignature
	}
	var timestamp int64
	if _, err := fmt.Sscanf(parts[0], "t=%d", &timestamp); err != nil || now-timestamp > 300 || timestamp-now > 300 {
		return false, errSimSignature
	}
	want := simulatedWebhookSignature(secret, timestamp, payload)
	if !hmac.Equal([]byte(want), []byte(signature)) {
		return false, errSimSignature
	}
	digest := sha256.Sum256(payload)
	binding := hex.EncodeToString(digest[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, exists := s.webhooks[eventID]; exists {
		if existing != binding {
			return false, errSimConflict
		}
		return true, nil
	}
	s.webhooks[eventID] = binding
	return false, nil
}

func (s *deterministicStripeSimulator) reconcile(provider map[string]string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discrepancies = s.discrepancies[:0]
	for id, intent := range s.intents {
		if provider[id] != intent.State {
			s.discrepancies = append(s.discrepancies, id)
		}
	}
	sort.Strings(s.discrepancies)
	return append([]string(nil), s.discrepancies...)
}

func (s *deterministicStripeSimulator) invariant() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sum int64
	for _, entry := range s.ledger {
		sum += entry
	}
	if sum != 0 {
		return fmt.Errorf("simulator ledger sum=%d", sum)
	}
	for _, intent := range s.intents {
		if intent.RefundedCents < 0 || intent.RefundedCents > intent.AmountCents || intent.Transition < 1 {
			return errors.New("simulator intent invariant failed")
		}
		if intent.DisputeOpen && intent.PayoutState == "released" {
			return errors.New("simulator released a held payout during dispute")
		}
	}
	return nil
}

type simulatorReceipt struct {
	SchemaVersion int               `json:"schema_version"`
	Kind          string            `json:"kind"`
	Status        string            `json:"status"`
	Label         string            `json:"evidence_label"`
	Seed          int64             `json:"seed"`
	Scenarios     map[string]bool   `json:"scenarios"`
	Properties    map[string]bool   `json:"properties"`
	External      map[string]string `json:"external_gates"`
	Generated     *struct {
		Count        int   `json:"count"`
		SeedStart    int64 `json:"seed_start"`
		SeedEnd      int64 `json:"seed_end"`
		StepsPerSeed int   `json:"steps_per_seed"`
	} `json:"generated_sequences,omitempty"`
}

func runDeterministicStripeSimulation(seed int64) (simulatorReceipt, error) {
	sim := newDeterministicStripeSimulator(seed)
	first, err := sim.createPaymentIntent("create-1", 1000, "task-attempt-1", true)
	if err != nil {
		return simulatorReceipt{}, err
	}
	if _, err := sim.createPaymentIntent("create-timeout", 500, "task-attempt-2", false); err == nil {
		return simulatorReceipt{}, errors.New("timeout-before-persistence was accepted")
	}
	// Discard the first successful response to model a provider-side success
	// followed by a lost response, then recover the durable object by retrying
	// the exact idempotency binding.
	lost, err := sim.createPaymentIntent("lost-response", 850, "task-attempt-lost", true)
	if err != nil {
		return simulatorReceipt{}, err
	}
	recovered, err := sim.createPaymentIntent("lost-response", 850, "task-attempt-lost", true)
	if err != nil || recovered.ID != lost.ID {
		return simulatorReceipt{}, errors.New("persisted success was not recovered after a lost response")
	}
	if retry, err := sim.createPaymentIntent("create-1", 1000, "task-attempt-1", true); err != nil || retry.ID != first.ID {
		return simulatorReceipt{}, errors.New("idempotent retry failed")
	}
	if _, err := sim.createPaymentIntent("create-1", 1001, "task-attempt-1", true); !errors.Is(err, errSimConflict) {
		return simulatorReceipt{}, errors.New("conflicting idempotency reuse was accepted")
	}
	if err := sim.authorize(first.ID); err != nil {
		return simulatorReceipt{}, err
	}
	authorized, ledgerAfterAuthorization, err := sim.snapshot(first.ID)
	if err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.authorize(first.ID); err != nil {
		return simulatorReceipt{}, errors.New("idempotent authorization retry failed")
	}
	authorizedRetry, ledgerAfterAuthorizationRetry, _ := sim.snapshot(first.ID)
	if authorizedRetry.Transition != authorized.Transition || ledgerAfterAuthorizationRetry != ledgerAfterAuthorization {
		return simulatorReceipt{}, errors.New("duplicate authorization produced a second effect")
	}
	if err := sim.capture(first.ID); err != nil {
		return simulatorReceipt{}, err
	}
	captured, ledgerAfterCapture, err := sim.snapshot(first.ID)
	if err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.capture(first.ID); err != nil {
		return simulatorReceipt{}, errors.New("idempotent capture retry failed")
	}
	capturedRetry, ledgerAfterCaptureRetry, _ := sim.snapshot(first.ID)
	if capturedRetry.Transition != captured.Transition || ledgerAfterCaptureRetry != ledgerAfterCapture {
		return simulatorReceipt{}, errors.New("duplicate capture produced a second settlement")
	}
	if err := sim.refund(first.ID, "refund-1", 250); err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.refund(first.ID, "refund-too-large", 751); !errors.Is(err, errSimRefundExceeded) {
		return simulatorReceipt{}, errors.New("excessive refund was accepted")
	}
	if err := sim.dispute(first.ID, "dispute-open", true); err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.transfer(first.ID, "stale-transfer", "wrong-attempt", 400); err == nil {
		return simulatorReceipt{}, errors.New("stale attempt settled")
	}
	if err := sim.dispute(first.ID, "dispute-close", false); err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.transfer(first.ID, "transfer-1", "task-attempt-1", 400); err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.transfer(first.ID, "transfer-overflow", "task-attempt-1", 351); !errors.Is(err, errSimInvalidState) {
		return simulatorReceipt{}, errors.New("transfers exceeded the unsettled amount")
	}
	if err := sim.payout(first.ID, "payout-release", "released"); err != nil {
		return simulatorReceipt{}, err
	}
	released, ledgerAfterPayout, err := sim.snapshot(first.ID)
	if err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.payout(first.ID, "payout-release", "released"); err != nil {
		return simulatorReceipt{}, errors.New("idempotent payout retry failed")
	}
	releasedRetry, ledgerAfterPayoutRetry, _ := sim.snapshot(first.ID)
	if releasedRetry.Transition != released.Transition || ledgerAfterPayoutRetry != ledgerAfterPayout {
		return simulatorReceipt{}, errors.New("duplicate payout produced a second effect")
	}
	if err := sim.payout(first.ID, "payout-reverse", "reversed"); err != nil {
		return simulatorReceipt{}, err
	}
	declined, err := sim.createPaymentIntent("decline", 700, "task-attempt-3", true)
	if err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.decline(declined.ID); err != nil {
		return simulatorReceipt{}, err
	}
	now := time.Unix(1_800_000_000, 0).Unix()
	payload := []byte(`{"id":"evt_sim","type":"payment_intent.succeeded"}`)
	sig := simulatedWebhookSignature("simulator-secret", now, payload)
	if duplicate, err := sim.acceptWebhook("evt_sim", sig, "simulator-secret", payload, now); err != nil || duplicate {
		return simulatorReceipt{}, errors.New("first signed webhook failed")
	}
	if duplicate, err := sim.acceptWebhook("evt_sim", sig, "simulator-secret", payload, now); err != nil || !duplicate {
		return simulatorReceipt{}, errors.New("duplicate webhook was not inert")
	}
	invalidSignature := simulatedWebhookSignature("wrong-secret", now, payload)
	if _, err := sim.acceptWebhook("evt_invalid", invalidSignature, "simulator-secret", payload, now); !errors.Is(err, errSimSignature) {
		return simulatorReceipt{}, errors.New("invalid webhook signature was accepted")
	}
	if _, err := sim.acceptWebhook("evt_expired", sig, "simulator-secret", payload, now+301); !errors.Is(err, errSimSignature) {
		return simulatorReceipt{}, errors.New("expired signature was accepted")
	}
	// Deliver the terminal provider event before an earlier authorization
	// event, and prove the consumer's observed state cannot regress.
	terminalPayload := []byte(`{"id":"evt_terminal","type":"payment_intent.succeeded"}`)
	terminalSignature := simulatedWebhookSignature("simulator-secret", now, terminalPayload)
	if _, err := sim.acceptWebhook("evt_terminal", terminalSignature, "simulator-secret", terminalPayload, now); err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.applyWebhookState(first.ID, "captured"); err != nil {
		return simulatorReceipt{}, err
	}
	earlierPayload := []byte(`{"id":"evt_earlier","type":"payment_intent.amount_capturable_updated"}`)
	earlierSignature := simulatedWebhookSignature("simulator-secret", now, earlierPayload)
	if _, err := sim.acceptWebhook("evt_earlier", earlierSignature, "simulator-secret", earlierPayload, now); err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.applyWebhookState(first.ID, "authorized"); err != nil {
		return simulatorReceipt{}, err
	}
	ordered, _, _ := sim.snapshot(first.ID)
	if ordered.WebhookState != "captured" {
		return simulatorReceipt{}, errors.New("out-of-order webhook regressed payment state")
	}

	fullyRefunded, err := sim.createPaymentIntent("full-refund", 300, "task-attempt-refund", true)
	if err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.authorize(fullyRefunded.ID); err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.capture(fullyRefunded.ID); err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.refund(fullyRefunded.ID, "refund-partial", 100); err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.refund(fullyRefunded.ID, "refund-balance", 200); err != nil {
		return simulatorReceipt{}, err
	}
	refundedSnapshot, _, _ := sim.snapshot(fullyRefunded.ID)
	if refundedSnapshot.RefundedCents != refundedSnapshot.AmountCents {
		return simulatorReceipt{}, errors.New("full refund did not reach the captured bound")
	}
	failed, err := sim.createPaymentIntent("payout-failure", 600, "task-attempt-4", true)
	if err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.authorize(failed.ID); err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.capture(failed.ID); err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.transfer(failed.ID, "transfer-failure", "task-attempt-4", 300); err != nil {
		return simulatorReceipt{}, err
	}
	if err := sim.payout(failed.ID, "payout-failure", "failed"); err != nil {
		return simulatorReceipt{}, err
	}
	if got := sim.reconcile(map[string]string{first.ID: "captured", lost.ID: "created", fullyRefunded.ID: "captured", declined.ID: "created", failed.ID: "captured"}); len(got) != 1 {
		return simulatorReceipt{}, errors.New("reconciliation mismatch was not visible")
	}
	if got := sim.reconcile(map[string]string{first.ID: "captured", lost.ID: "created", fullyRefunded.ID: "captured", declined.ID: "declined", failed.ID: "captured"}); len(got) != 0 {
		return simulatorReceipt{}, errors.New("clean reconciliation failed")
	}
	if err := sim.invariant(); err != nil {
		return simulatorReceipt{}, err
	}
	return simulatorReceipt{
		SchemaVersion: 1, Kind: "stripe_provider_simulation", Status: "SIMULATED PASS", Label: "SIMULATED", Seed: seed,
		Scenarios:  map[string]bool{"payment_intent": true, "authorization": true, "capture": true, "decline": true, "timeout_before_persistence": true, "persistence_dropped_response": true, "idempotent_retry": true, "conflicting_idempotency": true, "duplicate_webhook": true, "out_of_order_webhook": true, "refund": true, "partial_refund": true, "excessive_refund_rejection": true, "dispute": true, "dispute_resolution": true, "transfer": true, "payout_hold": true, "payout_release": true, "payout_failure": true, "payout_reversal": true, "signature_validation": true, "timestamp_expiry": true, "reconciliation_mismatch": true, "clean_reconciliation": true},
		Properties: map[string]bool{"ledger_zero_sum": true, "effects_once": true, "no_duplicate_authorization": true, "no_duplicate_settlement": true, "no_duplicate_payout": true, "refund_bounded": true, "dispute_bounded": true, "state_monotonic": true, "state_non_regression": true, "hold_enforced": true, "stale_attempt_cannot_settle": true, "uncertain_response_recoverable": true, "reconciliation_identifies_faults": true, "discrepancy_visible": true},
		External:   map[string]string{"stripe_sandbox_api": "EXTERNAL CREDENTIAL REQUIRED", "sandbox_webhook": "EXTERNAL CREDENTIAL REQUIRED", "connect_test_accounts": "EXTERNAL CREDENTIAL REQUIRED", "provider_reconciliation": "NOT EXECUTED"},
	}, nil
}

func runGeneratedStripeProperties(seeds int) error {
	for seed := int64(1); seed <= int64(seeds); seed++ {
		rng := rand.New(rand.NewSource(seed))
		sim := newDeterministicStripeSimulator(seed)
		intent, err := sim.createPaymentIntent(fmt.Sprintf("key-%d", seed), int64(100+rng.Intn(10000)), fmt.Sprintf("attempt-%d", seed), true)
		if err != nil {
			return err
		}
		_ = sim.authorize(intent.ID)
		_ = sim.capture(intent.ID)
		remaining := intent.AmountCents
		for i := 0; i < 24; i++ {
			switch rng.Intn(7) {
			case 0:
				amount := int64(1 + rng.Intn(int(intent.AmountCents)))
				err := sim.refund(intent.ID, fmt.Sprintf("refund-%d-%d", seed, i), amount)
				if err == nil {
					remaining -= amount
				}
			case 1:
				_ = sim.dispute(intent.ID, fmt.Sprintf("open-%d-%d", seed, i), true)
			case 2:
				_ = sim.dispute(intent.ID, fmt.Sprintf("close-%d-%d", seed, i), false)
			case 3:
				_ = sim.transfer(intent.ID, fmt.Sprintf("transfer-%d-%d", seed, i), fmt.Sprintf("attempt-%d", seed), max64(1, remaining/2))
			case 4:
				_ = sim.payout(intent.ID, fmt.Sprintf("release-%d-%d", seed, i), "released")
			case 5:
				_ = sim.payout(intent.ID, fmt.Sprintf("failure-%d-%d", seed, i), "failed")
			case 6:
				_, _ = sim.createPaymentIntent(fmt.Sprintf("key-%d", seed), intent.AmountCents, fmt.Sprintf("attempt-%d", seed), true)
			}
			if err := sim.invariant(); err != nil {
				return fmt.Errorf("seed %d step %d: %w", seed, i, err)
			}
		}
	}
	return nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func dispatchRelease(cmd string, args []string) bool {
	if cmd != "release" {
		return false
	}
	if len(args) == 0 {
		fatalf("release requires stripe-simulate, stripe-check, approvals-check, payment-activation-sign, or payment-activation-check")
	}
	switch args[0] {
	case "doctor", "inputs", "plan", "render", "apply", "launch", "status", "canary", "soak", "prove", "rollback", "destroy", "resume", "evidence", "go-no-go", "ui":
		dispatchLaunchRelease(args)
	case "stripe-simulate":
		fs := flag.NewFlagSet("release stripe-simulate", flag.ExitOnError)
		seed := fs.Int64("seed", 20260719, "deterministic simulator seed")
		sequences := fs.Int("sequences", 4096, "generated property sequence count")
		_ = fs.Parse(args[1:])
		if *sequences < 1000 {
			fatalf("stripe simulator requires at least 1000 generated sequences")
		}
		if err := runGeneratedStripeProperties(*sequences); err != nil {
			fatalf("stripe simulator properties: %v", err)
		}
		receipt, err := runDeterministicStripeSimulation(*seed)
		if err != nil {
			fatalf("stripe simulation: %v", err)
		}
		receipt.Properties["generated_sequences"] = true
		receipt.Generated = &struct {
			Count        int   `json:"count"`
			SeedStart    int64 `json:"seed_start"`
			SeedEnd      int64 `json:"seed_end"`
			StepsPerSeed int   `json:"steps_per_seed"`
		}{Count: *sequences, SeedStart: 1, SeedEnd: int64(*sequences), StepsPerSeed: 24}
		raw, _ := json.Marshal(receipt)
		fmt.Println(string(raw))
	case "stripe-check":
		cmdStripeCredentialCheck()
	case "approvals-check":
		cmdApprovalsCheck(args[1:])
	case "payment-activation-sign":
		cmdPaymentActivationSign(args[1:])
	case "payment-activation-check":
		cmdPaymentActivationCheck(args[1:])
	default:
		fatalf("unknown release command %q", args[0])
	}
	return true
}

func cmdStripeCredentialCheck() {
	classes := map[string]string{
		"secret_key":      stripeCredentialClass(getenvTrim("STRIPE_SECRET_KEY")),
		"restricted_key":  stripeCredentialClass(getenvTrim("STRIPE_RESTRICTED_KEY")),
		"publishable_key": stripeCredentialClass(getenvTrim("STRIPE_PUBLISHABLE_KEY")),
		"billing_webhook": stripeCredentialClass(getenvTrim("STRIPE_WEBHOOK_SECRET")),
		"connect_webhook": stripeCredentialClass(getenvTrim("MERC_CONNECT_WEBHOOK_SECRET")),
	}
	ready := classes["secret_key"] == "sk_test" && classes["billing_webhook"] == "webhook_present" && classes["connect_webhook"] == "webhook_present" && getenvTrim("STRIPE_WEBHOOK_SECRET") != getenvTrim("MERC_CONNECT_WEBHOOK_SECRET")
	for _, class := range classes {
		if class == "sk_live" || class == "rk_live" || class == "publishable_live" {
			ready = false
		}
	}
	raw, _ := json.Marshal(map[string]any{"schema_version": 1, "secret_values_printed": false, "network_accessed": false, "classes": classes, "sandbox_ready": ready})
	fmt.Println(string(raw))
	if !ready {
		fatalf("Stripe Sandbox inputs are absent or refused")
	}
}

func getenvTrim(name string) string { return strings.TrimSpace(os.Getenv(name)) }

func cmdApprovalsCheck(args []string) {
	fs := flag.NewFlagSet("release approvals-check", flag.ExitOnError)
	path := fs.String("bundle", getenvTrim("GOVERNANCE_APPROVAL_BUNDLE_PATH"), "external approval bundle")
	_ = fs.Parse(args)
	if *path == "" {
		fatalf("governance approval bundle is required")
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		fatalf("read approval bundle: %v", err)
	}
	expectedCommit := controlCommit
	if expectedCommit == "" || expectedCommit == "unknown" {
		if head, gitErr := exec.Command("git", "rev-parse", "HEAD").Output(); gitErr == nil {
			expectedCommit = strings.TrimSpace(string(head))
		}
	}
	digest, err := validateGovernanceApprovalBundle(raw, expectedCommit, time.Now())
	if err != nil {
		fatalf("governance approval bundle rejected: %v", err)
	}
	out, _ := json.Marshal(map[string]any{
		"schema_version":         1,
		"kind":                   "merc_governance_approval_validation",
		"status":                 "PASS",
		"candidate_commit":       expectedCommit,
		"approval_bundle_sha256": digest,
		"candidate_bound":        true,
		"human_approvals":        len(requiredGovernanceApprovalDomains),
		"technical_exercises":    len(requiredGovernanceExerciseDomains),
		"secret_values_printed":  false,
	})
	fmt.Println(string(out))
}
