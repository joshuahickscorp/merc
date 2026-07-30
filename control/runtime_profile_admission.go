package main

import (
	"fmt"
	"sort"
)

// Validating a worker against the runtime profile it claims.
//
// Before this, the only checks a registering worker faced were
// `projectWorkerRuntimeCapabilities` — which matched on engine and hardware
// class against the flat advertised projection — and the database's
// `engine = 'candle'` string constraint. Neither could say anything about the
// profile's device-count range or its per-cell memory floor relative to the
// cells the worker actually claims to serve.
//
// The device_count range in particular was pure documentation until now: the
// authority declared it, the validator checked it was well-formed, and nothing
// ever compared it to a worker. A profile that says "exactly one device" could
// admit an eight-GPU host, and a future multi-device profile would silently
// admit a single-GPU one.

// runtimeProfileByID finds a registered profile in the embedded authority.
func runtimeProfileByID(runtimeID string) (authorityRuntimeProfile, bool) {
	for _, profile := range runtimeAuthority.Runtimes {
		if profile.RuntimeID == runtimeID {
			return profile, true
		}
	}
	return authorityRuntimeProfile{}, false
}

// routableProfileForEngine returns the single routable profile serving an
// engine. Two routable profiles on one engine is refused by the document
// validator, so finding more than one here means the authority and this code
// disagree, which is worth failing on rather than picking arbitrarily.
func routableProfileForEngine(engine string) (authorityRuntimeProfile, error) {
	var found []authorityRuntimeProfile
	for _, profile := range runtimeAuthority.RoutableRuntimes() {
		if profile.Engine == engine {
			found = append(found, profile)
		}
	}
	switch len(found) {
	case 0:
		return authorityRuntimeProfile{}, fmt.Errorf(
			"engine %q has no routable runtime profile (matrix %s)",
			engine, generatedRuntimeMatrixVersion)
	case 1:
		return found[0], nil
	default:
		return authorityRuntimeProfile{}, fmt.Errorf(
			"engine %q resolves to %d routable runtime profiles", engine, len(found))
	}
}

// workerDeviceCount is the host's declared device count. An agent that predates
// the topology fields is a single-device host, which is exactly what it was
// before the fields existed — the same reading multi_gpu_admission already uses.
func workerDeviceCount(cap WorkerCapability) int {
	if cap.GPUCount <= 0 {
		return 1
	}
	return cap.GPUCount
}

// ValidateWorkerAgainstProfile is the A3 gate. It is deliberately separate from
// projectWorkerRuntimeCapabilities: that answers "which advertised cells does
// this worker project onto", this answers "is this worker allowed to claim this
// profile at all".
//
// Ordering is intentional. Identity and routability are checked before shape,
// so a worker claiming a retired or unknown profile gets told that rather than a
// confusing memory error about cells it was never going to serve.
func ValidateWorkerAgainstProfile(profile authorityRuntimeProfile, cap WorkerCapability) error {
	if !runtimeLifecycleRoutable(profile.Lifecycle) {
		return fmt.Errorf(
			"runtime profile %q is %s and may not receive work",
			profile.RuntimeID, profile.Lifecycle)
	}
	if profile.SupersededBy != "" {
		return fmt.Errorf("runtime profile %q was superseded by %q",
			profile.RuntimeID, profile.SupersededBy)
	}
	if cap.Engine != profile.Engine {
		return fmt.Errorf("worker declares engine %q but profile %q is engine %q",
			cap.Engine, profile.RuntimeID, profile.Engine)
	}

	platformOK := false
	for _, platform := range profile.Hardware.Platforms {
		if platform == cap.HWClass {
			platformOK = true
			break
		}
	}
	if !platformOK {
		sorted := append([]string(nil), profile.Hardware.Platforms...)
		sort.Strings(sorted)
		return fmt.Errorf("hardware class %q is not a platform of runtime profile %q %v",
			cap.HWClass, profile.RuntimeID, sorted)
	}

	devices := workerDeviceCount(cap)
	if devices < profile.Hardware.DeviceCount.Minimum ||
		devices > profile.Hardware.DeviceCount.Maximum {
		return fmt.Errorf(
			"worker declares %d device(s) but runtime profile %q admits %d-%d",
			devices, profile.RuntimeID,
			profile.Hardware.DeviceCount.Minimum, profile.Hardware.DeviceCount.Maximum)
	}

	// Memory is checked against the cells this worker actually claims, not
	// against the profile's largest cell. A worker that only serves embeddings
	// must not be refused for failing to fit a generation model it never
	// advertised.
	claimed := 0
	for _, cell := range profile.Cells {
		if !containsStr(cap.SupportedJobs, cell.Job) || !containsStr(cap.SupportedModels, cell.Model) {
			continue
		}
		claimed++
		if float64(cap.MemoryGB) < cell.MinMemoryGB {
			return fmt.Errorf(
				"worker memory %.3f GB is below the %.3f GB floor of cell %q in profile %q",
				cap.MemoryGB, cell.MinMemoryGB, cell.ID, profile.RuntimeID)
		}
	}
	if claimed == 0 {
		return fmt.Errorf(
			"worker advertises no job/model pair served by runtime profile %q",
			profile.RuntimeID)
	}
	return nil
}

// ResolveWorkerRuntimeProfile picks the governed profile a registering worker
// belongs to and validates it against that profile.
//
// It resolves by ENGINE rather than trusting a worker-supplied profile id. A
// worker naming its own profile would be asserting governed identity from
// outside the governance boundary; the engine it reports is a fact about the
// binary it is running, and the authority decides which profile that engine maps
// to today.
func ResolveWorkerRuntimeProfile(cap WorkerCapability) (authorityRuntimeProfile, error) {
	profile, err := routableProfileForEngine(cap.Engine)
	if err != nil {
		return authorityRuntimeProfile{}, err
	}
	// Through the adapter boundary rather than around it. Today every engine uses
	// the generic adapter and this is the same check either way; the point is
	// that a runtime with genuinely different admission rules changes its own
	// adapter instead of this function.
	adapter, err := AdapterForProfile(profile)
	if err != nil {
		return authorityRuntimeProfile{}, err
	}
	if err := adapter.ValidateWorker(profile, cap); err != nil {
		return authorityRuntimeProfile{}, err
	}
	return profile, nil
}

// governedProfileIdentity returns the (id, revision, digest) triple for the
// single routable profile. Used where a caller needs to stamp a worker row with
// governed identity outside the normal enrollment path.
func governedProfileIdentity(engine string) (id, revision, digest string, err error) {
	profile, err := routableProfileForEngine(engine)
	if err != nil {
		return "", "", "", err
	}
	digest, err = profile.ContentDigest(runtimeAuthorityModels)
	if err != nil {
		return "", "", "", err
	}
	return profile.RuntimeID, profile.Revision, digest, nil
}
