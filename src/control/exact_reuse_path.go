package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/google/uuid"
)

// Live-path wiring for exact result reuse.
//
// RequestIdentity is derived server-side only (never accepted from the wire).
// api.go must not construct RequestIdentity literals — workload_simulation_test
// greps for that. Helpers here keep derivation out of the HTTP handlers.

// Cacheability is a closed set. prepareRealtimeRequest forwards the buyer's
// JSON with only a few defaults filled in — there is no field strip — so any
// generation-affecting key that is not already folded into RequestIdentity
// would silently collide if we hashed only the known fields. Instead:
//
//   - keys covered by RequestIdentity are hashed as before;
//   - keys that are provably non-semantic for the completion (stream flags,
//     OpenAI user tagging) are ignored;
//   - every other key, known or unknown, refuses cacheability.
//
// A field added to the OpenAI surface tomorrow therefore falls out of the
// cache automatically rather than colliding under an under-specified key.
// That property is the point of the closed set.
//
// Non-deterministic sampling yields errNonDeterministic from Compute;
// out-of-set fields yield errNotExactCacheable. Callers treat both as "not
// cacheable", never as a hard failure.

// exactReuseIdentityPayloadKeys are folded into RequestIdentity and may appear
// on a cacheable prepared body.
var exactReuseIdentityPayloadKeys = map[string]struct{}{
	"model":                 {},
	"messages":              {},
	"tools":                 {},
	"response_format":       {},
	"temperature":           {},
	"top_p":                 {},
	"seed":                  {},
	"max_tokens":            {},
	"max_completion_tokens": {},
}

// exactReuseNonSemanticPayloadKeys do not change the completion bytes and are
// stripped from identity consideration. Anything else is refused.
var exactReuseNonSemanticPayloadKeys = map[string]struct{}{
	"stream":         {},
	"stream_options": {},
	// OpenAI "user" is an abuse/tracking tag; engines do not sample on it.
	"user": {},
}

// payloadExactReuseEligible reports whether every key on the prepared body is
// either identity-covered or explicitly non-semantic. Unknown and known
// generation-affecting keys (stop, tool_choice, frequency_penalty, …) fail.
func payloadExactReuseEligible(payload map[string]any) error {
	for k := range payload {
		if _, ok := exactReuseIdentityPayloadKeys[k]; ok {
			continue
		}
		if _, ok := exactReuseNonSemanticPayloadKeys[k]; ok {
			continue
		}
		return fmt.Errorf("%w: %q", errNotExactCacheable, k)
	}
	return nil
}

func rawPayloadExactReuseEligible(payload map[string]json.RawMessage) error {
	for k := range payload {
		if _, ok := exactReuseIdentityPayloadKeys[k]; ok {
			continue
		}
		if _, ok := exactReuseNonSemanticPayloadKeys[k]; ok {
			continue
		}
		return fmt.Errorf("%w: %q", errNotExactCacheable, k)
	}
	return nil
}

// realtimeIdentityFromPayload builds a cache key from the canonical upstream
// payload prepareRealtimeRequest already produced.
func realtimeIdentityFromPayload(
	buyerID uuid.UUID, profile VLLMRuntimeProfile, payload map[string]any,
) (string, error) {
	if err := payloadExactReuseEligible(payload); err != nil {
		return "", err
	}
	model, _ := payload["model"].(string)
	inputBlob, err := canonicalJSON(payload["messages"])
	if err != nil {
		return "", err
	}
	toolsBlob := ""
	if raw, ok := payload["tools"]; ok {
		b, err := canonicalJSON(raw)
		if err != nil {
			return "", err
		}
		toolsBlob = string(b)
	}
	schemaBlob := ""
	if raw, ok := payload["response_format"]; ok {
		b, err := canonicalJSON(raw)
		if err != nil {
			return "", err
		}
		schemaBlob = string(b)
	}
	temp := jsonFloat(payload["temperature"], profile.GenerationPolicy.Temperature)
	topP := jsonFloat(payload["top_p"], profile.GenerationPolicy.TopP)
	seed := jsonInt64(payload["seed"], 0)
	maxTokens := int(profile.GenerationPolicy.MaximumOutputTokens)
	if n, ok := jsonInt(payload["max_completion_tokens"]); ok {
		maxTokens = int(n)
	} else if n, ok := jsonInt(payload["max_tokens"]); ok {
		maxTokens = int(n)
	}
	id := RequestIdentity{
		// The tenant is part of the key, not a filter applied after the lookup.
		// A filter still requires the row to be found first, which is the
		// existence side channel this closes.
		TenantScope:   buyerID.String(),
		ModelID:       model,
		ModelRevision: profile.ModelRevision,
		ProfileSHA256: profile.ProfileSHA256,
		Input:         string(inputBlob),
		Tools:         toolsBlob,
		Schema:        schemaBlob,
		Temperature:   temp,
		TopP:          topP,
		Seed:          seed,
		MaxTokens:     maxTokens,
		Policy:        profile.GenerationPolicy.Version,
	}
	return id.Compute()
}

// realtimeIdentityFromCanonicalBody derives the same identity directly from
// the top-level members of the canonical prepared body. RawMessage keeps the
// nested prompt/tool/schema bytes intact instead of recursively decoding and
// marshaling them a second time on an identity-cache miss.
func realtimeIdentityFromCanonicalBody(
	buyerID uuid.UUID, profile VLLMRuntimeProfile, body []byte,
) (string, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if err := rawPayloadExactReuseEligible(payload); err != nil {
		return "", err
	}

	model := ""
	if raw, ok := payload["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	inputBlob := []byte("null")
	if raw, ok := payload["messages"]; ok {
		inputBlob = raw
	}
	toolsBlob := ""
	if raw, ok := payload["tools"]; ok {
		toolsBlob = string(raw)
	}
	schemaBlob := ""
	if raw, ok := payload["response_format"]; ok {
		schemaBlob = string(raw)
	}
	temp := profile.GenerationPolicy.Temperature
	if raw, ok := payload["temperature"]; ok {
		temp = jsonFloat(realtimeRawScalar(raw), temp)
	}
	topP := profile.GenerationPolicy.TopP
	if raw, ok := payload["top_p"]; ok {
		topP = jsonFloat(realtimeRawScalar(raw), topP)
	}
	seed := int64(0)
	if raw, ok := payload["seed"]; ok {
		seed = jsonInt64(realtimeRawScalar(raw), 0)
	}
	maxTokens := int(profile.GenerationPolicy.MaximumOutputTokens)
	if raw, exists := payload["max_completion_tokens"]; exists {
		if n, ok := jsonInt(realtimeRawScalar(raw)); ok {
			maxTokens = int(n)
		} else if legacy, exists := payload["max_tokens"]; exists {
			if n, ok := jsonInt(realtimeRawScalar(legacy)); ok {
				maxTokens = int(n)
			}
		}
	} else if raw, exists := payload["max_tokens"]; exists {
		if n, ok := jsonInt(realtimeRawScalar(raw)); ok {
			maxTokens = int(n)
		}
	}
	id := RequestIdentity{
		TenantScope:   buyerID.String(),
		ModelID:       model,
		ModelRevision: profile.ModelRevision,
		ProfileSHA256: profile.ProfileSHA256,
		Input:         string(inputBlob),
		Tools:         toolsBlob,
		Schema:        schemaBlob,
		Temperature:   temp,
		TopP:          topP,
		Seed:          seed,
		MaxTokens:     maxTokens,
		Policy:        profile.GenerationPolicy.Version,
	}
	return id.Compute()
}

func realtimeRawScalar(raw json.RawMessage) any {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil
	}
	return value
}

func jsonFloat(v any, fallback float64) float64 {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err == nil {
			return f
		}
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err == nil {
			return f
		}
	}
	return fallback
}

func jsonInt64(v any, fallback int64) int64 {
	if n, ok := jsonInt(v); ok {
		return n
	}
	return fallback
}

// fullPricePer1KFromRealtime converts per-million token rates into the per-1k
// rate PriceAccounting expects. Input and output rates are blended at equal
// weight only when both are zero tokens of one kind; for pure reuse we charge
// against the higher of the two catalogue rates so reuse never undercuts the
// cheaper class of physical work in a way that invents a discount larger than
// reuseDiscountShare.
func fullPricePer1KFromRealtime(inputPerMillion, outputPerMillion float64) float64 {
	// PriceAccounting takes a single fullPricePer1K. Realtime has separate
	// input/output rates. Use the max so a reuse hit is never more expensive
	// than the dearer physical class, and still discounted by reuseDiscountShare.
	perMillion := inputPerMillion
	if outputPerMillion > perMillion {
		perMillion = outputPerMillion
	}
	return perMillion / 1000.0
}

// StoreExactResultBytes writes a content-addressed artifact and registers it
// in the exact-result cache. Tenant-scoped jobs/ paths are refused by
// StoreExactResult. result_bytes is the body length so the retention sweep can
// bound total cache size without listing the object store.
func (s *Store) StoreExactResultBytes(ctx context.Context, storage *Storage, identity string, body []byte, outputTokens int64) (string, error) {
	if storage == nil {
		return "", fmt.Errorf("object storage is required to cache an exact result")
	}
	sum := sha256.Sum256(body)
	ref := contentAddressedResultRef(hex.EncodeToString(sum[:]))
	if err := storage.PutObject(ctx, ref, body, "application/json"); err != nil {
		return "", fmt.Errorf("writing content-addressed result: %w", err)
	}
	if err := s.recordExactResult(ctx, identity, ref, outputTokens, int64(len(body))); err != nil {
		return "", err
	}
	return ref, nil
}

// LoadExactResultBytes fetches a previously cached result body.
func (s *Store) LoadExactResultBytes(ctx context.Context, storage *Storage, ref string) ([]byte, error) {
	if storage == nil {
		return nil, fmt.Errorf("object storage is required to load an exact result")
	}
	if tenantScopedRefPattern.MatchString(ref) {
		return nil, fmt.Errorf("refusing to load tenant-scoped cache reference %q", ref)
	}
	return storage.GetObject(ctx, ref)
}

// tryBatchExactReuse looks up a prior identical batch job. On hit it returns
// the settlement money and result ref; the caller must not schedule suppliers.
func (s *Store) tryBatchExactReuse(ctx context.Context, identity string, fullPricePer1K float64) (hit ExactCacheHit, money ReuseHitSettlement, ok bool, err error) {
	if identity == "" {
		return ExactCacheHit{}, ReuseHitSettlement{}, false, nil
	}
	hit, ok, err = s.LookupExactResult(ctx, identity)
	if err != nil || !ok {
		return hit, ReuseHitSettlement{}, ok, err
	}
	money = SettleReuseHitMoney(hit.OutputTokens, fullPricePer1K)
	if !money.Conserved() {
		return hit, money, false, fmt.Errorf("reuse settlement is not conserved: buyer=%d supplier=%d platform=%d",
			money.BuyerDebitMicros, money.SupplierLiabilityMicros, money.PlatformMicros)
	}
	return hit, money, true, nil
}

// batchFullPricePer1K is the catalogue per-1k rate used for class-aware pricing.
func batchFullPricePer1K(pricePer1K float64) float64 {
	if pricePer1K <= 0 {
		return 0
	}
	return pricePer1K
}
