package main

import (
	"math"
)

const (
	verificationArtifactPolicyFloor int64 = 1 << 20 // envelope + unusually long metadata
	verificationMaxGenerationTokens       = 8192    // agent's current production context ceiling
)

func verificationArtifactMaxBytes(jobType string, splitSize int, maxTokens uint32) int64 {
	return verificationArtifactMaxBytesForRecords(jobType, 0, splitSize, maxTokens)
}

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
		estimate = verificationArtifactPolicyFloor + saturatingMul(rows, 16<<10)
	case "batch_infer":
		tokens := int64(maxTokens)
		if tokens <= 0 {
			tokens = defaultQuoteMaxTokens
		}
		if tokens > verificationMaxGenerationTokens {
			tokens = verificationMaxGenerationTokens
		}
		perRow := saturatingAdd(saturatingMul(tokens, 128), 2<<10)
		estimate = verificationArtifactPolicyFloor + saturatingMul(rows, perRow)
	default:
		return verificationArtifactPolicyFloor
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
