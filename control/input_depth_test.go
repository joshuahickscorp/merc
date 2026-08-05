package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

func bodyJSONL(bodies ...string) []byte {
	var b bytes.Buffer
	for i, body := range bodies {
		// Prefer text field; tests that need prompt-only pass already-formed lines.
		rec := fmt.Sprintf(`{"id":"%d","text":%s}`, i, mustJSONString(body))
		b.WriteString(rec)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func mustJSONString(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// testInputDepthProfile builds a self-consistent short-band profile for
// fixtures that do not carry real JSONL. Each synthetic record is four ASCII
// bytes / one estimated token under the Unicode-aware estimator.
func testInputDepthProfile(records int) InputDepthProfile {
	if records <= 0 {
		records = 1
	}
	const perBody = "abcd"
	acc := newInputDepthAccumulator()
	for i := 0; i < records; i++ {
		acc.addBody(perBody)
	}
	p, err := acc.profile()
	if err != nil {
		panic(fmt.Sprintf("testInputDepthProfile(%d): %v", records, err))
	}
	return p
}

func TestDepthBandBoundaries(t *testing.T) {
	// short: <=128 estimated body tokens
	// medium: 129..512
	// long: >512
	// ASCII estimator: tokens = ceil(runes/4)
	shortBody := strings.Repeat("a", 128*4)  // 128 tokens
	mediumLo := strings.Repeat("a", 129*4)   // 129 tokens
	mediumHi := strings.Repeat("a", 512*4)   // 512 tokens
	longBody := strings.Repeat("a", 512*4+1) // 513 tokens
	if got := depthBandForTokens(estimateTokens([]byte(shortBody))); got != inputDepthBandShort {
		t.Fatalf("128-token body band=%q, want short", got)
	}
	if got := depthBandForTokens(estimateTokens([]byte(mediumLo))); got != inputDepthBandMedium {
		t.Fatalf("129-token body band=%q, want medium", got)
	}
	if got := depthBandForTokens(estimateTokens([]byte(mediumHi))); got != inputDepthBandMedium {
		t.Fatalf("512-token body band=%q, want medium", got)
	}
	if got := depthBandForTokens(estimateTokens([]byte(longBody))); got != inputDepthBandLong {
		t.Fatalf("513-token body band=%q, want long", got)
	}
}

func TestDeriveP90DepthBandDeterministic(t *testing.T) {
	// 10 short → rank ceil(9)=9 → short
	if got := deriveP90DepthBand(10, 0, 0); got != inputDepthBandShort {
		t.Fatalf("10 short p90=%q, want short", got)
	}
	// 9 short + 1 long → rank 9 → short
	if got := deriveP90DepthBand(9, 0, 1); got != inputDepthBandShort {
		t.Fatalf("9 short + 1 long p90=%q, want short", got)
	}
	// 8 short + 2 long → rank 9 → long
	if got := deriveP90DepthBand(8, 0, 2); got != inputDepthBandLong {
		t.Fatalf("8 short + 2 long p90=%q, want long", got)
	}
	// 5 short + 5 medium → rank 9 → medium
	if got := deriveP90DepthBand(5, 5, 0); got != inputDepthBandMedium {
		t.Fatalf("5 short + 5 medium p90=%q, want medium", got)
	}
	// Single long record → p90 long
	if got := deriveP90DepthBand(0, 0, 1); got != inputDepthBandLong {
		t.Fatalf("1 long p90=%q, want long", got)
	}
	if got := deriveP90DepthBand(0, 0, 0); got != "" {
		t.Fatalf("empty p90=%q, want empty", got)
	}
	if _, ok := checkedInputDepthRecordCount(math.MaxInt, 1, 0); ok {
		t.Fatal("overflowed record count was accepted")
	}
	if got := deriveP90DepthBand(math.MaxInt, 1, 0); got != "" {
		t.Fatalf("overflowed p90=%q, want empty", got)
	}
}

func TestUnicodeEstimatorConsistencyInDepthProfile(t *testing.T) {
	// Heavy non-ASCII: estimator uses 0.9 * runes when ascii/bytes < 0.5.
	body := strings.Repeat("世界", 100) // 200 runes, 600 bytes, 0 ASCII
	tokens := estimateTokens([]byte(body))
	want := int64(math.Ceil(float64(utf8.RuneCountInString(body)) * nonASCIITokensPerRune))
	if tokens != want {
		t.Fatalf("estimateTokens=%d, want %d", tokens, want)
	}
	p, err := buildInputDepthProfileFromJSONL(bodyJSONL(body))
	must(t, err)
	if p.EstimatedTokens != tokens {
		t.Fatalf("profile estimated_tokens=%d, want %d from estimator", p.EstimatedTokens, tokens)
	}
	if p.BodyRunes != int64(utf8.RuneCountInString(body)) || p.BodyASCIIBytes != 0 {
		t.Fatalf("profile body stats runes=%d ascii=%d unexpected", p.BodyRunes, p.BodyASCIIBytes)
	}
	mustf(t, validateInputDepthProfile(p), "self-validation failed: %v")
}

func TestTextPrecedenceMatchesRunner(t *testing.T) {
	// text wins over prompt when both present.
	line := []byte(`{"text":"from-text","prompt":"from-prompt"}`)
	body, err := selectJSONLBodyFromLine(line)
	must(t, err)
	if body != "from-text" {
		t.Fatalf("body=%q, want from-text", body)
	}
	// prompt used when text absent.
	line = []byte(`{"prompt":"from-prompt"}`)
	body, err = selectJSONLBodyFromLine(line)
	must(t, err)
	if body != "from-prompt" {
		t.Fatalf("body=%q, want from-prompt", body)
	}
	// text:null falls through to prompt (serde Option::None).
	line = []byte(`{"text":null,"prompt":"from-prompt"}`)
	body, err = selectJSONLBodyFromLine(line)
	must(t, err)
	if body != "from-prompt" {
		t.Fatalf("null text body=%q, want from-prompt", body)
	}
	// Empty winning text is rejected even when prompt is valid.
	line = []byte(`{"text":"","prompt":"valid"}`)
	if _, err := selectJSONLBodyFromLine(line); err == nil {
		t.Fatal("empty text with valid prompt was accepted")
	}
	if err := validateWorkloadJSONL("embed", append(line, '\n')); err == nil {
		t.Fatal("validation accepted empty winning text")
	}
	// Whitespace-only winning text rejected.
	line = []byte(`{"text":"   ","prompt":"valid"}`)
	if _, err := selectJSONLBodyFromLine(line); err == nil {
		t.Fatal("whitespace text with valid prompt was accepted")
	}
	// Serde rejects duplicate struct fields rather than silently taking the
	// last one, so admission and measurement must do the same.
	line = []byte(`{"text":"first","text":"second"}`)
	if _, err := selectJSONLBodyFromLine(line); err == nil {
		t.Fatal("duplicate text field was accepted")
	}
	// The runner calls str::from_utf8 before parsing JSON.
	line = append([]byte(`{"text":"`), 0xff)
	line = append(line, []byte(`"}`)...)
	if _, err := selectJSONLBodyFromLine(line); err == nil {
		t.Fatal("invalid UTF-8 body was accepted")
	}
	for _, line := range [][]byte{
		[]byte(`{"text":"\ud800"}`),
		[]byte(`{"text":"\udc00"}`),
		[]byte(`{"text":"\ud800\u0041"}`),
	} {
		if _, err := selectJSONLBodyFromLine(line); err == nil {
			t.Fatalf("unpaired Unicode surrogate was accepted: %s", line)
		}
	}
	body, err = selectJSONLBodyFromLine([]byte(`{"text":"\ud83d\ude00"}`))
	if err != nil || body != "😀" {
		t.Fatalf("valid Unicode surrogate pair body=%q err=%v", body, err)
	}
}

func TestQuoteScanAndStreamProfileIdentical(t *testing.T) {
	// Mix of short and medium bodies that exercise full-stream accumulation.
	short := strings.Repeat("s", 40)
	medium := strings.Repeat("m", 129*4)
	input := bodyJSONL(short, short, medium, short, medium)

	scan := scanJSONL(input)
	mustf(t, validateInputDepthProfile(scan.InputDepth), "scan profile invalid: %v")
	built, err := buildInputDepthProfileFromJSONL(input)
	must(t, err)
	if !inputDepthProfilesEqual(scan.InputDepth, built) {
		t.Fatalf("scan profile %+v != build profile %+v", scan.InputDepth, built)
	}

	// Stream path (no storage) must match: reuse the same body selection + accumulator.
	acc := newInputDepthAccumulator()
	for _, raw := range bytes.Split(input, []byte("\n")) {
		line := bytes.TrimSpace(bytes.TrimSuffix(raw, []byte("\r")))
		if len(line) == 0 {
			continue
		}
		body, err := parseWorkloadJSONLRecord("embed", line, 1)
		must(t, err)
		acc.addBody(body)
	}
	streamed, err := acc.profile()
	must(t, err)
	if !inputDepthProfilesEqual(scan.InputDepth, streamed) {
		t.Fatalf("quote scan profile %+v != stream profile %+v", scan.InputDepth, streamed)
	}
}

func TestProfileRejectsTamperedBandAndCounts(t *testing.T) {
	p, err := buildInputDepthProfileFromJSONL(bodyJSONL("hello", "world"))
	must(t, err)
	mutant := p
	mutant.P90DepthBand = inputDepthBandLong
	if err := validateInputDepthProfile(mutant); err == nil {
		t.Fatal("tampered p90 band accepted")
	}
	mutant = p
	mutant.EstimatedTokens++
	if err := validateInputDepthProfile(mutant); err == nil {
		t.Fatal("tampered estimated tokens accepted")
	}
	// Body rune count that changes the re-derived token estimate.
	mutant = p
	mutant.BodyRunes = p.BodyRunes + 100
	if err := validateInputDepthProfile(mutant); err == nil {
		t.Fatal("tampered body runes without token recompute accepted")
	}
	mutant = p
	mutant.BodyASCIIBytes = p.BodyBytes + 1 // ascii cannot exceed bytes
	if err := validateInputDepthProfile(mutant); err == nil {
		t.Fatal("ascii > body bytes accepted")
	}
	mutant = p
	mutant.ShortRecords = 0
	mutant.MediumRecords = 0
	mutant.LongRecords = 0
	if err := validateInputDepthProfile(mutant); err == nil {
		t.Fatal("zero classified records accepted")
	}
	mutant = p
	mutant.BodyBytes = 0
	mutant.BodyRunes = 0
	mutant.BodyASCIIBytes = 0
	mutant.EstimatedTokens = 0
	if err := validateInputDepthProfile(mutant); err == nil {
		t.Fatal("positive record counts with zero body authority accepted")
	}
	mutant = p
	mutant.BodyRunes = p.BodyBytes + 1
	mutant.EstimatedTokens = estimateTokensFromCounts(
		int(mutant.BodyRunes), int(mutant.BodyASCIIBytes), int(mutant.BodyBytes),
	)
	if err := validateInputDepthProfile(mutant); err == nil {
		t.Fatal("body runes greater than body bytes accepted")
	}
	impossible := InputDepthProfile{
		Version:         inputDepthProfileVersion,
		BodyBytes:       1,
		BodyRunes:       1,
		BodyASCIIBytes:  1,
		EstimatedTokens: 1,
		ShortRecords:    2,
		P90DepthBand:    inputDepthBandShort,
	}
	if err := validateInputDepthProfile(impossible); err == nil {
		t.Fatal("body byte count smaller than classified record count accepted")
	}
}
