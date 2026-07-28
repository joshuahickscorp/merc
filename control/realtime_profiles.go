package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

type GenerationPolicy struct {
	Version             string  `json:"version"`
	Temperature         float64 `json:"temperature"`
	TopP                float64 `json:"top_p"`
	MaximumOutputTokens int     `json:"maximum_output_tokens"`
}

type VLLMRuntimeProfile struct {
	SchemaVersion                  int              `json:"schema_version"`
	RuntimeProfileID               string           `json:"runtime_profile_id"`
	Engine                         string           `json:"engine"`
	EngineVersion                  string           `json:"engine_version"`
	ContainerImage                 string           `json:"container_image"`
	ContainerPlatform              string           `json:"container_platform"`
	ContainerDigest                string           `json:"container_digest"`
	ModelAlias                     string           `json:"model_alias"`
	ModelRepository                string           `json:"model_repository"`
	ModelRevision                  string           `json:"model_revision"`
	TokenizerRevision              string           `json:"tokenizer_revision"`
	Architecture                   string           `json:"architecture"`
	DType                          string           `json:"dtype"`
	ModelWeightsGB                 float64          `json:"model_weights_gb,omitempty"`
	PerRankOverheadGB              float64          `json:"per_rank_overhead_gb,omitempty"`
	AttentionHeads                 int              `json:"attention_heads,omitempty"`
	TensorParallelSize             int              `json:"tensor_parallel_size"`
	PipelineParallelSize           int              `json:"pipeline_parallel_size"`
	MaxModelLength                 int              `json:"max_model_length"`
	GPUMemoryUtilization           float64          `json:"gpu_memory_utilization"`
	PrefixCaching                  bool             `json:"prefix_caching"`
	ChunkedPrefill                 bool             `json:"chunked_prefill"`
	GenerationPolicy               GenerationPolicy `json:"generation_policy"`
	BuyerInputUSDPerMillionTokens  float64          `json:"buyer_input_usd_per_million_tokens"`
	BuyerOutputUSDPerMillionTokens float64          `json:"buyer_output_usd_per_million_tokens"`
	BenchmarkStatus                string           `json:"benchmark_status"`
	ProfileSHA256                  string           `json:"profile_sha256"`
}

func (p VLLMRuntimeProfile) hasPlacementRequirements() bool {
	return p.ModelWeightsGB > 0 || p.PerRankOverheadGB > 0 || p.AttentionHeads > 0
}

var (
	exactSHA256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	exactCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

//go:embed runtime-profiles/*.json
var vllmProfileFiles embed.FS

var vllmRuntimeProfiles = mustLoadVLLMRuntimeProfiles()

func validateVLLMRuntimeProfile(p VLLMRuntimeProfile) error {
	if p.SchemaVersion != 1 {
		return fmt.Errorf("schema_version must be 1")
	}
	if strings.TrimSpace(p.RuntimeProfileID) == "" || strings.TrimSpace(p.ModelAlias) == "" {
		return fmt.Errorf("runtime_profile_id and model_alias are required")
	}
	if p.Engine != "vllm" || strings.TrimSpace(p.EngineVersion) == "" {
		return fmt.Errorf("engine must be pinned vllm")
	}
	if strings.Contains(p.ContainerImage, ":latest") || !exactSHA256Pattern.MatchString(p.ContainerDigest) {
		return fmt.Errorf("container image must have an immutable sha256 digest and must not use latest")
	}
	if p.ContainerPlatform != "linux/amd64" {
		return fmt.Errorf("container_platform must match the pinned linux/amd64 manifest")
	}
	if strings.TrimSpace(p.ModelRepository) == "" || !exactCommitPattern.MatchString(p.ModelRevision) || !exactCommitPattern.MatchString(p.TokenizerRevision) {
		return fmt.Errorf("model and tokenizer must use exact 40-character revisions")
	}
	if strings.TrimSpace(p.Architecture) == "" || strings.TrimSpace(p.DType) == "" {
		return fmt.Errorf("architecture and dtype are required")
	}
	if p.TensorParallelSize < 1 || p.PipelineParallelSize < 1 || p.MaxModelLength < 1 {
		return fmt.Errorf("parallel sizes and max_model_length must be positive")
	}
	if p.hasPlacementRequirements() {
		if err := validateModelPlacement(modelPlacement{
			ModelID: p.ModelAlias, WeightsGB: p.ModelWeightsGB,
			PerRankOverheadGB: p.PerRankOverheadGB, AttentionHeads: p.AttentionHeads,
		}); err != nil {
			return err
		}
	}
	if p.TensorParallelSize > 1 && !p.hasPlacementRequirements() {
		return fmt.Errorf("multi-GPU profiles require model placement authority")
	}
	if p.GPUMemoryUtilization <= 0 || p.GPUMemoryUtilization > 1 {
		return fmt.Errorf("gpu_memory_utilization must be in (0,1]")
	}
	if strings.TrimSpace(p.GenerationPolicy.Version) == "" || p.GenerationPolicy.MaximumOutputTokens < 1 {
		return fmt.Errorf("generation policy must be versioned and bounded")
	}
	if p.BuyerInputUSDPerMillionTokens <= 0 || p.BuyerOutputUSDPerMillionTokens <= 0 {
		return fmt.Errorf("buyer token prices must be positive")
	}
	if p.BenchmarkStatus != "UNPROVEN" && p.BenchmarkStatus != "PASSED" {
		return fmt.Errorf("benchmark_status must be UNPROVEN or PASSED")
	}
	return nil
}

func mustLoadVLLMRuntimeProfiles() map[string]VLLMRuntimeProfile {
	entries, err := fs.Glob(vllmProfileFiles, "runtime-profiles/*.json")
	if err != nil || len(entries) == 0 {
		panic("vLLM runtime profile catalog is empty or unreadable")
	}
	profiles := make(map[string]VLLMRuntimeProfile, len(entries))
	aliases := make(map[string]string, len(entries))
	for _, name := range entries {
		raw, err := vllmProfileFiles.ReadFile(name)
		if err != nil {
			panic(fmt.Sprintf("read vLLM runtime profile %s: %v", name, err))
		}
		var profile VLLMRuntimeProfile
		if err := decodeStrictJSONObject(raw, &profile); err != nil {
			panic(fmt.Sprintf("decode vLLM runtime profile %s: %v", name, err))
		}
		if err := validateVLLMRuntimeProfile(profile); err != nil {
			panic(fmt.Sprintf("validate vLLM runtime profile %s: %v", name, err))
		}
		digest := sha256.Sum256(raw)
		profile.ProfileSHA256 = hex.EncodeToString(digest[:])
		if _, exists := profiles[profile.RuntimeProfileID]; exists {
			panic("duplicate vLLM runtime profile id " + profile.RuntimeProfileID)
		}
		if prior, exists := aliases[profile.ModelAlias]; exists {
			panic(fmt.Sprintf("model alias %s is ambiguous between %s and %s", profile.ModelAlias, prior, profile.RuntimeProfileID))
		}
		profiles[profile.RuntimeProfileID] = profile
		aliases[profile.ModelAlias] = profile.RuntimeProfileID
	}
	return profiles
}

func vllmProfileByID(id string) (VLLMRuntimeProfile, bool) {
	p, ok := vllmRuntimeProfiles[id]
	return p, ok
}

func vllmProfileForModel(alias string) (VLLMRuntimeProfile, bool) {
	for _, p := range vllmRuntimeProfiles {
		if p.ModelAlias == alias {
			return p, true
		}
	}
	return VLLMRuntimeProfile{}, false
}

func sortedVLLMProfiles() []VLLMRuntimeProfile {
	out := make([]VLLMRuntimeProfile, 0, len(vllmRuntimeProfiles))
	for _, p := range vllmRuntimeProfiles {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuntimeProfileID < out[j].RuntimeProfileID })
	return out
}

func canonicalJSON(v any) ([]byte, error) {
	// encoding/json deterministically sorts string-keyed maps and preserves
	// json.Number text. Avoid a float64 round trip: OpenAI seeds and other
	// integer fields may legitimately exceed JavaScript's exact range.
	return json.Marshal(v)
}
