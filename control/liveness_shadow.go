package main

import (
	"context"
	"math"
	"time"

	"github.com/google/uuid"
)

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
				SELECT MAX(device_slot), COUNT(*) FROM workers`).Scan(&maxSlot, &count)
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
			s.preloadDeviceSlots(ctx)
		}
		s.liveIndex = NewLiveDeviceIndex(n)
	})
}

func (s *Store) preloadDeviceSlots(ctx context.Context) {
	if s == nil || s.pool == nil {
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT id, device_slot FROM workers`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var slot int64
		if err := rows.Scan(&id, &slot); err != nil {
			return
		}
		if slot >= 0 && slot <= int64(math.MaxUint32) {
			s.slotCache.Store(id, uint32(slot))
		}
	}
}

func (s *Store) rememberDeviceSlot(workerID uuid.UUID, slot uint32) {
	if s == nil {
		return
	}
	s.slotCache.Store(workerID, slot)
}

func (s *Store) lookupDeviceSlot(workerID uuid.UUID) (uint32, bool) {
	if s == nil {
		return 0, false
	}
	if v, ok := s.slotCache.Load(workerID); ok {
		slot, ok := v.(uint32)
		return slot, ok
	}
	if s.pool == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var slot int64
	err := s.pool.QueryRow(ctx, `SELECT device_slot FROM workers WHERE id=$1`, workerID).Scan(&slot)
	if err != nil || slot < 0 || slot > math.MaxUint32 {
		return 0, false
	}
	u := uint32(slot)
	s.slotCache.Store(workerID, u)
	return u, true
}

// shadowIndexHeartbeat records presence after a durable heartbeat write.
// Fail-closed on any lookup/capacity error: skip, never invent a live slot.
func (s *Store) shadowIndexHeartbeat(worker WorkerAuth, observedAt, serverNow time.Time) {
	if s == nil {
		return
	}
	s.ensureLiveDeviceIndex()
	if s.liveIndex == nil {
		return
	}
	slot, ok := s.lookupDeviceSlot(worker.WorkerID)
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
