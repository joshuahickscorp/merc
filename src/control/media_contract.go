package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// MediaTranscode is intentionally a narrow, fixed contract. It is not a
// generic ffmpeg command surface: the agent owns the executable and argument
// template, while the control plane owns the bounded input, output and money
// geometry. Keeping these limits here gives quote, submit and result
// verification one shared boundary.
const (
	mediaTranscodeModelRef   = "ffmpeg-transcode-v1"
	maxMediaControlBytes     = 64 << 20
	maxMediaResultBytes      = 64 << 20
	mediaTranscodeDefaultFPS = 30
)

var mediaInputFormats = map[string]bool{
	"mp4": true, "mov": true, "webm": true, "mkv": true,
}

func isMediaTranscodeJob(sub jobSubmit) bool { return sub.JobType.Type == "media_transcode" }

func isBinaryMediaJob(sub jobSubmit) bool {
	return isMediaTranscodeJob(sub) || isMediaRenderingJob(sub)
}

func normalizeMediaJobType(j *JobType) error {
	if j == nil {
		return errors.New("media_transcode job_type is required")
	}
	if j.BatchSize != 0 || j.EmbedBinary || j.MaxTokens != 0 || j.Temperature != 0 {
		return errors.New("media_transcode cannot carry embedding or generation fields")
	}
	format := strings.ToLower(strings.TrimSpace(j.InputFormat))
	if !mediaInputFormats[format] {
		return fmt.Errorf("media_transcode input_format must be one of mp4, mov, webm, mkv")
	}
	j.InputFormat = format
	if j.MaxWidth < 64 || j.MaxWidth > 4096 || j.MaxWidth%2 != 0 {
		return errors.New("media_transcode max_width must be an even value in [64,4096]")
	}
	if j.MaxHeight < 64 || j.MaxHeight > 4096 || j.MaxHeight%2 != 0 {
		return errors.New("media_transcode max_height must be an even value in [64,4096]")
	}
	if j.FPS == 0 {
		j.FPS = mediaTranscodeDefaultFPS
	}
	if j.FPS > 60 {
		return errors.New("media_transcode fps must be in [1,60]")
	}
	if j.VideoBitrateKbps < 200 || j.VideoBitrateKbps > 50_000 {
		return errors.New("media_transcode video_bitrate_kbps must be in [200,50000]")
	}
	return nil
}

// mediaInputScan is the quote-side representation of a binary media object.
// The body-depth profile is deliberately synthetic and records only a bounded
// descriptor; it must never be mistaken for text tokenisation or used as a
// second price authority. Settlement units remain the explicitly documented
// media byte geometry in settlementInputUnitsForGeometry.
//
// segmentCount is the number of ordered work units (default 1). Each segment
// is one InputRecords unit so N segments price as N units of work rather than
// collapsing to a single binary blob under max(records, bytes/4).
func mediaInputScan(input []byte, segmentCount int) (QuoteInputScan, error) {
	if len(input) == 0 {
		return QuoteInputScan{}, errors.New("media_transcode input is empty")
	}
	if len(input) > maxMediaControlBytes {
		return QuoteInputScan{}, fmt.Errorf("media_transcode input exceeds %d bytes", maxMediaControlBytes)
	}
	if segmentCount <= 0 {
		segmentCount = 1
	}
	if segmentCount > maxMediaSegments {
		return QuoteInputScan{}, fmt.Errorf("media_transcode segment_count %d exceeds bound %d", segmentCount, maxMediaSegments)
	}
	var descriptor bytes.Buffer
	descriptor.WriteString("media-transcode:")
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

func validateMediaInputBytes(input []byte) error {
	if len(input) == 0 {
		return errors.New("media_transcode input is empty")
	}
	if len(input) > maxMediaControlBytes {
		return fmt.Errorf("media_transcode input exceeds %d bytes", maxMediaControlBytes)
	}
	// FFmpeg performs the authoritative stream/protocol validation in the
	// agent. The control plane still rejects obvious non-container bytes early,
	// so a JSONL payload cannot accidentally enter a binary task lane.
	if len(input) < 8 || (string(input[4:8]) != "ftyp" && !bytes.HasPrefix(input, []byte("\x1a\x45\xdf\xa3"))) {
		return errors.New("media_transcode input is not a recognized MP4/Matroska container")
	}
	return nil
}

func mediaContentType(format string) string {
	switch strings.ToLower(format) {
	case "webm":
		return "video/webm"
	case "mkv":
		return "video/x-matroska"
	default:
		return "video/" + strings.ToLower(format)
	}
}
