package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Baseline against unmodified code (program/network-10of10 substrate before this
// lane): every test below fails because validJobTypes rejects video_generation,
// no cell exists, no policy/allowlist/settlement symbols exist, and no
// synthesizer can reassemble segments. The comments name the requirement.

func TestVideoGenerationIsARealJobType(t *testing.T) {
	if !validJobTypes[videoGenerationJobType] {
		t.Fatal("video_generation missing from validJobTypes")
	}
	sub := jobSubmit{
		JobType: JobType{
			Type: videoGenerationJobType, RenderWidth: 512, RenderHeight: 512,
			FPS: 8, DurationSecs: 2,
		},
		Model:        ModelRef{Kind: "builtin", Ref: videoGenerationModelRef},
		Params:       json.RawMessage(`{"segment_count":2}`),
		Verification: VerificationPolicy{RedundancyFrac: 0},
	}
	// Shape path accepts the job type even though the cell is not buyer-routable.
	got, herr := normalizeAndValidateJobSubmit(sub)
	if herr != nil {
		t.Fatalf("shape validation refused a well-formed video job: %v", herr)
	}
	if got.JobType.Type != videoGenerationJobType {
		t.Fatalf("type = %q", got.JobType.Type)
	}
}

func TestVideoProfileAllowlistNotRange(t *testing.T) {
	// Arbitrary dimensions let a buyer force memory the price never covered.
	err := normalizeVideoGenerationJobType(&JobType{
		Type: videoGenerationJobType, RenderWidth: 4096, RenderHeight: 4096,
		FPS: 60, DurationSecs: 30,
	})
	if err == nil {
		t.Fatal("unoffered profile accepted")
	}
	if !errors.Is(err, errVideoRequestShape) {
		t.Fatalf("want shape error, got %v", err)
	}
	// Every advertised profile must accept.
	for _, key := range sortedVideoProfiles() {
		p := allowedVideoProfiles[key]
		jt := JobType{
			Type: videoGenerationJobType, RenderWidth: p.Width, RenderHeight: p.Height,
			FPS: p.FPS, DurationSecs: p.DurationSecs,
		}
		if err := normalizeVideoGenerationJobType(&jt); err != nil {
			t.Fatalf("advertised profile %q refused: %v", key, err)
		}
	}
}

func TestVideoJobDecomposesIntoNSegmentsAndReassembles(t *testing.T) {
	// Req 1: N segments, each claimed independently, reassemble under one engine.
	const (
		width, height uint32 = 256, 256
		fps, dur      uint32 = 8, 2
		segments             = 4
	)
	plan, err := deriveVideoSegmentPlan(segments, float64(dur))
	if err != nil {
		t.Fatal(err)
	}
	units := make([]MediaSegmentUnit, 0, plan.UnitCount)
	for o := int64(0); o < plan.UnitCount; o++ {
		u, err := mediaSegmentUnitAt(plan, o)
		if err != nil {
			t.Fatal(err)
		}
		units = append(units, u)
	}
	if err := validateVideoSegmentDurationSum(float64(dur), units); err != nil {
		t.Fatalf("duration plan: %v", err)
	}

	// Deterministic synthesizer: same segment twice → byte-equal; N segments
	// reassemble to the continuous generation under one pinned engine.
	prompt := "a lighthouse at dusk, oil painting"
	seed := int64(7)
	segsA := make([][]byte, segments)
	segsB := make([][]byte, segments)
	for i, u := range units {
		a := synthesizeVideoSegmentForTest(prompt, seed, width, height, fps, u)
		b := synthesizeVideoSegmentForTest(prompt, seed, width, height, fps, u)
		if !bytes.Equal(a, b) {
			t.Fatalf("segment %d not byte-reproducible under one engine", i)
		}
		if err := validateVideoGenerationResult(a, resultRecordContract{Exact: 1, Max: 1}); err != nil {
			t.Fatalf("segment %d invalid: %v", i, err)
		}
		segsA[i], segsB[i] = a, b
	}
	mergedA := reassembleVideoPayloadsForTest(t, segsA, width, height, fps, dur)
	mergedB := reassembleVideoPayloadsForTest(t, segsB, width, height, fps, dur)
	if !bytes.Equal(mergedA, mergedB) {
		t.Fatal("reassembly not byte-identical across independent claim runs")
	}
	continuous := synthesizeVideoSegmentForTest(prompt, seed, width, height, fps, MediaSegmentUnit{
		Ordinal: 0, UnitCount: 1, StartSecs: 0, EndSecs: float64(dur),
	})
	if !bytes.Equal(mergedA, videoPayload(t, continuous)) {
		t.Fatal("segmented reassembly under one engine does not match continuous generation")
	}

	// Coverage gate: every ordinal SUCCEEDED with a SHA-256.
	digest := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}
	results := make([]PrimaryResult, segments)
	for i, seg := range segsA {
		results[i] = PrimaryResult{
			ChunkIndex: i, ResultRef: "seg",
			Artifact: &VerificationArtifact{Key: "k", SHA256: digest(seg), Bytes: int64(len(seg))},
		}
	}
	if err := validateMediaMergeCoverage(int64(segments), results); err != nil {
		t.Fatalf("coverage: %v", err)
	}
}

func TestVideoMissingOrDuplicateOrdinalRefusesSettlement(t *testing.T) {
	// Req 2.
	sha := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	ok := []PrimaryResult{
		{ChunkIndex: 0, ResultRef: "a", Artifact: &VerificationArtifact{Key: "a", SHA256: sha("a"), Bytes: 1}},
		{ChunkIndex: 1, ResultRef: "b", Artifact: &VerificationArtifact{Key: "b", SHA256: sha("b"), Bytes: 1}},
		{ChunkIndex: 2, ResultRef: "c", Artifact: &VerificationArtifact{Key: "c", SHA256: sha("c"), Bytes: 1}},
	}
	if err := validateMediaMergeCoverage(3, ok); err != nil {
		t.Fatalf("complete coverage refused: %v", err)
	}
	if err := validateMediaMergeCoverage(3, []PrimaryResult{ok[0], ok[2]}); err == nil {
		t.Fatal("missing ordinal accepted")
	}
	dup := []PrimaryResult{ok[0], {ChunkIndex: 0, ResultRef: "a2", Artifact: &VerificationArtifact{Key: "a2", SHA256: sha("a2"), Bytes: 1}}, ok[1]}
	if err := validateMediaMergeCoverage(2, dup); err == nil {
		t.Fatal("duplicate ordinal accepted")
	}
}

func TestVideoSegmentDurationsMustSumToRequest(t *testing.T) {
	// Req 3 — the cross-unit invariant segmented media still lacked for generation.
	short := []MediaSegmentUnit{
		{Ordinal: 0, StartSecs: 0, EndSecs: 0.5},
		{Ordinal: 1, StartSecs: 0.5, EndSecs: 1.0}, // requested 2.0
	}
	if err := validateVideoSegmentDurationSum(2.0, short); err == nil {
		t.Fatal("short sum accepted")
	}
	full := []MediaSegmentUnit{
		{Ordinal: 0, StartSecs: 0, EndSecs: 1.0},
		{Ordinal: 1, StartSecs: 1.0, EndSecs: 2.0},
	}
	if err := validateVideoSegmentDurationSum(2.0, full); err != nil {
		t.Fatal(err)
	}
	// Plan-derived units for an allowlisted profile always sum.
	plan, err := deriveVideoSegmentPlan(3, 2.0)
	if err != nil {
		t.Fatal(err)
	}
	var units []MediaSegmentUnit
	for o := int64(0); o < plan.UnitCount; o++ {
		u, err := mediaSegmentUnitAt(plan, o)
		if err != nil {
			t.Fatal(err)
		}
		units = append(units, u)
	}
	if err := validateVideoSegmentDurationSum(2.0, units); err != nil {
		t.Fatalf("plan extents: %v", err)
	}
}

func TestVideoCrossSupplierRedundancyIsRefusedNotMismatched(t *testing.T) {
	// Req 4.
	if err := refuseSegmentedMediaCrossSupplierRedundancy(1, 1.0); err != nil {
		t.Fatalf("single-segment must allow redundancy: %v", err)
	}
	if err := refuseSegmentedMediaCrossSupplierRedundancy(4, 0); err != nil {
		t.Fatalf("zero redundancy on multi-segment should pass: %v", err)
	}
	err := refuseSegmentedMediaCrossSupplierRedundancy(4, 0.5)
	if err == nil {
		t.Fatal("multi-segment with redundancy_frac > 0 must be refused")
	}
	if !strings.Contains(err.Error(), "cross-supplier redundancy") {
		t.Fatalf("wrong refusal: %v", err)
	}
	// Submit shape path also refuses rather than scheduling a mismatch.
	sub := jobSubmit{
		JobType: JobType{
			Type: videoGenerationJobType, RenderWidth: 512, RenderHeight: 512,
			FPS: 8, DurationSecs: 2,
		},
		Model:        ModelRef{Kind: "builtin", Ref: videoGenerationModelRef},
		Params:       json.RawMessage(`{"segment_count":3}`),
		Verification: VerificationPolicy{RedundancyFrac: 0.5},
	}
	if _, herr := normalizeAndValidateJobSubmit(sub); herr == nil {
		t.Fatal("submit shape accepted multi-segment cross-supplier redundancy")
	} else if !strings.Contains(herr.msg, "cross-supplier redundancy") {
		t.Fatalf("submit refused for wrong reason: %s", herr.msg)
	}
}

func TestVideoPolicyRefusalsFireAndDoNotRetainPrompt(t *testing.T) {
	// Req 5.
	const prompt = "a nude child on a beach in barcelona"
	err := applyVideoGenerationPolicy(prompt)
	if err == nil {
		t.Fatal("policy accepted a CSAM prompt")
	}
	if !errors.Is(err, errVideoPolicyRefusal) {
		t.Fatalf("not a video policy refusal: %v", err)
	}
	var refusal videoPolicyRefusal
	if !errors.As(err, &refusal) || refusal.Rule != "csam" {
		t.Fatalf("rule = %+v", refusal)
	}
	for _, fragment := range []string{"barcelona", "beach", prompt, "nude child"} {
		if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(fragment)) {
			t.Fatalf("refusal echoed %q: %s", fragment, err)
		}
	}
	// Ordinary prompt still passes.
	if err := applyVideoGenerationPolicy("a lighthouse at dusk, oil painting"); err != nil {
		t.Fatalf("ordinary prompt refused: %v", err)
	}
	// Input validation rejects empty prompts without retaining anything useful.
	_, err = validateVideoGenerationInputBytes([]byte(`{"prompt":"   "}`))
	if err == nil {
		t.Fatal("blank prompt accepted")
	}
}

func TestVideoCellIsNotRoutableUntilLifecycleAndQualityTierSaySo(t *testing.T) {
	// Req 6.
	var found bool
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if cell.Job != videoGenerationJobType {
				continue
			}
			found = true
			if cell.Routable(profile) {
				t.Fatalf("video cell %q is routable at lifecycle %q quality %q",
					cell.ID, cell.EffectiveLifecycle(profile), cell.qualityTierFor(profile))
			}
			if cell.EffectiveLifecycle(profile) == runtimeLifecycleCanary ||
				cell.EffectiveLifecycle(profile) == runtimeLifecycleActive {
				t.Fatal("video cell lifecycle is already at a routable state")
			}
			if advertisedRuntimeJobModel(videoGenerationJobType, cell.Model) {
				t.Fatal("video model is buyer-advertised while cell is below routable")
			}
			if videoGenerationBuyerRoutable(cell.Model) {
				t.Fatal("videoGenerationBuyerRoutable is true for the exercise synthesizer")
			}
		}
	}
	if !found {
		t.Fatal("no video_generation cell in runtime-authority.json")
	}
	// Ordinary buyer quote/submit gate.
	sub := jobSubmit{
		JobType: JobType{
			Type: videoGenerationJobType, RenderWidth: 512, RenderHeight: 512,
			FPS: 8, DurationSecs: 2,
		},
		Model: ModelRef{Ref: videoGenerationModelRef},
	}
	if err := refuseVideoGenerationIfNotRoutable(sub); err == nil {
		t.Fatal("ordinary buyer gate did not refuse the exercise synthesizer")
	}
}

func TestVideoNSegmentsPriceAsNUnits(t *testing.T) {
	// Req 7.
	jt := JobType{
		Type: videoGenerationJobType, RenderWidth: 512, RenderHeight: 512,
		FPS: 8, DurationSecs: 2,
	}
	one := settlementInputUnitsForJobType(jt, 1, 100)
	four := settlementInputUnitsForJobType(jt, 4, 100)
	if one <= 0 {
		t.Fatalf("single-segment units = %v", one)
	}
	// pixels * frames * records
	wantOne := float64(512*512) * float64(8*2) * 1
	if one != wantOne {
		t.Fatalf("single-segment units = %v, want %v", one, wantOne)
	}
	if four != one*4 {
		t.Fatalf("4 segments units = %v, want 4×%v = %v", four, one, one*4)
	}
	// Media rendering remains pixel-only; video extends by frame count.
	render := settlementInputUnitsForJobType(JobType{
		Type: "media_rendering", RenderWidth: 512, RenderHeight: 512,
	}, 1, 100)
	if render != float64(512*512) {
		t.Fatalf("render units drifted: %v", render)
	}
	if one <= render {
		t.Fatalf("video units %v should exceed still-image pixels %v by the frame count", one, render)
	}
}

func TestVideoLicenceGateRunsOnRequestPath(t *testing.T) {
	if err := videoGenerationLicenceGate(videoGenerationModelRef); err != nil {
		t.Fatalf("exercise synthesizer licence refused: %v", err)
	}
	// OpenRAIL without a use policy is a breach (same structural obligation as image).
	err := validateVideoModelLicence("future-svd", "CreativeML-OpenRAIL-M", false)
	if err == nil {
		t.Fatal("OpenRAIL video model accepted without downstream use policy")
	}
	if !strings.Contains(err.Error(), "video model") {
		t.Fatalf("refusal not video-labelled: %v", err)
	}
	if err := validateVideoModelLicence("future-svd", "CreativeML-OpenRAIL-M", true); err != nil {
		t.Fatalf("OpenRAIL with enforced policy refused: %v", err)
	}
}

func TestVideoVerificationClassIsByteExact(t *testing.T) {
	if !byteExactJobType(videoGenerationJobType) {
		t.Fatal("video_generation must remain byte-exact; do not invent a perceptual class")
	}
	a := []byte("MERCVIDEO1\nsame")
	b := []byte("MERCVIDEO1\nsame")
	c := []byte("MERCVIDEO1\ndiff")
	if !resultsAgree(videoGenerationJobType, a, b) {
		t.Fatal("identical artifacts disagreed")
	}
	if resultsAgree(videoGenerationJobType, a, c) {
		t.Fatal("divergent artifacts agreed — would paper over cross-supplier nondeterminism")
	}
}

// synthesizeVideoSegmentForTest mirrors the agent merc-video-synth-v1 envelope
// so control-plane reassembly and verification can be exercised without a
// live worker. The algorithm is intentionally trivial and fully determined by
// (prompt, seed, geometry, ordinal extents).
func synthesizeVideoSegmentForTest(
	prompt string, seed int64,
	width, height, fps uint32,
	unit MediaSegmentUnit,
) []byte {
	startFrame := int(unit.StartSecs * float64(fps))
	endFrame := int(unit.EndSecs * float64(fps))
	if endFrame <= startFrame {
		endFrame = startFrame + 1
	}
	frameCount := endFrame - startFrame
	var out bytes.Buffer
	out.WriteString("MERCVIDEO1\n")
	_, _ = out.WriteString(formatVideoHeader(width, height, fps, uint32(frameCount), seed, unit.Ordinal, unit.UnitCount))
	// Deterministic RGB fill from prompt hash + seed + absolute frame index.
	base := sha256.Sum256([]byte(prompt))
	pixels := int(width) * int(height) * 3
	frame := make([]byte, pixels)
	for f := 0; f < frameCount; f++ {
		abs := startFrame + f
		for i := 0; i < pixels; i++ {
			frame[i] = byte(int(base[i%32]) + int(seed) + abs*3 + i)
		}
		out.Write(frame)
	}
	return out.Bytes()
}

func formatVideoHeader(width, height, fps, frames uint32, seed, ordinal, unitCount int64) string {
	return fmt.Sprintf("%d %d %d %d %d %d %d\n",
		width, height, fps, frames, seed, ordinal, unitCount)
}

func videoPayload(t *testing.T, seg []byte) []byte {
	t.Helper()
	if !bytes.HasPrefix(seg, []byte("MERCVIDEO1\n")) {
		t.Fatal("missing envelope")
	}
	rest := seg[len("MERCVIDEO1\n"):]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		t.Fatal("missing header")
	}
	return rest[nl+1:]
}

func reassembleVideoPayloadsForTest(t *testing.T, segments [][]byte, width, height, fps, durationSecs uint32) []byte {
	t.Helper()
	var frames []byte
	totalFrames := 0
	for _, seg := range segments {
		payload := videoPayload(t, seg)
		totalFrames += len(payload) / (int(width) * int(height) * 3)
		frames = append(frames, payload...)
	}
	wantFrames := int(durationSecs * fps)
	if totalFrames != wantFrames {
		t.Fatalf("reassembled %d frames, want %d", totalFrames, wantFrames)
	}
	return frames
}
