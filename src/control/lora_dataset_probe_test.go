package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCanonicalLoRAJSONNormalizesFiniteNumberSpellings(t *testing.T) {
	forms := []string{
		`{"target":"answer","weight":1,"meta":{"score":1.0}}`,
		`{"meta":{"score":1e0},"weight":1.00,"target":"answer"}`,
		`{"weight":1E+0,"target":"answer","meta":{"score":1.000e0}}`,
	}
	var canonical []byte
	for _, form := range forms {
		got, err := canonicalLoRAJSON([]byte(form))
		mustf(t, err, "canonicalize %s: %v", form)
		if canonical == nil {
			canonical = got
		} else if !bytes.Equal(canonical, got) {
			t.Fatalf("numeric spellings produced different canonical records: %s != %s", canonical, got)
		}
	}
	if string(canonical) != `{"meta":{"score":1},"target":"answer","weight":1}` {
		t.Fatalf("canonical record = %s", canonical)
	}
}

func TestLoRAProbeRejectsSemanticNumericDuplicateAcrossTrainingAndHeldOut(t *testing.T) {
	schema := loraDatasetSchema{
		Version: loraDatasetSchemaV1,
		Fields: map[string]string{
			"input": "string", "target": "string", "weight": "number", "meta": "object",
		},
		Required: []string{"input", "target", "weight", "meta"},
	}
	training, err := validateLoRAJSONL([]byte(`{"input":"prompt","target":"completion","weight":1.0,"meta":{"score":1e0}}`), schema, "training set")
	must(t, err)
	heldOut, err := validateLoRAJSONL([]byte(`{"meta":{"score":1.00},"weight":1E+0,"target":"completion","input":"prompt"}`), schema, "held-out set")
	must(t, err)
	for id := range heldOut.rowIDs {
		if _, overlap := training.rowIDs[id]; !overlap {
			t.Fatal("numeric spelling evaded training/held-out canonical overlap detection")
		}
	}

	_, err = validateLoRAJSONL([]byte(
		`{"input":"prompt","target":"completion","weight":1,"meta":{}}`+"\n"+
			`{"input":"prompt","target":"completion","weight":1.0,"meta":{}}`), schema, "training set")
	if err == nil || !strings.Contains(err.Error(), "repeats a canonical record") {
		t.Fatalf("semantic numeric duplicate accepted: %v", err)
	}
}
