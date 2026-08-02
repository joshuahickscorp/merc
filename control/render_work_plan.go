package main

import (
	"errors"
	"fmt"
)

const (
	renderWorkPlanVersion  = "MERC_RENDER_WORK_PLAN_V1"
	renderSamplesPerUnit   = 64
	renderWorkPlanOrdering = "FRAME_CAMERA_TILE_Y_TILE_X_SAMPLE_PARTITION_LEXICOGRAPHIC_V1"
)

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
