package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type audioHTTPTestPart struct {
	name     string
	filename string
	value    []byte
}

func audioHTTPTestRequest(t *testing.T, parts ...audioHTTPTestPart) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, part := range parts {
		var (
			w   interface{ Write([]byte) (int, error) }
			err error
		)
		if part.filename != "" {
			w, err = mw.CreateFormFile(part.name, part.filename)
		} else {
			w, err = mw.CreateFormField(part.name)
		}
		if err != nil {
			t.Fatalf("create multipart part: %v", err)
		}
		if _, err := w.Write(part.value); err != nil {
			t.Fatalf("write multipart part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/jobs", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestParseAudioMultipartDefaultsAndSubmitScalars(t *testing.T) {
	raw := validAudioUploadTestWAV(audioUploadSampleRate)
	tests := []struct {
		name   string
		submit bool
		parts  []audioHTTPTestPart
		check  func(t *testing.T, got audioMultipartFields)
	}{
		{
			name:  "quote defaults",
			parts: []audioHTTPTestPart{{name: "file", filename: "clip.wav", value: raw}},
			check: func(t *testing.T, got audioMultipartFields) {
				if got.model != audioUploadDefaultModel || got.tier != "batch" || !bytes.Equal(got.rawWAV, raw) {
					t.Fatalf("fields = %+v", got)
				}
			},
		},
		{
			name:   "submit scalars",
			submit: true,
			parts: []audioHTTPTestPart{
				{name: "model", value: []byte("whisper-base")},
				{name: "tier", value: []byte("priority")},
				{name: "quote_id", value: []byte("q_123")},
				{name: "max_usd", value: []byte("0.25")},
				{name: "file", filename: "clip.wav", value: raw},
			},
			check: func(t *testing.T, got audioMultipartFields) {
				if got.model != "whisper-base" || got.tier != "priority" || got.quoteID != "q_123" || got.maxUSD != .25 {
					t.Fatalf("fields = %+v", got)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := audioHTTPTestRequest(t, tc.parts...)
			got, herr := parseAudioMultipart(httptest.NewRecorder(), req, tc.submit)
			if herr != nil {
				t.Fatalf("parseAudioMultipart: %v", herr)
			}
			tc.check(t, got)
		})
	}
}

func TestParseAudioMultipartRejectsAmbiguousAndOversizedBodies(t *testing.T) {
	raw := validAudioUploadTestWAV(1)
	tests := []struct {
		name   string
		submit bool
		parts  []audioHTTPTestPart
		status int
		want   string
	}{
		{"missing file", false, nil, http.StatusBadRequest, "exactly one file"},
		{"duplicate file", false, []audioHTTPTestPart{
			{name: "file", filename: "a.wav", value: raw},
			{name: "file", filename: "b.wav", value: raw},
		}, http.StatusBadRequest, "duplicate"},
		{"unknown field", false, []audioHTTPTestPart{
			{name: "file", filename: "a.wav", value: raw},
			{name: "language", value: []byte("en")},
		}, http.StatusBadRequest, "unsupported"},
		{"submit-only field on quote", false, []audioHTTPTestPart{
			{name: "file", filename: "a.wav", value: raw},
			{name: "max_usd", value: []byte("1")},
		}, http.StatusBadRequest, "unsupported"},
		{"negative max", true, []audioHTTPTestPart{
			{name: "file", filename: "a.wav", value: raw},
			{name: "max_usd", value: []byte("-1")},
		}, http.StatusBadRequest, "finite non-negative"},
		{"file too large", true, []audioHTTPTestPart{
			{name: "file", filename: "a.wav", value: make([]byte, audioUploadMaxRawBytes+1)},
		}, http.StatusRequestEntityTooLarge, "exceeds"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := audioHTTPTestRequest(t, tc.parts...)
			_, herr := parseAudioMultipart(httptest.NewRecorder(), req, tc.submit)
			if herr == nil || herr.status != tc.status || !strings.Contains(herr.msg, tc.want) {
				t.Fatalf("error = %#v, want status %d containing %q", herr, tc.status, tc.want)
			}
		})
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/jobs", strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "application/json")
	if _, herr := parseAudioMultipart(httptest.NewRecorder(), req, true); herr == nil || herr.status != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong Content-Type error = %#v, want 415", herr)
	}
}

func TestAudioJobSubmissionBindsCanonicalSingleRecord(t *testing.T) {
	raw := validAudioUploadTestWAV(audioUploadSampleRate)
	sub, herr := audioJobSubmission(audioMultipartFields{rawWAV: raw, model: "whisper-tiny", tier: "batch"})
	if herr != nil {
		t.Fatalf("audioJobSubmission: %v", herr)
	}
	if sub.JobType.Type != audioUploadJobType || sub.Model.Kind != "hf" || sub.JobType.Language != nil || sub.JobType.Timestamps ||
		splitSizeOf(sub.Params) != 1 || sub.Verification.RedundancyFrac != 1 || sub.audioAdmission == nil {
		t.Fatalf("unsafe audio submission shape: %+v", sub)
	}
	var normalized string
	if err := json.Unmarshal(sub.Input, &normalized); err != nil {
		t.Fatalf("decode normalized input: %v", err)
	}
	sub.audioAdmission.pricePerAudioMinute = .004
	if meta := audioUploadMetadata(sub.audioAdmission); meta.PricePerAudioMinuteUSD != .004 || meta.Samples != audioUploadSampleRate {
		t.Fatalf("audio metadata did not retain pricing authority: %+v", meta)
	}
	if err := validateAudioAdmissionInput(sub, []byte(normalized)); err != nil {
		t.Fatalf("valid admission rejected: %v", err)
	}
	mutated := []byte(normalized)
	mutated[len(mutated)-2] ^= 1
	if err := validateAudioAdmissionInput(sub, mutated); err == nil {
		t.Fatal("mutated normalized input retained clip admission")
	}
}

func TestAudioRoutesUseTightBodyCap(t *testing.T) {
	want := int64(audioUploadMaxRawBytes + audioUploadBodyOverhead)
	for _, path := range []string{"/v1/audio/jobs", "/v1/audio/jobs/quote"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		if got := requestBodyLimit(req); got != want {
			t.Fatalf("requestBodyLimit(%s) = %d, want %d", path, got, want)
		}
	}
}

func TestAudioPricingUsesDurationNotNormalizedBytes(t *testing.T) {
	facts := audioUploadFacts{samples: 30 * audioUploadSampleRate, durationMinutes: .5}
	if got := estimateAudioUploadUSD(facts, .004, "batch"); got != .002 {
		t.Fatalf("batch estimate = %.6f, want .002", got)
	}
	if got := estimateAudioUploadUSD(facts, .004, "priority"); got != .003 {
		t.Fatalf("priority estimate = %.6f, want .003", got)
	}
	if got := estimateAudioUploadUSD(audioUploadFacts{samples: 1, durationMinutes: 1.0 / 960000}, .004, "batch"); got != 0 {
		t.Fatalf("rounded-zero boundary = %.6f, want 0", got)
	}
	admission := &audioAdmission{facts: facts, pricePerAudioMinute: .004}
	sub := jobSubmit{JobType: JobType{Type: audioUploadJobType}, Tier: "batch", audioAdmission: admission}
	server := &Server{}
	if a, b := server.estimateSubmissionUSD(context.Background(), sub, 1, 1), server.estimateSubmissionUSD(context.Background(), sub, 1<<30, 999999); a != b || a != .002 {
		t.Fatalf("audio estimate changed with normalized bytes/lines: small=%f large=%f", a, b)
	}
	if got := audioOfferedRateUSDHr(facts, .004, 2); math.Abs(float64(got)-14.4) > 1e-5 {
		t.Fatalf("audio offered rate = %f, want 14.4", got)
	}
}

func TestGenericAudioPathsFailClosedBeforeDependencies(t *testing.T) {
	server := &Server{}
	if _, herr := server.createJob(context.Background(), uuid.New(), jobSubmit{JobType: JobType{Type: audioUploadJobType}}); herr == nil || herr.status != http.StatusBadRequest {
		t.Fatalf("generic createJob audio error = %#v", herr)
	}

	authed := func(body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxBuyer, &AuthResult{}))
		rr := httptest.NewRecorder()
		handler(rr, req)
		return rr
	}
	generic := `{"job_type":{"type":"audio_transcribe"},"model":{"kind":"hf","ref":"whisper-tiny"},"input":"x"}`
	if rr := authed(generic, server.handleQuote); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "/v1/audio/jobs") {
		t.Fatalf("generic quote = %d %s", rr.Code, rr.Body.String())
	}
}

func TestBuildQuoteRejectsMismatchedAudioAdmissionInternally(t *testing.T) {
	raw := validAudioUploadTestWAV(audioUploadSampleRate)
	sub, herr := audioJobSubmission(audioMultipartFields{rawWAV: raw, model: "whisper-tiny", tier: "batch"})
	if herr != nil {
		t.Fatalf("audioJobSubmission: %v", herr)
	}
	q := (&Server{}).buildQuoteWithSchedule(context.Background(), uuid.New(), sub, []byte(`{"audio_b64":"different"}\n`), EconomicSchedule{})
	if q.Economics.Executable || len(q.Warnings) == 0 || !strings.Contains(q.Warnings[0], "audio admission blocked") {
		t.Fatalf("mismatched admission quote was not blocked: %+v", q)
	}
}
