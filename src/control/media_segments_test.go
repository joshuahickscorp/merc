package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMediaSegmentCountFromParams(t *testing.T) {
	n, err := mediaSegmentCountFromParams(nil)
	if err != nil || n != 1 {
		t.Fatalf("default segment count = %d, %v", n, err)
	}
	n, err = mediaSegmentCountFromParams(json.RawMessage(`{"segment_count":4}`))
	if err != nil || n != 4 {
		t.Fatalf("segment_count=4 → %d, %v", n, err)
	}
	n, err = mediaSegmentCountFromParams(json.RawMessage(`{"split_size":3}`))
	if err != nil || n != 3 {
		t.Fatalf("split_size alias → %d, %v", n, err)
	}
	if _, err := mediaSegmentCountFromParams(json.RawMessage(`{"segment_count":999}`)); err == nil {
		t.Fatal("oversized segment_count accepted")
	}
}

func TestMediaSegmentUnitAtIsContiguousBijection(t *testing.T) {
	plan, err := deriveMediaSegmentPlan(3, 6.0)
	must(t, err)
	var units []MediaSegmentUnit
	for o := int64(0); o < plan.UnitCount; o++ {
		u, err := mediaSegmentUnitAt(plan, o)
		must(t, err)
		if u.Ordinal != o {
			t.Fatalf("ordinal mismatch: got %d want %d", u.Ordinal, o)
		}
		units = append(units, u)
	}
	must(t, validateMediaSegmentExtents(6.0, units))
}

func TestMediaSegmentExtentsSummingShortOfSourceIsRefused(t *testing.T) {
	units := []MediaSegmentUnit{
		{Ordinal: 0, StartSecs: 0, EndSecs: 1},
		{Ordinal: 1, StartSecs: 1, EndSecs: 1.5}, // short of source 3.0
	}
	if err := validateMediaSegmentExtents(3.0, units); err == nil {
		t.Fatal("short extent sum was accepted")
	}
	// Gap between units
	gapped := []MediaSegmentUnit{
		{Ordinal: 0, StartSecs: 0, EndSecs: 1},
		{Ordinal: 1, StartSecs: 1.5, EndSecs: 3},
	}
	if err := validateMediaSegmentExtents(3.0, gapped); err == nil {
		t.Fatal("gapped extents were accepted")
	}
	// Full cover is accepted
	full := []MediaSegmentUnit{
		{Ordinal: 0, StartSecs: 0, EndSecs: 1.5},
		{Ordinal: 1, StartSecs: 1.5, EndSecs: 3.0},
	}
	must(t, validateMediaSegmentExtents(3.0, full))
}

func TestMediaMergeCoverageRefusesMissingAndDuplicateOrdinals(t *testing.T) {
	sha := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	ok := []PrimaryResult{
		{ChunkIndex: 0, ResultRef: "a", Artifact: &VerificationArtifact{Key: "a", SHA256: sha("a"), Bytes: 1}},
		{ChunkIndex: 1, ResultRef: "b", Artifact: &VerificationArtifact{Key: "b", SHA256: sha("b"), Bytes: 1}},
		{ChunkIndex: 2, ResultRef: "c", Artifact: &VerificationArtifact{Key: "c", SHA256: sha("c"), Bytes: 1}},
	}
	mustf(t, validateMediaMergeCoverage(3, ok), "complete coverage refused: %v")
	// Missing ordinal 1
	missing := []PrimaryResult{ok[0], ok[2]}
	if err := validateMediaMergeCoverage(3, missing); err == nil {
		t.Fatal("missing ordinal was accepted")
	}
	// Duplicate ordinal via non-contiguous index (chunk 0,0,1)
	dup := []PrimaryResult{ok[0], {ChunkIndex: 0, ResultRef: "a2", Artifact: &VerificationArtifact{Key: "a2", SHA256: sha("a2"), Bytes: 1}}, ok[1]}
	if err := validateMediaMergeCoverage(2, dup); err == nil {
		t.Fatal("duplicate ordinal was accepted")
	}
	// Extra trailing ordinal beyond unit count
	if err := validateMediaMergeCoverage(2, ok); err == nil {
		t.Fatal("extra ordinal beyond unit count was accepted")
	}
}

func TestRefuseSegmentedMediaCrossSupplierRedundancy(t *testing.T) {
	mustf(t, refuseSegmentedMediaCrossSupplierRedundancy(1, 1.0), "single-segment must still allow redundancy: %v")
	mustf(t, refuseSegmentedMediaCrossSupplierRedundancy(3, 0), "zero redundancy on multi-segment should be fine: %v")
	if err := refuseSegmentedMediaCrossSupplierRedundancy(3, 0.5); err == nil {
		t.Fatal("multi-segment with redundancy_frac > 0 must be refused")
	}
}

func TestSingleSegmentMediaScanPinsCurrentNumbers(t *testing.T) {
	// Identical fixture shape to the historical single-artifact pin.
	input := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p'}, bytes.Repeat([]byte{'x'}, 16)...)
	scan, err := mediaInputScan(input, 1)
	must(t, err)
	if scan.Records != 1 || scan.Bytes != len(input) {
		t.Fatalf("single-segment pin broken: %+v", scan)
	}
	units := settlementInputUnitsForMediaSegments(scan.Records, int64(scan.Bytes))
	// bytes=24 → bytes/4=6; single-segment multiplies the historical pin by 1.
	if units != 6 {
		t.Fatalf("single-segment settlement units=%v, want 6 (pinned historical geometry)", units)
	}
}

func TestMediaSegmentParamsForDispatch(t *testing.T) {
	raw, err := mediaSegmentParamsForDispatch(4, 2)
	must(t, err)
	var payload struct {
		MediaSegment struct {
			Ordinal   int    `json:"ordinal"`
			UnitCount int    `json:"unit_count"`
			Version   string `json:"version"`
		} `json:"media_segment"`
	}
	must(t, json.Unmarshal(raw, &payload))
	if payload.MediaSegment.Ordinal != 2 || payload.MediaSegment.UnitCount != 4 {
		t.Fatalf("dispatch params = %+v", payload.MediaSegment)
	}
	if payload.MediaSegment.Version != mediaSegmentPlanVersion {
		t.Fatalf("version = %q", payload.MediaSegment.Version)
	}
	if _, err := mediaSegmentParamsForDispatch(2, 2); err == nil {
		t.Fatal("out-of-range ordinal accepted")
	}
}

func TestMediaSegmentsReassembleViaMergeCoverageAndConcat(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	gen := exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=blue:s=160x90:r=24", "-t", "1.0",
		"-pix_fmt", "yuv420p", "-c:v", "libx264", "-f", "mp4", source)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate source: %v (%s)", err, out)
	}

	// Produce two segments twice under the same engine flags (agent contract).
	encodeSeg := func(ordinal int) []byte {
		outPath := filepath.Join(dir, "seg.mp4")
		start := float64(ordinal) * 0.5
		cmd := exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
			"-fflags", "+bitexact",
			"-ss", formatFloat(start), "-t", "0.5",
			"-i", source,
			"-an", "-vf", "scale=160:90", "-r", "24",
			"-c:v", "libx264", "-preset", "medium", "-b:v", "400k",
			"-maxrate", "400k", "-bufsize", "800k",
			"-pix_fmt", "yuv420p", "-threads", "1", "-flags:v", "+bitexact",
			"-force_key_frames", "expr:eq(n,0)",
			"-x264-params", "keyint=250:min-keyint=1:scenecut=0:bframes=0:open-gop=0",
			"-movflags", "+faststart", "-f", "mp4", outPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("encode seg %d: %v (%s)", ordinal, err, out)
		}
		b, err := os.ReadFile(outPath)
		must(t, err)
		return b
	}
	seg0a, seg1a := encodeSeg(0), encodeSeg(1)
	seg0b, seg1b := encodeSeg(0), encodeSeg(1)
	if !bytes.Equal(seg0a, seg0b) || !bytes.Equal(seg1a, seg1b) {
		t.Fatal("segment encodes under pinned flags are not byte-reproducible")
	}

	digest := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}
	results := []PrimaryResult{
		{ChunkIndex: 0, ResultRef: "seg0", Artifact: &VerificationArtifact{Key: "seg0", SHA256: digest(seg0a), Bytes: int64(len(seg0a))}},
		{ChunkIndex: 1, ResultRef: "seg1", Artifact: &VerificationArtifact{Key: "seg1", SHA256: digest(seg1a), Bytes: int64(len(seg1a))}},
	}
	mustf(t, validateMediaMergeCoverage(2, results), "coverage: %v")

	// Production merge path for N>1 is ffmpeg concat demuxer stream-copy.
	// Exercise it twice and require byte-identical reassembly.
	merged1 := concatSegmentsForTest(t, dir, "m1", [][]byte{seg0a, seg1a})
	merged2 := concatSegmentsForTest(t, dir, "m2", [][]byte{seg0b, seg1b})
	if !bytes.Equal(merged1, merged2) {
		t.Fatal("reassembly is not byte-identical across independent segment claims")
	}
	mustf(t, validateMediaTranscodeResult(merged1, resultRecordContract{Exact: 1, Max: 1}), "merged output invalid: %v")

	// Single-shot continuous encode (N=1 path shape) remains valid; it is not
	// required to match segmented reassembly under ABR (measured refuse).
	singlePath := filepath.Join(dir, "single.mp4")
	single := exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-fflags", "+bitexact", "-i", source, "-an",
		"-vf", "scale=160:90", "-r", "24",
		"-c:v", "libx264", "-preset", "medium", "-b:v", "400k",
		"-maxrate", "400k", "-bufsize", "800k",
		"-pix_fmt", "yuv420p", "-threads", "1", "-flags:v", "+bitexact",
		"-movflags", "+faststart", "-f", "mp4", singlePath)
	if out, err := single.CombinedOutput(); err != nil {
		t.Fatalf("single-shot: %v (%s)", err, out)
	}
	singleBytes, err := os.ReadFile(singlePath)
	must(t, err)
	mustf(t, validateMediaTranscodeResult(singleBytes, resultRecordContract{Exact: 1, Max: 1}), "single-shot invalid: %v")
	if bytes.Equal(singleBytes, merged1) {
		t.Log("continuous encode matched segmented reassembly for this fixture")
	} else {
		t.Logf("evidence: continuous (%d B) != segmented reassembly (%d B); cross-path byte identity refused",
			len(singleBytes), len(merged1))
	}
}

func concatSegmentsForTest(t *testing.T, dir, tag string, segments [][]byte) []byte {
	t.Helper()
	listPath := filepath.Join(dir, tag+"-list.txt")
	var list bytes.Buffer
	for i, seg := range segments {
		p := filepath.Join(dir, tag+formatFloat(float64(i))+".mp4")
		must(t, os.WriteFile(p, seg, 0o600))
		if _, err := list.WriteString("file '" + p + "'\n"); err != nil {
			t.Fatal(err)
		}
	}
	must(t, os.WriteFile(listPath, list.Bytes(), 0o600))
	outPath := filepath.Join(dir, tag+"-merged.mp4")
	cmd := exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c", "copy", "-movflags", "+faststart", "-f", "mp4", outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("concat %s: %v (%s)", tag, err, out)
	}
	b, err := os.ReadFile(outPath)
	must(t, err)
	return b
}

func formatFloat(v float64) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
