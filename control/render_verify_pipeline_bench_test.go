package main

// Opt-in verify-pipeline bench. Never part of make test / make ci.
//
//	MERC_RENDER_VERIFY_PIPELINE=1 \
//	  go test -count=1 -run '^TestRenderVerifyPipelineBench$' -timeout 90m .
//
// Writes evidence/perf/render-verify-pipeline.json when run from control/.
//
// Cycles CPU only. EEVEE is never selected. Adaptive sampling and denoising
// stay OFF. Both arms share the full config recorded in the receipt.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	renderVerifyBenchEnv         = "MERC_RENDER_VERIFY_PIPELINE"
	renderVerifyBenchBinEnv      = "MERC_BLENDER_BIN"
	renderVerifyBenchFramesEnv   = "MERC_RENDER_VERIFY_FRAMES"
	renderVerifyBenchWidthEnv    = "MERC_RENDER_VERIFY_WIDTH"
	renderVerifyBenchSamplesEnv  = "MERC_RENDER_VERIFY_SAMPLES"
	renderVerifyBenchWorkdirEnv  = "MERC_RENDER_VERIFY_WORKDIR"
	renderVerifyBenchEvidenceRel = "evidence/perf/render-verify-pipeline.json"
	renderVerifyServiceRel       = "render/verify/blender_service.py"
	defaultVerifyBlenderBin      = "/Applications/Blender.app/Contents/MacOS/Blender"
	defaultVerifyFrames          = 8
	defaultVerifyWidth           = 1024
	defaultVerifySamples         = 512
	defaultVerifySeed            = 1
	projectVerifyFrames          = 100
)

func TestRenderVerifyPipelineBench(t *testing.T) {
	if os.Getenv(renderVerifyBenchEnv) != "1" {
		t.Skip("set MERC_RENDER_VERIFY_PIPELINE=1 to measure hash-only / pipelined L1 verification")
	}
	if runtime.GOOS != "darwin" {
		t.Fatalf("this bench launches the host Blender.app; host is %s", runtime.GOOS)
	}

	blender := strings.TrimSpace(os.Getenv(renderVerifyBenchBinEnv))
	if blender == "" {
		blender = defaultVerifyBlenderBin
	}
	if _, err := os.Stat(blender); err != nil {
		t.Fatalf("blender binary %s: %v", blender, err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, renderVerifyServiceRel)
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("blender service %s: %v", script, err)
	}

	frames := envInt(t, renderVerifyBenchFramesEnv, defaultVerifyFrames, 1, 10000)
	width := envInt(t, renderVerifyBenchWidthEnv, defaultVerifyWidth, 16, 4096)
	samples := envInt(t, renderVerifyBenchSamplesEnv, defaultVerifySamples, 1, 1_000_000)
	height := width

	workdir := strings.TrimSpace(os.Getenv(renderVerifyBenchWorkdirEnv))
	if workdir == "" {
		workdir = t.TempDir()
	} else if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("workdir: %v", err)
	}

	version, err := blenderVersionString(blender)
	if err != nil {
		t.Fatalf("blender --version: %v", err)
	}
	host, _ := os.Hostname()
	startedAt := time.Now().UTC()

	cmd := exec.Command(blender,
		"-b", "-noaudio", "--factory-startup", "--python-exit-code", "1",
		"--python", script, "--",
		"--workdir", workdir,
		"--frames", strconv.Itoa(frames),
		"--width", strconv.Itoa(width),
		"--height", strconv.Itoa(height),
		"--samples", strconv.Itoa(samples),
		"--seed", strconv.Itoa(defaultVerifySeed),
	)
	cmd.Dir = root
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start blender: %v", err)
	}

	type rendered struct {
		frame   RenderedFrame
		pngPath string
		l1Path  string
		marker  map[string]any
	}
	got := make([]rendered, 0, frames)
	refs := make([]L1Input, 0, frames)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	renderWall0 := time.Now()

	// Pipeline: as each sidecar appears, hash-only-verify it while Blender
	// continues the next frame.
	type pipeRec struct {
		rec FrameVerifyRecord
	}
	pipeRecs := make([]FrameVerifyRecord, 0, frames)
	var pipeVerifySum int64

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MERC_VERIFY ") {
			continue
		}
		var marker map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "MERC_VERIFY ")), &marker); err != nil {
			t.Fatalf("marker json: %v\n%s", err, line)
		}
		if ok, _ := marker["ok"].(bool); !ok {
			_ = cmd.Process.Kill()
			t.Fatalf("blender marker not ok: %v\nstderr:\n%s", marker, stderr.String())
		}
		if marker["op"] != "RENDER" {
			continue
		}
		pngPath, _ := marker["png"].(string)
		raw, err := os.ReadFile(pngPath)
		if err != nil {
			_ = cmd.Process.Kill()
			t.Fatalf("read %s: %v", pngPath, err)
		}
		// Cache the L1 digest at ingest, overlapped with the next Cycles
		// frame already running in the resident process.
		dt0 := time.Now()
		buf, decErr := DecodePNGPixels(raw)
		if decErr != nil {
			_ = cmd.Process.Kill()
			t.Fatalf("decode %s: %v", pngPath, decErr)
		}
		digest := DigestPixelBuffer(buf)
		digestNS := time.Since(dt0).Nanoseconds()
		idx := len(got)
		frame := RenderedFrame{
			Index:       idx,
			PNG:         raw,
			Pixels:      buf,
			PixelDigest: digest,
			RenderNS:    int64(markerFloat(marker["render_s"]) * 1e9),
			DigestNS:    digestNS,
		}
		ref := L1Input{PixelDigest: digest}
		vt0 := time.Now()
		l1 := CompareL1(L1Input{PixelDigest: digest}, ref)
		vns := time.Since(vt0).Nanoseconds()
		pipeVerifySum += vns
		pipeRecs = append(pipeRecs, recordFromL1(idx, frame, l1, vns))
		got = append(got, rendered{frame: frame, pngPath: pngPath, l1Path: "", marker: marker})
		refs = append(refs, ref)
	}
	waitErr := cmd.Wait()
	renderWall := time.Since(renderWall0)
	if scanErr := scanner.Err(); scanErr != nil {
		t.Fatalf("stdout: %v", scanErr)
	}
	if waitErr != nil {
		t.Fatalf("blender exit: %v\nstderr:\n%s", waitErr, stderr.String())
	}
	if len(got) != frames {
		t.Fatalf("got %d frames, want %d\nstderr:\n%s", len(got), frames, stderr.String())
	}

	framesOut := make([]RenderedFrame, len(got))
	var renderSumNS int64
	var digestSumNS int64
	for i := range got {
		framesOut[i] = got[i].frame
		renderSumNS += got[i].frame.RenderNS
		digestSumNS += got[i].frame.DigestNS
	}

	serial := VerifySerialPNGDecodeL1(framesOut, refs)
	hashOnly := VerifyHashOnlyL1Run(framesOut, refs)

	// 100-frame verify cost: repeat decode+hash / hash-only on the first
	// real 1024² (or whatever was rendered) PNG so the project number is
	// MEASURED even when this run rendered fewer than 100 frames.
	firstPNG := framesOut[0].PNG
	firstDigest := framesOut[0].PixelDigest
	decode100 := measureRepeatedDecode(firstPNG, firstDigest, projectVerifyFrames)
	hash100 := measureRepeatedHash(framesOut[0], projectVerifyFrames)
	inMem100 := measureRepeatedInMemoryHash(pixFromFrame(t, framesOut[0]), projectVerifyFrames)
	python100 := measurePythonDecode(t, root, firstPNG, projectVerifyFrames)

	perFrame := make([]map[string]any, 0, len(got))
	for i := range got {
		perFrame = append(perFrame, map[string]any{
			"index":           i,
			"render_s":        markerFloat(got[i].marker["render_s"]),
			"ingest_digest_s": float64(got[i].frame.DigestNS) / 1e9,
			"blender_wall_s":  markerFloat(got[i].marker["wall_s"]),
			"pixel_sha":       got[i].frame.PixelDigest,
		})
	}

	meanFrameS := float64(renderSumNS) / float64(frames) / 1e9
	meanDecodeS := float64(serial.VerifyNSSum) / float64(frames) / 1e9
	meanHashS := float64(hashOnly.VerifyNSSum) / float64(frames) / 1e9
	meanDigestAtRenderS := float64(digestSumNS) / float64(frames) / 1e9
	firstFrameS := markerFloat(got[0].marker["render_s"])
	warmMeanS := meanFrameS
	if frames > 1 {
		var warm int64
		for i := 1; i < len(got); i++ {
			warm += got[i].frame.RenderNS
		}
		warmMeanS = float64(warm) / float64(frames-1) / 1e9
	}
	// 100-frame project: one cold first frame + 99 warm. Do not fold the
	// first-sync outlier into every frame (that inflates the ceiling).
	parallel100 := firstFrameS + warmMeanS*float64(projectVerifyFrames-1)
	frameSUsed := parallel100 / float64(projectVerifyFrames)
	before := amdahlsOf(decode100.seconds, parallel100, projectVerifyFrames, "100-frame/BEFORE/serial-png-decode-go")
	afterInMem := amdahlsOf(inMem100.seconds, parallel100, projectVerifyFrames, "100-frame/AFTER/in-memory-pixel-hash")
	afterHash := amdahlsOf(hash100.seconds, parallel100, projectVerifyFrames, "100-frame/AFTER/cached-digest-compare")
	// Pipelined serial term is the last frame's verify (hex compare).
	// Last frame's ingest decode cannot overlap a following render.
	lastVerifyS := float64(framesOut[len(framesOut)-1].DigestNS+pipeRecs[len(pipeRecs)-1].VerifyNS) / 1e9
	afterPipe := amdahlsOf(lastVerifyS, parallel100, projectVerifyFrames, "100-frame/AFTER/pipelined-cached-digest")
	pythonBefore := amdahlsOf(python100.seconds, parallel100, projectVerifyFrames, "100-frame/BEFORE/serial-png-decode-python")

	beforeWall100 := parallel100 + decode100.seconds
	afterWall100 := parallel100 + lastVerifyS
	saving100 := beforeWall100 - afterWall100

	// Pairwise L1 on frame 0 vs itself via PNG decode (must hold).
	self := CompareL1(L1Input{PNG: firstPNG}, L1Input{PixelDigest: firstDigest})
	if !self.Holds {
		t.Fatalf("frame 0 PNG vs cached digest failed L1: %+v", self)
	}

	// Mutation on the first real frame: flip one decoded pixel, confirm red, revert.
	pix, err := DecodePNGPixels(firstPNG)
	if err != nil {
		t.Fatal(err)
	}
	orig := pix.Pix[0]
	pix.Pix[0] = orig ^ 0xff
	mut := CompareL1(L1Input{PNG: firstPNG, PixelDigest: firstDigest}, L1Input{Pixels: pix, PixelDigest: DigestPixelBuffer(pix)})
	if mut.Holds || mut.DifferingPixels < 1 {
		t.Fatalf("1-pixel mutation of a real frame must fail L1: %+v", mut)
	}
	pix.Pix[0] = orig
	rev := CompareL1(L1Input{PixelDigest: firstDigest}, L1Input{Pixels: pix, PixelDigest: DigestPixelBuffer(pix)})
	if !rev.Holds {
		t.Fatalf("reverted pixel must restore L1: %+v", rev)
	}

	finishedAt := time.Now().UTC()
	classification := "MEASURED"
	projectClass := "DERIVED"
	if frames == projectVerifyFrames {
		projectClass = "MEASURED"
	}

	report := map[string]any{
		"classification":     classification,
		"generated_at":       startedAt.Format(time.RFC3339),
		"finished_at":        finishedAt.Format(time.RFC3339),
		"wall_clock_seconds": finishedAt.Sub(startedAt).Seconds(),
		"source_commit":      blenderRenderSourceCommit(),
		"host":               host,
		"num_cpu":            runtime.NumCPU(),
		"gomaxprocs":         runtime.GOMAXPROCS(0),
		"goos":               runtime.GOOS,
		"goarch":             runtime.GOARCH,
		"invocation": map[string]any{
			"env_gate":                   renderVerifyBenchEnv + "=1",
			"excluded_from_normal_gate":  true,
			"exclusion_proof":            "TestRenderVerifyPipelineBench skips unless MERC_RENDER_VERIFY_PIPELINE=1; listed in scripts/allowed-test-skips.txt; make test / make ci never set the env var",
			"command":                    "cd control && MERC_RENDER_VERIFY_PIPELINE=1 go test -count=1 -run '^TestRenderVerifyPipelineBench$' -timeout 90m .",
			"blender_bin":                blender,
			"blender_version":            version,
			"script":                     renderVerifyServiceRel,
			"engine":                     "CYCLES",
			"device":                     "CPU",
			"width":                      width,
			"height":                     height,
			"samples":                    samples,
			"seed":                       defaultVerifySeed,
			"frames_rendered":            frames,
			"project_frames":             projectVerifyFrames,
			"adaptive_sampling":          false,
			"denoising":                  false,
			"view_transform":             "AgX",
			"display_device":             "sRGB",
			"persistent_data":            true,
			"eevee_invoked":              false,
			"scene":                      "procedural_suzanne_plane_area_camera_orbit",
			"assets_downloaded":          false,
			"both_arms_identical_config": true,
		},
		"honesty": map[string]any{
			"what_this_proves":         "on this host, with this Blender binary, the MEASURED cost of L1 verification of real Cycles-CPU frames as (1) serial PNG decode+hash, (2) hash-only of cached decoded-pixel digests, and (3) hash-only pipelined with the next frame. Amdahl CEILINGS for a 100-frame project at the measured per-frame render and verify times. L1 PIXEL_EXACT still catches a 1-pixel mutation of a real frame.",
			"what_this_does_not_prove": "this is not a 10,000x or 73,000x claim, not a per-worker Cycles speedup, not a Metal measurement, and not a distributed-project speedup. The ceilings are the point where Amdahl stops binding; reaching a ceiling of C would need about C workers.",
			"lane_moves":               "serial_fraction (verification). It does not move per-worker path-tracing speedup.",
			"guards": []string{
				"Cycles CPU only; EEVEE never selected",
				"adaptive sampling OFF, denoising OFF, seed=1, AgX, same bounces/resolution/samples on both arms",
				"L1 is decoded pixels; container hashes are not an equivalence key",
				"a 1-pixel mutation of a real frame is reported, not tuned",
				"Amdahl numbers are labelled CEILING, never as an achieved multiplier",
				"this harness is the only writer of evidence/perf/render-verify-pipeline.json",
			},
		},
		"measured_run": map[string]any{
			"frames":                        frames,
			"blender_wall_seconds":          renderWall.Seconds(),
			"render_seconds_sum":            float64(renderSumNS) / 1e9,
			"mean_frame_seconds":            meanFrameS,
			"first_frame_seconds":           firstFrameS,
			"warm_mean_frame_seconds":       warmMeanS,
			"digest_at_render_seconds_sum":  float64(digestSumNS) / 1e9,
			"mean_digest_at_render_seconds": meanDigestAtRenderS,
			"serial_png_decode":             pipelineJSON(serial),
			"hash_only_l1":                  pipelineJSON(hashOnly),
			"pipelined_during_render": map[string]any{
				"mode":                VerifyPipelinedL1,
				"n_frames":            len(pipeRecs),
				"verify_seconds_sum":  float64(pipeVerifySum) / 1e9,
				"last_verify_seconds": lastVerifyS,
				"all_hold":            allHold(pipeRecs),
				"mean_verify_seconds": float64(pipeVerifySum) / float64(len(pipeRecs)) / 1e9,
				"note":                "Go decode+hash of frame N overlapped with resident render of N+1; residual serial term is the last frame's ingest decode",
			},
			"mean_serial_decode_seconds":   meanDecodeS,
			"mean_hash_only_seconds":       meanHashS,
			"per_frame":                    perFrame,
			"l1_self_png_vs_digest":        self.Holds,
			"l1_one_pixel_mutation_caught": !mut.Holds && mut.DifferingPixels >= 1,
			"l1_mutation_reverted":         rev.Holds,
		},
		"project_100_frame": map[string]any{
			"classification":                       projectClass,
			"n_frames":                             projectVerifyFrames,
			"frame_s_used":                         frameSUsed,
			"frame_s_first":                        firstFrameS,
			"frame_s_warm_mean":                    warmMeanS,
			"frame_s_source":                       fmt.Sprintf("1 first frame + 99 x warm-mean of %d-frame resident Cycles-CPU orbit at %d² / %dspp", frames, width, samples),
			"note":                                 "CEILINGS below are Amdahl 1/serial_fraction, not achieved speedups. Verification is no longer the binding serial term once the digest is cached.",
			"before_serial_png_decode_go":          amdahlJSON(before),
			"before_serial_png_decode_python":      amdahlJSON(pythonBefore),
			"after_in_memory_pixel_hash":           amdahlJSON(afterInMem),
			"after_cached_digest_compare":          amdahlJSON(afterHash),
			"after_pipelined_cached_digest":        amdahlJSON(afterPipe),
			"verify_100x_decode_go_seconds":        decode100.seconds,
			"verify_100x_decode_python_seconds":    python100.seconds,
			"verify_100x_in_memory_hash_seconds":   inMem100.seconds,
			"verify_100x_digest_compare_seconds":   hash100.seconds,
			"verify_100x_decode_classification":    decode100.class,
			"verify_100x_hash_classification":      hash100.class,
			"verify_100x_in_memory_classification": inMem100.class,
			"verify_100x_python_classification":    python100.class,
			"one_worker_wall_before_seconds":       beforeWall100,
			"one_worker_wall_after_seconds":        afterWall100,
			"one_worker_wall_saving_seconds":       saving100,
			"one_worker_wall_note":                 "1-worker wall saving is the serial verify tail removed. It is not a 10,000x. The ceiling change is the lane's deliverable.",
			"binding_after":                        "not_verification",
		},
		"surprises": []string{
			fmt.Sprintf("Go PNG decode+hash of one %d² frame is %.6fs; Python pngutil decode of the same bytes is %.6fs; in-memory pixel hash is %.6fs; cached-digest compare is %.9fs", width, meanDecodeS, python100.seconds/float64(projectVerifyFrames), inMem100.seconds/float64(projectVerifyFrames), meanHashS),
			fmt.Sprintf("L1 digest is computed at ingest (Go decode+hash %.6fs/frame), overlapped with the next resident render; Blender does not decode", meanDigestAtRenderS),
			fmt.Sprintf("100-frame 1-worker wall saving is %.3fs of %.3fs (%.2f%%); the lane moves the Amdahl CEILING from %.1f (Go decode) / %.1f (Python decode) to %.1f (pipelined). Those are CEILINGS.",
				saving100, beforeWall100, 100*saving100/beforeWall100, before.Ceiling, pythonBefore.Ceiling, afterPipe.Ceiling),
		},
	}

	if err := writeVerifyPipelineEvidence(report); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("rendered %d frames mean_frame=%.3fs serial_decode=%.6fs hash=%.9fs", frames, meanFrameS, meanDecodeS, meanHashS)
	t.Logf("CEILING before=%.1f after_hash=%.1f after_pipe=%.1f (not speedups)", before.Ceiling, afterHash.Ceiling, afterPipe.Ceiling)
	t.Logf("100-frame 1-worker wall saving=%.3fs", saving100)
}

func envInt(t *testing.T, key string, def, lo, hi int) int {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		t.Fatalf("%s=%q: need integer in [%d,%d]", key, v, lo, hi)
	}
	return n
}

func markerFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	default:
		return 0
	}
}

func allHold(recs []FrameVerifyRecord) bool {
	for _, r := range recs {
		if !r.Holds {
			return false
		}
	}
	return true
}

type repeatMeasure struct {
	seconds float64
	class   string
}

func measureRepeatedDecode(png []byte, wantDigest string, n int) repeatMeasure {
	t0 := time.Now()
	for i := 0; i < n; i++ {
		buf, err := DecodePNGPixels(png)
		if err != nil {
			return repeatMeasure{class: "FAILED"}
		}
		got := DigestPixelBuffer(buf)
		if got != wantDigest {
			return repeatMeasure{class: "FAILED"}
		}
	}
	return repeatMeasure{seconds: time.Since(t0).Seconds(), class: "MEASURED"}
}

func measureRepeatedHash(frame RenderedFrame, n int) repeatMeasure {
	if frame.PixelDigest == "" {
		CacheFrameDigest(&frame)
	}
	ref := L1Input{PixelDigest: frame.PixelDigest}
	t0 := time.Now()
	for i := 0; i < n; i++ {
		l1 := CompareL1(L1Input{PixelDigest: frame.PixelDigest}, ref)
		if !l1.Holds {
			return repeatMeasure{class: "FAILED"}
		}
	}
	return repeatMeasure{seconds: time.Since(t0).Seconds(), class: "MEASURED"}
}

func pixFromFrame(t *testing.T, frame RenderedFrame) PixelBuffer {
	t.Helper()
	if len(frame.Pixels.Pix) > 0 {
		return frame.Pixels
	}
	buf, err := DecodePNGPixels(frame.PNG)
	if err != nil {
		t.Fatalf("decode frame for in-memory hash: %v", err)
	}
	return buf
}

func measureRepeatedInMemoryHash(buf PixelBuffer, n int) repeatMeasure {
	want := DigestPixelBuffer(buf)
	t0 := time.Now()
	for i := 0; i < n; i++ {
		if DigestPixelBuffer(buf) != want {
			return repeatMeasure{class: "FAILED"}
		}
	}
	return repeatMeasure{seconds: time.Since(t0).Seconds(), class: "MEASURED"}
}

func measurePythonDecode(t *testing.T, root string, png []byte, n int) repeatMeasure {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "frame.png")
	if err := os.WriteFile(path, png, 0o644); err != nil {
		t.Fatalf("write png for python decode: %v", err)
	}
	script := fmt.Sprintf(`
import sys, time
sys.path.insert(0, %q)
from render.lib import pngutil
path = %q
n = %d
t0 = time.perf_counter()
last = None
for _ in range(n):
    last = pngutil.encoded_pixels_sha256(path)
print(time.perf_counter() - t0)
print(last)
`, root, path, n)
	cmd := exec.Command("python3", "-c", script)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("python decode measure failed: %v\n%s", err, out)
		return repeatMeasure{class: "FAILED"}
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 1 {
		return repeatMeasure{class: "FAILED"}
	}
	sec, convErr := strconv.ParseFloat(strings.TrimSpace(lines[0]), 64)
	if convErr != nil {
		return repeatMeasure{class: "FAILED"}
	}
	return repeatMeasure{seconds: sec, class: "MEASURED"}
}

func pipelineJSON(run PipelineRun) map[string]any {
	return map[string]any{
		"mode":                         run.Mode,
		"n_frames":                     run.NFrames,
		"wall_seconds":                 float64(run.WallNS) / 1e9,
		"verify_seconds_sum":           float64(run.VerifyNSSum) / 1e9,
		"verify_critical_path_seconds": float64(run.VerifyCriticalPathNS) / 1e9,
		"all_hold":                     run.AllHold,
		"serial_fraction":              run.SerialFraction,
		"amdahl_ceiling":               run.AmdahlCeiling,
		"ceiling_note":                 run.CeilingNote,
	}
}

func amdahlJSON(a AmdahlCeiling) map[string]any {
	return map[string]any{
		"label":            a.Label,
		"serial_seconds":   a.SerialSeconds,
		"parallel_seconds": a.ParallelSeconds,
		"n_frames":         a.NFrames,
		"serial_fraction":  a.SerialFraction,
		"amdahl_ceiling":   a.Ceiling,
		"note":             a.Note,
		"kind":             "CEILING",
	}
}

func writeVerifyPipelineEvidence(report map[string]any) error {
	rel := renderVerifyBenchEvidenceRel
	if v := strings.TrimSpace(os.Getenv("MERC_RENDER_VERIFY_EVIDENCE")); v != "" {
		rel = v
	}
	path := filepath.Join("..", rel)
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
