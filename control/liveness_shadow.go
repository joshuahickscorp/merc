package main

import (
	"context"
	"math"
	"os"
	"strconv"
	"strings"
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
// an unmapped offer_slot, a nil index, or an out-of-range epoch all read DEAD, so
// a missing/corrupt mapping can never make a stale offer selectable (gate D/E).
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
			s.preloadOfferSlots(ctx)
		}
		s.liveIndex = NewLiveDeviceIndex(n)
	})
}

// offerSlotKey identifies one realtime offer (the per-(worker_id,
// runtime_profile_id) money-selection liveness unit) for the offer-grain index.
type offerSlotKey struct {
	worker  uuid.UUID
	profile string
}

// rememberDeviceSlot is retained for the worker-enrolment callers; the shadow no
// longer reads workers.device_slot after the offer-grain re-key (the slotCache is
// vestigial). Kept to avoid editing money-path enrolment; a later cleanup can
// drop it with workers.device_slot.
func (s *Store) rememberDeviceSlot(workerID uuid.UUID, slot uint32) {
	if s == nil {
		return
	}
	s.slotCache.Store(workerID, slot)
}

func (s *Store) preloadOfferSlots(ctx context.Context) {
	if s == nil || s.pool == nil {
		return
	}
	rows, err := s.pool.Query(ctx,
		`SELECT worker_id, runtime_profile_id, offer_slot FROM realtime_worker_offers`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var wid uuid.UUID
		var profile string
		var slot int64
		if err := rows.Scan(&wid, &profile, &slot); err != nil {
			return
		}
		if slot >= 0 && slot <= int64(math.MaxUint32) {
			s.offerSlotCache.Store(offerSlotKey{wid, profile}, uint32(slot))
		}
	}
}

func (s *Store) lookupOfferSlot(workerID uuid.UUID, profileID string) (uint32, bool) {
	if s == nil {
		return 0, false
	}
	key := offerSlotKey{workerID, profileID}
	if v, ok := s.offerSlotCache.Load(key); ok {
		slot, ok := v.(uint32)
		return slot, ok
	}
	if s.pool == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var slot int64
	err := s.pool.QueryRow(ctx,
		`SELECT offer_slot FROM realtime_worker_offers WHERE worker_id=$1 AND runtime_profile_id=$2`,
		workerID, profileID).Scan(&slot)
	if err != nil || slot < 0 || slot > math.MaxUint32 {
		return 0, false
	}
	u := uint32(slot)
	s.offerSlotCache.Store(key, u)
	return u, true
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
	obsUnix := observedAt.Unix()
	nowUnix := serverNow.Unix()
	if obsUnix < 0 || nowUnix < 0 || nowUnix > math.MaxUint32 || obsUnix > math.MaxUint32 {
		return
	}
	_ = s.liveIndex.Heartbeat(slot, uint32(obsUnix), uint32(nowUnix))
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
