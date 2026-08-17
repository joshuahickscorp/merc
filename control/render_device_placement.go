package main

// GPU placement predicate. Checkable before dispatch from properties
// known in advance (resolution, samples, triangle count, instance
// count, texture bytes, project device contract, kernel warmth).
//
// Mirrors scripts/lib/device_placement.py. Keep the floors in sync.
// This is a planner helper, not a price, not a cell identity, and it
// does not touch money / settlement / auth / capability / trust.

// Host this table was derived on: Mac Studio M3 Ultra, Blender 4.2.1
// LTS, Cycles CPU vs Cycles Metal, identical samples/seed/bounces/AgX/
// adaptive-off/denoise-off. Unbound historical snapshot:
// evidence/perf/cycles-device-placement.json. Not bound authority.

const (
	devicePlacementDenseTriFloor     = 50_000
	devicePlacementInstanceFloor     = 256
	devicePlacementTextureByteFloor  = 1_000_000
	devicePlacementWinMargin         = 1.15
	devicePlacementCPUProcessConstS  = 0.40
	devicePlacementGPUCachedProcessS = 0.45
	devicePlacementGPUCompileColdS   = 32.75

	// Process-cold endpoints. Unknown band is refused, not interpolated.
	devicePlacementLightLosePS    = 4_194_304  // 256² × 64
	devicePlacementLightWinPS     = 67_108_864 // 512² × 256 / 1024² × 64
	devicePlacementDenseLosePS    = 4_194_304
	devicePlacementDenseWinPS     = 16_777_216 // 512² × 64
	devicePlacementInstanceLosePS = 4_194_304
	devicePlacementInstanceWinPS  = 16_777_216 // 512² × 64
	devicePlacementTextureLosePS  = 4_194_304
	devicePlacementTextureWinPS   = 536_870_912

	// Resident (warm worker) endpoints from this lane's sweep.
	devicePlacementLightResidentLosePS    = 1_048_576 // 256² × 16
	devicePlacementLightResidentWinPS     = 2_097_152 // 256² × 32
	devicePlacementDenseResidentLosePS    = 2_097_152 // 256² × 32
	devicePlacementDenseResidentWinPS     = 4_194_304 // 256² × 64
	devicePlacementInstanceResidentLosePS = 1_048_576
	devicePlacementInstanceResidentWinPS  = 2_097_152
	devicePlacementTextureResidentLosePS  = 4_194_304
	devicePlacementTextureResidentWinPS   = 536_870_912
)

// GPUPlacementDecision is the planner-visible result of gpuPlacementLicense.
type GPUPlacementDecision struct {
	Licensed                bool
	GPUFaster               bool
	Band                    string
	ComplexityClass         string
	PixelSamples            int
	SameProductAsCPU        bool
	MixRefused              bool
	DeviceIsQualityContract bool
	Reason                  string
}

// gpuPlacementLicense is the checkable-before-dispatch predicate.
//
// projectDevice is "", "CPU", or "GPU". Empty means first assignment.
// licensed=true means the planner may dispatch this frame to Metal.
func gpuPlacementLicense(
	width, height, samples int,
	triangleCount, instanceCount, textureBytes int,
	projectDevice string,
	metalAvailable, kernelsWarm, resident bool,
) GPUPlacementDecision {
	out := GPUPlacementDecision{
		SameProductAsCPU:        false,
		MixRefused:              true,
		DeviceIsQualityContract: true,
	}
	if width < 1 || height < 1 || samples < 1 {
		out.Reason = "REFUSE: width, height, samples must be >= 1"
		return out
	}
	out.PixelSamples = width * height * samples
	out.ComplexityClass = placementComplexityClass(triangleCount, instanceCount, textureBytes)
	loseAt, winAt := placementClassFloors(out.ComplexityClass, resident)

	if projectDevice != "" && projectDevice != "CPU" && projectDevice != "GPU" {
		out.Reason = "REFUSE: project_device must be empty, CPU, or GPU"
		return out
	}
	if !metalAvailable {
		out.Reason = "REFUSE: no Metal device asserted; silent CPU fallback is not a GPU placement"
		return out
	}

	speedBand, gpuFaster, speedReason := predictedGPUFaster(out.PixelSamples, loseAt, winAt, kernelsWarm)
	out.Band = speedBand
	out.GPUFaster = gpuFaster

	if projectDevice == "CPU" {
		out.Reason = "REFUSE mix: project already contracted CPU. CPU and Metal fail L1 PIXEL_EXACT. Device is part of the quality contract; do not silently switch."
		return out
	}
	if projectDevice == "GPU" {
		out.Licensed = true
		out.Reason = "LICENSED stay: project already contracted GPU. Speed band is " + speedBand + " (" + speedReason + "). Do not migrate this frame to CPU to chase a light-work win — that would mix products."
		return out
	}
	if gpuFaster {
		out.Licensed = true
		out.Reason = "LICENSED first-assignment GPU: " + speedReason
		return out
	}
	out.Reason = "REFUSE first-assignment GPU: " + speedReason
	return out
}

func placementComplexityClass(triangleCount, instanceCount, textureBytes int) string {
	if textureBytes >= devicePlacementTextureByteFloor {
		return "textured"
	}
	if triangleCount >= devicePlacementDenseTriFloor {
		return "dense"
	}
	if instanceCount >= devicePlacementInstanceFloor {
		return "instanced"
	}
	return "light"
}

func placementClassFloors(cls string, resident bool) (loseAt, winAt int) {
	if resident {
		switch cls {
		case "dense":
			return devicePlacementDenseResidentLosePS, devicePlacementDenseResidentWinPS
		case "instanced":
			return devicePlacementInstanceResidentLosePS, devicePlacementInstanceResidentWinPS
		case "textured":
			return devicePlacementTextureResidentLosePS, devicePlacementTextureResidentWinPS
		default:
			return devicePlacementLightResidentLosePS, devicePlacementLightResidentWinPS
		}
	}
	switch cls {
	case "dense":
		return devicePlacementDenseLosePS, devicePlacementDenseWinPS
	case "instanced":
		return devicePlacementInstanceLosePS, devicePlacementInstanceWinPS
	case "textured":
		return devicePlacementTextureLosePS, devicePlacementTextureWinPS
	default:
		return devicePlacementLightLosePS, devicePlacementLightWinPS
	}
}

func predictedGPUFaster(ps, loseAt, winAt int, kernelsWarm bool) (band string, faster bool, reason string) {
	if !kernelsWarm {
		return "compile_cold", false, "REFUSE speed: kernels are not warm. First Metal compile on this host is ~32.75s (66 kernels). No single published cell amortises that. Warm a GPU worker, then re-evaluate."
	}
	if ps <= loseAt {
		return "lose", false, "GPU loses on this host at or below the published lose floor for this class."
	}
	if ps >= winAt {
		return "win", true, "GPU wins on this host at or above the published win floor for this class. This raises per-worker speedup; it does not change the serial fraction."
	}
	return "unknown", false, "REFUSE speed: pixel_samples sits between the measured lose and win floors. The planner does not interpolate a license through an unmeasured band."
}
