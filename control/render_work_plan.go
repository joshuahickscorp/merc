package main

import (
	"errors"
	"fmt"
	"sort"
)

const (
	renderWorkPlanVersion  = "MERC_RENDER_WORK_PLAN_V1"
	renderSamplesPerUnit   = 64
	renderWorkPlanOrdering = "FRAME_CAMERA_TILE_Y_TILE_X_SAMPLE_PARTITION_LEXICOGRAPHIC_V1"
)

// ProjectRenderWorkUnit is one deterministic, bounded fragment of a declared
// render. It is an IR coordinate, not a task lease: no worker, engine, output,
// verifier, or settlement authority is implied by constructing one.
type ProjectRenderWorkUnit struct {
	Version     string `json:"version"`
	Ordinal     int64  `json:"ordinal"`
	Frame       int64  `json:"frame"`
	Camera      string `json:"camera"`
	TileX       int64  `json:"tile_x"`
	TileY       int64  `json:"tile_y"`
	PixelX      int    `json:"pixel_x"`
	PixelY      int    `json:"pixel_y"`
	PixelWidth  int    `json:"pixel_width"`
	PixelHeight int    `json:"pixel_height"`
	SampleStart int    `json:"sample_start"`
	SampleEnd   int    `json:"sample_end_exclusive"`
}

// deriveProjectRenderWorkPlans creates only a bounded decomposition receipt.
// It intentionally does not choose a node, transfer assets, run an engine, or
// clear money: treating a static plan as any of those would turn an IR helper
// into a false rendering capability.
func deriveProjectRenderWorkPlans(ir *ProjectWorkloadIR) {
	for index := range ir.Steps {
		step := &ir.Steps[index]
		if step.Kind != "media_rendering" || step.Rendering == nil {
			continue
		}
		plan, err := deriveProjectRenderWorkPlan(*step.Rendering)
		if err != nil {
			step.Rendering.WorkPlan = nil
			ir.RefusalReasons = append(ir.RefusalReasons,
				fmt.Sprintf("render work plan for step %s refused: %v", step.ID, err))
			continue
		}
		step.Rendering.WorkPlan = &plan
	}
}

func deriveProjectRenderWorkPlan(render ProjectIRRendering) (ProjectIRRenderWorkPlan, error) {
	if render.Width <= 0 || render.Height <= 0 || render.FrameEnd < render.FrameStart ||
		len(render.Cameras) == 0 || render.Samples <= 0 {
		return ProjectIRRenderWorkPlan{}, errors.New("render declaration has no complete decomposable shape")
	}
	tileWidth, tileHeight := render.Width, render.Height
	if render.TileWidth != 0 {
		tileWidth, tileHeight = render.TileWidth, render.TileHeight
	}
	frameCount := render.FrameEnd - render.FrameStart + 1
	tileColumns := int64((render.Width + tileWidth - 1) / tileWidth)
	tileRows := int64((render.Height + tileHeight - 1) / tileHeight)
	samplePartitions := int64((render.Samples + renderSamplesPerUnit - 1) / renderSamplesPerUnit)
	values := []int64{frameCount, int64(len(render.Cameras)), tileColumns, tileRows, samplePartitions}
	unitCount := int64(1)
	for _, value := range values {
		if value <= 0 || unitCount > projectRenderMaxWorkUnits/value {
			return ProjectIRRenderWorkPlan{}, fmt.Errorf("render decomposition exceeds %d work units", projectRenderMaxWorkUnits)
		}
		unitCount *= value
	}
	return ProjectIRRenderWorkPlan{
		Version: renderWorkPlanVersion, FrameCount: frameCount, CameraCount: int64(len(render.Cameras)),
		TileColumns: tileColumns, TileRows: tileRows, SamplePartitions: samplePartitions,
		UnitCount: unitCount, Ordering: renderWorkPlanOrdering,
	}, nil
}

// projectRenderWorkUnitAt expands a single lexical work ordinal without ever
// materialising the full plan. The exact order is part of the IR contract:
// FRAME → CAMERA → TILE_Y → TILE_X → SAMPLE_PARTITION. Sorting camera labels
// again here makes the mapping stable even for callers that hold a decoded IR
// rather than the already-normalised project declaration.
func projectRenderWorkUnitAt(render ProjectIRRendering, ordinal int64) (ProjectRenderWorkUnit, error) {
	plan, err := deriveProjectRenderWorkPlan(render)
	if err != nil {
		return ProjectRenderWorkUnit{}, err
	}
	if ordinal < 0 || ordinal >= plan.UnitCount {
		return ProjectRenderWorkUnit{}, fmt.Errorf("render work ordinal %d is outside [0,%d)", ordinal, plan.UnitCount)
	}
	cameras := append([]string(nil), render.Cameras...)
	sort.Strings(cameras)
	for i := 1; i < len(cameras); i++ {
		if cameras[i] == cameras[i-1] {
			return ProjectRenderWorkUnit{}, fmt.Errorf("render work unit has duplicate camera %q", cameras[i])
		}
	}
	tileWidth, tileHeight := render.Width, render.Height
	if render.TileWidth != 0 {
		tileWidth, tileHeight = render.TileWidth, render.TileHeight
	}

	remaining := ordinal
	samplePartition := remaining % plan.SamplePartitions
	remaining /= plan.SamplePartitions
	tileX := remaining % plan.TileColumns
	remaining /= plan.TileColumns
	tileY := remaining % plan.TileRows
	remaining /= plan.TileRows
	cameraIndex := remaining % plan.CameraCount
	frameIndex := remaining / plan.CameraCount

	pixelX, pixelY := int(tileX)*tileWidth, int(tileY)*tileHeight
	pixelWidth, pixelHeight := tileWidth, tileHeight
	if remainingWidth := render.Width - pixelX; remainingWidth < pixelWidth {
		pixelWidth = remainingWidth
	}
	if remainingHeight := render.Height - pixelY; remainingHeight < pixelHeight {
		pixelHeight = remainingHeight
	}
	sampleStart := int(samplePartition) * renderSamplesPerUnit
	sampleEnd := sampleStart + renderSamplesPerUnit
	if sampleEnd > render.Samples {
		sampleEnd = render.Samples
	}
	if pixelWidth <= 0 || pixelHeight <= 0 || sampleStart < 0 || sampleEnd <= sampleStart {
		return ProjectRenderWorkUnit{}, errors.New("render work plan produced an invalid bounded unit")
	}
	return ProjectRenderWorkUnit{
		Version: renderWorkPlanVersion, Ordinal: ordinal, Frame: render.FrameStart + frameIndex,
		Camera: cameras[cameraIndex], TileX: tileX, TileY: tileY,
		PixelX: pixelX, PixelY: pixelY, PixelWidth: pixelWidth, PixelHeight: pixelHeight,
		SampleStart: sampleStart, SampleEnd: sampleEnd,
	}, nil
}
