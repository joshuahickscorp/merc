package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Step 25 completes as: every ENABLED elimination class has a production caller
// and a receipt path; every INAPPLICABLE class has a bound refusal; every ABSENT
// class is named absent until substrate exists — "not scheduled as expansion
// theatre".
//
// The enabled and inapplicable halves are already pinned elsewhere: exact reuse
// and coalescing settle through their own billing classes with production
// callers in realtime.go, prefix is held to claiming no savings by
// TestPrefixReuseClaimsNoSavingsUntilItIsAttributed, and tokenization and media
// preprocessing carry DOES_NOT_APPLY in the five-cache audit.
//
// The absent half was named only in plan prose. That is the half most likely to
// rot, because the failure mode is not a broken test — it is someone scaffolding
// an empty cache class and reporting it as coverage. Each class below is pinned
// to the source fact that makes it absent, so the claim dies the moment the
// substrate actually arrives, and a new class cannot be quietly announced
// without this file disagreeing.
//
// Deliberately assertions rather than a register file: the list is short and
// stable, and the assertions ARE the register. A JSON plus a loader would be two
// more things to keep true.
func TestAbsentWorkEliminationClassesStayNamedAbsent(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(raw)
	}

	// Adapters — an identity field, not a residency or routing authority, and
	// LoRA adapter deployment is explicitly non-executable.
	if !strings.Contains(read("exact_reuse.go"), "Adapter       string") {
		t.Fatal("RequestIdentity no longer carries Adapter as a plain identity string; " +
			"if adapters gained residency or routing authority, they are no longer an " +
			"ABSENT elimination class and Step 25's register is stale")
	}
	lora := read("lora_evaluation_receipts.go")
	if !strings.Contains(lora, "EVALUATION_RECORDED_NOT_EXECUTABLE") ||
		!strings.Contains(lora, "no governed trainer, adapter deployment") {
		t.Fatal("LoRA evaluation no longer refuses adapter deployment; adapter work " +
			"elimination may now have substrate, so Step 25 must re-decide it rather " +
			"than keep calling it absent")
	}

	// Render assets — the assembly receipt refuses asset locality outright.
	render := read("render_assembly_receipts.go")
	if !strings.Contains(render, "ASSEMBLY_MANIFEST_VERIFIED_NOT_EXECUTABLE") ||
		!strings.Contains(render, "asset locality") {
		t.Fatal("render assembly no longer refuses asset locality; render assets may now " +
			"have substrate for an elimination class")
	}

	// Container layers and compiled kernels — no control-plane authority at all.
	// A billing class would be the first sign one had been invented, since every
	// enabled class settles through one.
	classes := read("billing_classes.go")
	for _, invented := range []string{
		"ClassContainerLayerReuse",
		"ClassCompiledKernelReuse",
		"ClassDatasetLocality",
		"ClassPreprocessedInput",
		"ClassAdapterResidency",
	} {
		if strings.Contains(classes, invented) {
			t.Fatalf("billing_classes.go declares %s. Step 25 names that class ABSENT, and "+
				"an enabled class must have a production caller AND receipt-backed savings. "+
				"Either the substrate now exists — in which case close the class properly — "+
				"or this is an empty cache class with a billing vocabulary in front of it, "+
				"which is the duplication hazard the step warns about", invented)
		}
	}
}
