package main

// Render verification pipeline (plan §3 L1 + Amdahl serial term).
//
// Hash-only L1 compares SHA-256 of decoded 8-bit RGB. That is PIXEL_EXACT,
// not a weaker contract: a one-pixel change moves the digest. The pixel-array
// walk (max-abs / mean / differing-pixel-count) runs only on divergence.
//
// Pipelined mode overlaps verify(frame N) with render(frame N+1). That is
// the serial-term attack. Numbers produced here are CEILINGS
// (1/serial_fraction), never achieved speedups.

import (
	"fmt"
	"sync"
	"time"
)

const (
	VerifySerialPNGDecode = "serial_png_decode"
	VerifyHashOnlyL1      = "hash_only_l1"
	VerifyPipelinedL1     = "pipelined_hash_only_l1"

	amdahlCeilingNote = "CEILING — Amdahl 1/serial_fraction; not an achieved speedup. Reaching this ceiling would need about that many workers."
)

// RenderedFrame is one worker output. PixelDigest must be filled at render
// time from the in-memory decoded RGB when the renderer still holds it.
type RenderedFrame struct {
	Index       int
	PNG         []byte
	Pixels      PixelBuffer
	PixelDigest string
	RenderNS    int64
	DigestNS    int64
}

// FrameVerifyRecord is the per-frame L1 observation.
type FrameVerifyRecord struct {
	Index       int              `json:"index"`
	Holds       bool             `json:"holds"`
	Path        string           `json:"path"`
	Decoded     bool             `json:"decoded"`
	Differing   int              `json:"differing_pixels"`
	MaxAbs      int              `json:"max_abs"`
	MeanAbs     float64          `json:"mean_abs"`
	VerifyNS    int64            `json:"verify_ns"`
	PixelSHA    string           `json:"pixel_sha256"`
	RefSHA      string           `json:"ref_sha256"`
	RenderStart time.Time        `json:"-"`
	RenderEnd   time.Time        `json:"-"`
	VerifyStart time.Time        `json:"-"`
	VerifyEnd   time.Time        `json:"-"`
	L1          PixelExactResult `json:"-"`
}

// PipelineRun is one measured verify placement against the same renders.
type PipelineRun struct {
	Mode                 string              `json:"mode"`
	NFrames              int                 `json:"n_frames"`
	WallNS               int64               `json:"wall_ns"`
	RenderNSSum          int64               `json:"render_ns_sum"`
	VerifyNSSum          int64               `json:"verify_ns_sum"`
	VerifyCriticalPathNS int64               `json:"verify_critical_path_ns"`
	AllHold              bool                `json:"all_hold"`
	OverlappedFrames     int                 `json:"overlapped_frames"`
	SerialSeconds        float64             `json:"serial_seconds"`
	ParallelSeconds      float64             `json:"parallel_seconds"`
	SerialFraction       float64             `json:"serial_fraction"`
	AmdahlCeiling        float64             `json:"amdahl_ceiling"`
	CeilingNote          string              `json:"ceiling_note"`
	Frames               []FrameVerifyRecord `json:"frames"`
}

// AmdahlCeiling is 1/serial_fraction. It is a ceiling, not a multiplier.
type AmdahlCeiling struct {
	Label           string  `json:"label"`
	SerialSeconds   float64 `json:"serial_seconds"`
	ParallelSeconds float64 `json:"parallel_seconds"`
	NFrames         int     `json:"n_frames"`
	SerialFraction  float64 `json:"serial_fraction"`
	Ceiling         float64 `json:"amdahl_ceiling"`
	Note            string  `json:"note"`
}

func amdahlsOf(serial, parallel float64, nFrames int, label string) AmdahlCeiling {
	out := AmdahlCeiling{
		Label:           label,
		SerialSeconds:   serial,
		ParallelSeconds: parallel,
		NFrames:         nFrames,
		Note:            amdahlCeilingNote,
	}
	denom := serial + parallel
	if denom > 0 {
		out.SerialFraction = serial / denom
	}
	if out.SerialFraction > 0 {
		out.Ceiling = 1 / out.SerialFraction
	}
	return out
}

// CacheFrameDigest hashes in-memory decoded pixels onto the frame. Cost is
// the hash, not a PNG decode, when Pixels is already populated.
func CacheFrameDigest(frame *RenderedFrame) {
	if frame == nil || frame.PixelDigest != "" {
		return
	}
	if len(frame.Pixels.Pix) == 0 && len(frame.PNG) > 0 {
		if buf, err := DecodePNGPixels(frame.PNG); err == nil {
			frame.Pixels = buf
		}
	}
	if len(frame.Pixels.Pix) == 0 {
		return
	}
	t0 := time.Now()
	frame.PixelDigest = DigestPixelBuffer(frame.Pixels)
	frame.DigestNS = time.Since(t0).Nanoseconds()
}

func frameToL1(frame RenderedFrame) L1Input {
	return L1Input{PNG: frame.PNG, Pixels: frame.Pixels, PixelDigest: frame.PixelDigest}
}

func recordFromL1(index int, got RenderedFrame, l1 PixelExactResult, verifyNS int64) FrameVerifyRecord {
	return FrameVerifyRecord{
		Index:     index,
		Holds:     l1.Holds,
		Path:      l1.Path,
		Decoded:   l1.Decoded,
		Differing: l1.DifferingPixels,
		MaxAbs:    l1.MaxAbs,
		MeanAbs:   l1.MeanAbs,
		VerifyNS:  verifyNS,
		PixelSHA:  l1.PixelSHA256A,
		RefSHA:    l1.PixelSHA256B,
		L1:        l1,
	}
}

func finishPipelineRun(mode string, wallNS, renderSum int64, recs []FrameVerifyRecord) PipelineRun {
	run := PipelineRun{
		Mode:        mode,
		NFrames:     len(recs),
		WallNS:      wallNS,
		RenderNSSum: renderSum,
		AllHold:     true,
		CeilingNote: amdahlCeilingNote,
		Frames:      recs,
	}
	var verifySum int64
	for i := range recs {
		verifySum += recs[i].VerifyNS
		if !recs[i].Holds {
			run.AllHold = false
		}
		if i+1 < len(recs) && !recs[i].VerifyStart.IsZero() && !recs[i+1].RenderEnd.IsZero() {
			if recs[i].VerifyStart.Before(recs[i+1].RenderEnd) && recs[i].VerifyEnd.After(recs[i+1].RenderStart) {
				run.OverlappedFrames++
			}
		}
	}
	run.VerifyNSSum = verifySum
	switch mode {
	case VerifyPipelinedL1:
		residual := wallNS - renderSum
		if residual < 0 {
			residual = 0
		}
		run.VerifyCriticalPathNS = residual
	default:
		run.VerifyCriticalPathNS = verifySum
	}
	run.SerialSeconds = float64(run.VerifyCriticalPathNS) / 1e9
	run.ParallelSeconds = float64(renderSum) / 1e9
	a := amdahlsOf(run.SerialSeconds, run.ParallelSeconds, run.NFrames, mode)
	run.SerialFraction = a.SerialFraction
	run.AmdahlCeiling = a.Ceiling
	return run
}

// VerifySerialPNGDecodeL1 decodes each PNG and walks (or hashes) decoded
// pixels against the reference. This is the binding serial tail the locality
// lane measured.
func VerifySerialPNGDecodeL1(frames []RenderedFrame, refs []L1Input) PipelineRun {
	if len(refs) != len(frames) {
		return PipelineRun{Mode: VerifySerialPNGDecode, AllHold: false}
	}
	recs := make([]FrameVerifyRecord, len(frames))
	var renderSum int64
	t0 := time.Now()
	for i, frame := range frames {
		renderSum += frame.RenderNS
		in := L1Input{PNG: frame.PNG}
		vt0 := time.Now()
		l1 := CompareL1(in, refs[i])
		recs[i] = recordFromL1(frame.Index, frame, l1, time.Since(vt0).Nanoseconds())
	}
	return finishPipelineRun(VerifySerialPNGDecode, time.Since(t0).Nanoseconds(), renderSum, recs)
}

// VerifyHashOnlyL1 compares cached decoded-pixel digests. No PNG decode on
// the equal path.
func VerifyHashOnlyL1Run(frames []RenderedFrame, refs []L1Input) PipelineRun {
	if len(refs) != len(frames) {
		return PipelineRun{Mode: VerifyHashOnlyL1, AllHold: false}
	}
	recs := make([]FrameVerifyRecord, len(frames))
	var renderSum int64
	t0 := time.Now()
	for i := range frames {
		CacheFrameDigest(&frames[i])
		renderSum += frames[i].RenderNS
		in := L1Input{PixelDigest: frames[i].PixelDigest, Pixels: frames[i].Pixels}
		vt0 := time.Now()
		l1 := CompareL1(in, refs[i])
		recs[i] = recordFromL1(frames[i].Index, frames[i], l1, time.Since(vt0).Nanoseconds())
	}
	return finishPipelineRun(VerifyHashOnlyL1, time.Since(t0).Nanoseconds(), renderSum, recs)
}

// RenderFunc produces one frame. The implementation must cache PixelDigest
// from the in-memory buffer before returning when it still holds the pixels.
type RenderFunc func(index int) (RenderedFrame, error)

// RunPipelinedL1 renders frame i+1 while verifying frame i. Verify is
// hash-only L1. The last frame's verify is the residual serial tail.
func RunPipelinedL1(render RenderFunc, refs []L1Input) (PipelineRun, error) {
	n := len(refs)
	if n == 0 {
		return PipelineRun{Mode: VerifyPipelinedL1}, nil
	}
	type stamp struct {
		renderStart, renderEnd time.Time
	}
	stamps := make([]stamp, n)
	recs := make([]FrameVerifyRecord, n)
	var renderSum int64
	var mu sync.Mutex
	t0 := time.Now()

	var prev RenderedFrame
	var prevIdx int
	var havePrev bool
	var wg sync.WaitGroup

	startVerify := func(frame RenderedFrame, idx int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vs := time.Now()
			CacheFrameDigest(&frame)
			l1 := CompareL1(L1Input{PixelDigest: frame.PixelDigest, Pixels: frame.Pixels}, refs[idx])
			rec := recordFromL1(frame.Index, frame, l1, time.Since(vs).Nanoseconds())
			rec.VerifyStart = vs
			rec.VerifyEnd = time.Now()
			mu.Lock()
			recs[idx] = rec
			mu.Unlock()
		}()
	}

	for i := 0; i < n; i++ {
		if havePrev {
			// Verify N starts before render N+1 so the two overlap.
			startVerify(prev, prevIdx)
		}
		rs := time.Now()
		frame, err := render(i)
		re := time.Now()
		if err != nil {
			wg.Wait()
			return PipelineRun{}, fmt.Errorf("render frame %d: %w", i, err)
		}
		if frame.RenderNS == 0 {
			frame.RenderNS = re.Sub(rs).Nanoseconds()
		}
		CacheFrameDigest(&frame)
		renderSum += frame.RenderNS
		stamps[i] = stamp{renderStart: rs, renderEnd: re}
		prev = frame
		prevIdx = i
		havePrev = true
	}
	if havePrev {
		startVerify(prev, prevIdx)
	}
	wg.Wait()
	for i := range recs {
		recs[i].RenderStart = stamps[i].renderStart
		recs[i].RenderEnd = stamps[i].renderEnd
	}
	return finishPipelineRun(VerifyPipelinedL1, time.Since(t0).Nanoseconds(), renderSum, recs), nil
}
