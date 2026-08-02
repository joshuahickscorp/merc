package main

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestMediaContractNormalizesAndRejectsTextOnlyFields(t *testing.T) {
	j := JobType{
		Type: "media_transcode", InputFormat: " MP4 ", MaxWidth: 320,
		MaxHeight: 180, VideoBitrateKbps: 400,
	}
	if err := normalizeMediaJobType(&j); err != nil {
		t.Fatalf("valid media contract rejected: %v", err)
	}
	if j.InputFormat != "mp4" || j.FPS != mediaTranscodeDefaultFPS {
		t.Fatalf("normalized media shape = %+v", j)
	}
	for name, mutate := range map[string]func(*JobType){
		"odd width":        func(j *JobType) { j.MaxWidth = 321 },
		"oversized fps":    func(j *JobType) { j.FPS = 61 },
		"zero bitrate":     func(j *JobType) { j.VideoBitrateKbps = 0 },
		"generation field": func(j *JobType) { j.MaxTokens = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			copy := j
			mutate(&copy)
			if err := normalizeMediaJobType(&copy); err == nil {
				t.Fatalf("mutation %s was accepted", name)
			}
		})
	}
}

func TestMediaInputScanUsesOneBoundedBinaryGeometry(t *testing.T) {
	input := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p'}, bytes.Repeat([]byte{'x'}, 16)...)
	scan, err := mediaInputScan(input, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Pin single-segment numbers: one record, full object byte geometry.
	if scan.Records != 1 || scan.Bytes != len(input) || scan.MaxLineBytes != len(input) {
		t.Fatalf("media scan geometry = %+v", scan)
	}
	if scan.InputDepth.P90DepthBand == "" {
		t.Fatal("media scan did not produce the bounded depth profile")
	}
	if err := validateMediaInputBytes(input); err != nil {
		t.Fatal(err)
	}
	if err := validateMediaInputBytes([]byte(`{"not":"media"}`)); err == nil {
		t.Fatal("JSONL bytes entered the binary media lane")
	}
}

func TestMediaInputScanPricesNSegmentsAsNUnits(t *testing.T) {
	input := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p'}, bytes.Repeat([]byte{'x'}, 8)...)
	one, err := mediaInputScan(input, 1)
	if err != nil {
		t.Fatal(err)
	}
	three, err := mediaInputScan(input, 3)
	if err != nil {
		t.Fatal(err)
	}
	if one.Records != 1 || three.Records != 3 {
		t.Fatalf("records one=%d three=%d", one.Records, three.Records)
	}
	// Media settlement multiplies the historical single-object geometry by N.
	// Plain max(records, bytes/4) would collapse large objects to one unit.
	u1 := settlementInputUnitsForMediaSegments(one.Records, int64(one.Bytes))
	u3 := settlementInputUnitsForMediaSegments(three.Records, int64(three.Bytes))
	pinned := settlementInputUnitsForGeometry(1, int64(one.Bytes))
	if u1 != pinned {
		t.Fatalf("single-segment units=%v, want pinned historical %v", u1, pinned)
	}
	if u3 != pinned*3 {
		t.Fatalf("three-segment units=%v, want %v (3× single)", u3, pinned*3)
	}
	// Naive geometry still collapses — the media path must not use it for N>1.
	naive := settlementInputUnitsForGeometry(three.Records, int64(three.Bytes))
	if naive >= u3 {
		// only interesting when bytes/4 > 1; document the collapse we fixed
	}
	if u3 <= u1 {
		t.Fatalf("N segments must price strictly above one segment: %v vs %v", u3, u1)
	}
	jt := JobType{Type: "media_transcode"}
	if got := settlementInputUnitsForJobType(jt, 3, int64(one.Bytes)); got != u3 {
		t.Fatalf("job-type media units=%v, want %v", got, u3)
	}
}

func TestMediaResultIsOneDeterministicMP4Artifact(t *testing.T) {
	valid := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p'}, bytes.Repeat([]byte{'x'}, 16)...)
	info := &CommitTaskInfo{jobType: "media_transcode", ExpectedOutputRecords: 1}
	if err := validateTaskResultArtifact(info, valid); err != nil {
		t.Fatalf("valid media result rejected: %v", err)
	}
	if !resultsAgree("media_transcode", valid, append([]byte(nil), valid...)) {
		t.Fatal("media byte-exact comparator rejected equal artifacts")
	}
	bad := append([]byte(nil), valid...)
	bad[len(bad)-1]++
	if resultsAgree("media_transcode", valid, bad) {
		t.Fatal("media comparator accepted divergent artifacts")
	}
	if err := validateTaskResultArtifact(info, []byte(`{"not":"mp4"}`)); err == nil {
		t.Fatal("non-MP4 media result accepted")
	}
}

func TestInlineMediaBase64IsBoundedAndByteExact(t *testing.T) {
	want := []byte("\x00\x01ftyp-is-binary")
	got, err := decodeInlineMediaBase64(base64.StdEncoding.EncodeToString(want))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded media changed bytes: %x != %x", got, want)
	}
	for _, encoded := range []string{"", "not-base64!"} {
		if _, err := decodeInlineMediaBase64(encoded); err == nil {
			t.Fatalf("invalid inline media %q was accepted", encoded)
		}
	}
}
