package main

import (
	"testing"

	"github.com/minio/minio-go/v7"
)

func TestUnknownSizeStreamingUploadUsesBoundedPartMemory(t *testing.T) {
	_, partSize, _, err := minio.OptimalPartInfo(-1, boundedStreamingUploadPartBytes)
	if err != nil {
		t.Fatal(err)
	}
	if partSize != int64(boundedStreamingUploadPartBytes) {
		t.Fatalf("unknown-size upload part = %d, want explicit bound %d", partSize, boundedStreamingUploadPartBytes)
	}
	if partSize >= 64<<20 {
		t.Fatalf("unknown-size upload part = %d, exceeds bounded control-plane envelope", partSize)
	}
}

func TestVerificationMemoryCeilingFitsContainerAndWorstCasePair(t *testing.T) {
	minimum := 2*verificationArtifactAbsoluteMaxBytes + verificationDigestReadBufferBytes
	if verificationArtifactMemoryCeiling < minimum {
		t.Fatalf("verification ceiling = %d, below primary+redundancy minimum %d", verificationArtifactMemoryCeiling, minimum)
	}
	if verificationArtifactMemoryCeiling >= 1<<30 {
		t.Fatalf("verification ceiling = %d, can exhaust the 1 GiB control container", verificationArtifactMemoryCeiling)
	}
}
