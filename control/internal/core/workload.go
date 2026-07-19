// Package core is the ComputeExchange replacement kernel: one typed command model,
// one state reducer, and one workload registry that the control plane, agent, SDK,
// API adapters, and proof system all derive from. It is built from the behavioral
// oracle (control/oracle/behavior_matrix.json), not ported from the legacy flat
// package, and is the target the old buyer/job/task architecture cuts over to.
package core

// InputForm is how a workload's input is supplied and interpreted.
type InputForm string

const (
	InputJSONL InputForm = "jsonl"     // newline-delimited JSON records (embed, batch_infer, classify, rerank, json_extraction)
	InputAudio InputForm = "audio_wav" // a single WAV object, server-derived duration (audio_transcribe)
)

// PricingUnit is the meter a workload bills on.
type PricingUnit string

const (
	PricePerKToken   PricingUnit = "per_1k_tokens"    // text workloads
	PricePerAudioMin PricingUnit = "per_audio_minute" // audio_transcribe
)

// SplitPolicy governs how a job's input records are partitioned into tasks.
type SplitPolicy string

const (
	SplitByRecords SplitPolicy = "by_records" // chunk JSONL by record count (recommended_split_size)
	SplitWhole     SplitPolicy = "whole"      // one task for the whole input (audio)
)

// MergePolicy governs how per-task result artifacts recombine into the job result.
type MergePolicy string

const (
	MergeConcatJSONL MergePolicy = "concat_jsonl" // ordered concat of per-chunk JSONL
	MergeSingle      MergePolicy = "single"       // one task, one artifact
)

// Workload is the compact authority for one economically-supported job type. Every
// job-type switch (pricing, validation, splitting, result format, model requirement)
// in the legacy tree resolves to one of these instead of a scattered case arm.
type Workload struct {
	Name       string // wire job_type
	Input      InputForm
	Pricing    PricingUnit
	Split      SplitPolicy
	Merge      MergePolicy
	Generative bool // routes through the token/generation path (batch_infer, json_extraction)
	// ModelRequired: the workload cannot run without an advertised model of a matching
	// runtime cell. Every supported workload requires a model today; the field stays
	// explicit so a future model-free workload is representable as data.
	ModelRequired bool
}

// Registry is the complete set of actively-supported, economically-coherent workloads.
// Adding a runtime/model is a data edit here; a workload with no maintained execution
// path must NOT appear (represent those as design contracts, not registry entries).
var Registry = map[string]Workload{
	"embed": {
		Name: "embed", Input: InputJSONL, Pricing: PricePerKToken,
		Split: SplitByRecords, Merge: MergeConcatJSONL, Generative: false, ModelRequired: true,
	},
	"batch_infer": {
		Name: "batch_infer", Input: InputJSONL, Pricing: PricePerKToken,
		Split: SplitByRecords, Merge: MergeConcatJSONL, Generative: true, ModelRequired: true,
	},
	"classify": {
		Name: "classify", Input: InputJSONL, Pricing: PricePerKToken,
		Split: SplitByRecords, Merge: MergeConcatJSONL, Generative: false, ModelRequired: true,
	},
	"rerank": {
		Name: "rerank", Input: InputJSONL, Pricing: PricePerKToken,
		Split: SplitByRecords, Merge: MergeConcatJSONL, Generative: false, ModelRequired: true,
	},
	"json_extraction": {
		Name: "json_extraction", Input: InputJSONL, Pricing: PricePerKToken,
		Split: SplitByRecords, Merge: MergeConcatJSONL, Generative: true, ModelRequired: true,
	},
	"audio_transcribe": {
		Name: "audio_transcribe", Input: InputAudio, Pricing: PricePerAudioMin,
		Split: SplitWhole, Merge: MergeSingle, Generative: true, ModelRequired: true,
	},
}

// Lookup returns the workload for a wire job_type and whether it is supported. A
// false result is the single fail-closed point every surface (native, OpenAI,
// Concierge, agent) consults instead of an ad-hoc `switch job_type`.
func Lookup(jobType string) (Workload, bool) {
	w, ok := Registry[jobType]
	return w, ok
}
