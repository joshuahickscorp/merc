package main

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Synthetic "now" large enough that a zero epoch is stale (fail-closed
// restart) rather than accidentally inside the 45s window.
const liveIdxTestNow = uint32(1_700_000_000)

func TestLiveDeviceIndexWindowMatchesProduction(t *testing.T) {
	if liveDeviceWindowEpochs != 45 {
		t.Fatalf("liveDeviceWindowEpochs=%d; 45s eviction must stay unchanged", liveDeviceWindowEpochs)
	}
	if realtimeOfferLivenessWindow != 45*time.Second {
		t.Fatalf("realtimeOfferLivenessWindow=%v; index window drifted from production SQL", realtimeOfferLivenessWindow)
	}
	if liveDeviceWheelBuckets != 64 {
		t.Fatalf("wheel buckets=%d want 64", liveDeviceWheelBuckets)
	}
}

func TestLiveDeviceIndexFailClosedRestart(t *testing.T) {
	// A fresh index has NO persistence. Every slot is DEAD until it heartbeats.
	const n = uint32(256)
	idx := NewLiveDeviceIndex(n)
	if idx.Slots() != n {
		t.Fatalf("slots=%d want %d", idx.Slots(), n)
	}
	for slot := uint32(0); slot < n; slot++ {
		if idx.IsLive(slot, liveIdxTestNow) {
			t.Fatalf("fresh index slot %d is live; restart must be all-DEAD", slot)
		}
	}
	if got := idx.LiveSlots(liveIdxTestNow); len(got) != 0 {
		t.Fatalf("fresh LiveSlots=%v; want empty", got)
	}

	if err := idx.Heartbeat(7, liveIdxTestNow, liveIdxTestNow); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !idx.IsLive(7, liveIdxTestNow) {
		t.Fatal("slot 7 must be live after its own heartbeat")
	}
	for slot := uint32(0); slot < n; slot++ {
		if slot == 7 {
			continue
		}
		if idx.IsLive(slot, liveIdxTestNow) {
			t.Fatalf("slot %d became live without a heartbeat", slot)
		}
	}

	// A brand-new index does not inherit the previous one's state.
	fresh := NewLiveDeviceIndex(n)
	if fresh.IsLive(7, liveIdxTestNow) {
		t.Fatal("new index reconstructed liveness without a heartbeat")
	}
}

func TestLiveDeviceIndexCorruptionBoundary(t *testing.T) {
	// Losing or scrambling the index may only REDUCE availability.
	const n = uint32(32)
	idx := NewLiveDeviceIndex(n)
	now := liveIdxTestNow
	for slot := uint32(0); slot < 10; slot++ {
		if err := idx.Heartbeat(slot, now, now); err != nil {
			t.Fatalf("heartbeat %d: %v", slot, err)
		}
		if !idx.IsLive(slot, now) {
			t.Fatalf("slot %d not live after heartbeat", slot)
		}
	}

	// Clear: zero every epoch. Presence must collapse to DEAD.
	for slot := uint32(0); slot < n; slot++ {
		atomic.StoreUint32(&idx.epochs[slot], 0)
	}
	for slot := uint32(0); slot < 10; slot++ {
		if idx.IsLive(slot, now) {
			t.Fatalf("cleared slot %d still live; corruption must fail closed", slot)
		}
	}
	if got := liveSlotsIgnoringWheel(idx, now); len(got) != 0 {
		t.Fatalf("cleared epochs still produce live slots: %v", got)
	}

	// Re-heartbeat, then scramble epochs to FUTURE garbage → DEAD.
	if err := idx.Heartbeat(3, now, now); err != nil {
		t.Fatal(err)
	}
	futures := []uint32{now + 1, now + 1_000_000, ^uint32(0)}
	for _, garbage := range futures {
		atomic.StoreUint32(&idx.epochs[3], garbage)
		if idx.IsLive(3, now) {
			t.Fatalf("future epoch %d reads live; fail-closed guard (epoch<=now) broken", garbage)
		}
	}

	// Scramble the wheel independently of epochs: a dead slot planted in a
	// live bucket must not survive LiveSlots (IsLive is the authority).
	atomic.StoreUint32(&idx.epochs[3], 0)
	idx.wheel[now%liveDeviceWheelBuckets][3&(liveDeviceWheelShards-1)].mu.Lock()
	idx.wheel[now%liveDeviceWheelBuckets][3&(liveDeviceWheelShards-1)].slots = append(
		idx.wheel[now%liveDeviceWheelBuckets][3&(liveDeviceWheelShards-1)].slots, 3)
	idx.wheel[now%liveDeviceWheelBuckets][3&(liveDeviceWheelShards-1)].mu.Unlock()
	for _, slot := range idx.LiveSlots(now) {
		if slot == 3 {
			t.Fatal("LiveSlots returned a wheel-planted dead slot; presence leaked")
		}
		if !idx.IsLive(slot, now) {
			t.Fatalf("LiveSlots returned dead slot %d", slot)
		}
	}

	// Nothing in this type can grant eligibility — there is no such API.
	typ := reflect.TypeOf(idx)
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		lower := strings.ToLower(name)
		for _, banned := range []string{"eligib", "authoriz", "payout", "grant", "trust", "capabilit", "charge", "settle"} {
			if strings.Contains(lower, banned) {
				t.Fatalf("index exposes %s — presence must not grow an eligibility/money surface", name)
			}
		}
	}
}

func TestLiveDeviceIndexFutureEpochDead(t *testing.T) {
	idx := NewLiveDeviceIndex(4)
	now := liveIdxTestNow
	if err := idx.Heartbeat(1, now, now); err != nil {
		t.Fatal(err)
	}
	// Direct corruption: stamp a future epoch. IsLive must not treat it as live.
	atomic.StoreUint32(&idx.epochs[1], now+30)
	if idx.IsLive(1, now) {
		t.Fatal("garbage future epoch reads live; epoch<=now fail-closed guard failed")
	}
	if idx.IsLive(1, now+29) {
		t.Fatal("future epoch still ahead of now+29; must read DEAD")
	}
	// Only once now catches up to the (still-corrupt) stamp would the
	// numeric predicate admit it — and even then a real caller never
	// writes a future stamp (Heartbeat clamps). Document the clamp path:
	if err := idx.Heartbeat(1, now+30, now); err != nil {
		t.Fatalf("clamped heartbeat: %v", err)
	}
	if got := atomic.LoadUint32(&idx.epochs[1]); got != now {
		t.Fatalf("clamped epoch=%d want %d; device pushed liveness into the future", got, now)
	}
}

func TestLiveDeviceIndexEviction45sIndependentOfTick(t *testing.T) {
	// A device that stops heartbeating at T0 is DEAD by T0+46s (age 46 > 45)
	// regardless of tick timing or wheel bucketing. Live at T0+45 (window
	// edge, matching Heartbeat's accept of age==45).
	t0 := liveIdxTestNow
	schedules := []struct {
		name string
		tick func(idx *LiveDeviceIndex, at uint32)
	}{
		{"no_ticks", func(*LiveDeviceIndex, uint32) {}},
		{"tick_every_second", func(idx *LiveDeviceIndex, at uint32) { idx.Tick(at) }},
		{"tick_late", func(idx *LiveDeviceIndex, at uint32) {
			if at == t0+100 {
				idx.Tick(at)
			}
		}},
		{"tick_only_before_hb", func(idx *LiveDeviceIndex, at uint32) {
			if at == t0-1 {
				idx.Tick(at)
			}
		}},
		{"tick_at_window_edge", func(idx *LiveDeviceIndex, at uint32) {
			if at == t0+45 || at == t0+46 {
				idx.Tick(at)
			}
		}},
		{"tick_storm_after_death", func(idx *LiveDeviceIndex, at uint32) {
			if at >= t0+46 {
				idx.Tick(at)
			}
		}},
	}
	for _, sc := range schedules {
		t.Run(sc.name, func(t *testing.T) {
			idx := NewLiveDeviceIndex(8)
			sc.tick(idx, t0-1)
			if err := idx.Heartbeat(3, t0, t0); err != nil {
				t.Fatal(err)
			}
			for at := t0; at <= t0+60; at++ {
				sc.tick(idx, at)
				live := idx.IsLive(3, at)
				switch {
				case at <= t0+liveDeviceWindowEpochs && !live:
					t.Fatalf("at T0+%d (age %d) slot is DEAD; window is 45s", at-t0, at-t0)
				case at > t0+liveDeviceWindowEpochs && live:
					t.Fatalf("at T0+%d (age %d) slot is still LIVE; 45s eviction broken (schedule %s)", at-t0, at-t0, sc.name)
				}
			}
		})
	}
}

func TestLiveDeviceIndexClampFutureObservation(t *testing.T) {
	idx := NewLiveDeviceIndex(4)
	now := liveIdxTestNow
	if err := idx.Heartbeat(0, now+600, now); err != nil {
		t.Fatalf("future observation must clamp, not reject: %v", err)
	}
	if got := atomic.LoadUint32(&idx.epochs[0]); got != now {
		t.Fatalf("epoch=%d want clamped %d", got, now)
	}
	if !idx.IsLive(0, now) {
		t.Fatal("clamped-to-now observation must be live at now")
	}
	// Clamping must not extend liveness past a now-stamped heartbeat.
	if idx.IsLive(0, now+liveDeviceWindowEpochs+1) {
		t.Fatal("clamped future observation extended liveness past the window")
	}
	b, ok := liveMemberBucket(idx, 0)
	if !ok || b != now%liveDeviceWheelBuckets {
		t.Fatalf("bucket=%d ok=%v want %d (now, not the future observation)", b, ok, now%liveDeviceWheelBuckets)
	}
}

func TestLiveDeviceIndexRejectsStaleNotFloorStamp(t *testing.T) {
	idx := NewLiveDeviceIndex(4)
	now := liveIdxTestNow
	if err := idx.Heartbeat(1, now, now); err != nil {
		t.Fatal(err)
	}

	// Exactly at the window edge is accepted (same as resolveHeartbeatObservation).
	edge := now + liveDeviceWindowEpochs
	if err := idx.Heartbeat(1, now, edge); err != nil {
		t.Fatalf("window-edge observation: %v", err)
	}
	if got := atomic.LoadUint32(&idx.epochs[1]); got != now {
		t.Fatalf("edge epoch=%d want %d", got, now)
	}

	// Older than the window is rejected — not floor-stamped to now (that
	// would resurrect a device whose last real observation is already dead).
	staleNow := now + liveDeviceWindowEpochs + 1
	err := idx.Heartbeat(1, now, staleNow)
	if !errors.Is(err, errStaleHeartbeatObservation) {
		t.Fatalf("stale: got %v want errStaleHeartbeatObservation", err)
	}
	if got := atomic.LoadUint32(&idx.epochs[1]); got != now {
		t.Fatalf("rejected stale write mutated epoch to %d; must leave prior stamp", got)
	}
	if idx.IsLive(1, staleNow) {
		t.Fatal("stale heartbeat resurrected a dead device (floor-stamp?)")
	}
}

func TestLiveDeviceIndexReplayDoesNotAdvanceBucket(t *testing.T) {
	idx := NewLiveDeviceIndex(4)
	// First observation at T=100 relative to a synthetic base so buckets
	// are easy to compute. Use a large base so epoch 0 stays stale.
	base := liveIdxTestNow
	tObs := base + 100
	if err := idx.Heartbeat(2, tObs, tObs); err != nil {
		t.Fatal(err)
	}
	firstB, ok := liveMemberBucket(idx, 2)
	if !ok || firstB != tObs%liveDeviceWheelBuckets {
		t.Fatalf("first bucket=%d ok=%v want %d", firstB, ok, tObs%liveDeviceWheelBuckets)
	}

	// Replayed OLD heartbeat (observed=90) at now=110. Accepted (age 20),
	// but the slot must land in the bucket for 90, not for 110.
	replayObs := base + 90
	replayNow := base + 110
	if err := idx.Heartbeat(2, replayObs, replayNow); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := atomic.LoadUint32(&idx.epochs[2]); got != replayObs {
		t.Fatalf("replay epoch=%d want observed %d (not now)", got, replayObs)
	}
	gotB, ok := liveMemberBucket(idx, 2)
	wantB := replayObs % liveDeviceWheelBuckets
	newerB := replayNow % liveDeviceWheelBuckets
	if !ok || gotB != wantB {
		t.Fatalf("replay bucket=%d ok=%v want %d (observed), not %d (now)", gotB, ok, wantB, newerB)
	}
	if gotB == newerB && wantB != newerB {
		t.Fatal("replayed old heartbeat moved the slot to the current (newer) bucket")
	}
}

func TestLiveDeviceIndexAuthenticationContract(t *testing.T) {
	// Heartbeat takes a dense slot, not a WorkerAuth or token. Authentication
	// is the caller's job and must happen first. This test locks that
	// contract: no method accepts WorkerAuth, and the comment on Heartbeat
	// names the requirement so a later wiring lane cannot "forget" it.
	var idx *LiveDeviceIndex
	typ := reflect.TypeOf(idx)
	authType := reflect.TypeOf(WorkerAuth{})
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		for p := 1; p < m.Type.NumIn(); p++ { // 0 is receiver
			if m.Type.In(p) == authType || (m.Type.In(p).Kind() == reflect.Ptr && m.Type.In(p).Elem() == authType) {
				t.Fatalf("%s accepts WorkerAuth — the index must not authenticate", m.Name)
			}
		}
	}
	hb, ok := typ.MethodByName("Heartbeat")
	if !ok {
		t.Fatal("Heartbeat missing")
	}
	// slot, observedEpoch, nowEpoch — three uint32s. No token, no identity.
	if hb.Type.NumIn() != 4 { // recv + 3
		t.Fatalf("Heartbeat signature changed (in=%d); auth must stay at the caller", hb.Type.NumIn())
	}
	for p := 1; p < hb.Type.NumIn(); p++ {
		if hb.Type.In(p).Kind() != reflect.Uint32 {
			t.Fatalf("Heartbeat arg %d is %s; only authenticated dense slots belong here", p, hb.Type.In(p))
		}
	}
}

func TestLiveDeviceIndexPresenceOperations(t *testing.T) {
	idx := NewLiveDeviceIndex(64)
	now := liveIdxTestNow
	if idx.IsLive(0, now) {
		t.Fatal("empty slot live")
	}
	if err := idx.Heartbeat(0, now, now); err != nil {
		t.Fatal(err)
	}
	if err := idx.Heartbeat(1, now-10, now); err != nil {
		t.Fatal(err)
	}
	if err := idx.Heartbeat(2, now-liveDeviceWindowEpochs, now); err != nil {
		t.Fatal(err)
	}
	if !idx.IsLive(0, now) || !idx.IsLive(1, now) || !idx.IsLive(2, now) {
		t.Fatal("expected slots 0,1,2 live")
	}
	if err := idx.Heartbeat(99, now, now); !errors.Is(err, errInvalidDeviceSlot) {
		t.Fatalf("out-of-range: %v", err)
	}
	if idx.IsLive(99, now) {
		t.Fatal("out-of-range slot must be DEAD")
	}

	got := idx.LiveSlots(now)
	if len(got) != 3 {
		t.Fatalf("LiveSlots=%v want 3 slots", got)
	}
	seen := map[uint32]bool{}
	for _, s := range got {
		seen[s] = true
		if !idx.IsLive(s, now) {
			t.Fatalf("LiveSlots returned dead %d", s)
		}
	}
	for _, s := range []uint32{0, 1, 2} {
		if !seen[s] {
			t.Fatalf("LiveSlots missing %d", s)
		}
	}

	idx.Tick(now)
	if !idx.IsLive(0, now) {
		t.Fatal("Tick at now must not evict a just-heartbeated slot")
	}

	// Move: a later heartbeat relocates the slot.
	later := now + 5
	if err := idx.Heartbeat(1, later, later); err != nil {
		t.Fatal(err)
	}
	b, ok := liveMemberBucket(idx, 1)
	if !ok || b != later%liveDeviceWheelBuckets {
		t.Fatalf("moved bucket=%d ok=%v want %d", b, ok, later%liveDeviceWheelBuckets)
	}
}

func TestLiveDeviceIndexHotBytesBudget(t *testing.T) {
	// At a size where shard-header amortization is honest, stay under the
	// 32 byte/device target (stretch 16).
	const n = uint32(50_000)
	idx := NewLiveDeviceIndex(n)
	now := liveIdxTestNow
	for slot := uint32(0); slot < n; slot++ {
		if err := idx.Heartbeat(slot, now, now); err != nil {
			t.Fatal(err)
		}
	}
	per := float64(idx.HotBytes()) / float64(n)
	if per > 32 {
		t.Fatalf("hot bytes/device=%.2f exceeds 32-byte target (hot=%d wheel=%d epochs=%d)",
			per, idx.HotBytes(), idx.WheelBytes(), idx.EpochBytes())
	}
	t.Logf("hot=%.2f B/device epochs=%.2f wheel=%.2f (target <=32, stretch <=16)",
		per, float64(idx.EpochBytes())/float64(n), float64(idx.WheelBytes())/float64(n))
}

func TestLiveDeviceIndexConcurrentSafety(t *testing.T) {
	const n = uint32(4096)
	idx := NewLiveDeviceIndex(n)
	base := liveIdxTestNow
	var wg sync.WaitGroup

	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			slot := uint32(g)
			for i := 0; i < 4000; i++ {
				now := base + uint32(i%30)
				_ = idx.Heartbeat((slot+uint32(i))%n, now, now+uint32(g%3))
			}
		}(g)
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 8000; i++ {
				_ = idx.IsLive(uint32(i)%n, base+uint32(i%40))
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			idx.Tick(base + uint32(i%50))
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			slots := idx.LiveSlots(base + uint32(i%40))
			for _, s := range slots {
				if s >= n {
					t.Errorf("LiveSlots produced out-of-range %d", s)
					return
				}
			}
		}
	}()
	wg.Wait()

	// After the storm, a clean heartbeat must still take and IsLive must
	// agree with the epoch word (fail-closed predicate).
	if err := idx.Heartbeat(1, base+10, base+10); err != nil {
		t.Fatal(err)
	}
	if !idx.IsLive(1, base+10) {
		t.Fatal("slot 1 dead after post-storm heartbeat")
	}
	if idx.IsLive(1, base+10+liveDeviceWindowEpochs+1) {
		t.Fatal("slot 1 still live past the window after the storm")
	}
}

func liveMemberBucket(idx *LiveDeviceIndex, slot uint32) (uint32, bool) {
	b, _, ok := unpackLiveMember(atomic.LoadUint32(&idx.member[slot]))
	return b, ok
}

func liveSlotsIgnoringWheel(idx *LiveDeviceIndex, now uint32) []uint32 {
	var out []uint32
	for slot := uint32(0); slot < idx.n; slot++ {
		if idx.IsLive(slot, now) {
			out = append(out, slot)
		}
	}
	return out
}
