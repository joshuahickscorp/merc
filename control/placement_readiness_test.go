package main

import (
	"strings"
	"testing"
)

// The CUDA embed cell exists so a heterogeneous placement measurement can name
// a matched identity (same model, same job, cosine) without inventing one at
// spend time. It must remain non-routable and non-advertised: identity, not
// capability.
const cudaEmbedCellID = "vllm-cuda-minilm-embed"

func TestCUDAEmbedCellIsMatchedIdentityNotProduct(t *testing.T) {
	doc := mutableAuthority(t)
	idx := runtimeIndex(t, doc, "vllm_cuda")
	profile := doc.Runtimes[idx]

	var cell *authorityCell
	for i := range profile.Cells {
		if profile.Cells[i].ID == cudaEmbedCellID {
			cell = &profile.Cells[i]
			break
		}
	}
	if cell == nil {
		t.Fatalf("vllm_cuda does not declare %s — placement readiness needs the matched identity before spend",
			cudaEmbedCellID)
	}

	if cell.Job != "embed" || cell.Model != "all-minilm-l6-v2" {
		t.Fatalf("%s job/model = %s/%s, want embed/all-minilm-l6-v2",
			cudaEmbedCellID, cell.Job, cell.Model)
	}
	if cell.Verification != "cosine" {
		t.Fatalf("%s verification = %q, want cosine (matched Metal contract)",
			cudaEmbedCellID, cell.Verification)
	}
	if wireKindFor(*cell, "hf") != "hf" {
		t.Fatalf("%s wire_kind resolves to %q, want hf (third arm on dual-wire Metal pair)",
			cudaEmbedCellID, cell.WireKind)
	}
	if !cell.CloudBacked {
		t.Fatalf("%s must be cloud_backed: true (RunPod-billed arm)", cudaEmbedCellID)
	}

	// Metal arms that define the contract this cell must match.
	_, candle := cellByID(t, "candle-metal-minilm-embed")
	_, llama := cellByID(t, "llama-cpp-metal-minilm-embed")
	for _, peer := range []authorityCell{candle, llama} {
		if peer.Job != cell.Job || peer.Model != cell.Model || peer.Verification != cell.Verification {
			t.Fatalf("peer %s is %s/%s/%s; CUDA cell is %s/%s/%s — not a matched identity",
				peer.ID, peer.Job, peer.Model, peer.Verification,
				cell.Job, cell.Model, cell.Verification)
		}
	}

	effective := cell.EffectiveLifecycle(profile)
	if effective != runtimeLifecycleDraft {
		t.Fatalf("%s effective lifecycle = %s, want DRAFT (identity, not promotion)",
			cudaEmbedCellID, effective)
	}
	if cell.Routable(profile) {
		t.Fatalf("%s is Routable under its profile; identity work must not widen ordinary admission",
			cudaEmbedCellID)
	}
	if cellIsAdvertised(cudaEmbedCellID) {
		t.Fatalf("%s is advertised; buyers must not see a DRAFT CUDA embed cell", cudaEmbedCellID)
	}
	if cellIsDirected(cudaEmbedCellID) {
		t.Fatalf("%s is directed-reachable; DRAFT must not open the operator escape hatch either",
			cudaEmbedCellID)
	}

	// Artifact pin: HF safetensors for all-minilm-l6-v2 (same file candle loads).
	models := doc.authorityModels()
	model, ok := models[cell.Model]
	if !ok {
		t.Fatalf("model %s missing from authority", cell.Model)
	}
	arts := model.artifactsFor("hf")
	if len(arts) == 0 {
		t.Fatal("all-minilm-l6-v2 declares no hf artifacts for the CUDA arm to pin")
	}
	var safetensors string
	for _, a := range arts {
		if strings.HasSuffix(a.Path, "model.safetensors") || a.Path == "model.safetensors" {
			safetensors = a.SHA256
			break
		}
	}
	const want = "53aa51172d142c89d9012cce15ae4d6cc0ca6895895114379cacb4fab128d9db"
	if safetensors != want {
		t.Fatalf("CUDA arm model.safetensors digest = %q, want pinned %q", safetensors, want)
	}
}

// vllm-cuda-llama1-infer remains the wrong contract for placement: batch_infer +
// byte_exact + different model. The readiness programme exists because that
// cell cannot stand in for a matched embed measurement.
func TestExistingCUDAInferCellIsNotAMatchedEmbedArm(t *testing.T) {
	_, infer := cellByID(t, "vllm-cuda-llama1-infer")
	if infer.Job == "embed" && infer.Model == "all-minilm-l6-v2" && infer.Verification == "cosine" {
		t.Fatal("vllm-cuda-llama1-infer unexpectedly matches the embed contract; " +
			"the placement readiness identity cell would be redundant")
	}
	if infer.Job != "batch_infer" || infer.Model != "llama-3.2-1b-instruct-q4" {
		t.Fatalf("vllm-cuda-llama1-infer drifted: job/model=%s/%s", infer.Job, infer.Model)
	}
}
