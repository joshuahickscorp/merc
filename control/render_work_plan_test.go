package main

import "testing"

func TestProjectRenderWorkUnitAtUsesStableBoundedLexicalCoordinates(t *testing.T) {
	seed := int64(0)
	render := ProjectIRRendering{
		FrameStart: 10, FrameEnd: 11, Cameras: []string{"z-camera", "a-camera"},
		Width: 32, Height: 16, TileWidth: 16, TileHeight: 16, Samples: 130, Seed: &seed,
	}
	plan, err := deriveProjectRenderWorkPlan(render)
	if err != nil {
		t.Fatal(err)
	}
	if plan.UnitCount != 24 || plan.Ordering != renderWorkPlanOrdering {
		t.Fatalf("render plan = %+v", plan)
	}
	tests := []struct {
		ordinal int64
		frame   int64
		camera  string
		tileX   int64
		sample0 int
		sample1 int
	}{
		{0, 10, "a-camera", 0, 0, 64},
		{1, 10, "a-camera", 0, 64, 128},
		{2, 10, "a-camera", 0, 128, 130},
		{3, 10, "a-camera", 1, 0, 64},
		{6, 10, "z-camera", 0, 0, 64},
		{23, 11, "z-camera", 1, 128, 130},
	}
	for _, test := range tests {
		unit, err := projectRenderWorkUnitAt(render, test.ordinal)
		if err != nil {
			t.Fatalf("ordinal %d: %v", test.ordinal, err)
		}
		if unit.Version != renderWorkPlanVersion || unit.Ordinal != test.ordinal || unit.Frame != test.frame ||
			unit.Camera != test.camera || unit.TileX != test.tileX || unit.TileY != 0 ||
			unit.PixelX != int(test.tileX)*16 || unit.PixelY != 0 || unit.PixelWidth != 16 || unit.PixelHeight != 16 ||
			unit.SampleStart != test.sample0 || unit.SampleEnd != test.sample1 {
			t.Fatalf("ordinal %d unit = %+v", test.ordinal, unit)
		}
	}
	if _, err := projectRenderWorkUnitAt(render, plan.UnitCount); err == nil {
		t.Fatal("work plan admitted an ordinal after its bounded end")
	}
}

func TestProjectRenderWorkUnitAtCoversRaggedEdgeTiles(t *testing.T) {
	seed := int64(1)
	render := ProjectIRRendering{
		FrameStart: 0, FrameEnd: 0, Cameras: []string{"camera"},
		Width: 34, Height: 18, TileWidth: 16, TileHeight: 16, Samples: 1, Seed: &seed,
	}
	unit, err := projectRenderWorkUnitAt(render, 5) // final row, final column
	if err != nil {
		t.Fatal(err)
	}
	if unit.TileX != 2 || unit.TileY != 1 || unit.PixelX != 32 || unit.PixelY != 16 ||
		unit.PixelWidth != 2 || unit.PixelHeight != 2 || unit.SampleStart != 0 || unit.SampleEnd != 1 {
		t.Fatalf("ragged final tile = %+v", unit)
	}
}
