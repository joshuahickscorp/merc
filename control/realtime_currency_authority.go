package main

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
)

const (
	realtimeReferenceCurrency        = "usd"
	realtimeFXAuthorityVersion       = 1
	realtimeFXRateScale        int64 = NanosPerMajorUnit
	realtimeFXRoundingPolicy         = "reference-charge-then-fx-nanos-v1"
)

// RealtimeFXAuthority freezes the conversion used between the USD-denominated
// runtime profile / supplier offer book and the deployment settlement currency.
// ReferenceToSettlementNanos is the exact number of nano settlement-major-units
// per one USD. Arithmetic uses that integer, never the display float.
type RealtimeFXAuthority struct {
	Version                    int     `json:"version"`
	ReferenceCurrency          string  `json:"reference_currency"`
	SettlementCurrency         string  `json:"settlement_currency"`
	ReferenceToSettlementRate  float64 `json:"reference_to_settlement_rate"`
	ReferenceToSettlementNanos int64   `json:"reference_to_settlement_nanos"`
	FXRevision                 string  `json:"fx_revision"`
	RoundingPolicy             string  `json:"rounding_policy"`
}

func zeroRealtimeFXAuthority(a RealtimeFXAuthority) bool {
	return reflect.DeepEqual(a, RealtimeFXAuthority{})
}

func validateRealtimeFXAuthority(a RealtimeFXAuthority, settlement Currency) error {
	if a.Version != realtimeFXAuthorityVersion ||
		a.ReferenceCurrency != realtimeReferenceCurrency ||
		a.SettlementCurrency != settlement.Code() ||
		a.RoundingPolicy != realtimeFXRoundingPolicy {
		return errors.New("realtime FX authority has unsupported version, currency pair, or rounding policy")
	}
	if !finiteNonNegative(a.ReferenceToSettlementRate) || a.ReferenceToSettlementRate <= 0 ||
		a.ReferenceToSettlementNanos <= 0 {
		return errors.New("realtime FX authority lacks a finite positive rate")
	}
	canonical, err := nanoRatePerMillionFromFloat(a.ReferenceToSettlementRate)
	if err != nil || int64(canonical) != a.ReferenceToSettlementNanos {
		return errors.New("realtime FX display rate disagrees with exact nano authority")
	}
	if strings.TrimSpace(a.FXRevision) == "" || len(a.FXRevision) > 128 ||
		strings.ContainsAny(a.FXRevision, "\r\n\t") {
		return errors.New("realtime FX authority lacks a valid governed revision")
	}
	if a.ReferenceCurrency == a.SettlementCurrency {
		if a.ReferenceToSettlementNanos != realtimeFXRateScale ||
			a.ReferenceToSettlementRate != 1 ||
			a.FXRevision != "identity-"+a.ReferenceCurrency {
			return errors.New("realtime same-currency FX authority is not exact identity")
		}
	} else if strings.HasPrefix(a.FXRevision, "identity-") {
		return errors.New("realtime cross-currency FX authority falsely claims identity")
	}
	return nil
}

// loadRealtimeFXAuthority resolves the same operator-governed FX inputs used by
// catalogue publication, then closes the float boundary into an exact 1e-9
// factor. A CAD (or other non-USD) settlement deployment therefore refuses new
// realtime work when the governed rate or revision is absent.
func loadRealtimeFXAuthority(settlement Currency) (RealtimeFXAuthority, error) {
	if !settlement.Valid() {
		return RealtimeFXAuthority{}, errors.New("realtime FX authority requires a settlement currency")
	}
	rate, revision, err := catalogueFXAuthority(realtimeReferenceCurrency, settlement.Code())
	if err != nil {
		return RealtimeFXAuthority{}, fmt.Errorf("load realtime FX authority: %w", err)
	}
	exact, err := nanoRatePerMillionFromFloat(rate)
	if err != nil || exact <= 0 {
		return RealtimeFXAuthority{}, errors.New("realtime FX authority cannot be represented in exact nanos")
	}
	a := RealtimeFXAuthority{
		Version: realtimeFXAuthorityVersion, ReferenceCurrency: realtimeReferenceCurrency,
		SettlementCurrency: settlement.Code(), ReferenceToSettlementRate: rate,
		ReferenceToSettlementNanos: int64(exact), FXRevision: revision,
		RoundingPolicy: realtimeFXRoundingPolicy,
	}
	if err := validateRealtimeFXAuthority(a, settlement); err != nil {
		return RealtimeFXAuthority{}, err
	}
	return a, nil
}

// realtimeFXForNewIngress refuses a caller-supplied rate that is no longer the
// current governed rate. The returned value is then frozen in PricingDecision;
// historical replay validates that frozen value and never calls this function.
func realtimeFXForNewIngress(frozen RealtimeFXAuthority, settlement Currency) (RealtimeFXAuthority, error) {
	current, err := loadRealtimeFXAuthority(settlement)
	if err != nil {
		return RealtimeFXAuthority{}, err
	}
	if !zeroRealtimeFXAuthority(frozen) && !reflect.DeepEqual(frozen, current) {
		return RealtimeFXAuthority{}, errors.New("realtime request FX authority is not the current governed authority")
	}
	return current, nil
}

// convertRealtimeReferenceNanos converts an already-complete USD nano amount.
// Buyer/supplier charges round up so conversion cannot silently erase a
// positive liability. Buyer-declared ceilings round down so conversion cannot
// spend more than the USD cap the buyer supplied.
func convertRealtimeReferenceNanos(
	referenceNanos int64,
	settlement Currency,
	fx RealtimeFXAuthority,
	roundUp bool,
) (MoneyNanos, error) {
	if referenceNanos < 0 {
		return MoneyNanos{}, errors.New("realtime FX conversion refuses a negative amount")
	}
	if err := validateRealtimeFXAuthority(fx, settlement); err != nil {
		return MoneyNanos{}, err
	}
	nanos, err := mulDiv(referenceNanos, fx.ReferenceToSettlementNanos, realtimeFXRateScale, roundUp)
	if err != nil {
		return MoneyNanos{}, err
	}
	return NewMoneyNanos(settlement, nanos)
}

func convertRealtimeReferenceRate(
	referenceRate NanoMajorPerMillionTokens,
	settlement Currency,
	fx RealtimeFXAuthority,
) (NanoMajorPerMillionTokens, error) {
	converted, err := convertRealtimeReferenceNanos(int64(referenceRate), settlement, fx, true)
	if err != nil {
		return 0, err
	}
	if converted.Nanos <= 0 {
		return 0, errors.New("realtime FX conversion collapsed a positive token rate")
	}
	return NanoMajorPerMillionTokens(converted.Nanos), nil
}

func realtimeReferenceTokenCharge(
	prompt, completion int64,
	inputRate, outputRate NanoMajorPerMillionTokens,
	supplier bool,
) (MoneyNanos, error) {
	reference := MustParseCurrency(realtimeReferenceCurrency)
	if supplier {
		return SupplierRealtimeTokenEntitlementNanos(reference, prompt, completion, inputRate, outputRate)
	}
	return BuyerRealtimeTokenChargeNanos(reference, prompt, completion, inputRate, outputRate)
}

func realtimeBuyerPriceBounds(
	profile VLLMRuntimeProfile,
	maximumPrompt, maximumCompletion, estimatedPrompt, estimatedCompletion int64,
	settlement Currency,
	fx RealtimeFXAuthority,
) (referenceExpected, referenceMaximum, settlementExpected, settlementMaximum MoneyNanos, err error) {
	input, err := nanoRatePerMillionFromFloat(profile.BuyerInputUSDPerMillionTokens)
	if err != nil {
		return MoneyNanos{}, MoneyNanos{}, MoneyNanos{}, MoneyNanos{}, err
	}
	output, err := nanoRatePerMillionFromFloat(profile.BuyerOutputUSDPerMillionTokens)
	if err != nil {
		return MoneyNanos{}, MoneyNanos{}, MoneyNanos{}, MoneyNanos{}, err
	}
	referenceExpected, err = realtimeReferenceTokenCharge(
		estimatedPrompt, estimatedCompletion, input, output, false)
	if err != nil {
		return MoneyNanos{}, MoneyNanos{}, MoneyNanos{}, MoneyNanos{}, err
	}
	referenceMaximum, err = realtimeReferenceTokenCharge(
		maximumPrompt, maximumCompletion, input, output, false)
	if err != nil {
		return MoneyNanos{}, MoneyNanos{}, MoneyNanos{}, MoneyNanos{}, err
	}
	settlementExpected, err = convertRealtimeReferenceNanos(
		referenceExpected.Nanos, settlement, fx, true)
	if err != nil {
		return MoneyNanos{}, MoneyNanos{}, MoneyNanos{}, MoneyNanos{}, err
	}
	settlementMaximum, err = convertRealtimeReferenceNanos(
		referenceMaximum.Nanos, settlement, fx, true)
	return referenceExpected, referenceMaximum, settlementExpected, settlementMaximum, err
}

func projectRealtimeNanosToMajor(amount MoneyNanos) (float64, error) {
	micros, err := LedgerMicrosFromNanos(amount)
	if err != nil {
		return 0, err
	}
	return microsToUSD(micros), nil
}

// parsePositiveUSDNanos parses the buyer's USD ceiling without a binary-float
// round trip. Scientific notation, signs, and more than nine fractional digits
// are refused so the header/body has one exact interpretation.
func parsePositiveUSDNanos(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, errors.New("USD amount is required")
	}
	wholeRaw, fractionRaw, hasFraction := strings.Cut(raw, ".")
	if wholeRaw == "" || (hasFraction && fractionRaw == "") ||
		strings.Contains(fractionRaw, ".") || len(fractionRaw) > 9 {
		return 0, errors.New("USD amount must be a positive decimal with at most 9 fractional digits")
	}
	for _, part := range []string{wholeRaw, fractionRaw} {
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return 0, errors.New("USD amount must be a positive decimal with at most 9 fractional digits")
			}
		}
	}
	whole, err := strconv.ParseUint(wholeRaw, 10, 64)
	if err != nil || whole > uint64(math.MaxInt64)/uint64(NanosPerMajorUnit) {
		return 0, errors.New("USD amount is outside the supported range")
	}
	for len(fractionRaw) < 9 {
		fractionRaw += "0"
	}
	var fraction uint64
	if fractionRaw != "" {
		fraction, err = strconv.ParseUint(fractionRaw, 10, 64)
		if err != nil {
			return 0, errors.New("USD amount is outside the supported range")
		}
	}
	total := whole*uint64(NanosPerMajorUnit) + fraction
	if total == 0 || total > uint64(math.MaxInt64) {
		return 0, errors.New("USD amount must be positive and within the supported range")
	}
	return int64(total), nil
}

func referenceCeilingNanosFromLegacyFloat(value float64) (int64, error) {
	if value == 0 {
		return 0, nil
	}
	if !finiteNonNegative(value) || value < 0 {
		return 0, errors.New("realtime buyer USD ceiling must be finite and non-negative")
	}
	reference, err := MoneyNanosFromUSDFloat(MustParseCurrency(realtimeReferenceCurrency), value)
	if err != nil || reference.Nanos <= 0 {
		return 0, errors.New("realtime buyer USD ceiling cannot be represented in exact nanos")
	}
	return reference.Nanos, nil
}
