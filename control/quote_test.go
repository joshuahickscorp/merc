package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

func TestServiceTierSemanticsDoNotPromiseCapacity(t *testing.T) {
	for _, tier := range []string{"batch", "priority", "trusted"} {
		got := serviceTierSemantics(tier)
		if got == "" {
			t.Fatalf("%s semantics are empty", tier)
		}
		for _, forbidden := range []string{"guaranteed fan-out", "reserved devices"} {
			if strings.Contains(strings.ToLower(got), forbidden) {
				t.Fatalf("%s semantics imply unimplemented capacity: %q", tier, got)
			}
		}
	}
	priority := serviceTierSemantics("priority")
	if !strings.Contains(priority, "three") || !strings.Contains(priority, "no device reservation") {
		t.Fatalf("priority semantics do not disclose bounded preference: %q", priority)
	}
}

func TestScanJSONLCountsAndFields(t *testing.T) {
	input := []byte(`{"id":"a","text":"hello world"}

{"id":"b","text":"compute"}
{"id":"c","prompt":"go"}
`)
	s := scanJSONL(input)
	if s.Records != 3 {
		t.Fatalf("records=%d, want 3 (blank line dropped)", s.Records)
	}
	if s.MalformedRecords != 0 || s.FirstBadLine != 0 {
		t.Fatalf("clean input flagged malformed: %+v", s)
	}
	if s.EstimatedTokens <= 0 || int(s.EstimatedTokens) < s.Bytes/4 {
		t.Fatalf("estimated_tokens=%d for %d bytes looks wrong", s.EstimatedTokens, s.Bytes)
	}
	want := []string{"id", "prompt", "text"}
	if len(s.DetectedFields) != 3 || s.DetectedFields[0] != "id" ||
		s.DetectedFields[1] != "prompt" || s.DetectedFields[2] != "text" {
		t.Fatalf("detected_fields=%v, want %v", s.DetectedFields, want)
	}
}

func TestScanJSONLRecommendsLongestStringField(t *testing.T) {
	input := []byte(`{"id":"1","category":"news","body":"A long-form article about distributed compute markets and idle GPUs."}
{"id":"2","category":"blog","body":"Another substantial paragraph of real prose that dwarfs the id and category fields in length."}
{"id":"3","category":"news","body":"Yet more multi-sentence content here, clearly the primary text column of this dataset."}
`)
	s := scanJSONL(input)

	if s.RecommendedField != "body" {
		t.Fatalf("recommended_field=%q, want \"body\" (the longest-average-string column)", s.RecommendedField)
	}
	if len(s.FieldStats) != 3 {
		t.Fatalf("field_stats len=%d, want 3 (id, category, body)", len(s.FieldStats))
	}
	if s.FieldStats[0].Field != "body" {
		t.Fatalf("field_stats[0]=%q, want \"body\" (highest avg length first): %+v", s.FieldStats[0].Field, s.FieldStats)
	}
	if s.FieldStats[0].Field != s.RecommendedField {
		t.Fatalf("recommendation (%q) must be FieldStats[0] (%q)", s.RecommendedField, s.FieldStats[0].Field)
	}
	byField := map[string]FieldStat{}
	for _, fs := range s.FieldStats {
		byField[fs.Field] = fs
	}
	if !(byField["body"].AvgStringLen > byField["category"].AvgStringLen &&
		byField["category"].AvgStringLen > byField["id"].AvgStringLen) {
		t.Fatalf("avg string lengths must order body > category > id, got %+v", s.FieldStats)
	}
	for _, fs := range s.FieldStats {
		if fs.Occurrences != 3 {
			t.Fatalf("field %q occurrences=%d, want 3", fs.Field, fs.Occurrences)
		}
	}
}

func TestScanJSONLNoStringFieldNoRecommendation(t *testing.T) {
	input := []byte(`{"a":1,"b":2.5,"c":true}
{"a":9,"b":0.1,"c":false}
`)
	s := scanJSONL(input)
	if s.RecommendedField != "" {
		t.Fatalf("no string field must yield NO recommendation, got %q", s.RecommendedField)
	}
	if len(s.FieldStats) != 3 {
		t.Fatalf("field_stats must still list all candidates (avg 0), got %+v", s.FieldStats)
	}
	for _, fs := range s.FieldStats {
		if fs.AvgStringLen != 0 {
			t.Fatalf("a non-string field must have avg_string_len 0, got %+v", fs)
		}
	}
}

func TestEstimateTokensFixesMultiByteUndercounting(t *testing.T) {
	cjk := []byte("こんにちはこんにちはこんにちはこんにちは")
	runeCount := 20
	if got := utf8.RuneCount(cjk); got != runeCount {
		t.Fatalf("test fixture: want %d runes, got %d", runeCount, got)
	}
	old := int64((len(cjk) + 3) / 4) // the literal old bytes/4 (ceil) computation
	got := estimateTokens(cjk)
	if got <= old {
		t.Fatalf("fixed estimate (%d) must exceed the old bytes/4 estimate (%d) for CJK text  -  the whole point of the fix", got, old)
	}
	if got < int64(runeCount)/2 {
		t.Fatalf("fixed estimate (%d) is still implausibly low for %d real characters", got, runeCount)
	}
}

func TestEstimateTokensUnchangedForASCII(t *testing.T) {
	ascii := []byte("the quick brown fox jumps over the lazy dog") // 44 bytes, all ASCII
	want := int64((44 + 3) / 4)                                    // ceil(44/4) = 11
	if got := estimateTokens(ascii); got != want {
		t.Fatalf("ASCII estimate changed: want %d (unchanged bytes/4 behavior), got %d", want, got)
	}
}

func TestScanJSONLFindsFirstMalformedLine(t *testing.T) {
	input := []byte("{\"ok\":1}\n\n{not json}\n{\"ok\":2}\n")
	s := scanJSONL(input)
	if s.Records != 3 {
		t.Fatalf("records=%d, want 3", s.Records)
	}
	if s.MalformedRecords != 1 {
		t.Fatalf("malformed=%d, want 1", s.MalformedRecords)
	}
	if s.FirstBadLine != 3 {
		t.Fatalf("first_bad_line=%d, want 3 (line numbers count blanks)", s.FirstBadLine)
	}
}

func TestScanJSONLEmpty(t *testing.T) {
	s := scanJSONL([]byte("\n  \n\n"))
	if s.Records != 0 || s.EstimatedTokens != 0 || len(s.DetectedFields) != 0 {
		t.Fatalf("empty input should be all-zero, got %+v", s)
	}
}

func TestAssessRiskSupplyDrivesOOMAndConfidence(t *testing.T) {
	clean := QuoteInputScan{Records: 100, Bytes: 4000, EstimatedTokens: 1000}

	oom, cold, conf, warn := assessRisk(clean, 8, 0, 4.0)
	if oom != "low" {
		t.Fatalf("oom=%q with 8 eligible workers, want low", oom)
	}
	if cold != "medium" {
		t.Fatalf("cold-start should be medium with no warm supply, got %q", cold)
	}
	if conf.Score < 0.6 {
		t.Fatalf("confidence=%v too low for ample supply", conf.Score)
	}
	if len(warn) != 0 {
		t.Fatalf("clean input + supply should warn nothing, got %v", warn)
	}

	oomWarm, coldWarm, confWarm, _ := assessRisk(clean, 8, 3, 4.0)
	if oomWarm != "low" {
		t.Fatalf("warm supply should not worsen OOM, got %q", oomWarm)
	}
	if coldWarm != "low" {
		t.Fatalf("warm eligible workers should make cold-start low, got %q", coldWarm)
	}
	if confWarm.Score <= conf.Score {
		t.Fatalf("warm supply should raise confidence (%v !> %v)", confWarm.Score, conf.Score)
	}

	oom2, _, conf2, warn2 := assessRisk(clean, 0, 0, 4.0)
	if oom2 != "high" {
		t.Fatalf("oom=%q with 0 eligible workers, want high", oom2)
	}
	if conf2.Score >= conf.Score {
		t.Fatalf("no supply should lower confidence (%v !< %v)", conf2.Score, conf.Score)
	}
	if len(warn2) == 0 {
		t.Fatal("no eligible supply must surface a warning")
	}
}

func TestAssessRiskMalformedLowersConfidenceAndWarns(t *testing.T) {
	bad := QuoteInputScan{Records: 100, Bytes: 4000, MalformedRecords: 5, FirstBadLine: 12}
	_, _, conf, warn := assessRisk(bad, 8, 0, 4.0)
	foundMalformedWarn := false
	for _, w := range warn {
		if len(w) > 0 && (containsSub(w, "malformed")) {
			foundMalformedWarn = true
		}
	}
	if !foundMalformedWarn {
		t.Fatalf("malformed records must produce a warning, got %v", warn)
	}
	if conf.Score >= 0.8 {
		t.Fatalf("malformed input should lower confidence, got %v", conf.Score)
	}
}

func TestApplyMemoryFloorRisk(t *testing.T) {
	base := QuoteConfidence{Score: 0.80, Reasons: []string{"baseline"}}

	oom, conf := applyMemoryFloorRisk("low", base, 24, 16)
	if oom != "medium" {
		t.Fatalf("floor>median should escalate low->medium, got %q", oom)
	}
	if conf.Score >= base.Score {
		t.Fatalf("floor>median must lower confidence (%v !< %v)", conf.Score, base.Score)
	}
	if !reasonsMention(conf.Reasons, "exceeds the median") {
		t.Fatalf("expected an explainable median reason, got %v", conf.Reasons)
	}

	if oom2, _ := applyMemoryFloorRisk("high", base, 24, 16); oom2 != "high" {
		t.Fatalf("high should stay high, got %q", oom2)
	}

	oomTight, confTight := applyMemoryFloorRisk("low", base, 15, 16)
	if oomTight != "low" {
		t.Fatalf("a tight-but-clearing floor should NOT escalate, got %q", oomTight)
	}
	if confTight.Score >= base.Score {
		t.Fatalf("a tight floor should still shave confidence (%v !< %v)", confTight.Score, base.Score)
	}

	oomOK, confOK := applyMemoryFloorRisk("low", base, 8, 64)
	if oomOK != "low" {
		t.Fatalf("ample median should leave OOM low, got %q", oomOK)
	}
	if confOK.Score != base.Score {
		t.Fatalf("ample median should not change confidence (%v != %v)", confOK.Score, base.Score)
	}
	if !reasonsMention(confOK.Reasons, "comfortably clears") {
		t.Fatalf("expected a reassuring reason, got %v", confOK.Reasons)
	}
}

func reasonsMention(reasons []string, sub string) bool {
	for _, r := range reasons {
		if containsSub(r, sub) {
			return true
		}
	}
	return false
}

func TestQuoteIDToUUIDRoundTrip(t *testing.T) {
	id := uuid.New()

	got, err := quoteIDToUUID("q_" + id.String())
	if err != nil {
		t.Fatalf("q_ handle should parse, got %v", err)
	}
	if got != id {
		t.Fatalf("q_ handle resolved to %v, want %v", got, id)
	}

	got2, err := quoteIDToUUID(id.String())
	if err != nil {
		t.Fatalf("bare uuid should parse, got %v", err)
	}
	if got2 != id {
		t.Fatalf("bare uuid resolved to %v, want %v", got2, id)
	}

	if got3, err := quoteIDToUUID("  q_" + id.String() + "  "); err != nil || got3 != id {
		t.Fatalf("padded handle: got (%v,%v), want (%v,nil)", got3, err, id)
	}

	for _, bad := range []string{"", "q_", "q_not-a-uuid", "deadbeef", "q_q_" + id.String()} {
		if _, err := quoteIDToUUID(bad); err == nil {
			t.Fatalf("quoteIDToUUID(%q) should error", bad)
		}
	}
}

func TestPerTaskSecsFromP90FallbackAndConversion(t *testing.T) {
	if got := perTaskSecsFromP90(0); got != targetTaskSecs {
		t.Fatalf("p90ms=0 must fall back to targetTaskSecs(%d), got %d", targetTaskSecs, got)
	}
	if got := perTaskSecsFromP90(-5); got != targetTaskSecs {
		t.Fatalf("negative p90ms must fall back to targetTaskSecs(%d), got %d", targetTaskSecs, got)
	}

	if got := perTaskSecsFromP90(1); got != 1 {
		t.Fatalf("p90ms=1 must round up to 1s, got %d", got)
	}
	if got := perTaskSecsFromP90(1000); got != 1 {
		t.Fatalf("p90ms=1000 must be 1s, got %d", got)
	}
	if got := perTaskSecsFromP90(1001); got != 2 {
		t.Fatalf("p90ms=1001 must round up to 2s, got %d", got)
	}
	if got := perTaskSecsFromP90(90000); got != 90 || got <= targetTaskSecs {
		t.Fatalf("observed p90=90000ms must override the %ds target, got %d", targetTaskSecs, got)
	}
}

func TestSustainedBatchETASecs(t *testing.T) {
	long := 600 // 10 min peak estimate  -  well past the throttle onset
	got := sustainedBatchETASecs(long, "batch", false /*usedObservedHistory*/)
	want := 947 // ceil(600 * 1/(1-0.366)) = ceil(946.37)
	if got != want {
		t.Fatalf("a long peak-derived batch ETA must derate to sustained: want %d, got %d", want, got)
	}
	if got <= long {
		t.Fatalf("the sustained ETA must be strictly longer than the peak ETA (%d), got %d", long, got)
	}

	shortPeak := sustainedETAThresholdSecs - 1
	if got := sustainedBatchETASecs(shortPeak, "batch", false); got != shortPeak {
		t.Fatalf("a short batch ETA (%ds < %ds threshold) must stay at peak, got %d", shortPeak, sustainedETAThresholdSecs, got)
	}

	if got := sustainedBatchETASecs(sustainedETAThresholdSecs, "batch", false); got <= sustainedETAThresholdSecs {
		t.Fatalf("at the threshold the sustained derating must engage, got %d (<= %d)", got, sustainedETAThresholdSecs)
	}

	if got := sustainedBatchETASecs(long, "priority", false); got != long {
		t.Fatalf("a non-batch tier must not be derated, got %d (want %d)", got, long)
	}
	if got := sustainedBatchETASecs(long, "trusted", false); got != long {
		t.Fatalf("the trusted tier must not be derated, got %d (want %d)", got, long)
	}

	if got := sustainedBatchETASecs(long, "batch", true /*usedObservedHistory*/); got != long {
		t.Fatalf("an observed-history ETA must not be re-derated, got %d (want %d)", got, long)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestSLAGuaranteedSecsFormula(t *testing.T) {
	if got := slaGuaranteedSecs(100); got != 185 {
		t.Fatalf("slaGuaranteedSecs(100)=%d, want 185 (ceil(100*1.25)+60)", got)
	}
	if got := slaGuaranteedSecs(1); got != 62 {
		t.Fatalf("slaGuaranteedSecs(1)=%d, want 62 (ceil(1*1.25)+60)", got)
	}
	if got := slaGuaranteedSecs(0); got != 0 {
		t.Fatalf("slaGuaranteedSecs(0)=%d, want 0", got)
	}
	if got := slaGuaranteedSecs(-5); got != 0 {
		t.Fatalf("slaGuaranteedSecs(-5)=%d, want 0", got)
	}
	if slaGuaranteedSecs(200) <= slaGuaranteedSecs(100) {
		t.Fatalf("guarantee must grow with the band: g(200)=%d g(100)=%d",
			slaGuaranteedSecs(200), slaGuaranteedSecs(100))
	}
	for _, cons := range []int{1, 10, 47, 100, 3600} {
		if g := slaGuaranteedSecs(cons); g <= cons {
			t.Fatalf("guarantee %d must exceed its own conservative band %d", g, cons)
		}
	}
}

func TestSLAGuaranteeIsBuiltOnConservativeNotP50(t *testing.T) {
	p50, conservative := 60, 80 // planner invariant: conservative >= p50
	if g, gp := slaGuaranteedSecs(conservative), slaGuaranteedSecs(p50); g <= gp {
		t.Fatalf("guarantee on the conservative band (%d) must exceed a p50-based one (%d)", g, gp)
	}
}

func TestDeriveQuoteSLAHonestDegradation(t *testing.T) {
	const expected = 10.0
	if sla := deriveQuoteSLA(false, true, 100, expected); sla != nil {
		t.Fatalf("sla offered without eligible supply: %+v", sla)
	}
	if sla := deriveQuoteSLA(true, false, 100, expected); sla != nil {
		t.Fatalf("sla offered without planner-backed ETA: %+v", sla)
	}
	if sla := deriveQuoteSLA(true, true, 0, expected); sla != nil {
		t.Fatalf("sla offered with no conservative band: %+v", sla)
	}
	if sla := deriveQuoteSLA(true, true, 100, 0); sla != nil {
		t.Fatalf("sla offered with an un-priceable premium: %+v", sla)
	}
	sla := deriveQuoteSLA(true, true, 100, expected)
	if sla == nil {
		t.Fatal("sla expected when every precondition holds")
	}
	if sla.GuaranteedSecs != slaGuaranteedSecs(100) {
		t.Fatalf("guaranteed_secs=%d, want %d", sla.GuaranteedSecs, slaGuaranteedSecs(100))
	}
	if want := roundUSD(expected * slaPremiumRate); sla.PremiumUSD != want {
		t.Fatalf("premium=%v, want %v (%.0f%% of expected)", sla.PremiumUSD, want, slaPremiumRate*100)
	}
	if sla.ConservativeModelSecs != 100 || sla.SafetyMarginFactor != slaSafetyMarginFactor ||
		sla.MergeAllowanceSecs != slaMergeAllowanceSecs || sla.Remedy == "" {
		t.Fatalf("offer must surface every formula term + the remedy text: %+v", sla)
	}
}

func TestSLAPremiumMath(t *testing.T) {
	cases := []struct{ expected, want float64 }{
		{10.00, 1.50},
		{0.10, 0.015},
		{0.003072, roundUSD(0.003072 * 0.15)}, // a tiny real batch_infer quote still prices a non-zero premium at 6dp
	}
	for _, c := range cases {
		sla := deriveQuoteSLA(true, true, 50, c.expected)
		if sla == nil {
			t.Fatalf("expected an offer for expected=%v", c.expected)
		}
		if sla.PremiumUSD != c.want {
			t.Fatalf("premium for expected=%v: got %v want %v", c.expected, sla.PremiumUSD, c.want)
		}
	}
}

func TestSLARefundAmount(t *testing.T) {
	if got := slaRefundAmount(1.50, 10.0); got != 1.50 {
		t.Fatalf("refund=%v, want the full premium 1.50", got)
	}
	if got := slaRefundAmount(1.50, 0.40); got != 0.40 {
		t.Fatalf("refund=%v, want capped at the chargeable 0.40", got)
	}
	if got := slaRefundAmount(0, 10.0); got != 0 {
		t.Fatalf("no premium -> no refund, got %v", got)
	}
	if got := slaRefundAmount(1.50, 0); got != 0 {
		t.Fatalf("nothing chargeable -> no refund, got %v", got)
	}
}

func TestSLASpanMissed(t *testing.T) {
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	if slaSpanMissed(base, base.Add(90*time.Second), 90) {
		t.Fatal("landing exactly on the guarantee must be MET (within = inclusive)")
	}
	if !slaSpanMissed(base, base.Add(90*time.Second+time.Millisecond), 90) {
		t.Fatal("one millisecond past the guarantee must be a MISS")
	}
	if slaSpanMissed(base, base.Add(10*time.Second), 90) {
		t.Fatal("well inside the guarantee must be MET")
	}
}

func TestSLABindingValidation(t *testing.T) {
	bind := func(q boundQuote) (guarantee int, premium, cap float64) {
		cap = q.CostMaxUSD
		if q.SLAGuaranteedSecs > 0 && q.SLAPremiumUSD > 0 {
			guarantee, premium = q.SLAGuaranteedSecs, q.SLAPremiumUSD
			cap = q.CostMaxUSD + q.SLAPremiumUSD
		}
		return
	}
	g, p, cap := bind(boundQuote{CostMaxUSD: 20, SLAGuaranteedSecs: 185, SLAPremiumUSD: 1.5})
	if g != 185 || p != 1.5 || cap != 21.5 {
		t.Fatalf("sla binding: got g=%d p=%v cap=%v, want 185/1.5/21.5", g, p, cap)
	}
	g, p, cap = bind(boundQuote{CostMaxUSD: 20})
	if g != 0 || p != 0 || cap != 20 {
		t.Fatalf("price-only binding: got g=%d p=%v cap=%v, want 0/0/20", g, p, cap)
	}
	g, p, cap = bind(boundQuote{CostMaxUSD: 20, SLAGuaranteedSecs: 185})
	if g != 0 || p != 0 || cap != 20 {
		t.Fatalf("guarantee without premium must not bind: got g=%d p=%v cap=%v", g, p, cap)
	}
}
