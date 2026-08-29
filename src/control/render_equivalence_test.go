package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func solidPNG(t *testing.T, w, h int, r, g, b uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func solidPixels(w, h int, r, g, b uint8) PixelBuffer {
	pix := make([]byte, w*h*3)
	for i := 0; i < w*h; i++ {
		pix[i*3] = r
		pix[i*3+1] = g
		pix[i*3+2] = b
	}
	return PixelBuffer{Width: w, Height: h, Channels: 3, Pix: pix}
}

func TestComparePixelsExactAndDivergence(t *testing.T) {
	a := solidPixels(4, 2, 10, 20, 30)
	b := solidPixels(4, 2, 10, 20, 30)
	eq := ComparePixels(a, b)
	if !eq.Holds || eq.DifferingPixels != 0 || eq.MaxAbs != 0 {
		t.Fatalf("identical pixels: %+v", eq)
	}
	b.Pix[3] = 12 // first channel of pixel (1,0)
	diff := ComparePixels(a, b)
	if diff.Holds {
		t.Fatal("expected L1 fail")
	}
	if diff.DifferingPixels != 1 || diff.MaxAbs != 2 {
		t.Fatalf("want 1 pixel / max 2, got %+v", diff)
	}
	if len(diff.FirstDiffXY) != 2 || diff.FirstDiffXY[0] != 1 || diff.FirstDiffXY[1] != 0 {
		t.Fatalf("first diff %+v", diff.FirstDiffXY)
	}
	if diff.MeanAbs <= 0 {
		t.Fatal("mean abs should be > 0")
	}
}

func TestL1HashOnlyEqualDoesNotNeedPNG(t *testing.T) {
	a := solidPixels(8, 8, 40, 50, 60)
	da := DigestPixelBuffer(a)
	db := DigestPixelBuffer(solidPixels(8, 8, 40, 50, 60))
	if da == "" || da != db {
		t.Fatalf("digests %s vs %s", da, db)
	}
	got := CompareL1(L1Input{PixelDigest: da}, L1Input{PixelDigest: db})
	if !got.Holds {
		t.Fatalf("digest-only L1 must hold: %+v", got)
	}
	if got.Path != l1PathDigest {
		t.Fatalf("path=%s, want %s", got.Path, l1PathDigest)
	}
	if got.Decoded {
		t.Fatal("digest-only equal path must not decode a PNG")
	}
}

func TestL1HashOnlyCatchesOnePixelAndReportsDiagnostics(t *testing.T) {
	a := solidPixels(6, 4, 10, 20, 30)
	b := solidPixels(6, 4, 10, 20, 30)
	b.Pix[(2*6+3)*3+1] = 99 // pixel (3,2) green
	da := DigestPixelBuffer(a)
	db := DigestPixelBuffer(b)
	if da == db {
		t.Fatal("a 1-pixel change must move the decoded-pixel digest")
	}
	got := CompareL1(
		L1Input{Pixels: a, PixelDigest: da},
		L1Input{Pixels: b, PixelDigest: db},
	)
	if got.Holds {
		t.Fatal("L1 must fail on a 1-pixel change")
	}
	if got.Path != l1PathArray {
		t.Fatalf("divergence must walk pixels, path=%s", got.Path)
	}
	if got.DifferingPixels != 1 {
		t.Fatalf("differing=%d, want 1", got.DifferingPixels)
	}
	if got.MaxAbs != int(99-20) {
		t.Fatalf("max_abs=%d", got.MaxAbs)
	}
	if got.MeanAbs <= 0 {
		t.Fatal("mean_abs must be > 0 on divergence")
	}
	if len(got.FirstDiffXY) != 2 || got.FirstDiffXY[0] != 3 || got.FirstDiffXY[1] != 2 {
		t.Fatalf("first diff %+v", got.FirstDiffXY)
	}
}

func TestL1HashOnlyMutationAlterConfirmRevert(t *testing.T) {
	// Mutation-check the contract: flip one byte, L1 goes red, revert, L1 holds.
	a := solidPixels(4, 4, 7, 8, 9)
	b := solidPixels(4, 4, 7, 8, 9)
	orig := b.Pix[0]
	if !CompareL1(L1Input{Pixels: a, PixelDigest: DigestPixelBuffer(a)}, L1Input{Pixels: b, PixelDigest: DigestPixelBuffer(b)}).Holds {
		t.Fatal("pre-mutation L1 must hold")
	}
	b.Pix[0] = orig + 1
	red := CompareL1(L1Input{Pixels: a, PixelDigest: DigestPixelBuffer(a)}, L1Input{Pixels: b, PixelDigest: DigestPixelBuffer(b)})
	if red.Holds {
		t.Fatal("mutated pixel must fail L1 (the test is red if the digest path is blind)")
	}
	if red.DifferingPixels != 1 {
		t.Fatalf("mutation diagnostics: %+v", red)
	}
	b.Pix[0] = orig
	if !CompareL1(L1Input{Pixels: a, PixelDigest: DigestPixelBuffer(a)}, L1Input{Pixels: b, PixelDigest: DigestPixelBuffer(b)}).Holds {
		t.Fatal("reverted pixel must restore L1")
	}
}

func TestL1DigestMismatchWithoutPixelsNamesDivergenceNeed(t *testing.T) {
	got := CompareL1(
		L1Input{PixelDigest: strings.Repeat("a", 64)},
		L1Input{PixelDigest: strings.Repeat("b", 64)},
	)
	if got.Holds {
		t.Fatal("mismatched digests must not hold")
	}
	if got.Error == "" {
		t.Fatal("expected an error asking for the pixel-array divergence path")
	}
	if !strings.Contains(got.Error, "divergence") {
		t.Fatalf("error=%q", got.Error)
	}
}

func TestCachePixelDigestUsesInMemoryBuffer(t *testing.T) {
	buf := solidPixels(3, 3, 1, 2, 3)
	in := L1Input{Pixels: buf}
	CachePixelDigest(&in)
	if in.PixelDigest == "" || in.PixelDigest != DigestPixelBuffer(buf) {
		t.Fatalf("cached digest %q", in.PixelDigest)
	}
}

func TestDigestIsNotPNGContainerHash(t *testing.T) {
	pngA := solidPNG(t, 4, 4, 1, 2, 3)
	pngB, err := insertPNGChunkAfterIHDR(pngA, "tEXt", []byte("Comment\x00stamp"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(pngA, pngB) {
		t.Fatal("fixture: metadata must change container bytes")
	}
	pa, err := DecodePNGPixels(pngA)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := DecodePNGPixels(pngB)
	if err != nil {
		t.Fatal(err)
	}
	if DigestPixelBuffer(pa) != DigestPixelBuffer(pb) {
		t.Fatal("decoded-pixel digest must ignore PNG eXIf/tEXt stamps")
	}
	if sha256Hex(pngA) == sha256Hex(pngB) {
		t.Fatal("container hashes collided")
	}
}

func TestCompareRenderPairStillDecodesWhenNoDigestCached(t *testing.T) {
	pngA := solidPNG(t, 2, 2, 8, 8, 8)
	pngB, err := insertPNGChunkAfterIHDR(pngA, "eXIf", []byte{1, 2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	rep := CompareRenderPair(pngA, pngB, nil, nil, QualitySpec{}, EquivL1PixelExact)
	if !rep.L1.Holds {
		t.Fatalf("L1 should hold: %+v", rep.L1)
	}
	if !rep.MeetsRequired {
		t.Fatal("required L1 should hold")
	}
	if rep.L0.RawBytesEqual {
		t.Fatal("raw PNG bytes should differ (eXIf trap)")
	}
}

func TestPythonEquivalenceAgreesOnFixtures(t *testing.T) {
	script := filepath.Join("..", "..", "ops", "scripts", "lib", "render_equivalence.py")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("python twin missing: %v", err)
	}
	dir := t.TempDir()
	pngA := solidPNG(t, 6, 4, 40, 50, 60)
	pngB, err := insertPNGChunkAfterIHDR(pngA, "tEXt", []byte("Software\x00Blender"))
	if err != nil {
		t.Fatal(err)
	}
	pngPathA := filepath.Join(dir, "a.png")
	pngPathB := filepath.Join(dir, "b.png")
	if err := os.WriteFile(pngPathA, pngA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pngPathB, pngB, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", script, "compare",
		"--png-a", pngPathA, "--png-b", pngPathB,
		"--required", EquivL1PixelExact)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python compare: %v\n%s", err, out)
	}
	var got EquivalenceReport
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("python json: %v\n%s", err, out)
	}
	want := CompareRenderPair(pngA, pngB, nil, nil, QualitySpec{}, EquivL1PixelExact)
	if got.L0.Holds != want.L0.Holds || got.L0.RawBytesEqual != want.L0.RawBytesEqual {
		t.Fatalf("L0 python=%+v go=%+v", got.L0, want.L0)
	}
	if got.L1.Holds != want.L1.Holds {
		t.Fatalf("L1 python=%+v go=%+v", got.L1, want.L1)
	}
}

func TestPythonL1DigestAgreesWithGo(t *testing.T) {
	script := filepath.Join("..", "..", "ops", "scripts", "lib", "render_equivalence.py")
	png := solidPNG(t, 5, 3, 9, 8, 7)
	dir := t.TempDir()
	path := filepath.Join(dir, "p.png")
	if err := os.WriteFile(path, png, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", script, "pixel-digest", "--png", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python pixel-digest: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	pa, err := DecodePNGPixels(png)
	if err != nil {
		t.Fatal(err)
	}
	want := DigestPixelBuffer(pa)
	if got != want {
		t.Fatalf("python digest %s != go %s", got, want)
	}
}

func TestAmdahlCeilingIsReciprocalNotASpeedup(t *testing.T) {
	// 17.3 s serial + 1654 s parallel → ceiling ~96.4. That is a CEILING.
	a := amdahlsOf(17.3, 1654, 100, "serial_png")
	if a.SerialFraction < 0.010 || a.SerialFraction > 0.011 {
		t.Fatalf("fraction %v", a.SerialFraction)
	}
	if a.Ceiling < 96 || a.Ceiling > 98 {
		t.Fatalf("ceiling %v", a.Ceiling)
	}
	if !strings.Contains(a.Note, "CEILING") {
		t.Fatalf("ceiling must be labelled: %s", a.Note)
	}
	if strings.Contains(strings.ToLower(a.Note), "speedup of") {
		t.Fatal("must not present the ceiling as an achieved speedup")
	}
}

func TestPipelinedVerifyOverlapsNextRender(t *testing.T) {
	n := 4
	refs := make([]L1Input, n)
	pixels := make([]PixelBuffer, n)
	for i := 0; i < n; i++ {
		pixels[i] = solidPixels(8, 8, uint8(10+i), 20, 30)
		refs[i] = L1Input{Pixels: pixels[i], PixelDigest: DigestPixelBuffer(pixels[i])}
	}
	render := func(index int) (RenderedFrame, error) {
		time.Sleep(12 * time.Millisecond)
		buf := pixels[index]
		return RenderedFrame{
			Index:       index,
			Pixels:      buf,
			PixelDigest: DigestPixelBuffer(buf),
		}, nil
	}
	run, err := RunPipelinedL1(render, refs)
	if err != nil {
		t.Fatal(err)
	}
	if !run.AllHold {
		t.Fatalf("all frames should hold: %+v", run.Frames)
	}
	if run.OverlappedFrames < n-1 {
		t.Fatalf("expected verify(N) to overlap render(N+1) for %d pairs, got %d", n-1, run.OverlappedFrames)
	}
	if run.Mode != VerifyPipelinedL1 {
		t.Fatalf("mode %s", run.Mode)
	}
}

func TestSerialPNGDecodeIsSlowerThanHashOnlyOnEqualFrames(t *testing.T) {
	n := 6
	frames := make([]RenderedFrame, n)
	refs := make([]L1Input, n)
	for i := 0; i < n; i++ {
		raw := solidPNG(t, 64, 64, uint8(i+1), 40, 50)
		buf, err := DecodePNGPixels(raw)
		if err != nil {
			t.Fatal(err)
		}
		frames[i] = RenderedFrame{Index: i, PNG: raw, Pixels: buf, PixelDigest: DigestPixelBuffer(buf)}
		refs[i] = L1Input{PNG: raw, Pixels: buf, PixelDigest: frames[i].PixelDigest}
	}
	serial := VerifySerialPNGDecodeL1(frames, refs)
	hash := VerifyHashOnlyL1Run(frames, refs)
	if !serial.AllHold || !hash.AllHold {
		t.Fatalf("both must hold: serial=%+v hash=%+v", serial, hash)
	}
	if hash.VerifyNSSum <= 0 || serial.VerifyNSSum <= 0 {
		t.Fatalf("expected positive verify times: serial=%d hash=%d", serial.VerifyNSSum, hash.VerifyNSSum)
	}
	if hash.VerifyNSSum >= serial.VerifyNSSum {
		t.Fatalf("hash-only (%d ns) should beat serial PNG decode (%d ns)", hash.VerifyNSSum, serial.VerifyNSSum)
	}
	for _, rec := range hash.Frames {
		if rec.Path != l1PathDigest {
			t.Fatalf("hash-only path=%s", rec.Path)
		}
	}
}

func TestRenderVerifyPipelineListedAsOptInSkip(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "ops", "scripts", "allowed-test-skips.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("TestRenderVerifyPipelineBench")) {
		t.Fatal("TestRenderVerifyPipelineBench must be listed in ops/scripts/allowed-test-skips.txt")
	}
	if !bytes.Contains(raw, []byte("MERC_RENDER_VERIFY_PIPELINE")) {
		t.Fatal("allowed-test-skips.txt must name MERC_RENDER_VERIFY_PIPELINE next to the bench")
	}
}

func TestRenderVerifyServicePinsCyclesCPU(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "ops", "scripts", "render", "verify", "blender_service.py"))
	if err != nil {
		t.Fatalf("read blender service: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, `scene.render.engine = "CYCLES"`) {
		t.Fatal("blender service must pin CYCLES")
	}
	if !strings.Contains(src, `scene.cycles.device = "CPU"`) {
		t.Fatal("blender service must pin CPU")
	}
	if strings.Contains(src, `= "BLENDER_EEVEE"`) || strings.Contains(src, `= "BLENDER_EEVEE_NEXT"`) {
		t.Fatal("blender service must never assign EEVEE")
	}
	if !strings.Contains(src, "use_adaptive_sampling") {
		t.Fatal("blender service must mention adaptive sampling so the OFF pin is reviewable")
	}
	if !strings.Contains(src, "L1 is decoded 8-bit RGB") {
		t.Fatal("blender service must document that L1 digest is of decoded 8-bit RGB, not computed as a linear-float hash")
	}
}
