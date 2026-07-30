package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

// Input-depth profile authority. Fixed published cut-points; no ML classifier.
// The profile freezes enough aggregate body statistics to re-validate itself
// without re-reading the buyer input.
const (
	inputDepthProfileVersion = 1

	inputDepthBandShort  = "short"
	inputDepthBandMedium = "medium"
	inputDepthBandLong   = "long"

	// Body-token bands (estimated tokens via estimateTokens / estimateTokensFromCounts).
	inputDepthShortMaxTokens  = 128 // short: <=128
	inputDepthMediumMaxTokens = 512 // medium: 129..512; long: >512
)

// InputDepthProfile is a deterministic, bounded summary of selected JSONL body
// depths. It is frozen inside ComputePlan so quote binding, receipts and
// ETA/history calibration share one authority.
type InputDepthProfile struct {
	Version         int    `json:"version"`
	BodyBytes       int64  `json:"body_bytes"`
	BodyRunes       int64  `json:"body_runes"`
	BodyASCIIBytes  int64  `json:"body_ascii_bytes"`
	EstimatedTokens int64  `json:"estimated_tokens"`
	ShortRecords    int    `json:"short_records"`
	MediumRecords   int    `json:"medium_records"`
	LongRecords     int    `json:"long_records"`
	P90DepthBand    string `json:"p90_depth_band"`
}

// inputDepthAccumulator builds an InputDepthProfile over the full accepted
// record stream without retaining per-record arrays.
//
// Per-record estimateTokens drives short/medium/long classification. Total
// EstimatedTokens is derived from the aggregate body byte/rune/ASCII counts so
// the frozen profile can re-validate without re-reading input (per-record
// ceil() does not recompose under summation).
type inputDepthAccumulator struct {
	bodyBytes      int64
	bodyRunes      int64
	bodyASCIIBytes int64
	short          int
	medium         int
	long           int
}

func newInputDepthAccumulator() *inputDepthAccumulator {
	return &inputDepthAccumulator{}
}

func (a *inputDepthAccumulator) addBody(body string) {
	b := []byte(body)
	ascii := 0
	for _, c := range b {
		if c < 128 {
			ascii++
		}
	}
	runes := utf8.RuneCount(b)
	a.bodyBytes += int64(len(b))
	a.bodyRunes += int64(runes)
	a.bodyASCIIBytes += int64(ascii)
	// Band uses the per-record estimator; the job-level p90 is then derived from
	// these record bands instead of from an average that erases their spread.
	switch depthBandForTokens(estimateTokensFromCounts(runes, ascii, len(b))) {
	case inputDepthBandShort:
		a.short++
	case inputDepthBandMedium:
		a.medium++
	default:
		a.long++
	}
}

func (a *inputDepthAccumulator) profile() (InputDepthProfile, error) {
	var estimated int64
	if a.bodyBytes > 0 {
		if a.bodyRunes > int64(math.MaxInt) || a.bodyASCIIBytes > int64(math.MaxInt) ||
			a.bodyBytes > int64(math.MaxInt) {
			return InputDepthProfile{}, errors.New("input depth body counts exceed estimator range")
		}
		estimated = estimateTokensFromCounts(int(a.bodyRunes), int(a.bodyASCIIBytes), int(a.bodyBytes))
	}
	p := InputDepthProfile{
		Version:         inputDepthProfileVersion,
		BodyBytes:       a.bodyBytes,
		BodyRunes:       a.bodyRunes,
		BodyASCIIBytes:  a.bodyASCIIBytes,
		EstimatedTokens: estimated,
		ShortRecords:    a.short,
		MediumRecords:   a.medium,
		LongRecords:     a.long,
		P90DepthBand:    deriveP90DepthBand(a.short, a.medium, a.long),
	}
	if err := validateInputDepthProfile(p); err != nil {
		return InputDepthProfile{}, err
	}
	return p, nil
}

func depthBandForTokens(tokens int64) string {
	switch {
	case tokens <= inputDepthShortMaxTokens:
		return inputDepthBandShort
	case tokens <= inputDepthMediumMaxTokens:
		return inputDepthBandMedium
	default:
		return inputDepthBandLong
	}
}

// deriveP90DepthBand ranks short < medium < long and returns the band at
// ceil(0.9*N). Empty input has no p90.
func deriveP90DepthBand(short, medium, longCount int) string {
	n, ok := checkedInputDepthRecordCount(short, medium, longCount)
	if !ok || n == 0 {
		return ""
	}
	// ceil(0.9*n) without a floating-point conversion that loses integer
	// precision for large valid profiles.
	rank := n - n/10
	switch {
	case rank <= short:
		return inputDepthBandShort
	case rank <= short+medium:
		return inputDepthBandMedium
	default:
		return inputDepthBandLong
	}
}

func checkedInputDepthRecordCount(short, medium, longCount int) (int, bool) {
	if short < 0 || medium < 0 || longCount < 0 {
		return 0, false
	}
	if medium > math.MaxInt-short {
		return 0, false
	}
	subtotal := short + medium
	if longCount > math.MaxInt-subtotal {
		return 0, false
	}
	return subtotal + longCount, true
}

func validateInputDepthProfile(p InputDepthProfile) error {
	if p.Version != inputDepthProfileVersion {
		return fmt.Errorf("unsupported input depth profile version %d", p.Version)
	}
	if p.BodyBytes < 0 || p.BodyRunes < 0 || p.BodyASCIIBytes < 0 ||
		p.EstimatedTokens < 0 || p.ShortRecords < 0 || p.MediumRecords < 0 || p.LongRecords < 0 {
		return errors.New("input depth profile has negative aggregates")
	}
	if p.BodyBytes == 0 || p.BodyRunes == 0 || p.EstimatedTokens == 0 {
		return errors.New("input depth profile requires positive body bytes, runes, and estimated tokens")
	}
	if p.BodyRunes > p.BodyBytes {
		return errors.New("input depth profile body runes exceed body bytes")
	}
	if p.BodyASCIIBytes > p.BodyBytes {
		return errors.New("input depth profile ascii bytes exceed body bytes")
	}
	n, ok := checkedInputDepthRecordCount(p.ShortRecords, p.MediumRecords, p.LongRecords)
	if !ok {
		return errors.New("input depth profile record counts overflow")
	}
	if n == 0 {
		return errors.New("input depth profile requires at least one classified record")
	}
	if p.BodyBytes < int64(n) {
		return errors.New("input depth profile body bytes cannot cover its record count")
	}
	// Re-derive token estimate from frozen body counts without re-reading input.
	// Counts that exceed int range cannot be re-checked byte-for-byte; reject them
	// rather than silently truncating authority.
	if p.BodyRunes > int64(math.MaxInt) || p.BodyASCIIBytes > int64(math.MaxInt) ||
		p.BodyBytes > int64(math.MaxInt) {
		return errors.New("input depth profile body counts exceed re-validation range")
	}
	wantTokens := estimateTokensFromCounts(int(p.BodyRunes), int(p.BodyASCIIBytes), int(p.BodyBytes))
	if p.EstimatedTokens != wantTokens {
		return fmt.Errorf("input depth profile estimated_tokens=%d does not match body counts (%d)",
			p.EstimatedTokens, wantTokens)
	}
	wantBand := deriveP90DepthBand(p.ShortRecords, p.MediumRecords, p.LongRecords)
	if p.P90DepthBand != wantBand {
		return fmt.Errorf("input depth profile p90_depth_band=%q does not match record counts (want %q)",
			p.P90DepthBand, wantBand)
	}
	switch p.P90DepthBand {
	case inputDepthBandShort, inputDepthBandMedium, inputDepthBandLong:
	default:
		return fmt.Errorf("input depth profile has invalid p90_depth_band %q", p.P90DepthBand)
	}
	return nil
}

func inputDepthProfilesEqual(a, b InputDepthProfile) bool {
	return a == b
}

// jsonRawIsNull reports whether a JSON raw value is JSON null (after trim).
func jsonRawIsNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// decodeStrictRawJSONObject performs the top-level decode shared by admission and
// depth measurement. The Rust runner first requires UTF-8 and serde rejects
// duplicate struct fields; encoding/json's ordinary map unmarshal would replace
// invalid UTF-8 and silently keep the last duplicate instead.
func decodeStrictRawJSONObject(line []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(line) {
		return nil, errors.New("record is not valid UTF-8")
	}
	if !validJSONSurrogateEscapes(line) {
		return nil, errors.New("record contains an unpaired Unicode surrogate escape")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	first, err := dec.Token()
	if err != nil {
		return nil, err
	}
	open, ok := first.(json.Delim)
	if !ok || open != '{' {
		return nil, errors.New("record must be a JSON object")
	}
	object := make(map[string]json.RawMessage)
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("record contains a non-string object key")
		}
		if _, exists := object[key]; exists {
			return nil, fmt.Errorf("record contains duplicate field %q", key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		object[key] = append(json.RawMessage(nil), raw...)
	}
	last, err := dec.Token()
	if err != nil {
		return nil, err
	}
	close, ok := last.(json.Delim)
	if !ok || close != '}' {
		return nil, errors.New("record has an invalid JSON object terminator")
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("record contains trailing JSON")
		}
		return nil, err
	}
	return object, nil
}

func validJSONSurrogateEscapes(raw []byte) bool {
	const (
		highStart = 0xD800
		highEnd   = 0xDBFF
		lowStart  = 0xDC00
		lowEnd    = 0xDFFF
	)
	inString := false
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || i+1 >= len(raw) {
				continue
			}
			i++
			if raw[i] != 'u' || i+4 >= len(raw) {
				continue
			}
			first, ok := decodeJSONHex4(raw[i+1 : i+5])
			if !ok {
				continue // the JSON decoder reports malformed hex syntax
			}
			i += 4
			switch {
			case first >= highStart && first <= highEnd:
				if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
					return false
				}
				second, ok := decodeJSONHex4(raw[i+3 : i+7])
				if !ok || second < lowStart || second > lowEnd {
					return false
				}
				i += 6
			case first >= lowStart && first <= lowEnd:
				return false
			}
		}
	}
	return true
}

func decodeJSONHex4(raw []byte) (uint16, bool) {
	if len(raw) != 4 {
		return 0, false
	}
	var value uint16
	for _, b := range raw {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value |= uint16(b-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

// selectJSONLBody mirrors the Rust runner (agent/src/executor.rs TextItem::body):
// if text is present and non-null it wins (including empty string); otherwise
// prompt. The selected value must be a non-empty (non-whitespace-only) string.
// In particular {text:"", prompt:"valid"} is rejected because the runner would
// execute the empty text.
func selectJSONLBody(object map[string]json.RawMessage) (string, error) {
	if object == nil {
		return "", errors.New("record must be a JSON object")
	}
	if raw, exists := object["text"]; exists && !jsonRawIsNull(raw) {
		var body string
		if err := json.Unmarshal(raw, &body); err != nil {
			return "", errors.New("text must be a string")
		}
		if strings.TrimSpace(body) == "" {
			return "", errors.New("text is present but empty; runner would select empty text over prompt")
		}
		return body, nil
	}
	if raw, exists := object["prompt"]; exists && !jsonRawIsNull(raw) {
		var body string
		if err := json.Unmarshal(raw, &body); err != nil {
			return "", errors.New("prompt must be a string")
		}
		if strings.TrimSpace(body) == "" {
			return "", errors.New("prompt is empty")
		}
		return body, nil
	}
	return "", errors.New("records require a non-empty text or prompt string")
}

// selectJSONLBodyFromLine unmarshals one JSONL record and returns the runner-selected body.
func selectJSONLBodyFromLine(line []byte) (string, error) {
	object, err := decodeStrictRawJSONObject(line)
	if err != nil {
		return "", err
	}
	return selectJSONLBody(object)
}

// buildInputDepthProfileFromJSONL measures the depth profile over every non-blank
// line using the same body-selection rules as validation and the runner.
func buildInputDepthProfileFromJSONL(data []byte) (InputDepthProfile, error) {
	acc := newInputDepthAccumulator()
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimSpace(bytes.TrimSuffix(raw, []byte("\r")))
		if len(line) == 0 {
			continue
		}
		body, err := selectJSONLBodyFromLine(line)
		if err != nil {
			return InputDepthProfile{}, err
		}
		acc.addBody(body)
	}
	return acc.profile()
}
