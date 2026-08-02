package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// mergeMediaSegmentsToFile concatenates N ordered segment MP4s into one
// delivery object. Segment 0 is already loaded as first; remaining ordinals
// are fetched in ChunkIndex order. The concat demuxer stream-copies so merge
// does not re-encode and cannot invent agreement across divergent segments.
func mergeMediaSegmentsToFile(
	ctx context.Context,
	storage *Storage,
	out io.Writer,
	results []PrimaryResult,
	first []byte,
) (int64, error) {
	if storage == nil {
		return 0, fmt.Errorf("merge: media segment assembly requires storage")
	}
	dir, err := os.MkdirTemp("", "merc-media-merge-*")
	if err != nil {
		return 0, fmt.Errorf("merge: create segment scratch: %w", err)
	}
	defer os.RemoveAll(dir)

	listPath := filepath.Join(dir, "concat.txt")
	list, err := os.Create(listPath)
	if err != nil {
		return 0, fmt.Errorf("merge: create concat list: %w", err)
	}
	for i, pr := range results {
		mark := verificationMemoryMark(ctx)
		obj := first
		if i != 0 {
			obj, err = readMergeResult(ctx, storage, pr)
			if err != nil {
				_ = list.Close()
				return 0, err
			}
		}
		if err := validateMediaTranscodeResult(obj, resultRecordContract{Exact: 1, Max: 1}); err != nil {
			_ = list.Close()
			return 0, fmt.Errorf("merge: media segment ordinal %d invalid: %w", pr.ChunkIndex, err)
		}
		segPath := filepath.Join(dir, fmt.Sprintf("seg-%d.mp4", pr.ChunkIndex))
		if err := os.WriteFile(segPath, obj, 0o600); err != nil {
			_ = list.Close()
			return 0, fmt.Errorf("merge: write segment ordinal %d: %w", pr.ChunkIndex, err)
		}
		if _, err := fmt.Fprintf(list, "file '%s'\n", segPath); err != nil {
			_ = list.Close()
			return 0, fmt.Errorf("merge: write concat list: %w", err)
		}
		if i != 0 {
			obj = nil
			releaseVerificationMemoryToMark(ctx, mark)
		}
	}
	if err := list.Close(); err != nil {
		return 0, fmt.Errorf("merge: close concat list: %w", err)
	}

	outPath := filepath.Join(dir, "merged.mp4")
	ffmpeg := mediaMergeFFmpegPath()
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c", "copy", "-movflags", "+faststart",
		"-f", "mp4", outPath,
	)
	cmd.Env = []string{} // no supplier environment influence on merge
	if outBytes, runErr := cmd.CombinedOutput(); runErr != nil {
		return 0, fmt.Errorf("merge: ffmpeg concat failed: %w (%s)", runErr, truncateForMergeErr(outBytes))
	}
	merged, err := os.ReadFile(outPath)
	if err != nil {
		return 0, fmt.Errorf("merge: read concatenated media: %w", err)
	}
	if err := validateMediaTranscodeResult(merged, resultRecordContract{Exact: 1, Max: 1}); err != nil {
		return 0, fmt.Errorf("merge: concatenated media invalid: %w", err)
	}
	n, err := out.Write(merged)
	if err == nil && n != len(merged) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return 0, fmt.Errorf("merge: write concatenated media: %w", err)
	}
	return int64(len(results)), nil
}

func mediaMergeFFmpegPath() string {
	if p := os.Getenv("MERC_FFMPEG_PATH"); p != "" {
		return p
	}
	return "ffmpeg"
}

func truncateForMergeErr(b []byte) string {
	const max = 512
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}
