package main

import (
	"context"
	"hash/fnv"
	"math"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// livenessIndexAuthoritative reports whether the in-process offer-grain live
// index is the authority for realtime money-selection liveness (the G082 flip),
// instead of the durable realtime_worker_offers.last_seen_at SQL predicate.
// Default OFF: production is byte-identical to the SQL predicate. Flipping it ON
// is a deliberate, operationally-gated step (fail-closed on restart until offers
// re-heartbeat).
func livenessIndexAuthoritative() bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("MERC_LIVENESS_INDEX_AUTHORITATIVE")))
	return err == nil && v
}

// offerIndexLive is the flag-ON liveness decision for one offer. Fail-closed:
// an unmapped offer_slot, a nil index, an out-of-range epoch, or a slot whose
// in-process owner fingerprint is not this offer all read DEAD, so a
// missing/corrupt mapping can never make a stale offer selectable (gate D/E).
// It mirrors realtime_worker_offers.last_seen_at > now()-45s exactly for a mapped,
// heartbeating offer (proven by the shadow parity), but keyed per-offer so a live
// sibling offer of the same worker never rescues a stale one.
func (s *Store) offerIndexLive(workerID uuid.UUID, profileID string, now time.Time) bool {
	if s == nil {
		return false
	}
	s.ensureLiveDeviceIndex()
	if s.liveIndex == nil {
		return false
	}
	slot, ok := s.lookupOfferSlot(workerID, profileID)
	if !ok {
		return false
	}
	if !s.slotOwnedBy(slot, workerID, profileID) {
		return false
	}
	nowUnix := now.Unix()
	if nowUnix < 0 || nowUnix > math.MaxUint32 {
		return false
	}
	return s.liveIndex.IsLive(slot, uint32(nowUnix))
}

// Shadow-wire for LiveDeviceIndex.
//
// The index is populated from the authenticated heartbeat path and compared
// against SQL liveness. Production authorize / claim / lease / pricing MUST
// NOT call anything in this file. Losing the index may only reduce
// availability (fail-closed: empty at process start).

const (
	// Minimum capacity so a fresh control plane with no workers still has
	// room for first enrolments without growing the lock-free arrays.
	liveDeviceIndexMinCapacity = 1024
	// Extra slots beyond max(max_slot+1, worker_count). New enrolments
	// this process that stay inside this headroom keep the index lock-free
	// (no realloc). Overflow is fail-closed: Heartbeat is skipped.
	liveDeviceIndexHeadroom = 1 << 16
)

func liveIndexCapacity(maxSlotPlusOne, workerCount uint32) uint32 {
	n := maxSlotPlusOne
	if workerCount > n {
		n = workerCount
	}
	if n < liveDeviceIndexMinCapacity {
		n = liveDeviceIndexMinCapacity
	}
	if n > math.MaxUint32-liveDeviceIndexHeadroom {
		return math.MaxUint32
	}
	return n + liveDeviceIndexHeadroom
}

func (s *Store) ensureLiveDeviceIndex() {
	if s == nil {
		return
	}
	s.liveIndexOnce.Do(func() {
		n := uint32(liveDeviceIndexMinCapacity + liveDeviceIndexHeadroom)
		if s.pool != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var maxSlot *int64
			var count int64
			err := s.pool.QueryRow(ctx, `
				SELECT MAX(offer_slot), COUNT(*) FROM realtime_worker_offers`).Scan(&maxSlot, &count)
			if err == nil {
				var need uint64
				if maxSlot != nil && *maxSlot >= 0 {
					need = uint64(*maxSlot) + 1
				}
				if uint64(count) > need {
					need = uint64(count)
				}
				if need > math.MaxUint32 {
					need = math.MaxUint32
				}
				countU := uint64(count)
				if countU > math.MaxUint32 {
					countU = math.MaxUint32
				}
				n = liveIndexCapacity(uint32(need), uint32(countU))
			}
			s.slotOwner = make([]uint64, n)
			s.preloadOfferSlots(ctx)
		} else {
			s.slotOwner = make([]uint64, n)
		}
		s.liveIndex = NewLiveDeviceIndex(n)
	})
}

// offerSlotKey identifies one realtime offer (the per-(worker_id,
// runtime_profile_id) money-selection liveness unit) for the offer-grain index.
// It is the table's PRIMARY KEY (schema.sql: realtime_worker_offers).
type offerSlotKey struct {
	worker  uuid.UUID
	profile string
}

// offerBinding is everything the flag-ON heartbeat path needs to decide, without
// a durable transaction, whether this authenticated heartbeat may touch the live
// index. It mirrors exactly the conditions the durable UPDATE's WHERE clause
// checks (liveness_ingest.go): the row exists, it belongs to this supplier, and
// available_sequences is inside the offer's declared capacity. A heartbeat that
// would not have matched that WHERE clause must not become index-live either —
// otherwise the index would be a weaker authority than the SQL it replaces.
type offerBinding struct {
	slot      uint32
	supplier  uuid.UUID
	maxActive int32
}

// slotOwner reserved values. 0 is unclaimed so a never-resolved slot cannot
// match any offer. 1 is the conflict poison: a second distinct offer tried to
// claim an already-claimed slot. Neither claimant's fingerprint equals 1, so
// both read dead. The newcomer must never overwrite the original fingerprint
// — that steal is the false-positive live this table exists to prevent.
const (
	offerSlotOwnerUnclaimed uint64 = 0
	offerSlotOwnerConflict  uint64 = 1
	// offerSlotOwnerRemap replaces a hash that landed on a reserved value.
	// Golden-ratio 64-bit constant: non-zero, not the conflict sentinel.
	offerSlotOwnerRemap uint64 = 0x9e3779b97f4a7c15
)

// offerFingerprint is an FNV-1a 64-bit hash of (worker_id bytes || profile).
// worker_id is a fixed 16-byte UUID so the concatenation is unambiguous.
//
// Collision probability among n distinct (worker, profile) pairs is the
// birthday bound n(n-1)/2^65. At 10^6 offers that is ~2.7e-8; at 10^4,
// ~7e-13. A collision would let one offer's heartbeat keep the other's slot
// live — the same class of bug this table exists to stop — so the bound is
// documented, not ignored. Hashes that land on a reserved value are remapped.
func offerFingerprint(workerID uuid.UUID, profileID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(workerID[:])
	_, _ = h.Write([]byte(profileID))
	return foldOfferFingerprint(h.Sum64())
}

func foldOfferFingerprint(sum uint64) uint64 {
	if sum == offerSlotOwnerUnclaimed || sum == offerSlotOwnerConflict {
		return offerSlotOwnerRemap
	}
	return sum
}

// claimSlotOwner records that fingerprint fp owns slot. First claim wins.
// A later different fingerprint poisons the slot so every claimant reads
// dead. Never stores the newcomer's fingerprint over a live claim (steal
// would be a false-positive live). Returns whether fp now owns the slot.
//
// slotOwner is allocated once under liveIndexOnce to the same capacity as
// liveIndex; there is no realloc. Overflow (slot >= len) is fail-closed,
// matching LiveDeviceIndex.Heartbeat. Elements are atomic uint64, matching
// LiveDeviceIndex.epochs.
func (s *Store) claimSlotOwner(slot uint32, fp uint64) bool {
	if s == nil || fp == offerSlotOwnerUnclaimed || fp == offerSlotOwnerConflict {
		return false
	}
	owners := s.slotOwner
	if slot >= uint32(len(owners)) {
		return false
	}
	for {
		cur := atomic.LoadUint64(&owners[slot])
		switch cur {
		case offerSlotOwnerUnclaimed:
			if atomic.CompareAndSwapUint64(&owners[slot], offerSlotOwnerUnclaimed, fp) {
				return true
			}
		case fp:
			return true
		default:
			// Conflict (another owner, or already poisoned). Do not steal.
			if cur != offerSlotOwnerConflict {
				atomic.CompareAndSwapUint64(&owners[slot], cur, offerSlotOwnerConflict)
			}
			return false
		}
	}
}

// slotOwnedBy reports whether slot's claimed fingerprint is this offer.
// Out of range, unclaimed, conflict-poisoned, or a different owner: false.
func (s *Store) slotOwnedBy(slot uint32, workerID uuid.UUID, profileID string) bool {
	if s == nil {
		return false
	}
	owners := s.slotOwner
	if slot >= uint32(len(owners)) {
		return false
	}
	return atomic.LoadUint64(&owners[slot]) == offerFingerprint(workerID, profileID)
}

func (s *Store) preloadOfferSlots(ctx context.Context) {
	if s == nil || s.pool == nil {
		return
	}
	rows, err := s.pool.Query(ctx,
		`SELECT worker_id, runtime_profile_id, offer_slot, supplier_id, max_active_sequences
		   FROM realtime_worker_offers`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var wid, supplier uuid.UUID
		var profile string
		var slot int64
		var maxActive int32
		if err := rows.Scan(&wid, &profile, &slot, &supplier, &maxActive); err != nil {
			return
		}
		if slot >= 0 && slot <= int64(math.MaxUint32) {
			u := uint32(slot)
			s.claimSlotOwner(u, offerFingerprint(wid, profile))
			s.offerSlotCache.Store(offerSlotKey{wid, profile},
				offerBinding{slot: u, supplier: supplier, maxActive: maxActive})
		}
	}
}

// forgetOfferBinding drops a cached binding so the next lookup re-reads it.
// Registration calls this because max_active_sequences (and, in principle, the
// supplier) may have changed: a stale cached capacity would let the flag-ON path
// admit a heartbeat the durable WHERE clause would reject.
func (s *Store) forgetOfferBinding(workerID uuid.UUID, profileID string) {
	if s == nil {
		return
	}
	key := offerSlotKey{workerID, profileID}
	s.offerSlotCache.Delete(key)
	// Registration rewrites the offer row, so what we believed was persisted no
	// longer describes it. Forcing the next heartbeat to write is the safe side.
	s.offerPersistCache.Delete(key)
}

// lookupOfferBinding resolves the offer's dense slot plus the supplier and
// capacity bounds. A miss (no such offer) is fail-closed: no slot, no liveness.
func (s *Store) lookupOfferBinding(workerID uuid.UUID, profileID string) (offerBinding, bool) {
	if s == nil {
		return offerBinding{}, false
	}
	key := offerSlotKey{workerID, profileID}
	if v, ok := s.offerSlotCache.Load(key); ok {
		b, ok := v.(offerBinding)
		return b, ok
	}
	if s.pool == nil {
		return offerBinding{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var slot int64
	var b offerBinding
	err := s.pool.QueryRow(ctx,
		`SELECT offer_slot, supplier_id, max_active_sequences
		   FROM realtime_worker_offers WHERE worker_id=$1 AND runtime_profile_id=$2`,
		workerID, profileID).Scan(&slot, &b.supplier, &b.maxActive)
	if err != nil || slot < 0 || slot > math.MaxUint32 {
		return offerBinding{}, false
	}
	b.slot = uint32(slot)
	s.ensureLiveDeviceIndex()
	s.claimSlotOwner(b.slot, offerFingerprint(workerID, profileID))
	s.offerSlotCache.Store(key, b)
	return b, true
}

func (s *Store) lookupOfferSlot(workerID uuid.UUID, profileID string) (uint32, bool) {
	b, ok := s.lookupOfferBinding(workerID, profileID)
	return b.slot, ok
}

// shadowIndexHeartbeat records presence after a durable heartbeat write.
// Fail-closed on any lookup/capacity error: skip, never invent a live slot.
func (s *Store) shadowIndexHeartbeat(worker WorkerAuth, profileID string, observedAt, serverNow time.Time) {
	if s == nil {
		return
	}
	s.ensureLiveDeviceIndex()
	if s.liveIndex == nil {
		return
	}
	slot, ok := s.lookupOfferSlot(worker.WorkerID, profileID)
	if !ok {
		return
	}
	s.indexHeartbeatSlot(slot, worker.WorkerID, profileID, observedAt, serverNow)
}

// indexHeartbeatSlot records presence for an already-resolved offer slot.
// Fail-closed on any range error or owner-fingerprint mismatch: skip, never
// invent a live slot. A heartbeat must be unable to mark any slot but its
// own live — that is the invariant that actually protects routing. The caller
// is responsible for having proven the binding (see admitIndexHeartbeat).
func (s *Store) indexHeartbeatSlot(slot uint32, workerID uuid.UUID, profileID string, observedAt, serverNow time.Time) {
	if s == nil {
		return
	}
	s.ensureLiveDeviceIndex()
	if s.liveIndex == nil {
		return
	}
	if !s.slotOwnedBy(slot, workerID, profileID) {
		return
	}
	obsUnix := observedAt.Unix()
	nowUnix := serverNow.Unix()
	if obsUnix < 0 || nowUnix < 0 || nowUnix > math.MaxUint32 || obsUnix > math.MaxUint32 {
		return
	}
	_ = s.liveIndex.Heartbeat(slot, uint32(obsUnix), uint32(nowUnix))
}

// livenessDurableRefreshInterval bounds how long an actively-heartbeating offer
// may go without a durable write while the live index is authoritative.
//
// It exists because the durable row is NOT purely a liveness stamp: authorize
// still filters on `last_seen_at > now()-45s` and reads status, warmth and
// available_sequences off the same row (realtime_supplier_outcome_stats.go).
// A third of the 45s window leaves ample margin for flush and commit latency,
// so a live offer can never age out of the authorize book between refreshes.
//
// Shrinking the window below this, or widening the interval toward it, would
// make an actively-heartbeating offer disappear from the money book. Neither is
// a safe tuning knob.
const livenessDurableRefreshInterval = 15 * time.Second

// offerPersistState is the last successfully persisted heartbeat payload for one
// offer. Only the fields a heartbeat actually writes are tracked.
//
// `at` is SERVER time, not the device's observation. The interval it feeds
// protects a wall-clock property — that last_seen_at stays inside the SQL
// `now() - 45 seconds` window — and now() is the server's. Keying it to the
// device instead would let a worker whose clock lags by 40s go a further 15s
// without a write, pushing its durable stamp past the eligibility edge while it
// is still faithfully heartbeating.
type offerPersistState struct {
	warmth    string
	status    string
	available int
	at        time.Time
}

// durableHeartbeatNeeded decides whether this heartbeat has to reach PostgreSQL
// at all. This is the write retirement: while the index answers "is this offer
// alive?", an identical repeat heartbeat inside the refresh interval changes
// nothing durable, so it does not earn a transaction.
//
// It returns true whenever anything is uncertain — no record, changed payload,
// or an expired refresh deadline — because a missed write is a money-visible
// state change (a DRAINING offer left ACTIVE) while a redundant write is only
// wasted work.
func (s *Store) durableHeartbeatNeeded(worker WorkerAuth, hb RealtimeOfferHeartbeat, serverNow time.Time) bool {
	if s == nil {
		return true
	}
	v, ok := s.offerPersistCache.Load(offerSlotKey{worker.WorkerID, hb.RuntimeProfileID})
	if !ok {
		return true
	}
	prev, ok := v.(offerPersistState)
	if !ok {
		return true
	}
	if prev.warmth != hb.Warmth || prev.status != hb.Status || prev.available != hb.AvailableSequences {
		return true
	}
	return serverNow.Sub(prev.at) >= livenessDurableRefreshInterval
}

// recordDurableHeartbeat notes what a heartbeat actually persisted. Called only
// after the durable write succeeded — recording an attempted write would let a
// failing PostgreSQL suppress the retries that heal it.
func (s *Store) recordDurableHeartbeat(worker WorkerAuth, hb RealtimeOfferHeartbeat, persistedAt time.Time) {
	if s == nil {
		return
	}
	s.offerPersistCache.Store(offerSlotKey{worker.WorkerID, hb.RuntimeProfileID}, offerPersistState{
		warmth:    hb.Warmth,
		status:    hb.Status,
		available: hb.AvailableSequences,
		at:        persistedAt,
	})
}

// admitIndexHeartbeat is the flag-ON admission gate: it reproduces, without a
// durable transaction, every condition the durable UPDATE's WHERE clause checks
// before it would have stamped last_seen_at. Returns errNotFound for exactly the
// cases that produce errNotFound today, so the agent still sees 404 "realtime
// offer not registered" on an unregistered, mis-bound, or over-capacity offer.
//
// This is the security boundary of the write retirement: the index must never be
// reachable by a heartbeat the SQL predicate would have refused, or the ephemeral
// structure would be a weaker authority than the durable one it replaces.
func (s *Store) admitIndexHeartbeat(worker WorkerAuth, hb RealtimeOfferHeartbeat) (offerBinding, error) {
	if s != nil {
		s.ensureLiveDeviceIndex()
	}
	binding, ok := s.lookupOfferBinding(worker.WorkerID, hb.RuntimeProfileID)
	if !ok {
		return offerBinding{}, errNotFound
	}
	// The durable UPDATE joins on o.supplier_id=$2; an offer bound to a different
	// supplier than the authenticated worker's must not be heartbeatable.
	if binding.supplier != worker.SupplierID {
		return offerBinding{}, errNotFound
	}
	// Mirrors `i.available_sequences BETWEEN 0 AND o.max_active_sequences`. An
	// out-of-range claim matches no row durably, so it must not go live either —
	// otherwise a DRAINING/FAILED status update could be silently dropped while
	// the offer stayed index-live and therefore routable.
	if hb.AvailableSequences < 0 || int64(hb.AvailableSequences) > int64(binding.maxActive) {
		return offerBinding{}, errNotFound
	}
	// The cache can be planted onto another offer's slot (see
	// TestOfferIndexLiveCorruptMappingToLiveSlotIsIneligible). A heartbeat
	// whose resolved slot is not this offer's claimed fingerprint must not
	// be admitted — otherwise we would stamp a victim slot live.
	if !s.slotOwnedBy(binding.slot, worker.WorkerID, hb.RuntimeProfileID) {
		return offerBinding{}, errNotFound
	}
	return binding, nil
}

// shadowSelectLiveSlots is the shadow view of in-process presence.
// Production authorize/claim/lease/pricing must never call this. A process
// that just started returns empty until heartbeats arrive (fail-closed).
func (s *Store) shadowSelectLiveSlots(nowEpoch uint32) []uint32 {
	if s == nil {
		return nil
	}
	s.ensureLiveDeviceIndex()
	if s.liveIndex == nil {
		return nil
	}
	return s.liveIndex.LiveSlots(nowEpoch)
}
