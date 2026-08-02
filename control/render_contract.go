package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	mediaRenderingModelRef   = "svg-scene-render-v1"
	maxRenderingControlBytes = 8 << 20
	maxRenderingResultBytes  = 4 << 20
)

func isMediaRenderingJob(sub jobSubmit) bool { return sub.JobType.Type == "media_rendering" }

func normalizeRenderingJobType(j *JobType) error {
	if j == nil {
		return errors.New("media_rendering job_type is required")
	}
	if j.BatchSize != 0 || j.EmbedBinary || j.MaxTokens != 0 || j.Temperature != 0 ||
		j.InputFormat != "" || j.MaxWidth != 0 || j.MaxHeight != 0 || j.FPS != 0 || j.VideoBitrateKbps != 0 {
		return errors.New("media_rendering cannot carry embedding, generation, or transcode fields")
	}
	if j.RenderWidth < 16 || j.RenderWidth > 1024 || j.RenderHeight < 16 || j.RenderHeight > 1024 {
		return errors.New("media_rendering render_width and render_height must be in [16,1024]")
	}
	if uint64(j.RenderWidth)*uint64(j.RenderHeight) > 1024*1024 {
		return errors.New("media_rendering canvas exceeds one-million-pixel bound")
	}
	return nil
}

type renderSceneContract struct {
	Background [3]uint8                  `json:"background"`
	Rectangles []renderRectangleContract `json:"rectangles,omitempty"`
}

type renderRectangleContract struct {
	X      uint32   `json:"x"`
	Y      uint32   `json:"y"`
	Width  uint32   `json:"width"`
	Height uint32   `json:"height"`
	Color  [3]uint8 `json:"color"`
}

func validateRenderingInputBytes(input []byte, width, height uint32) error {
	if len(input) == 0 || len(input) > maxRenderingControlBytes {
		return fmt.Errorf("media_rendering scene must contain 1..%d bytes", maxRenderingControlBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	var scene renderSceneContract
	if err := dec.Decode(&scene); err != nil {
		return fmt.Errorf("media_rendering scene is not the closed JSON contract: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("media_rendering scene has trailing JSON")
		}
		return fmt.Errorf("media_rendering scene has invalid trailing JSON: %w", err)
	}
	if len(scene.Rectangles) > 256 {
		return errors.New("media_rendering scene has more than 256 rectangles")
	}
	for i, rect := range scene.Rectangles {
		if rect.Width == 0 || rect.Height == 0 || uint64(rect.X)+uint64(rect.Width) > uint64(width) || uint64(rect.Y)+uint64(rect.Height) > uint64(height) {
			return fmt.Errorf("media_rendering rectangle %d is outside the declared canvas", i)
		}
	}
	return nil
}

func renderingInputScan(input []byte) (QuoteInputScan, error) {
	if len(input) == 0 || len(input) > maxRenderingControlBytes {
		return QuoteInputScan{}, fmt.Errorf("media_rendering scene must contain 1..%d bytes", maxRenderingControlBytes)
	}
	var descriptor bytes.Buffer
	descriptor.WriteString("media-rendering:")
	descriptor.WriteString(fmt.Sprintf("%d", len(input)))
	acc := newInputDepthAccumulator()
	acc.addBody(descriptor.String())
	depth, err := acc.profile()
	if err != nil {
		return QuoteInputScan{}, err
	}
	return QuoteInputScan{Records: 1, Bytes: len(input), EstimatedTokens: depth.EstimatedTokens, MaxLineBytes: len(input), SampledRecords: 0, InputDepth: depth}, nil
}

func validateMediaRenderingResult(body []byte, records resultRecordContract) error {
	if len(body) == 0 || len(body) > maxRenderingResultBytes {
		return invalidResultArtifact("media_rendering", resultValidationCount, fmt.Sprintf("output is %d bytes; allowed range is 1..%d", len(body), maxRenderingResultBytes))
	}
	if records.Exact > 0 && records.Exact != 1 {
		return invalidResultArtifact("media_rendering", resultValidationCount, "render output must contain exactly one artifact")
	}
	if !bytes.HasPrefix(body, []byte("P6\n")) {
		return invalidResultArtifact("media_rendering", resultValidationEnvelope, "output is not a P6 PPM artifact")
	}
	end := bytes.Index(body, []byte("\n255\n"))
	if end < 0 || end > 64 {
		return invalidResultArtifact("media_rendering", resultValidationEnvelope, "PPM header is missing or unbounded")
	}
	var width, height int
	if _, err := fmt.Sscanf(string(body[3:end]), "%d %d", &width, &height); err != nil || width < 16 || height < 16 || width > 1024 || height > 1024 {
		return invalidResultArtifact("media_rendering", resultValidationEnvelope, "PPM dimensions are outside the governed bound")
	}
	if len(body) != end+5+width*height*3 {
		return invalidResultArtifact("media_rendering", resultValidationCount, "PPM pixel payload length does not match its header")
	}
	return nil
}
