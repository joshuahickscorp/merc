package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestOfferIndexLiveCorruptMappingToLiveSlotIsIneligible plants a wrong-but-in-range
// offer→slot mapping for a stale offer A that points at the slot of a live offer B,
// then asserts offerIndexLive reports A dead. TestLiveDeviceIndexCorruptionBoundary
// only scrambles epochs, never the mapping.
//
// Seam: offerSlotCache. The durable offer_slot column is UNIQUE, so two live rows
// cannot share a slot in PostgreSQL; the in-process cache is the only place a
// swapped in-range mapping can be planted while B stays live on its own slot.
//
// At HEAD this test FAILS: offerIndexLive trusts lookupOfferSlot then
// IsLive(slot), so aliasing A onto B's live slot makes A selectable. The
// in-process slotOwner fingerprint must disagree with A, so offerIndexLive
// fails closed without a PostgreSQL round trip.
func TestOfferIndexLiveCorruptMappingToLiveSlotIsIneligible(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "liveness-map-corrupt-key-32-bytes-min!!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]

	workerA := seedOneRealtimeOffer(t, ctx, store, pool, profile)
	workerB := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	// Restart: empty index, mappings preloaded from the durable column. SQL
	// last_seen_at is still fresh from registration; A is index-dead until we
	// plant. Heartbeat only B so B is the live sibling.
	store = NewStore(pool)
	if err := store.adoptMigratedSchema(ctx); err != nil {
		t.Fatalf("restart adopt: %v", err)
	}
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	if err := store.HeartbeatRealtimeOffer(ctx, workerB, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("heartbeat B: %v", err)
	}
	now := time.Now().UTC()
	if !store.offerIndexLive(workerB.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("offer B must be live after its own heartbeat")
	}
	if store.offerIndexLive(workerA.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("offer A must be index-dead before the planted mapping (fresh restart, no heartbeat)")
	}

	slotA, ok := store.lookupOfferSlot(workerA.WorkerID, profile.RuntimeProfileID)
	if !ok {
		t.Fatal("offer A has no offer_slot")
	}
	slotB, ok := store.lookupOfferSlot(workerB.WorkerID, profile.RuntimeProfileID)
	if !ok {
		t.Fatal("offer B has no offer_slot")
	}
	if slotA == slotB {
		t.Fatalf("A and B share offer_slot %d — cannot plant a swapped mapping", slotA)
	}

	// Plant: A now resolves to B's live slot. A wrong-but-in-range mapping that
	// aliases a live sibling must not make the stale offer selectable.
	plantOfferSlotCache(t, store, workerA.WorkerID, profile.RuntimeProfileID, slotB)
	gotSlot, ok := store.lookupOfferSlot(workerA.WorkerID, profile.RuntimeProfileID)
	if !ok || gotSlot != slotB {
		t.Fatalf("plant did not take: slot=%d ok=%v want B's slot %d", gotSlot, ok, slotB)
	}
	if store.offerIndexLive(workerA.WorkerID, profile.RuntimeProfileID, time.Now().UTC()) {
		t.Fatal("offerIndexLive reported A live via a planted mapping onto B's live slot — a corrupt offer→slot mapping must be ineligible")
	}
	if !store.offerIndexLive(workerB.WorkerID, profile.RuntimeProfileID, time.Now().UTC()) {
		t.Fatal("planting A's mapping must not take B down")
	}
}

// TestOfferIndexLiveCorruptMappingHeartbeatDoesNotKeepVictimLive is the write
// direction of the same plant: A's cache points at B's slot, A heartbeats, and
// that heartbeat must not mark B live (B has sent nothing) or A live (A does
// not own the slot it resolved).
func TestOfferIndexLiveCorruptMappingHeartbeatDoesNotKeepVictimLive(t *testing.T) {
	// Flag OFF so the heartbeat is admitted and reaches indexHeartbeatSlot —
	// that is the write the planted mapping would otherwise apply to B.
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "0")
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "liveness-map-write-key-32-bytes-min!!!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]

	workerA := seedOneRealtimeOffer(t, ctx, store, pool, profile)
	workerB := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	store = NewStore(pool)
	if err := store.adoptMigratedSchema(ctx); err != nil {
		t.Fatalf("restart adopt: %v", err)
	}
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	now := time.Now().UTC()
	if store.offerIndexLive(workerA.WorkerID, profile.RuntimeProfileID, now) ||
		store.offerIndexLive(workerB.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("fresh restart must leave both offers index-dead")
	}

	slotB, ok := store.lookupOfferSlot(workerB.WorkerID, profile.RuntimeProfileID)
	if !ok {
		t.Fatal("offer B has no offer_slot")
	}
	plantOfferSlotCache(t, store, workerA.WorkerID, profile.RuntimeProfileID, slotB)
	gotSlot, ok := store.lookupOfferSlot(workerA.WorkerID, profile.RuntimeProfileID)
	if !ok || gotSlot != slotB {
		t.Fatalf("plant did not take: slot=%d ok=%v want B's slot %d", gotSlot, ok, slotB)
	}

	if err := store.HeartbeatRealtimeOffer(ctx, workerA, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("heartbeat A: %v", err)
	}
	now = time.Now().UTC()
	if store.offerIndexLive(workerB.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("A's heartbeat kept B live via a planted mapping — a heartbeat must be unable to mark any slot but its own live")
	}
	if store.offerIndexLive(workerA.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("A read live after heartbeating through a planted mapping onto B's slot")
	}
	store.ensureLiveDeviceIndex()
	if store.liveIndex != nil && store.liveIndex.IsLive(slotB, uint32(now.Unix())) {
		t.Fatal("A's heartbeat wrote B's slot in the live index")
	}
}

// TestAdmitIndexHeartbeatRejectsPlantedForeignSlot is the flag-ON half of the
// write-direction plant: admitIndexHeartbeat must refuse a binding whose
// resolved slot is not this offer's claimed fingerprint, so the heartbeat
// never reaches indexHeartbeatSlot.
func TestAdmitIndexHeartbeatRejectsPlantedForeignSlot(t *testing.T) {
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "liveness-map-admit-key-32-bytes-min!!!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]

	workerA := seedOneRealtimeOffer(t, ctx, store, pool, profile)
	workerB := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	store = NewStore(pool)
	if err := store.adoptMigratedSchema(ctx); err != nil {
		t.Fatalf("restart adopt: %v", err)
	}
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})

	slotB, ok := store.lookupOfferSlot(workerB.WorkerID, profile.RuntimeProfileID)
	if !ok {
		t.Fatal("offer B has no offer_slot")
	}
	plantOfferSlotCache(t, store, workerA.WorkerID, profile.RuntimeProfileID, slotB)

	err := store.HeartbeatRealtimeOffer(ctx, workerA, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	})
	if err == nil {
		t.Fatal("flag ON: planted mapping onto B's slot must not be admitted")
	}
	now := time.Now().UTC()
	if store.offerIndexLive(workerB.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("refused heartbeat still marked B live")
	}
	if store.offerIndexLive(workerA.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("refused heartbeat still marked A live")
	}
}

// TestOfferIndexLiveConflictingSlotClaimMakesBothIneligible: a second offer
// claiming an already-claimed slot must be refused, the slot poisoned, and
// both claimants read dead. Stealing (writing the newcomer's fingerprint)
// would make the newcomer a false-positive live.
func TestOfferIndexLiveConflictingSlotClaimMakesBothIneligible(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "liveness-map-conflict-key-32-bytes-min")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]

	workerA := seedOneRealtimeOffer(t, ctx, store, pool, profile)
	workerB := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	store = NewStore(pool)
	if err := store.adoptMigratedSchema(ctx); err != nil {
		t.Fatalf("restart adopt: %v", err)
	}
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	if err := store.HeartbeatRealtimeOffer(ctx, workerB, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("heartbeat B: %v", err)
	}
	now := time.Now().UTC()
	if !store.offerIndexLive(workerB.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("offer B must be live after its own heartbeat")
	}

	slotB, ok := store.lookupOfferSlot(workerB.WorkerID, profile.RuntimeProfileID)
	if !ok {
		t.Fatal("offer B has no offer_slot")
	}
	if offerFingerprint(workerA.WorkerID, profile.RuntimeProfileID) ==
		offerFingerprint(workerB.WorkerID, profile.RuntimeProfileID) {
		t.Fatal("fingerprint collision between A and B — cannot test a conflicting claim")
	}
	if store.slotOwnedBy(slotB, workerA.WorkerID, profile.RuntimeProfileID) {
		t.Fatal("A must not already own B's slot")
	}

	fpA := offerFingerprint(workerA.WorkerID, profile.RuntimeProfileID)
	if store.claimSlotOwner(slotB, fpA) {
		t.Fatal("conflicting claim must be refused — the newcomer must not steal the slot")
	}
	if store.slotOwnedBy(slotB, workerA.WorkerID, profile.RuntimeProfileID) {
		t.Fatal("newcomer fingerprint is on B's slot — steal, not poison")
	}
	if store.slotOwnedBy(slotB, workerB.WorkerID, profile.RuntimeProfileID) {
		t.Fatal("original owner still matches after a conflicting claim — both claimants must read dead")
	}

	// Both offers now resolve to the disputed slot so offerIndexLive sees it.
	plantOfferSlotCache(t, store, workerA.WorkerID, profile.RuntimeProfileID, slotB)
	now = time.Now().UTC()
	if store.offerIndexLive(workerA.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("newcomer A reads live after a conflicting claim — steal is a false-positive live")
	}
	if store.offerIndexLive(workerB.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("original owner B still reads live after a conflicting claim on its slot")
	}

	if err := store.HeartbeatRealtimeOffer(ctx, workerA, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("heartbeat A after conflict: %v", err)
	}
	if err := store.HeartbeatRealtimeOffer(ctx, workerB, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("heartbeat B after conflict: %v", err)
	}
	now = time.Now().UTC()
	if store.offerIndexLive(workerA.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("heartbeat as A after a conflicting claim resurrected A")
	}
	if store.offerIndexLive(workerB.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("heartbeat as B after a conflicting claim resurrected B")
	}
	if store.claimSlotOwner(slotB, offerFingerprint(workerB.WorkerID, profile.RuntimeProfileID)) {
		t.Fatal("original owner reclaimed a poisoned slot")
	}
}

func TestFoldOfferFingerprintReservesUnclaimedAndConflict(t *testing.T) {
	if got := foldOfferFingerprint(offerSlotOwnerUnclaimed); got != offerSlotOwnerRemap {
		t.Fatalf("hash 0 remapped to %d want %d", got, offerSlotOwnerRemap)
	}
	if got := foldOfferFingerprint(offerSlotOwnerConflict); got != offerSlotOwnerRemap {
		t.Fatalf("hash 1 remapped to %d want %d", got, offerSlotOwnerRemap)
	}
	if got := foldOfferFingerprint(99); got != 99 {
		t.Fatalf("non-reserved hash remapped: got %d", got)
	}
	a := offerFingerprint(uuid.MustParse("00000000-0000-0000-0000-000000000001"), "p")
	b := offerFingerprint(uuid.MustParse("00000000-0000-0000-0000-000000000002"), "p")
	if a == offerSlotOwnerUnclaimed || a == offerSlotOwnerConflict {
		t.Fatalf("offerFingerprint returned reserved value %d", a)
	}
	if a == b {
		t.Fatal("distinct offers hashed to the same fingerprint")
	}
}

func TestSlotOwnerAddsEightBytesPerSlotWithoutRegressingIndexHotBytes(t *testing.T) {
	store := &Store{}
	store.ensureLiveDeviceIndex()
	if store.liveIndex == nil {
		t.Fatal("live index is nil")
	}
	n := store.liveIndex.Slots()
	if n == 0 {
		t.Fatal("live index has zero capacity")
	}
	if uint32(len(store.slotOwner)) != n || uint32(cap(store.slotOwner)) != n {
		t.Fatalf("slotOwner len=%d cap=%d want %d (must match live index)",
			len(store.slotOwner), cap(store.slotOwner), n)
	}
	added := cap(store.slotOwner) * 8
	if per := added / int(n); per != 8 {
		t.Fatalf("slotOwner cost=%d B/slot want 8", per)
	}
	lone := NewLiveDeviceIndex(n)
	if store.liveIndex.HotBytes() != lone.HotBytes() {
		t.Fatalf("liveIndex.HotBytes()=%d standalone=%d; slotOwner must not enter index accounting",
			store.liveIndex.HotBytes(), lone.HotBytes())
	}
	t.Logf("slotOwner +%d B (%d slots × 8 B); index HotBytes=%d (unchanged vs standalone)",
		added, n, store.liveIndex.HotBytes())
}

func TestClaimSlotOwnerConcurrentDistinctFingerprintsPoison(t *testing.T) {
	store := &Store{}
	store.ensureLiveDeviceIndex()
	const slot uint32 = 7
	fpA := offerFingerprint(uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), "p")
	fpB := offerFingerprint(uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"), "p")
	if fpA == fpB {
		t.Fatal("test fingerprints collided")
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		fp := fpA
		if i%2 == 1 {
			fp = fpB
		}
		go func(fp uint64) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = store.claimSlotOwner(slot, fp)
			}
		}(fp)
	}
	wg.Wait()

	if store.claimSlotOwner(slot, fpA) || store.claimSlotOwner(slot, fpB) {
		t.Fatal("concurrent distinct claims left a winner; slot must be poisoned")
	}
	if atomic.LoadUint64(&store.slotOwner[slot]) != offerSlotOwnerConflict {
		t.Fatalf("slotOwner[%d]=%d want conflict sentinel %d",
			slot, atomic.LoadUint64(&store.slotOwner[slot]), offerSlotOwnerConflict)
	}
}

// TestOfferGrainSlotsIndependentWithoutSecondCatalogueProfile proves the same
// sibling-isolation property as TestShadowIndexPerOfferGrainNoSiblingRescue at a
// level that does not need a second advertised vLLM profile.
//
// A second offer row is inserted directly in SQL on the SAME worker (bypassing
// the profile catalogue and UpsertRealtimeOffer). That is a fair test of
// offer-grain vs worker-grain: two distinct offer_slots on one worker must be
// independently live/dead, and a live slot must never rescue a stale one.
//
// This does NOT cover two catalogue-advertised profiles going through
// UpsertRealtimeOffer (that path still skips when only one profile is advertised).
func TestOfferGrainSlotsIndependentWithoutSecondCatalogueProfile(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "liveness-sibling-sql-key-32-bytes-min!!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	siblingProfile := profile.RuntimeProfileID + "-sql-sibling"
	insertSiblingRealtimeOfferSQL(t, ctx, pool, worker, profile.RuntimeProfileID, siblingProfile)

	slot1, ok := store.lookupOfferSlot(worker.WorkerID, profile.RuntimeProfileID)
	if !ok {
		t.Fatal("catalogue offer has no offer_slot")
	}
	slot2, ok := store.lookupOfferSlot(worker.WorkerID, siblingProfile)
	if !ok {
		t.Fatal("SQL sibling offer has no offer_slot — insert did not become visible to lookupOfferSlot")
	}
	if slot1 == slot2 {
		t.Fatalf("two offers on one worker share offer_slot %d — grain is worker not offer", slot1)
	}

	// Heartbeat ONLY the catalogue offer. The SQL sibling must stay dead.
	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("heartbeat catalogue offer: %v", err)
	}
	now := time.Now().UTC()
	if !store.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("catalogue offer not live after its own heartbeat")
	}
	if store.offerIndexLive(worker.WorkerID, siblingProfile, now) {
		t.Fatal("SQL sibling offer is LIVE without its own heartbeat — a live slot rescued a stale one")
	}

	// Heartbeat the sibling (HeartbeatRealtimeOffer does not consult the
	// catalogue). Both must now be live, independently.
	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: siblingProfile, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("heartbeat SQL sibling: %v", err)
	}
	now = time.Now().UTC()
	if !store.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("catalogue offer lost liveness when the sibling heartbeated")
	}
	if !store.offerIndexLive(worker.WorkerID, siblingProfile, now) {
		t.Fatal("SQL sibling not live after its own heartbeat")
	}

	// Age only the catalogue slot. The sibling must stay live — a worker-grained
	// index would evict both.
	ageOfferIndexSlot(t, store, worker.WorkerID, profile.RuntimeProfileID)
	now = time.Now().UTC()
	if store.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, now) {
		t.Fatal("aged catalogue offer still live")
	}
	if !store.offerIndexLive(worker.WorkerID, siblingProfile, now) {
		t.Fatal("aging one offer_slot took the sibling down — slots are not independent")
	}
}

// TestFlagOnServiceLeaseDataPlaneTargetUsesIndexBothDirections drives the
// flag-ON selection path (ServiceLeaseDataPlaneTarget) in both directions:
// a heartbeating offer resolves SUCCESS, and an offer whose index entry has
// aged past 45s is refused even though durable last_seen_at is still fresh.
func TestFlagOnServiceLeaseDataPlaneTargetUsesIndexBothDirections(t *testing.T) {
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	ctx, store, pool, buyerID, lease, worker, profile := seedLeaseWithRealtimeOffer(t)

	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if _, err := store.ServiceLeaseDataPlaneTarget(ctx, buyerID, lease.ID); err != nil {
		t.Fatalf("flag ON: heartbeating offer must resolve SUCCESS: %v", err)
	}

	// Keep the durable stamp inside the 45s SQL window while aging the index.
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers SET last_seen_at=now()
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		worker.WorkerID, profile.RuntimeProfileID); err != nil {
		t.Fatal(err)
	}
	var sqlLive bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM realtime_worker_offers
		   WHERE worker_id=$1 AND runtime_profile_id=$2 AND status='ACTIVE'
		     AND last_seen_at > now() - interval '45 seconds')`,
		worker.WorkerID, profile.RuntimeProfileID).Scan(&sqlLive); err != nil {
		t.Fatal(err)
	}
	if !sqlLive {
		t.Fatal("durable last_seen_at is not fresh — cannot isolate the index decision")
	}
	ageOfferIndexSlot(t, store, worker.WorkerID, profile.RuntimeProfileID)
	if store.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, time.Now().UTC()) {
		t.Fatal("index entry did not age past 45s")
	}

	_, err := store.ServiceLeaseDataPlaneTarget(ctx, buyerID, lease.ID)
	if !errors.Is(err, errServiceLeaseDataPlaneUnavailable) {
		t.Fatalf("flag ON: aged index entry must be refused (durable last_seen_at still fresh): %v", err)
	}
}

// TestFlagOnChangeGatePersistsStatusChangeAndSkipsIdenticalRepeat: with the
// flag ON, an ACTIVE heartbeat followed by FAILED inside the refresh interval
// must reach the durable row (the change-gate must not skip a status change).
// A repeated identical ACTIVE inside the interval must perform no second
// durable write. The no-write half is observed via realtime_offer_samples
// row count, not internal counters.
func TestFlagOnChangeGatePersistsStatusChangeAndSkipsIdenticalRepeat(t *testing.T) {
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "liveness-changegate-key-32-bytes-min!!!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	active := RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}
	if err := store.HeartbeatRealtimeOffer(ctx, worker, active); err != nil {
		t.Fatalf("first ACTIVE: %v", err)
	}
	afterFirst := countOfferSamples(t, ctx, pool, worker.WorkerID, profile.RuntimeProfileID)
	if afterFirst < 1 {
		t.Fatal("first ACTIVE heartbeat produced no realtime_offer_samples row")
	}

	if err := store.HeartbeatRealtimeOffer(ctx, worker, active); err != nil {
		t.Fatalf("repeat ACTIVE: %v", err)
	}
	afterRepeat := countOfferSamples(t, ctx, pool, worker.WorkerID, profile.RuntimeProfileID)
	if afterRepeat != afterFirst {
		t.Fatalf("identical ACTIVE inside the refresh interval wrote again: samples %d → %d",
			afterFirst, afterRepeat)
	}

	failed := active
	failed.Status = "FAILED"
	if err := store.HeartbeatRealtimeOffer(ctx, worker, failed); err != nil {
		t.Fatalf("FAILED heartbeat: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `
		SELECT status FROM realtime_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		worker.WorkerID, profile.RuntimeProfileID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "FAILED" {
		t.Fatalf("durable status=%q after FAILED heartbeat inside the refresh interval — change-gate stranded the status change", status)
	}
	afterFailed := countOfferSamples(t, ctx, pool, worker.WorkerID, profile.RuntimeProfileID)
	if afterFailed <= afterRepeat {
		t.Fatalf("FAILED heartbeat did not produce a durable sample (count stayed %d) — status change was skipped", afterFailed)
	}
}

// TestFlushLivenessHeartbeatBatchDedupKeepsNewestObservation drives
// flushLivenessHeartbeatBatch with two heartbeats for the same (worker, profile)
// — an earlier ACTIVE and a later FAILED — and asserts the durable row ends
// FAILED. Both items must receive a non-error result.
func TestFlushLivenessHeartbeatBatchDedupKeepsNewestObservation(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "liveness-batch-dedup-key-32-bytes-min!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	t0 := time.Now().UTC()
	batch := []*livenessHeartbeatItem{
		{
			worker: worker,
			hb: RealtimeOfferHeartbeat{
				RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
				AvailableSequences: 8, Status: "ACTIVE",
			},
			observedAt: t0.Add(-time.Second),
		},
		{
			worker: worker,
			hb: RealtimeOfferHeartbeat{
				RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
				AvailableSequences: 8, Status: "FAILED",
			},
			observedAt: t0,
		},
	}
	errs := store.flushLivenessHeartbeatBatch(ctx, batch)
	if len(errs) != 2 {
		t.Fatalf("errs len=%d want 2", len(errs))
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("item %d: %v — both callers must get a non-error result", i, err)
		}
	}
	var status string
	if err := pool.QueryRow(ctx, `
		SELECT status FROM realtime_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		worker.WorkerID, profile.RuntimeProfileID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "FAILED" {
		t.Fatalf("durable status=%q — batch dedup must keep the newest (FAILED) observation", status)
	}
}

// TestFlushLivenessHeartbeatBatchWrongSupplierIsErrNotFound builds a batch of
// ≥2 where one item carries a supplier that does not own the offer. That item
// must return errNotFound; the legitimate item must succeed.
func TestFlushLivenessHeartbeatBatchWrongSupplierIsErrNotFound(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "liveness-batch-supplier-key-32-bytes!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	owner := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	impostor := owner
	impostor.SupplierID = uuid.New()
	now := time.Now().UTC()
	hb := RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}
	batch := []*livenessHeartbeatItem{
		{worker: owner, hb: hb, observedAt: now},
		{worker: impostor, hb: hb, observedAt: now},
	}
	errs := store.flushLivenessHeartbeatBatch(ctx, batch)
	if len(errs) != 2 {
		t.Fatalf("errs len=%d want 2", len(errs))
	}
	if errs[0] != nil {
		t.Fatalf("legitimate item: %v", errs[0])
	}
	if !errors.Is(errs[1], errNotFound) {
		t.Fatalf("wrong-supplier item: got %v want errNotFound", errs[1])
	}

	var status string
	var lastSeen time.Time
	if err := pool.QueryRow(ctx, `
		SELECT status, last_seen_at FROM realtime_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		owner.WorkerID, profile.RuntimeProfileID).Scan(&status, &lastSeen); err != nil {
		t.Fatal(err)
	}
	if status != "ACTIVE" {
		t.Fatalf("legitimate write did not land: status=%q", status)
	}
	if time.Since(lastSeen.UTC()) > 5*time.Second {
		t.Fatalf("legitimate last_seen_at=%v looks stale", lastSeen)
	}
}

// TestRealtimeSettlementUnchangedByLivenessWriteRetirement settles the same
// accepted contract shape twice — flag OFF then flag ON — and asserts identical
// buyer charge, supplier payable, and ledger rows.
//
// This is AuthorizeRealtimeContract + FinalizeRealtimeSuccess (physical token
// settle: buyer_charge, supplier_credit, platform_take, prepaid_debit). It
// does not reach service-lease replica-time settlement.
func TestRealtimeSettlementUnchangedByLivenessWriteRetirement(t *testing.T) {
	off := settleAcceptedRealtimeOnce(t, false)
	on := settleAcceptedRealtimeOnce(t, true)

	if off.buyerChargeNanos != on.buyerChargeNanos || off.supplierPayableNanos != on.supplierPayableNanos {
		t.Fatalf("flag ON changed settlement money: off charge=%d payable=%d; on charge=%d payable=%d",
			off.buyerChargeNanos, off.supplierPayableNanos, on.buyerChargeNanos, on.supplierPayableNanos)
	}
	if len(off.ledger) != len(on.ledger) {
		t.Fatalf("flag ON changed ledger row count: off=%d on=%d\n  off=%v\n  on=%v",
			len(off.ledger), len(on.ledger), off.ledger, on.ledger)
	}
	for i := range off.ledger {
		if off.ledger[i] != on.ledger[i] {
			t.Fatalf("flag ON changed ledger[%d]: off=%+v on=%+v", i, off.ledger[i], on.ledger[i])
		}
	}
}

// TestFlagOnRestartRefusesServiceLeaseDataPlaneTarget drives the real
// selection entry point on a freshly constructed Store (index empty) whose
// durable last_seen_at is still fresh, and asserts it refuses. Existing
// coverage stops at offerIndexLive.
func TestFlagOnRestartRefusesServiceLeaseDataPlaneTarget(t *testing.T) {
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	ctx, store, pool, buyerID, lease, worker, profile := seedLeaseWithRealtimeOffer(t)

	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	// Prove the lease is selectable on the live store before we restart.
	if _, err := store.ServiceLeaseDataPlaneTarget(ctx, buyerID, lease.ID); err != nil {
		t.Fatalf("pre-restart target: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers SET last_seen_at=now()
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		worker.WorkerID, profile.RuntimeProfileID); err != nil {
		t.Fatal(err)
	}
	var sqlLive bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM realtime_worker_offers
		   WHERE worker_id=$1 AND last_seen_at > now() - interval '45 seconds')`,
		worker.WorkerID).Scan(&sqlLive); err != nil {
		t.Fatal(err)
	}
	if !sqlLive {
		t.Fatal("SQL last_seen_at is not fresh; restart refusal must be index-empty, not SQL-empty")
	}

	restarted := NewStore(pool)
	if err := restarted.adoptMigratedSchema(ctx); err != nil {
		t.Fatalf("restart adopt: %v", err)
	}
	if restarted.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, time.Now().UTC()) {
		t.Fatal("fresh store reconstructed index liveness without a heartbeat")
	}
	_, err := restarted.ServiceLeaseDataPlaneTarget(ctx, buyerID, lease.ID)
	if !errors.Is(err, errServiceLeaseDataPlaneUnavailable) {
		t.Fatalf("flag ON restart must refuse at ServiceLeaseDataPlaneTarget: %v", err)
	}
}

func plantOfferSlotCache(t *testing.T, store *Store, workerID uuid.UUID, profileID string, slot uint32) {
	t.Helper()
	key := offerSlotKey{worker: workerID, profile: profileID}
	b, ok := store.lookupOfferBinding(workerID, profileID)
	if !ok {
		t.Fatal("cannot plant mapping: no offerBinding to copy")
	}
	b.slot = slot
	store.offerSlotCache.Store(key, b)
}

func insertSiblingRealtimeOfferSQL(t *testing.T, ctx context.Context, pool *pgxpool.Pool, worker WorkerAuth, srcProfile, siblingProfile string) {
	t.Helper()
	tag, err := pool.Exec(ctx, `
		INSERT INTO realtime_worker_offers
		  (worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,
		   placement_plan,placement_plan_sha256,
		   upstream_base_url,upstream_token_sealed,warmth,max_active_sequences,
		   available_sequences,supplier_input_usd_per_million_tokens,
		   supplier_output_usd_per_million_tokens,status,last_seen_at,updated_at)
		SELECT worker_id,supplier_id,$3,runtime_profile_sha256,
		       placement_plan,placement_plan_sha256,
		       upstream_base_url,upstream_token_sealed,warmth,max_active_sequences,
		       available_sequences,supplier_input_usd_per_million_tokens,
		       supplier_output_usd_per_million_tokens,'ACTIVE',
		       now()-interval '60 seconds', now()
		  FROM realtime_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		worker.WorkerID, srcProfile, siblingProfile)
	if err != nil {
		t.Fatalf("SQL sibling offer: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("SQL sibling offer rows=%d", tag.RowsAffected())
	}
}

func ageOfferIndexSlot(t *testing.T, store *Store, workerID uuid.UUID, profileID string) {
	t.Helper()
	store.ensureLiveDeviceIndex()
	if store.liveIndex == nil {
		t.Fatal("live index is nil")
	}
	slot, ok := store.lookupOfferSlot(workerID, profileID)
	if !ok {
		t.Fatal("no offer_slot to age")
	}
	if slot >= store.liveIndex.n {
		t.Fatalf("slot %d out of index capacity %d", slot, store.liveIndex.n)
	}
	now := uint32(time.Now().Unix())
	if now < liveDeviceWindowEpochs+1 {
		t.Fatal("unix now too small to age past the window")
	}
	atomic.StoreUint32(&store.liveIndex.epochs[slot], now-liveDeviceWindowEpochs-1)
}

func countOfferSamples(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workerID uuid.UUID, profileID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM realtime_offer_samples
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		workerID, profileID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func seedLeaseWithRealtimeOffer(t *testing.T) (
	ctx context.Context, store *Store, pool *pgxpool.Pool,
	buyerID uuid.UUID, lease ServiceLease, worker WorkerAuth, profile VLLMRuntimeProfile,
) {
	t.Helper()
	t.Setenv("MERC_TOKEN_KEY", "liveness-sel-e2e-key-32-bytes-minimum!!")
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool = openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	profile = sortedVLLMProfiles()[0]
	worker = seedOneRealtimeOffer(t, ctx, store, pool, profile)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-liv-" + uuid.NewString()
	if err := store.UpsertServiceLeaseOffer(ctx, worker, offer); err != nil {
		t.Fatalf("UpsertServiceLeaseOffer: %v", err)
	}
	buyerID = uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@liveness-sel.test"); err != nil {
		t.Fatal(err)
	}
	if err := store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "liveness-sel-"+buyerID.String()); err != nil {
		t.Fatal(err)
	}
	var err error
	lease, err = store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 120,
		MaximumP95LatencyMilliseconds: 500, BuyerDeclaredCeilingNanos: 135_000_000,
	})
	if err != nil {
		t.Fatalf("CreateServiceLease: %v", err)
	}
	return
}

type settledRealtimeShot struct {
	buyerChargeNanos     int64
	supplierPayableNanos int64
	ledger               []settledLedgerRow
}

type settledLedgerRow struct {
	kind     string
	currency string
	micros   int64
}

func settleAcceptedRealtimeOnce(t *testing.T, authoritative bool) settledRealtimeShot {
	t.Helper()
	if authoritative {
		t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	} else {
		t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "0")
	}
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "liveness-settle-parity-key-32-bytes-min")
	installSettlementCurrencyForTest(t, "usd")
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@liveness-settle.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 5_000_000, "liveness-settle-"+buyerID.String()))

	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-liv-settle-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
	})
	mustf(t, err, "authorize: %v")

	settlement, err := store.FinalizeRealtimeSuccess(ctx, contract.ID, RealtimeExecutionEvidence{
		ID: uuid.New(), HTTPStatus: http.StatusOK, StreamRootSHA256: strings.Repeat("1", 64),
		OutputCommitment: strings.Repeat("2", 64), PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9,
	})
	mustf(t, err, "finalize: %v")

	rows, err := pool.Query(ctx, `
		SELECT kind, currency, (amount_usd*1000000)::bigint
		  FROM ledger_entries
		 WHERE execution_contract_id=$1
		 ORDER BY kind, (amount_usd*1000000)::bigint, currency`, contract.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ledger []settledLedgerRow
	for rows.Next() {
		var r settledLedgerRow
		if err := rows.Scan(&r.kind, &r.currency, &r.micros); err != nil {
			t.Fatal(err)
		}
		ledger = append(ledger, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ledger) == 0 {
		t.Fatal("settlement produced no ledger rows")
	}
	return settledRealtimeShot{
		buyerChargeNanos:     settlement.BuyerChargeNanos,
		supplierPayableNanos: settlement.SupplierPayableNanos,
		ledger:               ledger,
	}
}
