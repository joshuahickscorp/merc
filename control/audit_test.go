package main

import "testing"

func TestLanguageOf(t *testing.T) {
	cases := map[string]string{
		"control/api.go":                  "go",
		"agent/src/runners.rs":            "rust",
		"scripts/spec-lab/foo.py":         "python",
		"macapp/App/StatusModel.swift":    "swift",
		"web/assets/site/three.module.js": "javascript",
		"db/schema.sql":                   "sql",
		"agent/vendor/x/quantized.metal":  "shader",
		"README.md":                       "documentation",
		"proof/5x5-gates.json":            "json",
		"Makefile":                        "config",
		"web/assets/site/oracles@3x.png":  "binary",
	}
	for path, want := range cases {
		if got := languageOf(path); got != want {
			t.Errorf("languageOf(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestSubsystemOf(t *testing.T) {
	cases := map[string]string{
		"control/billing.go":       "payment",
		"control/payout.go":        "payment",
		"control/verification.go":  "verification",
		"control/reputation.go":    "verification",
		"control/api.go":           "control",
		"agent/src/main.rs":        "agent",
		"agent/vendor/x/lib.rs":    "vendored",
		"spec-engine/src/lib.rs":   "speculation",
		"scripts/spec-lab/core.py": "speculation",
		"scripts/five-by-five.py":  "proof",
		"proto/tasks.proto":        "contract",
		"sdk/python/client.py":     "sdk",
		"db/schema.sql":            "store",
		"render/site/oracles.py":   "render",
		"docs/RUNBOOKS.md":         "documentation",
	}
	for path, want := range cases {
		if got := subsystemOf(path); got != want {
			t.Errorf("subsystemOf(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestPythonPlanNoUnknown(t *testing.T) {
	// Every routed path must land in a known bucket (the reclamation invariant).
	valid := map[string]bool{
		"rewrite_go": true, "rewrite_rust": true, "retain_sdk": true,
		"retain_blender_pack": true, "archive_research": true,
		"delete_duplicate": true, "delete_dead": true,
	}
	paths := []string{
		"sdk/python/client.py",
		"scripts/source_fingerprint.py",
		"scripts/spec-lab/cx_transcode_spec_adapter.py",
		"scripts/spec-lab/run_spatial75_cycles_frontier.py",
		"render/site/oracles.py",
		"docker/vllm/serve.py",
		"scripts/cx_logo.py",
	}
	for _, p := range paths {
		got := pythonPlan(p, nil)
		if !valid[got] {
			t.Errorf("pythonPlan(%q) = %q — not a valid bucket (unknown leak)", p, got)
		}
	}
	// bpy content forces the blender pack regardless of path
	if got := pythonPlan("scripts/spec-lab/weird.py", []byte("import bpy\n")); got != "retain_blender_pack" {
		t.Errorf("bpy content should route to retain_blender_pack, got %q", got)
	}
}

func TestCountLOC(t *testing.T) {
	src := []byte("package main\n\n// a comment\nfunc x() {}\n")
	total, code, blank, comment := countLOC(src, "go")
	if total != 4 || code != 2 || blank != 1 || comment != 1 {
		t.Errorf("countLOC go = total %d code %d blank %d comment %d; want 4/2/1/1", total, code, blank, comment)
	}
	py := []byte("import os\n# note\n\nx = 1\n")
	total, code, blank, comment = countLOC(py, "python")
	if total != 4 || code != 2 || blank != 1 || comment != 1 {
		t.Errorf("countLOC python = total %d code %d blank %d comment %d; want 4/2/1/1", total, code, blank, comment)
	}
}

func TestLooksBinary(t *testing.T) {
	if !looksBinary("x.png", nil) {
		t.Error("png extension should be binary")
	}
	if looksBinary("x.go", []byte("package main\n")) {
		t.Error("utf8 go source should not be binary")
	}
	if !looksBinary("x.dat", []byte{0x00, 0x01, 0x02}) {
		t.Error("null bytes should be binary")
	}
}
