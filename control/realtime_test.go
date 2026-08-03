package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

// TestRealtimeHTTPClientIdlePoolCoversOfferConcurrency guards the concurrency-
// scaling gateway tax fixed in newRealtimeHTTPClient. Go's DefaultTransport
// sets MaxIdleConnsPerHost=2 (DefaultMaxIdleConnsPerHost). Under concurrent
// streaming to one worker origin that leaves finished connections closed, so
// c=32 redials while c=1 reuses. The idle budget must cover the agent default
// max_active_sequences (128), and MaxIdleConns must not silently re-cap below
// that. Reverting to http.DefaultTransport fails this test.
func TestRealtimeHTTPClientIdlePoolCoversOfferConcurrency(t *testing.T) {
	client := newRealtimeHTTPClient()
	if client.Transport == http.DefaultTransport {
		t.Fatal("realtime client shares http.DefaultTransport; concurrent streaming would inherit MaxIdleConnsPerHost=2")
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("realtime client transport type %T, want *http.Transport", client.Transport)
	}
	if tr.MaxIdleConnsPerHost < realtimeMaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConnsPerHost=%d, want >= %d (agent default max_active_sequences); DefaultMaxIdleConnsPerHost=2 is the concurrency tax",
			tr.MaxIdleConnsPerHost, realtimeMaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConns=%d < MaxIdleConnsPerHost=%d; global idle cap would re-impose the tax",
			tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	// Pin the derivation: the constant is the agent default, not a round guess.
	if realtimeMaxIdleConnsPerHost != 128 {
		t.Fatalf("realtimeMaxIdleConnsPerHost=%d, want 128 (agent default_max_active_sequences)", realtimeMaxIdleConnsPerHost)
	}
}

// TestRealtimeHTTPClientReusesConnectionsUnderConcurrency is the behavioural
// twin of TestRealtimeHTTPClientIdlePoolCoversOfferConcurrency: under a wave
// of concurrent streaming requests to one origin, dials must stay near the
// concurrency level (reuse), not near the request count (no reuse). With
// MaxIdleConnsPerHost=2 almost every request after the first two dials cold.
func TestRealtimeHTTPClientReusesConnectionsUnderConcurrency(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the connection briefly so concurrent requests actually overlap
		// and then return to the idle pool for reuse by the next wave.
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
		flusher.Flush()
		time.Sleep(5 * time.Millisecond)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	client := newRealtimeHTTPClient()
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T", client.Transport)
	}
	// Count dials without changing the idle-pool settings under test.
	var dials atomic.Int64
	prev := tr.DialContext
	if prev == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		prev = d.DialContext
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return prev(ctx, network, addr)
	}

	const concurrency = 16
	const waves = 4
	// waves * concurrency requests; with reuse, dials ≈ concurrency (one wave
	// of connections reused). With DefaultMaxIdleConnsPerHost=2, dials ≈
	// almost every request after idle slots fill.
	total := concurrency * waves
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req, err := http.NewRequest(http.MethodPost, upstream.URL, strings.NewReader(`{}`))
			if err != nil {
				t.Errorf("new request: %v", err)
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("do: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()

	got := dials.Load()
	// Allow a small surplus for racing dials at wave boundaries, but refuse
	// the default-transport failure mode (dials ≈ total).
	if got > int64(concurrency*2) {
		t.Fatalf("client dialed %d times for %d requests at concurrency %d; idle pool is not reusing (DefaultMaxIdleConnsPerHost=2 fails here with dials≈requests)",
			got, total, concurrency)
	}
	if got < 1 {
		t.Fatal("expected at least one dial")
	}
	t.Logf("dials=%d total_requests=%d concurrency=%d (reuse ok)", got, total, concurrency)
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
