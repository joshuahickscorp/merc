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
	if reg.SupplierInputUSDPerMillionTokens < 0 || reg.SupplierOutputUSDPerMillionTokens < 0 ||
		reg.SupplierInputUSDPerMillionTokens > profile.BuyerInputUSDPerMillionTokens ||
		reg.SupplierOutputUSDPerMillionTokens > profile.BuyerOutputUSDPerMillionTokens {
		return VLLMRuntimeProfile{}, errors.New("supplier token rates must be non-negative and no greater than the buyer rates")
	}
	return profile, nil
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
	Body              []byte
	Profile           VLLMRuntimeProfile
	Stream            bool
	InputCommitment   string
	RequestSHA256     string
	MaximumPriceUSD   float64
	EstimatedPriceUSD float64
	MaxPriceCeiling   float64
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
			return preparedRealtimeRequest{}, errors.New("X-CX-Max-USD must be a positive number")
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
	maximumPrice := tokenCharge(maxInputTokens, maxOutput,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	estimatedPrice := tokenCharge(estimatedInputTokens, (maxOutput+1)/2,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
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
		MaxPriceCeiling: requestCeiling,
	}, nil
}

type streamEvidenceTracker struct {
	previous         [32]byte
	output           hash.Hash
	events           int64
	usageSeen        bool
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
	upstreamID       string
	startedAt        time.Time
	firstEventAt     time.Time
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
			ID    string `json:"id"`
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

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRealtimeRequestBytes+1))
	if err != nil || len(raw) > maxRealtimeRequestBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request body exceeds the realtime limit", "invalid_request_error", "request_too_large")
		return
	}
	prepared, err := prepareRealtimeRequest(raw, r.Header.Get("X-CX-Max-USD"))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_request")
		return
	}
	paused, err := s.store.OperationalControlPaused(r.Context(), controlIntake)
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
	contract, replay, err := s.store.AuthorizeRealtimeContract(r.Context(), RealtimeContractAuthorization{
		RequestID: requestID, BuyerID: auth.BuyerID, Profile: prepared.Profile,
		InputCommitment: prepared.InputCommitment, RequestSHA256: prepared.RequestSHA256,
		MaximumPriceUSD: prepared.MaximumPriceUSD, EstimatedPriceUSD: prepared.EstimatedPriceUSD,
		DeadlineAt: time.Now().Add(defaultRealtimeTimeout), IdempotencyKey: idempotencyKey,
	})
	if errors.Is(err, errRealtimeIdempotencyConflict) {
		writeOpenAIError(w, http.StatusConflict, err.Error(), "invalid_request_error", "idempotency_conflict")
		return
	}
	if errors.Is(err, errRealtimeNoSupply) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no compatible realtime capacity is currently available", "server_error", "no_capacity")
		return
	}
	if errors.Is(err, errRealtimeInsufficientFunds) {
		writeOpenAIError(w, http.StatusPaymentRequired, err.Error(), "insufficient_quota", "insufficient_quota")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "realtime contract authorization failed", "server_error", "authorization_failed")
		return
	}
	receiptPath := "/v1/realtime/requests/" + contract.ID.String() + "/receipt"
	w.Header().Set("X-CX-Contract-ID", contract.ID.String())
	w.Header().Set("X-CX-Receipt", receiptPath)
	w.Header().Set("X-CX-Max-USD", fmt.Sprintf("%.6f", contract.MaximumPriceUSD))
	if replay {
		writeOpenAIError(w, http.StatusConflict, "idempotent request already has a contract; inspect X-CX-Receipt", "invalid_request_error", "idempotent_replay")
		return
	}
	metrics.realtimeAuthorized.Add(1)

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
	response, err := client.Do(upstreamRequest)
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
		if err := proxySSE(w, response.Body, tracker); err != nil {
			cancelled := errors.Is(requestContext.Err(), context.Canceled)
			code := "stream_interrupted"
			if cancelled {
				code = "client_cancelled"
			}
			finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, code, err, cancelled)
			return
		}
		evidence, err := tracker.evidence(executionID, response.StatusCode, time.Since(started))
		if err != nil {
			finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, "usage_reconciliation_failed", err, false)
			return
		}
		settlementContext, settlementCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = s.store.FinalizeRealtimeSuccess(settlementContext, contract.ID, evidence)
		settlementCancel()
		if err != nil {
			metrics.realtimeFinalizationErrors.Add(1)
			finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, "settlement_failed", err, false)
		} else {
			metrics.realtimeVerified.Add(1)
		}
		return
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxRealtimeResponseBytes+1))
	if err != nil || len(body) > maxRealtimeResponseBytes {
		finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, "upstream_response_invalid", errors.New("upstream response exceeded the bounded JSON response size"), false)
		writeOpenAIError(w, http.StatusBadGateway, "realtime worker returned an invalid response", "server_error", "upstream_response_invalid")
		return
	}
	var completion struct {
		ID    string `json:"id"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &completion); err != nil || completion.Usage.TotalTokens != completion.Usage.PromptTokens+completion.Usage.CompletionTokens {
		finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, "usage_reconciliation_failed", errors.New("upstream JSON response did not include valid usage"), false)
		writeOpenAIError(w, http.StatusBadGateway, "realtime worker returned unreconciled usage", "server_error", "usage_reconciliation_failed")
		return
	}
	bodyDigest := sha256.Sum256(body)
	chainDigest := sha256.Sum256(append(make([]byte, 40), bodyDigest[:]...))
	evidence := RealtimeExecutionEvidence{
		ID: executionID, UpstreamRequestID: completion.ID, HTTPStatus: response.StatusCode,
		StreamEventCount: 1, StreamRootSHA256: hex.EncodeToString(chainDigest[:]),
		OutputCommitment: hex.EncodeToString(bodyDigest[:]), PromptTokens: completion.Usage.PromptTokens,
		CompletionTokens: completion.Usage.CompletionTokens, TotalTokens: completion.Usage.TotalTokens,
		TimeToFirstEventMS: time.Since(started).Milliseconds(), DurationMS: time.Since(started).Milliseconds(),
	}
	if _, err := s.store.FinalizeRealtimeSuccess(r.Context(), contract.ID, evidence); err != nil {
		metrics.realtimeFinalizationErrors.Add(1)
		finalizeRealtimeFailure(s.store, contract.ID, executionID, response.StatusCode, started, "settlement_failed", err, false)
		writeOpenAIError(w, http.StatusBadGateway, "verified response could not be settled", "server_error", "settlement_failed")
		return
	}
	metrics.realtimeVerified.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
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
