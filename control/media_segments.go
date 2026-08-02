package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

const (
	mediaSegmentPlanVersion = "MERC_MEDIA_SEGMENT_PLAN_V1"
	mediaSegmentOrdering    = "TIME_ORDINAL_V1"
	// maxMediaSegments bounds decomposition so a mis-set param cannot create
	// an unbounded task fan-out on a single binary object.
	maxMediaSegments = 64
	// mediaSegmentDurationEpsilon is the absolute tolerance when checking that
	// planned extents sum back to the probed source duration.
	mediaSegmentDurationEpsilon = 1e-6
)

// MediaSegmentPlan is the 1-D analogue of ProjectIRRenderWorkPlan: N ordered
// time units covering one source container. It is an IR coordinate system, not
// a task lease.
type MediaSegmentPlan struct {
	Version            string  `json:"version"`
	UnitCount          int64   `json:"unit_count"`
	SourceDurationSecs float64 `json:"source_duration_secs,omitempty"`
	Ordering           string  `json:"ordering"`
}

// MediaSegmentUnit is one deterministic time fragment of a media job. Ordinal
// is the ChunkIndex bijection used by task insert and merge assembly.
type MediaSegmentUnit struct {
	Version   string  `json:"version"`
	Ordinal   int64   `json:"ordinal"`
	UnitCount int64   `json:"unit_count"`
	StartSecs float64 `json:"start_secs"`
	EndSecs   float64 `json:"end_secs_exclusive"`
}

// mediaSegmentCountFromParams returns how many ordered units a media job
// decomposes into. segment_count is preferred; split_size is accepted as an
// alias so buyers already sending JSONL-style split geometry get N tasks.
// Default is 1 (today's single-artifact path).
func mediaSegmentCountFromParams(params json.RawMessage) (int, error) {
	if len(params) == 0 || string(params) == "null" {
		return 1, nil
	}
	var p struct {
		SegmentCount int `json:"segment_count"`
		SplitSize    int `json:"split_size"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return 0, fmt.Errorf("media segment params: %w", err)
	}
	n := p.SegmentCount
	if n <= 0 {
		n = p.SplitSize
	}
	if n <= 0 {
		return 1, nil
	}
	if n > maxMediaSegments {
		return 0, fmt.Errorf("media segment_count %d exceeds bound %d", n, maxMediaSegments)
	}
	return n, nil
}

// deriveMediaSegmentPlan freezes the unit count. Source duration may be zero
// when the control plane has not probed the container yet; extents are then
// derived later from agent-side evidence or an explicit duration.
func deriveMediaSegmentPlan(unitCount int64, sourceDurationSecs float64) (MediaSegmentPlan, error) {
	if unitCount <= 0 || unitCount > maxMediaSegments {
		return MediaSegmentPlan{}, fmt.Errorf("media segment unit count %d is outside [1,%d]", unitCount, maxMediaSegments)
	}
	if sourceDurationSecs < 0 || math.IsNaN(sourceDurationSecs) || math.IsInf(sourceDurationSecs, 0) {
		return MediaSegmentPlan{}, errors.New("media segment plan has a non-finite source duration")
	}
	return MediaSegmentPlan{
		Version:            mediaSegmentPlanVersion,
		UnitCount:          unitCount,
		SourceDurationSecs: sourceDurationSecs,
		Ordering:           mediaSegmentOrdering,
	}, nil
}

// mediaSegmentUnitAt expands one lexical time ordinal without materialising the
// full plan. Extents are half-open [start,end) and the final unit absorbs any
// residual so the set is contiguous and sums to the source duration.
func mediaSegmentUnitAt(plan MediaSegmentPlan, ordinal int64) (MediaSegmentUnit, error) {
	if plan.UnitCount <= 0 || ordinal < 0 || ordinal >= plan.UnitCount {
		return MediaSegmentUnit{}, fmt.Errorf("media segment ordinal %d is outside [0,%d)", ordinal, plan.UnitCount)
	}
	if plan.SourceDurationSecs <= 0 {
		// Duration-unknown plan: equal abstract partitions in [0,1). Callers
		// that need wall-clock extents must supply a probed duration first.
		start := float64(ordinal) / float64(plan.UnitCount)
		end := float64(ordinal+1) / float64(plan.UnitCount)
		return MediaSegmentUnit{
			Version: mediaSegmentPlanVersion, Ordinal: ordinal, UnitCount: plan.UnitCount,
			StartSecs: start, EndSecs: end,
		}, nil
	}
	start := plan.SourceDurationSecs * float64(ordinal) / float64(plan.UnitCount)
	end := plan.SourceDurationSecs * float64(ordinal+1) / float64(plan.UnitCount)
	if ordinal == plan.UnitCount-1 {
		end = plan.SourceDurationSecs
	}
	if end <= start {
		return MediaSegmentUnit{}, fmt.Errorf("media segment ordinal %d has non-positive extent", ordinal)
	}
	return MediaSegmentUnit{
		Version: mediaSegmentPlanVersion, Ordinal: ordinal, UnitCount: plan.UnitCount,
		StartSecs: start, EndSecs: end,
	}, nil
}

// validateMediaSegmentExtents requires planned units to be contiguous from the
// origin, free of gaps/overlaps, and to sum exactly to the source duration.
// This is the inter-unit invariant the per-artifact duration check cannot see.
func validateMediaSegmentExtents(sourceDurationSecs float64, units []MediaSegmentUnit) error {
	if sourceDurationSecs <= 0 || math.IsNaN(sourceDurationSecs) || math.IsInf(sourceDurationSecs, 0) {
		return errors.New("media segment extents need a positive finite source duration")
	}
	if len(units) == 0 {
		return errors.New("media segment extents are empty")
	}
	var sum float64
	expectedStart := 0.0
	for i, u := range units {
		if int64(i) != u.Ordinal {
			return fmt.Errorf("media segment extents are not ordered: index %d has ordinal %d", i, u.Ordinal)
		}
		if u.EndSecs <= u.StartSecs {
			return fmt.Errorf("media segment ordinal %d has non-positive extent", u.Ordinal)
		}
		if math.Abs(u.StartSecs-expectedStart) > mediaSegmentDurationEpsilon {
			return fmt.Errorf("media segment ordinal %d starts at %.9f; expected contiguous start %.9f",
				u.Ordinal, u.StartSecs, expectedStart)
		}
		sum += u.EndSecs - u.StartSecs
		expectedStart = u.EndSecs
	}
	if math.Abs(sum-sourceDurationSecs) > mediaSegmentDurationEpsilon {
		return fmt.Errorf("media segment extents sum to %.9fs; source is %.9fs", sum, sourceDurationSecs)
	}
	if math.Abs(expectedStart-sourceDurationSecs) > mediaSegmentDurationEpsilon {
		return fmt.Errorf("media segment coverage ends at %.9fs; source is %.9fs", expectedStart, sourceDurationSecs)
	}
	return nil
}

// mediaSegmentParamsForDispatch is the agent-visible slice of the plan for one
// claimed ordinal. ChunkIndex is the ordinal bijection.
func mediaSegmentParamsForDispatch(unitCount, ordinal int) (json.RawMessage, error) {
	if unitCount <= 0 {
		return nil, errors.New("media segment dispatch requires a positive unit count")
	}
	if ordinal < 0 || ordinal >= unitCount {
		return nil, fmt.Errorf("media segment ordinal %d is outside [0,%d)", ordinal, unitCount)
	}
	payload := map[string]any{
		"media_segment": map[string]any{
			"version":    mediaSegmentPlanVersion,
			"ordinal":    ordinal,
			"unit_count": unitCount,
			"ordering":   mediaSegmentOrdering,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// refuseSegmentedMediaCrossSupplierRedundancy is the honest policy when a
// multi-unit media or video job cannot be independently byte-verified across
// suppliers: do not sell redundancy that verification cannot settle.
//
// Measured fact: same segment, same engine, twice → byte-equal; cross-supplier
// determinism does not hold. A fabricated perceptual floor would be worse than
// refusing the redundancy product.
func refuseSegmentedMediaCrossSupplierRedundancy(segmentCount int, redundancyFrac float32) error {
	if segmentCount <= 1 {
		return nil
	}
	if redundancyFrac > 0 {
		return errors.New("segmented media refuses cross-supplier redundancy: independent segment outputs are only byte-reproducible under one pinned engine build, not across suppliers")
	}
	return nil
}

// assemblyManifestFromPrimaryResults projects ordered primary merge inputs into
// the existing render-assembly coverage shape so settlement reuses the same
// contiguous-ordinal gate.
func assemblyManifestFromPrimaryResults(results []PrimaryResult) (ProjectRenderAssemblyManifest, error) {
	units := make([]ProjectRenderAssemblyUnit, 0, len(results))
	for i, pr := range results {
		if pr.ChunkIndex != i {
			return ProjectRenderAssemblyManifest{}, fmt.Errorf("merge results are not contiguous ordinals: index %d has chunk_index %d", i, pr.ChunkIndex)
		}
		sha := ""
		if pr.Artifact != nil {
			sha = pr.Artifact.SHA256
		}
		if sha == "" {
			// Merge paths without a sealed artifact still need a digest so the
			// coverage gate can require exactly one success per ordinal.
			sum := sha256.Sum256([]byte(pr.ResultRef))
			sha = hex.EncodeToString(sum[:])
		}
		units = append(units, ProjectRenderAssemblyUnit{
			Ordinal: int64(pr.ChunkIndex), Attempt: 0, Status: "SUCCEEDED", OutputSHA256: sha,
		})
	}
	return ProjectRenderAssemblyManifest{Units: units}, nil
}

// validateMediaMergeCoverage requires every ordinal in [0, unitCount) to have
// exactly one successful primary result. Missing, duplicated, or gapped
// ordinals refuse settlement.
func validateMediaMergeCoverage(unitCount int64, results []PrimaryResult) error {
	if unitCount <= 0 {
		return errors.New("media merge coverage requires a positive unit count")
	}
	manifest, err := assemblyManifestFromPrimaryResults(results)
	if err != nil {
		return err
	}
	_, err = validateOrdinalAssemblyCoverage(unitCount, manifest)
	return err
}
