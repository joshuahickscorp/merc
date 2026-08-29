package main

// Device-placement predicate + opt-in host sweep. Cheap tests always
// run. The live Blender/Metal measurement is opt-in only:
//
//	MERC_RENDER_DEVICE_PLACEMENT=1 \
//	  python3 src/render/harness/device_placement_bench.py --write-evidence
//
//	MERC_RENDER_DEVICE_PLACEMENT=1 \
//	  go test -count=1 -run '^TestRenderDevicePlacementBench$' -timeout 180m .
//
// Cycles CPU or Metal. EEVEE is never selected. A GPU request that
// cannot enable a Metal device is a hard failure.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	devicePlacementEnv        = "MERC_RENDER_DEVICE_PLACEMENT"
	devicePlacementHarnessRel = "src/render/harness/device_placement_bench.py"
	devicePlacementPredRel    = "ops/scripts/lib/device_placement.py"
	devicePlacementEntryRel   = "src/render/metal/blender_entry.py"
	devicePlacementDeviceRel  = "src/render/metal/device.py"
)

func TestGPUPlacementLicensePredicate(t *testing.T) {
	// Known lose: 256²/64 light (GPU lane 0.70x cold).
	lose := gpuPlacementLicense(256, 256, 64, 1026, 2, 0, "", true, true, false)
	if lose.Licensed || lose.GPUFaster || lose.Band != "lose" {
		t.Fatalf("256²/64 light must refuse first-assignment: %+v", lose)
	}
	dense := gpuPlacementLicense(256, 256, 64, 81922, 2, 0, "", true, true, false)
	if dense.Licensed || dense.ComplexityClass != "dense" {
		t.Fatalf("256²/64 dense must refuse: %+v", dense)
	}
	inst := gpuPlacementLicense(256, 256, 64, 12, 1024, 0, "", true, true, false)
	if inst.Licensed || inst.ComplexityClass != "instanced" {
		t.Fatalf("256²/64 instanced must refuse: %+v", inst)
	}

	// Known cold-wall wins: light at 1024²/64 (67M); dense/instanced at 512²/64.
	win := gpuPlacementLicense(1024, 1024, 64, 1026, 2, 0, "", true, true, false)
	if !win.Licensed || !win.GPUFaster || win.Band != "win" {
		t.Fatalf("1024²/64 light must license first-assignment: %+v", win)
	}
	if !gpuPlacementLicense(512, 512, 64, 81922, 2, 0, "", true, true, false).Licensed {
		t.Fatal("512²/64 dense must license first-assignment")
	}
	weak := gpuPlacementLicense(512, 512, 64, 1026, 2, 0, "", true, true, false)
	if weak.Licensed || weak.Band != "unknown" {
		t.Fatalf("512²/64 light is 1.10x (below margin) and must refuse: %+v", weak)
	}

	// Quality contract: do not mix.
	mix := gpuPlacementLicense(1024, 1024, 512, 1026, 2, 0, "CPU", true, true, false)
	if mix.Licensed {
		t.Fatal("must refuse GPU on a CPU-contracted project")
	}
	stay := gpuPlacementLicense(256, 256, 64, 1026, 2, 0, "GPU", true, true, false)
	if !stay.Licensed {
		t.Fatal("must stay on GPU for a GPU-contracted project")
	}
	if stay.GPUFaster {
		t.Fatal("stay must not claim a light frame is faster")
	}

	// No Metal / compile-cold.
	if gpuPlacementLicense(1024, 1024, 512, 1026, 2, 0, "", false, true, false).Licensed {
		t.Fatal("no Metal must refuse")
	}
	cold := gpuPlacementLicense(1024, 1024, 512, 1026, 2, 0, "", true, false, false)
	if cold.Licensed || cold.Band != "compile_cold" {
		t.Fatalf("compile-cold must refuse even the heavy cell: %+v", cold)
	}

	// Resident curve: 256²/32 light wins; same cell process-cold does not.
	resWin := gpuPlacementLicense(256, 256, 32, 1026, 2, 0, "", true, true, true)
	if !resWin.Licensed || resWin.Band != "win" {
		t.Fatalf("resident 256²/32 light must license: %+v", resWin)
	}
	resLose := gpuPlacementLicense(256, 256, 16, 1026, 2, 0, "", true, true, true)
	if resLose.Licensed || resLose.Band != "lose" {
		t.Fatalf("resident 256²/16 light must refuse: %+v", resLose)
	}

	// Unknown band is not a license.
	mid := gpuPlacementLicense(256, 256, 256, 1026, 2, 0, "", true, true, false)
	if mid.PixelSamples > devicePlacementLightLosePS && mid.PixelSamples < devicePlacementLightWinPS {
		if mid.Licensed || mid.Band != "unknown" {
			t.Fatalf("unknown band must refuse: %+v", mid)
		}
	}

	if win.SameProductAsCPU {
		t.Fatal("CPU/GPU must not be marked interchangeable")
	}

	// textured outranks dense; dense outranks instanced.
	if placementComplexityClass(81922, 1024, 8_000_000) != "textured" {
		t.Fatal("textured must outrank dense")
	}
	if placementComplexityClass(81922, 1024, 0) != "dense" {
		t.Fatal("dense must outrank instanced")
	}
}

func TestDevicePlacementPythonPredicateSelfTest(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	cmd := exec.Command("python3", filepath.Join(root, devicePlacementPredRel))
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("device_placement self-test: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("self-test ok")) {
		t.Fatalf("device_placement self-test produced no ok line:\n%s", out)
	}
}

func TestDevicePlacementHarnessSelfTest(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	cmd := exec.Command("python3", filepath.Join(root, devicePlacementHarnessRel), "--self-test")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("device-placement harness self-test: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("self-test ok")) {
		t.Fatalf("harness self-test produced no ok line:\n%s", out)
	}
}

func TestDevicePlacementScriptPinsCyclesAndRefusesFallback(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", devicePlacementDeviceRel))
	if err != nil {
		t.Fatalf("read device pin: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, `prefs.compute_device_type = "METAL"`) {
		t.Fatal("device pin must assign compute_device_type METAL")
	}
	if !strings.Contains(src, `scene.cycles.device = "GPU"`) {
		t.Fatal("device pin must assign cycles.device GPU")
	}
	if !strings.Contains(src, "silent CPU fallback") && !strings.Contains(src, "refusing silent") {
		t.Fatal("device pin must refuse a silent CPU fallback")
	}
	entry, err := os.ReadFile(filepath.Join("..", "..", devicePlacementEntryRel))
	if err != nil {
		t.Fatalf("read blender entry: %v", err)
	}
	es := string(entry)
	if !strings.Contains(es, `scene.render.engine = "CYCLES"`) {
		t.Fatal("blender entry must pin CYCLES")
	}
	if strings.Contains(es, `= "BLENDER_EEVEE"`) || strings.Contains(es, `= "BLENDER_EEVEE_NEXT"`) {
		t.Fatal("blender entry must never assign an EEVEE engine")
	}
	if !strings.Contains(es, `"sweep"`) {
		t.Fatal("blender entry must accept sweep mode")
	}
}

func TestRenderDevicePlacementListedAsOptInSkip(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "ops", "scripts", "allowed-test-skips.txt"))
	if err != nil {
		t.Fatalf("read allowed-test-skips: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "TestRenderDevicePlacementBench" {
			if !bytes.Contains(raw, []byte("MERC_RENDER_DEVICE_PLACEMENT")) {
				t.Fatal("allowed-test-skips.txt must name MERC_RENDER_DEVICE_PLACEMENT next to TestRenderDevicePlacementBench")
			}
			return
		}
	}
	t.Fatal("TestRenderDevicePlacementBench must be listed in ops/scripts/allowed-test-skips.txt so make ci does not treat the env gate as an unmarked skip")
}

func TestRenderDevicePlacementBench(t *testing.T) {
	if os.Getenv(devicePlacementEnv) != "1" {
		t.Skip("set MERC_RENDER_DEVICE_PLACEMENT=1 to measure the Cycles CPU vs Metal placement curve")
	}
	if runtime.GOOS != "darwin" {
		t.Fatalf("this bench records a Darwin Blender.app Cycles Metal run; host is %s", runtime.GOOS)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	args := []string{filepath.Join(root, devicePlacementHarnessRel), "--write-evidence", "--print-json"}
	if os.Getenv(devicePlacementEnv+"_EXPENSIVE") == "1" {
		args = append(args, "--include-expensive")
	}
	cmd := exec.Command("python3", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), devicePlacementEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("device-placement bench: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
