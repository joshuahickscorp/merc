package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"math"
	"strings"
)

// Output equivalence levels from the Merc Render plan §3.
//
// L0 is encoded-file equality after container metadata is normalized.
// Raw PNG hashes are not L0: Blender stamps eXIf/tEXt so honest identical
// pixels produce different file bytes. L3 is never an exact level and is
// never substituted for a failed L0/L1/L2.
const (
	EquivL0EncodedFileEqual  = "L0_ENCODED_FILE_EQUAL"
	EquivL1PixelExact        = "L1_PIXEL_EXACT"
	EquivL2LinearBufferExact = "L2_LINEAR_BUFFER_EXACT"
	EquivL3QualityContract   = "L3_QUALITY_CONTRACT"
	linearBufferMagic        = "MERCLIN1"
	pngSig                   = "\x89PNG\r\n\x1a\n"
)

// PNG ancillary chunks that carry timestamps, software stamps, or EXIF and
// are not part of the encoded pixel payload. Color-space chunks stay.
var pngMetadataChunkTypes = map[string]bool{
	"tEXt": true,
	"zTXt": true,
	"iTXt": true,
	"eXIf": true,
	"tIME": true,
}

// EquivLevel is one named rung of the output contract.
type EquivLevel string

// QualitySpec is an explicit L3 metric and bound. An empty Metric refuses
// evaluation so a caller cannot accidentally inherit a default tolerance.
type QualitySpec struct {
	Metric  string  `json:"metric"`
	Bound   float64 `json:"bound"`
	Epsilon float64 `json:"epsilon,omitempty"`
}

// FileEqualResult is L0 plus the raw-hash trap diagnostic.
type FileEqualResult struct {
	Holds                bool     `json:"holds"`
	RawBytesEqual        bool     `json:"raw_bytes_equal"`
	NormalizedBytesEqual bool     `json:"normalized_bytes_equal"`
	RawSHA256A           string   `json:"raw_sha256_a"`
	RawSHA256B           string   `json:"raw_sha256_b"`
	NormalizedSHA256A    string   `json:"normalized_sha256_a"`
	NormalizedSHA256B    string   `json:"normalized_sha256_b"`
	StrippedChunksA      []string `json:"stripped_chunks_a"`
	StrippedChunksB      []string `json:"stripped_chunks_b"`
	KeptChunksA          []string `json:"kept_chunks_a"`
	KeptChunksB          []string `json:"kept_chunks_b"`
	Error                string   `json:"error,omitempty"`
}

// PixelExactResult is L1: decoded 8-bit RGB buffers.
//
// Path names how the decision was computed. "decoded_pixel_digest" is the
// same PIXEL_EXACT assertion as walking the arrays — a SHA-256 of the
// tightly packed decoded RGB — and is the common-case fast path.
// "pixel_array" is the divergence path: max-abs, mean-abs, and
// differing-pixel-count. A digest match never skips a real mismatch;
// a digest mismatch never reports Holds.
type PixelExactResult struct {
	Holds           bool    `json:"holds"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	Channels        int     `json:"channels"`
	Pixels          int     `json:"pixels"`
	DifferingPixels int     `json:"differing_pixels"`
	MaxAbs          int     `json:"max_abs"`
	MeanAbs         float64 `json:"mean_abs"`
	FirstDiffXY     []int   `json:"first_diff_xy,omitempty"`
	PixelSHA256A    string  `json:"pixel_sha256_a"`
	PixelSHA256B    string  `json:"pixel_sha256_b"`
	Path            string  `json:"path,omitempty"`
	Decoded         bool    `json:"decoded,omitempty"`
	Error           string  `json:"error,omitempty"`
}

const (
	l1PathDigest = "decoded_pixel_digest"
	l1PathArray  = "pixel_array"
)

// L1Input is one side of an L1 comparison. PixelDigest is SHA-256 of the
// decoded 8-bit RGB (never the PNG container). The renderer should fill
// PixelDigest from the in-memory buffer so the verify path does not decode.
type L1Input struct {
	PNG         []byte
	Pixels      PixelBuffer
	PixelDigest string
}

// LinearExactResult is L2: pre-encoding scene-linear float buffers.
type LinearExactResult struct {
	Holds           bool    `json:"holds"`
	BitsEqual       bool    `json:"bits_equal"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	Channels        int     `json:"channels"`
	Values          int     `json:"values"`
	DifferingValues int     `json:"differing_values"`
	DifferingPixels int     `json:"differing_pixels"`
	MaxAbs          float64 `json:"max_abs"`
	MeanAbs         float64 `json:"mean_abs"`
	RMSE            float64 `json:"rmse"`
	FirstDiffXY     []int   `json:"first_diff_xy,omitempty"`
	BufferSHA256A   string  `json:"buffer_sha256_a"`
	BufferSHA256B   string  `json:"buffer_sha256_b"`
	Error           string  `json:"error,omitempty"`
}

// QualityResult is L3: a named metric against a stated bound.
type QualityResult struct {
	Evaluated bool               `json:"evaluated"`
	Holds     bool               `json:"holds"`
	Metric    string             `json:"metric,omitempty"`
	Bound     float64            `json:"bound,omitempty"`
	Observed  float64            `json:"observed,omitempty"`
	Epsilon   float64            `json:"epsilon,omitempty"`
	Note      string             `json:"note,omitempty"`
	Error     string             `json:"error,omitempty"`
	Linear    *LinearExactResult `json:"linear,omitempty"`
	Pixel     *PixelExactResult  `json:"pixel,omitempty"`
}

// EquivalenceReport is the four levels for one pair. Each level is
// independent. HighestExactHolding never includes L3.
type EquivalenceReport struct {
	L0                     FileEqualResult   `json:"l0_encoded_file_equal"`
	L1                     PixelExactResult  `json:"l1_pixel_exact"`
	L2                     LinearExactResult `json:"l2_linear_buffer_exact"`
	L3                     QualityResult     `json:"l3_quality_contract"`
	HighestExactHolding    string            `json:"highest_exact_holding"`
	RequiredLevel          string            `json:"required_level,omitempty"`
	MeetsRequired          bool              `json:"meets_required"`
	SilentDowngradeRefused bool              `json:"silent_downgrade_refused"`
}

// PixelBuffer is tightly packed 8-bit RGB, row-major, top-left origin.
type PixelBuffer struct {
	Width    int
	Height   int
	Channels int
	Pix      []byte
}

// LinearBuffer is tightly packed float32 scene-linear RGB(A), row-major,
// top-left origin. This is the L2 authority — not an 8-bit PNG.
type LinearBuffer struct {
	Width    int
	Height   int
	Channels int
	Pix      []float32
}

// PNGChunk is one PNG chunk with its on-disk type and payload.
type PNGChunk struct {
	Type string
	Data []byte
}

// NormalizePNG strips eXIf/tEXt/zTXt/iTXt/tIME and returns a re-serialized
// PNG whose IDAT and color-space chunks are otherwise unchanged.
func NormalizePNG(raw []byte) ([]byte, []string, []string, error) {
	chunks, err := parsePNGChunks(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	var kept []PNGChunk
	var stripped, keptTypes []string
	for _, c := range chunks {
		if pngMetadataChunkTypes[c.Type] {
			stripped = append(stripped, c.Type)
			continue
		}
		kept = append(kept, c)
		keptTypes = append(keptTypes, c.Type)
	}
	out, err := serializePNG(kept)
	if err != nil {
		return nil, stripped, keptTypes, err
	}
	return out, stripped, keptTypes, nil
}

// CompareEncodedPNG is L0. Holds is true only when the normalized files
// are byte-identical. Raw equality is reported but is not L0.
func CompareEncodedPNG(a, b []byte) FileEqualResult {
	out := FileEqualResult{
		RawBytesEqual: bytes.Equal(a, b),
		RawSHA256A:    sha256Hex(a),
		RawSHA256B:    sha256Hex(b),
	}
	na, sa, ka, errA := NormalizePNG(a)
	nb, sb, kb, errB := NormalizePNG(b)
	out.StrippedChunksA = sa
	out.StrippedChunksB = sb
	out.KeptChunksA = ka
	out.KeptChunksB = kb
	if errA != nil {
		out.Error = "a: " + errA.Error()
		return out
	}
	if errB != nil {
		out.Error = "b: " + errB.Error()
		return out
	}
	out.NormalizedBytesEqual = bytes.Equal(na, nb)
	out.NormalizedSHA256A = sha256Hex(na)
	out.NormalizedSHA256B = sha256Hex(nb)
	out.Holds = out.NormalizedBytesEqual
	return out
}

// DecodePNGPixels returns 8-bit RGB (alpha dropped) in top-left order.
func DecodePNGPixels(raw []byte) (PixelBuffer, error) {
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return PixelBuffer{}, err
	}
	return pixelsFromImage(img), nil
}

func pixelsFromImage(img image.Image) PixelBuffer {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	pix := make([]byte, w*h*3)
	i := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			pix[i] = uint8(r >> 8)
			pix[i+1] = uint8(g >> 8)
			pix[i+2] = uint8(bl >> 8)
			i += 3
		}
	}
	return PixelBuffer{Width: w, Height: h, Channels: 3, Pix: pix}
}

// ComparePixels is L1.
func ComparePixels(a, b PixelBuffer) PixelExactResult {
	out := PixelExactResult{
		Width:        a.Width,
		Height:       a.Height,
		Channels:     a.Channels,
		Pixels:       a.Width * a.Height,
		PixelSHA256A: sha256Hex(a.Pix),
		PixelSHA256B: sha256Hex(b.Pix),
	}
	if a.Width != b.Width || a.Height != b.Height || a.Channels != b.Channels {
		out.Error = fmt.Sprintf("shape %dx%dx%d vs %dx%dx%d",
			a.Width, a.Height, a.Channels, b.Width, b.Height, b.Channels)
		return out
	}
	if len(a.Pix) != len(b.Pix) || len(a.Pix) != a.Width*a.Height*a.Channels {
		out.Error = "pixel buffer length mismatch"
		return out
	}
	ch := a.Channels
	var sum float64
	first := true
	for y := 0; y < a.Height; y++ {
		for x := 0; x < a.Width; x++ {
			off := (y*a.Width + x) * ch
			pixelDiff := false
			for c := 0; c < ch; c++ {
				d := int(a.Pix[off+c]) - int(b.Pix[off+c])
				if d < 0 {
					d = -d
				}
				sum += float64(d)
				if d > out.MaxAbs {
					out.MaxAbs = d
				}
				if d != 0 {
					pixelDiff = true
				}
			}
			if pixelDiff {
				out.DifferingPixels++
				if first {
					out.FirstDiffXY = []int{x, y}
					first = false
				}
			}
		}
	}
	n := float64(len(a.Pix))
	if n > 0 {
		out.MeanAbs = sum / n
	}
	out.Holds = out.DifferingPixels == 0 && out.Error == ""
	out.Path = l1PathArray
	return out
}

// DigestPixelBuffer is SHA-256 of tightly packed 8-bit RGB. This is L1
// PIXEL_EXACT computed as a digest: equal decoded pixels produce equal
// digests; a one-pixel change does not.
func DigestPixelBuffer(buf PixelBuffer) string {
	if len(buf.Pix) == 0 {
		return ""
	}
	return sha256Hex(buf.Pix)
}

// CachePixelDigest stores the decoded-pixel digest on in. Call this at
// render time while the buffer is still in memory. Hashing 1024² RGB is
// ~1 ms; decoding the PNG later is ~150× that.
func CachePixelDigest(in *L1Input) {
	if in == nil {
		return
	}
	if in.PixelDigest != "" {
		return
	}
	if len(in.Pixels.Pix) == 0 && len(in.PNG) > 0 {
		if buf, err := DecodePNGPixels(in.PNG); err == nil {
			in.Pixels = buf
		}
	}
	if len(in.Pixels.Pix) > 0 {
		in.PixelDigest = DigestPixelBuffer(in.Pixels)
	}
}

func l1PixelsPresent(in L1Input) bool {
	return len(in.Pixels.Pix) > 0 && in.Pixels.Width > 0 && in.Pixels.Height > 0
}

func resolveL1Pixels(in L1Input) (PixelBuffer, bool, error) {
	if l1PixelsPresent(in) {
		return in.Pixels, false, nil
	}
	if len(in.PNG) == 0 {
		return PixelBuffer{}, false, fmt.Errorf("no pixel buffer or PNG")
	}
	buf, err := DecodePNGPixels(in.PNG)
	if err != nil {
		return PixelBuffer{}, false, err
	}
	return buf, true, nil
}

func resolveL1Digest(in L1Input) (string, PixelBuffer, bool, error) {
	if in.PixelDigest != "" {
		return in.PixelDigest, in.Pixels, false, nil
	}
	buf, decoded, err := resolveL1Pixels(in)
	if err != nil {
		return "", PixelBuffer{}, decoded, err
	}
	return DigestPixelBuffer(buf), buf, decoded, nil
}

// CompareL1 is L1 PIXEL_EXACT. When both sides have a decoded-pixel digest
// and the digests match, the arrays are not walked and no PNG is decoded.
// When digests differ the pixel-array path runs so the report still has
// max-abs, mean-abs and differing-pixel-count.
func CompareL1(a, b L1Input) PixelExactResult {
	da, pa, decA, errA := resolveL1Digest(a)
	if errA != nil && a.PixelDigest == "" {
		return PixelExactResult{Error: "a: " + errA.Error()}
	}
	db, pb, decB, errB := resolveL1Digest(b)
	if errB != nil && b.PixelDigest == "" {
		return PixelExactResult{Error: "b: " + errB.Error()}
	}
	if a.PixelDigest != "" {
		da = a.PixelDigest
	}
	if b.PixelDigest != "" {
		db = b.PixelDigest
	}

	out := PixelExactResult{
		Width:        pa.Width,
		Height:       pa.Height,
		Channels:     pa.Channels,
		Pixels:       pa.Width * pa.Height,
		PixelSHA256A: da,
		PixelSHA256B: db,
		Decoded:      decA || decB,
	}
	if da == "" || db == "" {
		out.Error = "missing decoded-pixel digest"
		return out
	}
	if da == db {
		out.Holds = true
		out.Path = l1PathDigest
		if out.Width == 0 && l1PixelsPresent(b) {
			out.Width = pb.Width
			out.Height = pb.Height
			out.Channels = pb.Channels
			out.Pixels = pb.Width * pb.Height
		}
		return out
	}

	// Divergence: need the arrays for diagnostics.
	if !l1PixelsPresent(L1Input{Pixels: pa}) {
		var err error
		pa, decA, err = resolveL1Pixels(a)
		if err != nil {
			out.Error = "digests differ; pixel buffers required for divergence diagnostics: a: " + err.Error()
			out.Path = l1PathDigest
			return out
		}
		out.Decoded = out.Decoded || decA
	}
	if !l1PixelsPresent(L1Input{Pixels: pb}) {
		var err error
		pb, decB, err = resolveL1Pixels(b)
		if err != nil {
			out.Error = "digests differ; pixel buffers required for divergence diagnostics: b: " + err.Error()
			out.Path = l1PathDigest
			return out
		}
		out.Decoded = out.Decoded || decB
	}
	walked := ComparePixels(pa, pb)
	walked.Path = l1PathArray
	walked.Decoded = out.Decoded
	return walked
}

// ParseLinearBuffer reads the MERCLIN1 dump written by the Blender script.
func ParseLinearBuffer(raw []byte) (LinearBuffer, error) {
	const hdr = 8 + 4 + 4 + 4
	if len(raw) < hdr {
		return LinearBuffer{}, fmt.Errorf("linear buffer too short: %d bytes", len(raw))
	}
	if string(raw[:8]) != linearBufferMagic {
		return LinearBuffer{}, fmt.Errorf("linear buffer magic %q, want %q", raw[:8], linearBufferMagic)
	}
	w := int(binary.LittleEndian.Uint32(raw[8:12]))
	h := int(binary.LittleEndian.Uint32(raw[12:16]))
	ch := int(binary.LittleEndian.Uint32(raw[16:20]))
	if w < 1 || h < 1 || ch < 3 || ch > 4 {
		return LinearBuffer{}, fmt.Errorf("linear buffer shape %dx%dx%d rejected", w, h, ch)
	}
	need := hdr + w*h*ch*4
	if len(raw) != need {
		return LinearBuffer{}, fmt.Errorf("linear buffer size %d, want %d", len(raw), need)
	}
	n := w * h * ch
	pix := make([]float32, n)
	for i := 0; i < n; i++ {
		pix[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[hdr+i*4 : hdr+i*4+4]))
	}
	return LinearBuffer{Width: w, Height: h, Channels: ch, Pix: pix}, nil
}

// MarshalLinearBuffer writes MERCLIN1.
func MarshalLinearBuffer(buf LinearBuffer) ([]byte, error) {
	if buf.Width < 1 || buf.Height < 1 || buf.Channels < 3 || buf.Channels > 4 {
		return nil, fmt.Errorf("linear buffer shape %dx%dx%d rejected", buf.Width, buf.Height, buf.Channels)
	}
	if len(buf.Pix) != buf.Width*buf.Height*buf.Channels {
		return nil, fmt.Errorf("linear buffer length %d, want %d", len(buf.Pix), buf.Width*buf.Height*buf.Channels)
	}
	out := make([]byte, 20+len(buf.Pix)*4)
	copy(out[:8], linearBufferMagic)
	binary.LittleEndian.PutUint32(out[8:12], uint32(buf.Width))
	binary.LittleEndian.PutUint32(out[12:16], uint32(buf.Height))
	binary.LittleEndian.PutUint32(out[16:20], uint32(buf.Channels))
	for i, v := range buf.Pix {
		binary.LittleEndian.PutUint32(out[20+i*4:20+i*4+4], math.Float32bits(v))
	}
	return out, nil
}

// CompareLinear is L2. Holds requires bit-identical float32 payloads.
func CompareLinear(a, b LinearBuffer) LinearExactResult {
	out := LinearExactResult{
		Width:         a.Width,
		Height:        a.Height,
		Channels:      a.Channels,
		Values:        len(a.Pix),
		BufferSHA256A: linearSHA(a),
		BufferSHA256B: linearSHA(b),
	}
	if a.Width != b.Width || a.Height != b.Height || a.Channels != b.Channels {
		out.Error = fmt.Sprintf("shape %dx%dx%d vs %dx%dx%d",
			a.Width, a.Height, a.Channels, b.Width, b.Height, b.Channels)
		return out
	}
	if len(a.Pix) != len(b.Pix) {
		out.Error = "linear buffer length mismatch"
		return out
	}
	ch := a.Channels
	var sum, sumSq float64
	first := true
	bitsEqual := true
	for y := 0; y < a.Height; y++ {
		for x := 0; x < a.Width; x++ {
			off := (y*a.Width + x) * ch
			pixelDiff := false
			for c := 0; c < ch; c++ {
				av, bv := a.Pix[off+c], b.Pix[off+c]
				if math.Float32bits(av) != math.Float32bits(bv) {
					bitsEqual = false
					out.DifferingValues++
					pixelDiff = true
				}
				d := math.Abs(float64(av) - float64(bv))
				sum += d
				sumSq += d * d
				if d > out.MaxAbs {
					out.MaxAbs = d
				}
			}
			if pixelDiff {
				out.DifferingPixels++
				if first {
					out.FirstDiffXY = []int{x, y}
					first = false
				}
			}
		}
	}
	n := float64(len(a.Pix))
	if n > 0 {
		out.MeanAbs = sum / n
		out.RMSE = math.Sqrt(sumSq / n)
	}
	out.BitsEqual = bitsEqual
	out.Holds = bitsEqual && out.Error == ""
	return out
}

// EvaluateQuality is L3. It refuses to run without a named metric.
// A holding L3 never upgrades HighestExactHolding and never satisfies a
// required exact level.
func EvaluateQuality(linA, linB LinearBuffer, pixA, pixB PixelBuffer, spec QualitySpec) QualityResult {
	out := QualityResult{
		Metric:  strings.TrimSpace(spec.Metric),
		Bound:   spec.Bound,
		Epsilon: spec.Epsilon,
		Note:    "L3 is a bounded tolerance, not an exact level; it is never substituted for L0/L1/L2",
	}
	if out.Metric == "" {
		out.Error = "L3 refused: metric is empty (no implicit default)"
		return out
	}
	if math.IsNaN(spec.Bound) || math.IsInf(spec.Bound, 0) || spec.Bound < 0 {
		out.Error = "L3 refused: bound must be a finite number >= 0"
		return out
	}
	lin := CompareLinear(linA, linB)
	pix := ComparePixels(pixA, pixB)
	out.Linear = &lin
	out.Pixel = &pix

	var observed float64
	var err string
	switch out.Metric {
	case "max_abs_linear":
		observed, err = lin.MaxAbs, lin.Error
	case "mean_abs_linear":
		observed, err = lin.MeanAbs, lin.Error
	case "rmse_linear":
		observed, err = lin.RMSE, lin.Error
	case "max_abs_u8":
		observed, err = float64(pix.MaxAbs), pix.Error
	case "mean_abs_u8":
		observed, err = pix.MeanAbs, pix.Error
	case "differing_pixels_u8":
		observed, err = float64(pix.DifferingPixels), pix.Error
	case "differing_pixels_linear":
		observed, err = float64(lin.DifferingPixels), lin.Error
	default:
		out.Error = fmt.Sprintf("L3 refused: unknown metric %q", out.Metric)
		return out
	}
	if err != "" {
		out.Error = err
		return out
	}
	out.Evaluated = true
	out.Observed = observed
	out.Holds = observed <= spec.Bound
	return out
}

// CompareRenderPair evaluates every level that the provided bytes support.
// required is an exact level (L0/L1/L2) or empty. L3 is evaluated only when
// spec.Metric is set; a holding L3 cannot satisfy a required exact level.
func CompareRenderPair(pngA, pngB, linA, linB []byte, spec QualitySpec, required string) EquivalenceReport {
	rep := EquivalenceReport{RequiredLevel: strings.TrimSpace(required)}
	if len(pngA) > 0 && len(pngB) > 0 {
		rep.L0 = CompareEncodedPNG(pngA, pngB)
		pa, errA := DecodePNGPixels(pngA)
		pb, errB := DecodePNGPixels(pngB)
		if errA != nil {
			rep.L1 = PixelExactResult{Error: "a: " + errA.Error()}
		} else if errB != nil {
			rep.L1 = PixelExactResult{Error: "b: " + errB.Error()}
		} else {
			rep.L1 = ComparePixels(pa, pb)
		}
	} else {
		rep.L0.Error = "png bytes missing"
		rep.L1.Error = "png bytes missing"
	}

	var la, lb LinearBuffer
	var linOK bool
	if len(linA) > 0 && len(linB) > 0 {
		var err error
		la, err = ParseLinearBuffer(linA)
		if err != nil {
			rep.L2 = LinearExactResult{Error: "a: " + err.Error()}
		} else {
			lb, err = ParseLinearBuffer(linB)
			if err != nil {
				rep.L2 = LinearExactResult{Error: "b: " + err.Error()}
			} else {
				rep.L2 = CompareLinear(la, lb)
				linOK = true
			}
		}
	} else {
		rep.L2.Error = "linear buffers missing"
	}

	if strings.TrimSpace(spec.Metric) != "" {
		var pa, pb PixelBuffer
		if rep.L1.Error == "" && len(pngA) > 0 {
			pa, _ = DecodePNGPixels(pngA)
			pb, _ = DecodePNGPixels(pngB)
		}
		if !linOK {
			rep.L3 = QualityResult{Metric: spec.Metric, Bound: spec.Bound, Error: "L3 needs linear buffers for linear metrics; " + rep.L2.Error}
			if spec.Metric == "max_abs_u8" || spec.Metric == "mean_abs_u8" || spec.Metric == "differing_pixels_u8" {
				rep.L3 = EvaluateQuality(LinearBuffer{}, LinearBuffer{}, pa, pb, spec)
			}
		} else {
			rep.L3 = EvaluateQuality(la, lb, pa, pb, spec)
		}
	} else {
		rep.L3 = QualityResult{Note: "L3 not evaluated: no metric/bound stated (exact levels preferred)"}
	}

	rep.HighestExactHolding = highestExactHolding(rep)
	rep.MeetsRequired, rep.SilentDowngradeRefused = meetsRequired(rep)
	return rep
}

func highestExactHolding(rep EquivalenceReport) string {
	switch {
	case rep.L2.Holds:
		return EquivL2LinearBufferExact
	case rep.L1.Holds:
		return EquivL1PixelExact
	case rep.L0.Holds:
		return EquivL0EncodedFileEqual
	default:
		return "NONE"
	}
}

func meetsRequired(rep EquivalenceReport) (meets bool, refusedSilent bool) {
	req := strings.TrimSpace(rep.RequiredLevel)
	if req == "" {
		return true, false
	}
	holdsExact := false
	switch req {
	case EquivL0EncodedFileEqual:
		holdsExact = rep.L0.Holds
	case EquivL1PixelExact:
		holdsExact = rep.L1.Holds
	case EquivL2LinearBufferExact:
		holdsExact = rep.L2.Holds
	case EquivL3QualityContract:
		// L3 may be required only when the caller said so explicitly.
		holdsExact = rep.L3.Evaluated && rep.L3.Holds
		return holdsExact, false
	default:
		return false, false
	}
	if holdsExact {
		return true, false
	}
	// L3 holding must not rescue a failed exact requirement.
	if rep.L3.Evaluated && rep.L3.Holds {
		return false, true
	}
	return false, false
}

func parsePNGChunks(raw []byte) ([]PNGChunk, error) {
	if len(raw) < 8 || string(raw[:8]) != pngSig {
		return nil, fmt.Errorf("not a PNG")
	}
	var chunks []PNGChunk
	i := 8
	for i+12 <= len(raw) {
		n := int(binary.BigEndian.Uint32(raw[i : i+4]))
		if n < 0 || i+12+n > len(raw) {
			return nil, fmt.Errorf("truncated PNG chunk at %d", i)
		}
		typ := string(raw[i+4 : i+8])
		data := append([]byte(nil), raw[i+8:i+8+n]...)
		want := binary.BigEndian.Uint32(raw[i+8+n : i+12+n])
		crc := crc32.ChecksumIEEE(raw[i+4 : i+8+n])
		if crc != want {
			return nil, fmt.Errorf("PNG chunk %s crc mismatch", typ)
		}
		chunks = append(chunks, PNGChunk{Type: typ, Data: data})
		i += 12 + n
		if typ == "IEND" {
			break
		}
	}
	if len(chunks) == 0 || chunks[0].Type != "IHDR" {
		return nil, fmt.Errorf("PNG missing IHDR")
	}
	if chunks[len(chunks)-1].Type != "IEND" {
		return nil, fmt.Errorf("PNG missing IEND")
	}
	return chunks, nil
}

func serializePNG(chunks []PNGChunk) ([]byte, error) {
	if len(chunks) == 0 {
		return nil, fmt.Errorf("no PNG chunks")
	}
	var buf bytes.Buffer
	buf.WriteString(pngSig)
	for _, c := range chunks {
		if len(c.Type) != 4 {
			return nil, fmt.Errorf("bad chunk type %q", c.Type)
		}
		var hdr [8]byte
		binary.BigEndian.PutUint32(hdr[0:4], uint32(len(c.Data)))
		copy(hdr[4:8], c.Type)
		buf.Write(hdr[:])
		buf.Write(c.Data)
		crcSrc := append([]byte(c.Type), c.Data...)
		var crc [4]byte
		binary.BigEndian.PutUint32(crc[:], crc32.ChecksumIEEE(crcSrc))
		buf.Write(crc[:])
	}
	return buf.Bytes(), nil
}

// insertPNGChunkAfterIHDR injects an ancillary chunk. Test helper and the
// metadata-trap fixture builder share it.
func insertPNGChunkAfterIHDR(raw []byte, typ string, data []byte) ([]byte, error) {
	chunks, err := parsePNGChunks(raw)
	if err != nil {
		return nil, err
	}
	out := make([]PNGChunk, 0, len(chunks)+1)
	inserted := false
	for _, c := range chunks {
		out = append(out, c)
		if c.Type == "IHDR" && !inserted {
			out = append(out, PNGChunk{Type: typ, Data: append([]byte(nil), data...)})
			inserted = true
		}
	}
	return serializePNG(out)
}

func linearSHA(buf LinearBuffer) string {
	raw, err := MarshalLinearBuffer(buf)
	if err != nil {
		return ""
	}
	return sha256Hex(raw)
}
