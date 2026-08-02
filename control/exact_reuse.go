package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Exact deterministic result reuse.
//
// Prefix reuse removes prefill. This removes everything: when the complete
// effective request identity matches, a prior result is not an approximation of
// the answer, it IS the answer, and no model needs to run.
//
// Eligibility is narrow on purpose. Anything that could make two runs differ --
// a temperature above zero, a different seed, a different model revision, a
// different tool schema -- changes the identity and produces a miss. A
// near-miss must never silently stand in for fresh inference; that is a
// different product (SEMANTIC_REUSE) requiring buyer opt-in, and it is not
// this.

var requestIdentityPattern = regexp.MustCompile(`^req_[0-9a-f]{64}$`)

// RequestIdentity is every input that can change the output, plus the tenant it
// belongs to.
//
// TenantScope is not an input to the model and it is still part of the identity,
// because reuse across tenants is a cache-existence side channel: buyer B
// discovers that buyer A ran a particular request by observing that the same
// request comes back at the reuse price. The content is identical either way —
// the leak is the timing and the bill, not the bytes — which is exactly the kind
// of leak that is invisible in a correctness test.
//
// Scoping it costs a cache miss the first time each tenant asks. That is the
// right trade, and it is why the tranche says not to start with global
// cross-tenant sharing.
//
// ProfileSHA256 binds the durable key to the same runtime profile digest the
// in-memory identity cache already uses. A profile change that keeps model
// revision fixed (engine, container, dtype, max length) must miss rather than
// serve an old completion under a receipt naming the new profile.
//
// The domain separator is versioned (merc-request-identity-vN). Bumping it
// invalidates every prior row on purpose: an entry whose key was under-specified
// must never be served under the new definition.
type RequestIdentity struct {
	TenantScope   string
	ModelID       string
	ModelRevision string
	// ProfileSHA256 is the runtime profile content digest. Empty is allowed for
	// callers that do not yet bind a profile (batch path, unit fixtures); it is
	// still hashed so "no profile" and "a profile" cannot collide.
	ProfileSHA256 string
	Adapter       string
	Input         string
	Tools         string
	Schema        string
	// Sampling must be deterministic for the request to be cacheable at all.
	Temperature float64
	TopP        float64
	Seed        int64
	MaxTokens   int
	Policy      string
}

// requestIdentityDomain is the domain separator hashed into every durable key.
// Bump when the field set or its meaning changes so old rows miss cleanly.
const requestIdentityDomain = "merc-request-identity-v3"

// errNonDeterministic marks a request that cannot be cached because two runs
// need not agree.
var errNonDeterministic = errors.New("request sampling is not deterministic; exact reuse is not eligible")

// errNotExactCacheable marks a request that is outside the closed set of fields
// exact reuse knows how to key. Callers treat it like errNonDeterministic: the
// request runs live and is not stored.
var errNotExactCacheable = errors.New("request carries fields outside the exact-reuse identity; not cacheable")

// Deterministic reports whether this request would produce the same output
// every time. Temperature and top-p are the gate: anything that samples cannot
// be replayed from cache without changing what the buyer would have received.
func (r RequestIdentity) Deterministic() bool {
	return r.Temperature == 0 && (r.TopP == 0 || r.TopP == 1)
}

// Compute derives the cache key.
//
// Field names are included in the hashed payload so that moving a value from
// one field to another cannot collide, and the encoding is canonical JSON with
// sorted keys so that two identical requests always hash identically.
func (r RequestIdentity) Compute() (string, error) {
	if !r.Deterministic() {
		return "", errNonDeterministic
	}
	if strings.TrimSpace(r.ModelID) == "" || strings.TrimSpace(r.Input) == "" {
		return "", fmt.Errorf("request identity requires at least a model and an input")
	}
	// An empty tenant scope is refused rather than hashed as "". A caller that
	// forgets to pass the buyer would otherwise silently produce one shared key
	// for everyone — the exact leak TenantScope exists to close, reintroduced by
	// omission and passing every test that only checks the bytes.
	if strings.TrimSpace(r.TenantScope) == "" {
		return "", fmt.Errorf("request identity requires a tenant scope")
	}
	fields := map[string]any{
		"tenant": r.TenantScope,
		"model":  r.ModelID, "revision": r.ModelRevision, "adapter": r.Adapter,
		"profile_sha256": r.ProfileSHA256,
		"input":          r.Input, "tools": r.Tools, "schema": r.Schema,
		"temperature": r.Temperature, "top_p": r.TopP, "seed": r.Seed,
		"max_tokens": r.MaxTokens, "policy": r.Policy,
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	// Domain separator is versioned: every prior version's rows miss rather than
	// resolve under a changed field set. A miss costs work; a stale hit under a
	// changed identity definition would be wrong.
	h.Write([]byte(requestIdentityDomain + "\x00"))
	for _, k := range keys {
		blob, err := json.Marshal(fields[k])
		if err != nil {
			return "", err
		}
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(blob)
		h.Write([]byte{0})
	}
	return "req_" + hex.EncodeToString(h.Sum(nil)), nil
}

// computeRequestIdentityWithDomain is test-only: hash the same field set under
// an older domain separator so versioning tests can prove old rows never hit.
func (r RequestIdentity) computeRequestIdentityWithDomain(domain string) (string, error) {
	if !r.Deterministic() {
		return "", errNonDeterministic
	}
	if strings.TrimSpace(r.ModelID) == "" || strings.TrimSpace(r.Input) == "" {
		return "", fmt.Errorf("request identity requires at least a model and an input")
	}
	if strings.TrimSpace(r.TenantScope) == "" {
		return "", fmt.Errorf("request identity requires a tenant scope")
	}
	fields := map[string]any{
		"tenant": r.TenantScope,
		"model":  r.ModelID, "revision": r.ModelRevision, "adapter": r.Adapter,
		"profile_sha256": r.ProfileSHA256,
		"input":          r.Input, "tools": r.Tools, "schema": r.Schema,
		"temperature": r.Temperature, "top_p": r.TopP, "seed": r.Seed,
		"max_tokens": r.MaxTokens, "policy": r.Policy,
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(domain + "\x00"))
	for _, k := range keys {
		blob, err := json.Marshal(fields[k])
		if err != nil {
			return "", err
		}
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(blob)
		h.Write([]byte{0})
	}
	return "req_" + hex.EncodeToString(h.Sum(nil)), nil
}

// ValidRequestIdentity checks shape before it reaches SQL.
func ValidRequestIdentity(id string) bool {
	return requestIdentityPattern.MatchString(id)
}

// ExactCacheHit is a reusable prior result.
type ExactCacheHit struct {
	ResultRef    string
	OutputTokens int64
	Hits         int64
}

// LookupExactResult returns a prior result for an identical request.
func (s *Store) LookupExactResult(ctx context.Context, identity string) (ExactCacheHit, bool, error) {
	if !ValidRequestIdentity(identity) {
		return ExactCacheHit{}, false, nil
	}
	var hit ExactCacheHit
	err := s.pool.QueryRow(ctx, `
		UPDATE exact_result_cache
		   SET hits = hits + 1, last_hit_at = now()
		 WHERE request_identity = $1
		 RETURNING result_ref, output_tokens, hits`, identity).
		Scan(&hit.ResultRef, &hit.OutputTokens, &hit.Hits)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExactCacheHit{}, false, nil
	}
	if err != nil {
		return ExactCacheHit{}, false, err
	}
	return hit, true, nil
}

// tenantScopedRefPattern matches artifact paths that belong to one buyer's job.
//
// The scheduler writes results to jobs/<job_id>/tasks/<task_id>/... which is
// scoped to the buyer that submitted the job. The exact-result cache is keyed
// on request identity ONLY -- deliberately, because deterministic inference on
// identical input yields identical output regardless of who asked. That makes
// the key tenant-neutral while the path is not.
//
// Storing a tenant-scoped path under a tenant-neutral key means a later buyer
// gets a reference into someone else's job namespace: either the read is
// refused and the cache silently never works, or it succeeds and crosses a
// tenant boundary. Neither is acceptable, so the cache accepts only
// content-addressed references.
var tenantScopedRefPattern = regexp.MustCompile(`^jobs/`)

// RecordExactResult stores a completed deterministic result for reuse.
// result_bytes is recorded as 0 when the caller does not know the object size;
// StoreExactResultBytes always passes the real length so size-based eviction
// can bound the table without listing the object store.
func (s *Store) RecordExactResult(ctx context.Context, identity, resultRef string, outputTokens int64) error {
	return s.recordExactResult(ctx, identity, resultRef, outputTokens, 0)
}

func (s *Store) recordExactResult(ctx context.Context, identity, resultRef string, outputTokens, resultBytes int64) error {
	if !ValidRequestIdentity(identity) {
		return fmt.Errorf("refusing to cache under a malformed request identity")
	}
	if strings.TrimSpace(resultRef) == "" {
		return fmt.Errorf("refusing to cache an empty result reference")
	}
	if tenantScopedRefPattern.MatchString(resultRef) {
		return fmt.Errorf(
			"refusing to cache tenant-scoped reference %q: the exact-result cache is keyed on "+
				"request identity alone and is shared across buyers, so it may only hold "+
				"content-addressed artifacts", resultRef)
	}
	if resultBytes < 0 {
		resultBytes = 0
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO exact_result_cache (request_identity, result_ref, output_tokens, result_bytes)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (request_identity) DO NOTHING`, identity, resultRef, outputTokens, resultBytes)
	return err
}

// StoreExactResult is the production name for populating the exact-result cache.
// It is RecordExactResult; the alias exists so call sites read as the store side
// of the lookup/store pair and so the missing-writer defect cannot recur by
// grepping only for one of the two names.
func (s *Store) StoreExactResult(ctx context.Context, identity, resultRef string, outputTokens int64) error {
	return s.RecordExactResult(ctx, identity, resultRef, outputTokens)
}

// contentAddressedResultRef builds a tenant-neutral object key from a content
// digest. The exact-result cache refuses anything under jobs/.
func contentAddressedResultRef(sha256Hex string) string {
	return "cas/sha256/" + strings.TrimSpace(sha256Hex)
}

// ExactReuseAccounting is the token split for a pure cache hit: every delivered
// token is ClassExactResultReuse, none are physical.
func ExactReuseAccounting(deliveredTokens int64) TokenAccounting {
	if deliveredTokens < 0 {
		deliveredTokens = 0
	}
	return TokenAccounting{ClassExactResultReuse: deliveredTokens}
}

// PriceExactReuseHit bills a cache hit at the reuse class. Physical work is not
// charged; the supplier is not credited. Uses PriceAccounting so the full-rate
// ceiling clamp stays in force.
func PriceExactReuseHit(deliveredTokens int64, fullPricePer1K float64) float64 {
	return PriceAccounting(ExactReuseAccounting(deliveredTokens), fullPricePer1K)
}

// ReuseHitSettlement is the money split for a cache hit: buyer pays the reuse
// price, supplier liability is zero (nobody ran the model), platform keeps the
// rest. Amounts are micro-USD so conservation is exact.
type ReuseHitSettlement struct {
	BuyerDebitMicros        int64
	SupplierLiabilityMicros int64 // always 0 on a pure reuse hit
	PlatformMicros          int64
	Currency                string
	BuyerDebitNanos         int64
	PlatformNanos           int64
	MinimumChargeApplied    bool
	PhysicalTokens          int64
	DeliveredTokens         int64
	Accounting              TokenAccounting
}

// SettleRealtimeReuseHitMoney is the realtime cache/coalescing authority. It
// converts the embedded per-million profile rates once, selects the output/full
// delivery ceiling exactly, applies the reuse share in nanos, and projects the
// complete charge to the ledger once.
func SettleRealtimeReuseHitMoney(
	c Currency, deliveredTokens int64, inputPerMillion, outputPerMillion float64,
) (ReuseHitSettlement, error) {
	input, err := nanoRatePerMillionFromFloat(inputPerMillion)
	if err != nil {
		return ReuseHitSettlement{}, err
	}
	output, err := nanoRatePerMillionFromFloat(outputPerMillion)
	if err != nil {
		return ReuseHitSettlement{}, err
	}
	full := input
	if output > full {
		full = output
	}
	charge, minimumApplied, err := RealtimeReuseBuyerChargeNanos(c, deliveredTokens, full)
	if err != nil {
		return ReuseHitSettlement{}, err
	}
	micros, err := LedgerMicrosFromNanos(charge)
	if err != nil {
		return ReuseHitSettlement{}, err
	}
	acct := ExactReuseAccounting(deliveredTokens)
	return ReuseHitSettlement{
		BuyerDebitMicros: micros, PlatformMicros: micros, Currency: c.Code(),
		BuyerDebitNanos: charge.Nanos, PlatformNanos: charge.Nanos,
		MinimumChargeApplied: minimumApplied,
		PhysicalTokens:       acct.PhysicalTokens(), DeliveredTokens: acct.DeliveredTokens(), Accounting: acct,
	}, nil
}

// SettleReuseHitMoney prices a cache hit and checks micro-USD conservation.
// fullPricePer1K is the catalogue rate the buyer would have paid without reuse.
func SettleReuseHitMoney(deliveredTokens int64, fullPricePer1K float64) ReuseHitSettlement {
	acct := ExactReuseAccounting(deliveredTokens)
	charge := PriceExactReuseHit(deliveredTokens, fullPricePer1K)
	buyer := usdToMicros(charge)
	// The reuse rate is a discount off the full rate, so on a small result it
	// underflows micro-USD to exactly 0. The settle path then correctly refuses
	// ("must charge the buyer" -- reuse is cheaper, never free) and the caller
	// falls back to executing live at FULL price. The net effect was that reuse
	// was wired, hit the cache, and still never saved anyone anything on the
	// small repeated requests it exists to serve. Observed live: a 2-token embed
	// job hit the cache and was billed the full $0.000125 anyway.
	//
	// Floor a hit that delivered tokens at one micro-USD, the same minimum
	// billable treatment the supplier side and LoRA already use. Platform moves
	// with it so buyer = supplier + platform stays exact.
	if buyer <= 0 && deliveredTokens > 0 {
		buyer = 1
	}
	// Pure reuse: no supplier performed work. Platform retains the whole charge
	// (storage, lookup, delivery, verification still cost merc).
	return ReuseHitSettlement{
		BuyerDebitMicros:        buyer,
		SupplierLiabilityMicros: 0,
		PlatformMicros:          buyer,
		PhysicalTokens:          acct.PhysicalTokens(),
		DeliveredTokens:         acct.DeliveredTokens(),
		Accounting:              acct,
	}
}

// Conserved reports whether buyer = supplier + platform exactly.
func (s ReuseHitSettlement) Conserved() bool {
	return s.BuyerDebitMicros == s.SupplierLiabilityMicros+s.PlatformMicros
}

func (s ReuseHitSettlement) ConservedExact() bool {
	return s.BuyerDebitNanos == s.PlatformNanos && s.SupplierLiabilityMicros == 0
}

// In-flight coalescing lives in inflight_coalescing.go.
//
// The two functions that used to sit here — ClaimInflightLeader and
// ReleaseInflight — are gone rather than kept beside the governed path. They had
// no caller and could not have had one: a follower that waits must know what it
// is waiting for and when to give up, and there was no state, no lease, no
// result and no expiry to tell it. Keeping them alongside inflight_executions
// would leave two independently writable coalescing authorities, which is the
// failure the runtime registry work spent this whole branch removing elsewhere.
