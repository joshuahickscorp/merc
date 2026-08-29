package main

// Headless Cycles-CPU render baseline. Measurement only — not a workload
// family, not a price, not a cell identity.
//
// Opt-in only — never part of make test / make ci:
//
//	MERC_BLENDER_RENDER_BENCH=1 \
//	  go test -count=1 -run '^TestBlenderRenderBaseline$' -timeout 45m .
//
// Writes evidence/perf/blender-render-baseline.json when run from src/control/.
//
// Cycles on CPU only. The bpy script refuses any other engine/device.
// EEVEE is never selected (background EEVEE aborts with exit 134).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	blenderRenderBenchEnv         = "MERC_BLENDER_RENDER_BENCH"
	blenderRenderBenchBinEnv      = "MERC_BLENDER_BIN"
	blenderRenderBenchGridEnv     = "MERC_BLENDER_RENDER_BENCH_GRID"
	blenderRenderBenchWidthEnv    = "MERC_BLENDER_RENDER_BENCH_WIDTH"
	blenderRenderBenchSamplesEnv  = "MERC_BLENDER_RENDER_BENCH_SAMPLES"
	blenderRenderBenchWorkdirEnv  = "MERC_BLENDER_RENDER_BENCH_WORKDIR"
	blenderRenderBenchEvidenceRel = "evidence/perf/blender-render-baseline.json"
	blenderRenderScriptRel        = "ops/scripts/blender-render-baseline.py"
	defaultBlenderBin             = "/Applications/Blender.app/Contents/MacOS/Blender"
	defaultBlenderGrid            = 2
	defaultBlenderWidth           = 256
	defaultBlenderSamples         = 32
	defaultBlenderSeed            = 1
)

func TestBlenderRenderBaseline(t *testing.T) {
	if os.Getenv(blenderRenderBenchEnv) != "1" {
		t.Skip("set MERC_BLENDER_RENDER_BENCH=1 to measure a headless Cycles-CPU Blender baseline")
	}
	if runtime.GOOS != "darwin" {
		t.Fatalf("this baseline records Darwin /usr/bin/time -l peak RSS; host is %s", runtime.GOOS)
	}

	blender := strings.TrimSpace(os.Getenv(blenderRenderBenchBinEnv))
	if blender == "" {
		blender = defaultBlenderBin
	}
	if _, err := os.Stat(blender); err != nil {
		t.Fatalf("blender binary %s: %v", blender, err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", blenderRenderScriptRel))
	if err != nil {
		t.Fatalf("script path: %v", err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("bpy script %s: %v", script, err)
	}

	grid := defaultBlenderGrid
	if v := strings.TrimSpace(os.Getenv(blenderRenderBenchGridEnv)); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 1 || n > 16 {
			t.Fatalf("%s=%q: need integer in [1,16]", blenderRenderBenchGridEnv, v)
		}
		grid = n
	}
	width := defaultBlenderWidth
	if v := strings.TrimSpace(os.Getenv(blenderRenderBenchWidthEnv)); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 16 || n%grid != 0 {
			t.Fatalf("%s=%q: need integer >= 16 divisible by grid=%d", blenderRenderBenchWidthEnv, v, grid)
		}
		width = n
	}
	samples := defaultBlenderSamples
	if v := strings.TrimSpace(os.Getenv(blenderRenderBenchSamplesEnv)); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 1 {
			t.Fatalf("%s=%q: need integer >= 1", blenderRenderBenchSamplesEnv, v)
		}
		samples = n
	}
	height := width

	workdir := strings.TrimSpace(os.Getenv(blenderRenderBenchWorkdirEnv))
	if workdir == "" {
		workdir = t.TempDir()
	} else if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("workdir: %v", err)
	}

	host, _ := os.Hostname()
	startedAt := time.Now().UTC()
	version, versionErr := blenderVersionString(blender)
	if versionErr != nil {
		t.Fatalf("blender --version: %v", versionErr)
	}

	command := "cd src/control && MERC_BLENDER_RENDER_BENCH=1 go test -count=1 -run '^TestBlenderRenderBaseline$' -timeout 45m ."
	report := blenderRenderReport{
		Classification: "MEASURED",
		GeneratedAt:    startedAt.Format(time.RFC3339),
		SourceCommit:   blenderRenderSourceCommit(),
		Host:           host,
		NumCPU:         runtime.NumCPU(),
		GOMAXPROCS:     runtime.GOMAXPROCS(0),
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		Invocation: blenderRenderInvocation{
			EnvGate:                blenderRenderBenchEnv + "=1",
			ExcludedFromNormalGate: true,
			ExclusionProof:         "TestBlenderRenderBaseline skips unless MERC_BLENDER_RENDER_BENCH=1; listed in ops/scripts/allowed-test-skips.txt; make test / make ci never set the env var",
			Command:                command,
			BlenderBin:             blender,
			BlenderVersion:         version,
			Script:                 blenderRenderScriptRel,
			Engine:                 "CYCLES",
			Device:                 "CPU",
			Width:                  width,
			Height:                 height,
			Samples:                samples,
			Seed:                   defaultBlenderSeed,
			TileGrid:               []int{grid, grid},
			TileCount:              grid * grid,
			OverscanPx:             0,
			Scene:                  "procedural_suzanne_plane_area",
			AssetsDownloaded:       false,
			EEVEEInvoked:           false,
		},
		Honesty: blenderRenderHonesty{
			WhatThisProves:       "on this host, with this Blender binary, headless Cycles CPU can render one procedurally generated scene as a single frame and as N independent border-cropped tiles; the wall time, peak RSS and output bytes of those invocations; the wall time of CPU-side tile assembly; and whether the assembled tile image is bit-identical (file sha256 and raw pixels) to the single-frame render",
			WhatThisDoesNotProve: "this is one scene on one Mac, is not a Merc workload, is not priced, and proves nothing about heterogeneous GPU placement",
			Guards: []string{
				"Cycles CPU only; EEVEE is never selected (background EEVEE aborts with exit 134)",
				"scene is generated in-process from built-in primitives (Suzanne, plane, area light); no downloaded assets",
				"the same .blend bytes are used for the full-frame and every tile render",
				"fixed seed, adaptive sampling off, denoising off, Cycles auto-tile off, Standard view transform",
				"tiles are independent Blender processes (farm-shaped), not in-process Cycles auto-tile",
				"bit-identity reports both sha256(file) and sha256/raw-compare of tightly packed RGB pixels",
				"peak RSS is Darwin maximum resident set size from /usr/bin/time -l of each Blender process",
				"this harness is the only writer of this file",
			},
		},
	}

	blendPath := filepath.Join(workdir, "scene.blend")
	build := runBlenderMeasured(t, blender, script, workdir, []string{
		"--mode=build",
		"--blend=" + blendPath,
		fmt.Sprintf("--width=%d", width),
		fmt.Sprintf("--height=%d", height),
		fmt.Sprintf("--samples=%d", samples),
		fmt.Sprintf("--seed=%d", defaultBlenderSeed),
	})
	if build.ExitCode != 0 {
		t.Fatalf("scene build failed: exit=%d\nstdout:\n%s\nstderr:\n%s", build.ExitCode, build.Stdout, build.Stderr)
	}
	if _, err := os.Stat(blendPath); err != nil {
		t.Fatalf("blend was not written: %v", err)
	}

	fullPath := filepath.Join(workdir, "full.png")
	full := runBlenderMeasured(t, blender, script, workdir, []string{
		"--mode=full",
		"--blend=" + blendPath,
		"--out=" + fullPath,
		fmt.Sprintf("--width=%d", width),
		fmt.Sprintf("--height=%d", height),
		fmt.Sprintf("--samples=%d", samples),
		fmt.Sprintf("--seed=%d", defaultBlenderSeed),
	})
	if full.ExitCode != 0 {
		t.Fatalf("full-frame render failed: exit=%d\nstdout:\n%s\nstderr:\n%s", full.ExitCode, full.Stdout, full.Stderr)
	}
	fullBytes, fullSHA, err := readFileSHA(fullPath)
	if err != nil {
		t.Fatalf("read full frame: %v", err)
	}
	fullImg, err := decodePNG(fullBytes)
	if err != nil {
		t.Fatalf("decode full frame: %v", err)
	}
	if fullImg.Bounds().Dx() != width || fullImg.Bounds().Dy() != height {
		t.Fatalf("full frame size %dx%d, want %dx%d", fullImg.Bounds().Dx(), fullImg.Bounds().Dy(), width, height)
	}

	tileW := width / grid
	tileH := height / grid
	tiles := make([]blenderRenderTile, 0, grid*grid)
	tileImgs := make([]image.Image, 0, grid*grid)
	var tileWallSum float64
	tileWallMax := 0.0
	peakRSS := max64(build.PeakRSSBytes, full.PeakRSSBytes)
	for ty := 0; ty < grid; ty++ {
		for tx := 0; tx < grid; tx++ {
			out := filepath.Join(workdir, fmt.Sprintf("tile-%d-%d.png", tx, ty))
			got := runBlenderMeasured(t, blender, script, workdir, []string{
				"--mode=tile",
				"--blend=" + blendPath,
				"--out=" + out,
				fmt.Sprintf("--width=%d", width),
				fmt.Sprintf("--height=%d", height),
				fmt.Sprintf("--samples=%d", samples),
				fmt.Sprintf("--seed=%d", defaultBlenderSeed),
				fmt.Sprintf("--tile-x=%d", tx),
				fmt.Sprintf("--tile-y=%d", ty),
				fmt.Sprintf("--grid=%d", grid),
			})
			if got.ExitCode != 0 {
				t.Fatalf("tile (%d,%d) failed: exit=%d\nstdout:\n%s\nstderr:\n%s", tx, ty, got.ExitCode, got.Stdout, got.Stderr)
			}
			raw, sum, readErr := readFileSHA(out)
			if readErr != nil {
				t.Fatalf("read tile (%d,%d): %v", tx, ty, readErr)
			}
			img, decErr := decodePNG(raw)
			if decErr != nil {
				t.Fatalf("decode tile (%d,%d): %v", tx, ty, decErr)
			}
			if img.Bounds().Dx() != tileW || img.Bounds().Dy() != tileH {
				t.Fatalf("tile (%d,%d) size %dx%d, want %dx%d", tx, ty, img.Bounds().Dx(), img.Bounds().Dy(), tileW, tileH)
			}
			tiles = append(tiles, blenderRenderTile{
				ID:                 fmt.Sprintf("%d,%d", tx, ty),
				TileX:              tx,
				TileY:              ty,
				Width:              img.Bounds().Dx(),
				Height:             img.Bounds().Dy(),
				WallSeconds:        got.WallSeconds,
				BlenderTimeSeconds: got.BlenderTimeSeconds,
				PeakRSSBytes:       got.PeakRSSBytes,
				OutputBytes:        int64(len(raw)),
				SHA256:             sum,
				ExitCode:           got.ExitCode,
			})
			tileImgs = append(tileImgs, img)
			tileWallSum += got.WallSeconds
			if got.WallSeconds > tileWallMax {
				tileWallMax = got.WallSeconds
			}
			if got.PeakRSSBytes > peakRSS {
				peakRSS = got.PeakRSSBytes
			}
		}
	}

	assembleStarted := time.Now()
	assembledImg := assembleTiles(width, height, grid, tileImgs)
	assembledPNG, err := encodePNG(assembledImg)
	if err != nil {
		t.Fatalf("encode assembled png: %v", err)
	}
	assembleWall := time.Since(assembleStarted).Seconds()
	assembledSHA := sha256Hex(assembledPNG)
	if err := os.WriteFile(filepath.Join(workdir, "assembled.png"), assembledPNG, 0o644); err != nil {
		t.Fatalf("write assembled png: %v", err)
	}

	identity := compareRenderIdentity(fullBytes, fullSHA, assembledPNG, assembledSHA, fullImg, assembledImg)
	finishedAt := time.Now().UTC()

	report.FinishedAt = finishedAt.Format(time.RFC3339)
	report.WallClockSeconds = finishedAt.Sub(startedAt).Seconds()
	report.Cells = []blenderRenderCell{{
		Classification:     "MEASURED",
		Valid:              true,
		Scene:              "procedural_suzanne_plane_area",
		Engine:             "CYCLES",
		Device:             "CPU",
		Width:              width,
		Height:             height,
		Samples:            samples,
		Seed:               defaultBlenderSeed,
		TileGrid:           []int{grid, grid},
		TileCount:          grid * grid,
		OverscanPx:         0,
		Build:              invocationToJSON(build, blendPath),
		FullFrame:          invocationToJSON(full, fullPath),
		Tiles:              tiles,
		TileWallSecondsSum: tileWallSum,
		TileWallSecondsMax: tileWallMax,
		Assembly: blenderRenderAssembly{
			WallSeconds: assembleWall,
			OutputBytes: int64(len(assembledPNG)),
			SHA256:      assembledSHA,
		},
		Identity:     identity,
		PeakRSSBytes: peakRSS,
	}}
	report.Dropped = []blenderRenderDropped{}
	report.Surprises = blenderRenderSurprises(identity, full, tiles, assembleWall)

	if err := writeBlenderRenderEvidence(report); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("full wall=%.3fs rss=%d bytes=%d sha=%s", full.WallSeconds, full.PeakRSSBytes, len(fullBytes), fullSHA)
	t.Logf("tiles n=%d wall_sum=%.3fs wall_max=%.3fs", len(tiles), tileWallSum, tileWallMax)
	t.Logf("assemble wall=%.6fs bytes=%d sha=%s", assembleWall, len(assembledPNG), assembledSHA)
	t.Logf("identity file=%v pixels=%v differing=%d max_delta=%d why=%s",
		identity.FileBytesIdentical, identity.PixelsIdentical, identity.PixelsDiffering, identity.MaxChannelDelta, identity.Why)
}

func TestBlenderRenderBaselineScriptPinsCyclesCPU(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", blenderRenderScriptRel))
	if err != nil {
		t.Fatalf("read bpy script: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, `scene.render.engine = "CYCLES"`) {
		t.Fatal("bpy script must pin scene.render.engine to CYCLES")
	}
	if !strings.Contains(src, `scene.cycles.device = "CPU"`) {
		t.Fatal("bpy script must pin scene.cycles.device to CPU")
	}
	if !strings.Contains(src, "def require_cycles_cpu") {
		t.Fatal("bpy script must define require_cycles_cpu")
	}
	if strings.Contains(src, `= "BLENDER_EEVEE"`) || strings.Contains(src, `= "BLENDER_EEVEE_NEXT"`) {
		t.Fatal("bpy script must never assign an EEVEE engine")
	}
}

func TestBlenderRenderBaselineListedAsOptInSkip(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "ops", "scripts", "allowed-test-skips.txt"))
	if err != nil {
		t.Fatalf("read allowed-test-skips: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "TestBlenderRenderBaseline" {
			if !bytes.Contains(raw, []byte("MERC_BLENDER_RENDER_BENCH")) {
				t.Fatal("allowed-test-skips.txt must name MERC_BLENDER_RENDER_BENCH next to TestBlenderRenderBaseline")
			}
			return
		}
	}
	t.Fatal("TestBlenderRenderBaseline must be listed in ops/scripts/allowed-test-skips.txt so make ci does not treat the env gate as an unmarked skip")
}

type blenderProcResult struct {
	WallSeconds        float64
	BlenderTimeSeconds *float64
	PeakRSSBytes       int64
	ExitCode           int
	Stdout             string
	Stderr             string
	Marker             map[string]any
}

func runBlenderMeasured(t *testing.T, blender, script, workdir string, scriptArgs []string) blenderProcResult {
	t.Helper()
	args := []string{
		"-l",
		blender,
		"-b",
		"-noaudio",
		"--factory-startup",
		"--python",
		script,
		"--",
	}
	args = append(args, scriptArgs...)
	cmd := exec.Command("/usr/bin/time", args...)
	cmd.Dir = workdir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	started := time.Now()
	runErr := cmd.Run()
	wall := time.Since(started).Seconds()
	exitCode := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	outStr := stdout.String()
	errStr := stderr.String()
	if crash := crashPathFromOutput(outStr + "\n" + errStr); crash != "" {
		if dump, readErr := os.ReadFile(crash); readErr == nil {
			const capBytes = 4000
			if len(dump) > capBytes {
				dump = dump[:capBytes]
			}
			errStr += "\n--- blender.crash.txt ---\n" + string(dump)
		}
	}
	return blenderProcResult{
		WallSeconds:        wall,
		BlenderTimeSeconds: parseBlenderTimeSeconds(outStr + "\n" + errStr),
		PeakRSSBytes:       parseTimeLRSS(errStr),
		ExitCode:           exitCode,
		Stdout:             outStr,
		Stderr:             errStr,
		Marker:             parseBlenderMarker(outStr),
	}
}

func blenderVersionString(bin string) (string, error) {
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return strings.TrimSpace(string(out)), nil
	}
	first := strings.TrimSpace(lines[0])
	hash := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "build hash:") {
			hash = strings.TrimSpace(strings.TrimPrefix(line, "build hash:"))
			break
		}
	}
	if hash != "" {
		return first + " hash " + hash, nil
	}
	return first, nil
}

func blenderRenderSourceCommit() string {
	if v := strings.TrimSpace(os.Getenv("MERC_SOURCE_COMMIT")); v != "" {
		return v
	}
	headOut, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return mercSourceCommitSHA()
	}
	head := strings.TrimSpace(string(headOut))
	// Avoid `git status --porcelain`: the LFS clean filter can fail in a
	// sandbox and hide dirtiness. Name-only queries do not clean payloads.
	mod, _ := exec.Command("git", "diff", "--name-only", "HEAD").Output()
	untracked, _ := exec.Command("git", "ls-files", "--others", "--exclude-standard").Output()
	if strings.TrimSpace(string(mod)) != "" || strings.TrimSpace(string(untracked)) != "" {
		return head + "-dirty"
	}
	return head
}

var (
	timeLRSSRe      = regexp.MustCompile(`(?m)^\s*(\d+)\s+maximum resident set size\s*$`)
	blenderTimeRe   = regexp.MustCompile(`Time:\s*(\d+):(\d+\.\d+)`)
	blenderCrashRe  = regexp.MustCompile(`Writing:\s*(\S*blender\.crash\.txt)`)
	blenderMarkerRe = regexp.MustCompile(`(?m)^MERC_BLENDER_RENDER\s+(\{.*\})\s*$`)
)

func parseTimeLRSS(stderr string) int64 {
	matches := timeLRSSRe.FindAllStringSubmatch(stderr, -1)
	if len(matches) == 0 {
		return 0
	}
	n, err := strconv.ParseInt(matches[len(matches)-1][1], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func parseBlenderTimeSeconds(text string) *float64 {
	m := blenderTimeRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	min, err1 := strconv.Atoi(m[1])
	sec, err2 := strconv.ParseFloat(m[2], 64)
	if err1 != nil || err2 != nil {
		return nil
	}
	v := float64(min)*60 + sec
	return &v
}

func crashPathFromOutput(text string) string {
	m := blenderCrashRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return m[1]
}

func parseBlenderMarker(stdout string) map[string]any {
	m := blenderMarkerRe.FindStringSubmatch(stdout)
	if m == nil {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(m[1]), &payload); err != nil {
		return nil
	}
	return payload
}

func readFileSHA(path string) ([]byte, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	return raw, sha256Hex(raw), nil
}

func decodePNG(raw []byte) (image.Image, error) {
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return img, nil
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func assembleTiles(width, height, grid int, tiles []image.Image) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	tileW := width / grid
	tileH := height / grid
	i := 0
	for ty := 0; ty < grid; ty++ {
		for tx := 0; tx < grid; tx++ {
			src := tiles[i]
			i++
			r := image.Rect(tx*tileW, ty*tileH, (tx+1)*tileW, (ty+1)*tileH)
			draw.Draw(dst, r, src, src.Bounds().Min, draw.Src)
		}
	}
	return dst
}

func compareRenderIdentity(fullPNG []byte, fullSHA string, assembledPNG []byte, assembledSHA string, fullImg, assembledImg image.Image) blenderRenderIdentity {
	out := blenderRenderIdentity{
		FileBytesIdentical:   bytes.Equal(fullPNG, assembledPNG),
		FileSHA256Full:       fullSHA,
		FileSHA256Assembled:  assembledSHA,
		PixelsCompared:       fullImg.Bounds().Dx() * fullImg.Bounds().Dy(),
		PixelSHA256Full:      pixelSHA(fullImg),
		PixelSHA256Assembled: pixelSHA(assembledImg),
	}
	out.PixelsIdentical = out.PixelSHA256Full == out.PixelSHA256Assembled
	if !out.PixelsIdentical {
		dx, dy, nDiff, maxDelta := firstPixelDiff(fullImg, assembledImg)
		out.PixelsDiffering = nDiff
		out.MaxChannelDelta = maxDelta
		if nDiff > 0 {
			out.FirstDiffXY = []int{dx, dy}
		}
	}
	switch {
	case out.FileBytesIdentical && out.PixelsIdentical:
		out.Why = "assembled PNG bytes and raw RGB pixels both match the single-frame render"
	case !out.FileBytesIdentical && out.PixelsIdentical:
		out.Why = "raw RGB pixels match; PNG file bytes differ because Blender and the Go assembler use different PNG filters/compression"
	default:
		out.Why = "raw RGB pixels differ from the single-frame render; independent border-cropped tiles do not reproduce the full-frame filter footprint (no overscan)"
		if out.PixelsDiffering > 0 {
			out.Why += fmt.Sprintf("; first diff at (%d,%d), max channel delta %d, %d/%d pixels differ",
				out.FirstDiffXY[0], out.FirstDiffXY[1], out.MaxChannelDelta, out.PixelsDiffering, out.PixelsCompared)
		}
	}
	return out
}

func pixelSHA(img image.Image) string {
	b := img.Bounds()
	buf := make([]byte, 0, b.Dx()*b.Dy()*3)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			buf = append(buf, uint8(r>>8), uint8(g>>8), uint8(bl>>8))
		}
	}
	return sha256Hex(buf)
}

func firstPixelDiff(a, b image.Image) (dx, dy, nDiff, maxDelta int) {
	ab, bb := a.Bounds(), b.Bounds()
	dx, dy = -1, -1
	for y := ab.Min.Y; y < ab.Max.Y && y < bb.Max.Y; y++ {
		for x := ab.Min.X; x < ab.Max.X && x < bb.Max.X; x++ {
			ar, ag, abl, _ := a.At(x, y).RGBA()
			br, bg, bbl, _ := b.At(x, y).RGBA()
			d := absDelta(ar, br)
			if g := absDelta(ag, bg); g > d {
				d = g
			}
			if bl := absDelta(abl, bbl); bl > d {
				d = bl
			}
			// RGBA() is 16-bit; compare on the 8-bit channel the PNG stores.
			d8 := int(d >> 8)
			if d8 == 0 {
				continue
			}
			nDiff++
			if d8 > maxDelta {
				maxDelta = d8
			}
			if dx < 0 {
				dx, dy = x, y
			}
		}
	}
	return dx, dy, nDiff, maxDelta
}

func absDelta(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

func invocationToJSON(p blenderProcResult, path string) blenderRenderInvocationStats {
	st, _ := os.Stat(path)
	bytes := int64(0)
	sum := ""
	if st != nil {
		bytes = st.Size()
		if raw, err := os.ReadFile(path); err == nil {
			sum = sha256Hex(raw)
		}
	}
	return blenderRenderInvocationStats{
		WallSeconds:        p.WallSeconds,
		BlenderTimeSeconds: p.BlenderTimeSeconds,
		PeakRSSBytes:       p.PeakRSSBytes,
		OutputBytes:        bytes,
		SHA256:             sum,
		ExitCode:           p.ExitCode,
		Marker:             p.Marker,
	}
}

func blenderRenderSurprises(id blenderRenderIdentity, full blenderProcResult, tiles []blenderRenderTile, assembleWall float64) []string {
	out := []string{}
	if !id.FileBytesIdentical {
		out = append(out, "assembled PNG file bytes are not sha256-identical to the single-frame PNG")
	}
	if !id.PixelsIdentical {
		out = append(out, fmt.Sprintf("assembled pixels are not identical (%d differing, max channel delta %d)", id.PixelsDiffering, id.MaxChannelDelta))
	} else if id.FileBytesIdentical {
		out = append(out, "tile-assembled output is bit-identical to the single-frame render (file and pixels)")
	} else {
		out = append(out, "tile-assembled pixels are identical; only PNG encoding differs")
	}
	if full.BlenderTimeSeconds != nil && full.WallSeconds > 0 {
		startup := full.WallSeconds - *full.BlenderTimeSeconds
		if startup > 0.2 {
			out = append(out, fmt.Sprintf("full-frame wall includes ~%.3fs beyond Blender's reported Time (startup/shutdown)", startup))
		}
	}
	if assembleWall*100 < full.WallSeconds {
		out = append(out, "CPU-side tile assembly is negligible next to Blender wall time")
	}
	if len(tiles) > 0 && tiles[0].PeakRSSBytes > 0 && full.PeakRSSBytes > 0 {
		ratio := float64(tiles[0].PeakRSSBytes) / float64(full.PeakRSSBytes)
		out = append(out, fmt.Sprintf("first-tile peak RSS / full-frame peak RSS = %.3f (independent processes, not a shared address space)", ratio))
	}
	return out
}

func writeBlenderRenderEvidence(report blenderRenderReport) error {
	rel := blenderRenderBenchEvidenceRel
	if v := strings.TrimSpace(os.Getenv("MERC_BLENDER_RENDER_BENCH_EVIDENCE")); v != "" {
		rel = v
	}
	path := filepath.Join("..", "..", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}

type blenderRenderReport struct {
	Classification   string                  `json:"classification"`
	GeneratedAt      string                  `json:"generated_at"`
	FinishedAt       string                  `json:"finished_at"`
	WallClockSeconds float64                 `json:"wall_clock_seconds"`
	SourceCommit     string                  `json:"source_commit"`
	Host             string                  `json:"host"`
	NumCPU           int                     `json:"num_cpu"`
	GOMAXPROCS       int                     `json:"gomaxprocs"`
	GOOS             string                  `json:"goos"`
	GOARCH           string                  `json:"goarch"`
	Invocation       blenderRenderInvocation `json:"invocation"`
	Honesty          blenderRenderHonesty    `json:"honesty"`
	Cells            []blenderRenderCell     `json:"cells"`
	Dropped          []blenderRenderDropped  `json:"dropped_cells"`
	Surprises        []string                `json:"surprises"`
}

type blenderRenderInvocation struct {
	EnvGate                string `json:"env_gate"`
	ExcludedFromNormalGate bool   `json:"excluded_from_normal_gate"`
	ExclusionProof         string `json:"exclusion_proof"`
	Command                string `json:"command"`
	BlenderBin             string `json:"blender_bin"`
	BlenderVersion         string `json:"blender_version"`
	Script                 string `json:"script"`
	Engine                 string `json:"engine"`
	Device                 string `json:"device"`
	Width                  int    `json:"width"`
	Height                 int    `json:"height"`
	Samples                int    `json:"samples"`
	Seed                   int    `json:"seed"`
	TileGrid               []int  `json:"tile_grid"`
	TileCount              int    `json:"tile_count"`
	OverscanPx             int    `json:"overscan_px"`
	Scene                  string `json:"scene"`
	AssetsDownloaded       bool   `json:"assets_downloaded"`
	EEVEEInvoked           bool   `json:"eevee_invoked"`
}

type blenderRenderHonesty struct {
	WhatThisProves       string   `json:"what_this_proves"`
	WhatThisDoesNotProve string   `json:"what_this_does_not_prove"`
	Guards               []string `json:"guards"`
}

type blenderRenderCell struct {
	Classification     string                       `json:"classification"`
	Valid              bool                         `json:"valid"`
	Scene              string                       `json:"scene"`
	Engine             string                       `json:"engine"`
	Device             string                       `json:"device"`
	Width              int                          `json:"width"`
	Height             int                          `json:"height"`
	Samples            int                          `json:"samples"`
	Seed               int                          `json:"seed"`
	TileGrid           []int                        `json:"tile_grid"`
	TileCount          int                          `json:"tile_count"`
	OverscanPx         int                          `json:"overscan_px"`
	Build              blenderRenderInvocationStats `json:"build"`
	FullFrame          blenderRenderInvocationStats `json:"full_frame"`
	Tiles              []blenderRenderTile          `json:"tiles"`
	TileWallSecondsSum float64                      `json:"tile_wall_seconds_sum"`
	TileWallSecondsMax float64                      `json:"tile_wall_seconds_max"`
	Assembly           blenderRenderAssembly        `json:"assembly"`
	Identity           blenderRenderIdentity        `json:"identity"`
	PeakRSSBytes       int64                        `json:"peak_rss_bytes"`
}

type blenderRenderInvocationStats struct {
	WallSeconds        float64        `json:"wall_seconds"`
	BlenderTimeSeconds *float64       `json:"blender_time_seconds"`
	PeakRSSBytes       int64          `json:"peak_rss_bytes"`
	OutputBytes        int64          `json:"output_bytes"`
	SHA256             string         `json:"sha256"`
	ExitCode           int            `json:"exit_code"`
	Marker             map[string]any `json:"marker"`
}

type blenderRenderTile struct {
	ID                 string   `json:"id"`
	TileX              int      `json:"tile_x"`
	TileY              int      `json:"tile_y"`
	Width              int      `json:"width"`
	Height             int      `json:"height"`
	WallSeconds        float64  `json:"wall_seconds"`
	BlenderTimeSeconds *float64 `json:"blender_time_seconds"`
	PeakRSSBytes       int64    `json:"peak_rss_bytes"`
	OutputBytes        int64    `json:"output_bytes"`
	SHA256             string   `json:"sha256"`
	ExitCode           int      `json:"exit_code"`
}

type blenderRenderAssembly struct {
	WallSeconds float64 `json:"wall_seconds"`
	OutputBytes int64   `json:"output_bytes"`
	SHA256      string  `json:"sha256"`
}

type blenderRenderIdentity struct {
	FileBytesIdentical   bool   `json:"file_bytes_identical"`
	FileSHA256Full       string `json:"file_sha256_full"`
	FileSHA256Assembled  string `json:"file_sha256_assembled"`
	PixelsIdentical      bool   `json:"pixels_identical"`
	PixelsCompared       int    `json:"pixels_compared"`
	PixelsDiffering      int    `json:"pixels_differing"`
	MaxChannelDelta      int    `json:"max_channel_delta"`
	FirstDiffXY          []int  `json:"first_diff_xy,omitempty"`
	PixelSHA256Full      string `json:"pixel_sha256_full"`
	PixelSHA256Assembled string `json:"pixel_sha256_assembled"`
	Why                  string `json:"why"`
}

type blenderRenderDropped struct {
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}
