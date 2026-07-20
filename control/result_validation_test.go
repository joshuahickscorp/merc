package main

import (
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestValidateTaskResultArtifactContracts(t *testing.T) {
	valid := []struct {
		name    string
		jobType string
		body    []byte
	}{
		{"embed-json", "embed", []byte(`{"job_type":"embed","model":"m","dim":2,"count":1,"vectors":[[1,0]]}`)},
		{"embed-cxem", "embed", binaryEmbeddingForValidation(2, [][]float32{{1, 0}})},
		{"batch-infer", "batch_infer", []byte(`{"job_type":"batch_infer","model":"m","completions":[{"index":0,"text":"ok","tokens":1}]}`)},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			info := &CommitTaskInfo{jobType: tc.jobType, SplitSize: 4}
			if err := validateTaskResultArtifact(info, tc.body); err != nil {
				t.Fatalf("valid %s artifact rejected: %v", tc.jobType, err)
			}
		})
	}

	invalid := []struct {
		name    string
		jobType string
		model   string
		body    []byte
	}{
		{"empty", "embed", "", nil},
		{"embed-empty", "embed", "", []byte(`{"job_type":"embed","model":"m","dim":2,"count":0,"vectors":[]}`)},
		{"embed-ragged", "embed", "", []byte(`{"job_type":"embed","model":"m","dim":2,"count":2,"vectors":[[1,0],[1]]}`)},
		{"embed-known-model-wrong-dim", "embed", "all-minilm-l6-v2", []byte(`{"job_type":"embed","model":"all-minilm-l6-v2","dim":2,"count":1,"vectors":[[1,0]]}`)},
		{"embed-overflow-number", "embed", "", []byte(`{"job_type":"embed","model":"m","dim":1,"count":1,"vectors":[[1e999]]}`)},
		{"embed-duplicate-key", "embed", "", []byte(`{"job_type":"embed","model":"m","dim":2,"count":1,"count":2,"vectors":[[1,0]]}`)},
		{"batch-index-gap", "batch_infer", "", []byte(`{"job_type":"batch_infer","model":"m","completions":[{"index":1,"text":"x","tokens":1}]}`)},
		{"removed-workload", "unsupported", "", []byte(`{"value":[]}`)},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			info := &CommitTaskInfo{jobType: tc.jobType, ModelRef: tc.model, SplitSize: 4}
			err := validateTaskResultArtifact(info, tc.body)
			if !errors.Is(err, ErrResultArtifactInvalid) {
				t.Fatalf("invalid artifact error = %v, want typed %v", err, ErrResultArtifactInvalid)
			}
			var typed *ResultArtifactValidationError
			if !errors.As(err, &typed) || typed.Code == "" || typed.JobType != tc.jobType {
				t.Fatalf("invalid artifact did not carry stable type/code: %#v", typed)
			}
		})
	}
}

func TestInvalidResultVerificationDecisionIsDeterministicAndNonPayable(t *testing.T) {
	info := &CommitTaskInfo{
		TaskID: uuid.New(), JobID: uuid.New(), SupplierID: uuid.New(),
		Attempt: 7, jobType: "batch_infer",
	}
	err := validateTaskResultArtifact(info, nil)
	first := invalidResultVerificationDecision(info, err)
	second := invalidResultVerificationDecision(info, err)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same invalid attempt produced different decisions:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if first.Outcome != OutcomeFail || first.Failure == nil ||
		first.Failure.Kind != "artifact_invalid" || first.Failure.Code != resultValidationEmpty ||
		first.Failure.JobType != info.jobType {
		t.Fatalf("typed failure = %#v", first)
	}
	wantKinds := []VerificationEffectKind{
		VerificationEffectDockReputation,
		VerificationEffectRecordEvent,
		VerificationEffectClawbackCredit,
		VerificationEffectQuarantine,
		VerificationEffectRequeue,
	}
	if len(first.Effects) != len(wantKinds) {
		t.Fatalf("effects = %#v", first.Effects)
	}
	for i, effect := range first.Effects {
		if effect.Kind != wantKinds[i] || effect.ID == uuid.Nil {
			t.Fatalf("effect %d = %#v, want kind %q and canonical id", i, effect, wantKinds[i])
		}
	}
	if first.Effects[0].ReputationEvent != EventResultCorrupt || first.Effects[1].EventKind != "artifact_invalid" {
		t.Fatalf("invalid artifact was not typed as result_corrupt/artifact_invalid: %#v", first.Effects)
	}
}

func TestExactTaskCardinalityRejectsShortRecordShapedResults(t *testing.T) {
	cases := []struct {
		jobType string
		body    []byte
	}{
		{"embed", []byte(`{"job_type":"embed","model":"m","dim":2,"count":1,"vectors":[[1,0]]}`)},
		{"batch_infer", []byte(`{"job_type":"batch_infer","model":"m","completions":[{"index":0,"text":"ok","tokens":1}]}`)},
	}
	for _, tc := range cases {
		t.Run(tc.jobType, func(t *testing.T) {
			info := &CommitTaskInfo{jobType: tc.jobType, SplitSize: 1000, ExpectedOutputRecords: 2}
			err := validateTaskResultArtifact(info, tc.body)
			var typed *ResultArtifactValidationError
			if !errors.As(err, &typed) || typed.Code != resultValidationCount {
				t.Fatalf("one-row artifact for exact two-row task = %v (%#v), want invalid_count", err, typed)
			}
			decision := invalidResultVerificationDecision(info, err)
			if decision.Outcome != OutcomeFail || decision.Failure == nil ||
				decision.Failure.Code != resultValidationCount {
				t.Fatalf("short artifact did not become a deterministic non-payable failure: %#v", decision)
			}

			info.ExpectedOutputRecords = 0
			info.SplitSize = 2
			if err := validateTaskResultArtifact(info, tc.body); err != nil {
				t.Fatalf("explicit legacy cardinality compatibility rejected valid bounded artifact: %v", err)
			}
		})
	}
}

func TestFinalPartialChunkUsesExactCountNotJobSplitCeiling(t *testing.T) {
	info := &CommitTaskInfo{
		jobType: "embed", SplitSize: 1000, ExpectedOutputRecords: 1,
	}
	body := []byte(`{"job_type":"embed","model":"m","dim":2,"count":1,"vectors":[[1,0]]}`)
	if err := validateTaskResultArtifact(info, body); err != nil {
		t.Fatalf("one-row final chunk rejected because job split ceiling was larger: %v", err)
	}
}

func TestCountNonBlankJSONLRecordsMatchesStreamingSplitDefinition(t *testing.T) {
	if got := countNonBlankJSONLRecords([]byte("{\"a\":1}\r\n \n{\"b\":2}")); got != 2 {
		t.Fatalf("exact JSONL record count=%d, want 2", got)
	}
	if got := countNonBlankJSONLRecords([]byte("\n\r\n  \t")); got != 0 {
		t.Fatalf("blank JSONL record count=%d, want 0", got)
	}
}

func TestEmbeddingJSONPreflightStopsAtExactRowsAndElements(t *testing.T) {
	if _, _, err := preflightEmbeddingJSONVectors([]byte(`[[1,0],[0,1]]`), 1, 2); err == nil {
		t.Fatal("preflight consumed a second row beyond the exact one-row bound")
	}
	if _, _, err := preflightEmbeddingJSONVectors([]byte(`[[1,0,2]]`), 1, 2); err == nil {
		t.Fatal("preflight consumed elements beyond the exact two-element row bound")
	}
	rows, dim, err := preflightEmbeddingJSONVectors([]byte(`[[1,0]]`), 1, 2)
	if err != nil || rows != 1 || dim != 2 {
		t.Fatalf("bounded embedding preflight = %d x %d, err=%v", rows, dim, err)
	}
}

func TestEmbeddingBinaryCraftedHeadersNeverAllocateOrPanic(t *testing.T) {
	header := func(dim, count uint32, values ...uint32) []byte {
		body := make([]byte, embedBinHeaderLen+4*len(values))
		copy(body[:4], embedBinMagic)
		binary.LittleEndian.PutUint32(body[4:8], embedBinVersion)
		binary.LittleEndian.PutUint32(body[8:12], dim)
		binary.LittleEndian.PutUint32(body[12:16], count)
		for i, bits := range values {
			binary.LittleEndian.PutUint32(body[embedBinHeaderLen+i*4:], bits)
		}
		return body
	}
	cases := map[string][]byte{
		"multiplication-overflow": header(math.MaxUint32, math.MaxUint32),
		"zero-dimension":          header(0, 1),
		"zero-count":              header(1, 0),
		"count-exceeds-body":      header(1, 2, math.Float32bits(1)),
		"nan":                     header(1, 1, math.Float32bits(float32(math.NaN()))),
		"positive-infinity":       header(1, 1, math.Float32bits(float32(math.Inf(1)))),
	}
	for name, artifact := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := parseEmbeddingVectors(artifact); ok {
				t.Fatal("crafted binary header parsed successfully")
			}
			if _, err := mergeEmbedBinary([][]byte{artifact}, []PrimaryResult{{ResultRef: name}}); err == nil {
				t.Fatal("crafted binary header merged successfully")
			}
		})
	}

	bounded := binaryEmbeddingForValidation(1, [][]float32{{1}, {2}})
	if _, err := parseEmbeddingBinaryEnvelope(bounded, 1); err == nil {
		t.Fatal("attempt row bound did not reject a syntactically valid two-row artifact")
	}
}

func binaryEmbeddingForValidation(dim uint32, rows [][]float32) []byte {
	body := make([]byte, embedBinHeaderLen+len(rows)*int(dim)*4)
	copy(body[:4], embedBinMagic)
	binary.LittleEndian.PutUint32(body[4:8], embedBinVersion)
	binary.LittleEndian.PutUint32(body[8:12], dim)
	binary.LittleEndian.PutUint32(body[12:16], uint32(len(rows)))
	offset := embedBinHeaderLen
	for _, row := range rows {
		for _, value := range row {
			binary.LittleEndian.PutUint32(body[offset:offset+4], math.Float32bits(value))
			offset += 4
		}
	}
	return body
}
