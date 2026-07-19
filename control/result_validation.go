package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"strconv"
	"strings"
)

// ErrResultArtifactInvalid classifies a worker upload whose bytes are present
// and bounded, but do not satisfy the immutable output contract for the task.
// This is a deterministic worker result, not a retryable object-store failure.
var ErrResultArtifactInvalid = errors.New("result artifact violates its job contract")

// ResultArtifactValidationError carries a stable, low-cardinality reason code.
// The persisted verification plan records Code, never the untrusted body or a
// high-cardinality parser error.
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
	resultValidationMedia       = "invalid_media"
	resultValidationUnsupported = "unsupported_shape"

	// These limits are deliberately far beyond every advertised embedding model
	// (currently 384 dimensions) while preventing a syntactically valid malicious
	// container from turning a bounded byte artifact into an unbounded slice graph.
	embeddingDimensionHardMax uint64 = 65_536
	embeddingRowsHardMax      uint64 = 1_000_000
	embeddingElementsHardMax  uint64 = 16 << 20
)

func invalidResultArtifact(jobType, code, detail string) error {
	return &ResultArtifactValidationError{JobType: jobType, Code: code, Detail: detail}
}

// invalidResultVerificationDecision converts a deterministic validation failure
// into the same ordered, exactly-once mutation vocabulary used by every other
// verifier rejection. OutcomeFail guarantees an empty settlement and makes the
// apply transaction omit duration/completion counters.
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

// validateTaskResultArtifact is pure: it depends only on the immutable attempt
// snapshot and the exact server-sealed bytes. It runs before peer lookup,
// verification success, duration accounting, completion counters, or settlement.
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
	case "batch_classification":
		return validateClassificationResult(body, records)
	case "json_extraction":
		return validateJSONExtractionResult(body, records)
	case "rerank":
		return validateRerankResult(body, records)
	case "audio_transcribe":
		return validateAudioTranscriptionResult(body, records)
	case "image_gen":
		return validateImageResult(body)
	default:
		// Opaque/custom/checkpoint job types do not have a control-plane result
		// schema. Presence is the only honest invariant available; whitespace-only
		// text is never a meaningful artifact.
		if len(bytes.TrimSpace(body)) == 0 {
			return invalidResultArtifact(info.jobType, resultValidationEmpty, "whitespace-only artifact")
		}
		return nil
	}
}

// resultRecordContract separates exact modern authority from legacy ceiling
// compatibility. Exact is positive only when submit/chunk splitting persisted a
// real count; Max is that same exact count, or the old split-size upper bound for
// an explicitly unknown legacy row.
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
	case "all-minilm-l6-v2", "bge-small-en-v1.5":
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

// parseEmbeddingBinaryEnvelope validates all arithmetic before any allocation.
// maxRecords is the immutable attempt's maximum row count when non-zero.
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

// parseEmbeddingJSONVectors accepts the legacy vectors-only shape for semantic
// comparison (known-answer fixtures predate the envelope), while production
// attempt validation requires the full current agent envelope.
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

	// Preflight the vectors as a token stream before json.Unmarshal is allowed to
	// build [][]float64. In an exact one-row attempt, for example, a second row is
	// rejected before even its first number is consumed, so parser expansion can
	// never allocate beyond the immutable row/element contract.
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

// preflightEmbeddingJSONVectors proves row, dimension, finite-number, and total
// element bounds without constructing any slice graph. The caller may allocate
// vectors only after this succeeds.
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

func validateClassificationResult(body []byte, records resultRecordContract) error {
	var r struct {
		JobType string `json:"job_type"`
		Model   string `json:"model"`
		Count   uint64 `json:"count"`
		Labels  []struct {
			Index int    `json:"index"`
			Label string `json:"label"`
		} `json:"labels"`
	}
	if err := decodeStrictJSON(body, &r); err != nil {
		return invalidResultArtifact("batch_classification", resultValidationJSON, err.Error())
	}
	if r.JobType != "batch_classification" || strings.TrimSpace(r.Model) == "" {
		return invalidResultArtifact("batch_classification", resultValidationEnvelope, "job_type/model are missing or incorrect")
	}
	if r.Count != uint64(len(r.Labels)) {
		return invalidResultArtifact("batch_classification", resultValidationCount, "declared count does not match labels")
	}
	if err := validateResultRecordCount("batch_classification", len(r.Labels), records); err != nil {
		return err
	}
	for i, item := range r.Labels {
		if item.Index != i || strings.TrimSpace(item.Label) == "" {
			return invalidResultArtifact("batch_classification", resultValidationIndex,
				fmt.Sprintf("invalid label/index at position %d", i))
		}
	}
	return nil
}

func validateJSONExtractionResult(body []byte, records resultRecordContract) error {
	var r struct {
		JobType string `json:"job_type"`
		Model   string `json:"model"`
		Count   uint64 `json:"count"`
		Items   []struct {
			Index int             `json:"index"`
			JSON  json.RawMessage `json:"json"`
		} `json:"items"`
	}
	if err := decodeStrictJSON(body, &r); err != nil {
		return invalidResultArtifact("json_extraction", resultValidationJSON, err.Error())
	}
	if r.JobType != "json_extraction" || strings.TrimSpace(r.Model) == "" {
		return invalidResultArtifact("json_extraction", resultValidationEnvelope, "job_type/model are missing or incorrect")
	}
	if r.Count != uint64(len(r.Items)) {
		return invalidResultArtifact("json_extraction", resultValidationCount, "declared count does not match items")
	}
	if err := validateResultRecordCount("json_extraction", len(r.Items), records); err != nil {
		return err
	}
	for i, item := range r.Items {
		if item.Index != i {
			return invalidResultArtifact("json_extraction", resultValidationIndex,
				fmt.Sprintf("item index %d at position %d", item.Index, i))
		}
		if err := validateFiniteJSONValue(item.JSON, true); err != nil {
			return invalidResultArtifact("json_extraction", resultValidationNumeric,
				fmt.Sprintf("item %d: %v", i, err))
		}
	}
	return nil
}

func validateRerankResult(body []byte, records resultRecordContract) error {
	var r struct {
		JobType  string `json:"job_type"`
		Model    string `json:"model"`
		Count    uint64 `json:"count"`
		Rankings []struct {
			Index int   `json:"index"`
			Order []int `json:"order"`
		} `json:"rankings"`
	}
	if err := decodeStrictJSON(body, &r); err != nil {
		return invalidResultArtifact("rerank", resultValidationJSON, err.Error())
	}
	if r.JobType != "rerank" || strings.TrimSpace(r.Model) == "" {
		return invalidResultArtifact("rerank", resultValidationEnvelope, "job_type/model are missing or incorrect")
	}
	if r.Count != uint64(len(r.Rankings)) {
		return invalidResultArtifact("rerank", resultValidationCount, "declared count does not match rankings")
	}
	if err := validateResultRecordCount("rerank", len(r.Rankings), records); err != nil {
		return err
	}
	for i, item := range r.Rankings {
		if item.Index != i {
			return invalidResultArtifact("rerank", resultValidationIndex,
				fmt.Sprintf("ranking index %d at position %d", item.Index, i))
		}
		seen := make(map[int]struct{}, len(item.Order))
		for _, candidate := range item.Order {
			if candidate < 0 {
				return invalidResultArtifact("rerank", resultValidationIndex, "ranking contains a negative candidate index")
			}
			if _, duplicate := seen[candidate]; duplicate {
				return invalidResultArtifact("rerank", resultValidationIndex, "ranking contains a duplicate candidate index")
			}
			seen[candidate] = struct{}{}
		}
	}
	return nil
}

func validateAudioTranscriptionResult(body []byte, records resultRecordContract) error {
	var r struct {
		JobType  string `json:"job_type"`
		Model    string `json:"model"`
		Text     string `json:"text"`
		Segments []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Text  string  `json:"text"`
		} `json:"segments"`
	}
	if err := decodeStrictJSON(body, &r); err != nil {
		return invalidResultArtifact("audio_transcribe", resultValidationJSON, err.Error())
	}
	if r.JobType != "audio_transcribe" || strings.TrimSpace(r.Model) == "" {
		return invalidResultArtifact("audio_transcribe", resultValidationEnvelope, "job_type/model are missing or incorrect")
	}
	if err := validateResultRecordCount("audio_transcribe", len(r.Segments), records); err != nil {
		return err
	}
	lastEnd := 0.0
	for i, segment := range r.Segments {
		if math.IsNaN(segment.Start) || math.IsInf(segment.Start, 0) ||
			math.IsNaN(segment.End) || math.IsInf(segment.End, 0) ||
			segment.Start < 0 || segment.End < segment.Start || (i > 0 && segment.Start < lastEnd) {
			return invalidResultArtifact("audio_transcribe", resultValidationNumeric,
				fmt.Sprintf("segment %d has invalid/non-monotonic timestamps", i))
		}
		lastEnd = segment.End
	}
	return nil
}

func validateImageResult(body []byte) error {
	width, height, ok := imageDimensions(body)
	if !ok || width == 0 || height == 0 {
		return invalidResultArtifact("image_gen", resultValidationMedia, "expected a complete PNG, JPEG, or WebP image with nonzero dimensions")
	}
	const maxImageDimension = 32_768
	const maxImagePixels = 268_435_456
	if width > maxImageDimension || height > maxImageDimension || uint64(width)*uint64(height) > maxImagePixels {
		return invalidResultArtifact("image_gen", resultValidationDimension,
			fmt.Sprintf("image dimensions %dx%d exceed the safe output envelope", width, height))
	}
	return nil
}

// imageDimensions recognizes only the formats the browser/product surface can
// consume directly. It validates enough of each header to derive nonzero
// dimensions without decoding attacker-controlled pixels.
func imageDimensions(body []byte) (uint32, uint32, bool) {
	if len(body) >= 8 && bytes.Equal(body[:8], []byte("\x89PNG\r\n\x1a\n")) {
		var width, height uint32
		seenIHDR, seenIDAT := false, false
		for offset := 8; offset+12 <= len(body); {
			chunkBytes := uint64(binary.BigEndian.Uint32(body[offset : offset+4]))
			end := uint64(offset) + 12 + chunkBytes
			if end > uint64(len(body)) {
				return 0, 0, false
			}
			kind := body[offset+4 : offset+8]
			dataEnd := offset + 8 + int(chunkBytes)
			gotCRC := binary.BigEndian.Uint32(body[dataEnd : dataEnd+4])
			if crc32.ChecksumIEEE(body[offset+4:dataEnd]) != gotCRC {
				return 0, 0, false
			}
			switch string(kind) {
			case "IHDR":
				if seenIHDR || offset != 8 || chunkBytes != 13 {
					return 0, 0, false
				}
				width = binary.BigEndian.Uint32(body[offset+8 : offset+12])
				height = binary.BigEndian.Uint32(body[offset+12 : offset+16])
				seenIHDR = true
			case "IDAT":
				seenIDAT = seenIDAT || chunkBytes > 0
			case "IEND":
				return width, height, seenIHDR && seenIDAT && chunkBytes == 0 && int(end) == len(body)
			}
			offset = int(end)
		}
		return 0, 0, false
	}
	if len(body) >= 4 && body[0] == 0xff && body[1] == 0xd8 && body[len(body)-2] == 0xff && body[len(body)-1] == 0xd9 {
		var width, height uint32
		for offset := 2; offset+4 <= len(body)-2; {
			if body[offset] != 0xff {
				offset++
				continue
			}
			for offset < len(body)-2 && body[offset] == 0xff {
				offset++
			}
			if offset >= len(body)-2 {
				break
			}
			marker := body[offset]
			offset++
			if marker == 0xd8 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd9) {
				continue
			}
			if offset+2 > len(body)-2 {
				break
			}
			segmentBytes := int(binary.BigEndian.Uint16(body[offset : offset+2]))
			if segmentBytes < 2 || offset+segmentBytes > len(body)-2 {
				break
			}
			if isJPEGStartOfFrame(marker) && segmentBytes >= 7 {
				height = uint32(binary.BigEndian.Uint16(body[offset+3 : offset+5]))
				width = uint32(binary.BigEndian.Uint16(body[offset+5 : offset+7]))
			}
			if marker == 0xda {
				return width, height, width > 0 && height > 0
			}
			offset += segmentBytes
		}
	}
	if len(body) >= 30 && bytes.Equal(body[:4], []byte("RIFF")) && bytes.Equal(body[8:12], []byte("WEBP")) &&
		uint64(binary.LittleEndian.Uint32(body[4:8]))+8 == uint64(len(body)) {
		switch string(body[12:16]) {
		case "VP8X":
			width := uint32(body[24]) | uint32(body[25])<<8 | uint32(body[26])<<16
			height := uint32(body[27]) | uint32(body[28])<<8 | uint32(body[29])<<16
			return width + 1, height + 1, true
		case "VP8L":
			if len(body) >= 25 && body[20] == 0x2f {
				bits := binary.LittleEndian.Uint32(body[21:25])
				return (bits & 0x3fff) + 1, ((bits >> 14) & 0x3fff) + 1, true
			}
		case "VP8 ":
			if len(body) >= 30 && bytes.Equal(body[23:26], []byte("\x9d\x01\x2a")) {
				return uint32(binary.LittleEndian.Uint16(body[26:28]) & 0x3fff),
					uint32(binary.LittleEndian.Uint16(body[28:30]) & 0x3fff), true
			}
		}
	}
	return 0, 0, false
}

func isJPEGStartOfFrame(marker byte) bool {
	switch marker {
	case 0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf:
		return true
	default:
		return false
	}
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
	// encoding/json otherwise accepts duplicate object members and silently keeps
	// the last value. A verifier and a buyer parser that choose different members
	// could then disagree about the exact artifact that was approved. Walk the
	// complete value first so duplicates at any nesting depth fail closed.
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

func validateFiniteJSONValue(raw []byte, requireObject bool) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	if requireObject {
		if _, ok := value.(map[string]any); !ok {
			return errors.New("value is not a JSON object")
		}
	}
	return walkFiniteJSON(value)
}

func walkFiniteJSON(value any) error {
	switch v := value.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("number %q is not finite float64", v)
		}
	case []any:
		for _, item := range v {
			if err := walkFiniteJSON(item); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, item := range v {
			if err := walkFiniteJSON(item); err != nil {
				return err
			}
		}
	}
	return nil
}
