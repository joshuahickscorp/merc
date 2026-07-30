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

// realtimeIdentityFromPayload builds a cache key from the canonical upstream
// payload prepareRealtimeRequest already produced. Non-deterministic sampling
// yields errNonDeterministic from Compute — callers treat that as "not
// cacheable", never as a hard failure.
func realtimeIdentityFromPayload(
	buyerID uuid.UUID, profile VLLMRuntimeProfile, payload map[string]any,
) (string, error) {
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

// decodeRealtimePayload re-parses the prepared upstream body for identity
// derivation. Body is already canonical JSON from prepareRealtimeRequest.
// Use bytes.NewReader so we do not allocate a second full copy of the body as
// a string just to re-parse it (the previous strings.NewReader(string(body))
// path did exactly that on every cache lookup and every cache store).
func decodeRealtimePayload(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		return nil, err
	}
	return payload, nil
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
// StoreExactResult.
func (s *Store) StoreExactResultBytes(ctx context.Context, storage *Storage, identity string, body []byte, outputTokens int64) (string, error) {
	if storage == nil {
		return "", fmt.Errorf("object storage is required to cache an exact result")
	}
	sum := sha256.Sum256(body)
	ref := contentAddressedResultRef(hex.EncodeToString(sum[:]))
	if err := storage.PutObject(ctx, ref, body, "application/json"); err != nil {
		return "", fmt.Errorf("writing content-addressed result: %w", err)
	}
	if err := s.StoreExactResult(ctx, identity, ref, outputTokens); err != nil {
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
