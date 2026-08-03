package main

import "testing"

// CUDA admission.
//
// Until 2026-07-27 the control plane admitted only apple_silicon_* classes and
// the candle engine, so an NVIDIA worker was refused at registration with HTTP
// 400. That single fact is what made the realtime lane unsellable and got it
// deleted by [KILL-RT]. RunPod supply reverses the premise; these tests pin the
// admission so it cannot regress silently and cannot become a hole.

func TestCUDAClassesAreAdmitted(t *testing.T) {
	for _, c := range []string{"nvidia_24gb", "nvidia_48gb", "nvidia_80gb", "nvidia_180gb"} {
		if !validHWClasses[c] {
			t.Fatalf("%s is not admitted; an NVIDIA worker would still be refused at registration", c)
		}
	}
	// Apple Silicon must not have been displaced.
	for _, c := range []string{"apple_silicon_base", "apple_silicon_pro",
		"apple_silicon_max", "apple_silicon_ultra"} {
		if !validHWClasses[c] {
			t.Fatalf("%s lost admission; existing supply would be evicted", c)
		}
	}
}

// Every admitted class needs a real sustained-power figure. Without one,
// supplier viability falls back to a default and the economics report is wrong
// in the direction that hides a loss.
func TestEveryAdmittedClassHasAPowerFigure(t *testing.T) {
	for class := range validHWClasses {
		w, ok := sustainedWattsByHWClass[class]
		if !ok || w <= 0 {
			t.Fatalf("hardware class %q is admitted but has no sustained-power figure; "+
				"supplier break-even for it would be computed from a default", class)
		}
	}
	// CUDA draws must be materially above Apple Silicon, or the break-even
	// arithmetic is quietly using the wrong order of magnitude.
	if sustainedWattsByHWClass["nvidia_80gb"] <= sustainedWattsByHWClass["apple_silicon_ultra"] {
		t.Fatal("an 80GB CUDA board is not modelled as drawing more than an M-series Ultra")
	}
}

// An engine must not run on hardware that cannot serve it. A CUDA host claiming
// candle, or an Apple host claiming vllm, routes work to a runtime that will
// fail at execution rather than at registration.
func TestEngineAdmissionIsPairedToHardware(t *testing.T) {
	cases := []struct {
		engine, hw string
		want       bool
	}{
		{"candle", "apple_silicon_pro", true},
		{"candle", "apple_silicon_ultra", true},
		{"vllm", "nvidia_80gb", true},
		{"vllm", "nvidia_24gb", true},
		{"sglang", "nvidia_80gb", true},
		{"tensorrt_llm", "nvidia_48gb", true},
		{"lmdeploy", "nvidia_24gb", true},

		{"vllm", "apple_silicon_pro", false},         // no CUDA on Apple
		{"sglang", "apple_silicon_ultra", false},     // no CUDA on Apple
		{"tensorrt_llm", "apple_silicon_pro", false}, // no CUDA on Apple
		{"lmdeploy", "apple_silicon_max", false},     // no CUDA on Apple
		{"candle", "nvidia_80gb", false},             // candle is not the CUDA path
		{"vllm", "cpu", false},                       // cpu is not an admitted class
		{"tensorrt", "nvidia_80gb", false},           // bare spelling not registered
		{"candle", "sun_sparc", false},               // hardware not admitted
		{"", "", false},                              // empty admits nothing
		{"vllm", "'; DROP TABLE workers;", false},
	}
	for _, c := range cases {
		if got := EngineAdmissibleFor(c.engine, c.hw); got != c.want {
			t.Fatalf("EngineAdmissibleFor(%q, %q) = %v, want %v", c.engine, c.hw, got, c.want)
		}
	}
}

func TestVLLMEngineIsAdmittedButNotDefault(t *testing.T) {
	if !validEngines["vllm"] {
		t.Fatal("vllm is not an admitted engine; RunPod workers cannot register")
	}
	// The default must stay candle: an unspecified engine on existing Apple
	// supply must keep working exactly as before.
	if defaultEngine != "candle" {
		t.Fatalf("default engine changed to %q; existing registrations would flip runtime", defaultEngine)
	}
	if normalizeEngine("") != "candle" {
		t.Fatalf("an empty engine no longer normalises to candle")
	}
	if normalizeEngine("vllm") != "vllm" {
		t.Fatal("normalizeEngine mangled an explicit vllm declaration")
	}
}
