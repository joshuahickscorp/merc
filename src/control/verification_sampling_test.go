package main

import (
	"testing"

	"github.com/google/uuid"
)

func TestVerificationSamplingIsSecretKeyedAndStable(t *testing.T) {
	taskID := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	a := taskSample([]byte("secret-a"), taskID)
	if again := taskSample([]byte("secret-a"), taskID); again != a {
		t.Fatalf("same secret/task must be retry-stable: %v != %v", a, again)
	}
	b := taskSample([]byte("secret-b"), taskID)
	if a == b {
		t.Fatalf("different secrets produced the same sample %v", a)
	}
	if a < 0 || a >= 1 || b < 0 || b >= 1 {
		t.Fatalf("samples must be in [0,1): a=%v b=%v", a, b)
	}
}

func TestVerificationSamplingCannotBeDerivedFromPublicUUIDPrefix(t *testing.T) {
	secret := []byte("server-only-secret")
	a := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	b := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000002")
	if taskSample(secret, a) == taskSample(secret, b) {
		t.Fatal("HMAC sampling must incorporate the full task UUID")
	}
}

func TestVerificationSamplingFailClosedProbabilityBounds(t *testing.T) {
	v := (&Verifier{}).WithSamplingSecret([]byte("test-secret"))
	id := uuid.New()
	if !v.checkSampled(id, 1) {
		t.Fatal("probability 1 must always check")
	}
	if !v.checkSampled(id, 0) || !v.checkSampled(id, -1) {
		t.Fatal("degenerate probabilities must fail closed to checking")
	}
}
