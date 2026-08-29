package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	paymentModeEnv                       = "MERC_PAYMENT_MODE"
	stripeSecretKeyFileEnv               = "STRIPE_SECRET_KEY_FILE"
	livePaymentActivationFileEnv         = "MERC_LIVE_PAYMENT_ACTIVATION_FILE"
	livePaymentActivationDigestEnv       = "MERC_LIVE_PAYMENT_ACTIVATION_SHA256"
	livePaymentActivationHMACKeyEnv      = "MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY"
	livePaymentActivationHMACKeyFileEnv  = "MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY_FILE"
	maxLivePaymentActivationBytes        = 64 << 10
	maxLivePaymentActivationHMACKeyBytes = 4 << 10
	maxPaymentProviderSecretBytes        = 16 << 10
	maxLivePaymentActivationDuration     = 72 * time.Hour
	maxLivePaymentRecoveryDuration       = 30 * 24 * time.Hour
	maxLivePaymentActivationSigningLead  = 7 * 24 * time.Hour
	minLivePaymentActivationHMACKeyLen   = 32
)

type PaymentMode string

const (
	PaymentModeSealed PaymentMode = "sealed"
	PaymentModeTest   PaymentMode = "test"
	PaymentModeLive   PaymentMode = "live"
)

type PaymentOperation string

const (
	paymentOperationRead     PaymentOperation = "provider_read"
	paymentOperationSetup    PaymentOperation = "provider_setup"
	paymentOperationCharge   PaymentOperation = "buyer_charge"
	paymentOperationPayout   PaymentOperation = "supplier_payout"
	paymentOperationRefund   PaymentOperation = "buyer_refund"
	paymentOperationReversal PaymentOperation = "supplier_reversal"
	paymentOperationWebhook  PaymentOperation = "provider_webhook"
)

var (
	errPaymentAuthoritySealed = errors.New("payment authority is SEALED; provider access and value movement are disabled")
	activationIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
	fullCommitPattern         = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern             = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type LivePaymentApproval struct {
	Role      string `json:"role"`
	Approver  string `json:"approver"`
	Reference string `json:"reference"`
}

// LivePaymentActivation is deliberately narrow. It authorizes a single,
// immutable control-plane candidate for a short interval and caps every
// individual cash operation. Aggregate provider/account limits remain a
// separately required external control; this object does not pretend that an
// in-process cap can govern several replicas or survive disaster recovery.
type LivePaymentActivation struct {
	ActivationID            string                `json:"activation_id"`
	CandidateCommit         string                `json:"candidate_commit"`
	Environment             string                `json:"environment"`
	Currency                string                `json:"currency"`
	ValidFrom               time.Time             `json:"valid_from"`
	ExpiresAt               time.Time             `json:"expires_at"`
	RecoveryExpiresAt       time.Time             `json:"recovery_expires_at"`
	MaxSingleChargeMinor    int64                 `json:"max_single_charge_minor"`
	MaxSinglePayoutMinor    int64                 `json:"max_single_payout_minor"`
	MaxSingleRefundMinor    int64                 `json:"max_single_refund_minor"`
	MaxSingleReversalMinor  int64                 `json:"max_single_reversal_minor"`
	ExternalAggregateCapRef string                `json:"external_aggregate_cap_ref"`
	Approvals               []LivePaymentApproval `json:"approvals"`
}

type livePaymentActivationEnvelope struct {
	SchemaVersion int                   `json:"schema_version"`
	Activation    LivePaymentActivation `json:"activation"`
	HMACSHA256    string                `json:"hmac_sha256"`
}

type livePaymentActivationSignedBody struct {
	SchemaVersion int                   `json:"schema_version"`
	Activation    LivePaymentActivation `json:"activation"`
}

type PaymentAuthority struct {
	Mode             PaymentMode
	CredentialClass  string
	Activation       *LivePaymentActivation
	ActivationDigest string
	Active           bool
	RecoveryActive   bool
}

func (a PaymentAuthority) ProviderEnabled() bool {
	return a.Mode == PaymentModeTest || a.Mode == PaymentModeLive
}

func (a PaymentAuthority) LiveValueMovementEnabled() bool {
	return a.Mode == PaymentModeLive && a.Active && a.Activation != nil
}

// OperationallyReady distinguishes platform health from permission to create
// new value movement. Recovery-only LIVE authority must keep the process alive
// for provider webhooks, reconciliation, refunds and transfer reversals even
// though charges, payouts and setup calls remain denied.
func (a PaymentAuthority) OperationallyReady() bool {
	switch a.Mode {
	case PaymentModeSealed, PaymentModeTest:
		return true
	case PaymentModeLive:
		return a.Activation != nil && (a.Active || a.RecoveryActive)
	default:
		return false
	}
}

func parsePaymentMode(raw, cxEnv, credential string) (PaymentMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(PaymentModeSealed):
		return PaymentModeSealed, nil
	case string(PaymentModeTest):
		return PaymentModeTest, nil
	case string(PaymentModeLive):
		return PaymentModeLive, nil
	case "":
		// Production defaults to structural SEALED mode. A credential appearing
		// in the environment does not silently change production authority.
		if isProductionEnv(cxEnv) {
			return PaymentModeSealed, nil
		}
		switch stripeCredentialClass(credential) {
		case "missing":
			return PaymentModeSealed, nil
		case "sk_test", "rk_test":
			// Backward compatibility for development tests and the existing
			// supervised Stripe-test tooling.
			return PaymentModeTest, nil
		default:
			return "", errors.New("MERC_PAYMENT_MODE is required when a non-test Stripe credential is present")
		}
	default:
		return "", fmt.Errorf("%s must be sealed, test, or live", paymentModeEnv)
	}
}

func paymentActivationSignedBytes(envelope livePaymentActivationEnvelope) ([]byte, error) {
	return json.Marshal(livePaymentActivationSignedBody{
		SchemaVersion: envelope.SchemaVersion,
		Activation:    envelope.Activation,
	})
}

func livePaymentActivationSigningValidationTime(now, validFrom time.Time) (time.Time, error) {
	now = now.UTC()
	if validFrom.IsZero() || !validFrom.After(now) {
		return now, nil
	}
	if validFrom.Sub(now) > maxLivePaymentActivationSigningLead {
		return time.Time{}, fmt.Errorf(
			"live payment activation cannot be signed more than %s before valid_from",
			maxLivePaymentActivationSigningLead,
		)
	}
	// Structural and candidate validation can run at the future boundary
	// without weakening runtime enforcement, which always uses the real clock.
	return validFrom, nil
}

func loadLivePaymentActivationHMACKey() (string, error) {
	path := strings.TrimSpace(os.Getenv(livePaymentActivationHMACKeyFileEnv))
	if path == "" {
		key := strings.TrimSpace(os.Getenv(livePaymentActivationHMACKeyEnv))
		if len(key) < minLivePaymentActivationHMACKeyLen {
			return "", fmt.Errorf("%s or %s must provide at least %d bytes",
				livePaymentActivationHMACKeyFileEnv, livePaymentActivationHMACKeyEnv,
				minLivePaymentActivationHMACKeyLen)
		}
		return key, nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s must be an absolute path", livePaymentActivationHMACKeyFileEnv)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat live payment activation HMAC key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o027 != 0 {
		return "", errors.New("live payment activation HMAC key must be a regular file, not group-writable, and inaccessible to other users")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read live payment activation HMAC key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if len(key) < minLivePaymentActivationHMACKeyLen || len(key) > maxLivePaymentActivationHMACKeyBytes {
		return "", fmt.Errorf("live payment activation HMAC key must contain %d..%d bytes",
			minLivePaymentActivationHMACKeyLen, maxLivePaymentActivationHMACKeyBytes)
	}
	return key, nil
}

func validateLivePaymentActivation(
	now time.Time,
	build ControlBuildInfo,
	settlementCurrency string,
	envelope livePaymentActivationEnvelope,
) (active bool, recoveryActive bool, err error) {
	if envelope.SchemaVersion != 1 {
		return false, false, fmt.Errorf("unsupported live payment activation schema version %d", envelope.SchemaVersion)
	}
	a := envelope.Activation
	if !activationIDPattern.MatchString(a.ActivationID) {
		return false, false, errors.New("live payment activation_id is invalid")
	}
	if !fullCommitPattern.MatchString(a.CandidateCommit) {
		return false, false, errors.New("live payment candidate_commit must be a full lowercase Git SHA-1")
	}
	if build.Modified || !fullCommitPattern.MatchString(build.Commit) || build.Commit != a.CandidateCommit {
		return false, false, fmt.Errorf(
			"live payment activation is not bound to this clean candidate: activation=%s build=%s modified=%t",
			a.CandidateCommit, build.Commit, build.Modified,
		)
	}
	if a.Environment != "production" {
		return false, false, errors.New("live payment activation environment must be production")
	}
	if a.Currency != settlementCurrency {
		return false, false, fmt.Errorf(
			"live payment activation currency %q must exactly match normalized settlement currency %q",
			a.Currency, settlementCurrency,
		)
	}
	if a.ValidFrom.IsZero() || a.ExpiresAt.IsZero() || a.RecoveryExpiresAt.IsZero() {
		return false, false, errors.New("live payment activation validity timestamps are required")
	}
	if !a.ValidFrom.Before(a.ExpiresAt) ||
		a.ExpiresAt.Sub(a.ValidFrom) > maxLivePaymentActivationDuration {
		return false, false, fmt.Errorf("live payment activation window must be positive and at most %s", maxLivePaymentActivationDuration)
	}
	if a.RecoveryExpiresAt.Before(a.ExpiresAt) ||
		a.RecoveryExpiresAt.Sub(a.ExpiresAt) > maxLivePaymentRecoveryDuration {
		return false, false, fmt.Errorf("live payment recovery window must end from 0 to %s after activation expiry", maxLivePaymentRecoveryDuration)
	}
	if now.Before(a.ValidFrom) {
		return false, false, errors.New("live payment activation is not valid yet")
	}
	if !now.Before(a.RecoveryExpiresAt) {
		return false, false, errors.New("live payment activation and recovery windows have expired")
	}
	for name, amount := range map[string]int64{
		"max_single_charge_minor":   a.MaxSingleChargeMinor,
		"max_single_payout_minor":   a.MaxSinglePayoutMinor,
		"max_single_refund_minor":   a.MaxSingleRefundMinor,
		"max_single_reversal_minor": a.MaxSingleReversalMinor,
	} {
		if amount <= 0 {
			return false, false, fmt.Errorf("%s must be positive", name)
		}
	}
	if strings.TrimSpace(a.ExternalAggregateCapRef) == "" {
		return false, false, errors.New("external_aggregate_cap_ref is required; per-operation limits are not an aggregate cap")
	}
	requiredRoles := map[string]bool{
		"payments": false, "release_manager": false, "security": false,
	}
	seenRoles := make(map[string]bool, len(a.Approvals))
	for _, approval := range a.Approvals {
		role := strings.ToLower(strings.TrimSpace(approval.Role))
		if _, required := requiredRoles[role]; !required {
			return false, false, fmt.Errorf("unsupported live payment approval role %q", approval.Role)
		}
		if seenRoles[role] {
			return false, false, fmt.Errorf("duplicate live payment approval role %q", approval.Role)
		}
		if strings.TrimSpace(approval.Approver) == "" || strings.TrimSpace(approval.Reference) == "" {
			return false, false, fmt.Errorf("live payment approval %q requires approver and reference", role)
		}
		seenRoles[role], requiredRoles[role] = true, true
	}
	for role, present := range requiredRoles {
		if !present {
			return false, false, fmt.Errorf("live payment activation is missing %s approval", role)
		}
	}
	return now.Before(a.ExpiresAt), true, nil
}

func loadLivePaymentActivationAt(
	now time.Time,
	build ControlBuildInfo,
	settlementCurrency string,
) (LivePaymentActivation, string, bool, bool, error) {
	path := strings.TrimSpace(os.Getenv(livePaymentActivationFileEnv))
	if path == "" || !filepath.IsAbs(path) {
		return LivePaymentActivation{}, "", false, false,
			fmt.Errorf("%s must be an absolute path", livePaymentActivationFileEnv)
	}
	info, err := os.Stat(path)
	if err != nil {
		return LivePaymentActivation{}, "", false, false, fmt.Errorf("stat live payment activation: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o027 != 0 {
		return LivePaymentActivation{}, "", false, false,
			errors.New("live payment activation must be a regular file, not group-writable, and inaccessible to other users")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return LivePaymentActivation{}, "", false, false, fmt.Errorf("read live payment activation: %w", err)
	}
	if len(raw) == 0 || len(raw) > maxLivePaymentActivationBytes {
		return LivePaymentActivation{}, "", false, false, errors.New("live payment activation file has an invalid size")
	}
	digestBytes := sha256.Sum256(raw)
	digest := hex.EncodeToString(digestBytes[:])
	expectedDigest := strings.ToLower(strings.TrimSpace(os.Getenv(livePaymentActivationDigestEnv)))
	if !digestPattern.MatchString(expectedDigest) || !hmac.Equal([]byte(digest), []byte(expectedDigest)) {
		return LivePaymentActivation{}, "", false, false,
			errors.New("live payment activation digest is absent, invalid, or does not match")
	}
	var envelope livePaymentActivationEnvelope
	if err := decodeStrictJSONObject(raw, &envelope); err != nil {
		return LivePaymentActivation{}, "", false, false, fmt.Errorf("decode live payment activation: %w", err)
	}
	if !digestPattern.MatchString(strings.ToLower(envelope.HMACSHA256)) {
		return LivePaymentActivation{}, "", false, false, errors.New("live payment activation HMAC is invalid")
	}
	hmacKey, err := loadLivePaymentActivationHMACKey()
	if err != nil {
		return LivePaymentActivation{}, "", false, false, err
	}
	signed, err := paymentActivationSignedBytes(envelope)
	if err != nil {
		return LivePaymentActivation{}, "", false, false, fmt.Errorf("encode live payment activation signature body: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(hmacKey))
	_, _ = mac.Write(signed)
	wantMAC := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(wantMAC), []byte(strings.ToLower(envelope.HMACSHA256))) {
		return LivePaymentActivation{}, "", false, false, errors.New("live payment activation HMAC does not match")
	}
	active, recoveryActive, err := validateLivePaymentActivation(
		now, build, settlementCurrency, envelope,
	)
	if err != nil {
		return LivePaymentActivation{}, "", false, false, err
	}
	return envelope.Activation, digest, active, recoveryActive, nil
}

// loadPaymentAuthorityAt is the only authority constructor. suppliedSecret is
// explicit so direct StripePayout tests cannot accidentally depend on ambient
// process state; production requires it to match STRIPE_SECRET_KEY exactly.
func loadPaymentAuthorityAt(now time.Time, build ControlBuildInfo, suppliedSecret string) (PaymentAuthority, error) {
	cxEnv := strings.TrimSpace(os.Getenv("MERC_ENV"))
	envSecret, err := configuredStripeKey()
	if err != nil {
		return PaymentAuthority{}, err
	}
	suppliedSecret = strings.TrimSpace(suppliedSecret)
	if suppliedSecret == "" {
		suppliedSecret = envSecret
	}
	mode, err := parsePaymentMode(os.Getenv(paymentModeEnv), cxEnv, suppliedSecret)
	if err != nil {
		return PaymentAuthority{}, err
	}
	credentialClass := stripeCredentialClass(suppliedSecret)
	authority := PaymentAuthority{Mode: mode, CredentialClass: credentialClass}
	switch mode {
	case PaymentModeSealed:
		if suppliedSecret != "" || envSecret != "" {
			return PaymentAuthority{}, errors.New("SEALED payment mode forbids STRIPE_SECRET_KEY")
		}
		return authority, nil
	case PaymentModeTest:
		if isProductionEnv(cxEnv) {
			return PaymentAuthority{}, errors.New("TEST payment mode is unavailable when MERC_ENV=production")
		}
		if credentialClass != "sk_test" && credentialClass != "rk_test" {
			return PaymentAuthority{}, errors.New("TEST payment mode requires sk_test_* or rk_test_*")
		}
		if envSecret != "" && !hmac.Equal([]byte(envSecret), []byte(suppliedSecret)) {
			return PaymentAuthority{}, errors.New("Stripe provider credential does not match the configured TEST credential")
		}
		authority.Active, authority.RecoveryActive = true, true
		return authority, nil
	case PaymentModeLive:
		if !isProductionEnv(cxEnv) {
			return PaymentAuthority{}, errors.New("LIVE payment mode requires MERC_ENV=production")
		}
		if strings.TrimSpace(os.Getenv(stripeSecretKeyFileEnv)) == "" ||
			strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY")) != "" {
			return PaymentAuthority{}, fmt.Errorf(
				"LIVE payment mode requires %s and forbids environment-inline STRIPE_SECRET_KEY",
				stripeSecretKeyFileEnv,
			)
		}
		if strings.TrimSpace(os.Getenv(livePaymentActivationHMACKeyFileEnv)) == "" ||
			strings.TrimSpace(os.Getenv(livePaymentActivationHMACKeyEnv)) != "" {
			return PaymentAuthority{}, fmt.Errorf(
				"LIVE payment mode requires %s and forbids environment-inline %s",
				livePaymentActivationHMACKeyFileEnv, livePaymentActivationHMACKeyEnv,
			)
		}
		if credentialClass != "sk_live" && credentialClass != "rk_live" {
			return PaymentAuthority{}, errors.New("LIVE payment mode requires sk_live_* or rk_live_*")
		}
		if envSecret == "" || !hmac.Equal([]byte(envSecret), []byte(suppliedSecret)) {
			return PaymentAuthority{}, errors.New("Stripe provider credential does not match the configured LIVE credential")
		}
		settlement, err := SettlementCurrency()
		if err != nil {
			return PaymentAuthority{}, err
		}
		activation, digest, active, recoveryActive, err := loadLivePaymentActivationAt(
			now, build, settlement.Code(),
		)
		if err != nil {
			return PaymentAuthority{}, err
		}
		authority.Activation = &activation
		authority.ActivationDigest = digest
		authority.Active = active
		authority.RecoveryActive = recoveryActive
		return authority, nil
	default:
		return PaymentAuthority{}, fmt.Errorf("unsupported payment mode %q", mode)
	}
}

func currentPaymentAuthority() (PaymentAuthority, error) {
	return loadPaymentAuthorityAt(time.Now().UTC(), currentControlBuildInfo(), stripeKey())
}

func validatePaymentAuthorityStartup(
	authority PaymentAuthority,
	provider, billingWebhookSecret, connectWebhookSecret, connectClientID, payoutExport string,
) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if err := validatePaymentProviderMode(os.Getenv("MERC_ENV"), provider); err != nil {
		return err
	}
	switch authority.Mode {
	case PaymentModeSealed:
		if provider != "" && provider != "none" {
			return errors.New("SEALED payment mode requires MERC_PAYMENT_PROVIDER=none or unset")
		}
		for name, value := range map[string]string{
			"STRIPE_WEBHOOK_SECRET":             billingWebhookSecret,
			"MERC_CONNECT_WEBHOOK_SECRET":       connectWebhookSecret,
			"MERC_CONNECT_CLIENT_ID":            connectClientID,
			"MERC_PAYOUT_EXPORT":                payoutExport,
			livePaymentActivationFileEnv:        os.Getenv(livePaymentActivationFileEnv),
			livePaymentActivationDigestEnv:      os.Getenv(livePaymentActivationDigestEnv),
			livePaymentActivationHMACKeyEnv:     os.Getenv(livePaymentActivationHMACKeyEnv),
			livePaymentActivationHMACKeyFileEnv: os.Getenv(livePaymentActivationHMACKeyFileEnv),
		} {
			if strings.TrimSpace(value) != "" {
				return fmt.Errorf("SEALED payment mode forbids %s", name)
			}
		}
		return nil
	case PaymentModeTest:
		if provider == "none" {
			return errors.New("TEST payment mode cannot use MERC_PAYMENT_PROVIDER=none")
		}
		if strings.TrimSpace(payoutExport) != "" {
			return errors.New("TEST payment mode refuses MERC_PAYOUT_EXPORT")
		}
		return nil
	case PaymentModeLive:
		if !authority.OperationallyReady() {
			return errors.New("LIVE payment activation is outside both its value-movement and recovery windows")
		}
		if provider == "none" {
			return errors.New("LIVE payment mode cannot use MERC_PAYMENT_PROVIDER=none")
		}
		if !strings.HasPrefix(billingWebhookSecret, "whsec_") ||
			!strings.HasPrefix(connectWebhookSecret, "whsec_") ||
			billingWebhookSecret == connectWebhookSecret {
			return errors.New("LIVE payment mode requires distinct billing and Connect whsec_* secrets")
		}
		if !strings.HasPrefix(strings.TrimSpace(connectClientID), "ca_") {
			return errors.New("LIVE payment mode requires MERC_CONNECT_CLIENT_ID=ca_*")
		}
		if strings.TrimSpace(payoutExport) != "" {
			return errors.New("LIVE payment mode refuses MERC_PAYOUT_EXPORT")
		}
		return nil
	default:
		return fmt.Errorf("unsupported payment mode %q", authority.Mode)
	}
}

func authorizePaymentOperationAt(
	now time.Time,
	build ControlBuildInfo,
	operation PaymentOperation,
	amountMinor int64,
	currency string,
	secret string,
) (PaymentAuthority, error) {
	authority, err := loadPaymentAuthorityAt(now, build, secret)
	if err != nil {
		return PaymentAuthority{}, err
	}
	if authority.Mode == PaymentModeSealed {
		return PaymentAuthority{}, errPaymentAuthoritySealed
	}
	if authority.Mode == PaymentModeTest {
		return authority, nil
	}
	if authority.Activation == nil {
		return PaymentAuthority{}, errors.New("LIVE payment activation is unavailable")
	}
	recoveryOperation := operation == paymentOperationRefund ||
		operation == paymentOperationReversal ||
		operation == paymentOperationWebhook ||
		operation == paymentOperationRead
	if recoveryOperation {
		if !authority.RecoveryActive {
			return PaymentAuthority{}, errors.New("LIVE payment recovery window has expired")
		}
	} else if !authority.Active {
		return PaymentAuthority{}, errors.New("LIVE payment value-movement window has expired")
	}
	if currency != "" && strings.ToLower(strings.TrimSpace(currency)) != authority.Activation.Currency {
		return PaymentAuthority{}, fmt.Errorf(
			"payment operation currency %q does not match activated currency %q",
			currency, authority.Activation.Currency,
		)
	}
	var capMinor int64
	switch operation {
	case paymentOperationCharge:
		capMinor = authority.Activation.MaxSingleChargeMinor
	case paymentOperationPayout:
		capMinor = authority.Activation.MaxSinglePayoutMinor
	case paymentOperationRefund:
		capMinor = authority.Activation.MaxSingleRefundMinor
	case paymentOperationReversal:
		capMinor = authority.Activation.MaxSingleReversalMinor
	case paymentOperationRead, paymentOperationSetup, paymentOperationWebhook:
		return authority, nil
	default:
		return PaymentAuthority{}, fmt.Errorf("unsupported payment operation %q", operation)
	}
	if amountMinor <= 0 || amountMinor > capMinor {
		return PaymentAuthority{}, fmt.Errorf(
			"%s amount %d exceeds activated per-operation range 1..%d minor units",
			operation, amountMinor, capMinor,
		)
	}
	return authority, nil
}

func authorizePaymentOperation(
	operation PaymentOperation,
	amountMinor int64,
	currency string,
	secret string,
) (PaymentAuthority, error) {
	return authorizePaymentOperationAt(
		time.Now().UTC(), currentControlBuildInfo(), operation, amountMinor, currency, secret,
	)
}
