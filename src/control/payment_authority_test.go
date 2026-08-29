package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testLiveActivationHMACKey = "separate-live-payment-activation-key-32-bytes-minimum"

func cleanBuildForPaymentAuthority() ControlBuildInfo {
	return ControlBuildInfo{
		Version: "test", Commit: strings.Repeat("a", 40), BuildDate: "test", Modified: false,
	}
}

func validLivePaymentActivation(now time.Time, build ControlBuildInfo) livePaymentActivationEnvelope {
	return livePaymentActivationEnvelope{
		SchemaVersion: 1,
		Activation: LivePaymentActivation{
			ActivationID:            "live-payment-test-001",
			CandidateCommit:         build.Commit,
			Environment:             "production",
			Currency:                "usd",
			ValidFrom:               now.Add(-time.Minute).UTC(),
			ExpiresAt:               now.Add(time.Hour).UTC(),
			RecoveryExpiresAt:       now.Add(24 * time.Hour).UTC(),
			MaxSingleChargeMinor:    2_000,
			MaxSinglePayoutMinor:    1_000,
			MaxSingleRefundMinor:    2_000,
			MaxSingleReversalMinor:  1_000,
			ExternalAggregateCapRef: "stripe-workbench-limit/live-payment-test-001",
			Approvals: []LivePaymentApproval{
				{Role: "payments", Approver: "payments@example.test", Reference: "PAY-001"},
				{Role: "release_manager", Approver: "release@example.test", Reference: "REL-001"},
				{Role: "security", Approver: "security@example.test", Reference: "SEC-001"},
			},
		},
	}
}

func installLivePaymentActivation(
	t *testing.T,
	envelope livePaymentActivationEnvelope,
) (path string, raw []byte) {
	t.Helper()
	signed, err := paymentActivationSignedBytes(envelope)
	must(t, err)
	mac := hmac.New(sha256.New, []byte(testLiveActivationHMACKey))
	_, _ = mac.Write(signed)
	envelope.HMACSHA256 = hex.EncodeToString(mac.Sum(nil))
	raw, err = json.Marshal(envelope)
	must(t, err)
	path = filepath.Join(t.TempDir(), "live-payment-activation.json")
	must(t, os.WriteFile(path, raw, 0o600))
	digest := sha256.Sum256(raw)
	t.Setenv(livePaymentActivationFileEnv, path)
	t.Setenv(livePaymentActivationDigestEnv, hex.EncodeToString(digest[:]))
	keyPath := filepath.Join(t.TempDir(), "live-payment-activation-hmac-key")
	must(t, os.WriteFile(keyPath, []byte(testLiveActivationHMACKey+"\n"), 0o600))
	t.Setenv(livePaymentActivationHMACKeyFileEnv, keyPath)
	t.Setenv(livePaymentActivationHMACKeyEnv, "")
	return path, raw
}

func configureLivePaymentTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MERC_ENV", "production")
	t.Setenv(paymentModeEnv, "live")
	keyPath := filepath.Join(t.TempDir(), "stripe-secret-key")
	must(t, os.WriteFile(keyPath, []byte("sk_live_payment_authority_test\n"), 0o600))
	t.Setenv(stripeSecretKeyFileEnv, keyPath)
	t.Setenv("STRIPE_SECRET_KEY", "")
	t.Setenv("MERC_PAYMENT_PROVIDER", "stripe")
	t.Setenv(livePaymentActivationHMACKeyEnv, "")
}

func TestProductionDefaultsToSealedAndCredentialCannotArmIt(t *testing.T) {
	mode, err := parsePaymentMode("", "production", "sk_live_accident")
	if err != nil || mode != PaymentModeSealed {
		t.Fatalf("production default = %q, %v; want SEALED", mode, err)
	}
	t.Setenv("MERC_ENV", "production")
	t.Setenv(paymentModeEnv, "")
	t.Setenv("STRIPE_SECRET_KEY", "sk_live_accident")
	if _, err := loadPaymentAuthorityAt(time.Now(), cleanBuildForPaymentAuthority(), "sk_live_accident"); err == nil {
		t.Fatal("a live credential armed production without explicit LIVE authority")
	}
}

func TestSealedModeStructurallyRefusesProviderNetwork(t *testing.T) {
	t.Setenv("MERC_ENV", "production")
	t.Setenv(paymentModeEnv, "sealed")
	t.Setenv("STRIPE_SECRET_KEY", "")

	var calls atomic.Int64
	oldClient := stripeHTTPClient
	stripeHTTPClient = &http.Client{Transport: authorityRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("network must not be reached")
	})}
	t.Cleanup(func() { stripeHTTPClient = oldClient })

	_, err := stripeForm(context.Background(), "customers", nil, "")
	if !errors.Is(err, errPaymentAuthoritySealed) {
		t.Fatalf("SEALED provider refusal = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("SEALED mode made %d provider request(s)", calls.Load())
	}
}

func TestTestModeRequiresTestCredential(t *testing.T) {
	t.Setenv("MERC_ENV", "development")
	t.Setenv(paymentModeEnv, "test")
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_authority")
	authority, err := loadPaymentAuthorityAt(time.Now(), currentControlBuildInfo(), "sk_test_authority")
	if err != nil || authority.Mode != PaymentModeTest || !authority.ProviderEnabled() {
		t.Fatalf("TEST authority = %+v, %v", authority, err)
	}
	t.Setenv("STRIPE_SECRET_KEY", "sk_live_wrong_mode")
	if _, err := loadPaymentAuthorityAt(time.Now(), currentControlBuildInfo(), "sk_live_wrong_mode"); err == nil {
		t.Fatal("TEST mode accepted a live credential")
	}
	t.Setenv("MERC_ENV", "production")
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_authority")
	if _, err := loadPaymentAuthorityAt(time.Now(), currentControlBuildInfo(), "sk_test_authority"); err == nil {
		t.Fatal("production accepted TEST payment authority")
	}
}

func TestLiveModeRequiresPermissionRestrictedHMACKeyFile(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	build := cleanBuildForPaymentAuthority()
	configureLivePaymentTestEnv(t)
	t.Setenv(livePaymentActivationHMACKeyEnv, testLiveActivationHMACKey)
	t.Setenv(livePaymentActivationHMACKeyFileEnv, "")
	if _, err := loadPaymentAuthorityAt(now, build, "sk_live_payment_authority_test"); err == nil {
		t.Fatal("LIVE mode accepted an environment-inline activation HMAC key")
	}

	envelope := validLivePaymentActivation(now, build)
	installLivePaymentActivation(t, envelope)
	keyPath := os.Getenv(livePaymentActivationHMACKeyFileEnv)
	must(t, os.Chmod(keyPath, 0o644))
	if _, err := loadPaymentAuthorityAt(now, build, "sk_live_payment_authority_test"); err == nil {
		t.Fatal("LIVE mode accepted an activation HMAC key readable by other users")
	}
}

func TestLiveModeRequiresPermissionRestrictedStripeKeyFile(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	build := cleanBuildForPaymentAuthority()
	configureLivePaymentTestEnv(t)
	installLivePaymentActivation(t, validLivePaymentActivation(now, build))

	keyPath := os.Getenv(stripeSecretKeyFileEnv)
	must(t, os.Chmod(keyPath, 0o644))
	if _, err := loadPaymentAuthorityAt(now, build, "sk_live_payment_authority_test"); err == nil {
		t.Fatal("LIVE mode accepted a Stripe key readable by other users")
	}
}

func TestLiveModeRejectsInlineStripeKey(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	build := cleanBuildForPaymentAuthority()
	configureLivePaymentTestEnv(t)
	installLivePaymentActivation(t, validLivePaymentActivation(now, build))
	t.Setenv(stripeSecretKeyFileEnv, "")
	t.Setenv("STRIPE_SECRET_KEY", "sk_live_payment_authority_test")

	if _, err := loadPaymentAuthorityAt(now, build, "sk_live_payment_authority_test"); err == nil {
		t.Fatal("LIVE mode accepted an environment-inline Stripe key")
	}
}

func TestLiveActivationSigningLeadIsBounded(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)
	validationTime, err := livePaymentActivationSigningValidationTime(now, future)
	if err != nil || !validationTime.Equal(future) {
		t.Fatalf("bounded pre-staging validation time = %s, %v; want %s", validationTime, err, future)
	}
	if _, err := livePaymentActivationSigningValidationTime(
		now, now.Add(maxLivePaymentActivationSigningLead+time.Nanosecond),
	); err == nil {
		t.Fatal("signer accepted an activation beyond the bounded pre-staging lead")
	}
	validationTime, err = livePaymentActivationSigningValidationTime(now, now.Add(-time.Minute))
	if err != nil || !validationTime.Equal(now) {
		t.Fatalf("active-window signing validation time = %s, %v; want %s", validationTime, err, now)
	}
}

func TestLiveActivationBindsCandidateWindowCapsAndApprovals(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	build := cleanBuildForPaymentAuthority()
	configureLivePaymentTestEnv(t)
	installLivePaymentActivation(t, validLivePaymentActivation(now, build))

	authority, err := loadPaymentAuthorityAt(now, build, "sk_live_payment_authority_test")
	mustf(t, err, "valid LIVE authority rejected: %v")
	if !authority.LiveValueMovementEnabled() || authority.Activation == nil ||
		authority.Activation.CandidateCommit != build.Commit {
		t.Fatalf("LIVE authority incomplete: %+v", authority)
	}
	if _, err := authorizePaymentOperationAt(
		now, build, paymentOperationCharge, 2_000, "usd", "sk_live_payment_authority_test",
	); err != nil {
		t.Fatalf("charge at cap rejected: %v", err)
	}
	if _, err := authorizePaymentOperationAt(
		now, build, paymentOperationCharge, 2_001, "usd", "sk_live_payment_authority_test",
	); err == nil {
		t.Fatal("charge above activated cap was accepted")
	}

	wrongBuild := build
	wrongBuild.Commit = strings.Repeat("b", 40)
	if _, err := loadPaymentAuthorityAt(now, wrongBuild, "sk_live_payment_authority_test"); err == nil {
		t.Fatal("activation accepted a different candidate")
	}
}

func TestExpiredLiveWindowAllowsOnlyBoundedRecovery(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	build := cleanBuildForPaymentAuthority()
	envelope := validLivePaymentActivation(now, build)
	envelope.Activation.ValidFrom = now.Add(-2 * time.Hour)
	envelope.Activation.ExpiresAt = now.Add(-time.Hour)
	envelope.Activation.RecoveryExpiresAt = now.Add(time.Hour)
	configureLivePaymentTestEnv(t)
	installLivePaymentActivation(t, envelope)

	authority, err := loadPaymentAuthorityAt(now, build, "sk_live_payment_authority_test")
	mustf(t, err, "recovery authority rejected: %v")
	if authority.Active || !authority.RecoveryActive {
		t.Fatalf("window state = active:%t recovery:%t", authority.Active, authority.RecoveryActive)
	}
	if !authority.OperationallyReady() {
		t.Fatal("recovery-only LIVE authority would fail the production healthcheck")
	}
	if err := validatePaymentAuthorityStartup(
		authority, "stripe", "whsec_billing", "whsec_connect", "ca_connect", "",
	); err != nil {
		t.Fatalf("recovery-only LIVE authority cannot restart safely: %v", err)
	}
	if _, err := authorizePaymentOperationAt(
		now, build, paymentOperationPayout, 1, "usd", "sk_live_payment_authority_test",
	); err == nil {
		t.Fatal("new payout accepted after LIVE value-movement expiry")
	}
	if _, err := authorizePaymentOperationAt(
		now, build, paymentOperationRefund, 2_000, "usd", "sk_live_payment_authority_test",
	); err != nil {
		t.Fatalf("bounded refund recovery rejected: %v", err)
	}
	if _, err := authorizePaymentOperationAt(
		now, build, paymentOperationRefund, 2_001, "usd", "sk_live_payment_authority_test",
	); err == nil {
		t.Fatal("refund above recovery cap was accepted")
	}

	if _, err := loadPaymentAuthorityAt(
		envelope.Activation.RecoveryExpiresAt.Add(time.Nanosecond),
		build, "sk_live_payment_authority_test",
	); err == nil {
		t.Fatal("authority remained operational after the bounded recovery window")
	}
}

func TestLiveActivationTamperAndWeakApprovalFailClosed(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	build := cleanBuildForPaymentAuthority()
	configureLivePaymentTestEnv(t)
	envelope := validLivePaymentActivation(now, build)
	path, raw := installLivePaymentActivation(t, envelope)
	raw[len(raw)-2] ^= 1
	must(t, os.WriteFile(path, raw, 0o600))
	if _, err := loadPaymentAuthorityAt(now, build, "sk_live_payment_authority_test"); err == nil {
		t.Fatal("tampered activation file was accepted")
	}

	envelope = validLivePaymentActivation(now, build)
	envelope.Activation.Approvals = envelope.Activation.Approvals[:2]
	installLivePaymentActivation(t, envelope)
	if _, err := loadPaymentAuthorityAt(now, build, "sk_live_payment_authority_test"); err == nil {
		t.Fatal("activation missing a required approval was accepted")
	}
}

func TestLiveCredentialWithoutActivationMakesZeroNetworkCalls(t *testing.T) {
	configureLivePaymentTestEnv(t)
	t.Setenv(livePaymentActivationFileEnv, "")
	var calls atomic.Int64
	oldClient := stripeHTTPClient
	stripeHTTPClient = &http.Client{Transport: authorityRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("network must not be reached")
	})}
	t.Cleanup(func() { stripeHTTPClient = oldClient })

	_, err := stripeForm(context.Background(), "payment_intents", mapValues(
		"amount", "100", "currency", "usd",
	), "live-without-activation")
	if err == nil {
		t.Fatal("live credential without activation was accepted")
	}
	if calls.Load() != 0 {
		t.Fatalf("unactivated live credential made %d provider request(s)", calls.Load())
	}
}

type authorityRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn authorityRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func mapValues(values ...string) map[string][]string {
	out := make(map[string][]string, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		out[values[i]] = []string{values[i+1]}
	}
	return out
}
