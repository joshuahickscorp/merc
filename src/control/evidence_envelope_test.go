package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func sampleBatchDigests() batchAcceptBoundDigests {
	return batchAcceptBoundDigests{
		RequestSHA256:     strings.Repeat("a", 64),
		WorkloadSHA256:    strings.Repeat("b", 64),
		PlacementSHA256:   strings.Repeat("c", 64),
		PricingSHA256:     strings.Repeat("d", 64),
		RuntimeSHA256:     strings.Repeat("e", 64),
		TopologySHA256:    strings.Repeat("f", 64),
		ComputePlanSHA256: strings.Repeat("1", 64),
	}
}

func TestEvidenceEnvelopeSealsAndValidates(t *testing.T) {
	jobID := uuid.New()
	env, err := buildBatchAcceptEvidenceEnvelope(jobID, sampleBatchDigests())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := ValidateEvidenceEnvelope(env); err != nil {
		t.Fatalf("validate sealed: %v", err)
	}
	if !validSHA256(env.EnvelopeSHA256) {
		t.Fatalf("missing root digest: %q", env.EnvelopeSHA256)
	}
	if env.Lane != EnvelopeLaneBatch || env.SubjectID != jobID.String() {
		t.Fatalf("lane/subject = %s/%s", env.Lane, env.SubjectID)
	}
	if len(env.Links) != len(evidenceEnvelopeChainOrder) {
		t.Fatalf("link count %d want %d", len(env.Links), len(evidenceEnvelopeChainOrder))
	}
}

func TestEvidenceEnvelopeTamperRejection(t *testing.T) {
	env, err := buildBatchAcceptEvidenceEnvelope(uuid.New(), sampleBatchDigests())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Mutate a bound digest; the sealed root no longer matches.
	pricing, ok := env.linkByKind(EnvelopeLinkPricing)
	if !ok || pricing.Status != EnvelopeLinkBound {
		t.Fatalf("pricing link not bound: %+v", pricing)
	}
	for i := range env.Links {
		if env.Links[i].Kind == EnvelopeLinkPricing {
			env.Links[i].Digest = strings.Repeat("f", 64)
		}
	}

	// Failing-before: with the tamper check neutralised, a mutated envelope
	// incorrectly passes structural validation alone.
	prev := evidenceEnvelopeTamperCheck
	evidenceEnvelopeTamperCheck = false
	t.Cleanup(func() { evidenceEnvelopeTamperCheck = prev })
	if err := ValidateEvidenceEnvelope(env); err != nil {
		t.Fatalf("neutralised tamper check should accept structure-only: %v", err)
	}

	// Passing-after: restore the check; mutation must fail the root digest.
	evidenceEnvelopeTamperCheck = true
	err = ValidateEvidenceEnvelope(env)
	if err == nil {
		t.Fatal("tampered bound digest must fail envelope digest")
	}
	if !strings.Contains(err.Error(), "digest mismatch") &&
		!strings.Contains(err.Error(), errEvidenceEnvelopeTampered.Error()) {
		t.Fatalf("want tamper error, got %v", err)
	}

	// Mutating any other bound link also fails.
	env2, err := buildBatchAcceptEvidenceEnvelope(uuid.New(), sampleBatchDigests())
	if err != nil {
		t.Fatalf("build2: %v", err)
	}
	for i := range env2.Links {
		if env2.Links[i].Kind == EnvelopeLinkWorkload {
			env2.Links[i].Digest = strings.Repeat("0", 64)
		}
	}
	if err := ValidateEvidenceEnvelope(env2); err == nil {
		t.Fatal("tampered workload digest must fail envelope digest")
	}
}

func TestEvidenceEnvelopeAbsentLinksAreLegibleAndNotBreaks(t *testing.T) {
	env, err := buildBatchAcceptEvidenceEnvelope(uuid.New(), sampleBatchDigests())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Authority types that do not exist yet as accept-transaction objects.
	// RuntimeDecision is BOUND at batch accept (Step 8); Market remains ABSENT.
	for _, kind := range []string{
		EnvelopeLinkMarket,
		EnvelopeLinkVerification, EnvelopeLinkSettlement,
	} {
		link, ok := env.linkByKind(kind)
		if !ok {
			t.Fatalf("missing link %s", kind)
		}
		if link.Status != EnvelopeLinkAbsent {
			t.Fatalf("%s status=%s want ABSENT", kind, link.Status)
		}
		if link.Reason == "" {
			t.Fatalf("%s ABSENT without reason", kind)
		}
		if link.Digest != "" {
			t.Fatalf("%s ABSENT carried fabricated digest %q", kind, link.Digest)
		}
		if link.Authority != "" {
			t.Fatalf("%s ABSENT named authority %q", kind, link.Authority)
		}
	}
	// Lifecycle stages not yet written at accept.
	for _, kind := range []string{EnvelopeLinkExecution, EnvelopeLinkReceipt} {
		link, ok := env.linkByKind(kind)
		if !ok {
			t.Fatalf("missing link %s", kind)
		}
		if link.Status != EnvelopeLinkPending {
			t.Fatalf("%s status=%s want PENDING", kind, link.Status)
		}
		if link.Reason == "" || link.Digest != "" {
			t.Fatalf("%s pending shape: %+v", kind, link)
		}
	}
	// Bound links cite real authorities (RuntimeDecision Step 8, Topology Step 10).
	for _, tc := range []struct {
		kind, authority string
	}{
		{EnvelopeLinkRequest, "SubmitRequest"},
		{EnvelopeLinkWorkload, "WorkloadDecision"},
		{EnvelopeLinkPricing, "PricingDecision"},
		{EnvelopeLinkRuntime, "RuntimeDecision"},
		{EnvelopeLinkPlacement, "PlacementRequirement"},
		{EnvelopeLinkTopology, "TopologyDecision"},
	} {
		link, ok := env.linkByKind(tc.kind)
		if !ok || link.Status != EnvelopeLinkBound || link.Authority != tc.authority {
			t.Fatalf("%s not bound to %s: %+v", tc.kind, tc.authority, link)
		}
		if !validSHA256(link.Digest) {
			t.Fatalf("%s bad digest", tc.kind)
		}
	}

	// A break is a validation failure, not an ABSENT row: empty reason on
	// ABSENT fails; empty digest on BOUND fails.
	brokenAbsent := env
	brokenAbsent.Links = append([]EvidenceEnvelopeLink(nil), env.Links...)
	for i := range brokenAbsent.Links {
		if brokenAbsent.Links[i].Kind == EnvelopeLinkMarket {
			brokenAbsent.Links[i].Reason = ""
		}
	}
	// Re-seal so structure is checked with empty reason.
	brokenAbsent.EnvelopeSHA256 = ""
	if err := validateEvidenceEnvelopeStructure(brokenAbsent); err == nil {
		t.Fatal("ABSENT without reason must be a break, not a silent partial")
	}
	brokenBound := env
	brokenBound.Links = append([]EvidenceEnvelopeLink(nil), env.Links...)
	for i := range brokenBound.Links {
		if brokenBound.Links[i].Kind == EnvelopeLinkPricing {
			brokenBound.Links[i].Digest = ""
		}
	}
	brokenBound.EnvelopeSHA256 = ""
	if err := validateEvidenceEnvelopeStructure(brokenBound); err == nil {
		t.Fatal("BOUND without digest must be a break")
	}

	// Fabricated digest on ABSENT is refused.
	forged := env
	forged.Links = append([]EvidenceEnvelopeLink(nil), env.Links...)
	for i := range forged.Links {
		if forged.Links[i].Kind == EnvelopeLinkMarket {
			forged.Links[i].Digest = strings.Repeat("1", 64)
		}
	}
	forged.EnvelopeSHA256 = ""
	if err := validateEvidenceEnvelopeStructure(forged); err == nil {
		t.Fatal("ABSENT with fabricated digest must be refused")
	}
}

func TestEvidenceEnvelopeDoesNotRehashBoundBodies(t *testing.T) {
	// The envelope binds caller-supplied digests exactly — it does not
	// recompute alternate digests of decision bodies.
	d := sampleBatchDigests()
	env, err := buildBatchAcceptEvidenceEnvelope(uuid.New(), d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for kind, want := range map[string]string{
		EnvelopeLinkWorkload:  d.WorkloadSHA256,
		EnvelopeLinkPlacement: d.PlacementSHA256,
		EnvelopeLinkPricing:   d.PricingSHA256,
		EnvelopeLinkRuntime:   d.RuntimeSHA256,
		EnvelopeLinkTopology:  d.TopologySHA256,
		EnvelopeLinkRequest:   d.RequestSHA256,
	} {
		link, ok := env.linkByKind(kind)
		if !ok || link.Digest != want {
			t.Fatalf("%s digest %q want %q (must cite existing, not recompute)",
				kind, link.Digest, want)
		}
	}
}

func TestEvidenceEnvelopePendingRequestWhenNoSubmitDigest(t *testing.T) {
	d := sampleBatchDigests()
	d.RequestSHA256 = ""
	env, err := buildBatchAcceptEvidenceEnvelope(uuid.New(), d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	link, ok := env.linkByKind(EnvelopeLinkRequest)
	if !ok || link.Status != EnvelopeLinkPending {
		t.Fatalf("request without digest should be PENDING, got %+v", link)
	}
	if link.Reason == "" {
		t.Fatal("pending request missing reason")
	}
}

func TestEvidenceEnvelopeRefusesMissingRequiredBoundDigest(t *testing.T) {
	d := sampleBatchDigests()
	d.PricingSHA256 = ""
	if _, err := buildBatchAcceptEvidenceEnvelope(uuid.New(), d); err == nil {
		t.Fatal("empty pricing digest at accept must refuse envelope")
	}
}
