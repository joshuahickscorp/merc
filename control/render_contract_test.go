package main

import "testing"

func TestMediaRenderingContractBoundsClosedSceneAndArtifact(t *testing.T) {
	if err := normalizeRenderingJobType(&JobType{Type: "media_rendering", RenderWidth: 64, RenderHeight: 32}); err != nil {
		t.Fatal(err)
	}
	scene := []byte(`{"background":[1,2,3],"rectangles":[{"x":1,"y":2,"width":4,"height":5,"color":[9,8,7]}]}`)
	if err := validateRenderingInputBytes(scene, 64, 32); err != nil {
		t.Fatal(err)
	}
	if _, err := renderingInputScan(scene); err != nil {
		t.Fatal(err)
	}
	artifact := append([]byte("P6\n64 32\n255\n"), make([]byte, 64*32*3)...)
	if err := validateMediaRenderingResult(artifact, resultRecordContract{Exact: 1, Max: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestMediaRenderingContractRejectsUnknownSceneAndWrongArtifact(t *testing.T) {
	if err := validateRenderingInputBytes([]byte(`{"background":[0,0,0],"url":"file:///etc/passwd"}`), 32, 32); err == nil {
		t.Fatal("unknown scene field was accepted")
	}
	if err := validateMediaRenderingResult([]byte("P6\n32 32\n255\n"), resultRecordContract{Exact: 1, Max: 1}); err == nil {
		t.Fatal("truncated PPM was accepted")
	}
}
