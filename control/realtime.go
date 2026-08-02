package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	maxRealtimeRequestBytes  = 4 << 20
	maxRealtimeResponseBytes = 16 << 20
	maxSSELineBytes          = 1 << 20
	maxSSEEventBytes         = 2 << 20
	defaultRealtimeTimeout   = 2 * time.Minute

	// bytesPerTokenEstimate converts a UTF-8 byte count into an optimistic token
	// count for the admission check.  Four is the usual English average; this
	// value only ever makes admission MORE permissive, and a request admitted
	// here but genuinely over-long is rejected cleanly by the engine.
	bytesPerTokenEstimate = 4
)

type openAIErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   any    `json:"param"`
		Code    string `json:"code"`
	} `json:"error"`
}

func writeOpenAIError(w http.ResponseWriter, status int, message, errorType, code string) {
	var envelope openAIErrorEnvelope
	envelope.Error.Message = message
	envelope.Error.Type = errorType
	envelope.Error.Param = nil
	envelope.Error.Code = code
	writeJSON(w, status, envelope)
}

func realtimeAllowedOrigins() map[string]bool {
	allowed := make(map[string]bool)
	for _, raw := range strings.Split(os.Getenv("MERC_VLLM_ALLOWED_ORIGINS"), ",") {
		if origin := strings.TrimRight(strings.TrimSpace(raw), "/"); origin != "" {
			allowed[origin] = true
		}
	}
	return allowed
}

func parsedRemoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}

func validateRealtimeUpstreamURL(raw, remoteAddr string, allowed map[string]bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("upstream_base_url must be an absolute URL without credentials, query, or fragment")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1"
	}
	if u.Path != "/v1" {
		return "", errors.New("upstream_base_url path must be /v1")
	}
	u.RawPath = ""
	origin := strings.TrimRight(u.Scheme+"://"+u.Host, "/")
	requestIP := parsedRemoteIP(remoteAddr)
	upstreamIP := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && (requestIP == nil || upstreamIP == nil ||
		!requestIP.IsLoopback() || !upstreamIP.IsLoopback()) {
		return "", errors.New("non-loopback upstreams must use https")
	}
	if allowed[origin] {
		if u.Scheme != "https" && u.Scheme != "http" {
			return "", errors.New("upstream_base_url must use http or https")
		}
		return strings.TrimRight(u.String(), "/"), nil
	}

	if requestIP == nil || upstreamIP == nil || !requestIP.Equal(upstreamIP) {
		return "", errors.New("upstream origin is not operator-allowlisted and does not match the authenticated worker source IP")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("upstream_base_url must use http or https")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func validateRealtimeOfferRegistration(reg *RealtimeOfferRegistration, remoteAddr string) (VLLMRuntimeProfile, error) {
	profile, ok := vllmProfileByID(strings.TrimSpace(reg.RuntimeProfileID))
	if !ok {
		return VLLMRuntimeProfile{}, errors.New("unknown runtime_profile_id")
	}
	if reg.RuntimeProfileSHA256 != profile.ProfileSHA256 {
		return VLLMRuntimeProfile{}, errors.New("runtime profile digest does not match control-plane authority")
	}
	if !strings.HasPrefix(reg.UpstreamToken, "cx_vllm_") || len(reg.UpstreamToken) < 24 || len(reg.UpstreamToken) > 256 {
		return VLLMRuntimeProfile{}, errors.New("upstream_token must be a generated cx_vllm_ credential")
	}
	baseURL, err := validateRealtimeUpstreamURL(reg.UpstreamBaseURL, remoteAddr, realtimeAllowedOrigins())
	if err != nil {
		return VLLMRuntimeProfile{}, err
	}
	reg.UpstreamBaseURL = baseURL
	switch reg.Warmth {
	case "HOT", "WARM", "CACHED", "COLD":
	default:
		return VLLMRuntimeProfile{}, errors.New("warmth must be HOT, WARM, CACHED, or COLD")
	}
	if reg.MaxActiveSequences < 1 || reg.MaxActiveSequences > 100000 ||
		reg.AvailableSequences < 0 || reg.AvailableSequences > reg.MaxActiveSequences {
		return VLLMRuntimeProfile{}, errors.New("invalid active sequence capacity")
	}
	if err := validateRealtimeOfferRates(profile, *reg); err != nil {
		return VLLMRuntimeProfile{}, err
	}
	if _, err := newRealtimePlacementPlan(profile, *reg); err != nil {
		return VLLMRuntimeProfile{}, fmt.Errorf("placement refused: %w", err)
	}
	return profile, nil
}

func validateRealtimeOfferRates(profile VLLMRuntimeProfile, reg RealtimeOfferRegistration) error {
	buyerInput, err := nanoRatePerMillionFromFloat(profile.BuyerInputUSDPerMillionTokens)
	if err != nil || buyerInput <= 0 {
		return errors.New("buyer input token rate lacks exact positive authority")
	}
	buyerOutput, err := nanoRatePerMillionFromFloat(profile.BuyerOutputUSDPerMillionTokens)
	if err != nil || buyerOutput <= 0 {
		return errors.New("buyer output token rate lacks exact positive authority")
	}
	supplierInput, err := nanoRatePerMillionFromFloat(reg.SupplierInputUSDPerMillionTokens)
	if err != nil || supplierInput <= 0 {
		return errors.New("supplier input token rate must be finite and positive")
	}
	supplierOutput, err := nanoRatePerMillionFromFloat(reg.SupplierOutputUSDPerMillionTokens)
	if err != nil || supplierOutput <= 0 {
		return errors.New("supplier output token rate must be finite and positive")
	}
	// With buyer products rounded down and supplier products rounded up, a rate
	// delta of one divisor can still collapse to zero on a single token when the
	// supplier product has a fractional nano. Two divisors guarantee at least one
	// exact nano of gross spread for every positive count in either class.
	const minimumRealtimeRateSpreadNanos = 2 * 1_000_000
	if int64(buyerInput-supplierInput) < minimumRealtimeRateSpreadNanos ||
		int64(buyerOutput-supplierOutput) < minimumRealtimeRateSpreadNanos {
		return errors.New("supplier token rates do not leave one exact nano of gross Merc contribution for every positive token class")
	}
	return nil
}

func (s *Server) handleRealtimeWorkerRegister(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
	if err != nil || len(raw) > 64<<10 {
		writeErr(w, http.StatusBadRequest, "invalid realtime offer json")
		return
	}
	var registration RealtimeOfferRegistration
	if err := decodeStrictJSONObject(raw, &registration); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid realtime offer json: "+err.Error())
		return
	}
	profile, err := validateRealtimeOfferRegistration(&registration, r.RemoteAddr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "realtime offer rejected: "+err.Error())
		return
	}
	if err := s.store.UpsertRealtimeOffer(r.Context(), *auth, registration); err != nil {
		writeErr(w, http.StatusInternalServerError, "realtime offer registration failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                 "ACTIVE",
		"runtime_profile_id":     profile.RuntimeProfileID,
		"runtime_profile_sha256": profile.ProfileSHA256,
		"tensor_parallel_size":   profile.TensorParallelSize,
		"model":                  profile.ModelAlias,
	})
}

func (s *Server) handleRealtimeWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	var hb RealtimeOfferHeartbeat
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<10+1))
	if err != nil || len(raw) > 32<<10 || decodeStrictJSONObject(raw, &hb) != nil {
		writeErr(w, http.StatusBadRequest, "invalid realtime heartbeat json")
		return
	}
	if _, ok := vllmProfileByID(hb.RuntimeProfileID); !ok {
		writeErr(w, http.StatusBadRequest, "unknown runtime_profile_id")
		return
	}
	switch hb.Warmth {
	case "HOT", "WARM", "CACHED", "COLD":
	default:
		writeErr(w, http.StatusBadRequest, "invalid warmth")
		return
	}
	switch hb.Status {
	case "ACTIVE", "DRAINING", "FAILED", "QUARANTINED":
	default:
		writeErr(w, http.StatusBadRequest, "invalid realtime worker status")
		return
	}
	if hb.AvailableSequences < 0 {
		writeErr(w, http.StatusBadRequest, "available_sequences must be non-negative")
		return
	}
	if err := s.store.HeartbeatRealtimeOffer(r.Context(), *auth, hb); errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "realtime offer not registered")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "realtime heartbeat failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type preparedRealtimeRequest struct {
	Body                      []byte
	Profile                   VLLMRuntimeProfile
	Stream                    bool
	InputCommitment           string
	RequestSHA256             string
	MaximumPriceUSD           float64
	EstimatedPriceUSD         float64
	MaxPriceCeiling           float64
	MaximumPromptTokens       int64
	MaximumCompletionTokens   int64
	EstimatedPromptTokens     int64
	EstimatedCompletionTokens int64
}

func jsonInt(value any) (int64, bool) {
	switch value := value.(type) {
	case json.Number:
		n, err := value.Int64()
		return n, err == nil
	case float64:
		return int64(value), value == float64(int64(value))
	case int:
		return int64(value), true
	default:
		return 0, false
	}
}

func prepareRealtimeRequest(raw []byte, headerCeiling string) (preparedRealtimeRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return preparedRealtimeRequest{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return preparedRealtimeRequest{}, errors.New("request body must contain one JSON object")
	}
	model, ok := payload["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return preparedRealtimeRequest{}, errors.New("model is required")
	}
	profile, ok := vllmProfileForModel(model)
	if !ok {
		return preparedRealtimeRequest{}, fmt.Errorf("model %q is not available for realtime inference", model)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return preparedRealtimeRequest{}, errors.New("messages must be a non-empty array")
	}
	stream := false
	if rawStream, exists := payload["stream"]; exists {
		var valid bool
		stream, valid = rawStream.(bool)
		if !valid {
			return preparedRealtimeRequest{}, errors.New("stream must be a boolean")
		}
	}
	maxOutput := int64(profile.GenerationPolicy.MaximumOutputTokens)
	if _, modern := payload["max_completion_tokens"]; modern {
		if _, legacy := payload["max_tokens"]; legacy {
			return preparedRealtimeRequest{}, errors.New("max_tokens and max_completion_tokens cannot both be set")
		}
	}
	for _, field := range []string{"max_completion_tokens", "max_tokens"} {
		if value, exists := payload[field]; exists {
			n, valid := jsonInt(value)
			if !valid || n < 1 || n > int64(profile.GenerationPolicy.MaximumOutputTokens) {
				return preparedRealtimeRequest{}, fmt.Errorf("%s must be an integer between 1 and %d", field, profile.GenerationPolicy.MaximumOutputTokens)
			}
			maxOutput = n
			break
		}
	}
	if _, exists := payload["max_completion_tokens"]; !exists {
		if _, legacy := payload["max_tokens"]; !legacy {
			payload["max_completion_tokens"] = maxOutput
		}
	}
	if _, exists := payload["temperature"]; !exists {
		payload["temperature"] = profile.GenerationPolicy.Temperature
	}
	if _, exists := payload["top_p"]; !exists {
		payload["top_p"] = profile.GenerationPolicy.TopP
	}
	if stream {
		streamOptions, valid := payload["stream_options"].(map[string]any)
		if _, exists := payload["stream_options"]; exists && !valid {
			return preparedRealtimeRequest{}, errors.New("stream_options must be an object")
		}
		if streamOptions == nil {
			streamOptions = make(map[string]any)
		}
		streamOptions["include_usage"] = true
		payload["stream_options"] = streamOptions
	}

	var requestCeiling float64
	if rawCX, exists := payload["cx"]; exists {
		cx, ok := rawCX.(map[string]any)
		if !ok {
			return preparedRealtimeRequest{}, errors.New("cx must be an object")
		}
		if value, exists := cx["maximum_price_usd"]; exists {
			switch value := value.(type) {
			case json.Number:
				requestCeiling, _ = strconv.ParseFloat(value.String(), 64)
			case float64:
				requestCeiling = value
			}
			if requestCeiling <= 0 {
				return preparedRealtimeRequest{}, errors.New("cx.maximum_price_usd must be positive")
			}
		}
		delete(payload, "cx")
	}
	if strings.TrimSpace(headerCeiling) != "" {
		ceiling, err := strconv.ParseFloat(strings.TrimSpace(headerCeiling), 64)
		if err != nil || ceiling <= 0 {
			return preparedRealtimeRequest{}, errors.New("X-Merc-Max-USD must be a positive number")
		}
		if requestCeiling == 0 || ceiling < requestCeiling {
			requestCeiling = ceiling
		}
	}

	upstreamBody, err := canonicalJSON(payload)
	if err != nil {
		return preparedRealtimeRequest{}, err
	}
	inputObject := map[string]any{
		"route":                  "/v1/chat/completions",
		"runtime_profile_id":     profile.RuntimeProfileID,
		"runtime_profile_sha256": profile.ProfileSHA256,
		"payload":                payload,
	}
	canonicalInput, err := canonicalJSON(inputObject)
	if err != nil {
		return preparedRealtimeRequest{}, err
	}
	inputDigest := sha256.Sum256(canonicalInput)
	requestDigest := sha256.Sum256(raw)
	// Two different bounds are needed here and they must point in opposite
	// directions, which is why one number was previously doing both jobs badly.
	//
	// Every token is at least one byte, so the UTF-8 byte count is an UPPER bound
	// on the token count. That is the right thing to reserve money against:
	// over-reserving is safe, and actual usage is reconciled from the engine's
	// own tokenizer at settlement.
	//
	// It is the wrong thing to admission-check against. Rejecting when
	// bytes > context_budget rejects any request whose bytes exceed the budget
	// even though its real token count is typically ~4x smaller -- so a profile
	// advertising 32,768 tokens only accepted about 7,000 tokens' worth of text.
	// Admission must use a LOWER bound on tokens and refuse only when even the
	// optimistic estimate cannot fit; a request that slips through fails cleanly
	// at the engine, which is far better than silently refusing three quarters of
	// legitimate traffic.
	maxInputTokens := int64(len(upstreamBody))
	estimatedInputTokens := maxInputTokens / bytesPerTokenEstimate
	if estimatedInputTokens < 1 {
		estimatedInputTokens = 1
	}
	if contextBudget := int64(profile.MaxModelLength) - maxOutput; estimatedInputTokens > contextBudget {
		return preparedRealtimeRequest{}, errors.New("request exceeds the runtime profile context bound")
	}
	maximumPrice, err := tokenCharge(maxInputTokens, maxOutput,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	if err != nil {
		return preparedRealtimeRequest{}, fmt.Errorf("derive maximum realtime price: %w", err)
	}
	estimatedPrice, err := tokenCharge(estimatedInputTokens, (maxOutput+1)/2,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	if err != nil {
		return preparedRealtimeRequest{}, fmt.Errorf("derive estimated realtime price: %w", err)
	}
	if estimatedPrice > maximumPrice {
		estimatedPrice = maximumPrice
	}
	if requestCeiling > 0 && maximumPrice > requestCeiling {
		return preparedRealtimeRequest{}, fmt.Errorf("maximum quoted price %.6f USD exceeds buyer ceiling %.6f USD", maximumPrice, requestCeiling)
	}
	return preparedRealtimeRequest{
		Body: upstreamBody, Profile: profile, Stream: stream,
		InputCommitment: hex.EncodeToString(inputDigest[:]),
		RequestSHA256:   hex.EncodeToString(requestDigest[:]),
		MaximumPriceUSD: maximumPrice, EstimatedPriceUSD: estimatedPrice,
		MaxPriceCeiling: requestCeiling, MaximumPromptTokens: maxInputTokens,
		MaximumCompletionTokens: maxOutput, EstimatedPromptTokens: estimatedInputTokens,
		EstimatedCompletionTokens: (maxOutput + 1) / 2,
	}, nil
}

// boundCompletionTokens refuses a bill the observed output cannot support.
//
// Token counts always arrive in the upstream's own usage message, and the
// upstream is the supplier's runtime, so on their own they are the supplier
// writing its own invoice. Generated bytes are the independent measure: the
// control plane proxies and hashes the response itself.
//
// No tokenizer emits a token in under one byte, so bytes are a sound ceiling -
// an honest response can never trip it. It is a ceiling and not a
// reconciliation: roughly 4x inflation on ordinary English still passes. The
// point is to make the fraudulent case impossible, not to price from bytes.
func boundCompletionTokens(completionTokens, generatedBytes int64) error {
	if completionTokens > generatedBytes {
		return fmt.Errorf(
			"upstream billed %d completion tokens against %d generated bytes observed in the response",
			completionTokens, generatedBytes)
	}
	return nil
}

type streamEvidenceTracker struct {
	previous         [32]byte
	output           hash.Hash
	events           int64
	usageSeen        bool
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
	// generatedBytes is the only independent measure of how much work the
	// upstream actually did. Token counts arrive in the upstream's own final
	// usage message, so on their own they are the supplier's assertion about
	// the supplier's bill; this is counted from bytes the control plane saw.
	generatedBytes int64
	upstreamID     string
	startedAt      time.Time
	firstEventAt   time.Time
}

func newStreamEvidenceTracker(startedAt time.Time) *streamEvidenceTracker {
	return &streamEvidenceTracker{output: sha256.New(), startedAt: startedAt}
}

func (t *streamEvidenceTracker) addEvent(event []byte) error {
	if t.events == 0 {
		t.firstEventAt = time.Now()
	}
	eventDigest := sha256.Sum256(event)
	h := sha256.New()
	_, _ = h.Write(t.previous[:])
	var sequence [8]byte
	binary.BigEndian.PutUint64(sequence[:], uint64(t.events))
	_, _ = h.Write(sequence[:])
	_, _ = h.Write(eventDigest[:])
	copy(t.previous[:], h.Sum(nil))
	t.events++

	for _, line := range bytes.Split(event, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(data, []byte("[DONE]")) || len(data) == 0 {
			continue
		}
		var chunk struct {
			ID string `json:"id"`
			// Raw rather than typed: an upstream that returns content in a
			// shape this struct does not expect would fail the whole unmarshal
			// and turn an honest response into a rejection. Raw length
			// over-counts by the surrounding quotes, which only loosens a
			// ceiling that must never refuse real work.
			Choices []struct {
				Delta struct {
					Content   json.RawMessage `json:"content"`
					Reasoning json.RawMessage `json:"reasoning_content"`
					ToolCalls []struct {
						Function struct {
							Name      json.RawMessage `json:"name"`
							Arguments json.RawMessage `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				Text json.RawMessage `json:"text"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				TotalTokens      int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("upstream emitted malformed SSE JSON: %w", err)
		}
		canonical, err := canonicalJSON(json.RawMessage(data))
		if err != nil {
			return err
		}
		_, _ = t.output.Write(canonical)
		if chunk.ID != "" {
			t.upstreamID = chunk.ID
		}
		// Every shape the upstream can bill for: chat deltas, legacy completion
		// text, tool-call arguments, and reasoning traces that never reach the
		// buyer as content but are still generated tokens.
		for _, choice := range chunk.Choices {
			t.generatedBytes += int64(len(choice.Delta.Content))
			t.generatedBytes += int64(len(choice.Delta.Reasoning))
			t.generatedBytes += int64(len(choice.Text))
			for _, call := range choice.Delta.ToolCalls {
				t.generatedBytes += int64(len(call.Function.Name))
				t.generatedBytes += int64(len(call.Function.Arguments))
			}
		}
		if chunk.Usage != nil {
			t.usageSeen = true
			t.promptTokens = chunk.Usage.PromptTokens
			t.completionTokens = chunk.Usage.CompletionTokens
			t.totalTokens = chunk.Usage.TotalTokens
		}
	}
	return nil
}

func (t *streamEvidenceTracker) evidence(executionID uuid.UUID, status int, duration time.Duration) (RealtimeExecutionEvidence, error) {
	if t.events == 0 || !t.usageSeen || t.promptTokens < 0 || t.completionTokens < 0 ||
		t.totalTokens != t.promptTokens+t.completionTokens {
		return RealtimeExecutionEvidence{}, errors.New("upstream stream did not include reconcilable final usage")
	}
	if err := boundCompletionTokens(t.completionTokens, t.generatedBytes); err != nil {
		return RealtimeExecutionEvidence{}, err
	}
	ttfe := int64(0)
	if !t.firstEventAt.IsZero() {
		ttfe = t.firstEventAt.Sub(t.startedAt).Milliseconds()
	}
	return RealtimeExecutionEvidence{
		ID: executionID, UpstreamRequestID: t.upstreamID, HTTPStatus: status,
		StreamEventCount: t.events, StreamRootSHA256: hex.EncodeToString(t.previous[:]),
		OutputCommitment: hex.EncodeToString(t.output.Sum(nil)), PromptTokens: t.promptTokens,
		CompletionTokens: t.completionTokens, TotalTokens: t.totalTokens,
		TimeToFirstEventMS: ttfe, DurationMS: duration.Milliseconds(),
	}, nil
}

func proxySSE(w http.ResponseWriter, body io.Reader, tracker *streamEvidenceTracker) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), maxSSELineBytes)
	var event bytes.Buffer
	for scanner.Scan() {
		line := scanner.Bytes()
		if event.Len()+len(line)+1 > maxSSEEventBytes {
			return errors.New("upstream SSE event exceeds the bounded event size")
		}
		event.Write(line)
		event.WriteByte('\n')
		if len(line) != 0 {
			continue
		}
		payload := append([]byte(nil), event.Bytes()...)
		if err := tracker.addEvent(payload); err != nil {
			return err
		}
		if _, err := w.Write(payload); err != nil {
			return err
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return err
		}
		event.Reset()
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if event.Len() != 0 {
		return errors.New("upstream SSE stream ended inside an event")
	}
	return nil
}

func realtimeUpstreamURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/chat/completions"
}

func newRealtimeHTTPClient() *http.Client {
	return &http.Client{
		Transport: http.DefaultTransport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// A registered worker origin must not redirect the gateway (or its
			// bearer credential) to an origin that did not pass registration.
			return http.ErrUseLastResponse
		},
	}
}

func finalizeRealtimeFailure(store *Store, contractID, executionID uuid.UUID, status int, started time.Time, code string, err error, cancelled bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	detail := code
	if err != nil {
		detail = err.Error()
	}
	finalized, finalizeErr := store.FinalizeRealtimeFailure(ctx, contractID, executionID, status,
		time.Since(started).Milliseconds(), code, detail, cancelled)
	if finalizeErr != nil {
		metrics.realtimeFinalizationErrors.Add(1)
		return
	}
	if finalized {
		if cancelled {
			metrics.realtimeCancelled.Add(1)
		} else {
			metrics.realtimeFailed.Add(1)
		}
	}
}

// realtimePathTiming is an opt-in stage clock for gateway overhead work.
// Enabled with MERC_REALTIME_PATH_TIMING=1; emits one structured log line per
// request. Never gates or alters the request path.
type realtimePathTiming struct {
	enabled bool
	t0      time.Time
	marks   map[string]time.Duration
}

func newRealtimePathTiming() *realtimePathTiming {
	if os.Getenv("MERC_REALTIME_PATH_TIMING") != "1" {
		return &realtimePathTiming{}
	}
	return &realtimePathTiming{enabled: true, t0: time.Now(), marks: make(map[string]time.Duration, 12)}
}

func (p *realtimePathTiming) mark(stage string, since time.Time) {
	if p == nil || !p.enabled {
		return
	}
	p.marks[stage] = time.Since(since)
}

func (p *realtimePathTiming) log(stream bool, contractID string) {
	if p == nil || !p.enabled {
		return
	}
	log.Printf("realtime_path_timing stream=%v contract=%s total_ms=%.3f stages_ms=%s",
		stream, contractID, float64(time.Since(p.t0).Microseconds())/1000.0, formatRealtimePathMarks(p.marks))
}

func formatRealtimePathMarks(marks map[string]time.Duration) string {
	// Stable order so a human can scan logs without sorting in their head.
	order := []string{
		"read_body", "prepare_json", "intake_control", "exact_reuse",
		"authorize_contract", "upstream_ttfb", "proxy_sse", "read_json_body",
		"usage_reconcile", "settlement", "exact_cache_store",
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		if d, ok := marks[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%.3f", k, float64(d.Microseconds())/1000.0))
		}
	}
	return strings.Join(parts, ",")
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	pathTiming := newRealtimePathTiming()
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	stage := time.Now()
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRealtimeRequestBytes+1))
	pathTiming.mark("read_body", stage)
	if err != nil || len(raw) > maxRealtimeRequestBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request body exceeds the realtime limit", "invalid_request_error", "request_too_large")
		return
	}
	stage = time.Now()
	prepared, err := prepareRealtimeRequest(raw, r.Header.Get("X-Merc-Max-USD"))
	pathTiming.mark("prepare_json", stage)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_request")
		return
	}
	stage = time.Now()
	paused, err := s.store.OperationalControlPaused(r.Context(), controlIntake)
	pathTiming.mark("intake_control", stage)
	if err != nil || paused {
		writeOpenAIError(w, http.StatusServiceUnavailable, "realtime intake is unavailable", "server_error", "intake_unavailable")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey != "" && !idempotencyKeyPattern.MatchString(idempotencyKey) {
		writeOpenAIError(w, http.StatusBadRequest, "Idempotency-Key must be 8-128 safe ASCII characters", "invalid_request_error", "invalid_idempotency_key")
		return
	}
	requestID := w.Header().Get("X-Request-ID")
	if requestID == "" {
		requestID = "req_" + uuid.NewString()
	}

	// Exact deterministic reuse: when the request is cacheable and a prior
	// result exists, bill the reuse class and serve without scheduling a
	// supplier. Non-deterministic sampling is simply not eligible (miss).
	//
	// Stream requests skip this path entirely. The cache stores a non-stream
	// JSON body; serving it for stream:true would break the OpenAI SSE
	// contract the client asked for. Skipping also removes a DB round-trip
	// from the TTFT-critical path of every streaming completion.
	if !prepared.Stream {
		stage = time.Now()
		if reuseContract, reuseBody, reuseHit, err := s.tryRealtimeExactReuse(
			r.Context(), auth.BuyerID, requestID, idempotencyKey, prepared); err != nil {
			pathTiming.mark("exact_reuse", stage)
			if errors.Is(err, errRealtimeInsufficientFunds) {
				writeOpenAIError(w, http.StatusPaymentRequired, err.Error(), "insufficient_quota", "insufficient_quota")
				return
			}
			// Cache/storage faults fall through to live execution rather than 5xx
			// the buyer for an optimization path.
			log.Printf("exact reuse lookup failed, executing live: %v", err)
		} else if reuseHit {
			pathTiming.mark("exact_reuse", stage)
			receiptPath := "/v1/realtime/requests/" + reuseContract.ID.String() + "/receipt"
			w.Header().Set("X-Merc-Contract-ID", reuseContract.ID.String())
			w.Header().Set("X-Merc-Receipt", receiptPath)
			w.Header().Set("X-Merc-Max-USD", fmt.Sprintf("%.6f", reuseContract.MaximumPriceUSD))
			w.Header().Set("X-Merc-Exact-Reuse", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(reuseBody)
			metrics.realtimeVerified.Add(1)
			pathTiming.log(false, reuseContract.ID.String())
			return
		} else {
			pathTiming.mark("exact_reuse", stage)
		}
	}

	// The cache missed. That leaves the other way the work can already be
	// happening: an identical request from this same tenant executing right now.
	// The cache serves repeats over time; this serves arrivals that overlap,
	// before there is anything to cache.
	//
	// Streaming is excluded for the same reason exact reuse is: a follower would
	// receive a complete JSON body for a request that asked for SSE.
	var coalesceLeaderRef string
	if !prepared.Stream {
		stage = time.Now()
		identity, leaderRef, coalescedContract, coalescedBody, followed, coalesceErr :=
			s.tryRealtimeCoalescedDelivery(r.Context(), auth.BuyerID, requestID, idempotencyKey, prepared)
		pathTiming.mark("coalesce", stage)
		switch {
		case coalesceErr != nil:
			if errors.Is(coalesceErr, errRealtimeInsufficientFunds) {
				writeOpenAIError(w, http.StatusPaymentRequired, coalesceErr.Error(), "insufficient_quota", "insufficient_quota")
				return
			}
			log.Printf("coalesced delivery failed, executing live: %v", coalesceErr)
		case followed:
			receiptPath := "/v1/realtime/requests/" + coalescedContract.ID.String() + "/receipt"
			w.Header().Set("X-Merc-Contract-ID", coalescedContract.ID.String())
			w.Header().Set("X-Merc-Receipt", receiptPath)
			w.Header().Set("X-Merc-Max-USD", fmt.Sprintf("%.6f", coalescedContract.MaximumPriceUSD))
			// A distinct header from X-Merc-Exact-Reuse. The buyer got the same
			// discount and it came from a different mechanism; reporting them as
			// one would make the two impossible to tell apart in the field.
			w.Header().Set("X-Merc-Coalesced", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(coalescedBody)
			metrics.realtimeVerified.Add(1)
			pathTiming.log(false, coalescedContract.ID.String())
			return
		default:
			// This caller leads. It executes below and publishes to whoever
			// joined behind it once its result is stored.
			coalesceLeaderRef = leaderRef
			if leaderRef != "" {
				// Every path out of this handler that is not a successful
				// publish must release the followers. There are a dozen such
				// exits — every upstream error, every settlement failure, every
				// early return — and one of them being forgotten leaves a set of
				// callers waiting out the full lease for an answer that is never
				// coming. A defer covers all of them at once, and it is inert
				// after a successful publish because ResolveInflightFailure only
				// matches a row still RUNNING under this leader.
				defer func() {
					releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := s.store.ResolveInflightFailure(releaseCtx, identity, leaderRef,
						"leader returned without publishing a result"); err != nil {
						log.Printf("inflight release: %v", err)
					}
				}()
				// Renew the lease while the leader works.
				//
				// RenewInflightLease existed and had no caller, so the 30-second
				// inflightLeaseTTL was a hard ceiling on any coalesced execution
				// while the contract deadline is two minutes. A leader slower than
				// the TTL was legitimately taken over by the next arrival, and the
				// followers behind it were NOT re-collapsed onto the new leader:
				// AwaitInflightResult returns no-result on lease expiry and sends
				// every one of them off to execute alone. The failure mode was a
				// fan-out, not a stall.
				//
				// TTL/3 so two consecutive renewals can be lost before the lease
				// does. context.Background(), because the buyer's context ending is
				// exactly when the release defer below needs the lease to still be
				// ours. Registered AFTER that defer so LIFO stops the renewal first.
				renewCtx, stopRenew := context.WithCancel(context.Background())
				defer stopRenew()
				go func() {
					ticker := time.NewTicker(inflightLeaseTTL / 3)
					defer ticker.Stop()
					for {
						select {
						case <-renewCtx.Done():
							return
						case <-ticker.C:
							// A renewal that lands after a successful publish finds
							// the row COMPLETE rather than RUNNING and reports that
							// it is no longer held. That is the normal end of a
							// leader's life, not a fault, so it is not logged --
							// otherwise every execution whose length lands near a
							// multiple of TTL/3 logs an error on success.
							if err := s.store.RenewInflightLease(
								renewCtx, identity, leaderRef); err != nil &&
								!errors.Is(err, context.Canceled) {
								log.Printf("inflight renew (%s): %v", leaderRef, err)
							}
						}
					}
				}()
			}
		}
	}

	stage = time.Now()
	contract, replay, err := s.store.AuthorizeRealtimeContract(r.Context(), RealtimeContractAuthorization{
		RequestID: requestID, BuyerID: auth.BuyerID, Profile: prepared.Profile,
		InputCommitment: prepared.InputCommitment, RequestSHA256: prepared.RequestSHA256,
		MaximumPriceUSD: prepared.MaximumPriceUSD, EstimatedPriceUSD: prepared.EstimatedPriceUSD,
		MaximumPromptTokens:       prepared.MaximumPromptTokens,
		MaximumCompletionTokens:   prepared.MaximumCompletionTokens,
		EstimatedPromptTokens:     prepared.EstimatedPromptTokens,
		EstimatedCompletionTokens: prepared.EstimatedCompletionTokens,
		BuyerDeclaredCeilingUSD:   prepared.MaxPriceCeiling,
		DeadlineAt:                time.Now().Add(defaultRealtimeTimeout), IdempotencyKey: idempotencyKey,
	})
	pathTiming.mark("authorize_contract", stage)
	if errors.Is(err, errRealtimeIdempotencyConflict) {
		writeOpenAIError(w, http.StatusConflict, err.Error(), "invalid_request_error", "idempotency_conflict")
		return
	}
	if errors.Is(err, errRealtimeNoSupply) {
		s.recordRealtimeAdmissionEvent(r.Context(), auth.BuyerID, prepared.Profile.RuntimeProfileID,
			"", realtimeAdmissionNoCapacity, uuid.Nil)
		writeOpenAIError(w, http.StatusServiceUnavailable, "no compatible realtime capacity is currently available", "server_error", "no_capacity")
		return
	}
	if errors.Is(err, errRealtimeInsufficientFunds) {
		s.recordRealtimeAdmissionEvent(r.Context(), auth.BuyerID, prepared.Profile.RuntimeProfileID,
			"", realtimeAdmissionInsufficient, uuid.Nil)
		writeOpenAIError(w, http.StatusPaymentRequired, err.Error(), "insufficient_quota", "insufficient_quota")
		return
	}
	if err != nil {
		s.recordRealtimeAdmissionEvent(r.Context(), auth.BuyerID, prepared.Profile.RuntimeProfileID,
			"", realtimeAdmissionAuthorization, uuid.Nil)
		writeOpenAIError(w, http.StatusServiceUnavailable, "realtime contract authorization failed", "server_error", "authorization_failed")
		return
	}
	receiptPath := "/v1/realtime/requests/" + contract.ID.String() + "/receipt"
	w.Header().Set("X-Merc-Contract-ID", contract.ID.String())
	w.Header().Set("X-Merc-Receipt", receiptPath)
	w.Header().Set("X-Merc-Max-USD", fmt.Sprintf("%.6f", contract.MaximumPriceUSD))
	if replay {
		writeOpenAIError(w, http.StatusConflict, "idempotent request already has a contract; inspect X-Merc-Receipt", "invalid_request_error", "idempotent_replay")
		return
	}
	metrics.realtimeAuthorized.Add(1)
	s.recordRealtimeAdmissionEvent(r.Context(), auth.BuyerID, prepared.Profile.RuntimeProfileID,
		contract.PlacementPlan.HWClass, realtimeAdmissionAdmitted, contract.ID)

	started := time.Now()
	executionID := uuid.New()
	requestContext, cancel := context.WithDeadline(r.Context(), contract.DeadlineAt)
	defer cancel()
	upstreamRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost,
		realtimeUpstreamURL(contract.UpstreamBaseURL), bytes.NewReader(prepared.Body))
	if err != nil {
		finalizeRealtimeFailure(s.store, contract.ID, executionID, 0, started, "upstream_request_invalid", err, false)
		writeOpenAIError(w, http.StatusInternalServerError, "could not construct the upstream request", "server_error", "upstream_request_invalid")
		return
	}
	upstreamRequest.Header.Set("Authorization", "Bearer "+contract.UpstreamToken)
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Set("X-Request-ID", contract.RequestID)
	client := s.realtimeHTTPClient
	if client == nil {
		client = newRealtimeHTTPClient()
	}
	stage = time.Now()
	response, err := client.Do(upstreamRequest)
	pathTiming.mark("upstream_ttfb", stage)
	if err != nil {
		cancelled := errors.Is(requestContext.Err(), context.Canceled)
		code := "upstream_unavailable"
		if cancelled {
			code = "client_cancelled"
		}
		finalizeRealtimeFailure(s.store, contract.ID, executionID, 0, started, code, err, cancelled)
		if !cancelled {
			writeOpenAIError(w, http.StatusBadGateway, "realtime worker is unavailable", "server_error", code)
		}
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, "upstream_rejected", errors.New(http.StatusText(response.StatusCode)), false)
		writeOpenAIError(w, response.StatusCode, "realtime worker rejected the canonical request", "server_error", "upstream_rejected")
		return
	}

	if prepared.Stream {
		if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
			finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, "invalid_upstream_content_type", errors.New("upstream did not return text/event-stream"), false)
			writeOpenAIError(w, http.StatusBadGateway, "realtime worker returned an invalid stream", "server_error", "invalid_upstream_stream")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		tracker := newStreamEvidenceTracker(started)
		stage = time.Now()
		if err := proxySSE(w, response.Body, tracker); err != nil {
			pathTiming.mark("proxy_sse", stage)
			cancelled := errors.Is(requestContext.Err(), context.Canceled)
			code := "stream_interrupted"
			if cancelled {
				code = "client_cancelled"
			}
			finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, code, err, cancelled)
			pathTiming.log(true, contract.ID.String())
			return
		}
		pathTiming.mark("proxy_sse", stage)
		evidence, err := tracker.evidence(executionID, response.StatusCode, time.Since(started))
		if err != nil {
			finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, "usage_reconciliation_failed", err, false)
			pathTiming.log(true, contract.ID.String())
			return
		}
		settlementContext, settlementCancel := context.WithTimeout(context.Background(), 5*time.Second)
		stage = time.Now()
		_, err = s.store.FinalizeRealtimeSuccess(settlementContext, contract.ID, evidence)
		pathTiming.mark("settlement", stage)
		settlementCancel()
		if err != nil {
			metrics.realtimeFinalizationErrors.Add(1)
			finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, "settlement_failed", err, false)
		} else {
			metrics.realtimeVerified.Add(1)
		}
		pathTiming.log(true, contract.ID.String())
		return
	}

	stage = time.Now()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRealtimeResponseBytes+1))
	pathTiming.mark("read_json_body", stage)
	if err != nil || len(body) > maxRealtimeResponseBytes {
		finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, "upstream_response_invalid", errors.New("upstream response exceeded the bounded JSON response size"), false)
		writeOpenAIError(w, http.StatusBadGateway, "realtime worker returned an invalid response", "server_error", "upstream_response_invalid")
		return
	}
	stage = time.Now()
	var completion struct {
		ID string `json:"id"`
		// Raw for the same reason as the streaming path: shape must not be
		// able to turn an honest response into a rejection.
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				Reasoning json.RawMessage `json:"reasoning_content"`
				ToolCalls []struct {
					Function struct {
						Name      json.RawMessage `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			Text json.RawMessage `json:"text"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &completion); err != nil || completion.Usage.TotalTokens != completion.Usage.PromptTokens+completion.Usage.CompletionTokens {
		pathTiming.mark("usage_reconcile", stage)
		finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, "usage_reconciliation_failed", errors.New("upstream JSON response did not include valid usage"), false)
		writeOpenAIError(w, http.StatusBadGateway, "realtime worker returned unreconciled usage", "server_error", "usage_reconciliation_failed")
		return
	}
	bodyDigest := sha256.Sum256(body)
	// Same rule as the streaming path: the bill must be supported by output this
	// process actually received. Here the whole body is in hand, so the measure
	// is exact rather than accumulated.
	var generatedBytes int64
	for _, choice := range completion.Choices {
		generatedBytes += int64(len(choice.Message.Content))
		generatedBytes += int64(len(choice.Message.Reasoning))
		generatedBytes += int64(len(choice.Text))
		for _, call := range choice.Message.ToolCalls {
			generatedBytes += int64(len(call.Function.Name))
			generatedBytes += int64(len(call.Function.Arguments))
		}
	}
	if err := boundCompletionTokens(completion.Usage.CompletionTokens, generatedBytes); err != nil {
		pathTiming.mark("usage_reconcile", stage)
		finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, "usage_reconciliation_failed", err, false)
		writeOpenAIError(w, http.StatusBadGateway, "realtime worker returned unreconciled usage", "server_error", "usage_reconciliation_failed")
		return
	}

	chainDigest := sha256.Sum256(append(make([]byte, 40), bodyDigest[:]...))
	evidence := RealtimeExecutionEvidence{
		ID: executionID, UpstreamRequestID: completion.ID, HTTPStatus: response.StatusCode,
		StreamEventCount: 1, StreamRootSHA256: hex.EncodeToString(chainDigest[:]),
		OutputCommitment: hex.EncodeToString(bodyDigest[:]), PromptTokens: completion.Usage.PromptTokens,
		CompletionTokens: completion.Usage.CompletionTokens, TotalTokens: completion.Usage.TotalTokens,
		TimeToFirstEventMS: time.Since(started).Milliseconds(), DurationMS: time.Since(started).Milliseconds(),
	}
	pathTiming.mark("usage_reconcile", stage)
	stage = time.Now()
	if _, err := s.store.FinalizeRealtimeSuccess(r.Context(), contract.ID, evidence); err != nil {
		pathTiming.mark("settlement", stage)
		metrics.realtimeFinalizationErrors.Add(1)
		finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, "settlement_failed", err, false)
		writeOpenAIError(w, http.StatusBadGateway, "verified response could not be settled", "server_error", "settlement_failed")
		pathTiming.log(false, contract.ID.String())
		return
	}
	pathTiming.mark("settlement", stage)
	metrics.realtimeVerified.Add(1)
	// Deliver the settled response first. Cache population is best-effort and
	// must never gate the buyer: it does object storage + a DB insert, which
	// used to sit on the critical path and add avoidable wall time after
	// settlement had already succeeded.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	// Populate the exact-result cache so a later identical deterministic
	// request pays the reuse class instead of re-running the model. Use a
	// detached timeout so a client disconnect after Write cannot cancel the
	// cache write, and so a slow object store is bounded.
	cacheCtx, cacheCancel := context.WithTimeout(context.Background(), 10*time.Second)
	stage = time.Now()
	s.maybeStoreRealtimeExactResult(cacheCtx, auth.BuyerID, prepared, coalesceLeaderRef, contract.ID,
		body, completion.Usage.CompletionTokens)
	pathTiming.mark("exact_cache_store", stage)
	cacheCancel()
	pathTiming.log(false, contract.ID.String())
}

// recordRealtimeAdmissionEvent is observational. A telemetry database fault
// must never turn an otherwise valid buyer request into a failed contract, nor
// can it alter capacity selection, pricing, or settlement.
func (s *Server) recordRealtimeAdmissionEvent(
	ctx context.Context, buyerID uuid.UUID, runtimeProfileID, hwClass, decision string, contractID uuid.UUID,
) {
	if err := s.store.RecordRealtimeAdmissionEvent(ctx, buyerID, runtimeProfileID, hwClass, decision, contractID); err != nil {
		log.Printf("realtime liquidity telemetry: decision=%s profile=%s: %v", decision, runtimeProfileID, err)
	}
}

// tryRealtimeExactReuse serves a prior identical deterministic response from
// the exact-result cache. Returns reuseHit=false when the request is not
// cacheable or the cache misses — the caller then runs live inference.
func (s *Server) tryRealtimeExactReuse(
	ctx context.Context,
	buyerID uuid.UUID,
	requestID, idempotencyKey string,
	prepared preparedRealtimeRequest,
) (RealtimeContract, []byte, bool, error) {
	identity, err := realtimeIdentityFromPreparedBody(buyerID, prepared.Profile, prepared.Body)
	if err != nil {
		// Non-deterministic or incomplete identity: not eligible for reuse.
		return RealtimeContract{}, nil, false, nil
	}
	hit, ok, err := s.store.LookupExactResult(ctx, identity)
	if err != nil || !ok {
		return RealtimeContract{}, nil, false, err
	}
	body, err := s.store.LoadExactResultBytes(ctx, s.storage, hit.ResultRef)
	if err != nil {
		return RealtimeContract{}, nil, false, err
	}
	// Delivered tokens for a pure result hit are the cached completion size.
	// Physical is zero — PriceAccounting charges the reuse class only.
	delivered := hit.OutputTokens
	if delivered <= 0 {
		delivered = 1
	}
	currency, err := SettlementCurrency()
	if err != nil {
		return RealtimeContract{}, nil, false, err
	}
	money, err := SettleRealtimeReuseHitMoney(currency, delivered,
		prepared.Profile.BuyerInputUSDPerMillionTokens, prepared.Profile.BuyerOutputUSDPerMillionTokens)
	if err != nil || !money.Conserved() || !money.ConservedExact() || money.SupplierLiabilityMicros != 0 {
		return RealtimeContract{}, nil, false, fmt.Errorf("reuse money invariant broken: %+v", money)
	}
	sum := sha256.Sum256(body)
	contract, _, err := s.store.SettleRealtimeExactReuse(ctx, RealtimeContractAuthorization{
		RequestID: requestID, BuyerID: buyerID, Profile: prepared.Profile,
		InputCommitment: prepared.InputCommitment, RequestSHA256: prepared.RequestSHA256,
		MaximumPriceUSD:         microsToUSD(money.BuyerDebitMicros),
		EstimatedPriceUSD:       microsToUSD(money.BuyerDebitMicros),
		BuyerDeclaredCeilingUSD: prepared.MaxPriceCeiling, ReuseClass: ClassExactResultReuse,
		DeadlineAt: time.Now().Add(defaultRealtimeTimeout), IdempotencyKey: idempotencyKey,
	}, hit, money, hex.EncodeToString(sum[:]))
	if err != nil {
		return RealtimeContract{}, nil, false, err
	}
	return contract, body, true, nil
}

// maybeStoreRealtimeExactResult caches a verified live response under its
// request identity when sampling is deterministic, and publishes it to any
// callers that coalesced onto this execution.
//
// Failures are logged only: a cache-write miss must not fail a successful buyer
// response. A publish failure is different in kind — followers are waiting — so
// it releases them explicitly rather than letting them time out on the lease.
func (s *Server) maybeStoreRealtimeExactResult(
	ctx context.Context, buyerID uuid.UUID, prepared preparedRealtimeRequest,
	leaderRef string, leaderContractID uuid.UUID, body []byte, completionTokens int64,
) {
	identity, err := realtimeIdentityFromPreparedBody(buyerID, prepared.Profile, prepared.Body)
	if err != nil {
		return
	}
	ref, err := s.store.StoreExactResultBytes(ctx, s.storage, identity, body, completionTokens)
	if err != nil {
		log.Printf("exact reuse store failed: %v", err)
		if leaderRef != "" {
			// Followers cannot be served from a result that was never stored.
			// Failing them now costs them a live execution each; leaving them to
			// discover it via lease expiry costs them that AND the wait.
			if err := s.store.ResolveInflightFailure(ctx, identity, leaderRef,
				"leader could not store its result"); err != nil {
				log.Printf("inflight failure publish: %v", err)
			}
		}
		return
	}
	if leaderRef == "" {
		return
	}
	sum := sha256.Sum256(body)
	if err := s.store.ResolveInflightSuccess(
		ctx, identity, leaderRef, ref, hex.EncodeToString(sum[:]), completionTokens, leaderContractID,
	); err != nil {
		log.Printf("inflight result publish: %v", err)
	}
}

// tryRealtimeCoalescedDelivery collapses identical concurrent requests onto one
// execution.
//
// Called only after the exact-result cache has missed, which is the window this
// exists for: the cache serves requests that repeat over time, and this serves
// requests that arrive together, before there is anything to cache.
//
// Returns leaderRef when this caller must execute. Returns a contract and body
// when it rode someone else's execution instead.
func (s *Server) tryRealtimeCoalescedDelivery(
	ctx context.Context,
	buyerID uuid.UUID,
	requestID, idempotencyKey string,
	prepared preparedRealtimeRequest,
) (identity, leaderRef string, contract RealtimeContract, body []byte, followed bool, err error) {
	identity, identityErr := realtimeIdentityFromPreparedBody(buyerID, prepared.Profile, prepared.Body)
	if identityErr != nil {
		// Non-deterministic sampling: two runs need not agree, so collapsing
		// them would hand one buyer another's roll of the dice.
		return "", "", RealtimeContract{}, nil, false, nil
	}

	role, err := s.store.ClaimInflightExecution(ctx, identity, buyerID, requestID)
	if err != nil {
		// Coalescing is an optimization. A fault in it means execute live, never
		// fail the buyer.
		log.Printf("inflight claim failed, executing live: %v", err)
		return "", "", RealtimeContract{}, nil, false, nil
	}
	if role.Ineligible {
		return "", "", RealtimeContract{}, nil, false, nil
	}
	if role.Leader {
		return identity, requestID, RealtimeContract{}, nil, false, nil
	}

	result, ok, err := s.store.AwaitInflightResult(ctx, identity, buyerID)
	if err != nil || !ok {
		// The leader failed, the lease expired, or this caller's own context
		// ended. In every case: execute live rather than fail.
		return "", "", RealtimeContract{}, nil, false, nil
	}
	body, err = s.store.LoadExactResultBytes(ctx, s.storage, result.ResultRef)
	if err != nil {
		return "", "", RealtimeContract{}, nil, false, nil
	}

	delivered := result.Tokens
	if delivered <= 0 {
		delivered = 1
	}
	// The follower performed no physical work, so the supplier is owed nothing
	// for it — the leader's own settlement is the one payable for the one
	// execution. Same money shape as a cache hit; the classes differ because the
	// stories differ, and the invariant checked here is the one that matters:
	// this must not mint a second supplier liability.
	currency, err := SettlementCurrency()
	if err != nil {
		return "", "", RealtimeContract{}, nil, false, err
	}
	money, err := SettleRealtimeReuseHitMoney(currency, delivered,
		prepared.Profile.BuyerInputUSDPerMillionTokens, prepared.Profile.BuyerOutputUSDPerMillionTokens)
	if err != nil || !money.Conserved() || !money.ConservedExact() || money.SupplierLiabilityMicros != 0 {
		return "", "", RealtimeContract{}, nil, false,
			fmt.Errorf("coalesced money invariant broken: %+v", money)
	}
	hit := ExactCacheHit{ResultRef: result.ResultRef, OutputTokens: delivered}
	contract, _, err = s.store.SettleRealtimeExactReuse(ctx, RealtimeContractAuthorization{
		RequestID: requestID, BuyerID: buyerID, Profile: prepared.Profile,
		InputCommitment: prepared.InputCommitment, RequestSHA256: prepared.RequestSHA256,
		MaximumPriceUSD:         microsToUSD(money.BuyerDebitMicros),
		EstimatedPriceUSD:       microsToUSD(money.BuyerDebitMicros),
		BuyerDeclaredCeilingUSD: prepared.MaxPriceCeiling, ReuseClass: ClassCoalescedDelivery,
		CoalescedLeaderContractID: result.LeaderContractID,
		DeadlineAt:                time.Now().Add(defaultRealtimeTimeout), IdempotencyKey: idempotencyKey,
	}, hit, money, result.ResultSHA256)
	if err != nil {
		return "", "", RealtimeContract{}, nil, false, err
	}
	return "", "", contract, body, true, nil
}

func (s *Server) handleRealtimeReceipt(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid contract id", "invalid_request_error", "invalid_contract_id")
		return
	}
	receipt, err := s.store.RealtimeReceipt(r.Context(), auth.BuyerID, id)
	if errors.Is(err, errNotFound) {
		writeOpenAIError(w, http.StatusNotFound, "realtime receipt not found", "invalid_request_error", "not_found")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "realtime receipt is unavailable", "server_error", "receipt_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}
