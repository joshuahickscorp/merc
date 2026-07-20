package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

var ErrResultArtifactInvalid = errors.New("result artifact violates its job contract")

type ResultArtifactValidationError struct {
	JobType string
	Code    string
	Detail  string
}

func (e *ResultArtifactValidationError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("%s: %s (%s)", ErrResultArtifactInvalid, e.JobType, e.Code)
	}
	return fmt.Sprintf("%s: %s (%s): %s", ErrResultArtifactInvalid, e.JobType, e.Code, e.Detail)
}

func (e *ResultArtifactValidationError) Unwrap() error { return ErrResultArtifactInvalid }

const (
	resultValidationEmpty       = "empty"
	resultValidationJSON        = "invalid_json"
	resultValidationEnvelope    = "invalid_envelope"
	resultValidationCount       = "invalid_count"
	resultValidationIndex       = "invalid_index"
	resultValidationDimension   = "invalid_dimension"
	resultValidationNumeric     = "non_finite_numeric"
	resultValidationUnsupported = "unsupported_shape"
	resultValidationDigest      = "digest_mismatch"

	embeddingDimensionHardMax uint64 = 65_536
	embeddingRowsHardMax      uint64 = 1_000_000
	embeddingElementsHardMax  uint64 = 16 << 20
)

func invalidResultArtifact(jobType, code, detail string) error {
	return &ResultArtifactValidationError{JobType: jobType, Code: code, Detail: detail}
}

func validateReportedResultDigest(jobType, reported, sealed string) error {
	if reportedResultDigestMatches(reported, sealed) {
		return nil
	}
	return invalidResultArtifact(jobType, resultValidationDigest,
		"worker-reported SHA-256 does not match the sealed attempt artifact")
}

func invalidResultVerificationDecision(info *CommitTaskInfo, validationErr error) VerificationDecision {
	code := resultValidationEnvelope
	var typed *ResultArtifactValidationError
	if errors.As(validationErr, &typed) && typed.Code != "" {
		code = typed.Code
	}
	effects := []VerificationEffect{
		{Kind: VerificationEffectDockReputation, SupplierID: info.SupplierID, ReputationEvent: EventResultCorrupt},
		{Kind: VerificationEffectRecordEvent, JobID: info.JobID, TaskID: info.TaskID, SupplierID: info.SupplierID, EventKind: "artifact_invalid"},
		{Kind: VerificationEffectClawbackCredit, TaskID: info.TaskID, SupplierID: info.SupplierID},
		{Kind: VerificationEffectQuarantine, SupplierID: info.SupplierID},
		{Kind: VerificationEffectRequeue, TaskID: info.TaskID},
	}
	for i := range effects {
		effects[i].ID = verificationEffectPayloadID(info.TaskID, info.Attempt, i, effects[i])
	}
	return VerificationDecision{
		Outcome: OutcomeFail,
		Effects: effects,
		Failure: &VerificationFailure{Kind: "artifact_invalid", Code: code, JobType: info.jobType},
	}
}

func validateTaskResultArtifact(info *CommitTaskInfo, body []byte) error {
	if info == nil {
		return invalidResultArtifact("unknown", resultValidationEnvelope, "nil attempt snapshot")
	}
	if len(body) == 0 {
		return invalidResultArtifact(info.jobType, resultValidationEmpty, "zero-byte artifact")
	}
	if info.ExpectedOutputRecords < 0 {
		return invalidResultArtifact(info.jobType, resultValidationCount, "attempt carries a negative expected record count")
	}
	records := resultRecordContractForAttempt(info)

	switch info.jobType {
	case "embed":
		expectedDim := expectedEmbeddingDimension(info.ModelRef)
		if isEmbedBinary(body) {
			h, err := parseEmbeddingBinaryEnvelope(body, records.Max)
			if err != nil {
				return invalidResultArtifact(info.jobType, resultValidationDimension, err.Error())
			}
			if err := validateResultRecordCount(info.jobType, int(h.Count), records); err != nil {
				return err
			}
			if expectedDim > 0 && uint64(h.Dim) != expectedDim {
				return invalidResultArtifact(info.jobType, resultValidationDimension,
					fmt.Sprintf("dimension %d does not match model dimension %d", h.Dim, expectedDim))
			}
			return nil
		}
		vectors, err := parseEmbeddingJSONVectors(body, records.Max, expectedDim, true)
		if err != nil {
			return err
		}
		return validateResultRecordCount(info.jobType, len(vectors), records)
	case "batch_infer":
		return validateBatchInferResult(body, records)
	default:
		return invalidResultArtifact(info.jobType, resultValidationUnsupported, "workload has no retained result contract")
	}
}

type resultRecordContract struct {
	Exact uint64
	Max   uint64
}

func resultRecordContractForAttempt(info *CommitTaskInfo) resultRecordContract {
	if info != nil && info.ExpectedOutputRecords > 0 {
		exact := uint64(info.ExpectedOutputRecords)
		return resultRecordContract{Exact: exact, Max: exact}
	}
	if info != nil && info.SplitSize > 0 {
		return resultRecordContract{Max: uint64(info.SplitSize)}
	}
	return resultRecordContract{}
}

func expectedEmbeddingDimension(modelRef string) uint64 {
	switch strings.ToLower(strings.TrimSpace(modelRef)) {
	case "all-minilm-l6-v2":
		return 384
	default:
		return 0
	}
}

type embeddingBinaryEnvelope struct {
	Dim   uint32
	Count uint32
	Body  []byte
}

func checkedMulUint64(a, b uint64) (uint64, bool) {
	if a != 0 && b > math.MaxUint64/a {
		return 0, false
	}
	return a * b, true
}

func checkedAddUint64(a, b uint64) (uint64, bool) {
	if b > math.MaxUint64-a {
		return 0, false
	}
	return a + b, true
}

func parseEmbeddingBinaryEnvelope(obj []byte, maxRecords uint64) (embeddingBinaryEnvelope, error) {
	var out embeddingBinaryEnvelope
	if len(obj) < embedBinHeaderLen {
		return out, fmt.Errorf("binary artifact is %d bytes, shorter than the %d-byte header", len(obj), embedBinHeaderLen)
	}
	if !bytes.Equal(obj[:4], embedBinMagic) {
		return out, fmt.Errorf("binary artifact has invalid magic")
	}
	if version := binary.LittleEndian.Uint32(obj[4:8]); version != embedBinVersion {
		return out, fmt.Errorf("unsupported binary embedding version %d", version)
	}
	out.Dim = binary.LittleEndian.Uint32(obj[8:12])
	out.Count = binary.LittleEndian.Uint32(obj[12:16])
	if out.Dim == 0 || uint64(out.Dim) > embeddingDimensionHardMax {
		return embeddingBinaryEnvelope{}, fmt.Errorf("invalid embedding dimension %d", out.Dim)
	}
	if out.Count == 0 || uint64(out.Count) > embeddingRowsHardMax {
		return embeddingBinaryEnvelope{}, fmt.Errorf("invalid embedding row count %d", out.Count)
	}
	if maxRecords > 0 && uint64(out.Count) > maxRecords {
		return embeddingBinaryEnvelope{}, fmt.Errorf("embedding row count %d exceeds attempt bound %d", out.Count, maxRecords)
	}

	elements, ok := checkedMulUint64(uint64(out.Dim), uint64(out.Count))
	if !ok || elements > embeddingElementsHardMax {
		return embeddingBinaryEnvelope{}, fmt.Errorf("embedding shape %dx%d exceeds safe element bound", out.Count, out.Dim)
	}
	bodyBytes, ok := checkedMulUint64(elements, 4)
	if !ok {
		return embeddingBinaryEnvelope{}, fmt.Errorf("embedding body byte count overflows uint64")
	}
	want, ok := checkedAddUint64(embedBinHeaderLen, bodyBytes)
	if !ok || want != uint64(len(obj)) {
		return embeddingBinaryEnvelope{}, fmt.Errorf("binary body is %d bytes, header implies %d (%dx%d f32)",
			len(obj)-embedBinHeaderLen, bodyBytes, out.Count, out.Dim)
	}

	out.Body = obj[embedBinHeaderLen:]
	for off := 0; off < len(out.Body); off += 4 {
		v := math.Float32frombits(binary.LittleEndian.Uint32(out.Body[off : off+4]))
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return embeddingBinaryEnvelope{}, fmt.Errorf("embedding contains a non-finite float32 at element %d", off/4)
		}
	}
	return out, nil
}

type embeddingJSONResult struct {
	JobType string          `json:"job_type"`
	Model   string          `json:"model"`
	Dim     *uint64         `json:"dim"`
	Count   *uint64         `json:"count"`
	Vectors json.RawMessage `json:"vectors"`
}

func parseEmbeddingJSONVectors(obj []byte, maxRecords, expectedDim uint64, requireEnvelope bool) ([][]float64, error) {
	var r embeddingJSONResult
	if err := decodeStrictJSON(obj, &r); err != nil {
		return nil, invalidResultArtifact("embed", resultValidationJSON, err.Error())
	}
	if requireEnvelope && (r.JobType != "embed" || strings.TrimSpace(r.Model) == "" || r.Dim == nil || r.Count == nil) {
		return nil, invalidResultArtifact("embed", resultValidationEnvelope, "missing or incorrect job_type/model/dim/count")
	}
	if r.JobType != "" && r.JobType != "embed" {
		return nil, invalidResultArtifact("embed", resultValidationEnvelope, "job_type does not match embed")
	}
	dimensionBound := expectedDim
	if r.Dim != nil {
		if *r.Dim == 0 || *r.Dim > embeddingDimensionHardMax {
			return nil, invalidResultArtifact("embed", resultValidationDimension, "declared dimension is zero or excessive")
		}
		if expectedDim > 0 && *r.Dim != expectedDim {
			return nil, invalidResultArtifact("embed", resultValidationDimension,
				fmt.Sprintf("dimension %d does not match model dimension %d", *r.Dim, expectedDim))
		}
		dimensionBound = *r.Dim
	}
	if r.Count != nil && (*r.Count == 0 || *r.Count > embeddingRowsHardMax || (maxRecords > 0 && *r.Count > maxRecords)) {
		return nil, invalidResultArtifact("embed", resultValidationCount, "declared count exceeds the attempt bound")
	}

	rows, dim, err := preflightEmbeddingJSONVectors(r.Vectors, maxRecords, dimensionBound)
	if err != nil {
		return nil, err
	}
	if r.Count != nil && *r.Count != rows {
		return nil, invalidResultArtifact("embed", resultValidationCount, "declared count does not match vectors")
	}
	if r.Dim != nil && *r.Dim != dim {
		return nil, invalidResultArtifact("embed", resultValidationDimension, "declared dim does not match vector width")
	}
	if expectedDim > 0 && dim != expectedDim {
		return nil, invalidResultArtifact("embed", resultValidationDimension,
			fmt.Sprintf("dimension %d does not match model dimension %d", dim, expectedDim))
	}
	var vectors [][]float64
	if err := json.Unmarshal(r.Vectors, &vectors); err != nil {
		return nil, invalidResultArtifact("embed", resultValidationJSON, err.Error())
	}
	if uint64(len(vectors)) != rows {
		return nil, invalidResultArtifact("embed", resultValidationCount, "vectors changed after bounded preflight")
	}
	for rowIndex, row := range vectors {
		if uint64(len(row)) != dim {
			return nil, invalidResultArtifact("embed", resultValidationDimension,
				fmt.Sprintf("row %d has width %d, want %d", rowIndex, len(row), dim))
		}
		nonzero := false
		for _, value := range row {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, invalidResultArtifact("embed", resultValidationNumeric,
					fmt.Sprintf("row %d contains a non-finite value", rowIndex))
			}
			nonzero = nonzero || value != 0
		}
		if !nonzero {
			return nil, invalidResultArtifact("embed", resultValidationNumeric,
				fmt.Sprintf("row %d is the zero vector", rowIndex))
		}
	}
	return vectors, nil
}

func preflightEmbeddingJSONVectors(raw []byte, maxRecords, dimensionBound uint64) (uint64, uint64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	start, err := decoder.Token()
	if err != nil {
		return 0, 0, invalidResultArtifact("embed", resultValidationJSON, err.Error())
	}
	if delim, ok := start.(json.Delim); !ok || delim != '[' {
		return 0, 0, invalidResultArtifact("embed", resultValidationCount, "vectors is not an array")
	}
	var rows, dim, elements uint64
	for decoder.More() {
		if rows >= embeddingRowsHardMax || (maxRecords > 0 && rows >= maxRecords) {
			return 0, 0, invalidResultArtifact("embed", resultValidationCount,
				fmt.Sprintf("vector count exceeds attempt bound %d", maxRecords))
		}
		rowStart, err := decoder.Token()
		if err != nil {
			return 0, 0, invalidResultArtifact("embed", resultValidationJSON, err.Error())
		}
		if delim, ok := rowStart.(json.Delim); !ok || delim != '[' {
			return 0, 0, invalidResultArtifact("embed", resultValidationDimension, "embedding row is not an array")
		}
		var width uint64
		for decoder.More() {
			limit := embeddingDimensionHardMax
			if dimensionBound > 0 && dimensionBound < limit {
				limit = dimensionBound
			}
			if width >= limit || elements >= embeddingElementsHardMax {
				return 0, 0, invalidResultArtifact("embed", resultValidationDimension,
					"embedding element count exceeds exact/safe bound")
			}
			token, err := decoder.Token()
			if err != nil {
				return 0, 0, invalidResultArtifact("embed", resultValidationJSON, err.Error())
			}
			number, ok := token.(json.Number)
			if !ok {
				return 0, 0, invalidResultArtifact("embed", resultValidationNumeric, "embedding element is not a number")
			}
			value, err := strconv.ParseFloat(number.String(), 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
				return 0, 0, invalidResultArtifact("embed", resultValidationNumeric, "embedding element is not finite float64")
			}
			width++
			elements++
		}
		rowEnd, err := decoder.Token()
		if err != nil {
			return 0, 0, invalidResultArtifact("embed", resultValidationJSON, err.Error())
		}
		if delim, ok := rowEnd.(json.Delim); !ok || delim != ']' {
			return 0, 0, invalidResultArtifact("embed", resultValidationDimension, "embedding row is not closed")
		}
		if width == 0 {
			return 0, 0, invalidResultArtifact("embed", resultValidationDimension, "embedding dimension is zero")
		}
		if rows == 0 {
			dim = width
		} else if width != dim {
			return 0, 0, invalidResultArtifact("embed", resultValidationDimension,
				fmt.Sprintf("row %d has width %d, want %d", rows, width, dim))
		}
		if dimensionBound > 0 && width != dimensionBound {
			return 0, 0, invalidResultArtifact("embed", resultValidationDimension,
				fmt.Sprintf("row %d has width %d, want %d", rows, width, dimensionBound))
		}
		rows++
	}
	end, err := decoder.Token()
	if err != nil {
		return 0, 0, invalidResultArtifact("embed", resultValidationJSON, err.Error())
	}
	if delim, ok := end.(json.Delim); !ok || delim != ']' {
		return 0, 0, invalidResultArtifact("embed", resultValidationJSON, "vectors array is not closed")
	}
	if rows == 0 {
		return 0, 0, invalidResultArtifact("embed", resultValidationCount, "vectors must contain at least one row")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple vectors values")
		}
		return 0, 0, invalidResultArtifact("embed", resultValidationJSON, err.Error())
	}
	return rows, dim, nil
}

func validateBatchInferResult(body []byte, records resultRecordContract) error {
	var r struct {
		JobType     string `json:"job_type"`
		Model       string `json:"model"`
		Completions []struct {
			Index  int    `json:"index"`
			Text   string `json:"text"`
			Tokens uint64 `json:"tokens"`
		} `json:"completions"`
	}
	if err := decodeStrictJSON(body, &r); err != nil {
		return invalidResultArtifact("batch_infer", resultValidationJSON, err.Error())
	}
	if r.JobType != "batch_infer" || strings.TrimSpace(r.Model) == "" {
		return invalidResultArtifact("batch_infer", resultValidationEnvelope, "job_type/model are missing or incorrect")
	}
	if err := validateResultRecordCount("batch_infer", len(r.Completions), records); err != nil {
		return err
	}
	for i, item := range r.Completions {
		if item.Index != i {
			return invalidResultArtifact("batch_infer", resultValidationIndex,
				fmt.Sprintf("completion index %d at position %d", item.Index, i))
		}
	}
	return nil
}

func validateResultRecordCount(jobType string, count int, records resultRecordContract) error {
	if count <= 0 {
		return invalidResultArtifact(jobType, resultValidationCount, "result contains no records")
	}
	if records.Exact > 0 && uint64(count) != records.Exact {
		return invalidResultArtifact(jobType, resultValidationCount,
			fmt.Sprintf("result count %d does not match exact task count %d", count, records.Exact))
	}
	if records.Max > 0 && uint64(count) > records.Max {
		return invalidResultArtifact(jobType, resultValidationCount,
			fmt.Sprintf("result count %d exceeds attempt bound %d", count, records.Max))
	}
	return nil
}

func decodeStrictJSON(body []byte, dst any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("empty JSON document")
	}
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON documents")
		}
		return err
	}
	return nil
}
