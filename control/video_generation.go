package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// Governance for video generation.
//
// Diffusion video is neither byte-reproducible across heterogeneous suppliers
// nor equipped with a similarity floor that means "correct". Merc's only
// equivalence predicate is bytes.Equal (verification.go); there is no PSNR,
// SSIM, LPIPS or VMAF anywhere. So this lane binds verification to one pinned
// engine build on one hardware class, refuses cross-supplier redundancy the way
// segmented media already does, and does not invent a perceptual threshold.
//
// Content risk is a superset of image generation and the rules are
// prompt-shaped, not artifact-shaped. Policy reuses applyImageGenerationPolicy
// and the same licence pass-through obligations.
//
// There is no pinnable open-licensed video weight set in-tree. The only model
// admitted for exercise is merc-video-synth-v1, a Merc-owned deterministic
// synthesizer marked below routable. Buyer advertisement stays off until a
// real weight set, lifecycle and quality tier authorize it.

const (
	videoGenerationModelRef = "merc-video-synth-v1"
	maxVideoControlBytes    = 64 << 10
	maxVideoResultBytes     = 64 << 20
	maxVideoPromptRunes     = 4000
	videoGenerationJobType  = "video_generation"
)

// allowedVideoProfiles is an allowlist of (resolution × duration × fps), not a
// range. An arbitrary dimension, duration or frame rate is a cheap way to make
// a supplier allocate far more memory and CPU than the quoted price covers, and
// every profile has to have a price.
type videoProfile struct {
	Width        uint32
	Height       uint32
	DurationSecs uint32
	FPS          uint32
}

func (p videoProfile) key() string {
	return fmt.Sprintf("%dx%d@%dfpsx%ds", p.Width, p.Height, p.FPS, p.DurationSecs)
}

func (p videoProfile) frameCount() uint64 {
	return uint64(p.DurationSecs) * uint64(p.FPS)
}

func (p videoProfile) pixelsPerFrame() uint64 {
	return uint64(p.Width) * uint64(p.Height)
}

var allowedVideoProfiles = map[string]videoProfile{
	"256x256@8fpsx1s":  {Width: 256, Height: 256, DurationSecs: 1, FPS: 8},
	"256x256@8fpsx2s":  {Width: 256, Height: 256, DurationSecs: 2, FPS: 8},
	"512x512@8fpsx1s":  {Width: 512, Height: 512, DurationSecs: 1, FPS: 8},
	"512x512@8fpsx2s":  {Width: 512, Height: 512, DurationSecs: 2, FPS: 8},
	"512x512@24fpsx1s": {Width: 512, Height: 512, DurationSecs: 1, FPS: 24},
	"768x768@8fpsx2s":  {Width: 768, Height: 768, DurationSecs: 2, FPS: 8},
}

var (
	errVideoPolicyRefusal = errors.New("video request refused by merc's generation policy")
	errVideoRequestShape  = errors.New("invalid video request")
)

// videoPolicyRefusal names which rule refused. The prompt itself is never
// returned or logged: a refusal that quotes the prompt stores the content the
// refusal existed to avoid storing.
type videoPolicyRefusal struct {
	Rule   string
	Reason string
}

func (e videoPolicyRefusal) Error() string {
	return fmt.Sprintf("%v: %s (%s)", errVideoPolicyRefusal, e.Reason, e.Rule)
}

func (e videoPolicyRefusal) Unwrap() error { return errVideoPolicyRefusal }

func isVideoGenerationJob(sub jobSubmit) bool {
	return sub.JobType.Type == videoGenerationJobType
}

func isVideoGenerationJobType(jobType string) bool {
	return jobType == videoGenerationJobType
}

// isSegmentedMediaJob is any lane that decomposes into ordered time units under
// the shared media segment plan. Media rendering is deliberately excluded: it
// is one scene, one artifact.
func isSegmentedMediaJob(sub jobSubmit) bool {
	return isMediaTranscodeJob(sub) || isVideoGenerationJob(sub)
}

func normalizeVideoGenerationJobType(j *JobType) error {
	if j == nil {
		return errors.New("video_generation job_type is required")
	}
	if j.BatchSize != 0 || j.EmbedBinary || j.MaxTokens != 0 || j.Temperature != 0 ||
		j.InputFormat != "" || j.MaxWidth != 0 || j.MaxHeight != 0 || j.VideoBitrateKbps != 0 {
		return errors.New("video_generation cannot carry embedding, generation, or transcode fields")
	}
	if j.RenderWidth == 0 || j.RenderHeight == 0 || j.FPS == 0 || j.DurationSecs == 0 {
		return fmt.Errorf("%w: render_width, render_height, fps and duration_secs are required",
			errVideoRequestShape)
	}
	profile := videoProfile{
		Width: j.RenderWidth, Height: j.RenderHeight,
		DurationSecs: j.DurationSecs, FPS: j.FPS,
	}
	if _, ok := allowedVideoProfiles[profile.key()]; !ok {
		return fmt.Errorf("%w: profile %q is not offered; merc generates %s",
			errVideoRequestShape, profile.key(), strings.Join(sortedVideoProfiles(), ", "))
	}
	return nil
}

func sortedVideoProfiles() []string {
	out := make([]string, 0, len(allowedVideoProfiles))
	for k := range allowedVideoProfiles {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// videoGenerationRequest is the closed prompt document the buyer stores as the
// job input. Geometry lives on job_type so quote, settlement and dispatch share
// one shape; the prompt is the input commitment.
type videoGenerationRequest struct {
	Prompt string `json:"prompt"`
	// Seed is accepted for the deterministic synthesizer. Diffusion seeds are
	// not a similarity floor; they only make one pinned engine reproducible.
	Seed int64 `json:"seed"`
}

func validateVideoGenerationInputBytes(input []byte) (videoGenerationRequest, error) {
	if len(input) == 0 || len(input) > maxVideoControlBytes {
		return videoGenerationRequest{}, fmt.Errorf(
			"%w: video_generation prompt document must contain 1..%d bytes",
			errVideoRequestShape, maxVideoControlBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	var req videoGenerationRequest
	if err := dec.Decode(&req); err != nil {
		return videoGenerationRequest{}, fmt.Errorf(
			"%w: video_generation input is not the closed prompt contract: %v",
			errVideoRequestShape, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return videoGenerationRequest{}, fmt.Errorf(
				"%w: video_generation input has trailing JSON", errVideoRequestShape)
		}
		return videoGenerationRequest{}, fmt.Errorf(
			"%w: video_generation input has invalid trailing JSON: %v",
			errVideoRequestShape, err)
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		return videoGenerationRequest{}, fmt.Errorf("%w: prompt is required", errVideoRequestShape)
	}
	if n := len([]rune(req.Prompt)); n > maxVideoPromptRunes {
		return videoGenerationRequest{}, fmt.Errorf(
			"%w: prompt is %d characters, the limit is %d",
			errVideoRequestShape, n, maxVideoPromptRunes)
	}
	return req, nil
}

// applyVideoGenerationPolicy reuses the image policy engine. Video content risk
// is a superset of image risk and the rules are prompt-shaped.
func applyVideoGenerationPolicy(prompt string) error {
	if err := applyImageGenerationPolicy(prompt); err != nil {
		var img imagePolicyRefusal
		if errors.As(err, &img) {
			return videoPolicyRefusal{Rule: img.Rule, Reason: img.Reason}
		}
		if errors.Is(err, errImagePolicyRefusal) {
			return videoPolicyRefusal{Rule: "policy", Reason: "refused by generation policy"}
		}
		return err
	}
	return nil
}

// videoLicenceTerms extends the image resale allowlist with the Merc-owned
// exercise synthesizer contract. Open image/video licences (OpenRAIL) still
// require pass-through use restrictions because merc resells generation.
var videoLicenceTerms = map[string]imageLicenceObligations{
	"Apache-2.0":             {PassThroughUseRestrictions: false},
	"MIT":                    {PassThroughUseRestrictions: false},
	"Merc-Internal-Contract": {PassThroughUseRestrictions: false},
	"CreativeML-OpenRAIL-M": {
		PassThroughUseRestrictions: true,
		AttributionText:            "CreativeML Open RAIL-M",
	},
	"OpenRAIL++-M": {
		PassThroughUseRestrictions: true,
		AttributionText:            "CreativeML Open RAIL++-M",
	},
}

// validateVideoModelLicence checks merc can resell generation from a video
// model, and that where the licence demands restrictions be passed downstream,
// merc has a policy that does so.
func validateVideoModelLicence(modelID, licence string, hasEnforcedUsePolicy bool) error {
	terms, known := videoLicenceTerms[licence]
	if !known {
		return fmt.Errorf("video model %q declares licence %q, which is not on the video "+
			"resale allowlist; open video licences attach use restrictions that text "+
			"licences do not, so each one has to be read before it is added", modelID, licence)
	}
	if terms.PassThroughUseRestrictions && !hasEnforcedUsePolicy {
		return fmt.Errorf("video model %q is licensed under %q, which requires merc to bind "+
			"downstream users to the same use restrictions; merc resells generation, so it "+
			"may not serve this model without a use policy of its own", modelID, licence)
	}
	return nil
}

// videoGenerationLicenceGate runs on the request path for every admitted video
// model. The exercise synthesizer is Merc-Internal-Contract (no pass-through).
// A future OpenRAIL video weight set is refused unless the use policy is live.
func videoGenerationLicenceGate(modelID string) error {
	model, ok := runtimeAuthorityModels[modelID]
	if !ok {
		return fmt.Errorf("%w: model %q is not admitted for video_generation",
			errVideoRequestShape, modelID)
	}
	// hasEnforcedUsePolicy is true because applyVideoGenerationPolicy runs on
	// every request (asserted by tests). That is what OpenRAIL pass-through needs.
	return validateVideoModelLicence(modelID, model.License, true)
}

// videoInputScan is the quote-side geometry for a prompt document. segmentCount
// is the number of ordered work units so N segments price as N units rather
// than collapsing under a single-blob formula.
func videoInputScan(input []byte, segmentCount int) (QuoteInputScan, error) {
	if _, err := validateVideoGenerationInputBytes(input); err != nil {
		return QuoteInputScan{}, err
	}
	if segmentCount <= 0 {
		segmentCount = 1
	}
	if segmentCount > maxMediaSegments {
		return QuoteInputScan{}, fmt.Errorf(
			"video_generation segment_count %d exceeds bound %d", segmentCount, maxMediaSegments)
	}
	var descriptor bytes.Buffer
	descriptor.WriteString("video-generation:")
	descriptor.WriteString(fmt.Sprintf("%d:%d", segmentCount, len(input)))
	acc := newInputDepthAccumulator()
	acc.addBody(descriptor.String())
	depth, err := acc.profile()
	if err != nil {
		return QuoteInputScan{}, err
	}
	return QuoteInputScan{
		Records: segmentCount, Bytes: len(input), EstimatedTokens: depth.EstimatedTokens,
		MaxLineBytes: len(input), SampledRecords: 0, InputDepth: depth,
	}, nil
}

// deriveVideoSegmentPlan freezes N ordered units covering the requested
// duration. Extents are wall-clock seconds, not abstract partitions.
func deriveVideoSegmentPlan(unitCount int64, durationSecs float64) (MediaSegmentPlan, error) {
	if durationSecs <= 0 || math.IsNaN(durationSecs) || math.IsInf(durationSecs, 0) {
		return MediaSegmentPlan{}, errors.New("video_generation requires a positive finite duration")
	}
	return deriveMediaSegmentPlan(unitCount, durationSecs)
}

// validateVideoSegmentDurationSum is the cross-unit invariant segmented media
// still lacked for generation: segment durations must sum to the requested
// duration. Per-artifact checks only see one segment relative to itself.
func validateVideoSegmentDurationSum(requestedDurationSecs float64, units []MediaSegmentUnit) error {
	if err := validateMediaSegmentExtents(requestedDurationSecs, units); err != nil {
		return fmt.Errorf("video_generation segment duration plan refused: %w", err)
	}
	return nil
}

// settlementInputUnitsForVideoGeneration prices on the existing pixel basis
// extended by frame count. Each segment is one record; the per-segment unit is
// width × height × total_frames so N segments price as N units of that geometry
// rather than collapsing under a single-blob formula (same N-scaling as
// segmented media_transcode).
func settlementInputUnitsForVideoGeneration(jobType JobType, records int) float64 {
	if records <= 0 || jobType.RenderWidth == 0 || jobType.RenderHeight == 0 ||
		jobType.FPS == 0 || jobType.DurationSecs == 0 {
		return 0
	}
	totalFrames := uint64(jobType.DurationSecs) * uint64(jobType.FPS)
	if totalFrames == 0 {
		return 0
	}
	pixels := uint64(jobType.RenderWidth) * uint64(jobType.RenderHeight)
	perSegment := float64(pixels) * float64(totalFrames)
	return perSegment * float64(records)
}

// videoGenerationBuyerRoutable reports whether ordinary buyer traffic may reach
// any cell for this job/model. The exercise synthesizer is deliberately false.
func videoGenerationBuyerRoutable(modelID string) bool {
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if cell.Job == videoGenerationJobType && cell.Model == modelID && cell.Routable(profile) {
				return true
			}
		}
	}
	return false
}

// refuseVideoGenerationIfNotRoutable is the ordinary-buyer gate. Directed
// exercise and unit tests may still touch the synthesizer; a quote or firm
// submit for ordinary traffic may not.
func refuseVideoGenerationIfNotRoutable(sub jobSubmit) error {
	if !isVideoGenerationJob(sub) {
		return nil
	}
	if videoGenerationBuyerRoutable(sub.Model.Ref) {
		return nil
	}
	return errors.New("video_generation is not buyer-routable: no cell has a lifecycle and quality tier that authorize ordinary buyer traffic")
}

// validateVideoGenerationResult accepts the deterministic synthesizer envelope.
// A diffusion MP4 is not admitted; inventing one would pretend a weight set exists.
func validateVideoGenerationResult(body []byte, records resultRecordContract) error {
	if len(body) == 0 || len(body) > maxVideoResultBytes {
		return invalidResultArtifact(videoGenerationJobType, resultValidationCount,
			fmt.Sprintf("output is %d bytes; allowed range is 1..%d", len(body), maxVideoResultBytes))
	}
	if records.Exact > 0 && records.Exact != 1 {
		return invalidResultArtifact(videoGenerationJobType, resultValidationCount,
			fmt.Sprintf("video segment task must produce exactly one artifact, attempt expects %d", records.Exact))
	}
	if !bytes.HasPrefix(body, []byte("MERCVIDEO1\n")) {
		return invalidResultArtifact(videoGenerationJobType, resultValidationEnvelope,
			"output is not the merc-video-synth-v1 envelope")
	}
	return nil
}
