package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// LiveDeviceIndex is a compact in-process live-device presence engine.
//
// It is a CORRUPTION BOUNDARY, not an authority. Losing or corrupting it may
// only REDUCE availability (devices read DEAD). It must never be treated as
// a source of eligibility, identity, money, trust, or capability — there is
// no API here that can grant any of those. A later reviewed lane may
// intersect LiveSlots with the durable selector; this type does not.
//
// AUTHENTICATION happens BEFORE Heartbeat is called. HeartbeatRealtimeOffer
// already holds a verified WorkerAuth and maps it to workers.device_slot
// (internal bookkeeping, never a credential). This index trusts the slot it
// is given; it must never be fed an unauthenticated slot.
//
// There is NO persistence. A fresh index is all-DEAD. Devices reconstruct
// liveness by heartbeating inside the 45s window.
//
// Hot representation, per slot:
//
//	last_seen_epoch  4 bytes  (packed []uint32, unix seconds)
//	wheel membership 4 bytes  (bucket+position; 0 = not on the wheel)
//	wheel occupancy  4 bytes  (slot id in one timing-wheel shard; live only)
//
// Total 12 bytes/live device, 8 bytes/dead device, plus a fixed ~32 KiB
// wheel (64 second-buckets × 16 shard mutex+slice headers).
const (
	liveDeviceWheelBuckets = 64
	liveDeviceWheelShards  = 16 // power of two; shard = slot&(shards-1)
	liveDeviceWindowEpochs = uint32(realtimeOfferLivenessWindow / time.Second)
	liveMemberPosBits      = 24
	liveMemberPosMask      = 1<<liveMemberPosBits - 1
	liveMemberBucketShift  = 24
)

var errInvalidDeviceSlot = errors.New("device slot out of range")

// LiveDeviceIndex tracks last-seen epochs for dense device slots and a
// 64-bucket second-granularity timing wheel of compact slot sets.
type LiveDeviceIndex struct {
	n      uint32
	epochs []uint32 // atomic unix-seconds; 0 = never seen
	member []uint32 // atomic packed wheel membership; 0 = absent

	wheel [liveDeviceWheelBuckets][liveDeviceWheelShards]liveWheelShard

	tickMu   sync.Mutex
	lastTick uint32
}

type liveWheelShard struct {
	mu    sync.Mutex
	slots []uint32
}

// NewLiveDeviceIndex allocates an empty, all-DEAD index of the given
// dense-slot capacity. Slots are 0 .. slots-1. There is no persistence
// and no enrolment side effect — workers.device_slot is assigned at
// enrolment; this type only sees the dense id.
func NewLiveDeviceIndex(slots uint32) *LiveDeviceIndex {
	idx := &LiveDeviceIndex{n: slots}
	if slots > 0 {
		idx.epochs = make([]uint32, slots)
		idx.member = make([]uint32, slots)
	}
	return idx
}

// Slots is the dense capacity (slot ids are [0, Slots)).
func (idx *LiveDeviceIndex) Slots() uint32 {
	if idx == nil {
		return 0
	}
	return idx.n
}

// Heartbeat records an authenticated device's observation.
//
// The caller MUST already hold a verified device-bound WorkerAuth for this
// slot. This method does not authenticate, authorise, or grant eligibility.
//
// observedEpoch is clamped to <= nowEpoch (a device may never push its own
// liveness into the future). Observations older than the 45s window are
// rejected — they are not floor-stamped, which would resurrect a dead
// device. On success, epoch[slot] = observed (clamped) and the slot moves
// to the wheel bucket for that observed epoch, not for nowEpoch.
func (idx *LiveDeviceIndex) Heartbeat(slot, observedEpoch, nowEpoch uint32) error {
	if idx == nil || slot >= idx.n {
		return errInvalidDeviceSlot
	}
	if observedEpoch > nowEpoch {
		observedEpoch = nowEpoch
	}
	if nowEpoch-observedEpoch > liveDeviceWindowEpochs {
		return errStaleHeartbeatObservation
	}
	atomic.StoreUint32(&idx.epochs[slot], observedEpoch)
	idx.moveToBucket(slot, observedEpoch%liveDeviceWheelBuckets)
	return nil
}

// IsLive reports presence. true iff epoch[slot] <= nowEpoch AND
// nowEpoch-epoch[slot] < 45. The exclusive upper bound matches production
// SQL `last_seen_at > now() - interval '45 seconds'` (dead at exactly 45s).
// The epoch<=now half is the fail-closed guard: a corrupted/garbage
// FUTURE epoch reads DEAD, never live.
//
// Heartbeat still *accepts* an observation of age==45 (same as
// resolveHeartbeatObservation); writing that stamp does not make the
// slot live, just as SQL would not treat last_seen_at = now()-45s as live.
//
// A never-heartbeated slot has epoch 0. With a real unix now that is
// stale (now-0 >= 45) and so DEAD. Out-of-range slots are DEAD.
//
// Lock-free: a single atomic load of the epoch word.
func (idx *LiveDeviceIndex) IsLive(slot, nowEpoch uint32) bool {
	if idx == nil || slot >= idx.n {
		return false
	}
	epoch := atomic.LoadUint32(&idx.epochs[slot])
	return epoch <= nowEpoch && nowEpoch-epoch < liveDeviceWindowEpochs
}

// Tick expires the oldest wheel bucket(s) at nowEpoch. Cost is O(devices
// actually sitting in those buckets), which after a regular 1s tick is
// the cohort that just aged out. IsLive does not depend on Tick: a device
// that stops at T0 is DEAD by T0+45s (age 45 is not < 45, matching SQL)
// even if Tick never ran.
//
// Clock going backwards is ignored (fail closed — do not resurrect).
func (idx *LiveDeviceIndex) Tick(nowEpoch uint32) {
	if idx == nil {
		return
	}
	idx.tickMu.Lock()
	defer idx.tickMu.Unlock()

	prev := idx.lastTick
	if prev != 0 && nowEpoch < prev {
		return
	}
	start := nowEpoch
	if prev != 0 && nowEpoch > prev {
		start = prev + 1
		if nowEpoch-prev > liveDeviceWheelBuckets {
			start = nowEpoch - liveDeviceWheelBuckets + 1
		}
	}
	for t := start; ; t++ {
		if t >= liveDeviceWindowEpochs {
			// The second that just left the exclusive 45s window
			// (age 45 is dead, matching SQL last_seen_at > now()-45s).
			idx.expireBucket((t-liveDeviceWindowEpochs)%liveDeviceWheelBuckets, nowEpoch)
		}
		// Wheel wrap: drop the generation that would collide with t+1.
		idx.expireBucket((t+1)%liveDeviceWheelBuckets, nowEpoch)
		if t == nowEpoch {
			break
		}
	}
	idx.lastTick = nowEpoch
}

// LiveSlots returns a compact list of slots that are live at nowEpoch,
// by walking the 45 in-window wheel buckets (ages 0..44) and filtering
// with IsLive.
// Dead occupants left by a late Tick are dropped (fail closed). Under
// concurrent Heartbeat a live slot may be omitted if it moves mid-walk
// — that is a reduction in availability, never a false live.
func (idx *LiveDeviceIndex) LiveSlots(nowEpoch uint32) []uint32 {
	if idx == nil || idx.n == 0 {
		return nil
	}
	// One alloc of the dense capacity: cheaper than growing through a
	// 10M-slot live set, and the result is not retained hot state.
	out := make([]uint32, 0, idx.n)
	var tmp []uint32
	// Newest first so a concurrent Heartbeat (old bucket → observed
	// bucket) cannot be appended twice; a miss is fail-closed.
	for age := uint32(0); age < liveDeviceWindowEpochs; age++ {
		if age > nowEpoch {
			break
		}
		b := (nowEpoch - age) % liveDeviceWheelBuckets
		for s := uint32(0); s < liveDeviceWheelShards; s++ {
			sh := &idx.wheel[b][s]
			sh.mu.Lock()
			n := len(sh.slots)
			if n == 0 {
				sh.mu.Unlock()
				continue
			}
			if cap(tmp) < n {
				tmp = make([]uint32, n)
			} else {
				tmp = tmp[:n]
			}
			copy(tmp, sh.slots)
			sh.mu.Unlock()
			for _, slot := range tmp {
				if idx.IsLive(slot, nowEpoch) {
					out = append(out, slot)
				}
			}
		}
	}
	return out
}

// HotBytes is the retained hot state of the index: epoch array, membership
// array, and wheel backing stores. It does not include LiveSlots output
// or the Go allocator's span waste. Divide by Slots() for bytes/device.
func (idx *LiveDeviceIndex) HotBytes() int64 {
	if idx == nil {
		return 0
	}
	var n int64
	n += int64(cap(idx.epochs)) * 4
	n += int64(cap(idx.member)) * 4
	n += int64(unsafe.Sizeof(*idx))
	for b := 0; b < liveDeviceWheelBuckets; b++ {
		for s := 0; s < liveDeviceWheelShards; s++ {
			sh := &idx.wheel[b][s]
			// Header + mutex live in idx; backing is the occupancy.
			n += int64(cap(sh.slots)) * 4
		}
	}
	return n
}

// EpochBytes is the last-seen array alone (4 bytes/slot).
func (idx *LiveDeviceIndex) EpochBytes() int64 {
	if idx == nil {
		return 0
	}
	return int64(cap(idx.epochs)) * 4
}

// WheelBytes is HotBytes minus the epoch array — membership + occupancy +
// the fixed wheel structure. This is the timing-wheel representation cost.
func (idx *LiveDeviceIndex) WheelBytes() int64 {
	if idx == nil {
		return 0
	}
	return idx.HotBytes() - idx.EpochBytes()
}

func (idx *LiveDeviceIndex) moveToBucket(slot, newB uint32) {
	newS := slot & (liveDeviceWheelShards - 1)
	for {
		cur := atomic.LoadUint32(&idx.member[slot])
		oldB, _, ok := unpackLiveMember(cur)
		if ok && oldB == newB {
			return
		}
		if !ok {
			sh := &idx.wheel[newB][newS]
			sh.mu.Lock()
			cur2 := atomic.LoadUint32(&idx.member[slot])
			if _, _, ok2 := unpackLiveMember(cur2); ok2 {
				sh.mu.Unlock()
				continue
			}
			pos := uint32(len(sh.slots))
			if pos > liveMemberPosMask {
				sh.mu.Unlock()
				return
			}
			sh.slots = append(sh.slots, slot)
			atomic.StoreUint32(&idx.member[slot], packLiveMember(newB, pos))
			sh.mu.Unlock()
			return
		}
		// Same shard index in both buckets (shard is a function of slot).
		// Lock the lower bucket first to avoid deadlock with a reverse move.
		lo, hi := oldB, newB
		if lo > hi {
			lo, hi = hi, lo
		}
		idx.wheel[lo][newS].mu.Lock()
		if hi != lo {
			idx.wheel[hi][newS].mu.Lock()
		}
		cur2 := atomic.LoadUint32(&idx.member[slot])
		oldB2, oldPos2, ok2 := unpackLiveMember(cur2)
		if !ok2 || oldB2 != oldB {
			if hi != lo {
				idx.wheel[hi][newS].mu.Unlock()
			}
			idx.wheel[lo][newS].mu.Unlock()
			continue
		}
		idx.removeLocked(oldB2, newS, oldPos2, slot)
		dst := &idx.wheel[newB][newS]
		pos := uint32(len(dst.slots))
		if pos > liveMemberPosMask {
			if hi != lo {
				idx.wheel[hi][newS].mu.Unlock()
			}
			idx.wheel[lo][newS].mu.Unlock()
			return
		}
		dst.slots = append(dst.slots, slot)
		atomic.StoreUint32(&idx.member[slot], packLiveMember(newB, pos))
		if hi != lo {
			idx.wheel[hi][newS].mu.Unlock()
		}
		idx.wheel[lo][newS].mu.Unlock()
		return
	}
}

// removeLocked drops slot from wheel[b][s]. Caller holds that shard lock.
func (idx *LiveDeviceIndex) removeLocked(b, s, pos, slot uint32) {
	sh := &idx.wheel[b][s]
	cur := sh.slots
	last := uint32(len(cur) - 1)
	if pos < last {
		moved := cur[last]
		cur[pos] = moved
		atomic.StoreUint32(&idx.member[moved], packLiveMember(b, pos))
	}
	sh.slots = cur[:last]
	atomic.StoreUint32(&idx.member[slot], 0)
}

func (idx *LiveDeviceIndex) expireBucket(b, nowEpoch uint32) {
	for s := uint32(0); s < liveDeviceWheelShards; s++ {
		sh := &idx.wheel[b][s]
		sh.mu.Lock()
		cur := sh.slots
		n := 0
		for i := 0; i < len(cur); i++ {
			slot := cur[i]
			if idx.IsLive(slot, nowEpoch) {
				if n != i {
					cur[n] = slot
					atomic.StoreUint32(&idx.member[slot], packLiveMember(b, uint32(n)))
				}
				n++
			} else {
				atomic.StoreUint32(&idx.member[slot], 0)
			}
		}
		sh.slots = cur[:n]
		sh.mu.Unlock()
	}
}

func packLiveMember(bucket, pos uint32) uint32 {
	return ((bucket + 1) << liveMemberBucketShift) | (pos & liveMemberPosMask)
}

func unpackLiveMember(m uint32) (bucket, pos uint32, ok bool) {
	if m == 0 {
		return 0, 0, false
	}
	return (m >> liveMemberBucketShift) - 1, m & liveMemberPosMask, true
}
