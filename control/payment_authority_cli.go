package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cmdPaymentActivationSign creates the exact canonical envelope verified by the
// server. The HMAC key is read from a permission-restricted file (with an
// inline development fallback) and never printed. Output uses O_EXCL so a
// reviewed activation cannot be silently overwritten.
func cmdPaymentActivationSign(args []string) {
	fs := flag.NewFlagSet("release payment-activation-sign", flag.ExitOnError)
	input := fs.String("input", "", "unsigned activation envelope JSON")
	output := fs.String("output", "", "new signed activation envelope path")
	_ = fs.Parse(args)
	if fs.NArg() != 0 || !filepath.IsAbs(*input) || !filepath.IsAbs(*output) {
		fatalf("--input and --output must be absolute paths and no positional arguments are accepted")
	}
	raw, err := os.ReadFile(*input)
	if err != nil {
		fatalf("read unsigned live payment activation: %v", err)
	}
	var envelope livePaymentActivationEnvelope
	if err := decodeStrictJSONObject(raw, &envelope); err != nil {
		fatalf("decode unsigned live payment activation: %v", err)
	}
	if strings.TrimSpace(envelope.HMACSHA256) != "" {
		fatalf("unsigned activation input must have an empty hmac_sha256")
	}
	settlement, err := LoadSettlementCurrencyFromEnv()
	if err != nil {
		fatalf("settlement currency: %v", err)
	}
	build := currentControlBuildInfo()
	validationTime, err := livePaymentActivationSigningValidationTime(
		time.Now().UTC(), envelope.Activation.ValidFrom,
	)
	if err != nil {
		fatalf("unsigned live payment activation signing window: %v", err)
	}
	active, _, err := validateLivePaymentActivation(
		validationTime, build, settlement.Code(), envelope,
	)
	if err != nil || !active {
		fatalf("unsigned live payment activation is not active and candidate-bound: %v", err)
	}
	key, err := loadLivePaymentActivationHMACKey()
	if err != nil {
		fatalf("live payment activation HMAC key: %v", err)
	}
	signedBody, err := paymentActivationSignedBytes(envelope)
	if err != nil {
		fatalf("encode live payment activation signature body: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write(signedBody)
	envelope.HMACSHA256 = hex.EncodeToString(mac.Sum(nil))
	signedRaw, err := json.Marshal(envelope)
	if err != nil {
		fatalf("encode signed live payment activation: %v", err)
	}
	out, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fatalf("create signed live payment activation without overwrite: %v", err)
	}
	if _, err := out.Write(signedRaw); err != nil {
		_ = out.Close()
		fatalf("write signed live payment activation: %v", err)
	}
	if err := out.Close(); err != nil {
		fatalf("close signed live payment activation: %v", err)
	}
	digest := sha256.Sum256(signedRaw)
	receipt, _ := json.Marshal(map[string]any{
		"schema_version":        1,
		"status":                "SIGNED",
		"activation_id":         envelope.Activation.ActivationID,
		"candidate_commit":      envelope.Activation.CandidateCommit,
		"expires_at":            envelope.Activation.ExpiresAt,
		"recovery_expires_at":   envelope.Activation.RecoveryExpiresAt,
		"output":                *output,
		"sha256":                hex.EncodeToString(digest[:]),
		"secret_values_printed": false,
	})
	fmt.Println(string(receipt))
}

func cmdPaymentActivationCheck(args []string) {
	fs := flag.NewFlagSet("release payment-activation-check", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() != 0 {
		fatalf("payment-activation-check accepts no positional arguments")
	}
	authority, err := currentPaymentAuthority()
	if err != nil {
		fatalf("payment authority check: %v", err)
	}
	receipt := map[string]any{
		"schema_version":          1,
		"status":                  "PASS",
		"mode":                    authority.Mode,
		"provider_enabled":        authority.ProviderEnabled(),
		"live_value_movement":     authority.LiveValueMovementEnabled(),
		"payment_recovery_active": authority.RecoveryActive,
		"secret_values_printed":   false,
		"network_accessed":        false,
	}
	if authority.Activation != nil {
		receipt["activation_id"] = authority.Activation.ActivationID
		receipt["candidate_commit"] = authority.Activation.CandidateCommit
		receipt["activation_sha256"] = authority.ActivationDigest
		receipt["expires_at"] = authority.Activation.ExpiresAt
		receipt["recovery_expires_at"] = authority.Activation.RecoveryExpiresAt
	}
	raw, _ := json.Marshal(receipt)
	fmt.Println(string(raw))
}
