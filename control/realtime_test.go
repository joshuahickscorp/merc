package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestVLLMRuntimeProfilesAreImmutableAndExact(t *testing.T) {
	profiles := sortedVLLMProfiles()
	if len(profiles) == 0 {
		t.Fatal("runtime profile catalog is empty")
	}
	for _, profile := range profiles {
		if err := validateVLLMRuntimeProfile(profile); err != nil {
			t.Fatalf("profile %s: %v", profile.RuntimeProfileID, err)
		}
		if len(profile.ProfileSHA256) != 64 {
			t.Fatalf("profile %s digest is not sha256 hex", profile.RuntimeProfileID)
		}
	}
}

func TestPrepareRealtimeRequestCanonicalizesDefaultsAndPriceCeiling(t *testing.T) {
	raw := []byte(`{"model":"cx-chat-1b","messages":[{"role":"user","content":"hello"}],"stream":true,"max_tokens":8}`)
	first, err := prepareRealtimeRequest(raw, "1.00")
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepareRealtimeRequest(raw, "1.00")
	if err != nil {
		t.Fatal(err)
	}
	if first.InputCommitment != second.InputCommitment || first.RequestSHA256 != second.RequestSHA256 {
		t.Fatal("canonical request commitments are not retry-stable")
	}
	if !first.Stream || first.MaximumPriceUSD <= 0 || first.EstimatedPriceUSD <= 0 || first.EstimatedPriceUSD > first.MaximumPriceUSD {
		t.Fatalf("invalid prepared economics: %+v", first)
	}
	var upstream map[string]any
	if err := json.Unmarshal(first.Body, &upstream); err != nil {
		t.Fatal(err)
	}
	if upstream["temperature"] == nil || upstream["top_p"] == nil {
		t.Fatal("versioned generation defaults were not resolved")
	}
	streamOptions, ok := upstream["stream_options"].(map[string]any)
	if !ok || streamOptions["include_usage"] != true {
		t.Fatal("stream usage reconciliation was not requested upstream")
	}
	if _, err := prepareRealtimeRequest(raw, "0.000001"); err == nil || !strings.Contains(err.Error(), "exceeds buyer ceiling") {
		t.Fatalf("price ceiling did not reject the request: %v", err)
	}
}

func TestPrepareRealtimeRequestPreservesLargeIntegerFields(t *testing.T) {
	const seed = "9007199254740993"
	prepared, err := prepareRealtimeRequest([]byte(`{"model":"cx-chat-1b","messages":[{"role":"user","content":"hello"}],"seed":`+seed+`}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(prepared.Body, []byte(`"seed":`+seed)) {
		t.Fatalf("large integer lost precision in canonical request: %s", prepared.Body)
	}
}

func TestPrepareRealtimeRequestRejectsInvalidCompatibilityInputs(t *testing.T) {
	tests := []string{
		`{}`,
		`{"model":"unknown","messages":[{"role":"user","content":"x"}]}`,
		`{"model":"cx-chat-1b","messages":[]}`,
		`{"model":"cx-chat-1b","messages":[{}],"stream":"yes"}`,
		`{"model":"cx-chat-1b","messages":[{}],"max_tokens":999999}`,
	}
	for _, raw := range tests {
		if _, err := prepareRealtimeRequest([]byte(raw), ""); err == nil {
			t.Fatalf("invalid request accepted: %s", raw)
		}
	}
}

func TestValidateRealtimeUpstreamURLPreventsWorkerSSRF(t *testing.T) {
	if got, err := validateRealtimeUpstreamURL("http://127.0.0.1:8000/v1", "127.0.0.1:4000", nil); err != nil || got != "http://127.0.0.1:8000/v1" {
		t.Fatalf("loopback development origin rejected: got=%q err=%v", got, err)
	}
	if _, err := validateRealtimeUpstreamURL("http://169.254.169.254/v1", "203.0.113.10:4000", nil); err == nil {
		t.Fatal("worker-controlled metadata-service SSRF origin was accepted")
	}
	allowed := map[string]bool{"https://gpu.example.test:8443": true}
	if _, err := validateRealtimeUpstreamURL("https://gpu.example.test:8443/v1", "203.0.113.10:4000", allowed); err != nil {
		t.Fatalf("operator-allowlisted TLS origin rejected: %v", err)
	}
	if _, err := validateRealtimeUpstreamURL("http://gpu.example.test:8000/v1", "203.0.113.10:4000", map[string]bool{"http://gpu.example.test:8000": true}); err == nil {
		t.Fatal("operator allowlist bypassed the non-loopback TLS requirement")
	}
}

func TestRealtimeHTTPClientDoesNotFollowWorkerRedirects(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/private", http.StatusFound)
	}))
	defer redirect.Close()
	response, err := newRealtimeHTTPClient().Get(redirect.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("worker redirect was followed: status=%d", response.StatusCode)
	}
}

func TestProxySSEBuildsUsageBoundHashChain(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl_test","choices":[{"delta":{"content":"hel"}}],"usage":null}`,
		"",
		`data: {"id":"chatcmpl_test","choices":[{"delta":{"content":"lo"}}],"usage":null}`,
		"",
		`data: {"id":"chatcmpl_test","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n") + "\n"
	recorder := httptest.NewRecorder()
	tracker := newStreamEvidenceTracker(time.Now())
	if err := proxySSE(recorder, strings.NewReader(stream), tracker); err != nil {
		t.Fatal(err)
	}
	if recorder.Body.String() != stream {
		t.Fatalf("proxy changed compatible SSE bytes\nwant: %q\n got: %q", stream, recorder.Body.String())
	}
	evidence, err := tracker.evidence(uuid.New(), 200, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.StreamEventCount != 4 || evidence.TotalTokens != 6 || evidence.PromptTokens != 4 || evidence.CompletionTokens != 2 {
		t.Fatalf("unexpected evidence: %+v", evidence)
	}
	if len(evidence.StreamRootSHA256) != 64 || len(evidence.OutputCommitment) != 64 {
		t.Fatal("stream evidence did not contain sha256 commitments")
	}
}

func TestProxySSERejectsWorkerDeathInsideEvent(t *testing.T) {
	recorder := httptest.NewRecorder()
	tracker := newStreamEvidenceTracker(time.Now())
	err := proxySSE(recorder, bytes.NewBufferString(`data: {"id":"partial"}`), tracker)
	if err == nil || !strings.Contains(err.Error(), "ended inside an event") {
		t.Fatalf("partial worker stream was not rejected: %v", err)
	}
	if _, err := tracker.evidence(uuid.New(), 200, time.Second); err == nil {
		t.Fatal("partial stream produced valid execution evidence")
	}
}

func TestRealtimeOfferRequiresPositiveGrossContributionInEveryTokenClass(t *testing.T) {
	profile := VLLMRuntimeProfile{
		BuyerInputUSDPerMillionTokens: 0.10, BuyerOutputUSDPerMillionTokens: 0.40,
	}
	valid := RealtimeOfferRegistration{
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}
	if err := validateRealtimeOfferRates(profile, valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RealtimeOfferRegistration){
		"zero floor":       func(r *RealtimeOfferRegistration) { r.SupplierInputUSDPerMillionTokens = 0 },
		"equal input rate": func(r *RealtimeOfferRegistration) { r.SupplierInputUSDPerMillionTokens = 0.10 },
		"equal output rate": func(r *RealtimeOfferRegistration) {
			r.SupplierOutputUSDPerMillionTokens = 0.40
		},
		"sub-nano spread": func(r *RealtimeOfferRegistration) {
			r.SupplierInputUSDPerMillionTokens = 0.099
		},
		"non-finite": func(r *RealtimeOfferRegistration) { r.SupplierOutputUSDPerMillionTokens = math.Inf(1) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := validateRealtimeOfferRates(profile, candidate); err == nil {
				t.Fatal("economically invalid offer passed")
			}
		})
	}
}
