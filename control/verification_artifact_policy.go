package main

import (
	"math"
)

const (
	verificationArtifactPolicyFloor int64 = 1 << 20 // envelope + unusually long metadata
	verificationMaxGenerationTokens       = 8192    // agent's current production context ceiling
)

// verificationArtifactMaxBytes derives a finite result cap from the immutable
// task shape already persisted on the job: job type, records-per-task split, and
// the generation token ceiling. The
// multipliers intentionally over-allow the current serializers (including JSON
// escaping) and the result is always clamped by the absolute 256 MiB backstop.
func verificationArtifactMaxBytes(jobType string, splitSize int, maxTokens uint32) int64 {
	return verificationArtifactMaxBytesForRecords(jobType, 0, splitSize, maxTokens)
}

// verificationArtifactMaxBytesForRecords prefers the exact immutable task
// cardinality. legacySplitSize is only a compatibility ceiling for rows created
// before exact per-task counts existed; it must never be presented as exact.
func verificationArtifactMaxBytesForRecords(jobType string, expectedRecords int64, legacySplitSize int, maxTokens uint32) int64 {
	rows := expectedRecords
	if rows <= 0 {
		rows = int64(legacySplitSize)
	}
	if rows <= 0 {
		rows = defaultSplitSize
	}

	var estimate int64
	switch jobType {
	case "embed":
		// Current catalogue embeddings are 384 f32 values. Even pathological
		// JSON float spellings stay well below 16 KiB/row; binary is 1,536 B.
		estimate = verificationArtifactPolicyFloor + saturatingMul(rows, 16<<10)
	case "batch_infer", "json_extraction":
		tokens := int64(maxTokens)
		if tokens <= 0 {
			tokens = defaultQuoteMaxTokens
		}
		if tokens > verificationMaxGenerationTokens {
			tokens = verificationMaxGenerationTokens
		}
		// 128 bytes/token is deliberately generous for decoded UTF-8 plus worst
		// common JSON escaping and per-record envelope fields.
		perRow := saturatingAdd(saturatingMul(tokens, 128), 2<<10)
		estimate = verificationArtifactPolicyFloor + saturatingMul(rows, perRow)
	case "audio_transcribe":
		// Whisper emits <=224 decoded tokens per input clip plus one segment.
		estimate = verificationArtifactPolicyFloor + saturatingMul(rows, 32<<10)
	case "batch_classification":
		// Labels ride inside the already-bounded tagged job descriptor. 64 KiB
		// per output row is deliberately generous even for unusual buyer labels.
		estimate = verificationArtifactPolicyFloor + saturatingMul(rows, 64<<10)
	case "rerank":
		// Rankings are integer indices shaped by each input row. One MiB/row is
		// far above normal top-k output while the absolute cap handles explicit
		// giant splits without arithmetic growth.
		estimate = verificationArtifactPolicyFloor + saturatingMul(rows, 1<<20)
	case "image_gen":
		estimate = 64 << 20
	case "custom", "eval", "lora_finetune":
		// Opaque/checkpoint-like outputs have no smaller trustworthy shape bound.
		// They still cannot exceed the control plane's existing untrusted-body cap.
		estimate = verificationArtifactAbsoluteMaxBytes
	default:
		estimate = 64 << 20
	}
	if estimate < verificationArtifactPolicyFloor {
		estimate = verificationArtifactPolicyFloor
	}
	if estimate > verificationArtifactAbsoluteMaxBytes {
		estimate = verificationArtifactAbsoluteMaxBytes
	}
	return estimate
}

func saturatingMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

func saturatingAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
